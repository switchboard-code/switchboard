package main

// First-run setup. An empty ladder on an interactive terminal is not an
// error, it is the beginning: instead of printing TOML to copy into a file,
// the TUI walks the same picker components /models and /login use — find a
// model, take a key if that model needs one, bind t1 — and then the ordinary
// startup proceeds against the config this just wrote.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/provider/ollama"
	"github.com/switchboard-code/switchboard/internal/provider/openaicompat"
	"github.com/switchboard-code/switchboard/internal/terminaltext"
)

// errSetupCancelled distinguishes "the user backed out" from a setup that
// failed; the caller turns it into guidance rather than a stack of context.
var errSetupCancelled = errors.New(
	"setup cancelled; run sb again to retry, or write a ladder into the config by hand")

// runOnboarding runs the wizard as its own inline Bubble Tea program, so the
// few lines it prints stay in the scrollback as a record of what was set up.
func runOnboarding(reg *providers, cat *catalog.Catalog, cfg *config.Config, workspace string) error {
	m := &onboardModel{reg: reg, cat: cat, cfg: cfg, workspace: workspace, th: themeFor(detectDark())}
	if _, err := tea.NewProgram(m).Run(); err != nil {
		return err
	}
	if m.cancelled {
		return errSetupCancelled
	}
	return m.err
}

type onboardChoicesMsg struct {
	items   []pickerItem
	choices map[string]modelChoice
}
type onboardPickedMsg struct{ id string }
type onboardKeyStoredMsg struct {
	note             string
	refreshProviders bool
}
type onboardWiredMsg struct {
	note string
	err  error
}
type onboardBoundMsg struct {
	tier string
	err  error
}

// onboardDoneMsg ends the wizard with the ladder as it stands.
type onboardDoneMsg struct{}

// onboardStep orders the wizard: connect what can be connected, then pick
// what t1 runs on. Keys first is deliberate — a key stored in the first step
// is a cloud model offerable in the second.
type onboardStep int

const (
	stepConnect onboardStep = iota
	stepModel
	stepMore
)

// onboardAddID is the row that keeps the wizard open for another rung.
const onboardAddID = "\x00add-rung"

type onboardModel struct {
	reg       *providers
	cat       *catalog.Catalog
	cfg       *config.Config
	workspace string
	th        *theme

	step   onboardStep
	dlg    dialog
	lines  []string
	choice modelChoice
	width  int
	height int

	cancelled bool
	quitting  bool
	err       error
}

func (m *onboardModel) Init() tea.Cmd { return m.connectStep() }

// connectStep is /setup's checklist wearing the wizard's frame: same rows,
// same standings, and its exit row hands over to the model picker.
func (m *onboardModel) connectStep() tea.Cmd {
	reg, cat, cfg := m.reg, m.cat, m.cfg
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		items := append(setupItems(ctx, reg, cat, cfg),
			pickerItem{id: setupDoneID, label: "continue", desc: "pick the model t1 starts on"})
		return pickerMsg{
			title: "switchboard setup — connect providers",
			items: items,
			action: func(id string) tea.Cmd {
				switch id {
				case setupDoneID:
					m.step = stepModel
					return m.modelStep()
				case setupLocalID:
					return askAddressCmd(asyncResultBinding{}, reg, cfg, ollama.Name, ollama.SurfaceLocal, m.connectStep)
				case setupCompatID:
					return askAddressCmd(asyncResultBinding{}, reg, cfg, openaicompat.Name, genericCompat, m.connectStep)
				case setupCodexID:
					if err := wireCodex(cfg, m.workspace); err != nil {
						return func() tea.Msg { return onboardWiredMsg{err: err} }
					}
					reg.reset()
					return func() tea.Msg {
						return onboardWiredMsg{note: "openai wired to your Codex CLI login"}
					}
				}
				ref, err := parseRef(id)
				if err != nil {
					return func() tea.Msg { return onboardWiredMsg{err: err} }
				}
				return m.askSecret(ref)
			},
		}
	}
}

func (m *onboardModel) askSecret(ref credential.Ref) tea.Cmd {
	store := credential.NewOSStore()
	writer, ok := any(store).(credential.Writer)
	if !ok {
		return func() tea.Msg {
			return onboardWiredMsg{note: "no OS credential store here; set " +
				credential.EnvNames(ref)[0] + " in the environment instead"}
		}
	}
	return func() tea.Msg {
		return secretPromptMsg{ref: ref, writer: writer, storeName: store.Name()}
	}
}

func (m *onboardModel) modelStep() tea.Cmd {
	reg, cat, cfg := m.reg, m.cat, m.cfg
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		items, choices := gatherModelChoices(ctx, reg, cat, cfg)
		return onboardChoicesMsg{items: items, choices: choices}
	}
}

func (m *onboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case onboardChoicesMsg:
		m.step = stepModel // the message is the model step, however it arrived
		if len(msg.items) == 0 {
			// Unreachable: the compatible-endpoint row is offered whatever
			// else is or is not there. The guard is what keeps first run from
			// ever drawing an empty picker.
			m.err = errors.New("nothing to offer, not even a server to point at.\n" +
				"Write a [tiers.t1] section into the config by hand, or run sb doctor")
			m.quitting = true
			return m, tea.Quit
		}
		choices := msg.choices
		m.dlg = &pickerDialog{
			title: "switchboard setup — pick the model t1 starts on",
			items: msg.items, sel: -1, requireSelection: true,
			onPick: func(id string) tea.Cmd {
				choice := choices[id]
				if choice.browse {
					// A surface, not a model yet: its server is asked what it
					// serves, and the pick comes back through here.
					return browseSurfaceCmdWithCatalog(m.reg, m.cat, m.cfg, choice, m.pick)
				}
				return m.pick(choice)
			},
		}
		return m, nil

	case onboardPickedMsg:
		return m, m.afterPick()

	case textPromptMsg:
		m.dlg = newTextDialog(msg)
		return m, nil

	// A notice inside the wizard is a step that resolved without storing
	// anything: an empty key prompt on a server that wants none, a save that
	// failed. It has to be said and then moved past, because a wizard that
	// closes its dialog and advances nothing leaves the user at a screen with
	// no keys that do anything.
	case noticeMsg:
		if msg.refreshProviders && m.reg != nil {
			m.reg.reset()
		}
		if msg.text != "" {
			m.lines = append(m.lines, msg.text)
		}
		if msg.after != nil {
			return m, msg.after
		}
		if msg.resumed {
			// Something else is already queued to continue this flow; taking
			// a step here as well would run two of them.
			return m, nil
		}
		switch {
		case m.step == stepConnect:
			return m, m.connectStep()
		case m.step == stepMore:
			return m, m.moreRungsStep()
		case m.choice.ref == "":
			return m, m.modelStep()
		}
		return m, m.outputCapOrEffort()

	case secretPromptMsg:
		m.dlg = newSecretDialog(msg.ref, msg.storeName, func(value string) tea.Cmd {
			store := func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := msg.writer.Set(ctx, msg.ref, value); err != nil {
					return onboardKeyStoredMsg{note: "storing the key failed: " + err.Error()}
				}
				note := "stored " + msg.ref.String() + " in the " + msg.storeName
				if msg.then != nil {
					return noticeMsg{
						text: note, resumed: true, refreshProviders: true, after: msg.then,
					}
				}
				return onboardKeyStoredMsg{note: note, refreshProviders: true}
			}
			// A continuation rides the store result instead of tea.Sequence:
			// Update must reset cached providers before the next probe runs.
			return store
		})
		return m, nil

	case onboardKeyStoredMsg:
		if msg.refreshProviders && m.reg != nil {
			m.reg.reset()
		}
		m.lines = append(m.lines, msg.note)
		// The same message ends a key entry in both steps; what follows it
		// depends on which step asked: the checklist reopens refreshed, the
		// model step moves on to binding.
		if m.step == stepConnect {
			return m, m.connectStep()
		}
		return m, m.outputCapOrEffort()

	case onboardWiredMsg:
		if msg.err != nil {
			m.lines = append(m.lines, "error: "+msg.err.Error())
		} else {
			m.lines = append(m.lines, msg.note)
		}
		return m, m.connectStep()

	case pickerMsg:
		m.dlg = &pickerDialog{
			title: msg.title, items: msg.items, sel: -1, requireSelection: true, onPick: msg.action,
		}
		return m, nil

	case onboardBoundMsg:
		if msg.err != nil {
			m.err = msg.err
			m.quitting = true
			return m, tea.Quit
		}
		if tier, ok := m.cfg.Tier(msg.tier); ok {
			m.lines = append(m.lines, tierBindingSummary(tier))
		} else {
			m.err = fmt.Errorf("saved %s is absent from the live ladder", msg.tier)
			m.quitting = true
			return m, tea.Quit
		}
		// The chosen target has been written; anything that arrives next is
		// about the ladder rather than about this rung.
		m.choice = modelChoice{}
		m.step = stepMore
		return m, m.moreRungsStep()

	case onboardDoneMsg:
		return m.finish()

	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			m.cancelled = true
			m.quitting = true
			return m, tea.Quit
		}
		if m.dlg != nil {
			done, cmd := m.dlg.update(msg, m.th)
			if done {
				m.dlg = nil
				if cmd == nil {
					// A bare close is the user backing out. Once a rung is
					// bound it is already written to the file, so backing out
					// means the ladder is finished rather than that a saved
					// configuration should be thrown away.
					if len(m.cfg.Tiers) > 0 {
						return m.finish()
					}
					m.cancelled = true
					m.quitting = true
					return m, tea.Quit
				}
			}
			return m, cmd
		}
	}
	return m, nil
}

// pick records the chosen target and moves the wizard on. It is the callback
// the surface browser resolves to, so a model reached through two menus and a
// typed id lands in exactly the same place as one picked off the first screen.
func (m *onboardModel) pick(choice modelChoice) tea.Cmd {
	m.choice = choice
	return func() tea.Msg { return onboardPickedMsg{id: choice.ref} }
}

// afterPick decides whether the chosen model needs a credential before it can
// answer. A local server never does; anything else is asked for only when the
// chain finds nothing, because a key in the environment already works.
func (m *onboardModel) afterPick() tea.Cmd {
	choice, cfg := m.choice, m.cfg
	if needsNoCredential(choice) {
		return m.outputCapOrEffort()
	}
	providerName := choiceProvider(choice)
	ref := credential.Ref{Provider: providerName, Account: choice.surface}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := credential.Chain(cfg.AuthFor(providerName)).Get(ctx, ref); err == nil {
			return onboardKeyStoredMsg{note: ref.String() + " already has a credential"}
		}
		store := credential.NewOSStore()
		writer, ok := any(store).(credential.Writer)
		if !ok {
			return onboardKeyStoredMsg{note: "no OS credential store here; set " +
				credential.EnvNames(ref)[0] + " before the first request"}
		}
		return secretPromptMsg{ref: ref, writer: writer, storeName: store.Name()}
	}
}

// outputCapOrEffort keeps an unlisted model from reaching the ladder with an
// omitted, unbounded wire default. Known catalog maxima and characterized
// adapter-owned finite defaults pass without acquiring a redundant target
// parameter; an unrecognized Messages model still asks because the adapter's
// required default is not evidence about that model's maximum.
func (m *onboardModel) outputCapOrEffort() tea.Cmd {
	if !modelChoiceNeedsExplicitOutputCap(m.reg, m.choice) {
		return m.effortOrBind()
	}
	return modelOutputCapPromptCmd(m.choice, "", "", nil, func(chosen modelChoice) tea.Cmd {
		m.choice = chosen
		return m.effortOrBind()
	})
}

func (m *onboardModel) effortOrBind() tea.Cmd {
	choice := m.choice
	if len(choice.effortLevels) == 0 {
		return m.bind("")
	}
	return func() tea.Msg {
		items := []pickerItem{{id: "", label: "default", desc: "let the provider decide"}}
		for _, level := range choice.effortLevels {
			items = append(items, pickerItem{id: level, label: level})
		}
		return pickerMsg{
			title:  "reasoning effort for " + modelChoiceLabel(choice),
			items:  items,
			action: func(effort string) tea.Cmd { return m.bind(effort) },
		}
	}
}

// bind writes the chosen target to the next free rung. The wizard fills the
// ladder from the bottom because that is the order it is read in: a session
// opens on t1 and escalates upward, so the first thing chosen is the thing
// that runs by default.
func (m *onboardModel) bind(effort string) tea.Cmd {
	choice, cfg := m.choice, m.cfg
	return func() tea.Msg {
		id := "t" + strconv.Itoa(highestRung(cfg)+1)
		if err := cfg.BindTierAndSave(id, "", choice.ref, choice.surface, effort, choice.maxOutput); err != nil {
			return onboardBoundMsg{err: err}
		}
		return onboardBoundMsg{tier: id}
	}
}

// moreRungsStep is the question the wizard used to answer for the user. A
// ladder is the thing this tool is for, and binding one rung then dropping
// into a session leaves the other rungs to a command nobody has met yet.
func (m *onboardModel) moreRungsStep() tea.Cmd {
	cfg := m.cfg
	return func() tea.Msg {
		next := "t" + strconv.Itoa(highestRung(cfg)+1)
		return pickerMsg{
			title: "ladder so far — " + ladderSummary(cfg),
			items: []pickerItem{
				{
					id:    onboardAddID,
					label: "add " + next + "…",
					desc:  "one rung up: /" + next + " switches to it, and escalation reaches for it",
				},
				{
					id:    setupDoneID,
					label: "start the session",
					desc:  "sessions open on t1; /models adds rungs later",
				},
			},
			action: func(id string) tea.Cmd {
				if id == onboardAddID {
					m.step = stepModel
					return m.modelStep()
				}
				return func() tea.Msg { return onboardDoneMsg{} }
			},
		}
	}
}

// ladderSummary is the one line that says what has been built so far, so the
// question "another rung?" is asked against something visible.
func ladderSummary(cfg *config.Config) string {
	parts := make([]string, 0, len(cfg.Tiers))
	for _, t := range cfg.Tiers {
		parts = append(parts, t.ID+" "+t.Target.Display())
	}
	if len(parts) == 0 {
		return "nothing bound yet"
	}
	return strings.Join(parts, " · ")
}

// finish closes the wizard with the ladder that was built.
func (m *onboardModel) finish() (tea.Model, tea.Cmd) {
	m.lines = append(m.lines,
		"saved to "+m.cfg.Path,
		"/models adds or rebinds rungs any time")
	m.quitting = true
	return m, tea.Quit
}

func (m *onboardModel) View() string {
	width := m.width
	if width <= 0 {
		width = 76
	}
	height := m.height
	if height <= 0 {
		height = dialogUnlimitedHeight
	}
	if width <= 0 || height <= 0 {
		return ""
	}
	if m.quitting {
		var rows []string
		for _, l := range m.lines {
			for _, row := range strings.Split(wrapCells(terminaltext.Display(l), width), "\n") {
				rows = append(rows, fitCells(row, width))
			}
		}
		if len(rows) > height {
			rows = rows[len(rows)-height:]
		}
		return strings.Join(rows, "\n")
	}
	// A first-run choice is the only path into a usable application. On a
	// one- or two-row terminal it therefore preempts the explanatory title,
	// just as a blocking session modal preempts passive status chrome.
	if m.dlg != nil && height <= 2 {
		return renderDialogWithin(m.dlg, width, height, m.th)
	}

	title := fitCells(m.th.bold.Render("switchboard")+m.th.dim.Render("  first run — nothing is configured yet"), width)
	top := []string{title}
	if height >= 8 {
		top = append(top, "")
	}
	var history []string
	for _, l := range m.lines {
		display := terminaltext.Display(l)
		for _, row := range strings.Split(wrapCells(m.th.dim.Render("  "+display), width), "\n") {
			history = append(history, fitCells(row, width))
		}
	}
	// Five rows keep a compact chooser's title, search, selected option, and
	// cancel hint together. Older progress notes yield first on a short first
	// run; the most recent notes remain directly above the current decision.
	reserveDialog := min(5, max(height-len(top), 0))
	historyBudget := max(height-len(top)-reserveDialog, 0)
	if len(history) > historyBudget {
		history = history[len(history)-historyBudget:]
	}
	rows := append(top, history...)
	remaining := max(height-len(rows), 0)
	if m.dlg != nil {
		if dialogView := renderDialogWithin(m.dlg, width, remaining, m.th); dialogView != "" {
			rows = append(rows, strings.Split(dialogView, "\n")...)
		}
	} else {
		rows = append(rows, fitCells(m.th.dim.Render("  looking for models…"), width))
	}
	if len(rows) > height {
		rows = rows[:height]
	}
	return strings.Join(rows, "\n")
}
