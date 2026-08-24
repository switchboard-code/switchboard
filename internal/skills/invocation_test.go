package skills

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestResolveRequiresAnExactCanonicalSelector(t *testing.T) {
	list := []Skill{
		{Name: "review", Selector: "codex:repo:.agents/skills/review"},
		{Name: "review", Selector: "claude:user:review"},
	}
	if _, err := Resolve(list, "review"); err == nil || !strings.Contains(err.Error(), "/skills") {
		t.Fatalf("a display name must never choose a source implicitly: %v", err)
	}
	got, err := Resolve(list, "claude:user:review")
	if err != nil || got.Origin.Ecosystem != "" || got.Selector != "claude:user:review" {
		t.Fatalf("exact resolution = %+v, %v", got, err)
	}

	list = append(list, Skill{Name: "other", Selector: "claude:user:review"})
	if _, err := Resolve(list, "claude:user:review"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("duplicate canonical selectors must fail closed: %v", err)
	}
}

func TestRenderExplicitClaudeStaticArguments(t *testing.T) {
	sk := Skill{
		Name:          "migrate",
		Selector:      "claude:repo:.claude/skills/migrate",
		Origin:        Origin{Ecosystem: EcosystemClaude},
		ArgumentNames: []string{"issue", "branch", "optional"},
		Body: `all=<$ARGUMENTS>
indexed=<$ARGUMENTS[0]> <$ARGUMENTS[1]> <$ARGUMENTS[2]>
short=<$0> <$1> <$2>
named=<$issue> <$branch> <$optional>
escaped=<\$0> doubled=<\\$1> unrelated=<\$OTHER>`,
	}
	raw := `"hello world" second`
	got, err := RenderExplicit(sk, raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`all=<"hello world" second>`,
		`indexed=<hello world> <second> <$ARGUMENTS[2]>`,
		`short=<hello world> <second> <$2>`,
		`named=<hello world> <second> <>`,
		`escaped=<$0> doubled=<\\second> unrelated=<\$OTHER>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\n\nARGUMENTS:") {
		t.Fatalf("recognized placeholders must not also append arguments:\n%s", got)
	}
}

func TestRenderExplicitAppendsArgumentsWithoutAPlaceholder(t *testing.T) {
	for _, ecosystem := range []Ecosystem{EcosystemClaude, EcosystemCodex, EcosystemSwitchboard} {
		sk := Skill{Name: "plain", Selector: string(ecosystem) + ":repo:plain", Body: "instructions", Origin: Origin{Ecosystem: ecosystem}}
		got, err := RenderExplicit(sk, `one "two words"`)
		if err != nil || got != "instructions\n\nARGUMENTS: one \"two words\"" {
			t.Errorf("%s render = %q, %v", ecosystem, got, err)
		}
	}
}

func TestRenderExplicitRedactsBodyWithoutMutatingSourceSkill(t *testing.T) {
	secret := "ghp_" + strings.Repeat("d", 36)
	for _, ecosystem := range []Ecosystem{EcosystemSwitchboard, EcosystemCodex, EcosystemClaude} {
		t.Run(string(ecosystem), func(t *testing.T) {
			sourceBody := strings.Repeat("x", 390) + " " + secret + " $ARGUMENTS"
			sk := Skill{
				Name: "boundary", Selector: string(ecosystem) + ":repo:boundary",
				Body: sourceBody, Origin: Origin{Ecosystem: ecosystem},
			}
			got, err := RenderExplicit(sk, "target")
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(got, secret) || !strings.Contains(got, "[redacted: a GitHub token]") || !strings.Contains(got, "target") {
				t.Fatalf("explicit %s body was not visibly redacted:\n%s", ecosystem, got)
			}
			if sk.Body != sourceBody {
				t.Fatalf("explicit rendering mutated %s source body", ecosystem)
			}
		})
	}
}

func TestRenderExplicitRejectsInvalidInvocationAndUnsupportedBehavior(t *testing.T) {
	base := Skill{Name: "blocked", Selector: "claude:user:blocked", Body: "body", Origin: Origin{Ecosystem: EcosystemClaude}}
	tests := []struct {
		name string
		sk   Skill
		args string
		want string
	}{
		{name: "user disabled", sk: func() Skill { s := base; s.UserInvocationDisabled = true; return s }(), want: "user-invocable:false"},
		{name: "frontmatter control", sk: func() Skill { s := base; s.InvocationBlockers = []string{`unsupported control "model"`}; return s }(), want: "unsupported control"},
		{name: "inline shell", sk: func() Skill { s := base; s.Body = "context: !`printf secret`"; return s }(), want: "shell injection"},
		{name: "fenced shell", sk: func() Skill { s := base; s.Body = "```!\nprintf secret\n```"; return s }(), want: "shell injection"},
		{name: "dynamic context", sk: func() Skill { s := base; s.Body = "${CLAUDE_PROJECT_DIR}"; return s }(), want: "dynamic context"},
		{name: "file attachment", sk: func() Skill { s := base; s.Body = "Read @secrets.env"; return s }(), want: "file attachment"},
		{name: "bad quoted args", sk: base, args: `"unterminated`, want: "unterminated quote"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := RenderExplicit(tt.sk, tt.args); err == nil || got != "" || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("render = %q, %v; want diagnostic containing %q", got, err, tt.want)
			}
		})
	}

	// Claude only recognizes inline injection at the start of a line or
	// after whitespace. A literal assignment is safe text and stays text.
	literal := base
	literal.Body = "KEY=!`not executable in Claude either`"
	if got, err := RenderExplicit(literal, ""); err != nil || got != literal.Body {
		t.Fatalf("literal shell-like text = %q, %v", got, err)
	}
}

func TestLoadRetainsInvocationInventoryAndFailsClosed(t *testing.T) {
	setTestHome(t, t.TempDir())
	ws := t.TempDir()
	claudeRoot := filepath.Join(ws, ".claude", "skills")

	writePackedSkill(t, claudeRoot, "manual", "---\ndescription: manual\ndisable-model-invocation: true\narguments: [issue, branch]\nargument-hint: '[issue] [branch]'\n---\nFix $issue on $branch")
	writePackedSkill(t, claudeRoot, "model-only", "---\ndescription: model only\nuser-invocable: false\n---\nBackground context")
	writePackedSkill(t, claudeRoot, "path-filtered", "---\ndescription: filtered\npaths: src/**\n---\nReview files")
	writePackedSkill(t, claudeRoot, "tool-control", "---\ndescription: controlled\nallowed-tools: Read Grep\n---\nReview")
	writePackedSkill(t, claudeRoot, "merged-control", "---\npolicy: &policy\n  model: opus\n<<: *policy\ndescription: merged\n---\nReview")
	writePackedSkill(t, claudeRoot, "dynamic-body", "---\ndescription: dynamic\n---\nRead @private.env then run !`pwd`")

	codex := filepath.Join(ws, ".agents", "skills", "dependency")
	writeSkill(t, codex, "SKILL.md", minimal)
	writeSkill(t, filepath.Join(codex, "agents"), "openai.yaml", "dependencies:\n  tools:\n    - type: mcp\n      value: github\n")

	list, notes := Load(ws)
	if len(list) != 7 {
		t.Fatalf("blocked and manual definitions must remain inspectable: %+v, notes %v", list, notes)
	}
	// tool-control declares allowed-tools, which grants tool permission
	// rather than withholding it. Not applying a grant asks more often than
	// the author intended and never less, so the skill stays usable and
	// visible; the merged-control skill hides a model override behind a YAML
	// merge key and stays blocked.
	if got := skillKeys(ModelVisible(list)); !slices.Equal(got, []string{
		"claude:repo:.claude/skills/model-only",
		"claude:repo:.claude/skills/tool-control",
	}) {
		t.Fatalf("model-visible inventory = %v", got)
	}
	controlled := findSkillKey(t, list, "claude:repo:.claude/skills/tool-control")
	if len(controlled.InvocationBlockers) != 0 {
		t.Fatalf("a permission grant blocked the skill: %v", controlled.InvocationBlockers)
	}
	if len(controlled.Notes) == 0 {
		t.Fatal("the unapplied grant has to be recorded rather than dropped")
	}
	if got, err := RenderExplicit(controlled, ""); err != nil || got != "Review" {
		t.Fatalf("tool-control render = %q, %v", got, err)
	}

	manual := findSkillKey(t, list, "claude:repo:.claude/skills/manual")
	if !manual.ImplicitDisabled || manual.ArgumentHint != "[issue] [branch]" || !slices.Equal(manual.ArgumentNames, []string{"issue", "branch"}) {
		t.Fatalf("manual metadata lost: %+v", manual)
	}
	if got, err := RenderExplicit(manual, "42 main"); err != nil || got != "Fix 42 on main" {
		t.Fatalf("manual render = %q, %v", got, err)
	}
	if _, err := RenderExplicit(findSkillKey(t, list, "claude:repo:.claude/skills/model-only"), ""); err == nil {
		t.Fatal("user-invocable:false was accepted explicitly")
	}
	if got, err := RenderExplicit(findSkillKey(t, list, "claude:repo:.claude/skills/path-filtered"), ""); err != nil || got != "Review files" {
		t.Fatalf("an explicit invocation should not need automatic path activation: %q, %v", got, err)
	}
	for _, key := range []string{
		"claude:repo:.claude/skills/merged-control",
		"claude:repo:.claude/skills/dynamic-body",
		"codex:repo:.agents/skills/dependency",
	} {
		sk := findSkillKey(t, list, key)
		if len(sk.InvocationBlockers) == 0 {
			t.Errorf("%s silently ignored its unsupported controls", key)
		}
		if _, err := RenderExplicit(sk, ""); err == nil {
			t.Errorf("%s invoked despite unsupported controls", key)
		}
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "invocation blocked") || !strings.Contains(joined, "dependencies") {
		t.Fatalf("blocked inventory needs explicit diagnostics: %v", notes)
	}
}

func TestInvalidNamedArgumentsBlockRatherThanRenderAmbiguously(t *testing.T) {
	sk, _, err := parseDocumentForEcosystem("bad", "---\ndescription: bad\narguments: [same, same, not.valid]\n---\nUse $same", EcosystemClaude)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(sk.InvocationBlockers, "\n")
	if !strings.Contains(joined, "duplicate named argument") || !strings.Contains(joined, "invalid named argument") {
		t.Fatalf("invalid arguments were silently accepted: %+v", sk)
	}
	if _, err := RenderExplicit(sk, "first second third"); err == nil {
		t.Fatal("ambiguous named arguments rendered")
	}
}

func TestNewToolDefensivelyHidesManualAndBlockedSkills(t *testing.T) {
	tool := NewTool([]Skill{
		{Name: "manual", Selector: "claude:user:manual", Description: "manual", ImplicitDisabled: true},
		{Name: "blocked", Selector: "claude:user:blocked", Description: "blocked", InvocationBlockers: []string{"unsupported control"}},
		{Name: "dynamic", Selector: "claude:user:dynamic", Description: "dynamic", Body: "!`pwd`", Origin: Origin{Ecosystem: EcosystemClaude}},
		{Name: "visible", Selector: "codex:user:visible", Description: "visible"},
	})
	if desc := tool.Description(); strings.Contains(desc, "claude:user:manual") || strings.Contains(desc, "claude:user:blocked") || strings.Contains(desc, "claude:user:dynamic") || !strings.Contains(desc, "codex:user:visible") {
		t.Fatalf("tool description leaked non-model inventory:\n%s", desc)
	}
	if _, err := tool.Plan([]byte(`{"name":"claude:user:manual"}`)); err == nil {
		t.Fatal("a guessed manual-only selector bypassed schema filtering")
	}
}

func TestRenderExplicitNeverExecutesClaudeShellInjection(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "executed")
	sk := Skill{
		Name:     "danger",
		Selector: "claude:user:danger",
		Origin:   Origin{Ecosystem: EcosystemClaude},
		Body:     "!`touch " + marker + "`",
	}
	if _, err := RenderExplicit(sk, ""); err == nil {
		t.Fatal("dynamic shell injection was accepted")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("rendering executed a command: %v", err)
	}
}
