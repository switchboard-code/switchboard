//go:build windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/switchboard-code/switchboard/internal/fileprivacy"
)

func TestWindowsNativeMCPStateAndLockUseOwnerOnlyDACLs(t *testing.T) {
	path := filepath.Join(t.TempDir(), nativeMCPStateFileName)
	state, err := openNativeMCPActivationStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.mutate(context.Background(), func(latest *nativeMCPActivationState) (bool, error) {
		latest.key = make([]byte, 32)
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	assertWindowsNativeMCPFileOwnerOnly(t, path)
	assertWindowsNativeMCPDirectoryOwnerOnly(t, filepath.Join(filepath.Dir(path), nativeMCPStateRecoveryDirName))

	lock, err := acquireNativeMCPStateFileLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := lock.path
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	assertWindowsNativeMCPFileOwnerOnly(t, lockPath)
}

func TestWindowsNativeMCPStateAndLockRejectEveryoneDACL(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), nativeMCPStateFileName)
	stateFile, err := fileprivacy.Create(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stateFile.WriteString(`{"version":1,"key":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","activations":[]}`); err != nil {
		_ = stateFile.Close()
		t.Fatal(err)
	}
	setWindowsNativeMCPFileEveryoneDACL(t, stateFile)
	if err := stateFile.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := openNativeMCPActivationStateFile(statePath); err == nil || !strings.Contains(err.Error(), "owner-only") {
		t.Fatalf("open Everyone state DACL = %v", err)
	}

	lockPath := statePath + ".lock"
	lockFile, _, err := fileprivacy.OpenReadWriteOrCreate(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	setWindowsNativeMCPFileEveryoneDACL(t, lockFile)
	if err := lockFile.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireNativeMCPStateFileLock(context.Background(), statePath); err == nil || !strings.Contains(err.Error(), "current-user-only") {
		t.Fatalf("acquire Everyone lock DACL = %v", err)
	}
}

func assertWindowsNativeMCPFileOwnerOnly(t *testing.T, path string) {
	t.Helper()
	f, err := fileprivacy.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ownerOnly, ownerErr := fileprivacy.IsOwnerOnly(f)
	closeErr := f.Close()
	if ownerErr != nil || closeErr != nil || !ownerOnly {
		t.Fatalf("%s owner-only=%v check=%v close=%v", path, ownerOnly, ownerErr, closeErr)
	}
}

func assertWindowsNativeMCPDirectoryOwnerOnly(t *testing.T, path string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ownerOnly, ownerErr := fileprivacy.DirectoryIsOwnerOnly(f)
	closeErr := f.Close()
	if ownerErr != nil || closeErr != nil || !ownerOnly {
		t.Fatalf("%s owner-only directory=%v check=%v close=%v", path, ownerOnly, ownerErr, closeErr)
	}
}

func setWindowsNativeMCPFileEveryoneDACL(t *testing.T, f *os.File) {
	t.Helper()
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;WD)")
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
}
