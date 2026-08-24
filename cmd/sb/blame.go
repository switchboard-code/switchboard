package main

// /blame and `sb blame`: which recorded turn wrote each line of a file, on
// which rung and model — and which lines no recorded turn wrote. git blame
// answers "who committed this"; in an agent session the missing half is
// "which model, asked what", and the session logs already hold it: every
// write's bytes and every edit's replacement, beside the usage record that
// names the target and the route record that names the rung. Replay is
// internal/blame's; this file is the two surfaces sharing one body, the
// cost/stats/find pattern. Read-only over the logs, the open one included.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/blame"
	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

const (
	blameMaxRuns      = 12
	maxBlameFileBytes = int64(4 << 20)
)

func readBlameFile(workspace, path string) ([]byte, error) {
	return readWorkspaceFileBounded(workspace, path, maxBlameFileBytes, nil)
}

// blameLines annotates one file from every session the workspace has
// recorded. abs is the file already resolved; shown is how the user named
// it, for the report's own words.
func blameLines(store, delegates *session.Store, workspace, abs, shown string) []string {
	disk, err := readBlameFile(workspace, abs)
	if err != nil {
		return []string{"  cannot read " + shown + ": " + err.Error()}
	}

	byPath, _, err := gatherEdits(workspace, store, delegates)
	if err != nil {
		return []string{"  " + err.Error()}
	}
	edits := byPath[abs]

	if len(edits) == 0 {
		return []string{
			"  no recorded turn has written " + shown,
			"  blame sees what write and edit put in the log; hands and shell commands are outside it",
		}
	}

	ann := blame.Annotate(disk, edits)
	total := len(ann.Lines)
	if total == 0 {
		return []string{"  " + shown + " is empty"}
	}

	counts := make([]int, len(ann.Origins))
	outside := 0
	for _, o := range ann.Lines {
		if o < 0 {
			outside++
		} else {
			counts[o]++
		}
	}
	order := make([]int, len(ann.Origins))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		if counts[order[i]] != counts[order[j]] {
			return counts[order[i]] > counts[order[j]]
		}
		return ann.Origins[order[i]].At.Before(ann.Origins[order[j]].At)
	})

	recorded := total - outside
	lines := []string{fmt.Sprintf("  %d lines: %d from recorded turns, %d outside the record", total, recorded, outside)}

	for rank, origin := range order {
		if rank >= 26 {
			lines = append(lines, fmt.Sprintf("  … and %d more origins with fewer lines", len(order)-rank))
			break
		}
		o := ann.Origins[origin]
		who := provider.DisplayRouteTargetID(provider.RouteTargetID(o.Target))
		if who == "" {
			who = "(target unrecorded)"
		}
		if o.Tier != "" {
			who = o.Tier + " " + who
		}
		turn := fmt.Sprintf("%s#%d", o.SessionID, o.Turn)
		prompt := "  " + recordedTurnPrompt(o.Prompt, o.PromptAuthoredKnown, o.PromptSynthetic, 44)
		word := "lines"
		if counts[origin] == 1 {
			word = "line"
		}
		lines = append(lines,
			fmt.Sprintf("  %c  %d %s  %s  %s%s", 'a'+rank, counts[origin], word, who, turn, prompt),
			"       "+lineRuns(ann.Lines, origin))
	}
	if outside > 0 {
		word := "lines"
		if outside == 1 {
			word = "line"
		}
		lines = append(lines,
			fmt.Sprintf("  ·  %d %s outside the record — typed, shell-made, or before the log", outside, word),
			"       "+lineRuns(ann.Lines, -1))
	}
	if ann.Unplaced > 0 {
		word := "edits"
		if ann.Unplaced == 1 {
			word = "edit"
		}
		lines = append(lines, fmt.Sprintf("  %d recorded %s could not be replayed against what the file became; those lines read as outside the record", ann.Unplaced, word))
	}
	return lines
}

// blameWorkspaceLines is the bare form's receipt: every file the record
// wrote, annotated, and the surviving lines summed by who wrote them —
// beside what each target's calls cost here, kept in the three meterings.
// The juxtaposition is the ladder's yield: whether the rungs that cost
// nothing are writing the lines that last. Lines and money keep separate
// scopes and the closing line says so — money covers all of a target's
// calls, lines only what survives on disk today.
func blameWorkspaceLines(store, delegates *session.Store, cat *catalog.Catalog, workspace string) []string {
	byPath, _, err := gatherEdits(workspace, store, delegates)
	if err != nil {
		return []string{"  " + err.Error()}
	}
	// Money spans the same record the lines do: the workspace's sessions
	// and their subagent errands, whose calls were as real as any. A
	// fork's copied prefix is one spend, counted once, same as its edits.
	var gathered []session.Usage
	for _, s := range []*session.Store{store, delegates} {
		if s == nil {
			continue
		}
		infos, err := s.List(workspace)
		if err != nil {
			continue
		}
		for _, info := range infos {
			usages, err := session.ReadUsages(info.Path)
			if err != nil {
				continue
			}
			gathered = append(gathered, usages...)
		}
	}
	byTarget := map[string][]session.Usage{}
	for _, u := range dedupeCopiedUsages(gathered) {
		byTarget[u.Target] = append(byTarget[u.Target], u)
	}
	if len(byPath) == 0 {
		return []string{
			"  no recorded turn has written anything here yet",
			"  blame sees what write and edit put in the log; hands and shell commands are outside it",
		}
	}

	type row struct {
		lines int
		tiers map[string]bool
	}
	rows := map[string]*row{}
	rowFor := func(target string) *row {
		r, ok := rows[target]
		if !ok {
			r = &row{tiers: map[string]bool{}}
			rows[target] = r
		}
		return r
	}
	outside, files, gone, unreadable := 0, 0, 0, 0
	paths := make([]string, 0, len(byPath))
	for p := range byPath {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, path := range paths {
		edits := byPath[path]
		// A target that wrote here holds a row even when nothing of its
		// survives; "paid, and no line of it lasted" is the receipt's
		// sharpest sentence and must not vanish with the lines.
		for _, e := range edits {
			r := rowFor(e.Target)
			if e.Tier != "" {
				r.tiers[e.Tier] = true
			}
		}
		disk, err := readBlameFile(workspace, path)
		if err != nil {
			if os.IsNotExist(err) {
				gone++
			} else {
				unreadable++
			}
			continue
		}
		files++
		ann := blame.Annotate(disk, edits)
		for _, o := range ann.Lines {
			if o < 0 {
				outside++
				continue
			}
			rowFor(ann.Origins[o].Target).lines++
		}
	}

	targets := make([]string, 0, len(rows))
	for target := range rows {
		targets = append(targets, target)
	}
	sort.Slice(targets, func(i, j int) bool {
		if rows[targets[i]].lines != rows[targets[j]].lines {
			return rows[targets[i]].lines > rows[targets[j]].lines
		}
		return targets[i] < targets[j]
	})

	word := "files"
	if files == 1 {
		word = "file"
	}
	lines := []string{fmt.Sprintf("  surviving lines across the %d %s the record touched", files, word)}
	nameWidth := 0
	names := map[string]string{}
	for _, target := range targets {
		name := provider.DisplayRouteTargetID(provider.RouteTargetID(target))
		if name == "" {
			name = "(target unrecorded)"
		}
		if tier := soleTier(rows[target].tiers); tier != "" {
			name = tier + " " + name
		}
		names[target] = name
		if len(name) > nameWidth {
			nameWidth = len(name)
		}
	}
	for _, target := range targets {
		lines = append(lines, fmt.Sprintf("  %-*s  %5d %s   %s",
			nameWidth, names[target], rows[target].lines, lineWord(rows[target].lines), targetPay(cat, target, byTarget[target])))
	}
	if outside > 0 {
		lines = append(lines, fmt.Sprintf("  %-*s  %5d %s   typed, shell-made, or before the log",
			nameWidth, "outside the record", outside, lineWord(outside)))
	}
	if gone > 0 {
		word := "files"
		if gone == 1 {
			word = "file"
		}
		lines = append(lines, fmt.Sprintf("  %d %s the record wrote are gone; whatever they held is nobody's now", gone, word))
	}
	if unreadable > 0 {
		lines = append(lines, fmt.Sprintf("  %d files could not be read and are not counted", unreadable))
	}
	return append(lines,
		"  lines are what survives on disk today; money is what the target's calls cost, surviving or not",
		"  subagent errands count on both sides — their writes were as real as their calls")
}

// blameLineLines is the drill-in: one line's whole story. The map form
// says who; this says who, asked what, beside what else that turn did and
// how the turn signed off — the answer to "why is this line here" that a
// transcript search cannot give, because the line number is not in the
// transcript.
func blameLineLines(store, delegates *session.Store, workspace, abs, shown string, line int) []string {
	disk, err := readBlameFile(workspace, abs)
	if err != nil {
		return []string{"  cannot read " + shown + ": " + err.Error()}
	}

	byPath, logByID, err := gatherEdits(workspace, store, delegates)
	if err != nil {
		return []string{"  " + err.Error()}
	}
	edits := byPath[abs]

	ann := blame.Annotate(disk, edits)
	if line < 1 || line > len(ann.Lines) {
		return []string{fmt.Sprintf("  %s has %d lines; there is no line %d", shown, len(ann.Lines), line)}
	}
	origin := ann.Lines[line-1]
	if origin < 0 {
		return []string{
			fmt.Sprintf("  line %d is outside the record — typed, shell-made, or before the log", line),
			"  blame sees what write and edit put in the log; no recorded turn wrote this line",
		}
	}

	o := ann.Origins[origin]
	who := provider.DisplayRouteTargetID(provider.RouteTargetID(o.Target))
	if o.Tier != "" {
		who = o.Tier + " " + who
	}
	lines := []string{fmt.Sprintf("  written by %s in %s#%d", who, o.SessionID, o.Turn)}
	lines = append(lines, "  asked: "+recordedTurnPrompt(o.Prompt, o.PromptAuthoredKnown, o.PromptSynthetic, 70))

	ref, ok := logByID[o.SessionID]
	if !ok {
		return append(lines, "  that session's log is no longer in the store; the story ends here")
	}
	if others := turnTouched(ref.path, o.Turn, abs); len(others) > 0 {
		lines = append(lines, "  the turn also touched: "+strings.Join(others, ", "))
	}
	if closing := turnClosing(ref.path, o.Turn); closing != "" {
		lines = append(lines, fmt.Sprintf("  the turn signed off: %q", redactCredentialTextBeforeTruncate(closing, 90)))
	}
	if ref.errand {
		// An errand's log is real and auditable, but /resume deliberately
		// never offers a context that was never the user's.
		return append(lines, "  a subagent errand wrote this; its log is on record, though not a session /resume offers")
	}
	return append(lines, fmt.Sprintf("  /resume %s reopens that session", o.SessionID))
}

// turnTouched names the other files a turn's calls wrote, as the calls
// named them, the queried file left out.
func turnTouched(logPath string, turn int, abs string) []string {
	edits, err := session.ReadFileEdits(logPath)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range edits {
		if e.Turn != turn || resolveEditPath(e) == abs || seen[e.Path] {
			continue
		}
		seen[e.Path] = true
		out = append(out, e.Path)
	}
	sort.Strings(out)
	if len(out) > 6 {
		out = append(out[:6], fmt.Sprintf("and %d more", len(out)-6))
	}
	return out
}

// turnClosing is the turn's last assistant words — how the work was
// explained when it was done, which is usually the why the line's reader
// is after.
func turnClosing(logPath string, turn int) string {
	timeline, err := session.ReadTimeline(logPath)
	if err != nil {
		return ""
	}
	current, closing := 0, ""
	for _, item := range timeline {
		if item.Message == nil {
			continue
		}
		if session.OpensTurn(*item.Message) {
			current++
			if current > turn {
				break
			}
			continue
		}
		if current == turn && item.Message.Role == provider.RoleAssistant {
			if text := strings.TrimSpace(item.Message.Text()); text != "" {
				closing = text
			}
		}
	}
	return closing
}

// lineWord keeps a one-line row from saying "1 lines"; the trailing space
// on the singular keeps the money column aligned.
func lineWord(n int) string {
	if n == 1 {
		return "line "
	}
	return "lines"
}

// soleTier names the one rung a target was routed on when the record
// agrees with itself; a target seen from several rungs, or from none the
// route records vouch for, shows bare.
func soleTier(tiers map[string]bool) string {
	if len(tiers) != 1 {
		return ""
	}
	for tier := range tiers {
		return tier
	}
	return ""
}

// targetPay is one target's money word, in the metering the catalog
// records for it — never collapsed into "free", the §4 rule this whole
// surface exists beside.
func targetPay(cat *catalog.Catalog, targetStr string, usages []session.Usage) string {
	target, err := parseRecordedTarget(targetStr)
	if err != nil {
		return "no price on record"
	}
	info, _, ok := cat.Lookup(target)
	switch {
	case !ok:
		return "no price on record"
	case info.Metering == catalog.Local:
		return "runs locally — nothing to bill"
	case info.Metering == catalog.Plan:
		return "bills a plan, not dollars"
	case info.Free():
		return "no price on record"
	}
	var dollars catalog.Money
	for _, u := range usages {
		dollars = addMoney(dollars, catalog.Money(u.CostMicroUSD))
	}
	if dollars == 0 {
		return "bills dollars, but no cost was recorded"
	}
	return fmt.Sprintf("%s as routed", dollars)
}

// delegateStore opens the store delegate errands record into: real logs,
// kept out of the primary store so /resume never offers a context that
// was never the user's. This helper is the one place the path is named,
// so the tool assembly and the readers cannot drift apart.
func delegateStore() (*session.Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return session.NewStore(filepath.Join(home, ".switchboard", "delegates"))
}

// resolveEditPath maps a recorded call's path to the file it named:
// workspace-relative paths resolve against the workspace the log's own
// header recorded, not against whoever is asking today.
func resolveEditPath(e session.FileEdit) string {
	if filepath.IsAbs(e.Path) {
		return filepath.Clean(e.Path)
	}
	return filepath.Join(e.Workspace, e.Path)
}

// logRef is where an origin's log lives and what kind of session it was:
// an errand's log is auditable but deliberately not resumable, and the
// drill-in must not offer what /resume would refuse.
type logRef struct {
	path   string
	errand bool
}

// gatherEdits reads every log the workspace recorded once — the primary
// store and the delegate errands' store, whose subagents write with the
// same tools into the same tree — returning mutations keyed by resolved
// path and oldest-first, plus each session's log for the surfaces that go
// back for the turn's story. A fork copies its source's records byte for
// byte — /races dedupes its verdicts for the same reason — so a call
// already seen under its id and timestamp is a copy, not a second
// mutation, and replaying it would raise a false drift alarm. Logs are
// read in id order, which is creation order, so the copy that survives is
// the source's.
func gatherEdits(workspace string, primary, delegates *session.Store) (map[string][]session.FileEdit, map[string]logRef, error) {
	type sourced struct {
		session.Info
		errand bool
	}
	infos, err := primary.List(workspace)
	if err != nil {
		return nil, nil, err
	}
	all := make([]sourced, 0, len(infos))
	for _, info := range infos {
		all = append(all, sourced{Info: info})
	}
	if delegates != nil {
		// An unreadable errand store narrows the record rather than
		// failing the question; what it held reads as outside it.
		if errands, err := delegates.List(workspace); err == nil {
			for _, info := range errands {
				all = append(all, sourced{Info: info, errand: true})
			}
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })

	byPath := map[string][]session.FileEdit{}
	logByID := map[string]logRef{}
	seen := map[string]bool{}
	for _, info := range all {
		logByID[info.ID] = logRef{path: info.Path, errand: info.errand}
		edits, err := session.ReadFileEdits(info.Path)
		if err != nil {
			continue
		}
		for _, e := range edits {
			key := e.CallID + "@" + strconv.FormatInt(e.At.UnixNano(), 10)
			if e.CallID != "" && seen[key] {
				continue
			}
			seen[key] = true
			abs := resolveEditPath(e)
			byPath[abs] = append(byPath[abs], e)
		}
	}
	for _, edits := range byPath {
		sort.SliceStable(edits, func(i, j int) bool { return edits[i].At.Before(edits[j].At) })
	}
	return byPath, logByID, nil
}

// lineRuns renders which 1-based lines carry an origin, as compact runs:
// "12-48, 60". A heavily interleaved file is capped rather than scrolled.
func lineRuns(lines []int, origin int) string {
	var runs []string
	start := -1
	flush := func(end int) {
		if start < 0 {
			return
		}
		if start == end {
			runs = append(runs, fmt.Sprintf("%d", start))
		} else {
			runs = append(runs, fmt.Sprintf("%d-%d", start, end))
		}
		start = -1
	}
	for i, o := range lines {
		if o == origin {
			if start < 0 {
				start = i + 1
			}
			continue
		}
		flush(i)
	}
	flush(len(lines))
	if len(runs) > blameMaxRuns {
		return strings.Join(runs[:blameMaxRuns], ", ") + fmt.Sprintf(" … and %d more runs", len(runs)-blameMaxRuns)
	}
	return strings.Join(runs, ", ")
}

// parseLineRef reads a trailing :N off a path. The caller checks the
// literal path first, so a file whose name really holds a colon is never
// misread as a line number — the filesystem is the tiebreak.
func parseLineRef(arg string) (string, int, bool) {
	i := strings.LastIndex(arg, ":")
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(arg[i+1:])
	if err != nil || n < 1 {
		return "", 0, false
	}
	return arg[:i], n, true
}

func cmdBlame(m *tuiModel, args string) tea.Cmd {
	// A missing errand store narrows the record rather than failing the
	// question; the gatherer treats nil the same way.
	delegates, _ := delegateStore()
	path := strings.TrimSpace(args)
	if path == "" {
		m.addInfo("who wrote this workspace\n" +
			strings.Join(blameWorkspaceLines(m.app.store, delegates, m.app.catalog, m.app.workspace), "\n"))
		return nil
	}
	resolve := func(p string) string {
		if filepath.IsAbs(p) {
			return filepath.Clean(p)
		}
		return filepath.Join(m.app.workspace, p)
	}
	if file, line, ok := parseLineRef(path); ok {
		if _, err := os.Stat(resolve(path)); err != nil {
			m.addInfo(fmt.Sprintf("why line %d of %s\n", line, file) +
				strings.Join(blameLineLines(m.app.store, delegates, m.app.workspace, resolve(file), file, line), "\n"))
			return nil
		}
	}
	m.addInfo(fmt.Sprintf("who wrote %s\n", path) +
		strings.Join(blameLines(m.app.store, delegates, m.app.workspace, resolve(path), path), "\n"))
	return nil
}

func runBlameCLI(w io.Writer, store *session.Store, cat *catalog.Catalog, workspace, path string) error {
	delegates, _ := delegateStore()
	if path == "" {
		fmt.Fprintln(w, "who wrote this workspace")
		for _, line := range blameWorkspaceLines(store, delegates, cat, workspace) {
			fmt.Fprintln(w, cliText(strings.TrimRight(line, " ")))
		}
		return nil
	}
	if file, line, ok := parseLineRef(path); ok {
		if _, statErr := os.Stat(path); statErr != nil {
			abs, err := filepath.Abs(file)
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "why line %d of %s\n", line, cliText(file))
			for _, out := range blameLineLines(store, delegates, workspace, filepath.Clean(abs), file, line) {
				fmt.Fprintln(w, cliText(strings.TrimRight(out, " ")))
			}
			return nil
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "who wrote %s\n", cliText(path))
	for _, line := range blameLines(store, delegates, workspace, filepath.Clean(abs), path) {
		fmt.Fprintln(w, cliText(strings.TrimRight(line, " ")))
	}
	return nil
}
