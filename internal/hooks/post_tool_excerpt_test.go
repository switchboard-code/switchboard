package hooks

import (
	"strings"
	"testing"
)

func TestPostToolResultExcerptPreservesCompleteValuesAndHidesBoundaryFragments(t *testing.T) {
	token := "ghp_" + strings.Repeat("A", 36)

	inside := "ordinary prefix " + token + " ordinary suffix"
	if got := postToolResultExcerpt(inside); got != inside {
		t.Fatalf("complete in-cap hook input changed:\n got %q\nwant %q", got, inside)
	}

	ordinary := strings.Repeat("x", maxHookOutput+32)
	if got, want := postToolResultExcerpt(ordinary), ordinary[:maxHookOutput]; got != want {
		t.Fatalf("ordinary bounded hook input changed:\n got %q\nwant %q", got, want)
	}

	const visibleBeforeOldCut = 8
	straddling := strings.Repeat("x", maxHookOutput-visibleBeforeOldCut) + token + " tail"
	got := postToolResultExcerpt(straddling)
	if strings.Contains(got, "ghp_") || strings.Contains(got, token[:visibleBeforeOldCut]) {
		t.Fatalf("boundary excerpt retained a credential fragment: %q", got)
	}
	if !strings.Contains(got, "[redacted: a GitHub token]") {
		t.Fatalf("boundary excerpt lost its redaction marker: %q", got)
	}
	if !strings.HasPrefix(got, strings.Repeat("x", maxHookOutput-visibleBeforeOldCut)) {
		t.Fatal("boundary handling changed ordinary bytes before the credential")
	}
}
