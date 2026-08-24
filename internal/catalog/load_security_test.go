package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestApplyOverridesRefusesOversizeFile(t *testing.T) {
	c, err := loadBundled()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), UserOverrideFile)
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxUserOverrideBytes + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.applyOverrides(path); err == nil {
		t.Fatal("oversize model override was accepted")
	}
}
