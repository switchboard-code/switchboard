package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRefusesOversizeDeclaration(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, FileName)
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxHookFileBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if set, err := Load(path, root); err == nil || set != nil {
		t.Fatalf("oversize declaration = %+v, %v", set, err)
	}
}
