package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOwnedRemoveMismatchRestoresWithoutOverwrite(t *testing.T) {
	for _, lateCollision := range []bool{false, true} {
		name := "restore"
		if lateCollision {
			name = "late-collision"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "session.log")
			original := filepath.Join(dir, "original.log")
			if err := os.WriteFile(path, []byte("owned original"), 0o600); err != nil {
				t.Fatal(err)
			}
			owner, expected := openedOwnedRemoveIdentity(t, path)
			defer owner.Close()
			err := os.Rename(path, original)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("foreign replacement"), 0o600); err != nil {
				t.Fatal(err)
			}
			if lateCollision {
				ownedRemoveBeforeRestoreTestHook = func(got string) {
					if got != path {
						t.Errorf("restore hook path = %q, want %q", got, path)
					}
					if writeErr := os.WriteFile(path, []byte("late contender"), 0o600); writeErr != nil {
						t.Errorf("create late contender: %v", writeErr)
					}
				}
				defer func() { ownedRemoveBeforeRestoreTestHook = nil }()
			}

			err = removePathIfSame(path, expected)
			if err == nil || !strings.Contains(err.Error(), "changed before identity-checked removal") {
				t.Fatalf("mismatch removal error = %v", err)
			}
			originalBytes, readErr := os.ReadFile(original)
			if readErr != nil || string(originalBytes) != "owned original" {
				t.Fatalf("owned original changed: %q, %v", originalBytes, readErr)
			}

			got, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !lateCollision {
				if string(got) != "foreign replacement" {
					t.Fatalf("restored replacement = %q", got)
				}
				assertNoOwnedRemoveQuarantine(t, dir)
				return
			}
			if string(got) != "late contender" {
				t.Fatalf("atomic restore overwrote late contender with %q", got)
			}
			matches, globErr := filepath.Glob(filepath.Join(dir, ".session-remove-*", "entry"))
			if globErr != nil || len(matches) != 1 {
				t.Fatalf("recovery evidence = %v, %v", matches, globErr)
			}
			retained, readErr := os.ReadFile(matches[0])
			if readErr != nil || string(retained) != "foreign replacement" {
				t.Fatalf("retained replacement = %q, %v", retained, readErr)
			}
			if !strings.Contains(err.Error(), "retained at "+matches[0]) {
				t.Fatalf("collision did not name recovery evidence: %v", err)
			}
		})
	}
}

func TestOwnedRemoveExactMatchLeavesNoRecoveryArtifact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.log")
	if err := os.WriteFile(path, []byte("owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	owner, expected := openedOwnedRemoveIdentity(t, path)
	defer owner.Close()
	if err := removePathIfSame(path, expected); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned path still exists: %v", err)
	}
	assertNoOwnedRemoveQuarantine(t, dir)
}

// removePathIfSame accepts a descriptor-derived identity. That distinction is
// observable on Windows: os.Lstat defers loading a file ID until SameFile and
// would try to resolve the old pathname only after removePathIfSame quarantines
// it. Keeping this descriptor open also verifies that the Windows opener grants
// delete sharing, as maintenance does while it owns the session lock.
func openedOwnedRemoveIdentity(t *testing.T, path string) (*os.File, os.FileInfo) {
	t.Helper()
	f, err := openSessionLogDescriptor(path, false)
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	return f, info
}

func assertNoOwnedRemoveQuarantine(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".session-remove-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("owned removal left recovery artifacts: %v", matches)
	}
}
