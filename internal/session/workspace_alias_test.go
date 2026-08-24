package session

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
)

func legacyWorkspaceAlias(t *testing.T, target string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("directory symlink creation is not generally available to unprivileged Windows tests")
	}
	alias := filepath.Join(t.TempDir(), "workspace-alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	return alias
}

func createLegacyAliasSession(t *testing.T, store *Store, alias string) (string, string) {
	t.Helper()
	sess, err := store.Create(alias, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.UserText("resume through the historical alias")); err != nil {
		t.Fatal(err)
	}
	id, path := sess.ID(), sess.Path()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	return id, path
}

func TestLegacyWorkspaceAliasIsListedResumedAndDurablyRebound(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	canonical := t.TempDir()
	alias := legacyWorkspaceAlias(t, canonical)
	id, path := createLegacyAliasSession(t, store, alias)

	infos, err := store.List(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].ID != id {
		t.Fatalf("canonical inventory = %+v, want legacy alias session %s", infos, id)
	}

	resumed, err := store.Latest(canonical)
	if err != nil {
		t.Fatalf("continue through canonical workspace: %v", err)
	}
	state := resumed.State()
	if state.Workspace != alias || state.WorkspaceBinding != canonical {
		t.Fatalf("resumed workspace provenance/binding = %q / %q, want %q / %q",
			state.Workspace, state.WorkspaceBinding, alias, canonical)
	}
	if err := resumed.Close(); err != nil {
		t.Fatal(err)
	}

	// The successful writable boundary made the canonical identity durable.
	// Discovery and explicit resume no longer depend on the obsolete alias.
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	infos, err = store.List(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Path != path {
		t.Fatalf("inventory after alias removal = %+v, want %s", infos, path)
	}
	resumed, err = store.OpenInWorkspace(id, canonical)
	if err != nil {
		t.Fatalf("explicit resume after alias removal: %v", err)
	}
	if resumed.State().WorkspaceBinding != canonical {
		t.Fatalf("replayed canonical binding = %q", resumed.State().WorkspaceBinding)
	}
	fork, err := store.ForkSession(resumed, len(resumed.State().Messages))
	if err != nil {
		t.Fatalf("forking durably rebound session after alias removal: %v", err)
	}
	if fork.State().Workspace != canonical {
		t.Fatalf("fork workspace = %q, want canonical %q", fork.State().Workspace, canonical)
	}
	if err := fork.Close(); err != nil {
		t.Fatal(err)
	}
	if err := resumed.Close(); err != nil {
		t.Fatal(err)
	}

	// Generic ID-based readers and forks also have to honor the effective
	// binding; they cannot fall back to requiring the now-missing start alias.
	resumed, err = store.Open(id)
	if err != nil {
		t.Fatalf("generic open after alias removal: %v", err)
	}
	if err := resumed.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyWorkspaceAliasMustStillProveTheSelectedDirectory(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	original := t.TempDir()
	other := t.TempDir()
	alias := legacyWorkspaceAlias(t, original)
	id, _ := createLegacyAliasSession(t, store, alias)

	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, alias); err != nil {
		t.Fatal(err)
	}
	if infos, err := store.List(original); err != nil || len(infos) != 0 {
		t.Fatalf("retargeted alias inventory = %+v, err=%v; want no match", infos, err)
	}
	if opened, err := store.OpenInWorkspace(id, original); err == nil {
		_ = opened.Close()
		t.Fatal("explicit resume accepted a retargeted legacy alias")
	}

	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if infos, err := store.List(original); err != nil || len(infos) != 0 {
		t.Fatalf("missing alias inventory = %+v, err=%v; want no match", infos, err)
	}
	if opened, err := store.OpenInWorkspace(id, original); err == nil {
		_ = opened.Close()
		t.Fatal("explicit resume accepted a missing unbound legacy alias")
	}
}

func TestExactWorkspaceBoundaryStillWorksWithoutFilesystemIdentity(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "workspace-that-does-not-exist")
	sess, err := store.Create(missing, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	id := sess.ID()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	infos, err := store.List(missing)
	if err != nil || len(infos) != 1 || infos[0].ID != id {
		t.Fatalf("exact missing workspace inventory = %+v, err=%v", infos, err)
	}
	opened, err := store.OpenInWorkspace(id, missing)
	if err != nil {
		t.Fatalf("exact historical boundary was narrowed: %v", err)
	}
	defer opened.Close()
	if opened.State().WorkspaceBinding != "" {
		t.Fatalf("exact boundary wrote an unnecessary canonical binding %q", opened.State().WorkspaceBinding)
	}
}

func TestWorkspaceBindingCannotBeChanged(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	canonical := t.TempDir()
	alias := legacyWorkspaceAlias(t, canonical)
	id, _ := createLegacyAliasSession(t, store, alias)
	opened, err := store.OpenInWorkspace(id, canonical)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	other := t.TempDir()
	if err := opened.AppendWorkspaceBinding(other); err == nil || !strings.Contains(err.Error(), "already bound") {
		t.Fatalf("workspace binding change = %v", err)
	}
}
