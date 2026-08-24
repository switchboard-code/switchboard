package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/switchboard-code/switchboard/internal/checkpoint"
)

func TestReviewCommandAppearsOnceInRegistryHelpAndPalette(t *testing.T) {
	count := 0
	for _, command := range commands() {
		if command.name != "review" {
			continue
		}
		count++
		if command.usage != "[turn]" || command.desc != "review one turn's recorded mutations" || command.busySafe || fmt.Sprintf("%p", command.run) != fmt.Sprintf("%p", cmdReview) {
			t.Fatalf("review command=%+v", command)
		}
	}
	if count != 1 {
		t.Fatalf("review registry count=%d, want 1", count)
	}

	m := testModel(t)
	cmdHelp(m, "")
	help := m.tr.last().text
	if got := strings.Count(help, "/review [turn]"); got != 1 {
		t.Fatalf("review help count=%d, want 1:\n%s", got, help)
	}

	if cmd := m.openPalette(); cmd != nil {
		t.Fatal("opening the command palette returned asynchronous work")
	}
	palette, ok := m.dlg.(*pickerDialog)
	if !ok {
		t.Fatalf("palette=%T, want *pickerDialog", m.dlg)
	}
	paletteCount := 0
	for _, item := range palette.items {
		if item.id == "review" && item.label == "/review" {
			paletteCount++
		}
	}
	if paletteCount != 1 {
		t.Fatalf("review palette count=%d, want 1", paletteCount)
	}
}

func TestCmdReviewLoadsCurrentAndExplicitTurnAsynchronously(t *testing.T) {
	dir := t.TempDir()
	recorder := checkpoint.NewRecorder()
	recorder.Begin("first mutation")
	commitReviewMutation(t, recorder, filepath.Join(dir, "first.txt"),
		checkpoint.FileState{}, checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("first\n")})
	recorder.Begin("second mutation")
	commitReviewMutation(t, recorder, filepath.Join(dir, "second.txt"),
		checkpoint.FileState{}, checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("second\n")})

	m := testModel(t)
	m.app.workspace = dir
	m.app.undo = recorder

	currentCmd := cmdReview(m, "")
	if currentCmd == nil || m.full != nil {
		t.Fatal("current review did not remain asynchronous")
	}
	current, ok := currentCmd().(turnReviewLoadedMsg)
	if !ok || current.err != nil {
		t.Fatalf("current result=%#v", currentCmd())
	}
	currentText := stripANSI(strings.Join(current.lines, "\n"))
	if current.index != 2 || current.label != "second mutation" || !strings.Contains(currentText, "second.txt") || strings.Contains(currentText, "first.txt") {
		t.Fatalf("current review=%#v\n%s", current, currentText)
	}
	if _, cmd := m.Update(current); cmd != nil {
		t.Fatalf("loaded current review returned command %v", cmd)
	}
	view, ok := m.full.(*turnReviewView)
	if !ok || view.index != 2 || view.label != "second mutation" {
		t.Fatalf("fullscreen=%T %#v", m.full, m.full)
	}
	m.full = nil

	explicit, ok := cmdReview(m, "1")().(turnReviewLoadedMsg)
	if !ok || explicit.err != nil {
		t.Fatalf("explicit result=%#v", explicit)
	}
	explicitText := stripANSI(strings.Join(explicit.lines, "\n"))
	if explicit.index != 1 || explicit.label != "first mutation" || !strings.Contains(explicitText, "first.txt") || strings.Contains(explicitText, "second.txt") {
		t.Fatalf("explicit review=%#v\n%s", explicit, explicitText)
	}
}

func TestCmdReviewRejectsInvalidGrammarAndUnavailableTurns(t *testing.T) {
	for _, args := range []string{"0", "-1", "+1", "one", "1 2", "1\t2"} {
		t.Run(args, func(t *testing.T) {
			result := cmdReview(testModel(t), args)()
			msg, ok := result.(noticeMsg)
			if !ok || msg.level != "error" || msg.text != "usage: /review [turn]" {
				t.Fatalf("invalid %q result=%#v", args, result)
			}
		})
	}

	dir := t.TempDir()
	m := testModel(t)
	m.app.workspace = dir
	m.app.undo = nil
	if msg := cmdReview(m, "")().(turnReviewLoadedMsg); msg.err == nil || !strings.Contains(msg.err.Error(), "no checkpoint recorder") {
		t.Fatalf("nil recorder result=%#v", msg)
	}

	m.app.undo = checkpoint.NewRecorder()
	if msg := cmdReview(m, "")().(turnReviewLoadedMsg); msg.err == nil || !strings.Contains(msg.err.Error(), "current turn has no recorded") {
		t.Fatalf("empty recorder result=%#v", msg)
	}
	m.app.undo.Begin("no-op current")
	if msg := cmdReview(m, "")().(turnReviewLoadedMsg); msg.err == nil || msg.err.Error() != "current turn has no recorded write/edit mutations" {
		t.Fatalf("no-op current result=%#v", msg)
	}

	path := filepath.Join(dir, "file.txt")
	commitReviewMutation(t, m.app.undo, path,
		checkpoint.FileState{}, checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("new\n")})
	if msg := cmdReview(m, "2")().(turnReviewLoadedMsg); msg.err == nil || !strings.Contains(msg.err.Error(), "out of range") {
		t.Fatalf("out-of-range result=%#v", msg)
	}

	m.full = &turnReviewView{index: 99}
	if _, cmd := m.Update(turnReviewLoadedMsg{err: errors.New("fixture")}); cmd != nil {
		t.Fatalf("error update returned command %v", cmd)
	}
	if got := m.tr.last(); got == nil || got.level != "error" || got.text != "review failed: fixture" {
		t.Fatalf("review failure notice=%#v", got)
	}
	if _, ok := m.full.(*turnReviewView); !ok {
		t.Fatal("failed asynchronous load disturbed the existing fullscreen")
	}
}

func TestCmdReviewIsNotBusySafe(t *testing.T) {
	m := testModel(t)
	m.busy = true
	result := m.runSlash("/review")()
	msg, ok := result.(noticeMsg)
	if !ok || msg.level != "warn" || !strings.Contains(msg.text, "turn is running") {
		t.Fatalf("busy /review result=%#v", result)
	}
}

func TestReviewDropsAnOlderAsynchronousResult(t *testing.T) {
	dir := t.TempDir()
	recorder := checkpoint.NewRecorder()
	recorder.Begin("mutation")
	commitReviewMutation(t, recorder, filepath.Join(dir, "file.txt"),
		checkpoint.FileState{}, checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("new\n")})
	m := testModel(t)
	m.app.workspace = dir
	m.app.undo = recorder

	older := cmdReview(m, "")().(turnReviewLoadedMsg)
	newer := cmdReview(m, "")().(turnReviewLoadedMsg)
	if older.generation == newer.generation || newer.generation != m.workspaceGeneration {
		t.Fatalf("review generations older=%d newer=%d current=%d", older.generation, newer.generation, m.workspaceGeneration)
	}
	if _, cmd := m.Update(older); cmd != nil || m.full != nil {
		t.Fatalf("older result=(cmd %v, full %T), want dropped", cmd, m.full)
	}
	if _, cmd := m.Update(newer); cmd != nil {
		t.Fatalf("newer result returned command %v", cmd)
	}
	if _, ok := m.full.(*turnReviewView); !ok {
		t.Fatalf("newer result opened %T, want *turnReviewView", m.full)
	}
}

func TestReviewAndDiffFullscreenResultsFollowInvocationOrder(t *testing.T) {
	root := initTUIDiffRepo(t)
	writeTUIDiffFile(t, root, "file.txt", []byte("base\n"))
	commitTUIDiffFiles(t, root)
	recorder := checkpoint.NewRecorder()
	recorder.Begin("mutation")
	commitReviewMutation(t, recorder, filepath.Join(root, "file.txt"),
		checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("base\n")},
		checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("new\n")})

	newModel := func(t *testing.T) *tuiModel {
		m := testModel(t)
		m.app.workspace = root
		m.app.trust = grantTUIDiffTrust(t, root)
		m.app.undo = recorder
		return m
	}

	t.Run("later diff wins", func(t *testing.T) {
		m := newModel(t)
		review := cmdReview(m, "")().(turnReviewLoadedMsg)
		diff := cmdDiff(m, "")().(diffLoadedMsg)
		m.Update(diff)
		m.Update(review)
		if _, ok := m.full.(*diffView); !ok {
			t.Fatalf("fullscreen=%T, want later *diffView", m.full)
		}
	})

	t.Run("later review wins", func(t *testing.T) {
		m := newModel(t)
		diff := cmdDiff(m, "")().(diffLoadedMsg)
		review := cmdReview(m, "")().(turnReviewLoadedMsg)
		m.Update(review)
		m.Update(diff)
		if _, ok := m.full.(*turnReviewView); !ok {
			t.Fatalf("fullscreen=%T, want later *turnReviewView", m.full)
		}
	})
}

func TestReviewResultCannotCrossTurnUndoOrNewerClosedSurface(t *testing.T) {
	newFixture := func(t *testing.T) (*tuiModel, *checkpoint.Recorder, string) {
		dir := t.TempDir()
		path := filepath.Join(dir, "file.txt")
		recorder := checkpoint.NewRecorder()
		recorder.Begin("mutation")
		commitReviewMutation(t, recorder, path, checkpoint.FileState{},
			checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("new\n")})
		m := testModel(t)
		m.app.workspace = dir
		m.app.undo = recorder
		return m, recorder, path
	}

	t.Run("new turn starts before load", func(t *testing.T) {
		m, recorder, _ := newFixture(t)
		load := cmdReview(m, "")
		m.turnGeneration++
		m.busy = true
		recorder.Begin("new prompt")
		msg := load().(turnReviewLoadedMsg)
		m.Update(msg)
		if m.full != nil || m.tr.last() != nil {
			t.Fatalf("cross-turn result surfaced: full=%T notice=%#v", m.full, m.tr.last())
		}
	})

	t.Run("turn starts after load before delivery", func(t *testing.T) {
		m, _, _ := newFixture(t)
		msg := cmdReview(m, "")().(turnReviewLoadedMsg)
		m.turnGeneration++
		m.busy = true
		m.Update(msg)
		if m.full != nil {
			t.Fatalf("busy model opened %T", m.full)
		}
	})

	t.Run("undo consumes cursor before delivery", func(t *testing.T) {
		m, recorder, path := newFixture(t)
		msg := cmdReview(m, "")().(turnReviewLoadedMsg)
		if _, _, err := recorder.UndoFile(path); err != nil {
			t.Fatal(err)
		}
		m.Update(msg)
		if m.full != nil {
			t.Fatalf("post-undo result opened %T", m.full)
		}
	})

	t.Run("newer surface opened and closed", func(t *testing.T) {
		m, _, _ := newFixture(t)
		msg := cmdReview(m, "")().(turnReviewLoadedMsg)
		m.full = newStartupNotesView(startupNoteReport{})
		m.closeFullscreen()
		m.Update(msg)
		if m.full != nil {
			t.Fatalf("closed newer surface resurrected %T", m.full)
		}
	})
}

func TestReviewStaleSurfaceDisclosesNoExternalBytesAndMutatesNothing(t *testing.T) {
	root := initTUIDiffRepo(t)
	path := filepath.Join(root, "tracked.txt")
	writeTUIDiffFile(t, root, "tracked.txt", []byte("base\n"))
	commitTUIDiffFiles(t, root)

	recorder := checkpoint.NewRecorder()
	recorder.Begin("agent edit")
	commitReviewMutation(t, recorder, path,
		checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("base\n")},
		checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("agent\n")})
	const external = "external private replacement\n"
	if err := os.WriteFile(path, []byte(external), 0o644); err != nil {
		t.Fatal(err)
	}

	beforeSnapshots := turnReviewSnapshotSignature(recorder.Snapshots())
	beforeIndex := tuiDiffIndexChecksum(t, root)
	beforeStatus := runTUIDiffGit(t, root, "status", "--porcelain=v2", "-z")
	beforeInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	m := testModel(t)
	m.app.workspace = root
	m.app.undo = recorder
	msg, ok := cmdReview(m, "")().(turnReviewLoadedMsg)
	if !ok || msg.err != nil {
		t.Fatalf("stale review result=%#v", msg)
	}
	loaded := stripANSI(strings.Join(msg.lines, "\n"))
	if !strings.Contains(loaded, checkpoint.ErrStale.Error()) || strings.Contains(loaded, strings.TrimSpace(external)) {
		t.Fatalf("stale review hid refusal or exposed external bytes:\n%s", loaded)
	}
	if _, cmd := m.Update(msg); cmd != nil {
		t.Fatalf("stale review update returned command %v", cmd)
	}
	visual := stripANSI(m.full.view(80, 24, m.th))
	if !strings.Contains(visual, checkpoint.ErrStale.Error()) || strings.Contains(visual, strings.TrimSpace(external)) {
		t.Fatalf("stale fullscreen hid refusal or exposed external bytes:\n%s", visual)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != external || afterInfo.Mode() != beforeInfo.Mode() {
		t.Fatalf("review changed file: content=%q mode=%v, want %q mode=%v", after, afterInfo.Mode(), external, beforeInfo.Mode())
	}
	if got := turnReviewSnapshotSignature(recorder.Snapshots()); got != beforeSnapshots {
		t.Fatalf("review mutated checkpoint snapshots:\n before %s\n after  %s", beforeSnapshots, got)
	}
	if got := tuiDiffIndexChecksum(t, root); got != beforeIndex {
		t.Fatalf("review changed Git index: got %x, want %x", got, beforeIndex)
	}
	if got := runTUIDiffGit(t, root, "status", "--porcelain=v2", "-z"); got != beforeStatus {
		t.Fatalf("review changed worktree status:\n before %q\n after  %q", beforeStatus, got)
	}
}

func TestTurnReviewViewIsBoundedScrollableAndReadOnly(t *testing.T) {
	stale := "refused: " + strings.Repeat("stale-path-detail-", 20)
	lines := []string{stale}
	for i := 0; i < 60; i++ {
		lines = append(lines, fmt.Sprintf("line %02d", i))
	}
	v := &turnReviewView{index: 7, label: "fixture", lines: lines}
	var _ fullscreen = v

	view := v.view(80, 24, darkTheme())
	plain := stripANSI(view)
	if !strings.Contains(plain, "turn 7 agent mutations · read-only") || !strings.Contains(plain, "/diff is repository vs HEAD") {
		t.Fatalf("review chrome missing:\n%s", plain)
	}
	rows := strings.Split(view, "\n")
	if len(rows) != 24 {
		t.Fatalf("fullscreen height=%d, want 24:\n%s", len(rows), plain)
	}
	for i, row := range rows {
		if width := lipgloss.Width(row); width > 80 {
			t.Fatalf("row %d width=%d, want <=80: %q", i, width, stripANSI(row))
		}
	}
	joinedVisual := stripANSI(strings.Join(v.visualLines(80), ""))
	if !strings.Contains(joinedVisual, stale) {
		t.Fatalf("hard wrapping omitted stale detail:\n%s", joinedVisual)
	}

	if close, cmd := v.key(tea.KeyMsg{Type: tea.KeyPgDown}); close || cmd != nil || v.offset != 20 {
		t.Fatalf("page down=(close %v, cmd %v, offset %d)", close, cmd, v.offset)
	}
	if cmd := v.mouse(tea.MouseMsg{Button: tea.MouseButtonWheelDown}); cmd != nil || v.offset != 23 {
		t.Fatalf("wheel down=(cmd %v, offset %d)", cmd, v.offset)
	}
	v.key(runeKey('G'))
	bottom := stripANSI(v.view(80, 24, darkTheme()))
	if !strings.Contains(bottom, "line 59") || !strings.Contains(bottom, "100%") {
		t.Fatalf("bottom did not expose final review line:\n%s", bottom)
	}
	v.key(runeKey('g'))
	if v.offset != 0 {
		t.Fatalf("g offset=%d, want 0", v.offset)
	}
	for _, key := range []tea.KeyMsg{{Type: tea.KeyEsc}, runeKey('q')} {
		if close, cmd := v.key(key); !close || cmd != nil {
			t.Fatalf("close key %q=(%v,%v)", key.String(), close, cmd)
		}
	}

	m := testModel(t)
	m.full = v
	if _, cmd := m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown}); cmd != nil {
		t.Fatalf("model wheel returned command %v", cmd)
	}
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc}); cmd != nil || m.full != nil {
		t.Fatalf("model close=(cmd %v, full %T)", cmd, m.full)
	}
}

func TestTurnReviewViewEscapesTabHeavyLinesWithin80Columns(t *testing.T) {
	v := &turnReviewView{index: 3, lines: []string{strings.Repeat("\tX", 80)}}
	visual := v.visualLines(80)
	for i, line := range visual {
		if strings.ContainsRune(line, '\t') {
			t.Fatalf("visual line %d retained tab: %q", i, line)
		}
		if width := lipgloss.Width(line); width > 80 {
			t.Fatalf("visual line %d width=%d, want <=80: %q", i, width, stripANSI(line))
		}
	}
	view := v.view(80, 24, darkTheme())
	rows := strings.Split(view, "\n")
	if len(rows) != 24 {
		t.Fatalf("tab-heavy fullscreen height=%d, want 24", len(rows))
	}
	for i, row := range rows {
		if strings.ContainsRune(row, '\t') || lipgloss.Width(row) > 80 {
			t.Fatalf("tab-heavy row %d escaped bounds: %q", i, stripANSI(row))
		}
	}
}

func TestTurnReviewRowsResumeStylesAndBoundWideGraphemes(t *testing.T) {
	v := &turnReviewView{lines: []string{"\x1b[31mabcdef\x1b[0m"}}
	rows := v.visualLines(3)
	if len(rows) != 2 {
		t.Fatalf("styled review rows = %q, want two", rows)
	}
	for i, row := range rows {
		if !strings.HasSuffix(row, ansi.ResetStyle) {
			t.Errorf("row %d does not close its style: %q", i, row)
		}
	}
	if !strings.Contains(rows[1], "\x1b[31m") {
		t.Fatalf("continuation row lost active style: %q", rows[1])
	}

	v = &turnReviewView{lines: []string{"界"}}
	for _, row := range v.visualLines(1) {
		if width := ansi.StringWidth(row); width > 1 {
			t.Fatalf("irreducible grapheme occupies %d cells at width one: %q", width, row)
		}
	}
}

func turnReviewSnapshotSignature(snapshots []checkpoint.TurnSnapshot) string {
	var out strings.Builder
	for index, snapshot := range snapshots {
		fmt.Fprintf(&out, "%d:%q:%t:%t|", index, snapshot.Label, snapshot.Open, snapshot.Partial)
		for _, file := range snapshot.Files {
			fmt.Fprintf(&out, "%q:%t:%o:%x:%t:%o:%x|", file.Path,
				file.Before.Existed, restorableTestMode(file.Before.Mode), file.Before.Content,
				file.After.Existed, restorableTestMode(file.After.Mode), file.After.Digest)
		}
		for _, path := range snapshot.Skipped {
			fmt.Fprintf(&out, "skip:%q|", path)
		}
	}
	return out.String()
}

func restorableTestMode(mode fs.FileMode) fs.FileMode {
	return mode & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
}
