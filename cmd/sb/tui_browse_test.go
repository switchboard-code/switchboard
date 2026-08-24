package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/credential"
)

// compatServer stands in for anything that speaks the chat-completions
// format: LM Studio, vLLM, llama.cpp, a proxy. All the picker needs from it is
// the model list.
func compatServer(t *testing.T, models ...string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		var b strings.Builder
		b.WriteString(`{"object":"list","data":[`)
		for i, m := range models {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"id":"` + m + `"}`)
		}
		b.WriteString(`]}`)
		io.WriteString(w, b.String())
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

// walk drives a dialog chain the way a person does, answering pickers with an
// id and text prompts with a line, until something that is not a dialog comes
// back.
func walk(t *testing.T, cmd tea.Cmd, choose func(pickerMsg) string, typed func(textPromptMsg) string) tea.Msg {
	t.Helper()
	for i := 0; i < 12; i++ {
		if cmd == nil {
			t.Fatal("the flow ended with no message")
		}
		switch msg := cmd().(type) {
		case pickerMsg:
			cmd = msg.action(choose(msg))
		case textPromptMsg:
			cmd = msg.submit(typed(msg))
		default:
			return msg
		}
	}
	t.Fatal("the dialog chain never terminated")
	return nil
}

func rowID(t *testing.T, p pickerMsg, label string) string {
	t.Helper()
	for _, it := range p.items {
		if it.label == label {
			return it.id
		}
	}
	var have []string
	for _, it := range p.items {
		have = append(have, it.label)
	}
	t.Fatalf("no row labelled %q in %q; rows are %s", label, p.title, strings.Join(have, ", "))
	return ""
}

// The bug this covers: the picker offered only what the catalog had priced —
// four Anthropic models and whatever Ollama had pulled — so a Kimi plan, a
// ChatGPT plan, and any OpenAI-compatible server were unreachable from the
// menus that exist to reach them.
func TestEverySurfaceIsOfferedNotJustPricedModels(t *testing.T) {
	m := modelsTestModel(t)

	p, ok := cmdModels(m, "")().(pickerMsg)
	if !ok {
		t.Fatal("/models did not open a picker")
	}
	labels := map[string]bool{}
	for _, it := range p.items {
		labels[it.label] = true
	}
	for _, want := range []string{
		"kimi/coding…",
		"openai/subscription…",
		"openaicompat/ollama…",
		"openaicompat/generic…",
		"ollama/local…",
	} {
		if !labels[want] {
			var have []string
			for _, it := range p.items {
				have = append(have, it.label)
			}
			t.Errorf("no row for %s; rows are %s", want, strings.Join(have, ", "))
		}
	}
	// The priced entries are still there and still bind directly.
	if !labels["anthropic/first-party/claude-opus-5"] {
		t.Error("a catalog entry stopped being offered")
	}
	api := modelChoice{ref: "openai/gpt-same", provider: "openai", surface: "first-party"}
	plan := modelChoice{ref: "openai/gpt-same", provider: "openai", surface: "subscription"}
	if modelChoiceLabel(api) == modelChoiceLabel(plan) {
		t.Fatal("same-model rows on different serving surfaces are indistinguishable")
	}
}

// The whole reported flow, end to end: point sb at an OpenAI-compatible
// server, pick one of the models it actually serves, and land with a rung
// bound and an address the next launch will read.
func TestCompatibleEndpointTakesAnAddressThenBindsAModel(t *testing.T) {
	m := modelsTestModel(t)
	addr := compatServer(t, "qwen3-coder", "gpt-oss-120b")

	var listed []string
	msg := walk(t, cmdModels(m, ""),
		func(p pickerMsg) string {
			switch {
			case strings.HasPrefix(p.title, "bind a model"):
				return rowID(t, p, "openaicompat/generic…")
			case strings.HasPrefix(p.title, "models on openaicompat/generic"):
				for _, it := range p.items {
					listed = append(listed, it.label)
				}
				return rowID(t, p, "openaicompat/gpt-oss-120b")
			case strings.HasPrefix(p.title, "which tier"):
				return p.items[len(p.items)-1].id
			}
			t.Fatalf("unexpected picker %q", p.title)
			return ""
		},
		func(tp textPromptMsg) string {
			switch {
			case strings.HasPrefix(tp.title, "server address"):
				return addr
			case strings.HasPrefix(tp.title, "positive maximum output"):
				return "4096"
			}
			t.Fatalf("unexpected prompt %q", tp.title)
			return ""
		})

	notice, ok := msg.(noticeMsg)
	if !ok || notice.level == "error" {
		t.Fatalf("binding failed: %#v", msg)
	}
	if len(listed) < 3 || listed[0] != "openaicompat/gpt-oss-120b" {
		t.Fatalf("the server's models should be listed in order, got %v", listed)
	}

	saved, err := config.LoadFile(m.app.config.Path)
	if err != nil {
		t.Fatal(err)
	}
	if got := saved.ProviderForTarget("openaicompat", "generic").BaseURL; got != addr {
		t.Fatalf("the address saved as %q, want %q", got, addr)
	}
	tier, ok := saved.Tier("t2")
	if !ok {
		t.Fatal("the new rung was not saved")
	}
	if tier.Target.Provider != "openaicompat" || tier.Target.Surface != "generic" ||
		tier.Target.ModelID != "gpt-oss-120b" {
		t.Fatalf("t2 bound to %+v, want the compatible endpoint's model", tier.Target)
	}
	if tier.Target.Params.MaxOutputTokens != 4096 {
		t.Fatalf("t2 max output = %d, want 4096", tier.Target.Params.MaxOutputTokens)
	}
}

func TestFailedAddressSaveLeavesLiveConfigAndProvidersUnchanged(t *testing.T) {
	key := config.ProviderSurfaceKey("openaicompat", "generic")
	cfg := &config.Config{
		Path: t.TempDir(), // a directory cannot be replaced by the config file
		Providers: map[string]config.ProviderSettings{
			key: {BaseURL: "http://original.invalid/v1", ContextWindow: 32_768},
		},
	}
	reg := newProviders("http://127.0.0.1:1", cfg)
	beforeGeneration := reg.generation
	continued := false

	msg, ok := askAddressCmd(asyncResultBinding{}, reg, cfg, "openaicompat", "generic", func() tea.Cmd {
		continued = true
		return nil
	})().(textPromptMsg)
	if !ok {
		t.Fatal("address flow did not open a text prompt")
	}
	result := msg.submit("http://replacement.invalid/v1")()
	notice, ok := result.(noticeMsg)
	if !ok || notice.level != "error" || !strings.Contains(notice.text, "saving the address failed") {
		t.Fatalf("failed address save = %#v, want error notice", result)
	}
	if got := cfg.ProviderForTarget("openaicompat", "generic"); got.BaseURL != "http://original.invalid/v1" || got.ContextWindow != 32_768 {
		t.Fatalf("failed save changed live provider settings to %+v", got)
	}
	if reg.generation != beforeGeneration {
		t.Fatal("failed address save reset cached providers")
	}
	if continued {
		t.Fatal("failed address save advanced the browse flow")
	}
}

func TestStandaloneBrowseRetryDoesNotReuseCachedUnauthenticatedClient(t *testing.T) {
	const envName = "SB_TEST_COMPAT_BROWSE_KEY"
	t.Setenv(envName, "stale-key")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer fresh-key" {
			http.Error(w, "credential required", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"fresh-model"}]}`)
	}))
	defer server.Close()

	cfg := &config.Config{
		Providers: map[string]config.ProviderSettings{
			config.ProviderSurfaceKey("openaicompat", "generic"): {BaseURL: server.URL + "/v1"},
		},
		Auth: map[string]credential.Settings{
			"openaicompat": {Env: envName},
		},
	}
	reg := newProviders("http://127.0.0.1:1", cfg)
	if _, _, err := listSurfaceModels(t.Context(), reg, "openaicompat", "generic"); !refusedForAuth(err) {
		t.Fatalf("priming request error = %v, want cached unauthenticated client", err)
	}

	// This is the continuation the auth-refusal picker assembles before the
	// credential store worker runs. Its private registry must remain client-
	// free until execution; otherwise it retains the stale Authorization.
	next := browseSurfaceCmdWithCatalog(reg, &catalog.Catalog{Revision: "test"}, cfg, modelChoice{
		provider: "openaicompat", surface: "generic",
	}, func(modelChoice) tea.Cmd { return nil })
	t.Setenv(envName, "fresh-key")
	raw := next()
	picker, ok := raw.(pickerMsg)
	if !ok {
		t.Fatalf("browse retry returned %T, want model picker", raw)
	}
	foundModel, offeredCredential := false, false
	for _, item := range picker.items {
		foundModel = foundModel || item.label == "openaicompat/fresh-model"
		offeredCredential = offeredCredential || item.id == storeSecretID
	}
	if !foundModel || offeredCredential {
		t.Fatalf("browse retry reused stale auth: foundModel=%v offeredCredential=%v items=%+v",
			foundModel, offeredCredential, picker.items)
	}
}

// A server that will not list is not a dead end: the id can still be typed,
// and the row that offers it says why the list above is empty.
func TestATypedModelIDIsAlwaysOffered(t *testing.T) {
	m := modelsTestModel(t)

	msg := walk(t, cmdModels(m, ""),
		func(p pickerMsg) string {
			switch {
			case strings.HasPrefix(p.title, "bind a model"):
				return rowID(t, p, "kimi/coding…")
			case strings.HasPrefix(p.title, "models on kimi/coding"):
				row := rowID(t, p, "type a model id…")
				for _, it := range p.items {
					if it.id == row && it.desc == "" {
						t.Error("the type-it-in row should say why the list is the length it is")
					}
				}
				return row
			case strings.HasPrefix(p.title, "which tier"):
				return p.items[len(p.items)-1].id
			}
			t.Fatalf("unexpected picker %q", p.title)
			return ""
		},
		func(tp textPromptMsg) string {
			switch {
			case strings.HasPrefix(tp.title, "model id"):
				return "kimi-for-coding"
			case strings.HasPrefix(tp.title, "positive maximum output"):
				return "4096"
			default:
				t.Fatalf("unexpected prompt %q", tp.title)
				return ""
			}
		})

	if notice, ok := msg.(noticeMsg); !ok || notice.level == "error" {
		t.Fatalf("binding a typed id failed: %#v", msg)
	}
	saved, err := config.LoadFile(m.app.config.Path)
	if err != nil {
		t.Fatal(err)
	}
	tier, ok := saved.Tier("t2")
	if !ok || tier.Target.ModelID != "kimi-for-coding" || tier.Target.Provider != "kimi" ||
		tier.Target.Params.MaxOutputTokens != 4096 {
		t.Fatalf("t2 = %+v, want the typed kimi model", tier.Target)
	}
}

// First run is where this matters most: the wizard has to be able to finish
// against a server the catalog has never heard of.
func TestOnboardingBindsT1OnACompatibleEndpoint(t *testing.T) {
	home := t.TempDir()
	isolateTestHome(t, home)

	cat, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Path: filepath.Join(home, config.FileName)}
	m := &onboardModel{reg: newProviders("http://127.0.0.1:1", cfg), cat: cat, cfg: cfg, th: darkTheme()}
	addr := compatServer(t, "qwen3-coder")

	items, choices := gatherModelChoices(t.Context(), m.reg, cat, cfg)
	step(t, m, onboardChoicesMsg{items: items, choices: choices})

	var browse string
	for id, c := range choices {
		if c.browse && c.provider == "openaicompat" && c.surface == "generic" {
			browse = id
		}
	}
	if browse == "" {
		t.Fatal("the wizard never offered the compatible endpoint")
	}

	dlg, ok := m.dlg.(*pickerDialog)
	if !ok {
		t.Fatalf("the model picker never opened, dialog is %T", m.dlg)
	}
	msg := walk(t, dlg.onPick(browse),
		func(p pickerMsg) string { return rowID(t, p, "openaicompat/qwen3-coder") },
		func(tp textPromptMsg) string { return addr })
	step(t, m, msg)

	// A compatible endpoint may or may not want a bearer token, so the wizard
	// asks; this server does not, and walking past the prompt has to leave the
	// wizard on the rung rather than on a dead screen.
	if _, asked := m.dlg.(*secretDialog); !asked {
		t.Fatalf("expected the optional key prompt, dialog is %T", m.dlg)
	}
	step(t, m, noticeMsg{text: "nothing entered, nothing stored"})
	if _, asked := m.dlg.(*textDialog); !asked {
		t.Fatalf("expected the required output-cap prompt, dialog is %T", m.dlg)
	}
	step(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("4096")})
	step(t, m, tea.KeyMsg{Type: tea.KeyEnter})

	if m.cancelled || m.err != nil {
		t.Fatalf("the wizard failed: cancelled=%v err=%v", m.cancelled, m.err)
	}
	saved, err := config.LoadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	tier, ok := saved.Tier("t1")
	if !ok {
		t.Fatal("t1 was not persisted")
	}
	if tier.Target.Provider != "openaicompat" || tier.Target.ModelID != "qwen3-coder" {
		t.Fatalf("t1 bound to %+v, want the compatible endpoint's model", tier.Target)
	}
	if tier.Target.Params.MaxOutputTokens != 4096 {
		t.Fatalf("t1 max output = %d, want the chosen 4096", tier.Target.Params.MaxOutputTokens)
	}
}

// A key prompt the user walks past used to close the dialog and advance
// nothing, leaving first run on a screen where no key did anything. An empty
// entry is a legitimate answer on a server that wants no key, so it has to
// carry the wizard forward.
func TestSkippingTheKeyPromptStillBindsTheRung(t *testing.T) {
	home := t.TempDir()
	isolateTestHome(t, home)

	cfg := &config.Config{Path: filepath.Join(home, config.FileName)}
	m := &onboardModel{
		reg:  newProviders("http://127.0.0.1:1", cfg),
		cat:  &catalog.Catalog{Revision: "test"},
		cfg:  cfg,
		th:   darkTheme(),
		step: stepModel,
		choice: modelChoice{
			ref: "openaicompat/qwen3", provider: "openaicompat", surface: "generic", catalogMaxOutput: 4096,
		},
	}
	step(t, m, noticeMsg{text: "nothing entered, nothing stored"})

	if m.err != nil {
		t.Fatalf("the wizard failed: %v", m.err)
	}
	if m.step != stepMore {
		t.Fatalf("the rung should be bound and the ladder question asked, step is %v", m.step)
	}
	saved, err := config.LoadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if tier, ok := saved.Tier("t1"); !ok || tier.Target.ModelID != "qwen3" {
		t.Fatalf("t1 = %+v, want the picked model bound anyway", tier.Target)
	}
}

// The checklist is where an address gets set before any model is picked, and
// the row has to reopen with what it stored.
func TestSetupChecklistSetsTheCompatibleEndpointAddress(t *testing.T) {
	isolateTestHome(t, t.TempDir())
	m := modelsTestModel(t)

	first, ok := setupChecklist(m)().(pickerMsg)
	if !ok {
		t.Fatal("the checklist did not open")
	}
	id := rowID(t, first, "openaicompat/generic")
	prompt, ok := first.action(id)().(textPromptMsg)
	if !ok {
		t.Fatal("the compatible endpoint row did not ask for an address")
	}
	reopened, ok := prompt.submit("http://localhost:1234/v1")().(pickerMsg)
	if !ok {
		t.Fatal("the checklist did not reopen after the address was stored")
	}
	for _, it := range reopened.items {
		if it.label == "openaicompat/generic" {
			if !strings.Contains(it.desc, "http://localhost:1234/v1") || !it.current {
				t.Fatalf("the row should show what it stored: %+v", it)
			}
			return
		}
	}
	t.Fatal("the row vanished after being configured")
}

// The local row is the one people reach for when Ollama is on another
// machine, so it has to change the address rather than print advice.
func TestSetupChecklistSetsTheOllamaAddress(t *testing.T) {
	isolateTestHome(t, t.TempDir())
	m := modelsTestModel(t)

	first := setupChecklist(m)().(pickerMsg)
	prompt, ok := first.action(setupLocalID)().(textPromptMsg)
	if !ok {
		t.Fatal("the ollama row did not ask for an address")
	}
	if _, ok := prompt.submit("box:11434")().(pickerMsg); !ok {
		t.Fatal("the checklist did not reopen after the address was stored")
	}

	if got := m.app.config.ProviderFor("ollama").BaseURL; got != "box:11434" {
		t.Fatalf("the address saved as %q", got)
	}
	// Stored is not enough: the adapter already built against the old address
	// has to be rebuilt, or the checklist reports a change nothing acts on.
	if got := m.app.providers.localServer().BaseURL(); got != "http://box:11434" {
		t.Fatalf("the live client still points at %q", got)
	}
}

// authServer refuses everything without a bearer token, which is what a
// hosted compatible endpoint does and what makes the listing order matter.
func authServer(t *testing.T, token string, models ...string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"message":"Not authenticated"}}`)
			return
		}
		var b strings.Builder
		b.WriteString(`{"object":"list","data":[`)
		for i, m := range models {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"id":"` + m + `"}`)
		}
		b.WriteString(`]}`)
		io.WriteString(w, b.String())
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

// The reported failure: a server that will not list without a key left the
// picker empty and pushed the user into typing a model id from memory, which
// is exactly what this menu exists to prevent. The credential has to be
// askable at the point of refusal.
func TestASurfaceThatRefusesOffersTheCredentialThere(t *testing.T) {
	m := modelsTestModel(t)
	addr := authServer(t, "sk-test", "unsloth/Qwen3.8-27B-GGUF")

	// The reported path exactly: nothing configured, so the address is given
	// first and the credential has to be asked for after it, without the user
	// having to know it was needed.
	var opened pickerMsg
	msg := walk(t, cmdModels(m, ""),
		func(p pickerMsg) string {
			if strings.HasPrefix(p.title, "bind a model") {
				return rowID(t, p, "openaicompat/generic…")
			}
			opened = p
			return rowID(t, p, "store a credential…")
		},
		func(tp textPromptMsg) string {
			if !strings.HasPrefix(tp.title, "server address") {
				t.Fatalf("a refusal should ask for a key, not %q", tp.title)
			}
			return addr
		})

	// The row that explains the empty list has to name the reason.
	for _, it := range opened.items {
		if it.label == "store a credential…" && !strings.Contains(it.desc, "401") {
			t.Errorf("the credential row should quote the refusal: %q", it.desc)
		}
	}

	prompt, ok := msg.(secretPromptMsg)
	if !ok {
		t.Fatalf("expected a masked key prompt, got %T", msg)
	}
	if prompt.ref.Provider != "openaicompat" || prompt.ref.Account != "generic" {
		t.Fatalf("the prompt asks for %+v, want the surface that refused", prompt.ref)
	}
	if prompt.then == nil {
		// Without this the key is stored and the user is returned nowhere,
		// which is the state that made them type an id blind.
		t.Fatal("storing the key must reopen the surface, not end the flow")
	}
}

// A model list nobody can read is not a reason to stop: with the address
// known, the surface is listed alongside everything else the machine reaches.
func TestAConnectedCompatibleEndpointListsInTheMainPicker(t *testing.T) {
	m := modelsTestModel(t)
	addr := compatServer(t, "unsloth/Qwen3.8-27B-GGUF")
	m.app.config.SetProviderBaseURL(
		config.ProviderSurfaceKey("openaicompat", "generic"), addr)

	p, ok := cmdModels(m, "")().(pickerMsg)
	if !ok {
		t.Fatal("/models did not open a picker")
	}
	var found bool
	for _, it := range p.items {
		found = found || it.label == "openaicompat/generic/unsloth/Qwen3.8-27B-GGUF"
	}
	if !found {
		var have []string
		for _, it := range p.items {
			have = append(have, it.label)
		}
		t.Fatalf("a connected server's models should be offered directly; rows are %s",
			strings.Join(have, ", "))
	}

	// The id keeps its slash. ParseTarget splits on the first one only, so a
	// namespaced model stays one model rather than becoming a surface.
	choice := modelChoice{ref: "openaicompat/unsloth/Qwen3.8-27B-GGUF", surface: "generic"}
	if err := m.app.config.BindTier("t9", "", choice.ref, choice.surface, ""); err != nil {
		t.Fatal(err)
	}
	tier, _ := m.app.config.Tier("t9")
	if tier.Target.ModelID != "unsloth/Qwen3.8-27B-GGUF" {
		t.Fatalf("the namespaced id bound as %q", tier.Target.ModelID)
	}
}

// Storing a credential has to reach the adapters. One built before the key
// existed caches its absence, and every later request goes out unauthenticated
// even though the store reported success.
func TestStoringACredentialDropsTheClientsBuiltWithoutIt(t *testing.T) {
	cfg := &config.Config{
		Path:      filepath.Join(t.TempDir(), config.FileName),
		Providers: map[string]config.ProviderSettings{},
	}
	cfg.SetProviderBaseURL(config.ProviderSurfaceKey("openaicompat", "generic"),
		compatServer(t, "a-model"))
	reg := newProviders("http://127.0.0.1:1", cfg)

	target := surfaceTarget("openaicompat", "generic")
	before, err := reg.get(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := reg.get(t.Context(), target); again != before {
		t.Fatal("the registry should cache a built client")
	}

	reg.reset()

	after, err := reg.get(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatal("reset must drop the cached client so the next one is built with the new credential")
	}
}

// A notice whose flow already queued its next step must not also be treated
// as a step finishing, or the wizard runs two at once.
func TestAResumedNoticeDoesNotAdvanceTheWizard(t *testing.T) {
	cfg := &config.Config{Path: filepath.Join(t.TempDir(), config.FileName)}
	m := &onboardModel{
		reg:    newProviders("http://127.0.0.1:1", cfg),
		cat:    &catalog.Catalog{Revision: "test"},
		cfg:    cfg,
		th:     darkTheme(),
		step:   stepModel,
		choice: modelChoice{ref: "openaicompat/x", provider: "openaicompat", surface: "generic"},
	}

	_, cmd := m.Update(noticeMsg{text: "stored openaicompat/generic", resumed: true})
	if cmd != nil {
		t.Fatal("a resumed notice should leave the queued continuation to run alone")
	}
	if m.quitting {
		t.Fatal("a resumed notice bound the rung instead of waiting for the flow")
	}
}

func TestOnboardingCredentialResetPrecedesContinuation(t *testing.T) {
	cfg := &config.Config{Path: filepath.Join(t.TempDir(), config.FileName)}
	reg := newProviders("http://127.0.0.1:1", cfg)
	m := &onboardModel{
		reg: reg, cat: &catalog.Catalog{Revision: "test"}, cfg: cfg, th: darkTheme(), step: stepConnect,
	}
	beforeGeneration := reg.generation
	continued := false
	after := func() tea.Msg {
		continued = true
		if reg.generation != beforeGeneration+1 {
			return noticeMsg{level: "error", text: "continued before provider reset"}
		}
		return noticeMsg{text: "continued with fresh providers"}
	}

	_, resume := m.Update(noticeMsg{
		text: "stored openaicompat/generic", resumed: true, refreshProviders: true, after: after,
	})
	if reg.generation != beforeGeneration+1 {
		t.Fatalf("provider generation = %d, want %d before resuming", reg.generation, beforeGeneration+1)
	}
	if resume == nil {
		t.Fatal("onboarding credential completion dropped its continuation")
	}
	result := resume()
	if !continued {
		t.Fatal("onboarding credential continuation did not run")
	}
	if notice, ok := result.(noticeMsg); !ok || notice.level == "error" {
		t.Fatalf("onboarding continuation = %#v, want fresh-provider success", result)
	}
}

// The reported failure was not a missing feature: the row existed and sat
// below a viewport that gave no sign anything was under it, so the list read
// as complete and the endpoint looked unreachable.
func TestThePickerSaysWhenRowsAreBelowTheFold(t *testing.T) {
	var items []pickerItem
	for i := 0; i < 14; i++ {
		items = append(items, pickerItem{id: string(rune('a' + i)), label: "row-" + string(rune('a'+i))})
	}
	d := &pickerDialog{title: "pick", items: items}

	view := d.view(80, darkTheme())
	if !strings.Contains(view, "4 more") {
		t.Errorf("a truncated list must say how much is hidden:\n%s", view)
	}
	if !strings.Contains(view, "1-10 of 14") {
		t.Errorf("a truncated list must say where in it you are:\n%s", view)
	}

	// A list that fits says none of this.
	short := &pickerDialog{title: "pick", items: items[:3]}
	if v := short.view(80, darkTheme()); strings.Contains(v, "more") || strings.Contains(v, " of ") {
		t.Errorf("a complete list should not claim to be cut off:\n%s", v)
	}
}

// A surface with nothing to show has something to do; one whose models are
// already in the list above does not. Ordering by that keeps the row someone
// actually needs off the bottom of a long list.
func TestSurfacesNeedingWorkComeBeforeOnesAlreadyListed(t *testing.T) {
	m := modelsTestModel(t)
	m.app.config.SetProviderBaseURL(
		config.ProviderSurfaceKey("openaicompat", "generic"),
		compatServer(t, "served-model"))

	p, ok := cmdModels(m, "")().(pickerMsg)
	if !ok {
		t.Fatal("/models did not open a picker")
	}
	pos := map[string]int{}
	for i, it := range p.items {
		pos[it.label] = i
	}

	connected, ok := pos["openaicompat/generic…"]
	if !ok {
		t.Fatal("the connected surface lost its row")
	}
	for _, unconnected := range []string{"openai/subscription…", "openaicompat/ollama…"} {
		at, ok := pos[unconnected]
		if !ok {
			t.Fatalf("no row for %s", unconnected)
		}
		if at > connected {
			t.Errorf("%s still needs connecting and should sort above %s, which is already listed",
				unconnected, "openaicompat/generic…")
		}
	}

	// The connected row says so rather than repeating a bare address.
	for _, it := range p.items {
		if it.label == "openaicompat/generic…" && !strings.Contains(it.desc, "listed above") {
			t.Errorf("a listed surface should say its models are already offered: %q", it.desc)
		}
	}
}
