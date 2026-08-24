//go:build windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/config"
	"golang.org/x/sys/windows"
)

func makeCodexAuthNonPrivateForTest(path string) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.READ_CONTROL|windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;WD)")
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil)
}

func TestWindowsCodexCredentialHelperDispatchAndPersistedConfig(t *testing.T) {
	home := t.TempDir()
	isolateTestHome(t, home)
	writePrivateCodexAuth(t, home, []byte(`{"tokens":{"access_token":"windows-token"}}`))

	var out strings.Builder
	handled, err := runCLISubcommand(context.Background(), &out, options{},
		[]string{codexCredentialHelperCommand, codexCredentialHelperKind})
	if !handled || err != nil || out.String() != "windows-token\n" {
		t.Fatalf("dispatch handled=%v err=%v stdout=%q", handled, err, out.String())
	}

	cfg := &config.Config{Path: filepath.Join(t.TempDir(), config.FileName)}
	if err := wireCodex(cfg, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	saved, err := config.LoadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	helper := saved.AuthFor("openai").Helper
	if len(helper) != 3 || helper[1] != codexCredentialHelperCommand || helper[2] != codexCredentialHelperKind {
		t.Fatalf("persisted Windows helper = %v", helper)
	}
	for _, arg := range helper {
		if strings.Contains(strings.ToLower(arg), "python") || strings.Contains(strings.ToLower(arg), "auth.json") {
			t.Fatalf("persisted Windows helper contains an interpreter or secret path: %v", helper)
		}
	}
}

func TestWindowsCodexCredentialHelperAcceptsTypicalInheritedProfileACL(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		t.Fatalf("current user SID: %v", err)
	}
	// Codex does an ordinary create/truncate on Windows. A normal profile file
	// therefore inherits allow ACEs for the user, LocalSystem, and built-in
	// Administrators rather than Switchboard's protected single-user DACL.
	descriptor, err := windows.SecurityDescriptorFromString("O:" + user.User.Sid.String() +
		"D:(A;ID;FA;;;" + user.User.Sid.String() + ")(A;ID;FA;;;SY)(A;ID;FA;;;BA)")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := codexWindowsDescriptorIsAcceptable(descriptor); err != nil || !ok {
		t.Fatalf("typical inherited Codex ACL acceptable=%v err=%v", ok, err)
	}

	broad, err := windows.SecurityDescriptorFromString("O:" + user.User.Sid.String() +
		"D:(A;ID;FA;;;" + user.User.Sid.String() + ")(A;ID;FA;;;SY)(A;ID;FA;;;BA)(A;ID;FR;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := codexWindowsDescriptorIsAcceptable(broad); err != nil || ok {
		t.Fatalf("world-readable Codex ACL acceptable=%v err=%v", ok, err)
	}
}

func TestWindowsCodexCredentialHelperRefusesReparsePoint(t *testing.T) {
	home := t.TempDir()
	isolateTestHome(t, home)
	authPath := writePrivateCodexAuth(t, home, []byte(`{"tokens":{"access_token":"secret"}}`))
	target := filepath.Join(home, ".codex", "target.json")
	if err := os.Rename(authPath, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, authPath); err != nil {
		t.Skipf("creating a Windows symlink requires developer mode or privilege: %v", err)
	}

	var out strings.Builder
	if err := runCredentialHelperDispatch(&out, []string{codexCredentialHelperKind}); err == nil {
		t.Fatal("reparse-point Codex login was accepted")
	}
	if out.Len() != 0 {
		t.Fatalf("reparse-point Codex login wrote stdout: %q", out.String())
	}
}
