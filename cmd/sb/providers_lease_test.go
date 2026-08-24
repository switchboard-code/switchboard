package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/provider"
)

func compatibleLeaseServer(t *testing.T, model string, handler func(http.ResponseWriter, *http.Request)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handler != nil {
			handler(w, r)
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"`+model+`","max_model_len":32768}]}`)
		case "/v1/chat/completions":
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

func drainProviderStream(t *testing.T, stream provider.EventStream) error {
	t.Helper()
	defer stream.Close()
	for {
		_, err := stream.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func TestEscapedProviderReferenceNeverSendsOldBearerOrEndpointAfterReset(t *testing.T) {
	const (
		model   = "lease-model"
		envName = "SB_TEST_PROVIDER_LEASE_KEY"
	)
	var oldChatCalls atomic.Int32
	oldServer := compatibleLeaseServer(t, model, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"`+model+`","max_model_len":32768}]}`)
		case "/v1/chat/completions":
			oldChatCalls.Add(1)
			http.Error(w, "old endpoint must not receive content", http.StatusTeapot)
		default:
			http.NotFound(w, r)
		}
	})

	var newChatCalls atomic.Int32
	var sawOldBearer atomic.Bool
	newServer := compatibleLeaseServer(t, model, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"`+model+`","max_model_len":32768}]}`)
		case "/v1/chat/completions":
			newChatCalls.Add(1)
			if r.Header.Get("Authorization") == "Bearer old-bearer" {
				sawOldBearer.Store(true)
			}
			if r.Header.Get("Authorization") != "Bearer new-bearer" {
				http.Error(w, "wrong bearer", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	})

	t.Setenv(envName, "old-bearer")
	cfg := &config.Config{
		Providers: map[string]config.ProviderSettings{
			config.ProviderSurfaceKey("openaicompat", "generic"): {BaseURL: oldServer.URL + "/v1"},
		},
		Auth: map[string]credential.Settings{"openaicompat": {Env: envName}},
	}
	registry := newProviders("", cfg)
	target := provider.RouteTarget{Provider: "openaicompat", Surface: "generic", ModelID: model}
	tier := config.Tier{ID: "t1", Target: target}
	_, escaped, err := registry.probeTier(t.Context(), tier)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv(envName, "new-bearer")
	cfg.Providers[config.ProviderSurfaceKey("openaicompat", "generic")] = config.ProviderSettings{BaseURL: newServer.URL + "/v1"}
	registry.reset()
	if registry.preparedClientCurrent(escaped) {
		t.Fatal("reset left the old probe proof current")
	}

	stream, err := escaped.Stream(t.Context(), target, provider.Request{Messages: []provider.Message{provider.UserText("workspace content")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := drainProviderStream(t, stream); err != nil {
		t.Fatal(err)
	}
	if got := oldChatCalls.Load(); got != 0 {
		t.Fatalf("discarded compatible endpoint received %d chat requests", got)
	}
	if got := newChatCalls.Load(); got != 1 {
		t.Fatalf("replacement compatible endpoint received %d chat requests, want 1", got)
	}
	if sawOldBearer.Load() {
		t.Fatal("discarded bearer reached the replacement compatible endpoint")
	}
}

func TestProviderResetCancelsInflightStreamAndReferenceReacquires(t *testing.T) {
	const model = "blocking-model"
	started := make(chan struct{})
	cancelled := make(chan struct{})
	var chats atomic.Int32
	server := compatibleLeaseServer(t, model, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"`+model+`","max_model_len":32768}]}`)
		case "/v1/chat/completions":
			if chats.Add(1) == 1 {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				close(started)
				<-r.Context().Done()
				close(cancelled)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"fresh\"},\"finish_reason\":\"stop\"}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	})
	cfg := &config.Config{Providers: map[string]config.ProviderSettings{
		config.ProviderSurfaceKey("openaicompat", "generic"): {BaseURL: server.URL + "/v1"},
	}}
	registry := newProviders("", cfg)
	target := provider.RouteTarget{Provider: "openaicompat", Surface: "generic", ModelID: model}
	_, ref, err := registry.probeTier(t.Context(), config.Tier{ID: "t1", Target: target})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := ref.Stream(t.Context(), target, provider.Request{Messages: []provider.Message{provider.UserText("first")}})
	if err != nil {
		t.Fatal(err)
	}
	next := make(chan error, 1)
	go func() {
		_, err := stream.Next()
		next <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("stream request did not start")
	}
	registry.reset()
	select {
	case err := <-next:
		if !errors.Is(err, provider.ErrStreamIncomplete) {
			t.Fatalf("revoked stream error = %v, want retryable provider reconfiguration", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reset did not interrupt stream read")
	}
	_ = stream.Close()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("discarded adapter request context was not cancelled")
	}

	fresh, err := ref.Stream(t.Context(), target, provider.Request{Messages: []provider.Message{provider.UserText("second")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := drainProviderStream(t, fresh); err != nil {
		t.Fatal(err)
	}
	if got := chats.Load(); got != 2 {
		t.Fatalf("chat calls = %d, want cancelled old call plus fresh reacquisition", got)
	}
}

func TestProviderReferenceRefusesDifferentServingModelBeforeNetwork(t *testing.T) {
	const model = "bound-model"
	var chats atomic.Int32
	server := compatibleLeaseServer(t, model, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"`+model+`","max_model_len":32768}]}`)
		case "/v1/chat/completions":
			chats.Add(1)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	})
	cfg := &config.Config{Providers: map[string]config.ProviderSettings{
		config.ProviderSurfaceKey("openaicompat", "generic"): {BaseURL: server.URL + "/v1"},
	}}
	registry := newProviders("", cfg)
	target := provider.RouteTarget{Provider: "openaicompat", Surface: "generic", ModelID: model}
	_, ref, err := registry.probeTier(t.Context(), config.Tier{ID: "t1", Target: target})
	if err != nil {
		t.Fatal(err)
	}
	different := target
	different.ModelID = "different-model"
	_, err = ref.Stream(t.Context(), different, provider.Request{Messages: []provider.Message{provider.UserText("must stay local")}})
	if err == nil || provider.RequestIssued(err) {
		t.Fatalf("different-target error = %v, issued=%v", err, provider.RequestIssued(err))
	}
	if got := chats.Load(); got != 0 {
		t.Fatalf("different target reached transport %d times", got)
	}
}

type terminalResetStream struct {
	reset func()
	calls atomic.Int32
}

func (s *terminalResetStream) Next() (provider.Event, error) {
	if s.calls.Add(1) != 1 {
		return provider.Event{}, errors.New("wrapped adapter was read after its terminal event")
	}
	if s.reset != nil {
		s.reset()
	}
	return provider.Event{Type: provider.EventDone, StopReason: provider.StopEndTurn}, nil
}
func (*terminalResetStream) Close() error { return nil }

func TestTerminalEventWinsOverResetAfterProviderCompleted(t *testing.T) {
	registry := newProviders("", &config.Config{})
	registry.clientsMu.Lock()
	call := newProviderCall(context.Background(), registry.epoch, registry.generation)
	registry.clientsMu.Unlock()
	stream := &providerRefStream{inner: &terminalResetStream{reset: registry.reset}, call: call}
	t.Cleanup(func() { _ = stream.Close() })
	event, err := stream.Next()
	if err != nil || event.Type != provider.EventDone {
		t.Fatalf("terminal event after reset race = %#v, %v", event, err)
	}
	for read := 1; read <= 4; read++ {
		if _, err := stream.Next(); !errors.Is(err, io.EOF) {
			t.Fatalf("post-terminal read %d = %v, want EOF", read, err)
		}
	}
	if got := stream.inner.(*terminalResetStream).calls.Load(); got != 1 {
		t.Fatalf("wrapped adapter Next calls = %d, want only the terminal read", got)
	}
}

func TestProviderCallRejectsSuccessfulResultFromRevokedEpoch(t *testing.T) {
	registry := newProviders("", &config.Config{})
	registry.clientsMu.Lock()
	call := newProviderCall(context.Background(), registry.epoch, registry.generation)
	registry.clientsMu.Unlock()
	t.Cleanup(call.release)

	registry.reset()
	err := call.translate(nil)
	if !errors.Is(err, provider.ErrStreamIncomplete) {
		t.Fatalf("successful stale result = %v, want provider reconfiguration", err)
	}
}

func TestProviderCallRejectsSuccessfulResultAfterCallerCancellation(t *testing.T) {
	registry := newProviders("", &config.Config{})
	ctx, cancel := context.WithCancel(context.Background())
	registry.clientsMu.Lock()
	call := newProviderCall(ctx, registry.epoch, registry.generation)
	registry.clientsMu.Unlock()
	t.Cleanup(call.release)
	cancel()

	if err := call.translate(nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("successful cancelled result = %v, want context cancellation", err)
	}
}

func TestProviderStreamRepeatedEOFWinsOverConcurrentPostTerminalResets(t *testing.T) {
	registry := newProviders("", &config.Config{})
	registry.clientsMu.Lock()
	call := newProviderCall(context.Background(), registry.epoch, registry.generation)
	registry.clientsMu.Unlock()
	inner := &terminalResetStream{}
	stream := &providerRefStream{inner: inner, call: call}
	t.Cleanup(func() { _ = stream.Close() })

	event, err := stream.Next()
	if err != nil || event.Type != provider.EventDone {
		t.Fatalf("terminal event = %#v, %v", event, err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 32 {
			registry.reset()
		}
	}()
	for read := 1; read <= 64; read++ {
		if _, err := stream.Next(); !errors.Is(err, io.EOF) {
			t.Fatalf("post-terminal read %d = %v, want EOF", read, err)
		}
	}
	<-done
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("concurrent reset caused %d wrapped-adapter reads, want 1", got)
	}
}

func TestDiscoverySnapshotIsRevokedWithLiveRegistry(t *testing.T) {
	started := make(chan struct{})
	server := compatibleLeaseServer(t, "m", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		close(started)
		<-r.Context().Done()
	})
	cfg := &config.Config{Providers: map[string]config.ProviderSettings{
		config.ProviderSurfaceKey("openaicompat", "generic"): {BaseURL: server.URL + "/v1"},
	}}
	live := newProviders("", cfg)
	snapshot := live.discoverySnapshot(cfg)
	done := make(chan error, 1)
	go func() {
		_, _, err := listSurfaceModels(context.Background(), snapshot, "openaicompat", "generic")
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot discovery did not start")
	}
	live.reset()
	select {
	case err := <-done:
		if !errors.Is(err, provider.ErrStreamIncomplete) {
			t.Fatalf("revoked discovery error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("live reset did not revoke discovery snapshot")
	}
}

func TestDiscoverySnapshotReleaseIsIdempotentAndPreventsReuse(t *testing.T) {
	live := newProviders("", &config.Config{})
	snapshot := live.discoverySnapshot(&config.Config{})
	snapshot.releaseSnapshot()
	snapshot.releaseSnapshot()
	_, _, err := snapshot.acquire(t.Context(), provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "m"})
	if err == nil {
		t.Fatal("released discovery snapshot remained usable")
	}
}
