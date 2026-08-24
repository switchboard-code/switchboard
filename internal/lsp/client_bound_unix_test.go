//go:build unix

package lsp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/switchboard-code/switchboard/internal/safeexec"
)

func TestBoundLanguageServerRefusesReplacementBeforeLazyStart(t *testing.T) {
	root := t.TempDir()
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "executed")
	path := filepath.Join(bin, "fixture-ls")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf "+body+" > '"+marker+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("original")
	executable, err := safeexec.ResolvePathOutside(path, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	write("replacement")

	_, err = startBoundWithProblems(
		context.Background(), executable, []string{executable.Path()}, root,
		[]string{"PATH=/usr/bin:/bin"}, NewProblemStore(root),
	)
	if !errors.Is(err, safeexec.ErrChanged) {
		t.Fatalf("replacement error = %v, want ErrChanged", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("replaced language server executed: %v", statErr)
	}
}
