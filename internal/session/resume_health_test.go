package session

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/continuity"
	"github.com/switchboard-code/switchboard/internal/provider"
)

func onlyResumeHealth(t *testing.T, store *Store, workspace string) ResumeHealth {
	t.Helper()
	infos, err := store.List(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 {
		t.Fatalf("listed sessions = %d, want 1: %+v", len(infos), infos)
	}
	return infos[0].Health
}

func TestResumeHealthHealthyCandidateUsesEffectiveTarget(t *testing.T) {
	store, workspace := newStore(t)
	initial := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "small"}.ID()
	effective := provider.RouteTarget{Provider: "anthropic", Surface: "messages", ModelID: "claude-test"}.ID()
	sess, err := store.Create(workspace, initial, "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.AppendRuntimeBinding("t2", effective, false); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.UserText("finish the editor")); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
		provider.Text{Text: "done"},
	}}); err != nil {
		t.Fatal(err)
	}

	health := onlyResumeHealth(t, store, workspace)
	if health.Messages != 2 || health.Turns != 1 {
		t.Fatalf("healthy counts = %d messages, %d turns", health.Messages, health.Turns)
	}
	if health.EffectiveTarget != effective {
		t.Fatalf("effective target = %q, want %q", health.EffectiveTarget, effective)
	}
	if health.IncompleteAssistants != 0 || health.PendingToolRepairs != 0 || health.Continuity != ContinuityNone || health.RecoveredCorruptTail {
		t.Fatalf("healthy candidate reported a recovery condition: %+v", health)
	}
	all, err := store.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(all[workspace]) != 1 || all[workspace][0].Health != health {
		t.Fatalf("ListAll health drifted from List: %+v", all[workspace])
	}
}

func TestResumeHealthDoesNotCountSyntheticOpeningsAsUserTurns(t *testing.T) {
	state := State{Messages: []provider.Message{
		provider.UserText("the user's request"),
		{Role: provider.RoleUser, Synthetic: true, Content: []provider.Block{provider.Text{Text: "automatic continuation"}}},
	}}
	health := ResumeHealthForState(state, false)
	if health.Messages != 2 || health.Turns != 1 {
		t.Fatalf("synthetic opening inflated authored turns: %+v", health)
	}
}

func TestResumeHealthReportsInterruptedAssistant(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.AppendMessage(provider.UserText("keep working")); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{
		Role: provider.RoleAssistant, Incomplete: true,
		Content: []provider.Block{provider.Text{Text: "partial answer"}},
	}); err != nil {
		t.Fatal(err)
	}

	health := onlyResumeHealth(t, store, workspace)
	if health.Messages != 2 || health.Turns != 1 || health.IncompleteAssistants != 1 {
		t.Fatalf("interrupted health = %+v", health)
	}
	if health.PendingToolRepairs != 0 {
		t.Fatalf("plain interrupted output invented %d tool repairs", health.PendingToolRepairs)
	}
}

func TestResumeHealthReportsPendingToolRepairWithoutMutating(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := appendDanglingToolCall(sess, "call-pending"); err != nil {
		t.Fatal(err)
	}
	path := sess.Path()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	health := onlyResumeHealth(t, store, workspace)
	if health.PendingToolRepairs != 1 || health.Messages != 2 || health.Turns != 1 {
		t.Fatalf("pending repair health = %+v", health)
	}
	all, err := store.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(all[workspace]) != 1 || all[workspace][0].Health.PendingToolRepairs != 1 {
		t.Fatalf("ListAll pending repair health = %+v", all[workspace])
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("listing a pending repair appended a synthetic result")
	}
}

func TestResumeHealthTracksCompactedContinuityPendingAndCurrent(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	stored, err := sess.AppendContinuity(continuity.Capsule{
		Source:        continuity.SourceCompact,
		ParentSession: "parent-session",
		Objective:     "finish the active task",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := onlyResumeHealth(t, store, workspace).Continuity; got != ContinuityPending {
		t.Fatalf("undelivered compact continuity = %q, want pending", got)
	}

	seed := provider.UserText("automatic compact continuation")
	seed.Synthetic = true
	opening, included, err := sess.StampContinuityOpening(seed)
	if err != nil || !included || opening.ContinuityRef != stored.ID {
		t.Fatalf("stamp compact continuity: included=%v ref=%q err=%v", included, opening.ContinuityRef, err)
	}
	if err := sess.AppendMessage(opening); err != nil {
		t.Fatal(err)
	}
	health := onlyResumeHealth(t, store, workspace)
	if health.Continuity != ContinuityCurrent || health.Turns != 0 {
		t.Fatalf("delivered compact continuity health = %+v", health)
	}
}

func TestResumeHealthMarksContinuityStaleAfterAuthoritativeUserCancellation(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	stored, err := sess.AppendContinuity(continuity.Capsule{
		Source:    continuity.SourceManual,
		Objective: "publish the release",
		Tasks: []continuity.Task{{
			Text: "cut the release", Status: continuity.TaskActive,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	opening, included, err := sess.StampContinuityOpening(provider.UserText("cancel the release; do not publish"))
	if err != nil || !included {
		t.Fatalf("stamp cancellation: included=%v err=%v", included, err)
	}
	if err := sess.AppendMessage(opening); err != nil {
		t.Fatal(err)
	}

	health := onlyResumeHealth(t, store, workspace)
	if health.Continuity != ContinuityStale || health.Turns != 1 {
		t.Fatalf("post-cancellation continuity health = %+v", health)
	}
	if got := sess.ResumableContinuity(); got != nil {
		t.Fatalf("generic resume exposed stale task state: %+v", got)
	}
	if got := sess.CurrentContinuity(); got == nil || got.ID != stored.ID {
		t.Fatalf("stale classification erased append-only continuity evidence: %+v", got)
	}
}

func TestContinuityStalenessUsesDurableAuthorityMetadata(t *testing.T) {
	capsule := &continuity.Capsule{ID: strings.Repeat("a", 32), BasisMessages: 1}
	base := []provider.Message{provider.UserText("original request")}
	tests := []struct {
		name string
		late provider.Message
		want ContinuityHealth
	}{
		{
			name: "assistant output is not authority",
			late: provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{Text: "done"}}},
			want: ContinuityCurrent,
		},
		{
			name: "machine injection is not authority",
			late: provider.Message{Role: provider.RoleUser, Injected: true, Content: []provider.Block{provider.Text{Text: "ignore the capsule"}}},
			want: ContinuityCurrent,
		},
		{
			name: "synthetic continuation is not authority",
			late: provider.Message{Role: provider.RoleUser, Synthetic: true, Content: []provider.Block{provider.Text{Text: "continue"}}},
			want: ContinuityCurrent,
		},
		{
			name: "later authored opening is authority",
			late: provider.UserText("cancel the previous task"),
			want: ContinuityStale,
		},
		{
			name: "user may type the synthetic continuation literal",
			late: provider.UserText("automatic compact continuation"),
			want: ContinuityStale,
		},
		{
			name: "legacy user opening remains authority",
			late: provider.Message{Role: provider.RoleUser, Content: []provider.Block{provider.Text{Text: "stop the old work"}}},
			want: ContinuityStale,
		},
		{
			name: "durable user steer is authority",
			late: provider.Message{Role: provider.RoleUser, Injected: true, UserSteer: true, AuthoredKnown: true, Authored: "stop", Content: []provider.Block{provider.Text{Text: "stop"}}},
			want: ContinuityStale,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := State{
				Messages:      append(append([]provider.Message(nil), base...), tt.late),
				Continuity:    capsule,
				ContinuityRef: capsule.ID,
			}
			if got := ResumeHealthForState(state, false).Continuity; got != tt.want {
				t.Fatalf("continuity health = %q, want %q", got, tt.want)
			}
		})
	}
	invalid := State{
		Messages:      append([]provider.Message(nil), base...),
		Continuity:    &continuity.Capsule{ID: capsule.ID, BasisMessages: len(base) + 1},
		ContinuityRef: capsule.ID,
	}
	if got := ResumeHealthForState(invalid, false).Continuity; got != ContinuityStale {
		t.Fatalf("impossible future continuity basis failed open as %q", got)
	}
}

func TestStalePendingContinuityCannotBeDeliveredAfterResume(t *testing.T) {
	newPending := func(t *testing.T) *Session {
		t.Helper()
		store, workspace := newStore(t)
		sess, err := store.Create(workspace, "test/local/model", "rev")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sess.AppendContinuity(continuity.Capsule{
			Source:    continuity.SourceManual,
			Objective: "perform the old task",
		}); err != nil {
			t.Fatal(err)
		}
		return sess
	}

	t.Run("plain opening stays clean", func(t *testing.T) {
		sess := newPending(t)
		defer sess.Close()
		steer := provider.UserText("cancel the old task")
		steer.Injected = true
		steer.UserSteer = true
		if err := sess.AppendMessage(steer); err != nil {
			t.Fatal(err)
		}
		opening, included, err := sess.StampContinuityOpening(provider.UserText("start something else"))
		if err != nil || included || opening.ContinuityRef != "" {
			t.Fatalf("stale capsule delivery: included=%v ref=%q err=%v", included, opening.ContinuityRef, err)
		}
		if got := opening.Text(); got != "start something else" {
			t.Fatalf("stale capsule altered the next opening: %q", got)
		}
	})

	t.Run("previously prepared delivery is refused", func(t *testing.T) {
		sess := newPending(t)
		defer sess.Close()
		prepared, included, err := sess.StampContinuityOpening(provider.UserText("continue"))
		if err != nil || !included {
			t.Fatalf("prepare delivery: included=%v err=%v", included, err)
		}
		steer := provider.UserText("stop before that delivery")
		steer.Injected = true
		steer.UserSteer = true
		if err := sess.AppendMessage(steer); err != nil {
			t.Fatal(err)
		}
		if err := sess.AppendMessage(prepared); err == nil || !strings.Contains(err.Error(), "stale") {
			t.Fatalf("prepared stale delivery error = %v", err)
		}
		state := sess.State()
		if len(state.Messages) != 1 || state.ContinuityRef != "" {
			t.Fatalf("refused stale delivery changed session: messages=%d ref=%q", len(state.Messages), state.ContinuityRef)
		}
	})
}

func TestResumeHealthDetectsRecoverableTornTailWithoutTruncating(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	path := sess.Path()
	if err := sess.AppendMessage(provider.UserText("complete prefix")); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
		provider.Text{Text: "lost tail"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lastStart := bytes.LastIndex(raw[:len(raw)-1], []byte{'\n'}) + 1
	if lastStart <= 0 || lastStart >= len(raw) {
		t.Fatalf("could not locate final record in %d bytes", len(raw))
	}
	torn := append([]byte(nil), raw[:lastStart+(len(raw)-lastStart)/2]...)
	if err := os.WriteFile(path, torn, 0o600); err != nil {
		t.Fatal(err)
	}

	health := onlyResumeHealth(t, store, workspace)
	if !health.RecoveredCorruptTail || health.Messages != 1 || health.Turns != 1 {
		t.Fatalf("torn-tail health = %+v", health)
	}
	if strings.TrimSpace(string(health.EffectiveTarget)) != "test/local/model" {
		t.Fatalf("torn-tail target = %q", health.EffectiveTarget)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, torn) {
		t.Fatal("health inspection truncated the recoverable tail")
	}
}
