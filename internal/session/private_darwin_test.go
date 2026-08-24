//go:build darwin

package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/switchboard-code/switchboard/internal/fileprivacy"
)

func TestDarwinSessionProtectionStripsInheritedExtendedACLs(t *testing.T) {
	dirPath := filepath.Join(t.TempDir(), "sessions")
	if err := ensurePrivateSessionDirectory(dirPath); err != nil {
		t.Fatal(err)
	}
	addSessionDarwinACL(t, dirPath, "everyone allow list,search")
	if err := ensurePrivateSessionDirectory(dirPath); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open(dirPath)
	if err != nil {
		t.Fatal(err)
	}
	ownerOnly, ownerErr := fileprivacy.DirectoryIsOwnerOnly(dir)
	closeErr := dir.Close()
	if ownerErr != nil || closeErr != nil || !ownerOnly {
		t.Fatalf("secured session directory owner-only=%v ownerErr=%v closeErr=%v", ownerOnly, ownerErr, closeErr)
	}

	filePath := filepath.Join(dirPath, "session.jsonl")
	f, err := createPrivateSessionFile(filePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	addSessionDarwinACL(t, filePath, "everyone allow read")
	f, err = os.OpenFile(filePath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ownerOnly, err := privateSessionFileIsOwnerOnly(f); err != nil || ownerOnly {
		_ = f.Close()
		t.Fatalf("extended-ACL session file owner-only=%v err=%v", ownerOnly, err)
	}
	if err := securePrivateSessionFile(f); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if ownerOnly, err := privateSessionFileIsOwnerOnly(f); err != nil || !ownerOnly {
		_ = f.Close()
		t.Fatalf("repaired session file owner-only=%v err=%v", ownerOnly, err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func addSessionDarwinACL(t *testing.T, path, rule string) {
	t.Helper()
	if output, err := exec.Command("chmod", "+a", rule, path).CombinedOutput(); err != nil {
		t.Fatalf("adding Darwin ACL: %v: %s", err, output)
	}
}
