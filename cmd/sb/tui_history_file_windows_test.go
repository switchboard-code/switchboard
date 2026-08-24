//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsHistoryDACLIsVerifiedAndNarrowed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.hist")
	f, err := openHistoryDataDescriptor(path, true, true)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if ownerOnly, err := historyFileIsOwnerOnly(f); err != nil || !ownerOnly {
		t.Fatalf("new history file owner-only = %v, err=%v", ownerOnly, err)
	}

	world, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := world.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	if ownerOnly, err := historyFileIsOwnerOnly(f); err != nil || ownerOnly {
		t.Fatalf("Everyone DACL classified owner-only = %v, err=%v", ownerOnly, err)
	}

	if err := secureHistoryFile(f); err != nil {
		t.Fatal(err)
	}
	if ownerOnly, err := historyFileIsOwnerOnly(f); err != nil || !ownerOnly {
		t.Fatalf("narrowed history file owner-only = %v, err=%v", ownerOnly, err)
	}
}

func TestWindowsHistoryCreateUsesProtectedDACLBeforeWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.hist")
	f, err := openHistoryDataDescriptor(path, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("private prompt\n"); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	opened, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if ownerOnly, err := historyFileIsOwnerOnly(opened); err != nil || !ownerOnly {
		t.Fatalf("persisted history owner-only = %v, err=%v", ownerOnly, err)
	}
}

func TestWindowsBoundHistoryStageUsesProtectedDACLInCreateCall(t *testing.T) {
	dir := t.TempDir()
	directory, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	world, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;WD)")
	if err != nil {
		_ = directory.Close()
		t.Fatal(err)
	}
	dacl, _, err := world.DACL()
	if err != nil {
		_ = directory.Close()
		t.Fatal(err)
	}
	if err := windows.SetSecurityInfo(windows.Handle(directory.Fd()), windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		_ = directory.Close()
		t.Fatal(err)
	}
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	stage, err := createHistoryBoundPrivateFile(root, ".history-create-test")
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	if ownerOnly, err := historyFileIsOwnerOnly(stage); err != nil || !ownerOnly {
		t.Fatalf("root-relative stage owner-only at create return = %v, err=%v", ownerOnly, err)
	}
}

func TestWindowsHistoryOwnerOnlyRequiresCurrentUserAsObjectOwner(t *testing.T) {
	current, err := currentHistoryUserSID()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString("O:WDD:P(A;;FA;;;" + current.String() + ")")
	if err != nil {
		t.Fatal(err)
	}
	if ownerOnly, err := historyWindowsDescriptorIsOwnerOnly(descriptor, current); err != nil || ownerOnly {
		t.Fatalf("foreign-owner descriptor owner-only=%v err=%v", ownerOnly, err)
	}
	if owned, err := historyWindowsDescriptorIsCurrentUserOwner(descriptor, current); err != nil || owned {
		t.Fatalf("foreign-owner descriptor current-owned=%v err=%v", owned, err)
	}
}
