//go:build darwin

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/switchboard-code/switchboard/internal/fileprivacy"
)

func TestDarwinHistoryProtectionStripsExtendedACLs(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "history")
	if err := fileprivacy.EnsurePrivateDir(parent); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "workspace.hist")
	f, err := openHistoryDataDescriptor(path, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := secureHistoryFile(f); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("chmod", "+a", "everyone allow read", path).CombinedOutput(); err != nil {
		t.Fatalf("adding Darwin ACL: %v: %s", err, output)
	}
	f, err = openHistoryDataDescriptor(path, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if ownerOnly, err := historyFileIsOwnerOnly(f); err != nil || ownerOnly {
		_ = f.Close()
		t.Fatalf("extended-ACL history owner-only=%v err=%v", ownerOnly, err)
	}
	if err := secureHistoryFile(f); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if ownerOnly, err := historyFileIsOwnerOnly(f); err != nil || !ownerOnly {
		_ = f.Close()
		t.Fatalf("repaired history owner-only=%v err=%v", ownerOnly, err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if output, err := exec.Command("chmod", "+a", "everyone allow list,search", parent).CombinedOutput(); err != nil {
		t.Fatalf("adding Darwin directory ACL: %v: %s", err, output)
	}
	if err := appendHistoryPrompt(filepath.Join(parent, "other.hist"), "prompt", nil); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open(parent)
	if err != nil {
		t.Fatal(err)
	}
	ownerOnly, ownerErr := fileprivacy.DirectoryIsOwnerOnly(dir)
	closeErr := dir.Close()
	if ownerErr != nil || closeErr != nil || !ownerOnly {
		t.Fatalf("history parent owner-only=%v ownerErr=%v closeErr=%v", ownerOnly, ownerErr, closeErr)
	}
}
