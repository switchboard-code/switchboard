package main

import (
	"bufio"
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/credential"
)

const testGitHubToken = "ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789"

func chooseRedact(m *tuiModel) tea.Cmd {
	// Both secret gates put the safe drop row last. Moving up twice reaches
	// redact in the three-row outbound gate and remains there in the two-row
	// durable-storage gate.
	m.key(tea.KeyMsg{Type: tea.KeyUp})
	m.key(tea.KeyMsg{Type: tea.KeyUp})
	return m.key(tea.KeyMsg{Type: tea.KeyEnter})
}

// A key-shaped prompt does not start a turn; it opens the gate, and until
// the user answers, nothing has left the machine.
func TestStartTurnHoldsAKeyBehindTheGate(t *testing.T) {
	m := testModel(t)
	cmd := m.startTurn("review this: "+testGitHubToken, "")
	if cmd != nil {
		t.Error("a gated turn still returned a command")
	}
	if m.dlg == nil {
		t.Fatal("a prompt carrying a token opened no gate")
	}
	if m.busy {
		t.Error("the turn began before the gate was answered")
	}
	if flat := strings.Join(m.tr.flat, "\n"); strings.Contains(flat, testGitHubToken) {
		t.Fatal("the transcript rendered the token before the gate was answered")
	}
	view := m.dlg.view(90, m.th)
	if !strings.Contains(view, "GitHub token") {
		t.Errorf("the gate does not name what it found:\n%s", view)
	}
	if strings.Contains(view, testGitHubToken) {
		t.Errorf("the gate shows the secret it exists to hold back:\n%s", view)
	}
}

func TestSubmitRedactsHistoryAndDefersTheUserCard(t *testing.T) {
	home := t.TempDir()
	isolateTestHome(t, home)
	m := testModel(t)
	prompt := "review this: " + testGitHubToken
	m.ta.SetValue(prompt)

	if cmd := m.submit(); cmd != nil {
		t.Fatal("a gated submit returned work before the gate resolved")
	}
	if len(m.history) != 1 || strings.Contains(m.history[0], testGitHubToken) {
		t.Fatalf("in-memory history kept the raw token: %#v", m.history)
	}
	path, err := historyPath(m.app.workspace)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), testGitHubToken) {
		t.Fatal("durable history kept the raw token")
	}
	if flat := strings.Join(m.tr.flat, "\n"); strings.Contains(flat, testGitHubToken) {
		t.Fatal("the transcript painted the raw token while the gate was open")
	}

	chooseRedact(m)
	if m.dlg != nil {
		t.Fatal("redact did not close the gate")
	}
	flat := strings.Join(m.tr.flat, "\n")
	if strings.Contains(flat, testGitHubToken) || !strings.Contains(flat, "[redacted: a GitHub token]") {
		t.Fatalf("the resolved transcript did not use the redacted spelling:\n%s", flat)
	}
}

func TestDroppingSecretPromptLeavesNoUserCard(t *testing.T) {
	m := testModel(t)
	prompt := "review this: " + testGitHubToken
	if cmd := m.startTurn(prompt, ""); cmd != nil {
		t.Fatal("a gated turn returned work")
	}
	before := strings.Join(m.tr.flat, "\n")
	done, _ := m.dlg.update(tea.KeyMsg{Type: tea.KeyEscape}, m.th)
	if !done {
		t.Fatal("escape did not resolve the gate")
	}
	after := strings.Join(m.tr.flat, "\n")
	if after != before || strings.Contains(after, testGitHubToken) {
		t.Fatalf("dropping the prompt changed the transcript:\n%s", after)
	}
}

func TestSteerAndSkillPromptsDoNotPaintSecretsBeforeTheGate(t *testing.T) {
	t.Run("steer", func(t *testing.T) {
		m := testModel(t)
		prompt := "use " + testGitHubToken + " in the next round"
		if cmd := m.steer(prompt); cmd != nil {
			t.Fatal("secret-bearing steer ran before the gate")
		}
		if got := m.app.takeSteers(); len(got) != 0 {
			t.Fatalf("pre-gate steers = %+v", got)
		}
		if flat := strings.Join(m.tr.flat, "\n"); strings.Contains(flat, testGitHubToken) {
			t.Fatal("steer painted the raw token before the gate")
		}
		chooseRedact(m)
		if m.dlg != nil {
			t.Fatal("redact did not close the steer gate")
		}
		steers := m.app.takeSteers()
		if len(steers) != 1 || strings.Contains(steers[0], testGitHubToken) ||
			!strings.Contains(steers[0], "[redacted: a GitHub token]") {
			t.Fatalf("redacted steers = %+v", steers)
		}
	})

	t.Run("skill", func(t *testing.T) {
		m := testModel(t)
		display := "/skill review " + testGitHubToken
		prompt := "follow the review instructions with " + testGitHubToken
		if cmd := m.startSkillPrompt(display, prompt); cmd != nil {
			t.Fatal("secret-bearing skill prompt ran before the gate")
		}
		if flat := strings.Join(m.tr.flat, "\n"); strings.Contains(flat, testGitHubToken) {
			t.Fatal("skill invocation painted the raw token before the gate")
		}
		chooseRedact(m)
		if m.dlg != nil {
			t.Fatal("redact did not close the skill gate")
		}
		flat := strings.Join(m.tr.flat, "\n")
		if strings.Contains(flat, testGitHubToken) || !strings.Contains(flat, "[redacted: a GitHub token]") {
			t.Fatalf("skill display was not redacted:\n%s", flat)
		}
	})
}

func TestCredentialGatedSteerCannotCrossItsTurnBoundary(t *testing.T) {
	m := testModel(t)
	m.busy = true
	m.turnPlanning = false
	m.turnGeneration = 41
	prompt := "use " + testGitHubToken + " in this turn only"

	if cmd := m.steer(prompt); cmd != nil {
		t.Fatal("secret-bearing steer ran before the gate")
	}
	if m.dlg == nil {
		t.Fatal("secret-bearing steer opened no gate")
	}
	// The model turn completes while the user is still considering the
	// credential decision. Resolving the old gate must not arm a future turn.
	m.busy = false
	chooseRedact(m)

	if pending := m.app.takeSteers(); len(pending) != 0 {
		t.Fatalf("late credential decision armed a later turn: %v", pending)
	}
	if len(m.queue) != 0 {
		t.Fatalf("late credential decision silently queued a new prompt: %v", m.queue)
	}
	flat := strings.Join(m.tr.flat, "\n")
	if strings.Contains(flat, testGitHubToken) || !strings.Contains(flat, "that turn ended while the credential decision was open") {
		t.Fatalf("late steer refusal was unsafe or silent:\n%s", flat)
	}
}

func TestRacePromptSecretGateDefersBothBranchesAndTheTranscript(t *testing.T) {
	m := raceModel(t)
	_, generation, sourceID, err := m.startOperation("race setup")
	if err != nil {
		t.Fatal(err)
	}
	prompt := "compare with " + testGitHubToken
	probe := raceProbeMsg{
		operation: generation, sourceID: sourceID, prompt: prompt,
		a: m.app.config.Tiers[0], b: m.app.config.Tiers[1],
		ca: &racedProvider{}, cb: &racedProvider{},
	}
	if cmd := m.onRaceProbe(probe); cmd != nil {
		t.Fatal("secret-bearing race began before the gate resolved")
	}
	if m.dlg == nil {
		t.Fatal("race prompt opened no secret gate")
	}
	if flat := strings.Join(m.tr.flat, "\n"); strings.Contains(flat, testGitHubToken) {
		t.Fatal("race prompt painted the token before the gate")
	}
	setup := chooseRedact(m)
	if m.dlg != nil || setup == nil {
		t.Fatalf("redacted race gate dialog=%T setup=%v", m.dlg, setup)
	}
	flat := strings.Join(m.tr.flat, "\n")
	if strings.Contains(flat, testGitHubToken) || !strings.Contains(flat, "[redacted: a GitHub token]") {
		t.Fatalf("race transcript was not redacted:\n%s", flat)
	}
	m.finishOperation(generation, false)

	// Dropping the same prompt frees the exclusive operation and leaves no
	// user card or branch setup behind.
	m = raceModel(t)
	_, generation, sourceID, err = m.startOperation("race setup")
	if err != nil {
		t.Fatal(err)
	}
	probe.operation, probe.sourceID = generation, sourceID
	m.onRaceProbe(probe)
	before := strings.Join(m.tr.flat, "\n")
	done, setup := m.dlg.update(tea.KeyMsg{Type: tea.KeyEscape}, m.th)
	if !done || setup != nil || m.operationActive || strings.Join(m.tr.flat, "\n") != before {
		t.Fatalf("dropped race: done=%v setup=%v active=%v", done, setup, m.operationActive)
	}
}

// The three answers: a stray Enter drops, redact rewrites the outbound copy,
// send passes it as typed, and esc drops it too.
func TestSecretGateAnswers(t *testing.T) {
	m := testModel(t)
	prompt := "use " + testGitHubToken + " for the API"
	leaks := credential.ScanPrompt(prompt)
	if len(leaks) == 0 {
		t.Fatal("fixture token was not detected")
	}

	var sent string
	m.openSecretGate(leaks, prompt, func(p string) tea.Cmd { sent = p; return nil })
	if cmd := m.key(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		t.Fatal("the safe drop unexpectedly returned a command")
	}
	if sent != "" || m.dlg != nil {
		t.Fatalf("stray Enter sent %q or left dialog %T", sent, m.dlg)
	}

	// The gate can arrive between composer keystrokes. Ordinary text must not
	// filter the choices and turn a later Enter into raw-secret egress.
	m.openSecretGate(leaks, prompt, func(p string) tea.Cmd { sent = p; return nil })
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m.key(tea.KeyMsg{Type: tea.KeyEnter})
	if sent != "" || m.dlg != nil {
		t.Fatalf("composer rune plus Enter sent %q or left dialog %T", sent, m.dlg)
	}

	m.openSecretGate(leaks, prompt, func(p string) tea.Cmd { sent = p; return nil })
	chooseRedact(m)
	if strings.Contains(sent, testGitHubToken) {
		t.Errorf("redact sent the secret: %q", sent)
	}
	if !strings.Contains(sent, "[redacted: a GitHub token]") {
		t.Errorf("redact does not say what stood there: %q", sent)
	}

	sent = ""
	m.openSecretGate(leaks, prompt, func(p string) tea.Cmd { sent = p; return nil })
	m.key(tea.KeyMsg{Type: tea.KeyUp}) // drop -> send is an explicit move
	m.key(tea.KeyMsg{Type: tea.KeyEnter})
	if sent != prompt {
		t.Errorf("deliberate send passed %q, want the original prompt", sent)
	}

	sent = ""
	m.openSecretGate(leaks, prompt, func(p string) tea.Cmd { sent = p; return nil })
	m.key(tea.KeyMsg{Type: tea.KeyEscape})
	if sent != "" {
		t.Errorf("esc sent the prompt anyway: %q", sent)
	}
}

// The REPL's form of the gate: same three answers, asked in line, and the
// question itself never quotes the key.
func TestReplSecretGateAnswers(t *testing.T) {
	prompt := "use " + testGitHubToken + " for the API"
	leaks := credential.ScanPrompt(prompt)
	if len(leaks) == 0 {
		t.Fatal("fixture token was not detected")
	}
	ask := func(answer string) (string, string) {
		t.Helper()
		out, err := os.CreateTemp(t.TempDir(), "repl")
		if err != nil {
			t.Fatal(err)
		}
		defer out.Close()
		r := &repl{in: bufio.NewReader(strings.NewReader(answer)), out: newRenderer(out)}
		sent := r.secretGate(prompt, leaks)
		r.out.flush()
		printed, err := os.ReadFile(out.Name())
		if err != nil {
			t.Fatal(err)
		}
		return sent, string(printed)
	}

	if sent, printed := ask("r\n"); strings.Contains(sent, testGitHubToken) ||
		!strings.Contains(sent, "[redacted: a GitHub token]") || strings.Contains(printed, testGitHubToken) {
		t.Errorf("redact answer: sent %q, printed %q", sent, printed)
	}
	if sent, _ := ask("s\n"); sent != prompt {
		t.Errorf("send answer did not pass the prompt as typed: %q", sent)
	}
	if sent, printed := ask("\n"); sent != "" || strings.Contains(printed, testGitHubToken) {
		t.Errorf("an empty answer sent anyway: %q (printed %q)", sent, printed)
	}
}

// The race record is a summary, not the transcript: a key typed into the
// /race prompt must not ride the record into the log after the gate
// scrubbed it from what was sent.
func TestFinishRaceRedactsTheRecordedPrompt(t *testing.T) {
	m := raceModel(t)
	armA, err := assembleRaceArm(m.app, m.app.config.Tiers[0], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	armB, err := assembleRaceArm(m.app, m.app.config.Tiers[1], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	armA.status, armB.status = "completed", "completed"
	run := &raceRun{typed: "compare with " + testGitHubToken, arms: [2]*raceArm{armA, armB}}
	m.race = run
	m.busy = true

	winnerPath := armA.sess.Path()
	m.finishRace(run, "a", "a")
	log, err := os.ReadFile(winnerPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(log), testGitHubToken) {
		t.Error("the race record carried the raw key into the log")
	}
	if !strings.Contains(string(log), "[redacted: a GitHub token]") {
		t.Error("the record does not say a key stood in the prompt")
	}
}

// The headless surface has no one to ask, so the answer is no, with the
// widening flag named — and the refusal itself never quotes the key.
func TestRefuseLeakedSecrets(t *testing.T) {
	if err := refuseLeakedSecrets("a clean prompt", false); err != nil {
		t.Errorf("a clean prompt was refused: %v", err)
	}
	if err := refuseLeakedSecrets("key: "+testGitHubToken, true); err != nil {
		t.Errorf("-allow-secrets did not widen the gate: %v", err)
	}
	err := refuseLeakedSecrets("key: "+testGitHubToken, false)
	if err == nil {
		t.Fatal("a token rode through a -p prompt unchallenged")
	}
	if !strings.Contains(err.Error(), "-allow-secrets") {
		t.Errorf("the refusal does not name the deliberate widening: %v", err)
	}
	if strings.Contains(err.Error(), testGitHubToken) {
		t.Errorf("the refusal quotes the secret: %v", err)
	}
}
