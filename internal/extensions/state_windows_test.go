//go:build windows

package extensions

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/switchboard-code/switchboard/internal/fileprivacy"
)

func TestOpenRejectsPluginStateReadableByEveryoneOnWindows(t *testing.T) {
	path := filepath.Join(t.TempDir(), StateFileName)
	f, err := fileprivacy.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"version":1,"activations":[]}`); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	world, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;WD)")
	if err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	dacl, _, err := world.DACL()
	if err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := windows.SetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStateFile(path); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("OpenStateFile Everyone DACL = %v", err)
	}
}

func TestPluginStateMutationUsesProtectedCurrentUserDACLOnWindows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", StateFileName)
	state, err := OpenStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	plugin := testPlugin(t, filepath.Join(t.TempDir(), "plugin"), "windows-private-state", false)
	if err := state.Enable(testActivationCandidate(t, plugin), ""); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{path, path + ".lock"} {
		file, err := fileprivacy.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		ownerOnly, ownerErr := fileprivacy.IsOwnerOnly(file)
		closeErr := file.Close()
		if ownerErr != nil || closeErr != nil || !ownerOnly {
			t.Fatalf("%s owner-only=%v ownerErr=%v closeErr=%v", name, ownerOnly, ownerErr, closeErr)
		}
	}
	directory, err := openPluginStateDirectory(path)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := directory.journalRoot.Open(".")
	if err != nil {
		_ = directory.close()
		t.Fatal(err)
	}
	ownerOnly, ownerErr := fileprivacy.DirectoryIsOwnerOnly(journal)
	closeErr := errors.Join(journal.Close(), directory.close())
	if ownerErr != nil || closeErr != nil || !ownerOnly {
		t.Fatalf("journal owner-only=%v ownerErr=%v closeErr=%v", ownerOnly, ownerErr, closeErr)
	}
}
