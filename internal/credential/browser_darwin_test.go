package credential

import (
	"strings"
	"testing"
)

func TestBrowserPinsSystemOpenAndScrubsChildEnvironment(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SB_BROWSER_TOKEN", value)

	cmd, err := browserCommand("https://example.invalid/authorize")
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Path != browserOpenPath {
		t.Fatalf("browser opener path = %q, want pinned %q", cmd.Path, browserOpenPath)
	}
	for _, entry := range cmd.Env {
		if strings.Contains(entry, value) || strings.HasPrefix(entry, "SB_BROWSER_TOKEN=") {
			t.Fatalf("credential-bearing environment reached browser opener: %q", entry)
		}
	}
}
