package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/tools"
)

// The platform gate is the whole registration decision: on macOS the tool
// is present because osascript always is, and everywhere else the model
// never sees it — absent, not broken.
func TestComputerJoinsTheSuiteOnlyOnDarwin(t *testing.T) {
	registry, err := tools.NewRegistry(t.TempDir(), execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	addComputerUse(registry)
	_, present := registry.Get("computer")
	if want := runtime.GOOS == "darwin"; present != want {
		t.Fatalf("computer present=%v on %s, want %v", present, runtime.GOOS, want)
	}
}

func TestComputerExecutableIgnoresPATHShadow(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("computer use is a macOS capability")
	}
	shadow := filepath.Join(t.TempDir(), "osascript")
	if err := os.WriteFile(shadow, []byte("#!/bin/sh\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(shadow))

	got, ok := computerExecutable()
	if !ok {
		t.Fatal("fixed macOS osascript was not detected")
	}
	if got != osascriptExecutable {
		t.Fatalf("computer executable = %q, want fixed system path %q", got, osascriptExecutable)
	}
}
