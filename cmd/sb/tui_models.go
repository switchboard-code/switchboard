package main

// /models: discover what can run and bind it to the ladder, without leaving
// the TUI. Discovery has two sources with different freshness: the local
// Ollama server answers for what is pulled right now, and the catalog answers
// for what a key would unlock. Binding goes through config.BindTier and Save,
// so what this writes is exactly what the next launch loads.

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/provider/anthropic"
	"github.com/switchboard-code/switchboard/internal/provider/kimi"
	"github.com/switchboard-code/switchboard/internal/provider/ollama"
	"github.com/switchboard-code/switchboard/internal/provider/openaicompat"
)

// modelChoice is one bindable target, or one surface to open. The ref/surface
// split mirrors what BindTier validates, and effortLevels comes from the
// catalog because only it knows whether a level is a real request parameter or
// a string the adapter would reject.
type modelChoice struct {
	ref          string // provider/model; empty while the model is still unknown
	provider     string
	surface      string
	desc         string
	effortLevels []string

	// catalogMaxOutput is evidence used to admit the target without rewriting
	// its identity. maxOutput is different: it is the explicit cap a user chose
	// for an unlisted target, and is therefore persisted on RouteTarget.Params
	// and sent on the wire. Keeping the two apart prevents a catalog maximum
	// from silently turning into a user-configured inference parameter.
	catalogMaxOutput int
	maxOutput        int

	// browse marks a surface rather than a model. The catalog knows a
	// surface exists and what it costs; only its server knows which models
	// it serves today, so the pick opens a question rather than answering
	// one.
	browse bool
}

const removeRungID = "\x00remove"

// gatherModelChoices assembles everything bindable: live local models, then
// the catalog's priced entries, then every serving surface the catalog knows
// the mechanics of. The third group is the one that makes the ladder
// buildable at all for plan-metered and compatible endpoints, where the models
// are the account's rather than the catalog's and cannot be enumerated here.
// Shared by /models and first-run setup, which are the same question asked at
// different moments.
func gatherModelChoices(ctx context.Context, reg *providers, cat *catalog.Catalog, cfg *config.Config) ([]pickerItem, map[string]modelChoice) {
	choices := map[string]modelChoice{}
	var items []pickerItem
	add := func(id string, c modelChoice, label, desc string) {
		if _, dup := choices[id]; dup {
			return
		}
		choices[id] = c
		items = append(items, pickerItem{id: id, label: label, desc: desc})
	}

	local, localErr := reg.localServer().Models(ctx)
	if localErr == nil {
		sort.Strings(local)
		for _, name := range local {
			c := modelChoice{ref: "ollama/" + name, provider: "ollama", surface: "local", desc: "pulled locally"}
			c = withModelChoiceCatalogEvidence(cat, c)
			add(c.ref+" "+c.surface, c, modelChoiceLabel(c), c.desc)
		}
	}
	for _, info := range cat.Entries() {
		c := modelChoice{
			ref:              info.Provider + "/" + info.ProviderModelID,
			provider:         info.Provider,
			surface:          info.Surface,
			desc:             catalogDesc(info),
			effortLevels:     info.EffortLevels,
			catalogMaxOutput: info.MaxOutput,
		}
		add(c.ref+" "+c.surface, c, modelChoiceLabel(c), c.desc)
	}
	// Everything the machine is actually connected to, asked live. The
	// catalog above is what has been priced; this is what the account holds,
	// and for a plan or a compatible server the two have almost nothing in
	// common. A surface that is not connected yet contributes nothing here
	// and keeps its browse row, which is where it gets connected.
	surfaces := browsableSurfaces(cat, cfg, localErr)
	asked := listConnectedSurfaces(ctx, reg, cfg, surfaces)
	for _, c := range surfaces {
		for _, name := range asked[surfaceKey(c)].models {
			bound := modelChoice{
				ref:      c.provider + "/" + name,
				provider: c.provider,
				surface:  c.surface,
				desc:     c.desc,
			}
			bound = withModelChoiceCatalogEvidence(cat, bound)
			// Per-model live evidence is newer than the catalog. Surface-level
			// effort words are not inherited: on mixed-dialect APIs that made an
			// unrecognized model look safely configurable when it was not.
			if stated := asked[surfaceKey(c)].efforts[name]; len(stated) > 0 {
				bound.effortLevels = append([]string(nil), stated...)
			}
			add(bound.ref+" "+bound.surface, bound, modelChoiceLabel(bound), bound.desc)
		}
	}

	// Surfaces that produced nothing come first. They are the ones with
	// something left to do — an address to give, a key to store — and burying
	// them under the models of a surface already listed above is how the row
	// someone needs ends up out of sight.
	priced := map[string]int{}
	for _, info := range cat.Entries() {
		priced[info.Provider+"/"+info.Surface]++
	}
	for _, want := range []bool{false, true} {
		for _, c := range surfaces {
			status := asked[surfaceKey(c)]
			shown := len(status.models) + priced[surfaceKey(c)]
			if (shown > 0) != want {
				continue
			}
			add(browsePrefix+c.provider+"/"+c.surface, c, c.provider+"/"+c.surface+"…",
				browseDesc(c, status, shown))
		}
	}
	return items, choices
}

// withModelChoiceCatalogEvidence keeps a live account/server listing tied to
// exact model evidence the catalog already has, including a canonical dated
// snapshot of a catalogued alias. A surface prior is deliberately insufficient:
// model-specific reasoning dialects and output ceilings vary on one surface.
// The catalog's limit is an admission fact, not a reason to persist a redundant
// target parameter, so it stays on the choice rather than RouteTarget.Params.
func withModelChoiceCatalogEvidence(cat *catalog.Catalog, choice modelChoice) modelChoice {
	if cat == nil || choice.ref == "" {
		return choice
	}
	target, err := config.ParseTarget(choice.ref, choice.surface, "")
	if err != nil {
		return choice
	}
	if info, confidence, ok := cat.Lookup(target); ok && confidence == catalog.Verified {
		choice.catalogMaxOutput = info.MaxOutput
		choice.effortLevels = append([]string(nil), info.EffortLevels...)
		choice.desc = catalogDesc(info)
	}
	return choice
}

// modelChoiceLabel includes the serving surface because the same provider
// model reached through two surfaces is two different targets. The id already
// carried this distinction; hiding it in the picker made first-party and plan
// rows with the same model text visually indistinguishable.
func modelChoiceLabel(choice modelChoice) string {
	providerName := choice.provider
	model := strings.TrimPrefix(choice.ref, providerName+"/")
	if providerName == "" {
		if parsedProvider, parsedModel, ok := strings.Cut(choice.ref, "/"); ok {
			providerName, model = parsedProvider, parsedModel
		}
	}
	if providerName == "" || choice.surface == "" {
		return choice.ref
	}
	return providerName + "/" + choice.surface + "/" + model
}

func tierBindingSummary(tier config.Tier) string {
	summary := tier.ID + " now runs " + tier.Target.Display()
	if tier.Target.Params.MaxOutputTokens > 0 {
		summary += " (rung max_output applies to primary and fallbacks)"
	}
	return summary
}

func surfaceKey(c modelChoice) string { return c.provider + "/" + c.surface }

// browseDesc says what this surface answered a moment ago, because "refused
// without a key" and "not pointed anywhere yet" send the user to different
// rows once they open it.
func browseDesc(c modelChoice, status surfaceModels, shown int) string {
	switch {
	case shown > 0:
		return fmt.Sprintf("%s · %d listed above", c.desc, shown)
	case refusedForAuth(status.err):
		return c.desc + " · refused without a credential; store one here"
	case status.err != nil:
		return c.desc + " · " + firstLine(status.err.Error())
	}
	return c.desc + " · pick from what this server serves"
}

// surfaceModels is one surface's live answer, including the refusal, which is
// as informative as a list and is what the row above it has to report. The
// efforts map is the per-model effort levels the same answer carried, nil
// where the surface's discovery says nothing about them.
type surfaceModels struct {
	models  []string
	efforts map[string][]string
	err     error
}

// listConnectedSurfaces asks every surface that can answer right now what it
// serves, all at once. Concurrently because the deadline is the picker's and
// three servers in sequence would spend it; only where a credential already
// resolves, because the alternative is a request that is certain to be
// refused and a wait for it on every open.
func listConnectedSurfaces(ctx context.Context, reg *providers, cfg *config.Config, candidates []modelChoice) map[string]surfaceModels {
	out := make([]surfaceModels, len(candidates))

	var wg sync.WaitGroup
	for i, c := range candidates {
		if c.provider == ollama.Name {
			continue // already listed live, above
		}
		if !surfaceConnected(ctx, cfg, c.provider, c.surface) {
			continue
		}
		wg.Add(1)
		go func(i int, c modelChoice) {
			defer wg.Done()
			names, efforts, err := listSurfaceModels(ctx, reg, c.provider, c.surface)
			out[i] = surfaceModels{models: names, efforts: efforts, err: err}
		}(i, c)
	}
	wg.Wait()

	byKey := make(map[string]surfaceModels, len(candidates))
	for i, c := range candidates {
		byKey[surfaceKey(c)] = out[i]
	}
	return byKey
}

// surfaceConnected reports whether a surface can be asked for its models
// without prompting for anything first. An address it cannot know and a key
// it has not been given are both the browse flow's job, not this one's.
func surfaceConnected(ctx context.Context, cfg *config.Config, providerName, surface string) bool {
	if surfaceNeedsAddress(providerName, surface) {
		// An address the user typed is itself the connection. Whether that
		// server also wants a key is its own answer to give, and plenty of
		// compatible servers want none; predicting it here would hide every
		// keyless endpoint from the list.
		return cfg.ProviderForTarget(providerName, surface).BaseURL != ""
	}
	if needsNoCredential(modelChoice{provider: providerName, surface: surface}) {
		return true
	}
	_, err := credential.Chain(cfg.AuthFor(providerName)).Get(
		ctx, credential.Ref{Provider: providerName, Account: surface})
	return err == nil
}

// browsableSurfaces is every surface worth opening, in the order a first run
// should read them: the local server, then what the catalog describes, then
// the uncharacterized compatible endpoint that stands for everything else.
func browsableSurfaces(cat *catalog.Catalog, cfg *config.Config, localErr error) []modelChoice {
	var out []modelChoice
	seen := map[string]bool{}
	add := func(c modelChoice) {
		key := c.provider + "/" + c.surface
		if seen[key] {
			return
		}
		seen[key] = true
		c.browse = true
		out = append(out, c)
	}

	if localErr != nil {
		// The list above is empty because nothing answered. The row that
		// says so is also the row that fixes it, since a server on another
		// host is the ordinary reason.
		add(modelChoice{
			provider: "ollama", surface: "local",
			desc: "server not answering; set its address or start it",
		})
	}
	// Not in the catalog, and it cannot be: the generic profile is the floor
	// of assumed capability for a server nobody has characterized, so there is
	// no price sheet to publish for it and pretending otherwise would price
	// every request at zero. It sits here, next to the other row that connects
	// a server rather than naming a vendor, because it is the only way to
	// reach an endpoint nothing else in this list can name.
	add(modelChoice{
		provider: openaicompat.Name, surface: genericCompat,
		desc: "any OpenAI-compatible server, at " + orNone(cfg.ProviderForTarget(openaicompat.Name, genericCompat).BaseURL),
	})
	for _, info := range cat.Surfaces() {
		add(modelChoice{
			provider: info.Provider,
			surface:  info.Surface,
			desc:     catalogDesc(info),
		})
	}
	// A concrete catalog entry proves that a surface exists, but none of that
	// model's capabilities are a surface default. Add any entry-only surfaces
	// as neutral browse destinations; Entries is sorted, so both the chosen
	// description and the final order are deterministic.
	for _, info := range cat.Entries() {
		add(modelChoice{
			provider: info.Provider,
			surface:  info.Surface,
			desc:     "catalogued surface; model capabilities are resolved after selection",
		})
	}
	return out
}

func cmdModels(m *tuiModel, args string) tea.Cmd {
	reg, cat, cfg := m.app.providers, m.app.catalog, m.app.config
	readCfg := cfg.Snapshot()
	readReg := reg.discoverySnapshot(readCfg)
	hasTiers := len(readCfg.Tiers) > 0
	binding := m.bindAsyncResult()
	return func() tea.Msg {
		defer readReg.releaseSnapshot()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		items, choices := gatherModelChoices(ctx, readReg, cat, readCfg)
		if len(items) == 0 {
			// The compatible-endpoint row is unconditional, so this is
			// unreachable rather than a state worth diagnosing. It stays as
			// the guard that keeps an empty picker from ever being drawn.
			return noticeMsg{level: "error", text: "nothing to offer; bind a rung by hand in " + readCfg.Path}
		}
		if hasTiers {
			items = append(items, pickerItem{id: removeRungID, label: "remove a rung…", desc: "drop a tier from the ladder"})
		}

		return binding.bindPicker(pickerMsg{
			title: "bind a model to a tier",
			items: items,
			action: func(id string) tea.Cmd {
				if id == removeRungID {
					return removeRungCmd(m)
				}
				choice := choices[id]
				if choice.browse {
					return browseSurfaceCmdForTUI(m, reg, cfg, choice,
						func(c modelChoice) tea.Cmd { return chooseTierOrOutputCapCmd(m, c) })
				}
				return chooseTierOrOutputCapCmd(m, choice)
			},
		})
	}
}

// chooseTierOrOutputCapCmd admits a choice only when its concrete adapter,
// explicit target parameters, or catalog evidence supplies a finite wire
// allowance. An omitted limit on a custom server is not a small limit: Ollama
// documents it as infinite and compatible endpoints choose their own default.
// The cap is therefore a deliberate user choice, before any rung is changed.
func chooseTierOrOutputCapCmd(m *tuiModel, choice modelChoice) tea.Cmd {
	if !modelChoiceNeedsExplicitOutputCap(m.app.providers, choice) {
		return chooseTierCmd(m, choice)
	}
	binding := m.bindAsyncResult()
	return modelOutputCapPromptCmd(choice, "", "", binding.bindText,
		func(chosen modelChoice) tea.Cmd { return chooseTierCmd(m, chosen) })
}

func modelChoiceNeedsExplicitOutputCap(reg *providers, choice modelChoice) bool {
	// A Messages adapter's required default is a wire value, not evidence about
	// an unrecognized model's supported maximum. A live or typed model with no
	// exact catalog evidence therefore gets an explicit rung cap before
	// publication, just like an uncharacterized server target.
	messagesSurface := choice.provider == anthropic.Name || choice.provider == kimi.Name
	needsExplicitMessagesCap := messagesSurface &&
		choice.catalogMaxOutput <= 0 && choice.maxOutput <= 0
	return needsExplicitMessagesCap || modelChoiceOutputAllowance(reg, choice) == math.MaxInt
}

func modelChoiceOutputAllowance(reg *providers, choice modelChoice) int {
	target, err := config.ParseTarget(choice.ref, choice.surface, "")
	if err != nil {
		return math.MaxInt
	}
	target.Params.MaxOutputTokens = choice.maxOutput
	return reg.outputTokenAllowance(target, choice.catalogMaxOutput)
}

// modelOutputCapPromptCmd is shared by /models and first-run onboarding. bind
// stamps asynchronous TUI ownership in the former and is the identity in the
// standalone wizard. Invalid input reopens the same owned prompt; empty input
// and Esc follow textDialog's ordinary cancellation path and publish nothing.
func modelOutputCapPromptCmd(choice modelChoice, initial, problem string,
	bind func(textPromptMsg) textPromptMsg, next func(modelChoice) tea.Cmd,
) tea.Cmd {
	return func() tea.Msg {
		help := "sent on every request; for example 4096"
		if problem != "" {
			help = problem
		}
		msg := textPromptMsg{
			title:   "positive maximum output for " + modelChoiceLabel(choice),
			help:    help,
			initial: initial,
			submit: func(value string) tea.Cmd {
				n, err := strconv.Atoi(strings.TrimSpace(value))
				if err != nil || n <= 0 {
					return modelOutputCapPromptCmd(choice, value,
						"0=unbounded; positive cap required",
						bind, next)
				}
				choice.maxOutput = n
				return next(choice)
			},
		}
		if bind != nil {
			msg = bind(msg)
		}
		return msg
	}
}

// catalogDesc is the one line that has to say what choosing this costs. The
// three zero-cost meterings are deliberately kept distinct (§4): free because
// local, free because a plan pays, and free-for-now are different promises.
func catalogDesc(info catalog.ModelInfo) string {
	name := info.DisplayName
	if name == "" {
		name = info.ProviderModelID
	}
	switch info.Metering {
	case catalog.Local:
		return name + " · local"
	case catalog.Plan:
		return name + " · plan-metered"
	}
	if info.Free() {
		return name + " · free"
	}
	if band, ok := info.Band(0); ok {
		return name + " · " + band.InputPerMTok.String() + "/" + band.OutputPerMTok.String() + " per MTok in/out"
	}
	return name
}

// chooseTierCmd is stage two: which rung takes the model. Existing rungs
// rebind in place and keep their labels; one new rung past the top is always
// offered, which is how a ladder grows and how the first tier gets made.
func chooseTierCmd(m *tuiModel, choice modelChoice) tea.Cmd {
	cfg := m.app.config
	tiers := cfg.Snapshot().Tiers
	binding := m.bindAsyncResult()
	return func() tea.Msg {
		var items []pickerItem
		for _, t := range tiers {
			items = append(items, pickerItem{
				id:    t.ID,
				label: t.ID,
				desc:  "rebind; now " + t.Target.Display(),
			})
		}
		// One past the highest rung, not the count plus one: the ladder can
		// have gaps (t1, t3) and a "new" rung must not collide with t3.
		next := "t" + strconv.Itoa(highestRung(&config.Config{Tiers: tiers})+1)
		items = append(items, pickerItem{id: next, label: next, desc: "new rung at the top"})

		return binding.bindPicker(pickerMsg{
			title: "which tier runs " + modelChoiceLabel(choice),
			items: items,
			action: func(tierID string) tea.Cmd {
				if len(choice.effortLevels) > 0 {
					return chooseEffortCmd(m, choice, tierID)
				}
				return bindCmd(m, choice, tierID, "")
			},
		})
	}
}

func chooseEffortCmd(m *tuiModel, choice modelChoice, tierID string) tea.Cmd {
	binding := m.bindAsyncResult()
	return func() tea.Msg {
		items := []pickerItem{{id: "", label: "default", desc: "let the provider decide"}}
		for _, level := range choice.effortLevels {
			items = append(items, pickerItem{id: level, label: level})
		}
		return binding.bindPicker(pickerMsg{
			title: "reasoning effort for " + modelChoiceLabel(choice) + " on " + tierID,
			items: items,
			action: func(effort string) tea.Cmd {
				return bindCmd(m, choice, tierID, effort)
			},
		})
	}
}

func bindCmd(m *tuiModel, choice modelChoice, tierID, effort string) tea.Cmd {
	cfg := m.app.config
	label := ""
	maxOutput := choice.maxOutput
	if existing, ok := cfg.Tier(tierID); ok {
		label = existing.Label
		// /models changes the primary model, not hand-authored rung policy.
		// A prompted custom cap replaces the old one; a catalog-backed choice
		// supplies no explicit value and therefore preserves an existing cap
		// that may also be what keeps a custom fallback usable.
		if maxOutput == 0 {
			maxOutput = existing.Target.Params.MaxOutputTokens
		}
	}
	if choice.catalogMaxOutput > 0 && maxOutput > choice.catalogMaxOutput {
		binding := m.bindAsyncResult()
		problem := fmt.Sprintf(
			"%s keeps max_output %d; this model's verified maximum is %d; enter a positive cap no greater than %d",
			tierID, maxOutput, choice.catalogMaxOutput, choice.catalogMaxOutput)
		// Do not prefill a valid value in an asynchronously opened prompt: a
		// delayed Enter must cancel, not silently accept a suggested wire cap.
		return modelOutputCapPromptCmd(choice, "", problem, binding.bindText,
			func(chosen modelChoice) tea.Cmd { return bindCmd(m, chosen, tierID, effort) })
	}
	if err := cfg.BindTierAndSave(tierID, label, choice.ref, choice.surface, effort, maxOutput); err != nil {
		return noticeCmd("error", "binding "+tierID+" failed: "+err.Error())
	}
	tier, ok := cfg.Tier(tierID)
	if !ok {
		return noticeCmd("error", "binding "+tierID+" saved but is absent from the live ladder")
	}
	return noticeCmd("", tierBindingSummary(tier)+"; /"+tierID+" switches to it")
}

func removeRungCmd(m *tuiModel) tea.Cmd {
	cfg := m.app.config
	binding := m.bindAsyncResult()
	var items []pickerItem
	for _, t := range cfg.Snapshot().Tiers {
		desc := t.Target.Display()
		if t.ID == m.app.tier.ID {
			desc += " · active now"
		}
		items = append(items, pickerItem{id: t.ID, label: t.ID, desc: desc})
	}
	return func() tea.Msg {
		return binding.bindPicker(pickerMsg{
			title: "remove which rung",
			items: items,
			action: func(tierID string) tea.Cmd {
				if tierID == m.app.tier.ID {
					return noticeCmd("error", tierID+" is the active tier; switch off it before removing it")
				}
				removed, err := cfg.RemoveTierAndSave(tierID)
				if err != nil {
					return noticeCmd("error", "removing "+tierID+" failed to save: "+err.Error())
				}
				if !removed {
					return noticeCmd("error", "no rung named "+tierID)
				}
				return noticeCmd("", tierID+" removed from the ladder")
			},
		})
	}
}

func highestRung(cfg *config.Config) int {
	high := 0
	for _, t := range cfg.Tiers {
		n, err := strconv.Atoi(strings.TrimPrefix(t.ID, "t"))
		if err == nil && n > high {
			high = n
		}
	}
	return high
}
