//go:build darwin

package extensions

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestDarwinInstallProtectionRejectsAndRepairsExtendedACLs(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "cache")
	if err := prepareInstallCacheDirectory(cache); err != nil {
		t.Fatal(err)
	}
	addInstallDarwinACL(t, cache, "everyone allow list,search")
	info, err := os.Stat(cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateInstallCacheDirectory(cache, info); err == nil {
		t.Fatal("cache with an extended ACL passed privacy validation")
	}
	if err := prepareInstallCacheDirectory(cache); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateInstallCacheDirectory(cache, info); err != nil {
		t.Fatalf("repaired cache: %v", err)
	}

	rootPath := filepath.Join(cache, "objects")
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.Mkdir("plugin", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := securePrivateInstallDirectory(root, "plugin"); err != nil {
		t.Fatal(err)
	}
	dirPath := filepath.Join(rootPath, "plugin")
	addInstallDarwinACL(t, dirPath, "everyone allow list,search")
	dirInfo, err := os.Stat(dirPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := validatePrivateInstallDirectory(root, "plugin", dirInfo); err == nil {
		t.Fatal("installed directory with an extended ACL passed privacy validation")
	}
	if err := securePrivateInstallDirectory(root, "plugin"); err != nil {
		t.Fatal(err)
	}
	dirInfo, _ = os.Stat(dirPath)
	if err := validatePrivateInstallDirectory(root, "plugin", dirInfo); err != nil {
		t.Fatalf("repaired installed directory: %v", err)
	}

	filePath := filepath.Join(rootPath, "plugin", "run")
	if err := os.WriteFile(filePath, []byte("payload"), 0o700); err != nil {
		t.Fatal(err)
	}
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		t.Fatal(err)
	}
	addInstallDarwinACL(t, filePath, "everyone allow read")
	fileInfo, _ = os.Stat(filePath)
	pluginRoot, err := root.OpenRoot("plugin")
	if err != nil {
		t.Fatal(err)
	}
	defer pluginRoot.Close()
	if err := validatePrivateInstallFile(pluginRoot, "run", fileInfo, true); err == nil {
		t.Fatal("installed file with an extended ACL passed privacy validation")
	}
	f, err := pluginRoot.OpenFile("run", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := securePrivateInstallFile(pluginRoot, "run", f, true); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	fileInfo, _ = os.Stat(filePath)
	if err := validatePrivateInstallFile(pluginRoot, "run", fileInfo, true); err != nil {
		t.Fatalf("repaired installed file: %v", err)
	}
}

func addInstallDarwinACL(t *testing.T, path, rule string) {
	t.Helper()
	if output, err := exec.Command("chmod", "+a", rule, path).CombinedOutput(); err != nil {
		t.Fatalf("adding Darwin ACL: %v: %s", err, output)
	}
}
