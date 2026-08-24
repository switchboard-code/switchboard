package main

import (
	"strings"
	"testing"
)

func TestClipboardPinsSystemToolAndScrubsChildEnvironment(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SB_CLIPBOARD_TOKEN", "must-not-reach-pbcopy")
	t.Setenv("DYLD_INSERT_LIBRARIES", "/workspace/must-not-load.dylib")

	cmd := systemClipboardCommand()
	if cmd.Path != pbcopyToolPath {
		t.Fatalf("clipboard path = %q, want pinned %q", cmd.Path, pbcopyToolPath)
	}
	for _, entry := range cmd.Env {
		if strings.Contains(entry, "must-not-reach-pbcopy") || strings.HasPrefix(entry, "SB_CLIPBOARD_TOKEN=") {
			t.Fatalf("credential-bearing environment reached pbcopy: %q", entry)
		}
		if strings.HasPrefix(entry, "DYLD_INSERT_LIBRARIES=") {
			t.Fatalf("loader injection reached pbcopy: %q", entry)
		}
	}
}
