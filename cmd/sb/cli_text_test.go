package main

import (
	"errors"
	"strings"
	"testing"
)

func TestCLITextRedactsBeforeEscapingTerminalControls(t *testing.T) {
	token := "ghp_" + strings.Repeat("a", 36)
	unsafe := "first\nsecond\x1b]2;spoof\a\u202eright " + token
	got := cliText(unsafe)
	if strings.Contains(got, token) || !strings.Contains(got, "[redacted: a GitHub token]") {
		t.Fatalf("CLI text exposed a credential: %q", got)
	}
	for _, control := range []string{"\n", "\x1b", "\a", "\u202e"} {
		if strings.Contains(got, control) {
			t.Fatalf("CLI text retained terminal control %q: %q", control, got)
		}
	}
	for _, visible := range []string{`\x0a`, `\x1b`, `\x07`, `\u202e`} {
		if !strings.Contains(got, visible) {
			t.Fatalf("CLI text did not visibly escape %q: %q", visible, got)
		}
	}
}

func TestWriteCLIErrorUsesTheSafeBoundary(t *testing.T) {
	token := "ghp_" + strings.Repeat("b", 36)
	var out strings.Builder
	if code := writeCLIError(&out, errors.New("bad\n\x1b]8;;https://spoof\a"+token)); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	got := out.String()
	if strings.Contains(got, token) || strings.Contains(got, "\x1b") || strings.Count(got, "\n") != 1 {
		t.Fatalf("CLI error crossed its output boundary: %q", got)
	}
	if !strings.Contains(got, "[redacted: a GitHub token]") || !strings.Contains(got, `\x1b`) {
		t.Fatalf("CLI error lost its safe diagnostic: %q", got)
	}
}

func TestRendererStyleEscapesUntrustedMetadataBeforeAddingSGR(t *testing.T) {
	renderer := &renderer{color: false}
	got := renderer.style(dim, "label\n\x1b]8;;https://spoof\a\u202eright")
	for _, control := range []string{"\n", "\x1b", "\a", "\u202e"} {
		if strings.Contains(got, control) {
			t.Fatalf("styled metadata retained terminal control %q: %q", control, got)
		}
	}
	for _, visible := range []string{`\x0a`, `\x1b`, `\x07`, `\u202e`} {
		if !strings.Contains(got, visible) {
			t.Fatalf("styled metadata did not visibly escape %q: %q", visible, got)
		}
	}
}
