package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/provider"
	route "github.com/switchboard-code/switchboard/internal/router"
	"github.com/switchboard-code/switchboard/internal/session"
)

type resumeAdmissionFixture struct {
	model     *tuiModel
	candidate *session.Session
	path      string
	want      provider.RouteTarget
	wantNote  bool
}

func TestResumeAdmissionRejectsUnsafeReplayAtStartupAndTUI(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{name: "unapproved fallback", want: "not an approved destination"},
		{name: "tool incapable", want: "does not support tool calling"},
		{name: "vision incapable", want: "cannot read images"},
		{name: "frozen zones exceed context", want: "holds 512 tokens"},
		{name: "output envelope exceeds context", want: "reserved output"},
		{name: "retry reserve exhausts budget", want: "above the $0.00 ceiling"},
	}

	for _, test := range cases {
		for _, entrance := range []string{"startup", "tui"} {
			t.Run(test.name+"/"+entrance, func(t *testing.T) {
				fixture := newResumeAdmissionFixture(t, test.name)
				before := fixture.candidate.State().RuntimeBinding
				sourceID := fixture.model.app.loop.Session.ID()
				preliminary := fixture.model.app.loop.Binding()

				var err error
				switch entrance {
				case "startup":
					if bindErr := fixture.model.app.loop.BindSession(fixture.candidate); bindErr != nil {
						t.Fatal(bindErr)
					}
					_, _, _, err = finalizeStartupResume(context.Background(), fixture.candidate,
						fixture.model.app.loop, fixture.model.app.config, fixture.model.app.catalog,
						fixture.model.app.providers, fixture.model.app.budget, &options{})
					if got := fixture.model.app.loop.Binding(); got.Target.ID() != preliminary.Target.ID() || got.Provider != preliminary.Provider {
						t.Fatalf("failed startup admission changed preliminary binding: got %s want %s", got.Target.ID(), preliminary.Target.ID())
					}
				case "tui":
					id := fixture.candidate.ID()
					if closeErr := fixture.candidate.Close(); closeErr != nil {
						t.Fatal(closeErr)
					}
					msg, ok := fixture.model.app.reopen(context.Background(), 0, sourceID, id)().(sessionSwapMsg)
					if !ok {
						t.Fatalf("TUI resume returned %T", msg)
					}
					err = msg.err
					fixture.model.onSessionSwap(msg)
					if got := fixture.model.app.loop.Session.ID(); got != sourceID {
						t.Fatalf("failed TUI admission adopted session %s, want source %s", got, sourceID)
					}
					if got := fixture.model.app.loop.Binding(); got.Target.ID() != preliminary.Target.ID() || got.Provider != preliminary.Provider {
						t.Fatalf("failed TUI admission changed live binding: got %s want %s", got.Target.ID(), preliminary.Target.ID())
					}
				}

				if err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("resume admission error = %v, want %q", err, test.want)
				}
				for _, actionable := range []string{"was not adopted", "-tier/-model"} {
					if !strings.Contains(err.Error(), actionable) {
						t.Errorf("resume refusal is not actionable; missing %q in %v", actionable, err)
					}
				}
				var after session.State
				if entrance == "startup" {
					after = fixture.candidate.State()
				} else {
					var readErr error
					after, readErr = session.ReadState(fixture.path)
					if readErr != nil {
						t.Fatal(readErr)
					}
				}
				if after.RuntimeBinding != before {
					t.Fatalf("failed admission mutated runtime binding: got %+v want %+v", after.RuntimeBinding, before)
				}
			})
		}
	}
}

func TestResumeAdmissionAdoptsConfiguredOrderAndRemovedTierAtStartupAndTUI(t *testing.T) {
	for _, scenario := range []string{"configured primary", "configured fallback", "removed tier"} {
		for _, entrance := range []string{"startup", "tui"} {
			t.Run(scenario+"/"+entrance, func(t *testing.T) {
				fixture := newResumeAdmissionFixture(t, scenario)
				wantID := fixture.want.ID()
				switch entrance {
				case "startup":
					if err := fixture.model.app.loop.BindSession(fixture.candidate); err != nil {
						t.Fatal(err)
					}
					got, _, note, err := finalizeStartupResume(context.Background(), fixture.candidate,
						fixture.model.app.loop, fixture.model.app.config, fixture.model.app.catalog,
						fixture.model.app.providers, fixture.model.app.budget, &options{})
					if err != nil {
						t.Fatal(err)
					}
					if got.Target.ID() != wantID || (note != "") != fixture.wantNote {
						t.Fatalf("startup target=%s note=%q, want %s note=%v", got.Target.ID(), note, wantID, fixture.wantNote)
					}
				case "tui":
					id := fixture.candidate.ID()
					if err := fixture.candidate.Close(); err != nil {
						t.Fatal(err)
					}
					msg, ok := fixture.model.app.reopen(context.Background(), 0, fixture.model.app.loop.Session.ID(), id)().(sessionSwapMsg)
					if !ok || msg.err != nil {
						t.Fatalf("TUI resume result = %#v", msg)
					}
					if msg.tier.Target.ID() != wantID || msg.warnNote != fixture.wantNote {
						msg.sess.Close()
						t.Fatalf("prepared TUI target=%s note=%q, want %s note=%v", msg.tier.Target.ID(), msg.note, wantID, fixture.wantNote)
					}
					fixture.model.onSessionSwap(msg)
					if got := fixture.model.app.loop.Session.ID(); got != id {
						t.Fatalf("TUI adopted session %s, want %s", got, id)
					}
				}

				state := fixture.model.app.loop.Session.State()
				if state.RuntimeBinding.Target != wantID || fixture.model.app.loop.Binding().Target.ID() != wantID {
					t.Fatalf("adopted binding state=%+v live=%s, want %s", state.RuntimeBinding,
						fixture.model.app.loop.Binding().Target.ID(), wantID)
				}
			})
		}
	}
}

func TestOpenSessionDefersResumedFallbackBindingUntilFullAdmission(t *testing.T) {
	server := fakeOllama(t, "resume-backup")
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	tier := ollamaTier("t1", "resume-primary", "resume-backup")
	cfg := &config.Config{Tiers: []config.Tier{tier}}
	cat := catalogWithLocalModels(t,
		localModelSpec{name: "resume-primary", contextWindow: 100_000},
		localModelSpec{name: "resume-backup", contextWindow: 100_000},
	)
	original, err := store.Create(workspace, tier.Target.ID(), cat.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if err := original.AppendRuntimeBinding(tier.ID, tier.Target.ID(), false); err != nil {
		t.Fatal(err)
	}
	id := original.ID()
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}

	opts := options{resume: id}
	var chosen route.Decision
	resumed, preliminary, _, ok, note, err := openSession(context.Background(), store,
		newProviders(server.URL, cfg), cfg, cat, workspace, &opts, &chosen)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if !ok || preliminary.Target.ModelID != "resume-backup" || note == "" {
		t.Fatalf("preliminary resume = tier %+v resumed=%v note=%q", preliminary, ok, note)
	}
	if got := resumed.State().RuntimeBinding; got.Target != tier.Target.ID() {
		t.Fatalf("preliminary probe persisted fallback before full admission: %+v", got)
	}
}

func newResumeAdmissionFixture(t *testing.T, scenario string) resumeAdmissionFixture {
	t.Helper()
	m := testModel(t)
	t.Cleanup(func() {
		if m.app.loop.Session != nil {
			_ = m.app.loop.Session.Close()
		}
	})
	var (
		cfg          *config.Config
		cat          *catalog.Catalog
		reg          *providers
		tier         config.Tier
		recordedTier string
		recorded     provider.RouteTarget
		message      provider.Message
		reserve      int64
		want         provider.RouteTarget
		wantNote     bool
	)

	switch scenario {
	case "unapproved fallback":
		ollamaServer := fakeOllama(t)
		compatServer := fakeCompatibleModels(t, "compat-backup")
		recorded = provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "missing-primary"}
		fallback := provider.RouteTarget{Provider: "openaicompat", Surface: "generic", ModelID: "compat-backup"}
		tier = config.Tier{ID: "t1", Target: recorded, Fallbacks: []provider.RouteTarget{fallback}}
		cfg = &config.Config{
			Tiers:        []config.Tier{tier},
			Destinations: []string{"ollama"},
			Providers: map[string]config.ProviderSettings{
				config.ProviderSurfaceKey("openaicompat", "generic"): {BaseURL: compatServer.URL + "/v1"},
			},
		}
		cat = catalogWithLocalModels(t, localModelSpec{name: recorded.ModelID, contextWindow: 100_000})
		reg = newProviders(ollamaServer.URL, cfg)
		recordedTier = tier.ID

	case "tool incapable":
		server := noToolOllama(t, "no-tools")
		tier = ollamaTier("t1", "no-tools")
		cfg = &config.Config{Tiers: []config.Tier{tier}}
		cat = catalogWithLocalModels(t, localModelSpec{name: "no-tools", contextWindow: 100_000})
		reg = newProviders(server.URL, cfg)
		recorded, recordedTier = tier.Target, tier.ID

	case "vision incapable":
		server := capabilityOllama(t, map[string]bool{"text-only": false})
		tier = ollamaTier("t1", "text-only")
		cfg = &config.Config{Tiers: []config.Tier{tier}}
		cat = catalogWithLocalModels(t, localModelSpec{name: "text-only", contextWindow: 100_000})
		reg = newProviders(server.URL, cfg)
		recorded, recordedTier = tier.Target, tier.ID
		message = provider.Message{Role: provider.RoleUser, Content: []provider.Block{
			provider.Text{Text: "inspect this"}, provider.Image{MediaType: "image/png", Data: []byte{1, 2, 3}},
		}}

	case "frozen zones exceed context":
		server := capabilityOllama(t, map[string]bool{"small-window": false})
		tier = ollamaTier("t1", "small-window")
		cfg = &config.Config{Tiers: []config.Tier{tier}}
		cat = catalogWithLocalModels(t, localModelSpec{name: "small-window", contextWindow: 512})
		reg = newProviders(server.URL, cfg)
		recorded, recordedTier = tier.Target, tier.ID
		message = provider.UserText("resume the work")

	case "output envelope exceeds context":
		server := capabilityOllama(t, map[string]bool{"output-heavy": false})
		tier = ollamaTier("t1", "output-heavy")
		tier.Target.Params.MaxOutputTokens = 600
		cfg = &config.Config{Tiers: []config.Tier{tier}}
		cat = catalogWithLocalModels(t, localModelSpec{name: "output-heavy", contextWindow: 512})
		reg = newProviders(server.URL, cfg)
		recorded, recordedTier = tier.Target, tier.ID

	case "retry reserve exhausts budget":
		server := capabilityOllama(t, map[string]bool{"priced": false})
		tier = ollamaTier("t1", "priced")
		cfg = &config.Config{Tiers: []config.Tier{tier}, Budget: catalog.Money(100)}
		cat = catalogWithLocalModels(t, localModelSpec{
			name: "priced", contextWindow: 100_000, inputPerMTok: "1", outputPerMTok: "1", priceMaxInput: 100_000,
		})
		reg = newProviders(server.URL, cfg)
		recorded, recordedTier, reserve = tier.Target, tier.ID, 100

	case "configured fallback":
		server := fakeOllama(t, "resume-backup")
		tier = ollamaTier("t1", "resume-primary", "resume-backup")
		cfg = &config.Config{Tiers: []config.Tier{tier}}
		cat = catalogWithLocalModels(t,
			localModelSpec{name: "resume-primary", contextWindow: 100_000},
			localModelSpec{name: "resume-backup", contextWindow: 100_000},
		)
		reg = newProviders(server.URL, cfg)
		recorded, recordedTier = tier.Target, tier.ID
		want, wantNote = tier.Fallbacks[0], true

	case "configured primary":
		server := fakeOllama(t, "resume-primary", "resume-backup")
		tier = ollamaTier("t1", "resume-primary", "resume-backup")
		cfg = &config.Config{Tiers: []config.Tier{tier}}
		cat = catalogWithLocalModels(t,
			localModelSpec{name: "resume-primary", contextWindow: 100_000},
			localModelSpec{name: "resume-backup", contextWindow: 100_000},
		)
		reg = newProviders(server.URL, cfg)
		recorded, recordedTier = tier.Target, tier.ID
		want = tier.Target

	case "removed tier":
		server := capabilityOllama(t, map[string]bool{"orphan": false})
		recorded = provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "orphan"}
		cfg = &config.Config{Tiers: []config.Tier{ollamaTier("t1", "other")}}
		cat = catalogWithLocalModels(t, localModelSpec{name: "orphan", contextWindow: 100_000})
		reg = newProviders(server.URL, cfg)
		recordedTier, want = "removed", recorded

	default:
		t.Fatalf("unknown resume admission scenario %q", scenario)
	}
	if want.Provider == "" {
		want = recorded
	}

	m.app.config = cfg
	m.app.catalog = cat
	m.app.providers = reg
	m.app.loop.Catalog = cat
	m.app.loop.System = []provider.Block{provider.Text{Text: strings.Repeat("frozen-system ", 24)}}
	m.app.loop.OutputAllowance = reg.outputTokenAllowance
	m.app.loop.ContextWindow = func(target provider.RouteTarget) int {
		return effectiveContextWindow(cfg, reg, cat, target)
	}
	m.app.budget = &budgetState{}
	m.app.budget.set(cfg.Budget)
	if len(cfg.Tiers) > 0 {
		m.app.tier = cfg.Tiers[0]
	}

	candidate, err := m.app.store.Create(m.app.workspace, recorded.ID(), cat.Revision)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = candidate.Close() })
	if err := candidate.AppendRuntimeBinding(recordedTier, recorded.ID(), false); err != nil {
		t.Fatal(err)
	}
	if len(message.Content) > 0 {
		if err := candidate.AppendMessage(message); err != nil {
			t.Fatal(err)
		}
	}
	if reserve > 0 {
		if err := candidate.AppendRetryReserve(reserve); err != nil {
			t.Fatal(err)
		}
	}
	return resumeAdmissionFixture{model: m, candidate: candidate, path: candidate.Path(), want: want, wantNote: wantNote}
}

func noToolOllama(t *testing.T, model string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[{"name":` + string(mustJSON(t, model)) + `}]}`))
		case "/api/show":
			_, _ = w.Write([]byte(`{"capabilities":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func fakeCompatibleModels(t *testing.T, models ...string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		data := make([]map[string]string, len(models))
		for i, model := range models {
			data[i] = map[string]string{"id": model}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(server.Close)
	return server
}

func mustJSON(t *testing.T, value string) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
