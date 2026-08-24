package main

// Custom commands: a markdown file per command, the format the neighboring
// tools converged on, so what a user already wrote for one of them ports
// here by copying a file. .switchboard/commands/review.md becomes /review;
// the project directory wins over ~/.switchboard/commands on a name clash
// because the project speaks for itself.
//
// The body is a prompt template. $ARGUMENTS is everything after the command,
// $1..$9 are its fields, a backtick-quoted !`cmd` runs a shell command at
// expansion time and inlines its output, and @path attachments ride the same
// expansion every prompt gets.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

type customCommand struct {
	name string
	desc string
	body string

	// fromHome records which directory supplied the file, and it is a trust
	// statement, not a detail: ~/.switchboard/commands is the user speaking,
	// a repository's .switchboard/commands is whoever was cloned. Inline
	// shell runs only for the former — a checked-out repo must not get
	// commands executed by the act of typing a slash.
	fromHome bool
}

const (
	customCommandMaxBytes            = int64(64 << 10)
	customCommandMaxAggregateBytes   = int64(1 << 20)
	customCommandMaxDefinitions      = 128
	customCommandMaxDirectoryEntries = 512
	customCommandMaxNameBytes        = 128
	customCommandMaxDescriptionBytes = 4 << 10
	customInlineShellOutputCap       = 8 << 10
	customInlineShellCaptureCap      = 64 << 10
)

// customInlineShellOutput bounds a trusted user command's expansion-time
// subprocess without turning discarded bytes into a pipe error. Unlike the
// interactive shell fold, an overflow makes the entire output unusable: a
// credential may straddle the capture boundary, so returning the retained
// prefix would evade a precision-oriented credential scanner.
type customInlineShellOutput struct {
	mu       sync.Mutex
	buf      []byte
	overflow bool
}

func (o *customInlineShellOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	written := len(p)
	remaining := customInlineShellCaptureCap - len(o.buf)
	if remaining > 0 {
		keep := min(remaining, len(p))
		o.buf = append(o.buf, p[:keep]...)
	}
	if len(p) > remaining {
		o.overflow = true
	}
	return written, nil
}

func (o *customInlineShellOutput) snapshot() ([]byte, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return bytes.Clone(o.buf), o.overflow
}

type customCommandSource struct {
	anchor     string
	fromHome   bool
	scopeLabel string
}

type customCommandCandidate struct {
	name    string
	command customCommand
	valid   bool
}

// customSelector is the spelling that can actually reach a custom command.
// Built-ins, aliases, and tier shorthands keep their established meaning; a
// colliding file remains usable through an explicit namespace instead of
// appearing in completion as a command that dispatches somewhere else.
func customSelector(m *tuiModel, custom customCommand) string {
	if slashNameClaimed(m, custom.name) || strings.HasPrefix(custom.name, "custom:") ||
		strings.IndexFunc(custom.name, unicode.IsSpace) >= 0 {
		return "custom:" + url.PathEscape(custom.name)
	}
	return custom.name
}

func customNameFromSelector(selector string) (string, bool) {
	encoded, explicit := strings.CutPrefix(selector, "custom:")
	if !explicit {
		return "", false
	}
	name, err := url.PathUnescape(encoded)
	if err != nil {
		return "", false
	}
	return name, true
}

func slashNameClaimed(m *tuiModel, name string) bool {
	if m != nil && m.app != nil && m.app.config != nil {
		if _, ok := m.app.config.Tier(name); ok {
			return true
		}
	}
	for _, command := range commands() {
		if command.name == name {
			return true
		}
		for _, alias := range command.aliases {
			if alias == name {
				return true
			}
		}
	}
	return false
}

func customCommandDescription(custom customCommand) string {
	scope := "workspace command"
	if custom.fromHome {
		scope = "user command"
	}
	if custom.desc == "" || custom.desc == "custom command" {
		return scope
	}
	return scope + " · " + custom.desc
}

// loadCustomCommands reads both directories once at startup. Project first:
// on a name clash the repository's version wins, because the project speaks
// for its own workflows.
func loadCustomCommands(workspace string) []customCommand {
	commands, _ := loadCustomCommandsWithNotes(workspace)
	return commands
}

// loadCustomCommandsWithNotes keeps discovery bounded and returns only fixed,
// terminal-safe diagnostics. Repository filenames and I/O errors are untrusted
// metadata, so they never ride a startup notice.
func loadCustomCommandsWithNotes(workspace string) ([]customCommand, []string) {
	var out []customCommand
	var notes []string
	claimed := map[string]bool{}
	allowUserSource := true
	sources := []customCommandSource{{anchor: workspace, scopeLabel: "workspace"}}
	if home, err := os.UserHomeDir(); err == nil {
		sources = append(sources, customCommandSource{anchor: home, fromHome: true, scopeLabel: "user"})
	}

	for _, src := range sources {
		if src.fromHome && !allowUserSource {
			notes = append(notes, "user custom commands were not loaded because workspace command discovery failed closed")
			continue
		}
		candidates, rejected, reason, missing := loadCustomCommandSource(src)
		if missing {
			continue
		}
		if reason != "" {
			notes = append(notes, src.scopeLabel+" custom commands were not loaded: "+reason)
			if !src.fromHome {
				// A source-wide failure leaves no bounded inventory whose names can
				// tombstone user fallbacks. Suppress that lower-precedence source
				// instead of activating trusted inline shell under an ambiguous name.
				allowUserSource = false
			}
			continue
		}
		for _, candidate := range candidates {
			// A rejected workspace definition still owns its basename. Falling
			// through to a same-named user command could unexpectedly enable its
			// trusted inline shell under a spelling the repository appeared to own.
			if claimed[candidate.name] {
				continue
			}
			claimed[candidate.name] = true
			if !candidate.valid {
				continue
			}
			out = append(out, candidate.command)
		}
		if rejected > 0 {
			noun := "files"
			if rejected == 1 {
				noun = "file"
			}
			notes = append(notes, fmt.Sprintf(
				"ignored %d unsafe or invalid %s custom command %s", rejected, src.scopeLabel, noun))
		}
	}
	return out, notes
}

func loadCustomCommandSource(src customCommandSource) ([]customCommandCandidate, int, string, bool) {
	dir, err := openCustomCommandDirectory(src.anchor)
	if err != nil {
		return nil, 0, "could not be opened safely", os.IsNotExist(err)
	}
	defer dir.Close()

	directory, err := dir.Open(".")
	if err != nil {
		return nil, 0, "could not be opened safely", false
	}
	entries, readErr := directory.ReadDir(customCommandMaxDirectoryEntries + 1)
	closeErr := directory.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) || closeErr != nil {
		return nil, 0, "could not be read safely", false
	}
	if len(entries) > customCommandMaxDirectoryEntries {
		return nil, 0, fmt.Sprintf("directory exceeds the %d-entry limit", customCommandMaxDirectoryEntries), false
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	definitions := make([]os.DirEntry, 0, len(entries))
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".md") {
			definitions = append(definitions, entry)
		}
	}
	if len(definitions) > customCommandMaxDefinitions {
		return nil, 0, fmt.Sprintf("inventory exceeds the %d-definition limit", customCommandMaxDefinitions), false
	}

	var aggregate int64
	var candidates []customCommandCandidate
	rejected := 0
	for _, entry := range definitions {
		name := strings.TrimSuffix(entry.Name(), ".md")
		if !validCustomCommandName(name) {
			rejected++
			continue
		}
		candidate := customCommandCandidate{name: name}
		data, err := readCustomCommandDefinition(dir, entry.Name())
		if err != nil {
			rejected++
			candidates = append(candidates, candidate)
			continue
		}
		aggregate += int64(len(data))
		if aggregate > customCommandMaxAggregateBytes {
			return nil, 0, fmt.Sprintf("definitions exceed the %d-byte aggregate limit", customCommandMaxAggregateBytes), false
		}
		desc, body := splitFrontmatter(string(data))
		if strings.TrimSpace(body) == "" || len(desc) > customCommandMaxDescriptionBytes {
			rejected++
			candidates = append(candidates, candidate)
			continue
		}
		candidate.valid = true
		candidate.command = customCommand{name: name, desc: desc, body: body, fromHome: src.fromHome}
		candidates = append(candidates, candidate)
	}
	return candidates, rejected, "", false
}

// openCustomCommandDirectory walks the fixed source path one capability at a
// time. Both the workspace and user source reject symlinked .switchboard or
// commands directories, including symlinks whose target remains inside the
// selected root: command provenance is the path the UI names, not its target.
func openCustomCommandDirectory(anchor string) (*os.Root, error) {
	root, err := rootedfs.OpenRoot(anchor)
	if err != nil {
		return nil, err
	}
	for _, component := range []string{".switchboard", "commands"} {
		before, err := root.Lstat(component)
		if err != nil {
			root.Close()
			return nil, err
		}
		if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
			root.Close()
			return nil, errors.New("custom command path component is not a direct directory")
		}
		child, err := rootedfs.OpenRootAt(root, component)
		if err != nil {
			root.Close()
			return nil, err
		}
		opened, openErr := child.Stat(".")
		after, afterErr := root.Lstat(component)
		if openErr != nil || afterErr != nil || !opened.IsDir() ||
			!os.SameFile(before, opened) || !os.SameFile(opened, after) {
			child.Close()
			root.Close()
			return nil, errors.New("custom command directory changed while it was opened")
		}
		root.Close()
		root = child
	}
	return root, nil
}

func readCustomCommandDefinition(root *os.Root, name string) ([]byte, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, errors.New("custom command is not a direct regular file")
	}
	if before.Size() > customCommandMaxBytes {
		return nil, errors.New("custom command exceeds its byte limit")
	}

	file, err := openCustomCommandFile(root, name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, errors.New("custom command changed while it was opened")
	}
	data, err := io.ReadAll(io.LimitReader(file, customCommandMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > customCommandMaxBytes {
		return nil, errors.New("custom command exceeds its byte limit")
	}
	afterFD, err := file.Stat()
	if err != nil || !os.SameFile(opened, afterFD) || opened.Size() != afterFD.Size() ||
		!opened.ModTime().Equal(afterFD.ModTime()) || int64(len(data)) != afterFD.Size() {
		return nil, errors.New("custom command changed while it was read")
	}
	afterPath, err := root.Lstat(name)
	if err != nil || !afterPath.Mode().IsRegular() || !os.SameFile(afterFD, afterPath) {
		return nil, errors.New("custom command path changed while it was read")
	}
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		return nil, errors.New("custom command is not UTF-8 text")
	}
	return data, nil
}

func validCustomCommandName(name string) bool {
	return name != "" && len(name) <= customCommandMaxNameBytes && utf8.ValidString(name) &&
		strings.IndexFunc(name, unicode.IsControl) < 0
}

// splitFrontmatter reads an optional YAML block for its description line.
// Anything else in the frontmatter is ignored rather than erroring, so a file
// written for another tool loads here without editing.
func splitFrontmatter(content string) (desc, body string) {
	body = content
	if !strings.HasPrefix(content, "---\n") {
		return "custom command", body
	}
	rest := content[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "custom command", body
	}
	front := rest[:end]
	body = strings.TrimPrefix(rest[end+4:], "\n")
	desc = "custom command"
	for _, line := range strings.Split(front, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "description:"); ok {
			desc = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return desc, body
}

var inlineShell = regexp.MustCompile("!`([^`]+)`")

func renderCustomInlineShellOutput(captured []byte, overflow bool, runErr error) string {
	var text string
	if overflow {
		// Do not expose even the retained prefix: an otherwise recognized token
		// may begin there and finish in bytes deliberately discarded at the cap.
		text = fmt.Sprintf("[inline shell output omitted: exceeded the %d-byte capture limit]", customInlineShellCaptureCap)
	} else {
		text = strings.TrimRight(string(captured), "\r\n")
	}
	if runErr != nil {
		text += "\n[" + runErr.Error() + "]"
	}

	// Scan the complete captured value and error before UTF-8 repair or the
	// model-facing byte cap. Repair can expand invalid bytes, so cap once more
	// afterwards at a whole grapheme and scan the final composition as defense
	// against a token assembled across component boundaries.
	text = redactCredentialText(text)
	text = strings.ToValidUTF8(text, "�")
	text = redactCredentialText(text)
	bounded, truncated := truncateBytesAtGrapheme(text, customInlineShellOutputCap)
	if truncated {
		bounded += fmt.Sprintf("\n[truncated at %d-byte limit]", customInlineShellOutputCap)
	}
	return redactCredentialText(bounded)
}

// expandCustom renders a command body against its arguments. Inline shell
// runs now, at expansion, because a command that pastes today's diff into the
// prompt is the entire point of having one — but only when trusted: a project
// file's shell fragments are replaced with a note saying they did not run,
// since a cloned repository must not execute anything by being typed at.
func expandCustom(body, args, workspace string, trusted bool) string {
	fields := strings.Fields(args)
	body = strings.ReplaceAll(body, "$ARGUMENTS", args)
	for i := 9; i >= 1; i-- {
		val := ""
		if i <= len(fields) {
			val = fields[i-1]
		}
		body = strings.ReplaceAll(body, fmt.Sprintf("$%d", i), val)
	}

	return inlineShell.ReplaceAllStringFunc(body, func(match string) string {
		command := inlineShell.FindStringSubmatch(match)[1]
		if !trusted {
			return "[inline shell `" + command + "` skipped: it runs only from ~/.switchboard/commands, not from a repository's]"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := newUserShellCommandContext(ctx, command)
		cmd.Dir = workspace
		var out customInlineShellOutput
		cmd.Stdout = &out
		cmd.Stderr = &out
		err := cmd.Run()
		captured, overflow := out.snapshot()
		return renderCustomInlineShellOutput(captured, overflow, err)
	})
}

// expandedCustomMsg carries an expanded command body back to the model, so
// the inline shell's fifteen-second ceiling is spent off the UI goroutine.
// Its immutable binding prevents a late expansion from starting in a session
// that replaced the one in which the slash command was invoked.
type expandedCustomMsg struct {
	prompt     string
	authored   string
	generation uint64
	sessionID  string
}

func runCustom(m *tuiModel, c customCommand, args string) tea.Cmd {
	if m.busy {
		return noticeCmd("warn", "a turn is running; esc to interrupt it first")
	}
	workspace := m.app.workspace
	binding := m.bindAsyncResult()
	authored := "/" + customSelector(m, c)
	if args != "" {
		authored += " " + args
	}
	return func() tea.Msg {
		return expandedCustomMsg{
			prompt: expandCustom(c.body, args, workspace, c.fromHome), authored: authored,
			generation: binding.generation, sessionID: binding.sessionID,
		}
	}
}

func (m *tuiModel) startExpandedTurn(prompt, authored string) tea.Cmd {
	if m.busy || m.turnPlanning || m.operationActive {
		return noticeCmd("warn", "the custom command finished expanding after another turn started; run it again when the prompt is idle")
	}
	expanded, images := m.expandMentions(prompt)
	prompt = m.watchContext(m.adviceContext(m.shellContext(expanded)))
	leaks := credential.ScanPrompt(prompt)
	launch := func(providerPrompt string) tea.Cmd {
		display := authored
		if len(leaks) > 0 && providerPrompt != prompt {
			display = credential.Redact(display, leaks)
		}
		m.addUser(display)
		return m.launchTurnAuthored(providerPrompt, display, images)
	}
	if len(leaks) > 0 {
		return m.openSecretGate(leaks, prompt, launch)
	}
	return launch(prompt)
}
