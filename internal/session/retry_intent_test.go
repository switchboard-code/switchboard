package session

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/provider"
)

func TestRetryIntentPersistsHandoffWithoutCopyingOpeningAndLatestPrefersIt(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	source, err := store.Create(workspace, "test/local/source", "rev")
	if err != nil {
		t.Fatal(err)
	}
	secret := "sk-test-retry-intent-must-not-copy-this-opening"
	opening := provider.UserText("repair using " + secret)
	if err := source.AppendMessage(opening); err != nil {
		t.Fatal(err)
	}
	if err := source.AppendMessage(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{Text: "done"}}}); err != nil {
		t.Fatal(err)
	}

	child, err := store.ForkSessionForRetryStaged(source, 0)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := child.AppendRetryIntent(source.ID(), 0, opening, "t2", "test/local/target", strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if intent.OwnerSessionID != child.ID() || intent.SourceSessionID != source.ID() || intent.Status != RetryIntentPending {
		t.Fatalf("intent = %#v", intent)
	}
	if err := child.StartRetryIntent(intent.ID); err == nil || !bytes.Contains([]byte(err.Error()), []byte("before the staged child is published")) {
		t.Fatalf("pre-publication StartRetryIntent error = %v", err)
	}
	childBytes, err := os.ReadFile(child.Path())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(childBytes, []byte(secret)) {
		t.Fatal("retry intent copied secret-bearing opening bytes into the child log")
	}
	if err := child.Publish(); err != nil {
		t.Fatal(err)
	}
	if err := child.StartRetryIntent(intent.ID); err == nil {
		t.Fatal("retry execution started without a recorded opening")
	}
	if err := child.AppendMessage(opening); err == nil {
		t.Fatal("pending retry accepted an unbound ordinary opening")
	}
	markedOpening := provider.CloneMessage(opening)
	markedOpening.RetryIntentID = intent.ID
	if err := child.AppendMessage(markedOpening); err != nil {
		t.Fatal(err)
	}
	// The source outcome is intentionally later than publication. Latest must
	// still select the child that owns the unresolved workspace handoff.
	if err := source.AppendNote("info", "retry committed"); err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}

	resumed, err := store.Latest(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if resumed.ID() != child.ID() {
		t.Fatalf("Latest resumed %s, want retry child %s", resumed.ID(), child.ID())
	}
	if got := resumed.State().RetryIntent; got == nil || got.Status != RetryIntentPending || got.ID != intent.ID {
		t.Fatalf("replayed pending intent = %#v", got)
	}
	if err := resumed.StartRetryIntent(intent.ID); err != nil {
		t.Fatal(err)
	}
	if got := resumed.State().RetryIntent; got == nil || got.Status != RetryIntentStarted {
		t.Fatalf("started intent = %#v", got)
	}
	if err := resumed.CompleteRetryIntent(intent.ID); err != nil {
		t.Fatal(err)
	}
	if got := resumed.State().RetryIntent; got != nil {
		t.Fatalf("completed intent remained active: %#v", got)
	}
}

func TestLatestKeepsRetryOwnershipFromOneInventorySnapshot(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	source, err := store.Create(workspace, "test/local/source", "rev")
	if err != nil {
		t.Fatal(err)
	}
	opening := provider.UserText("complete this retry between inventory and selection")
	if err := source.AppendMessage(opening); err != nil {
		t.Fatal(err)
	}
	child, err := store.ForkSessionForRetryStaged(source, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Close() })
	intent, err := child.AppendRetryIntent(source.ID(), 0, opening, "t2", "test/local/target", strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Publish(); err != nil {
		t.Fatal(err)
	}
	markedOpening := provider.CloneMessage(opening)
	markedOpening.RetryIntentID = intent.ID
	if err := child.AppendMessage(markedOpening); err != nil {
		t.Fatal(err)
	}
	// Keep the source first by mtime even after the child completes. The old
	// two-scan implementation combined this ordering with the child's later
	// completed status and resumed the source.
	sourceID, sourcePath, childID := source.ID(), source.Path(), child.ID()
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(sourcePath, time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	infos, err := store.List(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 || infos[0].ID != sourceID {
		t.Fatalf("retry interleaving fixture order = %+v, want source %s first", infos, sourceID)
	}
	var childPending bool
	for _, info := range infos {
		if info.ID == childID && info.Health.RetryIntent == RetryIntentPending {
			childPending = true
		}
	}
	if !childPending {
		t.Fatalf("retry child %s was not pending in the inventory: %+v", childID, infos)
	}

	listed := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseLatest := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseLatest()
	store.latestAfterList = func() {
		close(listed)
		<-release
	}
	type latestResult struct {
		session *Session
		err     error
	}
	result := make(chan latestResult, 1)
	go func() {
		sess, latestErr := store.Latest(workspace)
		result <- latestResult{session: sess, err: latestErr}
	}()
	select {
	case <-listed:
	case <-time.After(5 * time.Second):
		t.Fatal("Latest did not reach its post-inventory selection boundary")
	}

	transitionErr := child.StartRetryIntent(intent.ID)
	if transitionErr == nil {
		transitionErr = child.CompleteRetryIntent(intent.ID)
	}
	closeErr := child.Close()
	releaseLatest()
	var got latestResult
	select {
	case got = <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("Latest did not return after retry completion")
	}
	if transitionErr != nil {
		t.Fatalf("completing retry during Latest selection: %v", transitionErr)
	}
	if closeErr != nil {
		t.Fatalf("closing completed retry child: %v", closeErr)
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	defer got.session.Close()
	if got.session.ID() != childID {
		t.Fatalf("Latest resumed %s, want retry child %s selected by the captured ownership snapshot", got.session.ID(), childID)
	}
	if got.session.State().RetryIntent != nil {
		t.Fatalf("resumed child retained completed retry intent: %#v", got.session.State().RetryIntent)
	}
}

func TestRetrySourceReadIsReadOnlyAcrossATornTail(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	source, err := store.Create(workspace, "test/local/source", "rev")
	if err != nil {
		t.Fatal(err)
	}
	opening := provider.UserText("read this coordinate without repairing the log")
	if err := source.AppendMessage(opening); err != nil {
		t.Fatal(err)
	}
	id, path := source.ID(), source.Path()
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("0000"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.ReadRetrySourceOpening(id, workspace, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.AuthoredText() != opening.AuthoredText() {
		t.Fatalf("source opening = %#v", got)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("retry source inspection repaired or otherwise mutated the source log")
	}
}

func TestUnresolvedRetryRefusesAmbiguousAndCorruptChildren(t *testing.T) {
	t.Run("ambiguous", func(t *testing.T) {
		store, _ := NewStore(t.TempDir())
		workspace := t.TempDir()
		source, err := store.Create(workspace, "test/local/source", "rev")
		if err != nil {
			t.Fatal(err)
		}
		defer source.Close()
		opening := provider.UserText("same source")
		if err := source.AppendMessage(opening); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			child, err := store.ForkSessionForRetryStaged(source, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := child.AppendRetryIntent(source.ID(), 0, opening, "t1", "test/local/target", strings.Repeat("d", 64)); err != nil {
				t.Fatal(err)
			}
			if err := child.Publish(); err != nil {
				t.Fatal(err)
			}
			if err := child.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, _, err := store.UnresolvedRetry(workspace); err == nil || !strings.Contains(err.Error(), "refusing to guess") {
			t.Fatalf("ambiguous unresolved retry = %v", err)
		}
		if _, err := store.Latest(workspace); err == nil || !strings.Contains(err.Error(), "refusing to guess") {
			t.Fatalf("Latest guessed across unresolved children: %v", err)
		}
	})

	t.Run("corrupt active", func(t *testing.T) {
		store, _ := NewStore(t.TempDir())
		workspace := t.TempDir()
		source, err := store.Create(workspace, "test/local/source", "rev")
		if err != nil {
			t.Fatal(err)
		}
		opening := provider.UserText("corrupt child")
		if err := source.AppendMessage(opening); err != nil {
			t.Fatal(err)
		}
		child, err := store.ForkSessionForRetryStaged(source, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := child.AppendRetryIntent(source.ID(), 0, opening, "t1", "test/local/target", strings.Repeat("e", 64)); err != nil {
			t.Fatal(err)
		}
		if err := child.Publish(); err != nil {
			t.Fatal(err)
		}
		path := child.Path()
		if err := child.Close(); err != nil {
			t.Fatal(err)
		}
		if err := source.Close(); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString("00000002 00000000 {}\n"); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := store.UnresolvedRetry(workspace); err == nil || !strings.Contains(err.Error(), "corrupt or unreadable") {
			t.Fatalf("corrupt unresolved retry = %v", err)
		}
	})
}

func TestForkStripsCompletedRetryOpeningCapability(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	workspace := t.TempDir()
	source, err := store.Create(workspace, "test/local/source", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	opening := provider.UserText("retry marker stays physical")
	if err := source.AppendMessage(opening); err != nil {
		t.Fatal(err)
	}
	child, err := store.ForkSessionForRetryStaged(source, 0)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := child.AppendRetryIntent(source.ID(), 0, opening, "t1", "test/local/target", strings.Repeat("f", 64))
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Publish(); err != nil {
		t.Fatal(err)
	}
	marked := provider.CloneMessage(opening)
	marked.RetryIntentID = intent.ID
	if err := child.AppendMessage(marked); err != nil {
		t.Fatal(err)
	}
	if err := child.StartRetryIntent(intent.ID); err != nil {
		t.Fatal(err)
	}
	if err := child.CompleteRetryIntent(intent.ID); err != nil {
		t.Fatal(err)
	}
	derived, err := store.ForkSession(child, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer derived.Close()
	if got := derived.State(); got.RetryIntent != nil || len(got.Messages) != 1 || got.Messages[0].RetryIntentID != "" {
		t.Fatalf("derived retry authority = intent %#v messages %#v", got.RetryIntent, got.Messages)
	}
}

func TestRetryIntentForkFilteringAndForeignOwnerCompatibility(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	workspace := t.TempDir()
	source, err := store.Create(workspace, "test/local/source", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	opening := provider.UserText("retry exactly")
	if err := source.AppendMessage(opening); err != nil {
		t.Fatal(err)
	}
	child, err := store.ForkSessionForRetryStaged(source, 0)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := child.AppendRetryIntent(source.ID(), 0, opening, "t1", "test/local/target", strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Publish(); err != nil {
		t.Fatal(err)
	}

	if _, err := store.ForkSessionForRetryStaged(child, 0); !errors.Is(err, ErrRetryIntentUnresolved) {
		t.Fatalf("fork through unresolved retry = %v", err)
	}
	if err := child.AbandonRetryIntent(intent.ID); err != nil {
		t.Fatal(err)
	}
	derived, err := store.ForkSessionForRetryStaged(child, 0)
	if err != nil {
		t.Fatal(err)
	}
	if derived.State().RetryIntent != nil {
		t.Fatal("fork copied source retry execution authority")
	}
	_ = derived.CloseDiscardingStaged()

	// Simulate an older schema-5 copier that treated retry_intent as an unknown
	// record and carried it. A newer reader recognizes the foreign owner and
	// treats the record as inert padding rather than corrupting the derived log.
	legacyDerived, err := store.CreateStaged(workspace, "test/local/derived", "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := legacyDerived.append(RecordRetryIntent, intent); err != nil {
		t.Fatal(err)
	}
	if err := legacyDerived.Publish(); err != nil {
		t.Fatal(err)
	}
	id := legacyDerived.ID()
	if err := legacyDerived.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.OpenInWorkspace(id, workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.State().RetryIntent != nil {
		t.Fatal("foreign copied retry intent became executable")
	}
}

func TestRetryIntentValidatesCutOpeningAndExplicitAbandon(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	workspace := t.TempDir()
	child, err := store.CreateStaged(workspace, "test/local/child", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer child.CloseDiscardingStaged()
	opening := provider.UserText("exact")
	if _, err := child.AppendRetryIntent(child.ID(), 0, opening, "t1", "test/local/target", strings.Repeat("c", 64)); err == nil {
		t.Fatal("self-sourced retry intent succeeded")
	}
	if _, err := child.AppendRetryIntent("20260823T120000-abcdef12", 1, opening, "t1", "test/local/target", strings.Repeat("c", 64)); err == nil {
		t.Fatal("retry intent with a cut beyond the child succeeded")
	}
	injected := opening
	injected.Injected = true
	if _, err := child.AppendRetryIntent("20260823T120000-abcdef12", 0, injected, "t1", "test/local/target", strings.Repeat("c", 64)); err == nil {
		t.Fatal("injected retry opening succeeded")
	}
	legacy := provider.Message{Role: provider.RoleUser, Content: []provider.Block{provider.Text{Text: "legacy"}}}
	if _, err := child.AppendRetryIntent("20260823T120000-abcdef12", 0, legacy, "t1", "test/local/target", strings.Repeat("c", 64)); err == nil {
		t.Fatal("opening without authored projection succeeded")
	}
	intent, err := child.AppendRetryIntent("20260823T120000-abcdef12", 0, opening, "t1", "test/local/target", strings.Repeat("c", 64))
	if err != nil {
		t.Fatal(err)
	}
	if err := child.AbandonRetryIntent(intent.ID); err != nil {
		t.Fatal(err)
	}
	if child.State().RetryIntent != nil {
		t.Fatal("explicit abandon left retry handoff active")
	}
	if err := child.StartRetryIntent(intent.ID); err == nil {
		// The exact error text is not API; the transition must simply remain
		// unavailable after its one durable resolution.
		t.Fatal("abandoned retry intent started again")
	}
}
