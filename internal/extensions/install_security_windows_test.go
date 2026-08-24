//go:build windows

package extensions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func assertInstallPlatformProtection(t *testing.T, path string, _ os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	assertWindowsPrivateInstallPath(t, path, info.IsDir())
}

func TestWindowsInstallDescriptorRejectsForeignOwner(t *testing.T) {
	current, err := currentInstallWindowsUserSID()
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range []bool{false, true} {
		flags := ""
		if directory {
			flags = "OICI"
		}
		foreign, err := windows.SecurityDescriptorFromString(
			"O:SYD:P(A;" + flags + ";FA;;;" + current.String() + ")")
		if err != nil {
			t.Fatal(err)
		}
		ownerOnly, err := installWindowsDescriptorIsOwnerOnly(foreign, current, directory)
		if err != nil {
			t.Fatal(err)
		}
		if ownerOnly {
			t.Fatalf("foreign-owned descriptor classified current-user-only (directory=%v)", directory)
		}

		owned, err := privateInstallWindowsDescriptor(directory)
		if err != nil {
			t.Fatal(err)
		}
		ownerOnly, err = installWindowsDescriptorIsOwnerOnly(owned, current, directory)
		if err != nil || !ownerOnly {
			t.Fatalf("current-user descriptor ownerOnly=%v err=%v (directory=%v)", ownerOnly, err, directory)
		}
	}
}

func TestWindowsInstallProtectsCacheAndPreservesExecutableSemantics(t *testing.T) {
	source := makePlugin(t, DialectCodex, `{"name":"windows-private","mcpServers":{"local":{"command":"never-run"}}}`)
	script := filepath.Join(source, "bin", "tool.exe")
	mustWriteInstallFile(t, script, []byte("not really an executable"), 0o777)
	mustWriteInstallFile(t, filepath.Join(source, "nested", "regular.txt"), []byte("private"), 0o666)
	plugin := discoverInstallPlugin(t, source, ScopeUser, DialectCodex)
	if !plugin.Executable || len(plugin.Components) != 1 || !plugin.Components[0].Executable {
		t.Fatalf("source executable capability = %#v", plugin)
	}
	sourceInfo, err := os.Stat(script)
	if err != nil {
		t.Fatal(err)
	}
	if installSourceExecutable(sourceInfo) {
		t.Fatal("synthetic Windows FileMode was treated as a portable execute bit")
	}

	cache := filepath.Join(t.TempDir(), "cache", "nested")
	installed, err := Install(plugin, cache)
	if err != nil {
		t.Fatal(err)
	}
	if installed.Digest != plugin.Digest || !installed.Executable ||
		len(installed.Components) != 1 || !installed.Components[0].Executable {
		t.Fatalf("installed executable semantics changed: %#v", installed)
	}
	assertWindowsPrivateInstallTree(t, cache)

	again, err := Install(plugin, cache)
	if err != nil {
		t.Fatal(err)
	}
	if again.RealPath != installed.RealPath || again.Digest != installed.Digest || !again.Executable {
		t.Fatalf("idempotent Windows install changed identity: %#v %#v", installed, again)
	}
}

func TestWindowsInstallSecuresExistingBroadCacheRoot(t *testing.T) {
	root := makePlugin(t, DialectCodex, `{"name":"windows-cache-root"}`)
	plugin := discoverInstallPlugin(t, root, ScopeUser, DialectCodex)
	cache := t.TempDir()
	setWindowsInstallWorldDACL(t, cache, true)

	if _, err := Install(plugin, cache); err != nil {
		t.Fatal(err)
	}
	assertWindowsPrivateInstallPath(t, cache, true)
}

func TestWindowsInstallRejectsBroadOrHardLinkedCachedFile(t *testing.T) {
	t.Run("broad DACL", func(t *testing.T) {
		root := makePlugin(t, DialectCodex, `{"name":"windows-broad-file"}`)
		mustWriteInstallFile(t, filepath.Join(root, "data"), []byte("original"), 0o600)
		plugin := discoverInstallPlugin(t, root, ScopeUser, DialectCodex)
		cache := t.TempDir()
		installed, err := Install(plugin, cache)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(installed.RealPath, "data")
		setWindowsInstallWorldDACL(t, path, false)

		if _, err := Install(plugin, cache); err == nil ||
			(!strings.Contains(err.Error(), "private") && !strings.Contains(err.Error(), "DACL")) {
			t.Fatalf("Install() error = %v, want broad-DACL rejection", err)
		}
		file, err := openInstallWindowsObject(path, false, windows.READ_CONTROL)
		if err != nil {
			t.Fatal(err)
		}
		ownerOnly, ownerErr := installWindowsObjectIsOwnerOnly(file, false)
		closeErr := file.Close()
		if ownerErr != nil || closeErr != nil || ownerOnly {
			t.Fatalf("broad cached file was modified: ownerOnly=%v err=%v close=%v", ownerOnly, ownerErr, closeErr)
		}
	})

	t.Run("hard link", func(t *testing.T) {
		root := makePlugin(t, DialectCodex, `{"name":"windows-hard-link"}`)
		mustWriteInstallFile(t, filepath.Join(root, "data"), []byte("original"), 0o600)
		plugin := discoverInstallPlugin(t, root, ScopeUser, DialectCodex)
		cache := t.TempDir()
		installed, err := Install(plugin, cache)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(installed.RealPath, "data")
		linked := filepath.Join(t.TempDir(), "linked")
		if err := os.Link(path, linked); err != nil {
			t.Fatal(err)
		}
		if _, err := Install(plugin, cache); err == nil || !strings.Contains(err.Error(), "hard links") {
			t.Fatalf("Install() error = %v, want hard-link rejection", err)
		}
		if _, err := os.Stat(linked); err != nil {
			t.Fatalf("rejected hard link was removed: %v", err)
		}
	})
}

func assertWindowsPrivateInstallTree(t *testing.T, root string) {
	t.Helper()
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		assertWindowsPrivateInstallPath(t, path, entry.IsDir())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func assertWindowsPrivateInstallPath(t *testing.T, path string, directory bool) {
	t.Helper()
	file, err := openInstallWindowsObject(path, directory, windows.READ_CONTROL)
	if err != nil {
		t.Fatal(err)
	}
	ownerOnly, ownerErr := installWindowsObjectIsOwnerOnly(file, directory)
	closeErr := file.Close()
	if ownerErr != nil || closeErr != nil || !ownerOnly {
		t.Fatalf("%s protected current-user-only = %v, err=%v close=%v", path, ownerOnly, ownerErr, closeErr)
	}
}

func setWindowsInstallWorldDACL(t *testing.T, path string, directory bool) {
	t.Helper()
	file, err := openInstallWindowsObject(path, directory, windows.READ_CONTROL|windows.WRITE_DAC)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	flags := ""
	if directory {
		flags = "OICI"
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;" + flags + ";FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	if ownerOnly, err := installWindowsObjectIsOwnerOnly(file, directory); err != nil || ownerOnly {
		t.Fatalf("Everyone DACL on %s ownerOnly=%v err=%v", path, ownerOnly, err)
	}
}
