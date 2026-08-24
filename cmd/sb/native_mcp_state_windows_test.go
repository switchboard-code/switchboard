//go:build windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/switchboard-code/switchboard/internal/fileprivacy"
)

var windowsNativeMCPTestReOpenFile = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReOpenFile")

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
	// The lock can already exist because opening the deliberately unsafe state
	// file acquires it before rejecting that state. OpenReadWriteOrCreate then
	// returns its intentionally least-privileged existing-file handle, which
	// has no WRITE_DAC. Reopen the exact selected file for the fixture mutation
	// instead of weakening the production lock handle's access mask.
	result, _, callErr := windowsNativeMCPTestReOpenFile.Call(
		f.Fd(),
		uintptr(windows.WRITE_DAC),
		uintptr(uint32(windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)),
		uintptr(uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)),
	)
	runtime.KeepAlive(f)
	handle := windows.Handle(result)
	if handle == windows.InvalidHandle {
		if callErr == nil || callErr == windows.ERROR_SUCCESS {
			callErr = windows.ERROR_INVALID_HANDLE
		}
		t.Fatal(callErr)
	}
	mutation := os.NewFile(uintptr(handle), f.Name()+" (test DACL mutation)")
	if mutation == nil {
		_ = windows.CloseHandle(handle)
		t.Fatal("converting native MCP test DACL handle")
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;WD)")
	if err != nil {
		_ = mutation.Close()
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		_ = mutation.Close()
		t.Fatal(err)
	}
	if err := windows.SetSecurityInfo(windows.Handle(mutation.Fd()), windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		_ = mutation.Close()
		t.Fatal(err)
	}
	if err := mutation.Close(); err != nil {
		t.Fatal(err)
	}
}
