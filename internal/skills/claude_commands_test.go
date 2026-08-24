package skills

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func writeClaudeCommand(t *testing.T, root, rel, content string) string {
	t.Helper()
	path := filepath.Join(root, rel)
	writeSkill(t, filepath.Dir(path), filepath.Base(path), content)
	return path
}

func TestLoadDiscoversLegacyClaudeCommandsAsManualOnlySkills(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	top := t.TempDir()
	repo := filepath.Join(top, "repo")
	start := filepath.Join(repo, "apps", "api")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(start, 0o755); err != nil {
		t.Fatal(err)
	}

	startPath := writeClaudeCommand(t, filepath.Join(start, ".claude", "commands"), "review.md", `---
name: ignored-display-name
description: Review one issue
argument-hint: '[issue]'
arguments: [issue]
---
Fix $issue; all=<$ARGUMENTS>; first=<$0>`)
	parentPath := writeClaudeCommand(t, filepath.Join(repo, "apps", ".claude", "commands"), filepath.Join("database", "migrate.md"), "Migrate carefully.")
	rootPath := writeClaudeCommand(t, filepath.Join(repo, ".claude", "commands"), "release.md", "Release the repository.")
	userPath := writeClaudeCommand(t, filepath.Join(home, ".claude", "commands"), filepath.Join("team", "review.md"), "Review for the team.")
	writeClaudeCommand(t, filepath.Join(top, ".claude", "commands"), "above-root.md", "Must not load.")

	list, notes := Load(start)
	if len(notes) != 0 {
		t.Fatalf("legacy command discovery notes: %v", notes)
	}
	wantKeys := []string{
		"claude:repo:.claude/commands/release.md",
		"claude:repo:apps/.claude/commands/database/migrate.md",
		"claude:repo:apps/api/.claude/commands/review.md",
		"claude:user:team/review.md",
	}
	if got := skillKeys(list); !slices.Equal(got, wantKeys) {
		t.Fatalf("legacy command selectors = %v, want %v", got, wantKeys)
	}
	if visible := ModelVisible(list); len(visible) != 0 {
		t.Fatalf("legacy commands must never be model-visible: %+v", visible)
	}
	if desc := NewTool(list).Description(); strings.Contains(desc, "claude:") {
		t.Fatalf("manual commands leaked into the model tool schema:\n%s", desc)
	}

	wantPaths := map[string]string{
		wantKeys[0]: rootPath,
		wantKeys[1]: parentPath,
		wantKeys[2]: startPath,
		wantKeys[3]: userPath,
	}
	for key, logicalPath := range wantPaths {
		sk := findSkillKey(t, list, key)
		resolved, err := filepath.EvalSymlinks(logicalPath)
		if err != nil {
			t.Fatal(err)
		}
		wantScope := ScopeWorkspace
		if strings.HasPrefix(key, "claude:user:") {
			wantScope = ScopeUser
		}
		logicalResolved, logicalErr := filepath.EvalSymlinks(sk.Origin.LogicalPath)
		if sk.Origin.Ecosystem != EcosystemClaude || sk.Origin.Scope != wantScope ||
			logicalErr != nil || logicalResolved != resolved || sk.Origin.Path != resolved || !sk.ImplicitDisabled {
			t.Errorf("%s origin/manual state = %+v", key, sk)
		}
	}

	review := findSkillKey(t, list, wantKeys[2])
	if review.Name != "review" || review.Description != "Review one issue" || review.ArgumentHint != "[issue]" {
		t.Fatalf("legacy command metadata = %+v", review)
	}
	if got, err := RenderExplicit(review, `"issue 42"`); err != nil || got != `Fix issue 42; all=<"issue 42">; first=<issue 42>` {
		t.Fatalf("legacy command render = %q, %v", got, err)
	}
}

func TestLegacyClaudeCommandDerivedDescriptionStaysLocalAndBodyEgressIsRedacted(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	ws := t.TempDir()
	secret := "ghp_" + strings.Repeat("e", 36)
	body := "Review with " + secret + "."
	writeClaudeCommand(t, filepath.Join(ws, ".claude", "commands"), "sensitive.md", body)

	list, notes := Load(ws)
	if len(notes) != 0 || len(list) != 1 {
		t.Fatalf("legacy sensitive command loaded %+v, notes %v", list, notes)
	}
	sk := list[0]
	if !sk.ImplicitDisabled || sk.Description != body || sk.Body != body {
		t.Fatalf("legacy source inventory changed: %+v", sk)
	}
	if strings.Contains(NewTool(list).Description(), secret) {
		t.Fatal("manual-only command description entered the frozen model inventory")
	}
	rendered, err := RenderExplicit(sk, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rendered, secret) || !strings.Contains(rendered, "[redacted: a GitHub token]") {
		t.Fatalf("legacy command body egress was not visibly redacted: %q", rendered)
	}
	if sk.Description != body || sk.Body != body {
		t.Fatal("legacy command source inventory was mutated")
	}
}

func TestClaudeSkillWinsSameScopeNativeCommandName(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	ws := t.TempDir()

	writePackedSkill(t, filepath.Join(ws, ".claude", "skills"), "deploy", "---\nname: Fancy Deploy\ndescription: workspace skill\n---\nskill")
	writeClaudeCommand(t, filepath.Join(ws, ".claude", "commands"), "deploy.md", "direct command")
	writeClaudeCommand(t, filepath.Join(ws, ".claude", "commands"), filepath.Join("ops", "deploy.md"), "nested command")
	writeClaudeCommand(t, filepath.Join(home, ".claude", "commands"), "deploy.md", "different-scope user command")

	writePackedSkill(t, filepath.Join(home, ".claude", "skills"), "personal", "---\nname: Renamed Personal\ndescription: user skill\n---\nskill")
	writeClaudeCommand(t, filepath.Join(home, ".claude", "commands"), "personal.md", "user command")
	writeClaudeCommand(t, filepath.Join(ws, ".claude", "commands"), "personal.md", "different-scope workspace command")

	list, notes := Load(ws)
	wantKeys := []string{
		"claude:repo:.claude/commands/personal.md",
		"claude:repo:.claude/skills/deploy",
		"claude:user:deploy.md",
		"claude:user:personal",
	}
	if got := skillKeys(list); !slices.Equal(got, wantKeys) {
		t.Fatalf("same-scope collision inventory = %v, want %v; notes %v", got, wantKeys, notes)
	}
	joined := strings.Join(notes, "\n")
	if strings.Count(joined, "wins native command name") != 3 ||
		!strings.Contains(joined, "claude:repo:.claude/skills/deploy") ||
		!strings.Contains(joined, filepath.Join("ops", "deploy.md")) ||
		!strings.Contains(joined, "personal.md") {
		t.Fatalf("omitted commands need deterministic diagnostics:\n%s", joined)
	}
	if got := findSkillKey(t, list, "claude:user:deploy.md").Body; got != "different-scope user command" {
		t.Fatalf("workspace skill incorrectly shadowed user command: %q", got)
	}
	if got := findSkillKey(t, list, "claude:repo:.claude/commands/personal.md").Body; got != "different-scope workspace command" {
		t.Fatalf("user skill incorrectly shadowed workspace command: %q", got)
	}
}

func TestClaudeCommandsFailClosedOnUnsupportedHostBehavior(t *testing.T) {
	setTestHome(t, t.TempDir())
	ws := t.TempDir()
	root := filepath.Join(ws, ".claude", "commands")
	commands := map[string]string{
		"allowed.md":    "---\ndescription: allowed\nallowed-tools: Read Bash\n---\nbody",
		"model.md":      "---\ndescription: model\nmodel: opus\n---\nbody",
		"hooks.md":      "---\ndescription: hooks\nhooks:\n  PreToolUse: []\n---\nbody",
		"shell.md":      "---\ndescription: shell\nshell: powershell\n---\nbody",
		"injection.md":  "---\ndescription: injection\n---\nUse !`git diff`",
		"dynamic.md":    "---\ndescription: dynamic\n---\nLog ${CLAUDE_SESSION_ID}",
		"attachment.md": "---\ndescription: attachment\n---\nReview **@private.env**",
		"safe.md":       "---\nname: \"unterminated\ndescription: Safe command\npaths: src/**\nargument-hint: '[target]'\n---\nEmail dev@example.com, then use $ARGUMENTS",
	}
	for name, content := range commands {
		writeClaudeCommand(t, root, name, content)
	}

	list, notes := Load(ws)
	if len(list) != len(commands) {
		t.Fatalf("blocked commands must remain inspectable: %+v; notes %v", list, notes)
	}
	if len(ModelVisible(list)) != 0 {
		t.Fatalf("a command became model-visible: %+v", ModelVisible(list))
	}
	// allowed-tools is a permission grant rather than a restriction: it
	// pre-approves tools so the skill's turn is not interrupted. Leaving it
	// unapplied can only ask more often than the author intended, so the
	// command stays usable and the difference is recorded on it.
	allowed := findSkillKey(t, list, "claude:repo:.claude/commands/allowed.md")
	if len(allowed.InvocationBlockers) != 0 {
		t.Errorf("a permission grant should not block the command: %+v", allowed.InvocationBlockers)
	}
	if len(allowed.Notes) == 0 || !strings.Contains(strings.Join(allowed.Notes, " "), "allowed-tools") {
		t.Errorf("the unapplied grant has to be recorded, not dropped: %+v", allowed.Notes)
	}
	if got, err := RenderExplicit(allowed, ""); err != nil || got != "body" {
		t.Errorf("allowed render = %q, %v; want the body", got, err)
	}

	for _, name := range []string{"model", "hooks", "shell", "injection", "dynamic", "attachment"} {
		sk := findSkillKey(t, list, "claude:repo:.claude/commands/"+name+".md")
		if len(sk.InvocationBlockers) == 0 {
			t.Errorf("%s silently dropped unsupported behavior: %+v", name, sk)
		}
		if _, err := RenderExplicit(sk, ""); err == nil {
			t.Errorf("%s rendered despite unsupported behavior", name)
		}
	}
	safe := findSkillKey(t, list, "claude:repo:.claude/commands/safe.md")
	if safe.Name != "safe" || safe.ArgumentHint != "[target]" || len(safe.InvocationBlockers) != 0 {
		t.Fatalf("ignored command-only metadata changed safe command: %+v", safe)
	}
	if got, err := RenderExplicit(safe, "api"); err != nil || got != "Email dev@example.com, then use api" {
		t.Fatalf("safe command render = %q, %v", got, err)
	}
	joined := strings.Join(notes, "\n")
	for _, want := range []string{"model", "hooks", "shell", "shell injection", "dynamic context", "file attachment"} {
		if !strings.Contains(joined, want) {
			t.Errorf("blocked-command diagnostics missing %q:\n%s", want, joined)
		}
	}
}

func TestClaudeCommandSymlinksStayWithinTheirRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not generally available")
	}
	setTestHome(t, t.TempDir())
	ws := t.TempDir()
	root := filepath.Join(ws, ".claude", "commands")
	realFile := writeClaudeCommand(t, root, "real.md", "real body")
	if err := os.Symlink("real.md", filepath.Join(root, "00-alias.md")); err != nil {
		t.Fatal(err)
	}
	realNested := writeClaudeCommand(t, root, filepath.Join("group", "nested.md"), "nested body")
	if err := os.Symlink("group", filepath.Join(root, "00-group")); err != nil {
		t.Fatal(err)
	}
	outside := writeClaudeCommand(t, t.TempDir(), "outside.md", "outside body")
	if err := os.Symlink(outside, filepath.Join(root, "escape.md")); err != nil {
		t.Fatal(err)
	}

	list, notes := Load(ws)
	wantKeys := []string{
		"claude:repo:.claude/commands/00-alias.md",
		"claude:repo:.claude/commands/00-group/nested.md",
	}
	if got := skillKeys(list); !slices.Equal(got, wantKeys) {
		t.Fatalf("symlinked command inventory = %v, want %v; notes %v", got, wantKeys, notes)
	}
	resolvedFile, _ := filepath.EvalSymlinks(realFile)
	resolvedNested, _ := filepath.EvalSymlinks(realNested)
	if got := list[0].Origin.Path; got != resolvedFile {
		t.Errorf("file alias canonical origin = %q, want %q", got, resolvedFile)
	}
	if got := list[1].Origin.Path; got != resolvedNested {
		t.Errorf("directory alias canonical origin = %q, want %q", got, resolvedNested)
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "escape.md") || !strings.Contains(joined, "leaves its command root") || strings.Contains(joined, "outside body") {
		t.Fatalf("escaping symlink diagnostic = %q", joined)
	}
}

func TestClaudeCommandDefinitionIsPinnedBetweenDiscoveryAndRead(t *testing.T) {
	setTestHome(t, t.TempDir())
	ws := t.TempDir()
	root := filepath.Join(ws, ".claude", "commands")
	path := writeClaudeCommand(t, root, "pinned.md", "original")
	src := claudeCommandSources(ws)[0]
	candidates, notes, err := discoverClaudeCommandCandidates(&src)
	if err != nil || len(notes) != 0 || len(candidates) != 1 {
		t.Fatalf("discovery = %+v, notes %v, err %v", candidates, notes, err)
	}

	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	writeClaudeCommand(t, root, "pinned.md", "replacement")
	if data, err := readClaudeCommandDefinition(src, candidates[0]); err == nil || len(data) != 0 || !strings.Contains(err.Error(), "changed after discovery") {
		t.Fatalf("replaced definition read = %q, %v", data, err)
	}

	// Rediscover the replacement, then replace the canonical root itself.
	src = claudeCommandSources(ws)[0]
	candidates, notes, err = discoverClaudeCommandCandidates(&src)
	if err != nil || len(notes) != 0 || len(candidates) != 1 {
		t.Fatalf("rediscovery = %+v, notes %v, err %v", candidates, notes, err)
	}
	retired := root + ".old"
	if err := os.Rename(root, retired); err != nil {
		t.Fatal(err)
	}
	writeClaudeCommand(t, root, "pinned.md", "new root")
	if data, err := readClaudeCommandDefinition(src, candidates[0]); err == nil || len(data) != 0 || !strings.Contains(err.Error(), "root changed") {
		t.Fatalf("replaced root read = %q, %v", data, err)
	}
}

func TestClaudeCommandDiscoveryBoundsSizeDepthAndEntries(t *testing.T) {
	t.Run("definition size", func(t *testing.T) {
		setTestHome(t, t.TempDir())
		ws := t.TempDir()
		writeClaudeCommand(t, filepath.Join(ws, ".claude", "commands"), "huge.md", strings.Repeat("x", int(maxDefinitionBytes)+1))
		list, notes := Load(ws)
		if len(list) != 0 || !strings.Contains(strings.Join(notes, "\n"), "exceeds the") {
			t.Fatalf("oversized command loaded: %+v; notes %v", list, notes)
		}
	})

	t.Run("depth", func(t *testing.T) {
		setTestHome(t, t.TempDir())
		ws := t.TempDir()
		root := filepath.Join(ws, ".claude", "commands")
		rel := ""
		for i := 0; i <= maxClaudeCommandDepth; i++ {
			rel = filepath.Join(rel, "d"+strconv.Itoa(i))
		}
		writeClaudeCommand(t, root, filepath.Join(rel, "too-deep.md"), "body")
		list, notes := Load(ws)
		if len(list) != 0 || !strings.Contains(strings.Join(notes, "\n"), "depth limit") {
			t.Fatalf("deep command loaded: %+v; notes %v", list, notes)
		}
	})

	t.Run("entries reject the whole tree deterministically", func(t *testing.T) {
		setTestHome(t, t.TempDir())
		ws := t.TempDir()
		root := filepath.Join(ws, ".claude", "commands")
		for i := 0; i <= maxClaudeCommandEntries; i++ {
			writeClaudeCommand(t, root, "entry-"+strconv.Itoa(i)+".txt", "ignored")
		}
		writeClaudeCommand(t, root, "would-have-loaded.md", "body")
		first, firstNotes := Load(ws)
		second, secondNotes := Load(ws)
		if len(first) != 0 || len(second) != 0 || !slices.Equal(firstNotes, secondNotes) ||
			!strings.Contains(strings.Join(firstNotes, "\n"), "entry limit exceeded") {
			t.Fatalf("entry-bound discovery = %+v/%+v; notes %v then %v", first, second, firstNotes, secondNotes)
		}
	})
}

func FuzzParseClaudeCommandNeverPanics(f *testing.F) {
	f.Add("command", minimal)
	f.Add("command", "---\ndescription: command\nargument-hint: '[arg]'\n---\nUse $ARGUMENTS")
	f.Add("command", "---\ndescription: blocked\nallowed-tools: Bash\n---\n!`pwd`")
	f.Fuzz(func(t *testing.T, fallback, content string) {
		sk, _, _ := parseClaudeCommandDocument(fallback, content)
		_ = claudeBodyBlockers(sk.Body)
	})
}
