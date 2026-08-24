package extensions

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDiscoverGolden(t *testing.T) {
	testdata, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatal(err)
	}
	candidates := []Candidate{
		{Root: filepath.Join(testdata, "claude-manifestless"), Scope: ScopeUser, Dialect: DialectClaude},
		{Root: filepath.Join(testdata, "codex-plugin"), Scope: ScopeWorkspace, Dialect: DialectCodex},
		{Root: filepath.Join(testdata, "claude-plugin"), Scope: ScopeUser, Dialect: DialectClaude},
	}

	result := Discover(candidates)
	got := snapshot(t, result, testdata)
	want, err := os.ReadFile(filepath.Join("testdata", "discovery.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	wantText := strings.ReplaceAll(string(want), "\r\n", "\n")
	if strings.TrimSpace(got) != strings.TrimSpace(wantText) {
		t.Fatalf("discovery snapshot differs (-want +got):\n--- want\n%s\n--- got\n%s", want, got)
	}

	reversed := append([]Candidate(nil), candidates...)
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	if other := Discover(reversed); !reflect.DeepEqual(result, other) {
		t.Fatalf("candidate order changed discovery:\nforward: %#v\nreverse: %#v", result, other)
	}
}

func TestDiscoverRejectsTraversal(t *testing.T) {
	root := makePlugin(t, DialectClaude, `{"name":"escape","skills":"./../outside"}`)
	result := Discover([]Candidate{{Root: root, Scope: ScopeWorkspace, Dialect: DialectClaude}})
	assertRejected(t, result, "unsafe-component", "parent traversal")
}

func TestDiscoverRejectsComponentSymlink(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "SKILL.md"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := makePlugin(t, DialectClaude, `{"name":"linked","skills":"./linked"}`)
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	result := Discover([]Candidate{{Root: root, Scope: ScopeWorkspace, Dialect: DialectClaude}})
	assertRejected(t, result, "unsafe-component", "symlink")
}

func TestDiscoverRejectsSymlinkInsideComponentTree(t *testing.T) {
	root := makePlugin(t, DialectCodex, `{"name":"nested-link","skills":"./skills"}`)
	skills := filepath.Join(root, "skills")
	if err := os.MkdirAll(skills, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(skills, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	result := Discover([]Candidate{{Root: root, Scope: ScopeWorkspace, Dialect: DialectCodex}})
	assertRejected(t, result, "invalid-component-tree", "symlink")
}

func TestDiscoverRejectsGitMetadataSymlink(t *testing.T) {
	root := makePlugin(t, DialectCodex, `{"name":"git-link"}`)
	if err := os.Symlink(t.TempDir(), filepath.Join(root, ".git")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	result := Discover([]Candidate{{Root: root, Scope: ScopeWorkspace, Dialect: DialectCodex}})
	assertRejected(t, result, "invalid-component-tree", "symlink")
}

func TestDiscoverRejectsExecutableComponentsExcludedFromGitDigest(t *testing.T) {
	tests := []struct {
		name     string
		dialect  Dialect
		manifest string
		path     string
	}{
		{
			name:     "hook",
			dialect:  DialectClaude,
			manifest: `{"name":"git-hook","hooks":"./.git/hooks.json"}`,
			path:     ".git/hooks.json",
		},
		{
			name:     "mcp",
			dialect:  DialectCodex,
			manifest: `{"name":"git-mcp","mcpServers":"./nested/.git/mcp.json"}`,
			path:     "nested/.git/mcp.json",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := makePlugin(t, test.dialect, test.manifest)
			component := filepath.Join(root, filepath.FromSlash(test.path))
			if err := os.MkdirAll(filepath.Dir(component), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(component, []byte(`{}`), 0o700); err != nil {
				t.Fatal(err)
			}

			candidate := Candidate{Root: root, Scope: ScopeUser, Dialect: test.dialect}
			assertRejected(t, Discover([]Candidate{candidate}), "unsafe-component", ".git metadata")

			// A component omitted from the digest must not become trustable before
			// or after its executable bytes change.
			if err := os.WriteFile(component, []byte(`{"changed":true}`), 0o700); err != nil {
				t.Fatal(err)
			}
			assertRejected(t, Discover([]Candidate{candidate}), "unsafe-component", ".git metadata")
		})
	}
}

func TestDiscoverKeepsInlineExecutableCapability(t *testing.T) {
	root := makePlugin(t, DialectClaude, `{"name":"inline","mcpServers":{"local":{"command":"never-run"}}}`)
	result := Discover([]Candidate{{Root: root, Scope: ScopeUser, Dialect: DialectClaude}})
	if len(result.Diagnostics) != 0 || len(result.Plugins) != 1 {
		t.Fatalf("inline plugin discovery: %#v", result)
	}
	plugin := result.Plugins[0]
	if !plugin.Executable || len(plugin.Components) != 1 || !plugin.Components[0].Inline || !plugin.Components[0].Executable {
		t.Fatalf("inline executable semantics were dropped: %#v", plugin)
	}
	assertWarning(t, plugin.Warnings, "inline-component-requires-adapter")
}

func TestDiscoverClaudeRootSkill(t *testing.T) {
	root := filepath.Join(t.TempDir(), "single-skill")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("# Native single skill"), 0o600); err != nil {
		t.Fatal(err)
	}

	result := Discover([]Candidate{{Root: root, Scope: ScopeUser, Dialect: DialectClaude}})
	if len(result.Diagnostics) != 0 || len(result.Plugins) != 1 {
		t.Fatalf("root skill discovery: %#v", result)
	}
	plugin := result.Plugins[0]
	if plugin.Executable || len(plugin.Components) != 1 {
		t.Fatalf("root skill classified incorrectly: %#v", plugin)
	}
	component := plugin.Components[0]
	if component.Kind != ComponentSkill || component.DeclaredPath != "./" || component.Path != root {
		t.Fatalf("root skill path was not normalized: %#v", component)
	}
}

func TestDiscoverClaudeRootSkillYieldsToSkillsDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills-win")
	if err := os.MkdirAll(filepath.Join(root, "skills"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("# Ignored native fallback"), 0o600); err != nil {
		t.Fatal(err)
	}

	result := Discover([]Candidate{{Root: root, Scope: ScopeUser, Dialect: DialectClaude}})
	if len(result.Diagnostics) != 0 || len(result.Plugins) != 1 {
		t.Fatalf("skills directory discovery: %#v", result)
	}
	components := result.Plugins[0].Components
	if len(components) != 1 || components[0].DeclaredPath != "./skills" {
		t.Fatalf("root SKILL.md should yield to skills/: %#v", components)
	}

	declared := makePlugin(t, DialectClaude, `{"name":"declared-skills","skills":"./custom"}`)
	if err := os.Mkdir(filepath.Join(declared, "custom"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(declared, "SKILL.md"), []byte("# Ignored native fallback"), 0o600); err != nil {
		t.Fatal(err)
	}
	result = Discover([]Candidate{{Root: declared, Scope: ScopeUser, Dialect: DialectClaude}})
	if len(result.Diagnostics) != 0 || len(result.Plugins) != 1 {
		t.Fatalf("declared skills discovery: %#v", result)
	}
	components = result.Plugins[0].Components
	if len(components) != 1 || components[0].DeclaredPath != "./custom" {
		t.Fatalf("root SKILL.md should yield to a skills manifest field: %#v", components)
	}
}

func TestDiscoverMarksUnsupportedClaudeExecutables(t *testing.T) {
	root := filepath.Join(t.TempDir(), "monitor-only")
	monitorDirectory := filepath.Join(root, "monitors")
	if err := os.MkdirAll(monitorDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(monitorDirectory, "monitors.json"), []byte(`{"monitors":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	result := Discover([]Candidate{{Root: root, Scope: ScopeUser, Dialect: DialectClaude}})
	if len(result.Diagnostics) != 0 || len(result.Plugins) != 1 {
		t.Fatalf("monitor-only discovery: %#v", result)
	}
	if !result.Plugins[0].Executable {
		t.Fatalf("monitor capability was classified non-executable: %#v", result.Plugins[0])
	}
	assertWarning(t, result.Plugins[0].Warnings, "unsupported-default-component")
}

func TestDiscoverKeepsClaudeThemeAndSettingsPluginsVisible(t *testing.T) {
	root := filepath.Join(t.TempDir(), "display-only")
	if err := os.MkdirAll(filepath.Join(root, "themes"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "settings.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	result := Discover([]Candidate{{Root: root, Scope: ScopeUser, Dialect: DialectClaude}})
	if len(result.Diagnostics) != 0 || len(result.Plugins) != 1 {
		t.Fatalf("display-only discovery: %#v", result)
	}
	plugin := result.Plugins[0]
	if plugin.Executable || len(plugin.Components) != 0 {
		t.Fatalf("display defaults should stay unsupported and non-executable: %#v", plugin)
	}
	if len(plugin.Warnings) != 3 { // manifestless plus themes and settings
		t.Fatalf("display defaults were silently dropped: %#v", plugin.Warnings)
	}
}

func TestDiscoverClaudeMetadataDoesNotImplyExecution(t *testing.T) {
	root := makePlugin(t, DialectClaude, `{
		"$schema":"https://example.invalid/schema.json",
		"name":"metadata-only",
		"displayName":"Metadata only",
		"metadata":{"fixture":true},
		"defaultEnabled":true
	}`)
	result := Discover([]Candidate{{Root: root, Scope: ScopeUser, Dialect: DialectClaude}})
	if len(result.Diagnostics) != 0 || len(result.Plugins) != 1 {
		t.Fatalf("metadata-only discovery: %#v", result)
	}
	plugin := result.Plugins[0]
	if plugin.Executable || len(plugin.Warnings) != 0 {
		t.Fatalf("native metadata was treated as executable semantics: %#v", plugin)
	}
}

func TestDiscoverUnconstrainedCandidateInspectsBothDialects(t *testing.T) {
	root := makePlugin(t, DialectCodex, `{"name":"codex-side"}`)
	claudeDirectory := filepath.Join(root, ".claude-plugin")
	if err := os.MkdirAll(claudeDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeDirectory, "plugin.json"), []byte(`{"name":"claude-side"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	result := Discover([]Candidate{{Root: root, Scope: ScopeLocal}})
	if len(result.Diagnostics) != 0 || len(result.Plugins) != 2 {
		t.Fatalf("unconstrained dialect discovery: %#v", result)
	}
	if result.Plugins[0].ID != "claude:claude-side" || result.Plugins[1].ID != "codex:codex-side" {
		t.Fatalf("unexpected dialect IDs: %#v", result.Plugins)
	}
}

func TestDiscoverReportsDuplicateRootAndIDDeterministically(t *testing.T) {
	first := makePlugin(t, DialectCodex, `{"name":"same"}`)
	second := makePlugin(t, DialectCodex, `{"name":"same"}`)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(first, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	candidates := []Candidate{
		{Root: second, Scope: ScopeUser, Dialect: DialectCodex},
		{Root: alias, Scope: ScopeWorkspace, Dialect: DialectCodex},
		{Root: first, Scope: ScopeWorkspace, Dialect: DialectCodex},
	}
	result := Discover(candidates)
	if len(result.Plugins) != 2 {
		t.Fatalf("got %d plugins, want two physical roots: %#v", len(result.Plugins), result)
	}
	assertDiagnostic(t, result.Diagnostics, "duplicate-root")
	assertDiagnostic(t, result.Diagnostics, "duplicate-id")

	reversed := []Candidate{candidates[2], candidates[1], candidates[0]}
	if other := Discover(reversed); !reflect.DeepEqual(result, other) {
		t.Fatalf("duplicate diagnostics depend on input order:\nfirst: %#v\nsecond: %#v", result, other)
	}
}

func TestDiscoverDigestChangesWithComponentContent(t *testing.T) {
	root := makePlugin(t, DialectCodex, `{"name":"digest","skills":"./skills"}`)
	if err := os.MkdirAll(filepath.Join(root, "skills", "one"), 0o700); err != nil {
		t.Fatal(err)
	}
	skill := filepath.Join(root, "skills", "one", "SKILL.md")
	if err := os.WriteFile(skill, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := Discover([]Candidate{{Root: root, Scope: ScopeLocal, Dialect: DialectCodex}})
	if len(first.Plugins) != 1 {
		t.Fatalf("first digest discovery: %#v", first)
	}
	if err := os.WriteFile(skill, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := Discover([]Candidate{{Root: root, Scope: ScopeLocal, Dialect: DialectCodex}})
	if len(second.Plugins) != 1 {
		t.Fatalf("second digest discovery: %#v", second)
	}
	if first.Plugins[0].Digest == second.Plugins[0].Digest {
		t.Fatal("component content changed without changing plugin digest")
	}
}

func TestDiscoverDigestIncludesEmptyDirectories(t *testing.T) {
	root := makePlugin(t, DialectCodex, `{"name":"empty-directory"}`)
	first := Discover([]Candidate{{Root: root, Scope: ScopeLocal, Dialect: DialectCodex}})
	if len(first.Plugins) != 1 {
		t.Fatalf("first digest discovery: %#v", first)
	}
	if err := os.Mkdir(filepath.Join(root, "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	second := Discover([]Candidate{{Root: root, Scope: ScopeLocal, Dialect: DialectCodex}})
	if len(second.Plugins) != 1 {
		t.Fatalf("second digest discovery: %#v", second)
	}
	if first.Plugins[0].Digest == second.Plugins[0].Digest {
		t.Fatal("empty directory changed without changing plugin digest")
	}
}

func TestDiscoverBoundsWholeTreeDigest(t *testing.T) {
	root := makePlugin(t, DialectCodex, `{"name":"bounded"}`)
	large, err := os.Create(filepath.Join(root, "oversized.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := large.Truncate(maxDigestBytes + 1); err != nil {
		large.Close()
		t.Fatal(err)
	}
	if err := large.Close(); err != nil {
		t.Fatal(err)
	}

	result := Discover([]Candidate{{Root: root, Scope: ScopeLocal, Dialect: DialectCodex}})
	assertRejected(t, result, "invalid-component-tree", "digest limit")
}

func TestDiscoverRejectsDuplicateManifestKeys(t *testing.T) {
	root := makePlugin(t, DialectCodex, `{"name":"first","name":"second"}`)
	result := Discover([]Candidate{{Root: root, Scope: ScopeUser, Dialect: DialectCodex}})
	assertRejected(t, result, "invalid-manifest", "duplicate key")
}

func TestDiscoverValidatesClaudeNativeNameAndPath(t *testing.T) {
	badName := makePlugin(t, DialectClaude, `{"name":"Not Native"}`)
	result := Discover([]Candidate{{Root: badName, Scope: ScopeUser, Dialect: DialectClaude}})
	assertRejected(t, result, "invalid-name", "kebab-case")

	barePath := makePlugin(t, DialectClaude, `{"name":"native-name","skills":"skills"}`)
	if err := os.Mkdir(filepath.Join(barePath, "skills"), 0o700); err != nil {
		t.Fatal(err)
	}
	result = Discover([]Candidate{{Root: barePath, Scope: ScopeUser, Dialect: DialectClaude}})
	assertRejected(t, result, "unsafe-component", "must begin with ./")

	codexRoot := makePlugin(t, DialectCodex, `{"name":"codex-root","skills":"."}`)
	result = Discover([]Candidate{{Root: codexRoot, Scope: ScopeUser, Dialect: DialectCodex}})
	assertRejected(t, result, "unsafe-component", "must begin with ./")
}

func TestDiscoverRequiresExplicitScope(t *testing.T) {
	root := makePlugin(t, DialectCodex, `{"name":"scope"}`)
	result := Discover([]Candidate{{Root: root}})
	assertRejected(t, result, "scope-required", "does not guess precedence")
}

func makePlugin(t *testing.T, dialect Dialect, manifest string) string {
	t.Helper()
	root := t.TempDir()
	directory := ".codex-plugin"
	if dialect == DialectClaude {
		directory = ".claude-plugin"
	}
	manifestDirectory := filepath.Join(root, directory)
	if err := os.MkdirAll(manifestDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestDirectory, "plugin.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func snapshot(t *testing.T, result Result, testdata string) string {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var stable Result
	if err := json.Unmarshal(raw, &stable); err != nil {
		t.Fatal(err)
	}
	for i := range stable.Plugins {
		plugin := &stable.Plugins[i]
		if digest, err := hex.DecodeString(plugin.Digest); err != nil || len(digest) != 32 {
			t.Fatalf("invalid SHA-256 digest %q", plugin.Digest)
		}
		plugin.Digest = "$SHA256"
		plugin.Root = snapshotPath(testdata, plugin.Root)
		plugin.RealPath = snapshotPath(testdata, plugin.RealPath)
		plugin.Manifest = snapshotPath(testdata, plugin.Manifest)
		for j := range plugin.Components {
			plugin.Components[j].Path = snapshotPath(testdata, plugin.Components[j].Path)
			plugin.Components[j].RealPath = snapshotPath(testdata, plugin.Components[j].RealPath)
		}
		for j := range plugin.Warnings {
			plugin.Warnings[j].Path = snapshotPath(testdata, plugin.Warnings[j].Path)
			plugin.Warnings[j].Message = strings.ReplaceAll(plugin.Warnings[j].Message, filepath.Clean(testdata), "$TESTDATA")
		}
	}
	for i := range stable.Diagnostics {
		stable.Diagnostics[i].Path = snapshotPath(testdata, stable.Diagnostics[i].Path)
		stable.Diagnostics[i].Message = strings.ReplaceAll(stable.Diagnostics[i].Message, filepath.Clean(testdata), "$TESTDATA")
	}
	raw, err = json.MarshalIndent(stable, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func snapshotPath(root, value string) string {
	if value == "" || !filepath.IsAbs(value) {
		return filepath.ToSlash(value)
	}
	relative, err := filepath.Rel(root, value)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(value)
	}
	if relative == "." {
		return "$TESTDATA"
	}
	return "$TESTDATA/" + filepath.ToSlash(relative)
}

func assertRejected(t *testing.T, result Result, code, text string) {
	t.Helper()
	if len(result.Plugins) != 0 {
		t.Fatalf("unsafe plugin was accepted: %#v", result.Plugins)
	}
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.Code == code && strings.Contains(diagnostic.Message, text) {
			return
		}
	}
	t.Fatalf("missing %s diagnostic containing %q: %#v", code, text, result.Diagnostics)
}

func assertDiagnostic(t *testing.T, diagnostics []Diagnostic, code string) {
	t.Helper()
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return
		}
	}
	t.Fatalf("missing diagnostic %q: %#v", code, diagnostics)
}

func assertWarning(t *testing.T, warnings []Warning, code string) {
	t.Helper()
	for _, warning := range warnings {
		if warning.Code == code {
			return
		}
	}
	t.Fatalf("missing warning %q: %#v", code, warnings)
}

func FuzzDecodeManifest(f *testing.F) {
	for _, seed := range []string{
		`{"name":"fixture"}`,
		`{"name":"fixture","skills":["./skills"]}`,
		`{"name":"a","name":"b"}`,
		`not-json`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		_, _ = decodeManifest(raw)
	})
}

func FuzzSafeRelativePath(f *testing.F) {
	for _, seed := range []string{"./skills", "../escape", "/absolute", "C:/absolute", "a/../../b", "hooks/hooks.json"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, declared string) {
		_, _ = safeRelativePath(declared)
	})
}
