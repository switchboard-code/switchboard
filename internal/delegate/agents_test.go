package delegate

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var suite = []string{"edit", "exec", "glob", "grep", "read", "todo", "write"}

func writeAgent(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAgentsParsesTheFourFields(t *testing.T) {
	workspace := t.TempDir()
	isolateTestHome(t, t.TempDir())
	writeAgent(t, filepath.Join(workspace, ".switchboard", "agents"), "reviewer.md",
		"---\ndescription: reviews a diff\ntier: t2\ntools: read, grep, glob\n---\n\nYou review changes.\n")

	agents, notes := LoadAgents(workspace, suite)
	if len(notes) != 0 {
		t.Fatalf("notes = %v, want none", notes)
	}
	if len(agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(agents))
	}
	ag := agents[0]
	if ag.Name != "reviewer" {
		t.Errorf("Name = %q, want the filename", ag.Name)
	}
	if ag.Description != "reviews a diff" || ag.Tier != "t2" {
		t.Errorf("Description/Tier = %q/%q", ag.Description, ag.Tier)
	}
	if strings.Join(ag.Tools, ",") != "read,grep,glob" {
		t.Errorf("Tools = %v", ag.Tools)
	}
	if !ag.ToolsSet {
		t.Error("ToolsSet = false, want the explicit grant recorded")
	}
	if ag.Prompt != "You review changes." {
		t.Errorf("Prompt = %q", ag.Prompt)
	}
	if ag.FromHome {
		t.Error("a project file must not claim home provenance")
	}
}

func TestLoadAgentsProjectWinsAndOutputIsSorted(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	isolateTestHome(t, home)
	writeAgent(t, filepath.Join(workspace, ".switchboard", "agents"), "zeta.md", "project zeta\n")
	writeAgent(t, filepath.Join(home, ".switchboard", "agents"), "zeta.md", "home zeta\n")
	writeAgent(t, filepath.Join(home, ".switchboard", "agents"), "alpha.md", "home alpha\n")

	agents, _ := LoadAgents(workspace, suite)
	if len(agents) != 2 {
		t.Fatalf("agents = %d, want 2", len(agents))
	}
	if agents[0].Name != "alpha" || agents[1].Name != "zeta" {
		t.Errorf("order = %s, %s: the schema needs a stable order", agents[0].Name, agents[1].Name)
	}
	if agents[1].Prompt != "project zeta" {
		t.Errorf("Prompt = %q, want the project's version to win the clash", agents[1].Prompt)
	}
	if !agents[0].FromHome || agents[1].FromHome {
		t.Error("provenance must record which directory spoke")
	}
}

func TestLoadAgentsRejectsAGrantOutsideTheSuite(t *testing.T) {
	workspace := t.TempDir()
	isolateTestHome(t, t.TempDir())
	writeAgent(t, filepath.Join(workspace, ".switchboard", "agents"), "bad.md",
		"---\ntools: read, telepathy\n---\nbody\n")

	agents, notes := LoadAgents(workspace, suite)
	if len(agents) != 0 {
		t.Fatalf("agents = %v, want the bad grant skipped", agents)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], `"telepathy"`) {
		t.Errorf("notes = %v, want the unknown tool named", notes)
	}
}

// A compatible definition from the neighboring tool is read from
// .claude/agents, including YAML's block-list spelling. The one capability
// both suites have under different names is translated rather than refused.
func TestLoadAgentsReadsNativeDefinitionWithMultilineTools(t *testing.T) {
	workspace := t.TempDir()
	isolateTestHome(t, t.TempDir())
	writeAgent(t, filepath.Join(workspace, ".claude", "agents"), "reviewer.md",
		"---\nname: reviewer\ndescription: reviews a diff\ntools:\n  - Read\n  - Grep\n  - Bash\n---\nReview the diff.\n")

	agents, notes := LoadAgents(workspace, suite)
	if len(agents) != 1 {
		t.Fatalf("agents = %v, notes = %v; want the native definition loaded", agents, notes)
	}
	got := agents[0]
	if got.Name != "reviewer" || got.Description != "reviews a diff" || got.Prompt != "Review the diff." {
		t.Fatalf("native definition loaded as %+v", got)
	}
	// Bash and exec are the same capability under two names, so the grant is
	// applied rather than the agent refused. An unmappable name would still
	// fail, because a tools list is a restriction.
	want := []string{"read", "grep", "exec"}
	if len(got.Tools) != len(want) {
		t.Fatalf("tools = %v, want %v", got.Tools, want)
	}
	for i, name := range want {
		if got.Tools[i] != name {
			t.Fatalf("tools = %v, want %v", got.Tools, want)
		}
	}
	if !got.ToolsSet {
		t.Error("ToolsSet = false, want the block-list grant recorded")
	}
	if len(notes) != 0 {
		t.Errorf("a native definition should load without complaint: %v", notes)
	}
}

func TestNativeToolsOmittedInheritsButExplicitEmptyIsRefused(t *testing.T) {
	valid := map[string]bool{"read": true}
	omitted, err := parseClaudeAgent("scout.md", "---\nname: scout\ndescription: finds code\n---\nbody\n", valid)
	if err != nil {
		t.Fatalf("omitted tools: %v", err)
	}
	if omitted.ToolsSet || omitted.Tools != nil {
		t.Fatalf("omitted tools = %#v, ToolsSet = %v; want inherited suite", omitted.Tools, omitted.ToolsSet)
	}

	for name, field := range map[string]string{
		"flow list":    "tools: []\n",
		"empty value":  "tools:\n",
		"empty string": `tools: ""` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseClaudeAgent("scout.md",
				"---\nname: scout\ndescription: finds code\n"+field+"---\nbody\n", valid)
			if err == nil || !strings.Contains(err.Error(), "explicitly grants zero tools") {
				t.Fatalf("error = %v, want an explicit zero-tool refusal", err)
			}
		})
	}
}

func TestNativeUnsupportedBehaviorFieldsFailClosed(t *testing.T) {
	valid := map[string]bool{"read": true}
	tests := map[string]string{
		"background":       "true",
		"color":            "blue",
		"disallowedTools":  "Write, Edit",
		"effort":           "high",
		"hooks":            "{}",
		"initialPrompt":    "begin",
		"isolation":        "worktree",
		"maxTurns":         "4",
		"mcpServers":       "[]",
		"memory":           "project",
		"model":            "opus",
		"permissionMode":   "plan",
		"skills":           "[]",
		"futureSafetyMode": "strict",
	}
	for field, value := range tests {
		t.Run(field, func(t *testing.T) {
			content := "---\nname: scout\ndescription: finds code\n" + field + ": " + value + "\n---\nbody\n"
			_, err := parseClaudeAgent("scout.md", content, valid)
			if err == nil || !strings.Contains(err.Error(), field) ||
				!strings.Contains(err.Error(), "cannot preserve") {
				t.Fatalf("error = %v, want %q named in a fail-closed diagnostic", err, field)
			}
		})
	}
}

func TestNativeUnsupportedFieldDiagnosticIsSorted(t *testing.T) {
	_, err := parseClaudeAgent("scout.md", `---
name: scout
description: finds code
permissionMode: plan
model: opus
disallowedTools: Write
---
body
`, map[string]bool{"read": true})
	if err == nil {
		t.Fatal("unsupported fields loaded")
	}
	want := "disallowedTools, model, permissionMode"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want deterministic field order %q", err, want)
	}
}

func TestSwitchboardUnknownFieldFailsClosedAndReservesItsName(t *testing.T) {
	valid := map[string]bool{"read": true}
	_, err := parseAgent("reviewer.md", "---\ntool: read\n---\nbody\n", valid)
	if err == nil || !strings.Contains(err.Error(), "unrecognized Switchboard field") ||
		!strings.Contains(err.Error(), "tool") {
		t.Fatalf("error = %v, want the misspelled grant refused", err)
	}

	workspace := t.TempDir()
	home := t.TempDir()
	isolateTestHome(t, home)
	writeAgent(t, filepath.Join(workspace, ".switchboard", "agents"), "reviewer.md",
		"---\ntool: read\n---\nproject body\n")
	writeAgent(t, filepath.Join(home, ".switchboard", "agents"), "reviewer.md", "user body\n")

	agents, notes := LoadAgents(workspace, suite)
	if len(agents) != 0 {
		t.Fatalf("agents = %v, want the malformed workspace definition to block user fallback", agents)
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "unrecognized Switchboard field") ||
		!strings.Contains(joined, "reserved by higher-precedence definition") {
		t.Fatalf("notes = %v, want the typo and tombstone visible", notes)
	}
}

func TestLoadAgentsReportsUnsupportedNativeFieldAndSkipsDefinition(t *testing.T) {
	workspace := t.TempDir()
	isolateTestHome(t, t.TempDir())
	writeAgent(t, filepath.Join(workspace, ".claude", "agents"), "reviewer.md",
		"---\nname: reviewer\ndescription: reviews a diff\npermissionMode: plan\n---\nReview the diff.\n")

	agents, notes := LoadAgents(workspace, suite)
	if len(agents) != 0 {
		t.Fatalf("agents = %v, want unsupported native definition skipped", agents)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "permissionMode") ||
		!strings.Contains(notes[0], "cannot preserve") {
		t.Fatalf("notes = %v, want the unsupported field surfaced", notes)
	}
}

func TestNativeDefinitionRequiresNativeIdentityFields(t *testing.T) {
	valid := map[string]bool{"read": true}
	for name, content := range map[string]string{
		"frontmatter": "body\n",
		"name":        "---\ndescription: finds code\n---\nbody\n",
		"description": "---\nname: scout\n---\nbody\n",
		"valid-name":  "---\nname: Bad_Name\ndescription: finds code\n---\nbody\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseClaudeAgent("scout.md", content, valid); err == nil {
				t.Fatal("invalid native identity loaded")
			}
		})
	}
}

func TestSameScopeDuplicateAgentNamesAreRejected(t *testing.T) {
	workspace := t.TempDir()
	isolateTestHome(t, t.TempDir())
	writeAgent(t, filepath.Join(workspace, ".switchboard", "agents"), "scout.md",
		"---\nname: scout\n---\nmine\n")
	writeAgent(t, filepath.Join(workspace, ".claude", "agents"), "scout.md",
		"---\nname: scout\ndescription: native scout\n---\ntheirs\n")

	agents, notes := LoadAgents(workspace, suite)
	if len(agents) != 0 {
		t.Fatalf("agents = %v, want the ambiguous identity rejected", agents)
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, `name "scout" is ambiguous`) ||
		!strings.Contains(joined, filepath.Join(".switchboard", "agents", "scout.md")) ||
		!strings.Contains(joined, filepath.Join(".claude", "agents", "scout.md")) {
		t.Fatalf("notes = %v, want both conflicting definitions named", notes)
	}
}

func TestLoadAgentsRequiresABody(t *testing.T) {
	workspace := t.TempDir()
	isolateTestHome(t, t.TempDir())
	writeAgent(t, filepath.Join(workspace, ".switchboard", "agents"), "empty.md",
		"---\ndescription: nothing follows\n---\n\n")

	agents, notes := LoadAgents(workspace, suite)
	if len(agents) != 0 || len(notes) != 1 {
		t.Fatalf("agents = %v, notes = %v: a bodyless agent has no instructions", agents, notes)
	}
}

func TestParseAgentAcceptsBothListShapesAndFrontmatterName(t *testing.T) {
	valid := map[string]bool{"read": true, "grep": true}
	ag, err := parseAgent("file.md", "---\nname: scout\ntools: [Read, GREP]\n---\nbody\n", valid)
	if err != nil {
		t.Fatal(err)
	}
	if ag.Name != "scout" {
		t.Errorf("Name = %q, want the frontmatter to override the filename", ag.Name)
	}
	if strings.Join(ag.Tools, ",") != "read,grep" {
		t.Errorf("Tools = %v, want bracketed, cased input normalized", ag.Tools)
	}
	if !ag.ToolsSet {
		t.Error("ToolsSet = false, want explicit list presence retained")
	}
}

func TestParseAgentRejectsDuplicateFieldsAndAcceptsCRLFBOM(t *testing.T) {
	valid := map[string]bool{"read": true}
	if _, err := parseAgent("file.md", "---\nname: first\nname: second\n---\nbody\n", valid); err == nil ||
		!strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("duplicate error = %v", err)
	}

	ag, err := parseClaudeAgent("file.md",
		"\ufeff---\r\nname: scout\r\ndescription: 'finds # code' # comment\r\ntools:\r\n  - Read\r\n---\r\nbody\r\n", valid)
	if err != nil {
		t.Fatal(err)
	}
	if ag.Description != "finds # code" || strings.Join(ag.Tools, ",") != "read" {
		t.Fatalf("CRLF/BOM definition = %+v", ag)
	}
}

func TestMalformedWorkspaceAgentReservesNameAgainstUserFallback(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	isolateTestHome(t, home)
	writeAgent(t, filepath.Join(workspace, ".claude", "agents"), "reviewer.md",
		"---\nname: reviewer\ndescription: project reviewer\npermissionMode: plan\n---\nproject\n")
	writeAgent(t, filepath.Join(home, ".claude", "agents"), "reviewer.md",
		"---\nname: reviewer\ndescription: user reviewer\n---\nuser\n")

	agents, notes := LoadAgents(workspace, suite)
	if len(agents) != 0 {
		t.Fatalf("agents = %v, want the rejected workspace identity to block user fallback", agents)
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "permissionMode") ||
		!strings.Contains(joined, "reserved by higher-precedence definition") {
		t.Fatalf("notes = %v, want both the rejection and blocked fallback visible", notes)
	}
}

func TestAgentDiscoveryRejectsSymlinksAndOversizedDefinitions(t *testing.T) {
	t.Run("definition symlink", func(t *testing.T) {
		workspace := t.TempDir()
		isolateTestHome(t, t.TempDir())
		external := filepath.Join(t.TempDir(), "outside")
		if err := os.WriteFile(external, []byte("outside instructions\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(workspace, ".switchboard", "agents")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, filepath.Join(dir, "reviewer.md")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}

		agents, notes := LoadAgents(workspace, suite)
		if len(agents) != 0 || !strings.Contains(strings.Join(notes, "\n"), "symbolic-link") {
			t.Fatalf("agents = %v, notes = %v; want the symlink refused", agents, notes)
		}
	})

	t.Run("root escape", func(t *testing.T) {
		workspace := t.TempDir()
		isolateTestHome(t, t.TempDir())
		external := t.TempDir()
		writeAgent(t, external, "reviewer.md", "outside instructions\n")
		if err := os.MkdirAll(filepath.Join(workspace, ".switchboard"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, filepath.Join(workspace, ".switchboard", "agents")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}

		agents, notes := LoadAgents(workspace, suite)
		if len(agents) != 0 || len(notes) == 0 {
			t.Fatalf("agents = %v, notes = %v; want an out-of-root directory refused", agents, notes)
		}
	})

	t.Run("recursive directory symlink", func(t *testing.T) {
		workspace := t.TempDir()
		home := t.TempDir()
		isolateTestHome(t, home)
		external := t.TempDir()
		writeAgent(t, external, "reviewer.md",
			"---\nname: reviewer\ndescription: outside\n---\noutside\n")
		dir := filepath.Join(workspace, ".claude", "agents")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, filepath.Join(dir, "nested")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		writeAgent(t, filepath.Join(home, ".claude", "agents"), "reviewer.md",
			"---\nname: reviewer\ndescription: user\n---\nuser\n")

		agents, notes := LoadAgents(workspace, suite)
		if len(agents) != 0 || !strings.Contains(strings.Join(notes, "\n"), "complete recursive discovery") {
			t.Fatalf("agents = %v, notes = %v; want incomplete recursive discovery to fail closed", agents, notes)
		}
	})

	t.Run("oversized file", func(t *testing.T) {
		workspace := t.TempDir()
		isolateTestHome(t, t.TempDir())
		writeAgent(t, filepath.Join(workspace, ".switchboard", "agents"), "huge.md",
			strings.Repeat("x", int(maxAgentDefinitionBytes)+1))

		agents, notes := LoadAgents(workspace, suite)
		if len(agents) != 0 || !strings.Contains(strings.Join(notes, "\n"), "byte limit") {
			t.Fatalf("agents = %v, notes = %v; want the byte cap enforced", agents, notes)
		}
	})

	t.Run("aggregate bytes", func(t *testing.T) {
		workspace := t.TempDir()
		isolateTestHome(t, t.TempDir())
		dir := filepath.Join(workspace, ".switchboard", "agents")
		body := strings.Repeat("x", int(maxAgentDefinitionBytes))
		for i := 0; i < int(maxAgentAggregateBytes/maxAgentDefinitionBytes)+1; i++ {
			writeAgent(t, dir, fmt.Sprintf("agent-%02d.md", i), body)
		}

		agents, notes := LoadAgents(workspace, suite)
		if len(agents) != 0 || !strings.Contains(strings.Join(notes, "\n"), "aggregate limit") {
			t.Fatalf("agents = %v, notes = %v; want the aggregate cap enforced", agents, notes)
		}
	})
}

func TestAgentDiscoveryBoundsDefinitionCount(t *testing.T) {
	workspace := t.TempDir()
	isolateTestHome(t, t.TempDir())
	dir := filepath.Join(workspace, ".switchboard", "agents")
	for i := 0; i <= maxAgentDefinitions; i++ {
		writeAgent(t, dir, fmt.Sprintf("agent-%03d.md", i), "body\n")
	}
	agents, notes := LoadAgents(workspace, suite)
	if len(agents) != 0 || !strings.Contains(strings.Join(notes, "\n"), "definition limit") {
		t.Fatalf("agents = %d, notes = %v; want bounded all-or-nothing discovery", len(agents), notes)
	}
}

func TestClaudeAgentDiscoveryRecursesAndPreservesOrigin(t *testing.T) {
	workspace := t.TempDir()
	isolateTestHome(t, t.TempDir())
	path := filepath.Join(workspace, ".claude", "agents", "review", "security.md")
	writeAgent(t, filepath.Dir(path), filepath.Base(path),
		"---\nname: security\ndescription: reviews security\ntools: [Read]\n---\nReview it.\n")

	agents, notes := LoadAgents(workspace, suite)
	if len(notes) != 0 || len(agents) != 1 {
		t.Fatalf("agents = %v, notes = %v", agents, notes)
	}
	origin := agents[0].Origin
	if origin.Dialect != AgentDialectClaude || origin.Scope != AgentScopeWorkspace ||
		origin.LogicalPath != path || origin.Path == "" {
		t.Fatalf("origin = %+v, want the recursive native source and both paths", origin)
	}
	if info, err := os.Stat(origin.Path); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("resolved origin %q is not the definition: %v", origin.Path, err)
	}
	label := agents[0].SourceLabel(workspace)
	if !strings.Contains(label, "claude") ||
		!strings.Contains(label, filepath.ToSlash(filepath.Join(".claude", "agents", "review", "security.md"))) {
		t.Fatalf("source label = %q, want dialect and workspace-relative path", label)
	}
}

func TestAgentYAMLSubsetRejectsInvalidYAMLInsteadOfReinterpretingIt(t *testing.T) {
	valid := map[string]bool{"read": true, "grep": true}
	tests := map[string]string{
		"Go octal escape": `---
name: "\162eviewer"
description: reviews code
---
body
`,
		"mapping in plain scalar": `---
name: reviewer
description: review: now
---
body
`,
		"malformed single quote": `---
name: reviewer
description: 'review'now'
---
body
`,
		"one list scalar is not two grants": `---
name: reviewer
description: reviews code
tools: ["Read, Grep"]
---
body
`,
		"escaped control character": `---
name: "review\eer"
description: reviews code
---
body
`,
		"boolean is not a string": `---
name: reviewer
description: true
---
body
`,
		"number is not a string": `---
name: reviewer
description: 42
---
body
`,
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseClaudeAgent("reviewer.md", content, valid); err == nil {
				t.Fatal("invalid or authority-ambiguous YAML loaded")
			}
		})
	}
}

func TestAgentDiagnosticsCannotCarryTerminalControls(t *testing.T) {
	t.Run("sanitizer", func(t *testing.T) {
		got := sanitizeDefinitionDiagnostic("bad\x1b.md")
		if strings.ContainsRune(got, '\x1b') || !strings.ContainsRune(got, '\ufffd') {
			t.Fatalf("unsafe diagnostic = %q", got)
		}
	})

	t.Run("filesystem discovery", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Win32 paths cannot contain escape characters")
		}
		workspace := t.TempDir()
		isolateTestHome(t, t.TempDir())
		writeAgent(t, filepath.Join(workspace, ".switchboard", "agents"), "bad\x1b.md", "body\n")

		_, notes := LoadAgents(workspace, suite)
		joined := strings.Join(notes, "\n")
		if strings.ContainsRune(joined, '\x1b') || !strings.ContainsRune(joined, '\ufffd') {
			t.Fatalf("unsafe diagnostic = %q", joined)
		}
	})
}

func TestAgentDefinitionRejectsInvalidUTF8AndLiteralControls(t *testing.T) {
	valid := map[string]bool{"read": true}
	for name, content := range map[string]string{
		"invalid UTF-8": string([]byte{'b', 'o', 'd', 'y', 0xff}),
		"NUL":           "body\x00text",
		"escape":        "body\x1btext",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseAgent("reviewer.md", content, valid); err == nil {
				t.Fatal("unsafe definition text loaded")
			}
		})
	}
}

func TestPlainYAMLContractionDoesNotHideItsTrailingComment(t *testing.T) {
	agent, err := parseClaudeAgent("reviewer.md", `---
name: reviewer
description: Don't edit files # metadata note
---
body
`, map[string]bool{"read": true})
	if err != nil {
		t.Fatal(err)
	}
	if agent.Description != "Don't edit files" {
		t.Fatalf("description = %q, want the YAML comment removed", agent.Description)
	}
}

func TestAgentMetadataAndPromptAreCredentialRedactedBeforeUse(t *testing.T) {
	agent, err := parseAgent("reviewer.md", "---\ndescription: review with "+boundaryTestToken+"\n---\nbody "+boundaryTestToken+"\n",
		map[string]bool{"read": true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(agent.Description, boundaryTestToken) || strings.Contains(agent.Prompt, boundaryTestToken) {
		t.Fatalf("parsed agent retained credential text: %+v", agent)
	}

	configured, err := prepareConfiguredAgents([]Agent{{
		Name: "reviewer", Description: "review with " + boundaryTestToken, Prompt: "use " + boundaryTestToken,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(configured[0].Description, boundaryTestToken) ||
		strings.Contains(configured[0].Prompt, boundaryTestToken) {
		t.Fatalf("configured agent retained credential text: %+v", configured[0])
	}
	if _, err := prepareConfiguredAgents([]Agent{{Name: boundaryTestToken}}); err == nil ||
		strings.Contains(err.Error(), boundaryTestToken) {
		t.Fatalf("credential-shaped identity error = %v, want a safe refusal", err)
	}
}

func FuzzParseAgentDefinitionNeverPanics(f *testing.F) {
	for _, seed := range []string{
		"body\n",
		"---\nname: reviewer\ndescription: review\ntools: [Read, Grep]\n---\nbody\n",
		"---\r\nname: 'reviewer'\r\ntools:\r\n  - Read\r\n---\r\nbody\r\n",
		"---\nname: first\nname: second\n---\nbody\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, content string) {
		if len(content) > int(maxAgentDefinitionBytes) {
			t.Skip()
		}
		valid := map[string]bool{"read": true, "grep": true, "exec": true}
		_, _ = parseAgent("fuzz.md", content, valid)
		_, _ = parseClaudeAgent("fuzz.md", content, valid)
	})
}
