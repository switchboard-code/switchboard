package agent

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// This repository is a monorepo-shaped checkout with a large AGENTS.md at its
// root, which is exactly the case the byte slice used to mangle. Running the
// composer against the real tree is the check a fixture cannot make.
func TestComposesThisRepositoryWithoutMangling(t *testing.T) {
	text, ok := ProjectInstructions("../../internal/tools")
	if !ok {
		t.Skip("this test only means something inside the checkout")
	}
	if !utf8.ValidString(text) {
		t.Error("the composed instructions are not valid UTF-8")
	}
	if len(text) > maxInstructionBytes {
		t.Errorf("composed to %d bytes, past the %d budget", len(text), maxInstructionBytes)
	}
	if !strings.Contains(text, "AGENTS.md") {
		t.Errorf("the repository's own instructions were not found:\n%s", text)
	}
	// The budget binds here, and a reader has to be told rather than left to
	// wonder why the rules stop mid-document.
	if !strings.Contains(text, "budget") {
		t.Errorf("a truncated composition did not say so:\n%s", text[max(0, len(text)-300):])
	}
}
