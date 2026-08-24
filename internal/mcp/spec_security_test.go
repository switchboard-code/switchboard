package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSpecsRefusesOversizeDeclaration(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, SpecFileName)
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxSpecFileBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if specs, err := LoadSpecs(path); err == nil || len(specs) != 0 {
		t.Fatalf("oversize declaration = %+v, %v", specs, err)
	}
}
