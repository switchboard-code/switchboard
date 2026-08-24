//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/switchboard-code/switchboard/internal/checkpoint"
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

func TestWindowsHistoryDACLRepairReopensExactRootHandleAcrossPathSubstitution(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.hist")
	moved := filepath.Join(dir, "selected.hist")
	created, err := openHistoryDataDescriptor(path, true, true)
	if err != nil {
		t.Fatal(err)
	}
	world, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;WD)")
	if err != nil {
		_ = created.Close()
		t.Fatal(err)
	}
	dacl, _, err := world.DACL()
	if err != nil {
		_ = created.Close()
		t.Fatal(err)
	}
	if err := windows.SetSecurityInfo(windows.Handle(created.Fd()), windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		_ = created.Close()
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	limited, err := root.OpenFile("history.hist", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer limited.Close()
	if err := os.Rename(path, moved); err != nil {
		t.Fatalf("moving selected history before exact-handle repair: %v", err)
	}
	replacement, err := openHistoryDataDescriptor(path, true, true)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if err := windows.SetSecurityInfo(windows.Handle(replacement.Fd()), windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	if err := secureHistoryFile(limited); err != nil {
		t.Fatalf("repairing DACL through root handle without WRITE_DAC: %v", err)
	}
	if private, err := historyFileIsOwnerOnly(limited); err != nil || !private {
		t.Fatalf("repaired history owner-only=%v err=%v", private, err)
	}
	if private, err := historyFileIsOwnerOnly(replacement); err != nil || private {
		t.Fatalf("replacement history owner-only=%v err=%v; exact-handle repair touched the path replacement", private, err)
	}
}

func TestWindowsBoundHistoryStageUsesProtectedDACLInCreateCall(t *testing.T) {
	dir := t.TempDir()
	directoryName, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		t.Fatal(err)
	}
	directoryHandle, err := windows.CreateFile(directoryName,
		windows.READ_CONTROL|windows.WRITE_DAC|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal(err)
	}
	directory := os.NewFile(uintptr(directoryHandle), dir+" (test DACL mutation)")
	if directory == nil {
		_ = windows.CloseHandle(directoryHandle)
		t.Fatal("converting test directory DACL handle")
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

func TestWindowsBoundHistoryStageAllowsExactHandlePublicationLease(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	stage, err := createHistoryBoundPrivateFile(root, "stage")
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	if _, err := stage.WriteString("private prompt\n"); err != nil {
		t.Fatal(err)
	}
	if err := stage.Sync(); err != nil {
		t.Fatal(err)
	}
	outcome, err := checkpoint.MoveOpenFileNoReplace(root, stage, "stage", "history")
	if err != nil || !outcome.Published {
		t.Fatalf("publishing selected history stage = %+v, %v", outcome, err)
	}
	if _, err := root.Lstat("stage"); !os.IsNotExist(err) {
		t.Fatalf("history stage remains after publication: %v", err)
	}
	stored, err := root.Open("history")
	if err != nil {
		t.Fatal(err)
	}
	defer stored.Close()
	if ownerOnly, err := historyFileIsOwnerOnly(stored); err != nil || !ownerOnly {
		t.Fatalf("published history owner-only=%v err=%v", ownerOnly, err)
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
