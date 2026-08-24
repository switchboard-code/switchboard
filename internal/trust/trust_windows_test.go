//go:build windows

package trust

import (
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/switchboard-code/switchboard/internal/fileprivacy"
)

func TestOpenRejectsTrustStoreReadableByEveryoneOnWindows(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	f, err := fileprivacy.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("[workspaces]\n"); err != nil {
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
	if _, err := OpenFile(path); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("OpenFile Everyone DACL = %v", err)
	}
}
