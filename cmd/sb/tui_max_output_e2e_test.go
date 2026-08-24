package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/provider"
)

type capturedRequest struct {
	mu   sync.Mutex
	body []byte
}

func (c *capturedRequest) set(body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.body = append([]byte(nil), body...)
}

func (c *capturedRequest) get() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.body...)
}

func customOllamaServer(t *testing.T, model string, captured *capturedRequest) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = io.WriteString(w, `{"models":[{"name":"`+model+`"}]}`)
		case "/api/show":
			_, _ = io.WriteString(w, `{"capabilities":["tools"],"parameters":"num_ctx 32768"}`)
		case "/api/chat":
			body, _ := io.ReadAll(r.Body)
			captured.set(body)
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = io.WriteString(w, `{"message":{"role":"assistant","content":"ok"},"done":true,"done_reason":"stop","prompt_eval_count":1,"eval_count":1}`+"\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestCustomOllamaBindPersistsRoutesAndSendsExactNumPredict(t *testing.T) {
	const modelID = "custom-coder-e2e"
	var captured capturedRequest
	server := customOllamaServer(t, modelID, &captured)

	m := modelsTestModel(t)
	m.app.providers = newProviders(server.URL, m.app.config)
	result := walk(t, cmdModels(m, ""), func(p pickerMsg) string {
		switch {
		case strings.HasPrefix(p.title, "bind a model"):
			return rowID(t, p, "ollama/local/"+modelID)
		case strings.HasPrefix(p.title, "which tier"):
			return "t1"
		default:
			t.Fatalf("unexpected picker %q", p.title)
			return ""
		}
	}, func(prompt textPromptMsg) string {
		if !strings.Contains(prompt.title, "positive maximum output") {
			t.Fatalf("unexpected text prompt %q", prompt.title)
		}
		return "4096"
	})
	if notice, ok := result.(noticeMsg); !ok || notice.level == "error" {
		t.Fatalf("custom Ollama bind failed: %#v", result)
	}

	loaded, err := config.LoadFile(m.app.config.Path)
	if err != nil {
		t.Fatal(err)
	}
	tier, ok := loaded.Tier("t1")
	if !ok || tier.Target.ModelID != modelID || tier.Target.Params.MaxOutputTokens != 4096 {
		t.Fatalf("reloaded t1 = %+v, want custom model with max_output 4096", tier)
	}
	if current, _ := m.app.config.Tier("t1"); current.Target.ID() != tier.Target.ID() {
		t.Fatalf("target identity changed across save/load: %s != %s", current.Target.ID(), tier.Target.ID())
	}

	registry := newProviders(server.URL, loaded)
	probed, client, err := registry.probeTier(context.Background(), tier)
	if err != nil {
		t.Fatal(err)
	}
	if window, enforced := registry.probedContextWindow(probed.Target); window != 32768 || !enforced {
		t.Fatalf("probed window = %d enforced=%v, want enforced 32768", window, enforced)
	}
	fit := turnPlan{PromptTokens: 100, ContextTokens: 100}
	if err := checkTurnFeasible(m.app.loop, m.app.catalog, registry, nil, nil, probed, 0, fit, provider.UserText("continue")); err != nil {
		t.Fatalf("small capped turn was rejected: %v", err)
	}
	over := turnPlan{PromptTokens: 28_673, ContextTokens: 28_673}
	if err := checkTurnFeasible(m.app.loop, m.app.catalog, registry, nil, nil, probed, 0, over, provider.UserText("continue")); err == nil || !strings.Contains(err.Error(), "4096 reserved output") {
		t.Fatalf("oversized capped turn error = %v, want 4096-token reserve refusal", err)
	}

	stream, err := client.Stream(context.Background(), probed.Target, provider.Request{Messages: []provider.Message{provider.UserText("hello")}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var wire struct {
		Options map[string]int `json:"options"`
	}
	if err := json.Unmarshal(captured.get(), &wire); err != nil {
		t.Fatal(err)
	}
	if got := wire.Options["num_predict"]; got != 4096 {
		t.Fatalf("Ollama num_predict = %d, want exact configured 4096; body=%s", got, captured.get())
	}
}

func TestRungCapBoundsCustomOllamaFallbackOnRouteAndWire(t *testing.T) {
	const fallbackModel = "custom-fallback-e2e"
	var captured capturedRequest
	server := customOllamaServer(t, fallbackModel, &captured)
	path := filepath.Join(t.TempDir(), config.FileName)
	if err := os.WriteFile(path, []byte(`[tiers.t1]
model = "ollama/missing-primary"
max_output = 4096
fallback = ["ollama/`+fallbackModel+`"]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	original, _ := cfg.Tier("t1")
	if len(original.Fallbacks) != 1 || original.Fallbacks[0].Params.MaxOutputTokens != 4096 {
		t.Fatalf("loaded fallback did not inherit rung cap: %+v", original.Fallbacks)
	}

	registry := newProviders(server.URL, cfg)
	served, client, note, err := registry.probeTierFallback(context.Background(), original)
	if err != nil {
		t.Fatal(err)
	}
	if served.ID != "t1" || served.Target.ModelID != fallbackModel || served.Target.Params.MaxOutputTokens != 4096 {
		t.Fatalf("served fallback = %+v, want bounded t1/%s", served, fallbackModel)
	}
	if !strings.Contains(note, "served by its fallback") {
		t.Fatalf("fallback substitution note = %q", note)
	}
	routeModel := testModel(t)
	fit := turnPlan{PromptTokens: 100, ContextTokens: 100}
	if err := checkTurnFeasible(routeModel.app.loop, routeModel.app.catalog, registry, nil, nil, served, 0, fit, provider.UserText("continue")); err != nil {
		t.Fatalf("small bounded fallback turn was rejected: %v", err)
	}
	over := turnPlan{PromptTokens: 28_673, ContextTokens: 28_673}
	if err := checkTurnFeasible(routeModel.app.loop, routeModel.app.catalog, registry, nil, nil, served, 0, over, provider.UserText("continue")); err == nil || !strings.Contains(err.Error(), "4096 reserved output") {
		t.Fatalf("oversized fallback error = %v, want 4096-token reserve refusal", err)
	}

	stream, err := client.Stream(context.Background(), served.Target, provider.Request{Messages: []provider.Message{provider.UserText("hello")}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var wire struct {
		Options map[string]int `json:"options"`
	}
	if err := json.Unmarshal(captured.get(), &wire); err != nil {
		t.Fatal(err)
	}
	if got := wire.Options["num_predict"]; got != 4096 {
		t.Fatalf("fallback num_predict = %d, want rung cap 4096; body=%s", got, captured.get())
	}
}

func compatibleCapServer(t *testing.T, model string, captured *capturedRequest) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"`+model+`","max_model_len":32768}]}`)
		case "/v1/chat/completions":
			body, _ := io.ReadAll(r.Body)
			captured.set(body)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestCustomCompatibleCapSurvivesReloadAndSendsExactMaxTokens(t *testing.T) {
	const modelID = "private-compatible-coder"
	var captured capturedRequest
	server := compatibleCapServer(t, modelID, &captured)
	m := modelsTestModel(t)
	m.app.config.SetProviderBaseURL(config.ProviderSurfaceKey("openaicompat", "generic"), server.URL+"/v1")
	if err := m.app.config.Save(); err != nil {
		t.Fatal(err)
	}
	m.app.providers = newProviders("http://127.0.0.1:1", m.app.config)
	result := walk(t, cmdModels(m, ""), func(p pickerMsg) string {
		switch {
		case strings.HasPrefix(p.title, "bind a model"):
			return rowID(t, p, "openaicompat/generic/"+modelID)
		case strings.HasPrefix(p.title, "which tier"):
			return "t1"
		default:
			t.Fatalf("unexpected picker %q", p.title)
			return ""
		}
	}, func(prompt textPromptMsg) string {
		if !strings.Contains(prompt.title, "positive maximum output") {
			t.Fatalf("unexpected text prompt %q", prompt.title)
		}
		return "4096"
	})
	if notice, ok := result.(noticeMsg); !ok || notice.level == "error" {
		t.Fatalf("custom compatible bind failed: %#v", result)
	}

	loaded, err := config.LoadFile(m.app.config.Path)
	if err != nil {
		t.Fatal(err)
	}
	tier, ok := loaded.Tier("t1")
	if !ok || tier.Target.ModelID != modelID || tier.Target.Params.MaxOutputTokens != 4096 {
		t.Fatalf("reloaded t1 = %+v, want custom compatible model with max_output 4096", tier)
	}
	if current, _ := m.app.config.Tier("t1"); current.Target.ID() != tier.Target.ID() {
		t.Fatalf("target identity changed across save/load: %s != %s", current.Target.ID(), tier.Target.ID())
	}
	registry := newProviders("http://127.0.0.1:1", loaded)
	probed, client, err := registry.probeTier(context.Background(), tier)
	if err != nil {
		t.Fatal(err)
	}
	if window, enforced := registry.probedContextWindow(probed.Target); window != 32768 || !enforced {
		t.Fatalf("compatible window = %d enforced=%v, want enforced 32768", window, enforced)
	}
	cat, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	routeModel := testModel(t)
	fit := turnPlan{PromptTokens: 100, ContextTokens: 100}
	if err := checkTurnFeasible(routeModel.app.loop, cat, registry, nil, nil, probed, 0, fit, provider.UserText("continue")); err != nil {
		t.Fatalf("small compatible turn was rejected: %v", err)
	}
	over := turnPlan{PromptTokens: 28_673, ContextTokens: 28_673}
	if err := checkTurnFeasible(routeModel.app.loop, cat, registry, nil, nil, probed, 0, over, provider.UserText("continue")); err == nil || !strings.Contains(err.Error(), "4096 reserved output") {
		t.Fatalf("oversized compatible turn error = %v, want 4096-token reserve refusal", err)
	}

	stream, err := client.Stream(context.Background(), probed.Target, provider.Request{Messages: []provider.Message{provider.UserText("hello")}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var wire struct {
		MaxTokens *int `json:"max_tokens"`
	}
	if err := json.Unmarshal(captured.get(), &wire); err != nil {
		t.Fatal(err)
	}
	if wire.MaxTokens == nil || *wire.MaxTokens != 4096 {
		t.Fatalf("compatible max_tokens = %v, want exact configured 4096; body=%s", wire.MaxTokens, captured.get())
	}
}
