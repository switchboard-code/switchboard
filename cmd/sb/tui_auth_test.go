package main

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/provider"
)

type credentialWriterStub struct {
	mu        sync.Mutex
	deleteErr error
	deletes   int
}

func (*credentialWriterStub) Name() string { return "test credential store" }

func (*credentialWriterStub) Get(context.Context, credential.Ref) (credential.Secret, error) {
	return credential.Secret{}, credential.ErrNotFound
}

func (*credentialWriterStub) Set(context.Context, credential.Ref, string) error { return nil }

func (s *credentialWriterStub) Delete(context.Context, credential.Ref) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletes++
	return s.deleteErr
}

func (s *credentialWriterStub) deleteCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deletes
}

// The login flow's contract: the secret is taken masked, and no rendering of
// the dialog ever contains what was typed.
func TestSecretDialogNeverEchoesTheSecret(t *testing.T) {
	m := testModel(t)
	var stored string
	m.dlg = newSecretDialog(credential.Ref{Provider: "kimi", Account: "coding"}, "test store", func(v string) tea.Cmd {
		stored = v
		return nil
	})

	const secret = "sk-test-not-a-real-key"
	for _, r := range secret {
		m.dlg.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}, m.th)
	}
	if view := m.dlg.view(80, m.th); strings.Contains(view, secret) {
		t.Fatalf("the dialog rendered the secret:\n%s", view)
	}

	done, _ := m.dlg.update(tea.KeyMsg{Type: tea.KeyEnter}, m.th)
	if !done {
		t.Fatal("enter did not close the dialog")
	}
	if stored != secret {
		t.Fatalf("submit delivered %q, want the typed secret", stored)
	}
}

func TestSecretDialogEscapeStoresNothing(t *testing.T) {
	m := testModel(t)
	submitted := false
	m.dlg = newSecretDialog(credential.Ref{Provider: "kimi", Account: "coding"}, "test store", func(string) tea.Cmd {
		submitted = true
		return nil
	})
	m.dlg.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("abc")}, m.th)
	done, _ := m.dlg.update(tea.KeyMsg{Type: tea.KeyEsc}, m.th)
	if !done {
		t.Fatal("escape did not close the dialog")
	}
	if submitted {
		t.Fatal("escape submitted the secret anyway")
	}
}

// /login with no argument resolves each reference's standing off the UI
// goroutine and comes back as a picker; the ladder's own targets lead it.
func TestLoginBuildsPickerFromLadderAndCatalog(t *testing.T) {
	m := modelsTestModel(t)
	cmd := cmdLogin(m, "")
	if cmd == nil {
		t.Fatal("/login produced no command")
	}
	msg, ok := cmd().(pickerMsg)
	if !ok {
		t.Fatalf("expected pickerMsg, got %T", cmd())
	}
	if len(msg.items) == 0 {
		t.Fatal("the picker has nothing to offer")
	}
	if msg.items[0].id != "ollama/local" {
		t.Errorf("the ladder's own reference should lead, got %q", msg.items[0].id)
	}
	if msg.items[0].desc != "local server, no key needed" {
		t.Errorf("ollama's standing should say no key is needed, got %q", msg.items[0].desc)
	}
	foundAnthropic := false
	for _, item := range msg.items {
		foundAnthropic = foundAnthropic || item.id == "anthropic/first-party"
	}
	if !foundAnthropic {
		t.Fatal("entry-only Anthropic surface disappeared from bare /login")
	}

	m.Update(msg)
	if m.dlg == nil {
		t.Fatal("pickerMsg did not open the picker")
	}
}

func TestLoginWithArgumentGoesStraightToThePrompt(t *testing.T) {
	m := testModel(t)
	cmd := cmdLogin(m, "kimi/coding")
	if cmd == nil {
		t.Fatal("/login kimi/coding produced no command")
	}
	msg := cmd()
	prompt, ok := msg.(secretPromptMsg)
	if !ok {
		// On a platform with no writable OS store the command degrades to a
		// notice naming the environment variable; that is also correct.
		if n, isNotice := msg.(noticeMsg); isNotice && strings.Contains(n.text, "SB_") {
			return
		}
		t.Fatalf("expected secretPromptMsg or an env-var notice, got %T", msg)
	}
	if prompt.ref.Provider != "kimi" || prompt.ref.Account != "coding" {
		t.Fatalf("prompt is for %+v, want kimi/coding", prompt.ref)
	}

	m.Update(msg)
	if m.dlg == nil {
		t.Fatal("secretPromptMsg did not open the masked input")
	}
}

func TestRemovingCredentialDropsCachedClientsAndProbeEvidence(t *testing.T) {
	reg := newProviders("http://127.0.0.1:11434", &config.Config{})
	m := testModel(t)
	m.app.providers = reg
	target := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "test:7b"}
	beforeClient := reg.localServer()
	beforeGeneration := reg.generation
	reg.mu.Lock()
	reg.probes[target.ID()] = provider.ProbeResult{Reachable: true, Vision: true, VisionKnown: true}
	reg.efforts[bareTargetKey(target)] = []string{"low", "high"}
	reg.windows[bareTargetKey(target)] = probedWindow{tokens: 32_768, enforced: true}
	reg.mu.Unlock()

	writer := &credentialWriterStub{}
	msg := removeSecretWithWriterCmd(credential.Ref{Provider: "test-logout", Account: "surface"}, writer)()
	notice, ok := msg.(noticeMsg)
	if !ok || notice.level != "" || !strings.Contains(notice.text, "removed test-logout/surface") {
		t.Fatalf("logout result = %#v, want success notice", msg)
	}
	if writer.deleteCount() != 1 {
		t.Fatalf("credential deletes = %d, want 1", writer.deleteCount())
	}
	if reg.generation != beforeGeneration || reg.localServer() != beforeClient {
		t.Fatal("credential worker reset providers before returning to the event loop")
	}
	m.Update(msg)
	if reg.generation != beforeGeneration+1 {
		t.Fatalf("provider generation = %d, want %d", reg.generation, beforeGeneration+1)
	}
	if reg.localServer() == beforeClient {
		t.Fatal("logout retained the client that captured the removed credential")
	}
	if _, ok := reg.probedCapabilities(target); ok {
		t.Fatal("logout retained capability evidence from the discarded client")
	}
	if _, ok := reg.probedEffortLevels(target); ok {
		t.Fatal("logout retained effort evidence from the discarded client")
	}
	if tokens, enforced := reg.probedContextWindow(target); tokens != 0 || enforced {
		t.Fatalf("logout retained context evidence: tokens=%d enforced=%v", tokens, enforced)
	}
}

func TestFailedCredentialRemovalKeepsClientsAndEvidence(t *testing.T) {
	reg := newProviders("http://127.0.0.1:11434", &config.Config{})
	m := testModel(t)
	m.app.providers = reg
	target := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "test:7b"}
	beforeClient := reg.localServer()
	beforeGeneration := reg.generation
	reg.mu.Lock()
	reg.probes[target.ID()] = provider.ProbeResult{Reachable: true}
	reg.mu.Unlock()

	wantErr := errors.New("credential service is locked")
	writer := &credentialWriterStub{deleteErr: wantErr}
	msg := removeSecretWithWriterCmd(credential.Ref{Provider: "test-logout", Account: "surface"}, writer)()
	notice, ok := msg.(noticeMsg)
	if !ok || notice.level != "error" || !strings.Contains(notice.text, wantErr.Error()) {
		t.Fatalf("logout result = %#v, want deletion error", msg)
	}
	m.Update(msg)
	if reg.generation != beforeGeneration || reg.localServer() != beforeClient {
		t.Fatal("failed deletion invalidated a client whose credential is still installed")
	}
	if _, ok := reg.probedCapabilities(target); !ok {
		t.Fatal("failed deletion discarded still-valid capability evidence")
	}
}

func TestCredentialRemovalResetIsRaceSafeWithEvidenceReaders(t *testing.T) {
	reg := newProviders("http://127.0.0.1:11434", &config.Config{})
	m := testModel(t)
	m.app.providers = reg
	target := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "test:7b"}
	reg.mu.Lock()
	reg.probes[target.ID()] = provider.ProbeResult{Reachable: true}
	reg.efforts[bareTargetKey(target)] = []string{"high"}
	reg.windows[bareTargetKey(target)] = probedWindow{tokens: 8_192, enforced: true}
	reg.mu.Unlock()

	ready := make(chan struct{})
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		first := true
		for {
			reg.probedCapabilities(target)
			reg.probedEffortLevels(target)
			reg.probedContextWindow(target)
			reg.localServer()
			if first {
				close(ready)
				first = false
			}
			select {
			case <-stop:
				return
			default:
			}
		}
	}()
	<-ready
	msg := removeSecretWithWriterCmd(credential.Ref{Provider: "test-logout", Account: "race"}, &credentialWriterStub{})()
	m.Update(msg)
	close(stop)
	<-done
	if notice, ok := msg.(noticeMsg); !ok || notice.level != "" {
		t.Fatalf("logout result = %#v, want success notice", msg)
	}
}

type blockingCredentialWriter struct {
	credentialWriterStub
	started chan struct{}
	release chan struct{}
}

func (w *blockingCredentialWriter) Delete(ctx context.Context, ref credential.Ref) error {
	close(w.started)
	select {
	case <-w.release:
		return w.credentialWriterStub.Delete(ctx, ref)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestCredentialWorkerDefersConfigReadingResetToEventLoop(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderSettings{
		config.ProviderSurfaceKey("openaicompat", "generic"): {BaseURL: "http://initial.invalid"},
	}}
	reg := newProviders("http://127.0.0.1:11434", cfg)
	m := testModel(t)
	m.app.config = cfg
	m.app.providers = reg
	beforeGeneration := reg.generation

	writer := &blockingCredentialWriter{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	result := make(chan tea.Msg, 1)
	go func() {
		result <- removeSecretWithWriterCmd(
			credential.Ref{Provider: "test-logout", Account: "race"}, writer)()
	}()
	<-writer.started

	stopWriting := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-stopWriting:
				return
			default:
				cfg.Providers = map[string]config.ProviderSettings{
					config.ProviderSurfaceKey("openaicompat", "generic"): {BaseURL: "http://changed.invalid"},
				}
				runtime.Gosched()
			}
		}
	}()
	close(writer.release)
	msg := <-result
	close(stopWriting)
	<-writerDone

	if reg.generation != beforeGeneration {
		t.Fatal("credential worker reset providers while live configuration could still be changing")
	}
	m.Update(msg)
	if reg.generation != beforeGeneration+1 {
		t.Fatalf("provider generation = %d after event-loop delivery, want %d", reg.generation, beforeGeneration+1)
	}
}

func TestStoredCredentialResetsProvidersBeforeContinuation(t *testing.T) {
	cfg := &config.Config{}
	reg := newProviders("http://127.0.0.1:11434", cfg)
	m := testModel(t)
	m.app.config = cfg
	m.app.providers = reg
	beforeGeneration := reg.generation
	continued := false
	after := func() tea.Msg {
		continued = true
		if reg.generation != beforeGeneration+1 {
			return noticeMsg{level: "error", text: "continued before provider reset"}
		}
		return noticeMsg{text: "continued with fresh providers"}
	}

	stored := storeSecretCmd(
		credential.Ref{Provider: "test-login", Account: "surface"},
		&credentialWriterStub{}, "test credential store", "not-a-real-secret", after,
	)()
	if reg.generation != beforeGeneration {
		t.Fatal("credential worker reset providers itself")
	}
	_, resume := m.Update(stored)
	if reg.generation != beforeGeneration+1 {
		t.Fatalf("provider generation = %d, want %d before resuming", reg.generation, beforeGeneration+1)
	}
	if resume == nil {
		t.Fatal("credential completion dropped its continuation")
	}
	result := resume()
	if !continued {
		t.Fatal("credential continuation did not run")
	}
	if notice, ok := result.(noticeMsg); !ok || notice.level == "error" {
		t.Fatalf("credential continuation = %#v, want fresh-provider success", result)
	}
}
