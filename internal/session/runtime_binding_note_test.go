package session

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
)

func TestRuntimeBindingNoteIsAtomicAuditEvidence(t *testing.T) {
	store, source := forkFixture(t)
	const note = "t2 is served by its fallback scripted/local/backup: scripted/local/primary is unavailable"
	want := RuntimeBinding{Tier: "t2", Target: "scripted/local/backup"}
	if err := source.AppendRuntimeBindingNote(want.Tier, want.Target, false, "warn", note); err != nil {
		t.Fatal(err)
	}
	if got := source.State().RuntimeBinding; got != want || got.Note != nil {
		t.Fatalf("live binding = %+v, want state-only %+v", got, want)
	}

	timeline, err := ReadTimeline(source.Path())
	if err != nil {
		t.Fatal(err)
	}
	assertTimelineNoteOnce(t, timeline, note)

	id := source.ID()
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.State().RuntimeBinding; got != want || got.Note != nil {
		t.Fatalf("reopened binding = %+v, want state-only %+v", got, want)
	}
	timeline, err = ReadTimeline(reopened.Path())
	if err != nil {
		t.Fatal(err)
	}
	assertTimelineNoteOnce(t, timeline, note)
}

func TestFailedRuntimeBindingNoteAppendChangesNeitherStateNorTimeline(t *testing.T) {
	store, source := forkFixture(t)
	_ = store
	want := RuntimeBinding{Tier: "t1", Target: provider.RouteTargetID(source.State().Target)}
	if err := source.AppendRuntimeBinding(want.Tier, want.Target, false); err != nil {
		t.Fatal(err)
	}
	beforePath := source.Path()
	source.mu.Lock()
	source.poisoned = errors.New("injected append refusal")
	source.mu.Unlock()

	err := source.AppendRuntimeBindingNote("t2", "scripted/local/backup", false, "warn", "must not land")
	if !errors.Is(err, ErrSessionPoisoned) {
		t.Fatalf("append error = %v, want ErrSessionPoisoned", err)
	}
	if got := source.State().RuntimeBinding; got != want {
		t.Fatalf("failed append changed binding to %+v, want %+v", got, want)
	}
	timeline, err := ReadTimeline(beforePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range timeline {
		if item.Note != nil && item.Note.Text == "must not land" {
			t.Fatal("failed composite append left its audit note in the timeline")
		}
	}
}

func TestExplicitRetargetForkKeepsBindingAuditNoteOnly(t *testing.T) {
	store, source := forkFixture(t)
	const note = "t1 was served by a verified fallback"
	if err := source.AppendRuntimeBindingNote("t1", "scripted/local/backup", false, "warn", note); err != nil {
		t.Fatal(err)
	}
	destination := provider.RouteTargetID("scripted/local/destination")
	child, err := store.ForkSessionOnto(source, len(source.State().Messages), destination)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	if got := child.State().RuntimeBinding; got.Target != "" {
		t.Fatalf("retargeted fork carried stale binding %+v", got)
	}
	timeline, err := ReadTimeline(child.Path())
	if err != nil {
		t.Fatal(err)
	}
	assertTimelineNoteOnce(t, timeline, note)

	// A reopen must retain the destination while the transformed audit record
	// remains an ordinary note rather than becoming a binding again.
	id, root := child.ID(), filepath.Dir(filepath.Dir(child.Path()))
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	reopenedStore, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := reopenedStore.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.State(); got.Target != string(destination) || got.RuntimeBinding.Target != "" {
		t.Fatalf("retargeted reopen = target %q binding %+v", got.Target, got.RuntimeBinding)
	}
}

func assertTimelineNoteOnce(t *testing.T, timeline []Timeline, text string) {
	t.Helper()
	count := 0
	for _, item := range timeline {
		if item.Note != nil && item.Note.Text == text {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("timeline has audit note %d times, want once: %+v", count, timeline)
	}
}
