package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/switchboard-code/switchboard/internal/scm"
	"github.com/switchboard-code/switchboard/internal/terminaltext"
)

const (
	tuiDiffMaxBytes              = 1 << 20
	tuiDiffInventoryMaxBytes     = 16 << 10
	tuiDiffInventoryMaxPaths     = 64
	tuiDiffInventoryPathMaxBytes = 512
	// Leave room inside the one-MiB render envelope for the truncation note
	// and the bounded inventory. The patch cap remains the only input to Git.
	tuiDiffPatchMaxBytes = tuiDiffMaxBytes - tuiDiffInventoryMaxBytes - 256
)

// diffView is the /diff fullscreen. The diff is highlighted once when it
// loads — §14's "rendered once and cached" — then wrapped once per viewport
// width so scrolling is a slice over the rows the terminal actually shows.
type diffView struct {
	lines  []string
	visual []string
	// visualWidth is the terminal-cell width used to build visual. Diff
	// lines can contain Chroma SGR sequences and wide graphemes, so byte or
	// rune counts are not a usable viewport boundary.
	visualWidth int
	offset      int
	stale       bool
}

type diffLoadedMsg struct {
	sessionID  string
	generation uint64
	lines      []string
	err        error
}

// openDiff diffs the workspace against HEAD. Git calls this a read, but status
// may execute repository filters and hooks, so the standing workspace trust
// authority is required before the first process. This is the harness, not the
// agent, and therefore does not pass through the per-tool permission engine.
func openDiff(workspace string, dark bool, authority scm.ExecutionAuthority) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		repo, err := scm.Discover(ctx, workspace, authority)
		if err != nil {
			if errors.Is(err, scm.ErrExecutionNotTrusted) {
				err = errors.New("/diff can execute repository Git filters and hooks; run /trust grant first")
			}
			return diffLoadedMsg{err: err}
		}
		paths, err := workspaceDiffPaths(workspace, repo.Root)
		if err != nil {
			return diffLoadedMsg{err: err}
		}
		result, err := repo.DiffHEAD(ctx, scm.DiffOptions{
			Paths:    paths,
			MaxBytes: tuiDiffPatchMaxBytes,
		})
		if err != nil {
			return diffLoadedMsg{err: err}
		}
		text := terminaltext.Display(renderSCMDiff(result))
		return diffLoadedMsg{lines: highlightDiff(text, dark)}
	}
}

// workspaceDiffPaths keeps /diff scoped to the directory Switchboard opened,
// even when that directory is nested inside a larger Git worktree. The SCM
// layer forces literal pathspec semantics before this value reaches Git.
func workspaceDiffPaths(workspace, repoRoot string) ([]string, error) {
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolving workspace for diff: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(abs); resolveErr == nil {
		abs = resolved
	}
	rel, err := filepath.Rel(repoRoot, abs)
	if err != nil {
		return nil, fmt.Errorf("resolving workspace within Git worktree: %w", err)
	}
	if rel == "." {
		return nil, nil
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil, fmt.Errorf("%w: workspace %s", scm.ErrOutsideRepo, workspace)
	}
	return []string{filepath.ToSlash(rel)}, nil
}

func renderSCMDiff(result scm.DiffResult) string {
	text := string(result.Text)
	if result.Truncated {
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text += "… diff truncated at 1 MiB; some changes are not shown …\n"
		text += renderDiffOmitted(result.Omitted)
	}
	if strings.TrimSpace(text) != "" {
		return text
	}

	changed := make([]scm.PathState, 0, len(result.Files))
	for _, file := range result.Files {
		if !file.Ignored {
			changed = append(changed, file)
		}
	}
	if len(changed) == 0 {
		return "working tree clean\n"
	}

	var b strings.Builder
	b.WriteString("working tree has changes with no textual patch:\n")
	for _, file := range changed {
		fmt.Fprintf(&b, "  %s  %s\n", diffStateLabel(file), file.Path)
	}
	return b.String()
}

func renderDiffOmitted(files []scm.PathState) string {
	if len(files) == 0 {
		return ""
	}

	// SCM already returns path-sorted status, but sorting a copy here makes
	// this renderer deterministic for every caller and leaves its input alone.
	ordered := append([]scm.PathState(nil), files...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Path == ordered[j].Path {
			return ordered[i].OriginalPath < ordered[j].OriginalPath
		}
		return ordered[i].Path < ordered[j].Path
	})

	header := fmt.Sprintf("files not fully shown (%d):\n", len(ordered))
	var body strings.Builder
	shown := 0
	for shown < len(ordered) && shown < tuiDiffInventoryMaxPaths {
		line := fmt.Sprintf("  %s  %s\n", diffStateLabel(ordered[shown]), diffInventoryPath(ordered[shown]))
		nextShown := shown + 1
		summary := ""
		if nextShown < len(ordered) {
			summary = fmt.Sprintf("  … %d more path(s) not listed; %d total not fully shown …\n", len(ordered)-nextShown, len(ordered))
		}
		if len(header)+body.Len()+len(line)+len(summary) > tuiDiffInventoryMaxBytes {
			break
		}
		body.WriteString(line)
		shown = nextShown
	}

	var b strings.Builder
	b.Grow(len(header) + body.Len() + 80)
	b.WriteString(header)
	b.WriteString(body.String())
	if shown < len(ordered) {
		fmt.Fprintf(&b, "  … %d more path(s) not listed; %d total not fully shown …\n", len(ordered)-shown, len(ordered))
	}
	return b.String()
}

func diffInventoryPath(file scm.PathState) string {
	path := terminaltext.Escape(file.Path)
	if file.OriginalPath != "" && file.OriginalPath != file.Path {
		path = terminaltext.Escape(file.OriginalPath) + " -> " + path
	}
	if len(path) <= tuiDiffInventoryPathMaxBytes {
		return path
	}
	cut := tuiDiffInventoryPathMaxBytes - len("…")
	for cut > 0 && !utf8.RuneStart(path[cut]) {
		cut--
	}
	return path[:cut] + "…"
}

func diffStateLabel(file scm.PathState) string {
	switch {
	case file.Unmerged:
		return "unmerged"
	case file.Untracked:
		return "untracked"
	case file.Staged && file.Unstaged:
		return "staged+unstaged"
	case file.Staged:
		return "staged"
	case file.Unstaged:
		return "unstaged"
	default:
		return "changed"
	}
}

// highlightDiff syntax-highlights a unified diff. A highlighting failure falls
// back to plain lines: the diff matters more than its colors.
func highlightDiff(text string, dark bool) []string {
	plain := strings.Split(strings.TrimRight(text, "\n"), "\n")
	profile := lipgloss.ColorProfile()
	if profile == termenv.Ascii {
		return plain
	}

	lexer := lexers.Get("diff")
	if lexer == nil {
		return plain
	}
	styleName := "github"
	if dark {
		styleName = "github-dark"
	}
	style := styles.Get(styleName)
	formatterName := ""
	switch profile {
	case termenv.ANSI:
		formatterName = "terminal16"
	case termenv.ANSI256:
		formatterName = "terminal256"
	case termenv.TrueColor:
		formatterName = "terminal16m"
	default:
		return plain
	}
	formatter := formatters.Get(formatterName)
	if style == nil || formatter == nil {
		return plain
	}
	it, err := lexer.Tokenise(nil, text)
	if err != nil {
		return plain
	}
	var buf bytes.Buffer
	if err := formatter.Format(&buf, style, it); err != nil {
		return plain
	}
	return strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
}

// key scrolls; it reports true when the view should close. The diff has no
// asynchronous key actions, so its command is always nil.
func (d *diffView) key(msg tea.KeyMsg) (bool, tea.Cmd) {
	if d.stale {
		switch msg.String() {
		case "esc", "q":
			return true, nil
		default:
			return false, noticeCmd("warn", "the workspace changed; close and reopen /diff for the current patch")
		}
	}
	switch msg.String() {
	case "esc", "q":
		return true, nil
	case "up", "k":
		d.scroll(-1)
	case "down", "j":
		d.scroll(1)
	case "pgup", "ctrl+u":
		d.scroll(-20)
	case "pgdown", "ctrl+d":
		d.scroll(20)
	case "g":
		d.offset = 0
	case "G":
		// visual is available after the first paint, which is the normal key
		// path. Keep the logical-line fallback for direct callers before then.
		d.offset = len(d.visual)
		if d.visual == nil {
			d.offset = len(d.lines)
		}
		d.scroll(0)
	}
	return false, nil
}

func (d *diffView) mouse(msg tea.MouseMsg) tea.Cmd {
	if d.stale {
		return nil
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		d.scroll(-3)
	case tea.MouseButtonWheelDown:
		d.scroll(3)
	}
	return nil
}

func (d *diffView) scroll(n int) {
	d.offset += n
	if d.offset < 0 {
		d.offset = 0
	}
}

func (d *diffView) view(width, height int, th *theme) string {
	if width < 1 || height < 1 {
		return ""
	}
	if th == nil {
		th = darkTheme()
	}

	headerText := " git diff HEAD"
	help := "  ↑↓ scroll · pgup/pgdn page · g/G ends · esc close"
	if d.stale {
		headerText += " · STALE"
		help = "  workspace changed · reopen /diff · esc close"
	}
	header := th.bold.Render(headerText) + th.faint.Render(help)
	header = ansi.Truncate(header, width, "")

	footerRows := 0
	if height > 2 {
		footerRows = 1
	}
	bodyHeight := height - 1 - footerRows
	visual := d.visualLines(width)
	if maximum := max(len(visual)-bodyHeight, 0); d.offset > maximum {
		d.offset = maximum
	}
	if d.offset < 0 {
		d.offset = 0
	}

	end := min(d.offset+bodyHeight, len(visual))
	visible := append([]string(nil), visual[d.offset:end]...)
	for len(visible) < bodyHeight {
		visible = append(visible, "")
	}

	rows := append([]string{header}, visible...)
	if footerRows == 1 {
		footer := ""
		if d.stale {
			footer = th.warn.Render(workspaceFit(" stale snapshot: workspace changed after this diff loaded", width))
		} else if len(visual) > bodyHeight && len(visual) > 0 {
			pct := min((d.offset+bodyHeight)*100/len(visual), 100)
			footer = th.faint.Render(workspaceFit(" "+itoa(pct)+"%", width))
		}
		rows = append(rows, footer)
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

// visualLines converts logical diff lines to viewport rows. Hardwrap knows
// about ANSI state and grapheme cell widths; rendering tabs as two printable
// cells avoids delegating their width to terminal tab-stop configuration.
func (d *diffView) visualLines(width int) []string {
	width = max(width, 1)
	if d.visual != nil && d.visualWidth == width {
		return d.visual
	}
	visual := make([]string, 0, len(d.lines))
	for _, line := range d.lines {
		line = strings.ReplaceAll(line, "\t", `\t`)
		wrapped := strings.Split(ansi.Hardwrap(line, width, true), "\n")
		styled := strings.Contains(line, "\x1b[")
		activeStyle := ""
		for _, row := range wrapped {
			rowStyle := activeStyle
			activeStyle = diffSGRState(activeStyle, row)
			// A grapheme can itself be wider than a one-cell viewport. It cannot
			// be represented there, but it must not make the frame wider.
			row = ansi.Truncate(row, width, "")
			if styled {
				// Make every row independently renderable: scrolling can begin or
				// end in the middle of a wrapped Chroma token.
				row = ansi.ResetStyle + rowStyle + row + ansi.ResetStyle
			}
			visual = append(visual, row)
		}
	}
	d.visual = visual
	d.visualWidth = width
	return d.visual
}

// diffSGRState returns the SGR sequences needed to resume the current style
// on a separately rendered continuation row. Chroma emits complete SGR
// sequences and a full reset around each token; handling general terminal
// control state here would be both unnecessary and unsafe.
func diffSGRState(active, text string) string {
	for rest := text; ; {
		start := strings.Index(rest, "\x1b[")
		if start < 0 {
			return active
		}
		rest = rest[start:]
		end := strings.IndexByte(rest, 'm')
		if end < 0 {
			return active
		}
		seq := rest[:end+1]
		params := rest[2:end]
		switch {
		case params == "" || params == "0":
			active = ""
		case strings.HasPrefix(params, "0;") || strings.HasPrefix(params, "0:"):
			// Replaying the whole sequence reproduces reset-then-style.
			active = seq
		default:
			active += seq
		}
		rest = rest[end+1:]
	}
}
