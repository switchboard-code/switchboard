package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestScrolledTranscriptStaysAnchoredWhileOutputStreams(t *testing.T) {
	tr := testTranscript(t, 40)
	for i := 0; i < 8; i++ {
		tr.add(&entry{kind: kindInfo, text: "recorded line"})
	}
	live := tr.add(&entry{kind: kindAssistant, text: "live answer", live: true})

	tr.view(5)
	tr.scrollBy(3)
	before := tr.view(5)
	beforeOffset := tr.offset

	tr.appendText(tr.indexOf(live), "\n"+strings.Repeat("more streamed output ", 6))
	after := tr.view(5)

	if after != before {
		t.Fatalf("streamed output moved a scrolled viewport:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if tr.offset <= beforeOffset {
		t.Fatalf("offset stayed at %d after the tail grew; want more than %d to preserve the viewport", tr.offset, beforeOffset)
	}
}

func TestTranscriptStillFollowsStreamingOutputAtBottom(t *testing.T) {
	tr := testTranscript(t, 40)
	live := tr.add(&entry{kind: kindAssistant, text: "live answer", live: true})
	tr.view(3)

	tr.appendText(tr.indexOf(live), "\nnew tail")

	if tr.offset != 0 {
		t.Fatalf("bottom-following transcript moved to offset %d", tr.offset)
	}
	if got := tr.view(3); !strings.Contains(got, "new tail") {
		t.Fatalf("bottom-following viewport omitted new output:\n%s", got)
	}
}

func TestScrolledTranscriptKeepsSemanticAnchorAcrossWidthReflow(t *testing.T) {
	tr := testTranscript(t, 31)
	var body strings.Builder
	body.WriteString("keyboard and mouse help\n")
	for i := 0; i < 8; i++ {
		fmt.Fprintf(&body, "before-%02d %s\n", i, strings.Repeat("navigation detail ", 4))
	}
	body.WriteString("ANCHOR_MOUSE wheel scrolls and dragging selects exact transcript lines\n")
	for i := 0; i < 18; i++ {
		fmt.Fprintf(&body, "after-%02d %s\n", i, strings.Repeat("later detail ", 5))
	}
	tr.add(&entry{kind: kindInfo, text: body.String()})

	const height = 6
	tr.view(height)
	anchorLine := -1
	for i, line := range tr.flat {
		if strings.Contains(ansi.Strip(line), "ANCHOR_MOUSE") {
			anchorLine = i
			break
		}
	}
	if anchorLine < 0 {
		t.Fatal("fixture did not render the semantic anchor")
	}
	tr.offset = len(tr.flat) - anchorLine - height
	tr.clampOffset()
	if tr.offset == 0 {
		t.Fatal("fixture did not scroll away from bottom")
	}
	if first := strings.Split(ansi.Strip(tr.view(height)), "\n")[0]; !strings.Contains(first, "ANCHOR_MOUSE") {
		t.Fatalf("fixture anchor is not on the first row: %q", first)
	}

	for _, width := range []int{80, 20, 120, 31} {
		tr.setWidth(width)
		visible := strings.Split(ansi.Strip(tr.view(height)), "\n")
		if !strings.Contains(visible[0], "ANCHOR_MOUSE") {
			t.Fatalf("width %d lost semantic top row (offset %d):\n%s", width, tr.offset, strings.Join(visible, "\n"))
		}
		if tr.offset == 0 {
			t.Fatalf("width %d unexpectedly resumed bottom-follow", width)
		}
	}
}

func TestWidthReflowKeepsBottomFollowAndSubsequentStreamingSemantics(t *testing.T) {
	tr := testTranscript(t, 31)
	for i := 0; i < 12; i++ {
		tr.add(&entry{kind: kindInfo, text: fmt.Sprintf("history %02d %s", i, strings.Repeat("detail ", 4))})
	}
	live := tr.add(&entry{kind: kindAssistant, text: "live tail", live: true})
	const height = 5
	tr.view(height)
	tr.setWidth(80)
	if tr.offset != 0 {
		t.Fatalf("bottom-following resize moved to offset %d", tr.offset)
	}
	if got := ansi.Strip(tr.view(height)); !strings.Contains(got, "live tail") {
		t.Fatalf("bottom-following resize omitted the tail:\n%s", got)
	}

	tr.scrollBy(4)
	before := tr.view(height)
	tr.setWidth(40)
	tr.view(height) // settle the semantic reflow target before streaming
	anchored := tr.view(height)
	tr.appendText(tr.indexOf(live), "\n"+strings.Repeat("new output ", 12))
	if after := tr.view(height); after != anchored {
		t.Fatalf("streaming after reflow moved the scrolled viewport:\npre-resize:\n%s\nanchored:\n%s\nafter:\n%s", before, anchored, after)
	}
}

func TestTruncateKeepsExtendedGraphemeClustersAtomic(t *testing.T) {
	clusters := []string{"e\u0301", "👩‍💻", "👍🏽", "🇺🇸", "🏳️‍🌈"}
	for _, cluster := range clusters {
		t.Run(fmt.Sprintf("%x", []byte(cluster)), func(t *testing.T) {
			if got := truncate(cluster, 1); got != cluster {
				t.Fatalf("one grapheme truncated to %q", got)
			}
			if got := truncate(cluster+"x", 1); got != cluster+"…" {
				t.Fatalf("grapheme prefix truncated to %q", got)
			}
			if got := truncate("A"+cluster+"B", 2); got != "A"+cluster+"…" {
				t.Fatalf("middle grapheme truncated to %q", got)
			}
		})
	}
}
