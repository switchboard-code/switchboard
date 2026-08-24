package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
)

func TestCustomCommandsLoadAndProjectWins(t *testing.T) {
	home := t.TempDir()
	isolateTestHome(t, home)
	ws := filepath.Join(home, "project")

	global := filepath.Join(home, ".switchboard", "commands")
	local := filepath.Join(ws, ".switchboard", "commands")
	for _, dir := range []string{global, local} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(global, "review.md"), []byte("---\ndescription: the global one\n---\nglobal body"), 0o644)
	os.WriteFile(filepath.Join(local, "review.md"), []byte("---\ndescription: the project one\n---\nproject body"), 0o644)
	os.WriteFile(filepath.Join(global, "standup.md"), []byte("what changed since yesterday?"), 0o644)

	cmds := loadCustomCommands(ws)
	if len(cmds) != 2 {
		t.Fatalf("loaded %d commands, want 2: %+v", len(cmds), cmds)
	}
	byName := map[string]customCommand{}
	for _, c := range cmds {
		byName[c.name] = c
	}
	if byName["review"].body != "project body" {
		t.Fatalf("on a name clash the project must win, got %q", byName["review"].body)
	}
	if byName["review"].fromHome {
		t.Fatal("a project file must not carry the home directory's trust")
	}
	if !byName["standup"].fromHome {
		t.Fatal("a home-directory file lost its provenance")
	}
	if byName["standup"].desc != "custom command" {
		t.Fatalf("a file without frontmatter still loads, desc %q", byName["standup"].desc)
	}
}

func TestCustomCommandsRejectDefinitionSymlinksAndReserveTheirNames(t *testing.T) {
	home := t.TempDir()
	isolateTestHome(t, home)
	ws := t.TempDir()
	local := filepath.Join(ws, ".switchboard", "commands")
	global := filepath.Join(home, ".switchboard", "commands")
	for _, dir := range []string{local, global} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("outside secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(local, "payload.txt")
	if err := os.WriteFile(inside, []byte("inside target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(local, "deploy.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink("payload.txt", filepath.Join(local, "alias.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(global, "deploy.md"), []byte("trusted home deployment"), 0o600); err != nil {
		t.Fatal(err)
	}

	commands, notes := loadCustomCommandsWithNotes(ws)
	if len(commands) != 0 {
		t.Fatalf("symlink definitions or same-named user fallback loaded: %+v", commands)
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "ignored 2 unsafe or invalid workspace custom command files") {
		t.Fatalf("sanitized rejection summary missing: %q", joined)
	}
	for _, secret := range []string{"outside.md", "deploy.md", "alias.md", "outside secret"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("startup diagnostic exposed untrusted path or content %q: %q", secret, joined)
		}
	}
}

func TestCustomCommandsRejectSymlinkedSourceDirectories(t *testing.T) {
	home := t.TempDir()
	isolateTestHome(t, home)
	global := filepath.Join(home, ".switchboard", "commands")
	if err := os.MkdirAll(global, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(global, "leak.md"), []byte("trusted fallback must stay disabled"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name     string
		external bool
	}{
		{name: "internal target"},
		{name: "external target", external: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ws := t.TempDir()
			targetRoot := ws
			if test.external {
				targetRoot = t.TempDir()
			}
			target := filepath.Join(targetRoot, "real-switchboard")
			if err := os.MkdirAll(filepath.Join(target, "commands"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(target, "commands", "leak.md"), []byte("must not load"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(ws, ".switchboard")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}

			commands, notes := loadCustomCommandsWithNotes(ws)
			if len(commands) != 0 {
				t.Fatalf("command or lower-precedence fallback loaded through a rejected source: %+v", commands)
			}
			got := strings.Join(notes, "\n")
			for _, want := range []string{
				"workspace custom commands were not loaded: could not be opened safely",
				"user custom commands were not loaded because workspace command discovery failed closed",
			} {
				if !strings.Contains(got, want) {
					t.Fatalf("missing fixed source rejection %q: %q", want, got)
				}
			}
		})
	}
}

func TestCustomCommandsBoundTextAndRejectBinary(t *testing.T) {
	home := t.TempDir()
	isolateTestHome(t, home)
	ws := t.TempDir()
	dir := filepath.Join(ws, ".switchboard", "commands")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"valid.md":       []byte("a useful prompt"),
		"huge.md":        bytes.Repeat([]byte{'x'}, int(customCommandMaxBytes)+1),
		"nul.md":         {'a', 0, 'b'},
		"invalid.md":     {0xff, 0xfe},
		"description.md": []byte("---\ndescription: " + strings.Repeat("d", customCommandMaxDescriptionBytes+1) + "\n---\nprompt"),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	commands, notes := loadCustomCommandsWithNotes(ws)
	if len(commands) != 1 || commands[0].name != "valid" || commands[0].body != "a useful prompt" {
		t.Fatalf("bounded loader returned %+v", commands)
	}
	if got := strings.Join(notes, "\n"); !strings.Contains(got, "ignored 4 unsafe or invalid workspace custom command files") {
		t.Fatalf("bounded rejection summary missing: %q", got)
	}
}

func TestCustomCommandsEnforceInventoryCapsBeforeLoading(t *testing.T) {
	home := t.TempDir()
	isolateTestHome(t, home)

	t.Run("definitions", func(t *testing.T) {
		ws := t.TempDir()
		dir := filepath.Join(ws, ".switchboard", "commands")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for i := 0; i <= customCommandMaxDefinitions; i++ {
			name := filepath.Join(dir, fmt.Sprintf("command-%03d.md", i))
			if err := os.WriteFile(name, []byte("prompt"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		commands, notes := loadCustomCommandsWithNotes(ws)
		if len(commands) != 0 || !strings.Contains(strings.Join(notes, "\n"), "inventory exceeds the 128-definition limit") {
			t.Fatalf("definition cap result commands=%d notes=%q", len(commands), notes)
		}
	})

	t.Run("directory entries", func(t *testing.T) {
		ws := t.TempDir()
		dir := filepath.Join(ws, ".switchboard", "commands")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for i := 0; i <= customCommandMaxDirectoryEntries; i++ {
			name := filepath.Join(dir, fmt.Sprintf("entry-%03d.txt", i))
			if err := os.WriteFile(name, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		commands, notes := loadCustomCommandsWithNotes(ws)
		if len(commands) != 0 || !strings.Contains(strings.Join(notes, "\n"), "directory exceeds the 512-entry limit") {
			t.Fatalf("directory cap result commands=%d notes=%q", len(commands), notes)
		}
	})

	t.Run("aggregate bytes", func(t *testing.T) {
		ws := t.TempDir()
		dir := filepath.Join(ws, ".switchboard", "commands")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		definition := bytes.Repeat([]byte{'p'}, int(customCommandMaxBytes))
		count := int(customCommandMaxAggregateBytes/customCommandMaxBytes) + 1
		for i := 0; i < count; i++ {
			name := filepath.Join(dir, fmt.Sprintf("large-%03d.md", i))
			if err := os.WriteFile(name, definition, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		commands, notes := loadCustomCommandsWithNotes(ws)
		if len(commands) != 0 || !strings.Contains(strings.Join(notes, "\n"), "definitions exceed the 1048576-byte aggregate limit") {
			t.Fatalf("aggregate cap result commands=%d notes=%q", len(commands), notes)
		}
	})
}

func TestLoadedCustomMetadataIsSafeInSuggestionsAndPalette(t *testing.T) {
	home := t.TempDir()
	isolateTestHome(t, home)
	ws := t.TempDir()
	dir := filepath.Join(ws, ".switchboard", "commands")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := "wide-" + strings.Repeat("界", 18) + "🙂\u202e"
	description := "purpose\x1b]2;forged\a\u202e " + strings.Repeat("界", 80)
	definition := "---\ndescription: " + description + "\n---\nreview the workspace"
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(definition), 0o600); err != nil {
		t.Fatal(err)
	}
	if validCustomCommandName("bad\x1b") || validCustomCommandName("bad\nname") {
		t.Fatal("control-bearing custom command name passed validation")
	}

	commands, notes := loadCustomCommandsWithNotes(ws)
	if len(notes) != 0 || len(commands) != 1 || commands[0].name != name || commands[0].desc != description {
		t.Fatalf("loaded metadata commands=%+v notes=%q", commands, notes)
	}

	m := testModel(t)
	m.custom = commands
	m.width, m.height = 180, 12
	m.ta.SetValue("/wide")
	assertSafe := func(surface, view string, wantVisibleBidi bool) {
		t.Helper()
		plain := stripANSI(view)
		for _, unsafe := range []string{"\x1b", "\a", "\u202e"} {
			if strings.Contains(plain, unsafe) {
				t.Fatalf("%s retained terminal control %q: %q", surface, unsafe, plain)
			}
		}
		if wantVisibleBidi && !strings.Contains(plain, `\u202e`) {
			t.Fatalf("%s did not render bidi metadata visibly: %q", surface, plain)
		}
		for _, line := range strings.Split(view, "\n") {
			if width := lipgloss.Width(line); width > m.width {
				t.Fatalf("%s row is %d cells at width %d: %q", surface, width, m.width, line)
			}
		}
	}
	assertSafe("suggestions", m.suggestionsView(), true)

	m.openPalette()
	picker, ok := m.dlg.(*pickerDialog)
	if !ok {
		t.Fatalf("command palette dialog = %T", m.dlg)
	}
	picker.setQuery("wide")
	assertSafe("palette", renderDialogWithin(m.dlg, m.width, m.height, m.th), true)

	// The same metadata remains physically bounded after the escape spelling
	// expands and both surfaces have to truncate it in a narrow terminal.
	m.width, m.height = 24, 8
	assertSafe("narrow suggestions", m.suggestionsView(), false)
	assertSafe("narrow palette", renderDialogWithin(m.dlg, m.width, m.height, m.th), false)
}

func TestExpandCustomSubstitutesAndRunsInlineShell(t *testing.T) {
	body := "Review $1 with focus on $ARGUMENTS.\n\nBranch: !`echo fake-branch`"
	got := expandCustom(body, "cmd/sb correctness", t.TempDir(), true)

	for _, want := range []string{
		"Review cmd/sb with focus on cmd/sb correctness.",
		"Branch: fake-branch",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expansion missing %q:\n%s", want, got)
		}
	}
}

func TestCustomInlineShellRedactsBeforeOutputCap(t *testing.T) {
	token := "ghp_" + strings.Repeat("A", 36)
	raw := strings.Repeat("x", customInlineShellOutputCap-len(token)/2-1) + " " + token
	got := renderCustomInlineShellOutput([]byte(raw), false, nil)
	if strings.Contains(got, "ghp_") || !strings.Contains(got, "[redacted:") {
		t.Fatalf("output-cap projection retained a boundary credential: %q", got[len(got)-min(256, len(got)):])
	}
}

func TestCustomInlineShellCaptureExactAndCrossBoundary(t *testing.T) {
	token := "ghp_" + strings.Repeat("B", 36)

	var exact customInlineShellOutput
	exactRaw := token + strings.Repeat("x", customInlineShellCaptureCap-len(token))
	if n, err := exact.Write([]byte(exactRaw)); err != nil || n != len(exactRaw) {
		t.Fatalf("exact capture write = %d, %v; want %d, nil", n, err, len(exactRaw))
	}
	exactBytes, exactOverflow := exact.snapshot()
	if exactOverflow || len(exactBytes) != customInlineShellCaptureCap {
		t.Fatalf("exact capture len=%d overflow=%v", len(exactBytes), exactOverflow)
	}
	exactRendered := renderCustomInlineShellOutput(exactBytes, exactOverflow, nil)
	if strings.Contains(exactRendered, token) || !strings.Contains(exactRendered, "[redacted:") {
		t.Fatalf("exact-cap output was not scanned before truncation: %q", exactRendered[:min(256, len(exactRendered))])
	}

	var crossing customInlineShellOutput
	crossingRaw := strings.Repeat("x", customInlineShellCaptureCap-3) + token
	if n, err := crossing.Write([]byte(crossingRaw)); err != nil || n != len(crossingRaw) {
		t.Fatalf("crossing capture write = %d, %v; want %d, nil", n, err, len(crossingRaw))
	}
	crossingBytes, crossingOverflow := crossing.snapshot()
	if !crossingOverflow || len(crossingBytes) != customInlineShellCaptureCap {
		t.Fatalf("crossing capture len=%d overflow=%v", len(crossingBytes), crossingOverflow)
	}
	crossingRendered := renderCustomInlineShellOutput(crossingBytes, crossingOverflow, nil)
	if strings.Contains(crossingRendered, "ghp") || strings.Contains(crossingRendered, strings.Repeat("x", 32)) ||
		!strings.Contains(crossingRendered, "output omitted") {
		t.Fatalf("overflow did not fail closed: %q", crossingRendered)
	}
}

func TestCustomInlineShellInvalidUTF8IsSafe(t *testing.T) {
	raw := []byte{'o', 'k', ':', 0xff, 0xfe, 'x'}
	got := renderCustomInlineShellOutput(raw, false, nil)
	if !utf8.ValidString(got) || !strings.Contains(got, "�") {
		t.Fatalf("invalid UTF-8 was not repaired safely: %q", got)
	}
}

// A repository's command file gets substitution but never execution: typing a
// slash in a cloned repo must not run what the repo wrote.
func TestExpandCustomRefusesShellFromUntrustedFiles(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "pwned")
	body := "Do the thing.\n\n!`touch " + marker + "`"
	got := expandCustom(body, "", t.TempDir(), false)

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("an untrusted command file executed shell anyway")
	}
	if !strings.Contains(got, "skipped") {
		t.Fatalf("the refusal should be visible in the prompt, got:\n%s", got)
	}
}
