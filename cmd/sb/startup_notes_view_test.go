package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/switchboard-code/switchboard/internal/config"
)

func TestStartupNoteFloodKeepsSmallTUIUsable(t *testing.T) {
	m := testModel(t)
	var notes []mcpNote
	for i := 0; i < 100; i++ {
		notes = append(notes, mcpNote{
			level: "error",
			text: fmt.Sprintf("plugins: malformed native extension %03d %s", i,
				strings.Repeat("descriptive-diagnostic ", 10)),
		})
	}
	report := aggregateStartupNotes(notes)
	m.app.startupNotes = report
	m.addBanner(m.app.loop.Session, false)
	addStartupNoteReport(m, report)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	view := stripANSI(m.View())
	for _, wanted := range []string{
		"switchboard",
		"ollama/local/test:7b",
		"describe a task",
		"extensions: 100 startup notes",
		"95 more in /doctor extensions",
	} {
		if !strings.Contains(view, wanted) {
			t.Errorf("80x24 startup lost %q:\n%s", wanted, view)
		}
	}
	if got := strings.Count(strings.Join(m.tr.flat, "\n"), "malformed native extension"); got != startupNoncriticalHighlightLimit {
		t.Fatalf("startup rendered %d ordinary failures, want hard limit %d", got, startupNoncriticalHighlightLimit)
	}
}

func TestStartupNotesViewHonorsTinyPhysicalViewport(t *testing.T) {
	v := newStartupNotesView(startupNoteReport{Details: []mcpNote{{text: strings.Repeat("界", 20)}}})
	for _, width := range []int{1, 2, 10} {
		for _, height := range []int{1, 2} {
			view := v.view(width, height, darkTheme())
			assertTUIViewBounds(t, view, width, height)
		}
	}
}

func TestDoctorExtensionsOpensEveryOrderedSanitizedDetail(t *testing.T) {
	m := testModel(t)
	input := []mcpNote{
		{"warn", "first\x1b]0;spoof\x07"},
		{"error", "second"},
		{"error", "second"},
		{"", "last " + strings.Repeat("word ", 40)},
	}
	m.app.startupNotes = aggregateStartupNotes(input)
	if cmd := cmdDoctor(m, "extensions"); cmd != nil {
		t.Fatal("static extension diagnostics unexpectedly launched asynchronous work")
	}
	view, ok := m.full.(*startupNotesView)
	if !ok {
		t.Fatalf("fullscreen = %T, want startupNotesView", m.full)
	}
	joined := strings.Join(view.lines, "\n")
	for _, wanted := range []string{`first\x1b]0;spoof\x07`, "second", "last word"} {
		if !strings.Contains(joined, wanted) {
			t.Errorf("detail surface lost %q:\n%s", wanted, joined)
		}
	}
	if got := strings.Count(joined, "second"); got != 2 {
		t.Fatalf("duplicate detail rendered %d times, want 2:\n%s", got, joined)
	}
	if strings.Index(joined, `first\x1b]0;spoof\x07`) > strings.Index(joined, "second") ||
		strings.LastIndex(joined, "second") > strings.Index(joined, "last word") {
		t.Fatalf("details did not retain discovery order:\n%s", joined)
	}
	if strings.ContainsAny(joined, "\x1b\x07") {
		t.Fatalf("detail surface retained raw terminal controls: %q", joined)
	}

	visual := strings.Join(view.visualLines(36), "\n")
	for _, wanted := range []string{"last", "word"} {
		if !strings.Contains(visual, wanted) {
			t.Fatalf("narrow fullscreen hid wrapped detail %q:\n%s", wanted, visual)
		}
	}
}

func TestREPLStartupAndDoctorDetailWritersShareExactReport(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "startup-notes")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	out := newRenderer(file)

	var input []mcpNote
	for i := 0; i < 100; i++ {
		input = append(input, mcpNote{"error", fmt.Sprintf("MCP: ordinary failure %03d", i)})
	}
	report := aggregateStartupNotes(input)
	writeStartupNoteReport(out, report)
	writeStartupNoteDetails(out, report)
	out.flush()
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if got := strings.Count(strings.Split(text, "extension startup diagnostics")[0], "ordinary failure"); got != startupNoncriticalHighlightLimit {
		t.Fatalf("REPL startup rendered %d ordinary failures, want %d:\n%s", got, startupNoncriticalHighlightLimit, text)
	}
	for i := 0; i < 100; i++ {
		wanted := fmt.Sprintf("MCP: ordinary failure %03d", i)
		if strings.Count(text, wanted) < 1 {
			t.Fatalf("REPL detail surface lost %q", wanted)
		}
	}
}

func TestREPLDoctorExtensionsPrintsStoredStartupDetails(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "repl-doctor")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	report := aggregateStartupNotes([]mcpNote{
		{"warn", "plugins: first diagnostic"},
		{"error", "MCP: second diagnostic"},
		{"error", "MCP: second diagnostic"},
	})
	r := &repl{
		out:          newRenderer(file),
		config:       &config.Config{},
		startupNotes: report,
	}
	if done := r.command(context.Background(), "/doctor extensions"); done {
		t.Fatal("/doctor extensions exited the REPL")
	}
	r.out.flush()
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Count(text, "second diagnostic") != 2 ||
		strings.Index(text, "first diagnostic") > strings.Index(text, "second diagnostic") {
		t.Fatalf("REPL /doctor extensions lost exact ordered duplicates:\n%s", text)
	}
}

func TestStartupAttachSeparatesBufferedReportFromLiveNotices(t *testing.T) {
	state := &mcpState{}
	state.add(mcpNote{"error", "plugins: buffered startup failure"})
	live := make(chan mcpNote, 1)
	report := aggregateStartupNotes(state.attach(func(note mcpNote) { live <- note }))
	state.add(mcpNote{"warn", "MCP: server disconnected later"})

	if len(report.Details) != 1 || report.Details[0].text != "plugins: buffered startup failure" {
		t.Fatalf("buffered report = %#v", report.Details)
	}
	select {
	case note := <-live:
		if note.text != "MCP: server disconnected later" {
			t.Fatalf("live notice = %#v", note)
		}
	default:
		t.Fatal("later extension notice did not reach the attached live surface")
	}
}

func TestStartupBufferOverflowIsDisclosed(t *testing.T) {
	state := &mcpState{}
	for i := 0; i < maxBufferedNotes+7; i++ {
		state.add(mcpNote{"error", fmt.Sprintf("plugins: startup failure %03d", i)})
	}
	retained, dropped := state.attachCounted(nil)
	if len(retained) != maxBufferedNotes || dropped != 7 {
		t.Fatalf("attached notes = %d retained, %d dropped; want %d and 7", len(retained), dropped, maxBufferedNotes)
	}
	report := aggregateStartupNotes(retained, dropped)
	if report.Retained != maxBufferedNotes || report.Dropped != 7 || len(report.Details) != maxBufferedNotes+1 {
		t.Fatalf("overflow report counts = retained %d, dropped %d, details %d",
			report.Retained, report.Dropped, len(report.Details))
	}
	disclosure := report.Details[len(report.Details)-1]
	if disclosure.level != "high" || !strings.Contains(disclosure.text, "7 startup diagnostics were dropped") ||
		!strings.Contains(disclosure.text, "/doctor extensions cannot show their text") {
		t.Fatalf("overflow disclosure = %#v", disclosure)
	}
	joined := strings.Join(report.Summary, "\n")
	assertBoundedStartupSummary(t, report.Summary)
	for _, wanted := range []string{"207 startup notes", "200 retained", "7 dropped", "/doctor extensions"} {
		if !strings.Contains(joined, wanted) {
			t.Errorf("overflow summary omitted %q:\n%s", wanted, joined)
		}
	}
	if strings.Contains(joined, "201 startup notes") {
		t.Fatalf("overflow disclosure was counted as an incoming diagnostic:\n%s", joined)
	}
	if got := report.Groups[1]; got.Total != maxBufferedNotes || got.Unique != maxBufferedNotes {
		t.Fatalf("retained plugin group = %#v, want %d/%d", got, maxBufferedNotes, maxBufferedNotes)
	}
	if details := strings.Join(startupNoteDetailLines(report), "\n"); !strings.Contains(details, "retained sources") {
		t.Fatalf("overflow details did not label source counts as retained:\n%s", details)
	}
	if again, againDropped := state.attachCounted(nil); len(again) != 0 || againDropped != 0 {
		t.Fatalf("overflow disclosure repeated after drain: notes=%#v dropped=%d", again, againDropped)
	}
}

func TestDoctorExtensionsRejectsUnexpectedArguments(t *testing.T) {
	m := testModel(t)
	cmd := cmdDoctor(m, "extensions extra")
	if cmd == nil {
		t.Fatal("invalid /doctor arguments were accepted")
	}
	result := cmd()
	msg, ok := result.(noticeMsg)
	if !ok || msg.level != "error" || !strings.Contains(msg.text, "usage") {
		t.Fatalf("invalid doctor result = %#v", result)
	}
}

func TestStartupDetailWrappingPreservesLongUnbrokenText(t *testing.T) {
	line := "MCP: /" + strings.Repeat("long-path-segment", 20) + " 界"
	wrapped := wrapStartupDetailLine(line, 23)
	if got := strings.Join(wrapped, ""); got != line {
		t.Fatalf("hard wrap changed diagnostic:\n got %q\nwant %q", got, line)
	}
	for _, part := range wrapped {
		if width := lipgloss.Width(part); width > 23 {
			t.Fatalf("wrapped detail width = %d, want <= 23: %q", width, part)
		}
	}
}

func TestStartupHighlightFitsDisplayColumns(t *testing.T) {
	text := strings.Repeat("界", 80)
	got := fitStartupHighlight(text, startupHighlightMaxRunes)
	if width := lipgloss.Width(got); width > startupHighlightMaxRunes {
		t.Fatalf("highlight width = %d, want <= %d", width, startupHighlightMaxRunes)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("bounded highlight did not disclose truncation: %q", got)
	}
}

func TestMandatoryStartupSeverityRemainsVisible(t *testing.T) {
	for _, level := range []string{"fatal", "critical", "high", "required"} {
		note := startupHighlightForDisplay(mcpNote{level: level, text: "component unavailable"})
		if note.level != "error" || !strings.HasPrefix(note.text, level+": ") {
			t.Errorf("%s display = %#v, want explicit error-severity label", level, note)
		}
	}
	if note := startupHighlightForDisplay(mcpNote{level: "warn", text: "required MCP server unavailable"}); note.level != "error" {
		t.Fatalf("textually required warning display = %#v, want error severity", note)
	}
	trailing := mcpNote{level: "warn", text: strings.Repeat("long prefix ", 20) + "required"}
	report := aggregateStartupNotes([]mcpNote{trailing})
	if len(report.Highlights) != 1 {
		t.Fatalf("trailing required note highlights = %#v", report.Highlights)
	}
	display := startupHighlightForDisplay(report.Highlights[0])
	if display.level != "error" || !strings.HasPrefix(display.text, "required: ") {
		t.Fatalf("truncated trailing-required display = %#v", display)
	}
}

func TestStartupDoctorViewFits80x24(t *testing.T) {
	report := aggregateStartupNotes([]mcpNote{{"error", "plugins: " + strings.Repeat("long-path", 40)}})
	view := newStartupNotesView(report).view(80, 24, darkTheme())
	lines := strings.Split(stripANSI(view), "\n")
	if len(lines) != 24 {
		t.Fatalf("80x24 doctor view rendered %d physical lines, want 24:\n%s", len(lines), stripANSI(view))
	}
	for i, line := range lines {
		if width := lipgloss.Width(line); width > 80 {
			t.Fatalf("doctor row %d is %d columns, want <= 80: %q", i, width, line)
		}
	}
}
