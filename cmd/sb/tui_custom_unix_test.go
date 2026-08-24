//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCustomCommandsRejectFIFOAndDevZeroWithoutBlocking(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ws := t.TempDir()
	dir := filepath.Join(ws, ".switchboard", "commands")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(dir, "pipe.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/zero", filepath.Join(dir, "zero.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	started := time.Now()
	commands, notes := loadCustomCommandsWithNotes(ws)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("special-file rejection blocked for %v", elapsed)
	}
	if len(commands) != 0 {
		t.Fatalf("special-file definitions loaded: %+v", commands)
	}
	if got := strings.Join(notes, "\n"); !strings.Contains(got, "ignored 2 unsafe or invalid workspace custom command files") {
		t.Fatalf("special-file rejection summary missing: %q", got)
	}
}
