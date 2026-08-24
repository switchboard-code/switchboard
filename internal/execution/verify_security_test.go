package execution

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadCheckRefusesOversizeCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sandbox-check.json")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxCheckCacheBytes + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readCheck(path); err == nil {
		t.Fatal("oversize sandbox cache was accepted")
	}
}
