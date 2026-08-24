//go:build windows

package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsSessionArtifactsUseCurrentUserOnlyDACLs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions", "nested")
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	assertWindowsPrivateSessionDirectory(t, root)

	workspace := t.TempDir()
	dir, err := store.WorkspaceDir(workspace)
	if err != nil {
		t.Fatal(err)
	}
	assertWindowsPrivateSessionDirectory(t, dir)

	sess, err := store.CreateStaged(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	path := sess.Path()
	assertWindowsPrivateSessionFile(t, path)
	if outcome, err := sess.PublishDurably(); err != nil || !outcome.Visible || !outcome.Durable {
		_ = sess.Close()
		t.Fatalf("PublishDurably() = %+v, %v", outcome, err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	assertWindowsPrivateSessionFile(t, publicationMarkerPath(path))

	// WorkspaceDir is also the creation seam for schedules, retry journals,
	// and checkpoint cleanup ledgers. Its inheritable ACE must keep an ordinary
	// child creation current-user-only before a later store open makes the
	// child's DACL explicit and protected.
	journal := filepath.Join(dir, ".switchboard-retry-transaction")
	if err := os.WriteFile(journal, []byte("private recovery state"), 0o600); err != nil {
		t.Fatal(err)
	}
	journalFile, err := openPrivateSessionWindowsObject(journal, false, false)
	if err != nil {
		t.Fatal(err)
	}
	ownerOnly, ownerErr := windowsSessionDACLHasOnlyCurrentUser(journalFile)
	closeErr := journalFile.Close()
	if ownerErr != nil || closeErr != nil || !ownerOnly {
		t.Fatalf("inherited journal DACL current-user-only = %v, err=%v close=%v", ownerOnly, ownerErr, closeErr)
	}

	if _, err := NewStore(root); err != nil {
		t.Fatal(err)
	}
	assertWindowsPrivateSessionFile(t, journal)
}

func TestWindowsNewStoreRepairsLegacyBroadArtifactDACLs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	dir, err := store.WorkspaceDir(workspace)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.CreateStaged(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	path := sess.Path()
	if outcome, err := sess.PublishDurably(); err != nil || !outcome.Visible {
		_ = sess.Close()
		t.Fatalf("PublishDurably() = %+v, %v", outcome, err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	marker := publicationMarkerPath(path)
	journal := filepath.Join(dir, "schedule.json")
	if err := os.WriteFile(journal, []byte("[]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, target := range []struct {
		path      string
		directory bool
	}{
		{root, true},
		{dir, true},
		{path, false},
		{marker, false},
		{journal, false},
	} {
		setWindowsSessionWorldDACL(t, target.path, target.directory)
	}

	if _, err := NewStore(root); err != nil {
		t.Fatal(err)
	}
	assertWindowsPrivateSessionDirectory(t, root)
	assertWindowsPrivateSessionDirectory(t, dir)
	assertWindowsPrivateSessionFile(t, path)
	assertWindowsPrivateSessionFile(t, marker)
	assertWindowsPrivateSessionFile(t, journal)
}

func TestWindowsSessionPrivacyMigrationInventoryIsBounded(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	if err := ensurePrivateSessionDirectory(root); err != nil {
		t.Fatal(err)
	}
	for i := range 8 {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("artifact-%02d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := securePrivateSessionTree(root, 8); err != nil {
		t.Fatalf("exact-bound privacy migration: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "artifact-08"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := securePrivateSessionTree(root, 8); !errors.Is(err, ErrSessionInventoryTooLarge) {
		t.Fatalf("over-bound privacy migration error = %v", err)
	}
}

func TestWindowsWritableSessionOpenRepairsBroadLogDACL(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	id, path := sess.ID(), sess.Path()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	setWindowsSessionWorldDACL(t, path, false)

	reopened, err := store.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	assertWindowsPrivateSessionFile(t, path)
}

func TestWindowsSessionRemovalTempDirectoryIsProtected(t *testing.T) {
	parent := t.TempDir()
	path, err := createPrivateSessionTempDir(parent, ".session-remove-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(path)
	assertWindowsPrivateSessionDirectory(t, path)

	child := filepath.Join(path, "entry")
	if err := os.WriteFile(child, []byte("private removed session"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openPrivateSessionWindowsObject(child, false, false)
	if err != nil {
		t.Fatal(err)
	}
	ownerOnly, ownerErr := windowsSessionDACLHasOnlyCurrentUser(f)
	closeErr := f.Close()
	if ownerErr != nil || closeErr != nil || !ownerOnly {
		t.Fatalf("temporary child DACL current-user-only = %v, err=%v close=%v", ownerOnly, ownerErr, closeErr)
	}
}

func TestWindowsSessionPrivacyRequiresCurrentUserAsObjectOwner(t *testing.T) {
	current, err := currentSessionWindowsUserSID()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString("O:WDD:P(A;;FA;;;" + current.String() + ")")
	if err != nil {
		t.Fatal(err)
	}
	if ownerOnly, err := privateSessionWindowsDescriptorIsOwnerOnly(descriptor, current, false); err != nil || ownerOnly {
		t.Fatalf("foreign-owner descriptor owner-only=%v err=%v", ownerOnly, err)
	}
	if owned, err := privateSessionWindowsDescriptorIsCurrentUserOwner(descriptor, current); err != nil || owned {
		t.Fatalf("foreign-owner descriptor current-owned=%v err=%v", owned, err)
	}
}

func assertWindowsPrivateSessionDirectory(t *testing.T, path string) {
	t.Helper()
	f, err := openPrivateSessionWindowsObject(path, true, false)
	if err != nil {
		t.Fatal(err)
	}
	ownerOnly, ownerErr := privateSessionWindowsObjectIsOwnerOnly(f, true)
	closeErr := f.Close()
	if ownerErr != nil || closeErr != nil || !ownerOnly {
		t.Fatalf("directory %s protected current-user-only = %v, err=%v close=%v", path, ownerOnly, ownerErr, closeErr)
	}
}

func assertWindowsPrivateSessionFile(t *testing.T, path string) {
	t.Helper()
	f, err := openPrivateSessionWindowsObject(path, false, false)
	if err != nil {
		t.Fatal(err)
	}
	ownerOnly, ownerErr := privateSessionFileIsOwnerOnly(f)
	closeErr := f.Close()
	if ownerErr != nil || closeErr != nil || !ownerOnly {
		t.Fatalf("file %s protected current-user-only = %v, err=%v close=%v", path, ownerOnly, ownerErr, closeErr)
	}
}

func setWindowsSessionWorldDACL(t *testing.T, path string, directory bool) {
	t.Helper()
	f, err := openPrivateSessionWindowsObject(path, directory, true)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	flags := ""
	if directory {
		flags = "OICI"
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;" + flags + ";FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	if ownerOnly, err := privateSessionWindowsObjectIsOwnerOnly(f, directory); err != nil || ownerOnly {
		t.Fatalf("Everyone DACL on %s classified current-user-only = %v, err=%v", path, ownerOnly, err)
	}
}

func windowsSessionDACLHasOnlyCurrentUser(f *os.File) (bool, error) {
	current, err := currentSessionWindowsUserSID()
	if err != nil {
		return false, err
	}
	descriptor, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return false, err
	}
	if dacl == nil || dacl.AceCount != 1 {
		return false, nil
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		return false, err
	}
	if ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
		return false, nil
	}
	sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	return sid.IsValid() && sid.Equals(current), nil
}
