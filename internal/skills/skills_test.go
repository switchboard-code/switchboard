package skills

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/tools"
)

func writeSkill(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writePackedSkill(t *testing.T, root, name, content string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	writeSkill(t, dir, "SKILL.md", content)
	return filepath.Join(dir, "SKILL.md")
}

func setTestHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	volume := filepath.VolumeName(home)
	t.Setenv("HOMEDRIVE", volume)
	t.Setenv("HOMEPATH", strings.TrimPrefix(home, volume))
}

func skillNames(list []Skill) []string {
	names := make([]string, len(list))
	for i := range list {
		names[i] = list[i].Name
	}
	return names
}

func skillKeys(list []Skill) []string {
	keys := make([]string, len(list))
	for i := range list {
		keys[i] = list[i].Key()
	}
	return keys
}

func findSkillKey(t *testing.T, list []Skill, key string) Skill {
	t.Helper()
	for _, sk := range list {
		if sk.Key() == key {
			return sk
		}
	}
	t.Fatalf("skill %q not found in %+v", key, list)
	return Skill{}
}

func findSkill(t *testing.T, list []Skill, name string) Skill {
	t.Helper()
	for _, sk := range list {
		if sk.Name == name {
			return sk
		}
	}
	t.Fatalf("skill %q not found in %+v", name, list)
	return Skill{}
}

const minimal = "---\ndescription: does a thing\n---\n\nThe instructions.\n"

func TestLoadReadsBothShapes(t *testing.T) {
	setTestHome(t, t.TempDir())
	ws := t.TempDir()
	skillsDir := filepath.Join(ws, ".switchboard", "skills")
	writeSkill(t, skillsDir, "flat.md", minimal)
	writeSkill(t, filepath.Join(skillsDir, "packed"), "SKILL.md", minimal)

	list, notes := Load(ws)
	if len(notes) != 0 {
		t.Fatalf("unexpected notes: %v", notes)
	}
	if len(list) != 2 || list[0].Name != "flat" || list[1].Name != "packed" {
		t.Fatalf("loaded %+v, want flat and packed, sorted", list)
	}
	if list[1].Dir != filepath.Join(skillsDir, "packed") {
		t.Errorf("a packed skill's dir must be its own folder, got %s", list[1].Dir)
	}
	if list[0].Dir != skillsDir {
		t.Errorf("a flat skill's dir is the skills directory, got %s", list[0].Dir)
	}
}

func TestLoadRetainsProjectAndHomeNameClashWithCanonicalSelectors(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	ws := t.TempDir()
	writeSkill(t, filepath.Join(ws, ".switchboard", "skills"), "review.md",
		"---\ndescription: the project one\n---\nproject body")
	writeSkill(t, filepath.Join(home, ".switchboard", "skills"), "review.md",
		"---\ndescription: the home one\n---\nhome body")

	list, _ := Load(ws)
	if len(list) != 2 {
		t.Fatalf("both source-qualified definitions must remain in inventory: %+v", list)
	}
	project := findSkillKey(t, list, "switchboard:repo:review.md")
	user := findSkillKey(t, list, "switchboard:user:review.md")
	if project.Description != "the project one" || project.FromHome ||
		user.Description != "the home one" || !user.FromHome {
		t.Fatalf("canonical definitions lost their origins: %+v", list)
	}
}

func TestLoadDiscoversNativeTreesAndRecordsOrigins(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	ws := t.TempDir()

	paths := map[string]string{
		"switchboard":      writePackedSkill(t, filepath.Join(ws, ".switchboard", "skills"), "switchboard", minimal),
		"codex":            writePackedSkill(t, filepath.Join(ws, ".agents", "skills"), "codex", minimal),
		"claude":           writePackedSkill(t, filepath.Join(ws, ".claude", "skills"), "claude", minimal),
		"user-switchboard": writePackedSkill(t, filepath.Join(home, ".switchboard", "skills"), "user-switchboard", minimal),
		"user-codex":       writePackedSkill(t, filepath.Join(home, ".agents", "skills"), "user-codex", minimal),
		"user-claude":      writePackedSkill(t, filepath.Join(home, ".claude", "skills"), "user-claude", minimal),
	}

	list, notes := Load(ws)
	if len(notes) != 0 {
		t.Fatalf("unexpected notes: %v", notes)
	}
	wantNames := []string{"claude", "user-claude", "codex", "user-codex", "switchboard", "user-switchboard"}
	if got := skillNames(list); !slices.Equal(got, wantNames) {
		t.Fatalf("loaded %v, want %v", got, wantNames)
	}

	wantOrigin := map[string]Origin{
		"switchboard":      {Ecosystem: EcosystemSwitchboard, Scope: ScopeWorkspace},
		"codex":            {Ecosystem: EcosystemCodex, Scope: ScopeWorkspace},
		"claude":           {Ecosystem: EcosystemClaude, Scope: ScopeWorkspace},
		"user-switchboard": {Ecosystem: EcosystemSwitchboard, Scope: ScopeUser},
		"user-codex":       {Ecosystem: EcosystemCodex, Scope: ScopeUser},
		"user-claude":      {Ecosystem: EcosystemClaude, Scope: ScopeUser},
	}
	for name, want := range wantOrigin {
		sk := findSkill(t, list, name)
		resolved, err := filepath.EvalSymlinks(paths[name])
		if err != nil {
			t.Fatal(err)
		}
		if sk.Origin.Ecosystem != want.Ecosystem || sk.Origin.Scope != want.Scope || sk.Origin.Path != resolved {
			t.Errorf("%s origin = %+v, want ecosystem=%s scope=%s path=%s", name, sk.Origin, want.Ecosystem, want.Scope, resolved)
		}
		logicalResolved, err := filepath.EvalSymlinks(sk.Origin.LogicalPath)
		if err != nil || logicalResolved != resolved {
			t.Errorf("%s logical path %q does not identify %q: %v", name, sk.Origin.LogicalPath, resolved, err)
		}
		if sk.FromHome != (want.Scope == ScopeUser) {
			t.Errorf("%s FromHome = %v, want scope-compatible value", name, sk.FromHome)
		}
	}
}

func TestLoadWalksNativeParentsOnlyThroughGitRoot(t *testing.T) {
	setTestHome(t, t.TempDir())
	top := t.TempDir()
	repo := filepath.Join(top, "repo")
	start := filepath.Join(repo, "packages", "app")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(start, 0o755); err != nil {
		t.Fatal(err)
	}
	writePackedSkill(t, filepath.Join(start, ".agents", "skills"), "nearest", minimal)
	writePackedSkill(t, filepath.Join(repo, "packages", ".claude", "skills"), "parent", minimal)
	writePackedSkill(t, filepath.Join(repo, ".agents", "skills"), "root", minimal)
	writePackedSkill(t, filepath.Join(start, ".claude", "skills"), "collision", "---\ndescription: nearest wins\n---\nbody")
	writePackedSkill(t, filepath.Join(repo, ".agents", "skills"), "collision", "---\ndescription: root loses\n---\nbody")
	writePackedSkill(t, filepath.Join(top, ".agents", "skills"), "above-root", minimal)

	list, notes := Load(start)
	if len(notes) != 0 {
		t.Fatalf("source-qualified collisions are not errors: %v", notes)
	}
	want := []string{
		"claude:repo:packages/.claude/skills/parent",
		"claude:repo:packages/app/.claude/skills/collision",
		"codex:repo:.agents/skills/collision",
		"codex:repo:.agents/skills/root",
		"codex:repo:packages/app/.agents/skills/nearest",
	}
	if got := skillKeys(list); !slices.Equal(got, want) {
		t.Fatalf("loaded %v, want %v", got, want)
	}
	if got := findSkillKey(t, list, want[1]).Description; got != "nearest wins" {
		t.Fatalf("qualified nearest definition changed, got %q", got)
	}
	if got := findSkillKey(t, list, want[2]).Description; got != "root loses" {
		t.Fatalf("qualified root definition changed, got %q", got)
	}

	t.Run("git worktree file is also a boundary", func(t *testing.T) {
		home := t.TempDir()
		setTestHome(t, home)
		worktree := filepath.Join(t.TempDir(), "worktree")
		child := filepath.Join(worktree, "child")
		if err := os.MkdirAll(child, 0o755); err != nil {
			t.Fatal(err)
		}
		writeSkill(t, worktree, ".git", "gitdir: elsewhere\n")
		writePackedSkill(t, filepath.Join(worktree, ".agents", "skills"), "worktree-root", minimal)
		loaded, gotNotes := Load(child)
		if len(gotNotes) != 0 || len(loaded) != 1 || loaded[0].Name != "worktree-root" {
			t.Fatalf("worktree boundary loaded %+v with notes %v", loaded, gotNotes)
		}
	})

	t.Run("outside a repository does not scan arbitrary parents", func(t *testing.T) {
		home := t.TempDir()
		setTestHome(t, home)
		base := t.TempDir()
		child := filepath.Join(base, "child")
		if err := os.MkdirAll(child, 0o755); err != nil {
			t.Fatal(err)
		}
		writePackedSkill(t, filepath.Join(base, ".agents", "skills"), "parent", minimal)
		loaded, _ := Load(child)
		if len(loaded) != 0 {
			t.Fatalf("outside a repository only the workspace should be scanned: %+v", loaded)
		}
	})
}

func TestLoadUsesDeterministicCanonicalCrossEcosystemInventory(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	ws := t.TempDir()

	writeSkill(t, filepath.Join(ws, ".switchboard", "skills"), "same.md", "---\ndescription: workspace switchboard\n---\nbody")
	writePackedSkill(t, filepath.Join(ws, ".agents", "skills"), "same", "---\ndescription: workspace codex\n---\nbody")
	writePackedSkill(t, filepath.Join(ws, ".claude", "skills"), "same", "---\ndescription: workspace claude\n---\nbody")
	writeSkill(t, filepath.Join(home, ".switchboard", "skills"), "same.md", "---\ndescription: user switchboard\n---\nbody")

	writePackedSkill(t, filepath.Join(ws, ".agents", "skills"), "native", "---\ndescription: codex\n---\nbody")
	writePackedSkill(t, filepath.Join(ws, ".claude", "skills"), "native", "---\ndescription: claude\n---\nbody")

	writePackedSkill(t, filepath.Join(ws, ".claude", "skills"), "project", "---\ndescription: project\n---\nbody")
	writeSkill(t, filepath.Join(home, ".switchboard", "skills"), "project.md", "---\ndescription: user\n---\nbody")

	list, notes := Load(ws)
	if len(notes) != 0 || len(list) != 8 {
		t.Fatalf("canonical collisions should all load without warnings: %+v, notes %v", list, notes)
	}
	wants := map[string]string{
		"switchboard:repo:same.md":           "workspace switchboard",
		"codex:repo:.agents/skills/same":     "workspace codex",
		"claude:repo:.claude/skills/same":    "workspace claude",
		"switchboard:user:same.md":           "user switchboard",
		"codex:repo:.agents/skills/native":   "codex",
		"claude:repo:.claude/skills/native":  "claude",
		"claude:repo:.claude/skills/project": "project",
		"switchboard:user:project.md":        "user",
	}
	for key, description := range wants {
		if got := findSkillKey(t, list, key).Description; got != description {
			t.Errorf("%s description = %q, want %q", key, got, description)
		}
	}

	again, _ := Load(ws)
	if !slices.Equal(skillKeys(list), skillKeys(again)) {
		t.Fatalf("repeated discovery changed order: %v then %v", skillKeys(list), skillKeys(again))
	}
}

func TestNativeTreesIgnoreFlatMarkdown(t *testing.T) {
	setTestHome(t, t.TempDir())
	ws := t.TempDir()
	writeSkill(t, filepath.Join(ws, ".agents", "skills"), "flat.md", minimal)
	writeSkill(t, filepath.Join(ws, ".claude", "skills"), "also-flat.md", minimal)

	list, notes := Load(ws)
	if len(list) != 0 || len(notes) != 0 {
		t.Fatalf("native trees only accept <name>/SKILL.md, got %+v, notes %v", list, notes)
	}
}

func TestClaudeCanDeriveDescriptionButCodexStillRequiresIt(t *testing.T) {
	setTestHome(t, t.TempDir())
	ws := t.TempDir()
	writePackedSkill(t, filepath.Join(ws, ".claude", "skills"), "claude-no-description",
		"First paragraph\ncontinues here.\n\nSecond paragraph.")
	writePackedSkill(t, filepath.Join(ws, ".agents", "skills"), "codex-no-description", "body only")

	list, notes := Load(ws)
	if got := skillNames(list); !slices.Equal(got, []string{"claude-no-description"}) {
		t.Fatalf("native description defaults loaded %v", got)
	}
	if got := list[0].Description; got != "First paragraph continues here." {
		t.Fatalf("derived Claude description = %q", got)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "codex-no-description") {
		t.Fatalf("missing Codex description needs a diagnostic: %v", notes)
	}
}

func TestLoadDoesNotAdvertiseManualOnlySkills(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	ws := t.TempDir()

	codexDir := filepath.Join(ws, ".agents", "skills", "deploy")
	writeSkill(t, codexDir, "SKILL.md", minimal)
	writeSkill(t, filepath.Join(codexDir, "agents"), "openai.yaml", "policy:\n  allow_implicit_invocation: false\n")
	writePackedSkill(t, filepath.Join(home, ".agents", "skills"), "deploy", minimal) // must remain shadowed

	writePackedSkill(t, filepath.Join(ws, ".claude", "skills"), "publish",
		"---\ndescription: publish\ndisable-model-invocation: true\n---\nbody")
	writePackedSkill(t, filepath.Join(home, ".claude", "skills"), "publish", minimal) // must remain shadowed
	copiedDir := filepath.Join(ws, ".switchboard", "skills", "copied")
	writeSkill(t, copiedDir, "SKILL.md", minimal)
	writeSkill(t, filepath.Join(copiedDir, "agents"), "openai.yaml", "policy:\n  allow_implicit_invocation: false\n")

	autoDir := filepath.Join(ws, ".agents", "skills", "review")
	writeSkill(t, autoDir, "SKILL.md", "---\ndescription: review\ndisable-model-invocation: false\n---\nbody")
	writeSkill(t, filepath.Join(autoDir, "agents"), "openai.yaml", "policy: { allow_implicit_invocation: true }\n")

	list, notes := Load(ws)
	if len(list) != 6 {
		t.Fatalf("complete inventory lost manual-only definitions: %+v", list)
	}
	if len(notes) != 0 {
		t.Fatalf("valid invocation opt-outs are inventory state, not load errors: %v", notes)
	}
	wantVisible := []string{
		"claude:user:publish",
		"codex:repo:.agents/skills/review",
		"codex:user:deploy",
	}
	visible := ModelVisible(list)
	if got := skillKeys(visible); !slices.Equal(got, wantVisible) {
		t.Fatalf("model-visible selectors = %v, want %v", got, wantVisible)
	}
	for _, key := range []string{
		"claude:repo:.claude/skills/publish",
		"codex:repo:.agents/skills/deploy",
		"switchboard:repo:copied",
	} {
		if !findSkillKey(t, list, key).ImplicitDisabled {
			t.Errorf("manual-only selector %s lost its opt-out", key)
		}
		if strings.Contains(NewTool(list).Description(), "- "+key+":") {
			t.Errorf("manual-only selector %s advertised to the model", key)
		}
	}
}

func TestMalformedInvocationPolicyFailsClosed(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	ws := t.TempDir()

	codexDir := filepath.Join(ws, ".agents", "skills", "codex-bad")
	writeSkill(t, codexDir, "SKILL.md", minimal)
	writeSkill(t, filepath.Join(codexDir, "agents"), "openai.yaml", "policy:\n  allow_implicit_invocation: perhaps\n")
	codexDynamic := filepath.Join(ws, ".agents", "skills", "codex-dynamic")
	writeSkill(t, codexDynamic, "SKILL.md", minimal)
	writeSkill(t, filepath.Join(codexDynamic, "agents"), "openai.yaml",
		"key: &manual allow_implicit_invocation\n? *manual\n: false\n")
	writePackedSkill(t, filepath.Join(ws, ".claude", "skills"), "claude-bad",
		"---\ndescription: bad\ndisable-model-invocation: perhaps\n---\nbody")
	writePackedSkill(t, filepath.Join(ws, ".claude", "skills"), "unterminated",
		"---\ndescription: bad\ndisable-model-invocation: true\nbody without a closing marker")
	writePackedSkill(t, filepath.Join(ws, ".claude", "skills"), "dynamic-key",
		"---\nkey: &manual disable-model-invocation\n? *manual\n: true\ndescription: bad\n---\nbody")
	writePackedSkill(t, filepath.Join(home, ".agents", "skills"), "codex-bad", minimal)
	writePackedSkill(t, filepath.Join(home, ".agents", "skills"), "codex-dynamic", minimal)
	writePackedSkill(t, filepath.Join(home, ".claude", "skills"), "claude-bad", minimal)
	writePackedSkill(t, filepath.Join(home, ".claude", "skills"), "unterminated", minimal)
	writePackedSkill(t, filepath.Join(home, ".claude", "skills"), "dynamic-key", minimal)

	list, notes := Load(ws)
	if len(list) != 5 || len(ModelVisible(list)) != 5 {
		t.Fatalf("malformed project definitions must not erase distinct safe user selectors: %+v", list)
	}
	for _, sk := range list {
		if sk.Origin.Scope != ScopeUser {
			t.Errorf("malformed project definition loaded: %+v", sk)
		}
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "allow_implicit_invocation") ||
		!strings.Contains(joined, "disable-model-invocation") ||
		!strings.Contains(joined, "unterminated") ||
		!strings.Contains(joined, "dynamic YAML keys") {
		t.Fatalf("malformed policies need actionable diagnostics:\n%s", joined)
	}
}

func TestMergedClaudeInvocationOptOutIsConservativelyHonored(t *testing.T) {
	_, manualOnly, err := parseDocument("deploy", `---
manual: &manual
  disable-model-invocation: true
<<: *manual
description: deploy
---
body`)
	if err != nil || !manualOnly {
		t.Fatalf("merged opt-out must not become model-visible: manual=%v err=%v", manualOnly, err)
	}

	_, _, err = parseDocument("deploy", `---
? disable-model-invocation
: true
description: deploy
---
body`)
	var policyErr *invocationPolicyError
	if err == nil || !errors.As(err, &policyErr) {
		t.Fatalf("an explicit key outside the supported subset must fail closed, got %v", err)
	}

	for name, content := range map[string]string{
		"alias key":  "---\nkey: &manual disable-model-invocation\n*manual: true\ndescription: deploy\n---\nbody",
		"tagged key": "---\n!!str \"disable-model-invocation\": true\ndescription: deploy\n---\nbody",
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := parseDocument("deploy", content)
			var policyErr *invocationPolicyError
			if err == nil || !errors.As(err, &policyErr) {
				t.Fatalf("a dynamic YAML key must fail closed, got %v", err)
			}
		})
	}

	for name, content := range map[string]string{
		"alias key":  "key: &manual allow_implicit_invocation\n*manual: false\n",
		"tagged key": "!!str \"allow_implicit_invocation\": false\n",
	} {
		t.Run("Codex "+name, func(t *testing.T) {
			_, _, err := findYAMLBoolField(content, "allow_implicit_invocation")
			if err == nil {
				t.Fatal("a dynamic Codex YAML key must fail closed")
			}
		})
	}

	_, manualOnly, err = parseDocument("docs", `---
description: "Explains disable-model-invocation: true without setting it"
---
body`)
	if err != nil || manualOnly {
		t.Fatalf("a quoted description mentioning the field is not policy: manual=%v err=%v", manualOnly, err)
	}
}

func TestInvocationOptOutWinsConflictingMetadata(t *testing.T) {
	allowed, found, err := findYAMLBoolField(
		"first:\n  allow_implicit_invocation: true\nsecond:\n  allow_implicit_invocation: false\n",
		"allow_implicit_invocation",
	)
	if err != nil || !found || allowed {
		t.Fatalf("Codex opt-out must win conflicting values: allowed=%v found=%v err=%v", allowed, found, err)
	}

	_, manualOnly, err := parseDocument("conflict",
		"---\ndescription: conflict\ndisable-model-invocation: true\ndisable-model-invocation: false\n---\nbody")
	if err != nil || !manualOnly {
		t.Fatalf("Claude opt-out must win conflicting values: manual=%v err=%v", manualOnly, err)
	}
}

func TestYAMLBooleanSpellingsUsedByNativeSkills(t *testing.T) {
	for _, value := range []string{"true", "TRUE", "yes", "on", "1"} {
		got, err := parseYAMLBool(value)
		if err != nil || !got {
			t.Errorf("parseYAMLBool(%q) = %v, %v; want true", value, got, err)
		}
	}
	for _, value := range []string{"false", "FALSE", "no", "off", "0"} {
		got, err := parseYAMLBool(value)
		if err != nil || got {
			t.Errorf("parseYAMLBool(%q) = %v, %v; want false", value, got, err)
		}
	}
}

func TestLoadRejectsOversizedDefinitions(t *testing.T) {
	setTestHome(t, t.TempDir())
	ws := t.TempDir()
	writePackedSkill(t, filepath.Join(ws, ".agents", "skills"), "huge",
		strings.Repeat("x", int(maxDefinitionBytes)+1))

	list, notes := Load(ws)
	if len(list) != 0 || !strings.Contains(strings.Join(notes, "\n"), "exceeds the") {
		t.Fatalf("oversized definition loaded: %+v, notes %v", list, notes)
	}
}

func TestToolBoundsAndTypeChecksSupportingFiles(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "huge", strings.Repeat("x", int(maxSupportingBytes)+1))
	if err := os.MkdirAll(filepath.Join(dir, "directory"), 0o755); err != nil {
		t.Fatal(err)
	}
	tool := NewTool([]Skill{{Name: "s", Description: "d", Body: "b", Dir: dir}})
	serve := func(file string) tools.Result {
		t.Helper()
		input, err := json.Marshal(skillInput{Name: "s", File: file})
		if err != nil {
			t.Fatal(err)
		}
		plan, err := tool.Plan(input)
		if err != nil {
			t.Fatal(err)
		}
		result, err := plan.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if got := serve("huge"); !got.IsError || !strings.Contains(got.Content, "exceeds the") {
		t.Fatalf("oversized supporting file was served: %+v", got)
	}
	if got := serve("directory"); !got.IsError || !strings.Contains(got.Content, "not a regular file") {
		t.Fatalf("non-regular supporting path was served: %+v", got)
	}
}

func TestLoadFollowsSafeSkillDirectorySymlinksAndDeduplicates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not generally available")
	}
	setTestHome(t, t.TempDir())
	ws := t.TempDir()
	real := filepath.Join(t.TempDir(), "real-skill")
	writeSkill(t, real, "SKILL.md", minimal)
	writeSkill(t, filepath.Join(real, "references"), "note.md", "reference")

	agents := filepath.Join(ws, ".agents", "skills")
	claude := filepath.Join(ws, ".claude", "skills")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(claude, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(agents, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(claude, "alias")); err != nil {
		t.Fatal(err)
	}

	list, notes := Load(ws)
	if len(notes) != 0 {
		t.Fatalf("unexpected notes: %v", notes)
	}
	if len(list) != 2 {
		t.Fatalf("one real target reached through different ecosystems must retain both identities: %+v", list)
	}
	codex := findSkillKey(t, list, "codex:repo:.agents/skills/linked")
	if codex.Name != "linked" || codex.Origin.Ecosystem != EcosystemCodex {
		t.Fatalf("Codex alias changed identity: %+v", codex)
	}
	plan, err := NewTool(list).Plan(json.RawMessage(`{"name":"codex:repo:.agents/skills/linked","file":"references/note.md"}`))
	if err != nil {
		t.Fatal(err)
	}
	res, err := plan.Run(context.Background())
	if err != nil || res.Content != "reference" {
		t.Fatalf("symlinked skill resource = %+v, %v", res, err)
	}

	// Discovery captures the resolved root. Retargeting the source symlink
	// after assembly must not change what the already-built tool can read.
	evil := filepath.Join(t.TempDir(), "evil-skill")
	writeSkill(t, filepath.Join(evil, "references"), "note.md", "wrong root")
	link := filepath.Join(agents, "linked")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(evil, link); err != nil {
		t.Fatal(err)
	}
	res, err = plan.Run(context.Background())
	if err != nil || res.Content != "reference" {
		t.Fatalf("retargeted skill symlink escaped the captured root: %+v, %v", res, err)
	}

	// Replacing the canonical directory itself is also detected. OpenRoot
	// anchors the new handle, then SameFile proves it is the identity that
	// supplied the skill definition before any supporting file is read.
	retired := real + "-retired"
	if err := os.Rename(real, retired); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(evil, real); err != nil {
		t.Fatal(err)
	}
	res, err = plan.Run(context.Background())
	if err != nil || !res.IsError || strings.Contains(res.Content, "wrong root") ||
		!strings.Contains(res.Content, "changed after discovery") {
		t.Fatalf("replaced canonical skill root was served: %+v, %v", res, err)
	}
}

func TestLoadRejectsDefinitionsAndMetadataThatEscapeBySymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not generally available")
	}
	setTestHome(t, t.TempDir())
	ws := t.TempDir()
	out := t.TempDir()
	outsideSkill := filepath.Join(out, "outside.md")
	writeSkill(t, out, "outside.md", minimal)

	escapeDir := filepath.Join(ws, ".agents", "skills", "escape")
	if err := os.MkdirAll(escapeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideSkill, filepath.Join(escapeDir, "SKILL.md")); err != nil {
		t.Fatal(err)
	}

	metadataDir := filepath.Join(ws, ".agents", "skills", "metadata")
	writeSkill(t, metadataDir, "SKILL.md", minimal)
	outsideMetadata := filepath.Join(out, "openai.yaml")
	writeSkill(t, out, "openai.yaml", "policy:\n  allow_implicit_invocation: true\n")
	if err := os.MkdirAll(filepath.Join(metadataDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideMetadata, filepath.Join(metadataDir, "agents", "openai.yaml")); err != nil {
		t.Fatal(err)
	}

	list, notes := Load(ws)
	if len(list) != 0 {
		t.Fatalf("escaping symlinks must not load: %+v", list)
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "leaves its skill directory") || !strings.Contains(joined, "unsafe agents/openai.yaml") {
		t.Fatalf("escaping symlinks need diagnostics:\n%s", joined)
	}
}

func TestLoadSkipsWhatItCannotUse(t *testing.T) {
	setTestHome(t, t.TempDir())
	ws := t.TempDir()
	dir := filepath.Join(ws, ".switchboard", "skills")
	writeSkill(t, dir, "nobody.md", "---\ndescription: fine\n---\n\n  \n")
	writeSkill(t, dir, "nodesc.md", "just a body")
	writeSkill(t, dir, "good.md", minimal)

	list, notes := Load(ws)
	if len(list) != 1 || list[0].Name != "good" {
		t.Fatalf("loaded %+v, want only the usable one", list)
	}
	if len(notes) != 2 {
		t.Fatalf("a skipped skill must say why, got %v", notes)
	}
}

func TestParseIgnoresForeignFrontmatterKeys(t *testing.T) {
	sk, err := parse("port", "---\nname: ported\ndescription: travels\nallowed-tools: Bash(git:*)\nmodel: something\n---\nbody")
	if err != nil {
		t.Fatal(err)
	}
	if sk.Name != "ported" || sk.Description != "travels" || sk.Body != "body" {
		t.Errorf("a pack written for a neighboring tool must load as written: %+v", sk)
	}
}

func TestParseHandlesPortableYAMLScalarForms(t *testing.T) {
	tests := []struct {
		name       string
		content    string
		wantName   string
		wantDesc   string
		wantBody   string
		manualOnly bool
	}{
		{
			name:     "quoted colon and hash",
			content:  "---\ndescription: \"keeps: # text\" # drops this comment\n---\nbody",
			wantName: "fallback", wantDesc: "keeps: # text", wantBody: "body",
		},
		{
			name:     "folded multiline with BOM and CRLF",
			content:  "\ufeff---\r\nname: native\r\ndescription: >-\r\n  first line\r\n  second line\r\n...\r\nbody\r\n",
			wantName: "native", wantDesc: "first line second line", wantBody: "body",
		},
		{
			name:     "literal multiline",
			content:  "---\ndescription: |-\n  first\n  second\n---\nbody",
			wantName: "fallback", wantDesc: "first\nsecond", wantBody: "body",
		},
		{
			name:     "explicit block indentation",
			content:  "---\ndescription: >2-\n  first\n  second\n---\nbody",
			wantName: "fallback", wantDesc: "first second", wantBody: "body",
		},
		{
			name:       "manual quoted boolean",
			content:    "---\ndescription: manual\ndisable-model-invocation: 'true'\n---\nbody",
			wantName:   "fallback",
			wantDesc:   "manual",
			wantBody:   "body",
			manualOnly: true,
		},
		{
			name:     "nested foreign fields do not override",
			content:  "---\nmetadata:\n  description: wrong\ndescription: right\nunknown: value\n---\nbody",
			wantName: "fallback", wantDesc: "right", wantBody: "body",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sk, manualOnly, err := parseDocument("fallback", tt.content)
			if err != nil {
				t.Fatal(err)
			}
			if sk.Name != tt.wantName || sk.Description != tt.wantDesc || sk.Body != tt.wantBody || manualOnly != tt.manualOnly {
				t.Errorf("parsed %+v, manual=%v", sk, manualOnly)
			}
		})
	}
}

func TestParseKeepsIndentedDocumentMarkersInsideBlocks(t *testing.T) {
	sk, err := parse("fallback", "---\ndescription: |-\n  ---\n  still description\n---\nbody")
	if err != nil {
		t.Fatal(err)
	}
	if sk.Description != "---\nstill description" {
		t.Fatalf("indented marker ended frontmatter early: %q", sk.Description)
	}
}

func TestParseRejectsMultilineNames(t *testing.T) {
	_, _, err := parseDocument("fallback", "---\nname: |-\n  first\n  second\ndescription: valid\n---\nbody")
	if err == nil || !strings.Contains(err.Error(), "invalid name") {
		t.Fatalf("multiline name must be rejected, got %v", err)
	}
}

func FuzzParseDocumentNeverPanics(f *testing.F) {
	f.Add("fallback", minimal)
	f.Add("fallback", "\ufeff---\r\ndescription: >-\r\n  folded\r\n---\r\nbody")
	f.Add("fallback", "---\ndescription: x\ndisable-model-invocation: false\n---\nbody")
	f.Fuzz(func(t *testing.T, fallback, content string) {
		_, _, _ = parseDocument(fallback, content)
	})
}

func TestToolDescriptionEnumeratesTheSet(t *testing.T) {
	tool := NewTool([]Skill{
		{Name: "a", Description: "first"},
		{Name: "b", Description: "second"},
	})
	desc := tool.Description()
	for _, want := range []string{"- a: first", "- b: second"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description missing %q:\n%s", want, desc)
		}
	}
}

func TestToolRedactsCompleteNativeDescriptionsAndBodiesWithoutMutatingInventory(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	ws := t.TempDir()
	secret := "ghp_" + strings.Repeat("a", 36)
	boundaryDescription := strings.Repeat("d", 390) + " " + secret + " after"

	writeSkill(t, filepath.Join(ws, ".switchboard", "skills"), "switchboard.md",
		"---\ndescription: switchboard "+secret+"\n---\nswitchboard body "+secret)
	writePackedSkill(t, filepath.Join(ws, ".agents", "skills"), "codex",
		"---\ndescription: "+boundaryDescription+"\n---\ncodex body "+secret)
	writePackedSkill(t, filepath.Join(ws, ".claude", "skills"), "claude",
		"---\ndescription: claude "+secret+"\n---\nclaude body "+secret)
	// Claude derives a missing description from the first body paragraph. That
	// derived value is just as provider-bound as explicit frontmatter.
	writePackedSkill(t, filepath.Join(ws, ".claude", "skills"), "derived",
		"derived "+secret+" paragraph\n\nsecond paragraph")

	list, notes := Load(ws)
	if len(notes) != 0 || len(list) != 4 {
		t.Fatalf("loaded credential fixtures = %+v, notes %v", list, notes)
	}
	type sourceValue struct {
		description string
		body        string
	}
	sources := make(map[string]sourceValue, len(list))
	for _, sk := range list {
		if !strings.Contains(sk.Description, secret) || !strings.Contains(sk.Body, secret) {
			t.Fatalf("fixture %s did not retain source credential: %+v", sk.Key(), sk)
		}
		sources[sk.Key()] = sourceValue{description: sk.Description, body: sk.Body}
	}

	tool := NewTool(list)
	description := tool.Description()
	if strings.Contains(description, secret) || strings.Count(description, "[redacted: a GitHub token]") != len(list) {
		t.Fatalf("frozen skill inventory did not visibly redact every description:\n%s", description)
	}
	for _, sk := range list {
		input, err := json.Marshal(skillInput{Name: sk.Key()})
		if err != nil {
			t.Fatal(err)
		}
		plan, err := tool.Plan(input)
		if err != nil {
			t.Fatal(err)
		}
		result, err := plan.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(result.Content, secret) || !strings.Contains(result.Content, "[redacted: a GitHub token]") {
			t.Errorf("skill %s body egress was not visibly redacted:\n%s", sk.Key(), result.Content)
		}
	}
	for _, sk := range list {
		want := sources[sk.Key()]
		if sk.Description != want.description || sk.Body != want.body {
			t.Errorf("source skill %s was mutated: got description=%q body=%q", sk.Key(), sk.Description, sk.Body)
		}
	}
}

func TestToolRedactsEveryProviderVisibleIdentityAndDiagnosticWithoutMutatingInventory(t *testing.T) {
	secret := "ghp_" + strings.Repeat("i", 36)
	parent := t.TempDir()
	dir := filepath.Join(parent, "skill-"+secret)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := "reference-" + secret + ".md"
	writeSkill(t, dir, file, "ordinary supporting content")

	source := Skill{
		Name:        "identity " + secret,
		Selector:    "codex:repo:" + strings.Repeat("p", 390) + "%20" + secret,
		Description: "description " + secret,
		Body:        "body " + secret,
		Dir:         dir,
	}
	list := []Skill{source}
	tool := NewTool(list)

	visibleSelector := redactSkillSelector(source.Selector)
	description := tool.Description()
	if strings.Contains(description, secret) || !strings.Contains(description, visibleSelector) ||
		!strings.Contains(description, "[redacted: a GitHub token]") {
		t.Fatalf("provider definition exposed or lost the redacted identity:\n%s", description)
	}

	plan, err := tool.Plan(mustSkillInput(t, skillInput{Name: visibleSelector}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plan.Request.Detail, secret) {
		t.Fatalf("permission detail exposed source identity: %q", plan.Request.Detail)
	}
	result, err := plan.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Content, secret) || strings.Count(result.Content, "[redacted: a GitHub token]") < 3 {
		t.Fatalf("body result exposed identity, directory, or body metadata:\n%s", result.Content)
	}

	filePlan, err := tool.Plan(mustSkillInput(t, skillInput{Name: visibleSelector, File: file}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(filePlan.Request.Detail, secret) || !strings.Contains(filePlan.Request.Detail, "[redacted: a GitHub token]") {
		t.Fatalf("supporting-file permission detail was not redacted: %q", filePlan.Request.Detail)
	}
	if _, err := filePlan.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	missingPlan, err := tool.Plan(mustSkillInput(t, skillInput{Name: visibleSelector, File: "missing-" + secret}))
	if err != nil {
		t.Fatal(err)
	}
	missing, err := missingPlan.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(missing.Content, secret) || !strings.Contains(missing.Content, "[redacted: a GitHub token]") {
		t.Fatalf("supporting-file error exposed input metadata: %q", missing.Content)
	}

	_, err = tool.Plan(mustSkillInput(t, skillInput{Name: "unknown-" + secret}))
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[redacted: a GitHub token]") {
		t.Fatalf("unknown-selector error was not redacted: %v", err)
	}

	if list[0].Name != source.Name || list[0].Selector != source.Selector ||
		list[0].Description != source.Description || list[0].Body != source.Body || list[0].Dir != source.Dir {
		t.Fatalf("tool construction mutated the source inventory: got %+v want %+v", list[0], source)
	}
}

func TestToolTreatsSelectorsThatCollideAfterRedactionAsAmbiguous(t *testing.T) {
	one := "ghp_" + strings.Repeat("a", 36)
	two := "ghp_" + strings.Repeat("b", 36)
	tool := NewTool([]Skill{
		{Name: "one", Selector: "codex:repo:" + one, Description: "first", Body: "first"},
		{Name: "two", Selector: "codex:repo:" + two, Description: "second", Body: "second"},
	})
	selector := "codex:repo:%5Bredacted%3A%20a%20GitHub%20token%5D"
	_, err := tool.Plan(mustSkillInput(t, skillInput{Name: selector}))
	if err == nil || !strings.Contains(err.Error(), "ambiguous across 2 definitions") ||
		strings.Contains(err.Error(), one) || strings.Contains(err.Error(), two) {
		t.Fatalf("redacted selector collision did not fail safely: %v", err)
	}
}

func TestToolReescapesRedactedSelectorComponents(t *testing.T) {
	secret := "ghp_" + strings.Repeat("z", 36)
	sourceSelector := "codex:repo:folder%0Aname%2Fpart%3Arole%20" + secret
	visibleSelector := redactSkillSelector(sourceSelector)
	if strings.Contains(visibleSelector, secret) || strings.ContainsAny(visibleSelector, "\r\n") ||
		strings.Count(visibleSelector, ":") != 2 || strings.Contains(visibleSelector, "/") {
		t.Fatalf("redacted selector lost its escaped component grammar: %q", visibleSelector)
	}
	for _, want := range []string{"%0A", "%2F", "%3A", "%5Bredacted%3A%20a%20GitHub%20token%5D"} {
		if !strings.Contains(visibleSelector, want) {
			t.Errorf("redacted selector %q is missing %q", visibleSelector, want)
		}
	}

	tool := NewTool([]Skill{{
		Name: "encoded", Selector: sourceSelector, Description: "encoded selector", Body: "body",
	}})
	if description := tool.Description(); strings.Contains(description, secret) ||
		strings.Contains(description, "\r") || strings.Count(description, "\n") != 1 ||
		!strings.Contains(description, "- "+visibleSelector+" (encoded): encoded selector") {
		t.Fatalf("tool description did not preserve the safe model-visible key:\n%s", description)
	}
	plan, err := tool.Plan(mustSkillInput(t, skillInput{Name: visibleSelector}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func mustSkillInput(t *testing.T, input skillInput) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestToolRedactsBodyAtDefinitionReadLimit(t *testing.T) {
	setTestHome(t, t.TempDir())
	ws := t.TempDir()
	secret := "ghp_" + strings.Repeat("b", 36)
	header := "---\ndescription: boundary body\n---\n"
	content := header + strings.Repeat("x", int(maxDefinitionBytes)-len(header)-len(secret)-1) + " " + secret
	if len(content) != int(maxDefinitionBytes) {
		t.Fatalf("fixture length = %d, want %d", len(content), maxDefinitionBytes)
	}
	writePackedSkill(t, filepath.Join(ws, ".agents", "skills"), "boundary", content)

	list, notes := Load(ws)
	if len(notes) != 0 || len(list) != 1 {
		t.Fatalf("boundary definition loaded %+v, notes %v", list, notes)
	}
	sourceBody := list[0].Body
	if !strings.HasSuffix(sourceBody, secret) {
		t.Fatal("boundary fixture did not retain its source token")
	}
	plan, err := NewTool(list).Plan(json.RawMessage(`{"name":"codex:repo:.agents/skills/boundary"}`))
	if err != nil {
		t.Fatal(err)
	}
	result, err := plan.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Content, secret) || !strings.Contains(result.Content, "[redacted: a GitHub token]") {
		t.Fatalf("definition-boundary credential reached the tool result: suffix %q", result.Content[len(result.Content)-128:])
	}
	if list[0].Body != sourceBody {
		t.Fatal("tool construction mutated the source body")
	}
}

func TestToolRedactsCompleteSupportingFilesAtAndAcrossReadBoundary(t *testing.T) {
	dir := t.TempDir()
	secret := "ghp_" + strings.Repeat("c", 36)
	writeSkill(t, dir, "small.md", strings.Repeat("s", 390)+" "+secret+" tail")
	exact := strings.Repeat("x", int(maxSupportingBytes)-len(secret)-1) + " " + secret
	writeSkill(t, dir, "exact.md", exact)
	// This token starts within the read allowance and ends beyond it. The
	// resource must be refused whole, never returned as a prefix.
	crossing := strings.Repeat("y", int(maxSupportingBytes)-11) + " " + secret
	writeSkill(t, dir, "crossing.md", crossing)

	tool := NewTool([]Skill{{Name: "support", Description: "supporting files", Body: "body", Dir: dir}})
	serve := func(name string) tools.Result {
		t.Helper()
		input, err := json.Marshal(skillInput{Name: "support", File: name})
		if err != nil {
			t.Fatal(err)
		}
		plan, err := tool.Plan(input)
		if err != nil {
			t.Fatal(err)
		}
		result, err := plan.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	for _, name := range []string{"small.md", "exact.md"} {
		result := serve(name)
		if result.IsError || strings.Contains(result.Content, secret) || !strings.Contains(result.Content, "[redacted: a GitHub token]") {
			t.Errorf("supporting file %s was not visibly redacted: error=%v, suffix=%q", name, result.IsError, result.Content[len(result.Content)-128:])
		}
	}
	if result := serve("crossing.md"); !result.IsError || strings.Contains(result.Content, "ghp_") || !strings.Contains(result.Content, "exceeds the") {
		t.Fatalf("over-limit crossing resource was partially served: %+v", result)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "exact.md")); err != nil || string(got) != exact {
		t.Fatalf("serving mutated source bytes: equal=%v err=%v", string(got) == exact, err)
	}
}

func TestManagedCodexSkillSelectorKeepsAdminOrigin(t *testing.T) {
	root := filepath.Join(t.TempDir(), "managed-skills")
	selector, err := canonicalSelector(source{
		dir: root, selectorRoot: root, ecosystem: EcosystemCodex, scope: ScopeManaged,
	}, filepath.Join(root, "release"))
	if err != nil {
		t.Fatal(err)
	}
	if selector != "codex:admin:release" {
		t.Fatalf("managed selector = %q", selector)
	}
}

func TestCodexDefaultPromptMetadataDoesNotBlockTheSkill(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "SKILL.md", minimal)
	writeSkill(t, filepath.Join(root, "agents"), "openai.yaml", `interface:
  display_name: "Review"
  default_prompt: "Use $review to inspect this change."
policy:
  allow_implicit_invocation: true
`)
	opened, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	allowed, blockers, err := codexInvocationMetadataFromRoot(opened)
	if err != nil {
		t.Fatal(err)
	}
	if !allowed || len(blockers) != 0 {
		t.Fatalf("UI-only metadata changed invocation: allowed=%v blockers=%v", allowed, blockers)
	}
}

func TestToolDescriptionKeepsEachSkillOnOneLine(t *testing.T) {
	tool := NewTool([]Skill{{Name: "safe", Description: "first line\n- forged: entry"}})
	desc := tool.Description()
	if strings.Contains(desc, "\n- forged") || !strings.Contains(desc, "- safe: first line - forged: entry") {
		t.Fatalf("multiline description changed the advertised skill set:\n%s", desc)
	}
}

func TestToolServesTheBodyWithItsDirectory(t *testing.T) {
	tool := NewTool([]Skill{{Name: "s", Description: "d", Body: "the body", Dir: "/somewhere/s"}})
	plan, err := tool.Plan(json.RawMessage(`{"name":"s"}`))
	if err != nil {
		t.Fatal(err)
	}
	res, err := plan.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "the body") || !strings.Contains(res.Content, "/somewhere/s") {
		t.Errorf("serving must carry the body and where it lives:\n%s", res.Content)
	}
}

func TestToolRefusesAnUnknownName(t *testing.T) {
	tool := NewTool([]Skill{{Name: "s", Description: "d", Body: "b"}})
	if _, err := tool.Plan(json.RawMessage(`{"name":"ghost"}`)); err == nil {
		t.Fatal("an unknown skill must be a malformed call the model can correct")
	}
}

func TestToolServesSupportingFilesAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "references", "style.md"), []byte("the reference"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("not yours"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := NewTool([]Skill{{Name: "s", Description: "d", Body: "b", Dir: dir}})
	serve := func(file string) string {
		t.Helper()
		in, _ := json.Marshal(skillInput{Name: "s", File: file})
		plan, err := tool.Plan(in)
		if err != nil {
			t.Fatal(err)
		}
		res, err := plan.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return res.Content
	}

	if got := serve("references/style.md"); got != "the reference" {
		t.Errorf("a named reference must be served as written, got %q", got)
	}
	if got := serve("../secret"); !strings.Contains(got, "leaves skill") && !strings.Contains(got, "does not exist") {
		t.Errorf("a traversal must be refused, got %q", got)
	}
	if got := serve(outside); !strings.Contains(got, "absolute") {
		t.Errorf("an absolute path must be refused, got %q", got)
	}
	if got := serve("references/missing.md"); !strings.Contains(got, "does not exist") {
		t.Errorf("a missing file must be named, got %q", got)
	}

	if runtime.GOOS != "windows" {
		if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
			t.Fatal(err)
		}
		if got := serve("link"); !strings.Contains(got, "leaves skill") {
			t.Errorf("a symlink out of the directory must be refused, got %q", got)
		}
	}
}
