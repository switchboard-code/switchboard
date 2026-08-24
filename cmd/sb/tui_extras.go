package main

// The affordances every neighboring tool has and a user's hands expect:
// /init, /export, /context, the command palette, and the external editor.
// Each is small; their absence is what reads as unfinished.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/fileprivacy"
	"github.com/switchboard-code/switchboard/internal/prefix"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/rootedfs"
	"github.com/switchboard-code/switchboard/internal/session"
)

// initPrompt is /init: a turn like any other, using the loop and the tools it
// already has, because a canned prompt the user can read beats a bespoke
// generator they cannot.
const initPrompt = `Explore this repository and write an AGENTS.md at its root for coding agents working here. Keep it under 100 lines. Cover: what the project is in two sentences; the layout (which directories hold what, only the ones that matter); how to build, test, and lint, with exact commands; conventions an agent must follow that it could not guess from the code; and anything that looks like it would bite a newcomer. If AGENTS.md already exists, read it first and revise rather than replace. Do not pad: a rule that is obvious from reading any file does not need writing down.`

func cmdInit(m *tuiModel, _ string) tea.Cmd {
	return m.enqueue(initPrompt, "")
}

// cmdExport writes the conversation as markdown, through the same
// renderer sb export uses on any recorded session. The session state is
// the source, not the transcript: the transcript is a rendering, the
// session is the record.
func cmdExport(m *tuiModel, args string) tea.Cmd {
	state := m.app.loop.Session.State()
	name := strings.TrimSpace(args)
	if name == "" {
		name = "sb-session-" + state.ID + ".md"
	}
	path := name
	if !filepath.IsAbs(path) {
		path = filepath.Join(m.app.workspace, name)
	}

	// A log that cannot be read as a timeline degrades to the messages
	// alone rather than failing the export.
	timeline, terr := session.ReadTimeline(m.app.loop.Session.Path())
	if terr != nil {
		timeline = nil
	}
	if err := os.WriteFile(path, []byte(exportMarkdown(state, timeline)), 0o644); err != nil {
		return noticeCmd("error", "export failed: "+err.Error())
	}
	return noticeCmd("", "exported to "+path)
}

// setContextWindowCmd records how large a window this target's endpoint
// accepts. The chat-completions format has no field for it and the catalog
// cannot describe a server it has never seen, so for a compatible endpoint the
// number is the user's to state, and stating it is what turns auto-compaction
// back on.
func setContextWindowCmd(m *tuiModel, raw string) tea.Cmd {
	tokens, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || tokens < 0 {
		return noticeCmd("error", "context window must be a count of tokens, for example /context 32768")
	}
	target := m.app.loop.Binding().Target
	key := config.ProviderSurfaceKey(target.Provider, target.Surface)
	if err := m.app.config.SetProviderContextWindowAndSave(key, tokens); err != nil {
		return noticeCmd("error", "saving the context window failed, nothing changed: "+err.Error())
	}
	m.refreshCtxWindow()
	if tokens == 0 {
		return noticeCmd("", "context window for "+key+" is unset again; auto-compaction is off for it")
	}
	return noticeCmd("", fmt.Sprintf("%s accepts %s tokens; auto-compaction fires at %d%% of it",
		key, compact(tokens), compactThreshold(m.app.config)))
}

// cmdContext shows where the window is going. The constraint is invisible
// until it is fatal; a bar makes it something the user can see coming.
func cmdContext(m *tuiModel, args string) tea.Cmd {
	if tokens := strings.TrimSpace(args); tokens != "" {
		return setContextWindowCmd(m, tokens)
	}
	state := m.app.loop.Session.State()
	used := m.callTokens
	window := m.ctxWindow

	var b strings.Builder
	if window > 0 && used > 0 {
		pct := used * 100 / window
		filled := used * 30 / window
		if filled > 30 {
			filled = 30
		}
		fmt.Fprintf(&b, "context  [%s%s] %d%%  %s of %s tokens\n",
			strings.Repeat("█", filled), strings.Repeat("░", 30-filled), pct, compact(used), compact(window))
	} else if window > 0 {
		fmt.Fprintf(&b, "context window %s; usage is measured on the first turn\n", compact(window))
	} else {
		target := m.app.loop.Binding().Target
		b.WriteString("this target does not report a context window, and none is configured\n")
		if m.app.config.CompactAuto {
			// The consequence is the part worth stating. Auto-compaction is
			// gated on the window, so on this target it is off, and the
			// session will run until the server refuses a request.
			b.WriteString("auto-compaction cannot fire without one; /compact summarizes by hand meanwhile\n")
		}
		fmt.Fprintf(&b, "/context <tokens> records what %s/%s accepts, for this and every later session\n",
			target.Provider, target.Surface)
	}

	// The window's composition, in the estimator's own terms: what the next
	// request would send, split by zone. System and tools are the frozen
	// zone a provider cache holds; the conversation is what grows. The
	// split is chars-over-four (the measured floor in docs/estimator.md),
	// while the meter above is what the provider last reported, so the two
	// are stated separately rather than pretending to reconcile.
	sys := prefix.RequestTokens(provider.Request{System: m.app.loop.System})
	tools := prefix.RequestTokens(provider.Request{Tools: m.app.loop.Tools.Definitions()})
	conv := prefix.RequestTokens(provider.Request{Messages: state.Messages})
	if sys+tools+conv > 0 {
		fmt.Fprintf(&b, "the next request, estimated: system %s · tools %s · conversation %s · ~%s total\n",
			compact(sys), compact(tools), compact(conv), compact(sys+tools+conv))
	}
	// The meter's consequence sits beside the meter: a reading at 78% means
	// something different when the tripwire at 85% is in the same glance.
	if m.app.config.CompactAuto {
		fmt.Fprintf(&b, "auto-compact fires at %d%%; /compact preview states the trade, /compact auto off disarms it\n",
			compactThreshold(m.app.config))
	}
	fmt.Fprintf(&b, "messages %d · tool calls %d · session ↓%s ↑%s tokens",
		len(state.Messages), state.Calls, compact(state.Usage.InputTokens), compact(state.Usage.OutputTokens))
	m.addInfo(b.String())
	return nil
}

// openPalette is ctrl+p: every command in one searchable, fuzzy-ranked picker.
// It runs the bare command; one that needs arguments opens its own picker or
// says so.
func (m *tuiModel) openPalette() tea.Cmd {
	var items []pickerItem
	for _, c := range commands() {
		items = append(items, pickerItem{id: c.name, label: "/" + c.name, desc: c.desc})
	}
	for _, c := range m.custom {
		selector := customSelector(m, c)
		items = append(items, pickerItem{
			id: selector, label: "/" + selector, desc: customCommandDescription(c),
		})
	}
	for _, t := range m.app.config.Tiers {
		items = append(items, pickerItem{id: t.ID, label: "/" + t.ID, desc: "switch to " + t.ID, current: t.ID == m.app.tier.ID})
	}
	m.openDialog(&pickerDialog{
		title:  "commands",
		items:  items,
		onPick: func(id string) tea.Cmd { return m.runSlash("/" + id) },
	})
	return nil
}

// --- external editor ---------------------------------------------------------

type editorDoneMsg struct {
	content string
	err     error
}

const (
	editorPromptName     = "prompt.md"
	maxEditorPromptBytes = int64(4 << 20)
)

// editorDraft holds the directory capability open across the external editor.
// Editors commonly save by atomically replacing a file, so the leaf identity
// may change; the private physical directory and the replacement's own
// owner-private regular-file contract may not.
type editorDraft struct {
	dir     string
	root    *os.Root
	dirInfo os.FileInfo
}

func newEditorDraft(content string) (*editorDraft, error) {
	if int64(len(content)) > maxEditorPromptBytes {
		return nil, fmt.Errorf("prompt exceeds the %d-byte external-editor limit", maxEditorPromptBytes)
	}
	dir, err := os.MkdirTemp("", "sb-prompt-*")
	if err != nil {
		return nil, err
	}
	removeDir := true
	defer func() {
		if removeDir {
			_ = os.Remove(dir)
		}
	}()
	if err := fileprivacy.EnsurePrivateDir(dir); err != nil {
		return nil, fmt.Errorf("securing the external-editor directory: %w", err)
	}
	root, err := rootedfs.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	draft := &editorDraft{dir: dir, root: root}
	fail := func(cause error) (*editorDraft, error) {
		_ = root.Close()
		return nil, cause
	}
	draft.dirInfo, err = root.Stat(".")
	if err != nil {
		return fail(err)
	}
	if err := draft.verifyDirectory(); err != nil {
		return fail(err)
	}

	// fileprivacy.Create applies the native owner-only contract atomically.
	// Compare it to the rooted name before writing the user's draft so even a
	// parent-path race cannot redirect those bytes.
	file, err := fileprivacy.Create(filepath.Join(dir, editorPromptName))
	if err != nil {
		return fail(err)
	}
	opened, statErr := file.Stat()
	linked, linkErr := root.Lstat(editorPromptName)
	if statErr != nil || linkErr != nil || !linked.Mode().IsRegular() || !os.SameFile(opened, linked) {
		_ = file.Close()
		_ = root.Remove(editorPromptName)
		return fail(errors.Join(statErr, linkErr, errors.New("external-editor prompt changed while it was created")))
	}
	if _, err := io.WriteString(file, content); err != nil {
		_ = file.Close()
		_ = root.Remove(editorPromptName)
		return fail(err)
	}
	if err := file.Close(); err != nil {
		_ = root.Remove(editorPromptName)
		return fail(err)
	}
	removeDir = false
	return draft, nil
}

func (d *editorDraft) path() string {
	if d == nil {
		return ""
	}
	return filepath.Join(d.dir, editorPromptName)
}

func (d *editorDraft) verifyDirectory() error {
	return d.verifyDirectoryWithHook(nil)
}

func (d *editorDraft) verifyDirectoryWithHook(beforeRetainedOpen func()) error {
	if d == nil || d.root == nil || d.dirInfo == nil {
		return errors.New("external-editor directory capability is unavailable")
	}
	opened, err := d.root.Stat(".")
	if err != nil {
		return err
	}
	linked, linkErr := os.Lstat(d.dir)
	if linkErr != nil || linked.Mode()&os.ModeSymlink != 0 || !linked.IsDir() ||
		!os.SameFile(d.dirInfo, opened) || !os.SameFile(opened, linked) {
		return errors.Join(linkErr, errors.New("external-editor directory changed identity"))
	}
	if beforeRetainedOpen != nil {
		beforeRetainedOpen()
	}
	// Open through the retained root, not the pathname just inspected. A
	// regular-directory-to-FIFO swap in that check/open gap must neither block
	// this read nor redirect the privacy check.
	directory, err := d.root.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	pathOpened, err := directory.Stat()
	if err != nil || !os.SameFile(opened, pathOpened) {
		return errors.Join(err, errors.New("external-editor directory changed while its privacy was checked"))
	}
	private, err := fileprivacy.DirectoryIsOwnerOnly(directory)
	if err != nil {
		return fmt.Errorf("checking external-editor directory privacy: %w", err)
	}
	if !private {
		return errors.New("external-editor directory is no longer owner-private")
	}
	linkedAfter, linkErr := os.Lstat(d.dir)
	if linkErr != nil || linkedAfter.Mode()&os.ModeSymlink != 0 || !linkedAfter.IsDir() ||
		!os.SameFile(opened, linkedAfter) {
		return errors.Join(linkErr, errors.New("external-editor directory changed while its privacy was checked"))
	}
	return nil
}

func (d *editorDraft) read() ([]byte, error) {
	return d.readWithHook(nil)
}

func (d *editorDraft) readWithHook(beforeOpen func()) ([]byte, error) {
	if err := d.verifyDirectory(); err != nil {
		return nil, err
	}
	before, err := d.root.Lstat(editorPromptName)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("external-editor prompt is not a regular file")
	}
	if before.Size() < 0 || before.Size() > maxEditorPromptBytes {
		return nil, fmt.Errorf("external-editor prompt exceeds the %d-byte limit", maxEditorPromptBytes)
	}
	if beforeOpen != nil {
		beforeOpen()
	}
	file, err := openEditorPromptRead(d.root, editorPromptName)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, errors.Join(err, errors.New("external-editor prompt changed identity while it was opened"))
	}
	private, err := fileprivacy.IsOwnerOnly(file)
	if err != nil {
		return nil, fmt.Errorf("checking external-editor prompt privacy: %w", err)
	}
	if !private {
		return nil, errors.New("external-editor prompt is not an owner-private single-link file")
	}
	owned, err := fileprivacy.IsCurrentUserOwner(file)
	if err != nil {
		return nil, fmt.Errorf("checking external-editor prompt owner: %w", err)
	}
	if !owned {
		return nil, errors.New("external-editor prompt is not owned by the current user")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxEditorPromptBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxEditorPromptBytes {
		return nil, fmt.Errorf("external-editor prompt exceeds the %d-byte limit", maxEditorPromptBytes)
	}
	finished, err := file.Stat()
	if err != nil {
		return nil, err
	}
	linked, linkErr := d.root.Lstat(editorPromptName)
	privateAfter, privateErr := fileprivacy.IsOwnerOnly(file)
	ownedAfter, ownerErr := fileprivacy.IsCurrentUserOwner(file)
	if linkErr != nil || privateErr != nil || ownerErr != nil || !privateAfter || !ownedAfter || !linked.Mode().IsRegular() ||
		!os.SameFile(opened, finished) || !os.SameFile(finished, linked) ||
		opened.Size() != finished.Size() || finished.Size() != int64(len(data)) ||
		!opened.ModTime().Equal(finished.ModTime()) || opened.Mode().Perm() != finished.Mode().Perm() {
		return nil, errors.Join(linkErr, privateErr, ownerErr, errors.New("external-editor prompt changed while it was read"))
	}
	if err := d.verifyDirectory(); err != nil {
		return nil, err
	}
	if !utf8.Valid(data) {
		return nil, errors.New("external-editor prompt is not valid UTF-8 text")
	}
	return data, nil
}

func (d *editorDraft) cleanup() error {
	if d == nil || d.root == nil {
		return nil
	}
	// Remove through the retained directory capability. If the pathname was
	// swapped, this still cleans only the original private directory.
	removeErr := d.root.Remove(editorPromptName)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	closeErr := d.root.Close()
	d.root = nil
	var dirErr error
	if linked, err := os.Lstat(d.dir); err == nil && linked.IsDir() && linked.Mode()&os.ModeSymlink == 0 &&
		d.dirInfo != nil && os.SameFile(d.dirInfo, linked) {
		dirErr = os.Remove(d.dir)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		dirErr = err
	}
	return errors.Join(removeErr, closeErr, dirErr)
}

func finishEditorDraft(draft *editorDraft, editorErr error) editorDoneMsg {
	if draft == nil {
		return editorDoneMsg{err: errors.Join(editorErr, errors.New("external-editor draft is unavailable"))}
	}
	if editorErr != nil {
		return editorDoneMsg{err: errors.Join(editorErr, draft.cleanup())}
	}
	data, readErr := draft.read()
	cleanupErr := draft.cleanup()
	if readErr != nil || cleanupErr != nil {
		return editorDoneMsg{err: errors.Join(readErr, cleanupErr)}
	}
	return editorDoneMsg{content: strings.TrimRight(string(data), "\n")}
}

// openEditor is ctrl+g: the prompt in $VISUAL or $EDITOR. Bubble Tea suspends
// the TUI for the child process and resumes when it exits, which is the whole
// trick; everything else is a temp file.
func (m *tuiModel) openEditor() tea.Cmd {
	// ExecProcess suspends Bubble Tea. Letting the model, a probe, or an
	// operation continue while approvals and cancellation are invisible would
	// turn an editor convenience into a control-plane outage.
	if m.busy || m.turnPlanning || m.operationActive || m.race != nil || m.bisect != nil {
		return noticeCmd("warn", "the external editor cannot suspend the TUI while work is running; wait for it to finish or esc to interrupt it")
	}

	parts, err := promptEditorArgv()
	if err != nil {
		return noticeCmd("error", err.Error())
	}

	draft, err := newEditorDraft(m.ta.Value())
	if err != nil {
		return noticeCmd("error", err.Error())
	}

	cmd := sanitizedCommand(parts[0], append(parts[1:], draft.path())...)
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return finishEditorDraft(draft, err)
	})
}

// promptEditorArgv parses the editor as argv, never as shell source. Quoted
// application paths and flags are common in $VISUAL; variable expansion,
// command substitution, and globbing are intentionally absent.
func promptEditorArgv() ([]string, error) {
	editor := strings.TrimSpace(os.Getenv("VISUAL"))
	if editor == "" {
		editor = strings.TrimSpace(os.Getenv("EDITOR"))
	}
	if editor == "" {
		return nil, errors.New("set $EDITOR or $VISUAL to use the external editor")
	}
	parts, err := workspaceSplitArgv(editor)
	if err != nil {
		return nil, fmt.Errorf("editor: %w", err)
	}
	return parts, nil
}

func (m *tuiModel) onEditorDone(msg editorDoneMsg) {
	if msg.err != nil {
		m.addNotice("error", "editor: "+msg.err.Error())
		return
	}
	m.ta.SetValue(msg.content)
	m.ta.CursorEnd()
	m.resetHistoryNavigation()
	m.growInput()
}
