package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestCompletionMetadataMatchesBinaryAndHelp(t *testing.T) {
	dispatchSource, err := os.ReadFile("cli_dispatch.go")
	if err != nil {
		t.Fatal(err)
	}

	// Read only top-level case labels from the dedicated dispatcher. It has no
	// nested switches, and quoted extraction also covers grouped cases.
	caseRe := regexp.MustCompile(`(?m)^\s*case ((?:"[a-z]+"(?:,\s*)?)+):`)
	wordRe := regexp.MustCompile(`"([a-z]+)"`)
	var dispatched []string
	for _, label := range caseRe.FindAllStringSubmatch(string(dispatchSource), -1) {
		for _, match := range wordRe.FindAllStringSubmatch(label[1], -1) {
			dispatched = append(dispatched, match[1])
		}
	}
	assertSameWords(t, "subcommands", dispatched, completionSubcommands)

	helpCommands := make([]string, 0, len(commandHelp))
	for command := range commandHelp {
		helpCommands = append(helpCommands, command)
	}
	assertSameWords(t, "help topics", helpCommands, completionSubcommands)

	// Most flags use typed helpers; bool-like enum flags such as sandbox use
	// flag.Var directly.
	flagRe := regexp.MustCompile(`flags\.(?:[A-Za-z]+Var|Var)\([^,]+, "([a-zA-Z-]+)"`)
	var flags []string
	for _, match := range flagRe.FindAllStringSubmatch(string(dispatchSource), -1) {
		flags = append(flags, "-"+match[1])
	}
	assertSameWords(t, "flags", flags, completionFlags)

	for command, actions := range completionActions {
		helpActions := make([]string, 0, len(nestedActionHelp[command]))
		for action := range nestedActionHelp[command] {
			helpActions = append(helpActions, action)
		}
		assertSameWords(t, command+" actions", helpActions, actions)
	}
	for flag := range completionFlagValues {
		if !containsWord(completionFlags, flag) {
			t.Errorf("enum metadata names undeclared flag %s", flag)
		}
	}
	for command, values := range completionArguments {
		if len(values) == 0 {
			t.Errorf("static completion %s has no values", command)
		}
	}
}

func TestCompletionScriptsCoverEveryCommandActionAndEnum(t *testing.T) {
	for _, shell := range []string{"zsh", "bash", "fish"} {
		t.Run(shell, func(t *testing.T) {
			script := completionScript(t, shell)
			for _, command := range completionSubcommands {
				if !strings.Contains(script, command) {
					t.Errorf("script missing subcommand %q", command)
				}
			}
			for command, actions := range completionActions {
				for _, action := range actions {
					if !strings.Contains(script, action) {
						t.Errorf("script missing %s action %q", command, action)
					}
				}
			}
			for flag, values := range completionFlagValues {
				flagSpelling := flag
				if shell == "fish" {
					flagSpelling = strings.TrimPrefix(flag, "-")
				}
				if !strings.Contains(script, flagSpelling) {
					t.Errorf("script missing enum flag %q", flag)
				}
				for _, value := range values {
					if !strings.Contains(script, value) {
						t.Errorf("script missing %s value %q", flag, value)
					}
				}
			}
			for command, values := range completionArguments {
				for _, value := range values {
					if !strings.Contains(script, value) {
						t.Errorf("script missing %s argument %q", command, value)
					}
				}
			}
		})
	}

	var unsupported strings.Builder
	if err := runCompletionCLI(&unsupported, "powershell"); err == nil ||
		!strings.Contains(err.Error(), "zsh, bash, or fish") {
		t.Fatalf("unsupported shell error = %v", err)
	}
}

func TestGeneratedShellScriptsParse(t *testing.T) {
	for _, test := range []struct {
		shell string
		args  []string
	}{
		{shell: "bash", args: []string{"-n"}},
		{shell: "zsh", args: []string{"-n"}},
		{shell: "fish", args: []string{"-n"}},
	} {
		t.Run(test.shell, func(t *testing.T) {
			path, err := exec.LookPath(test.shell)
			if err != nil {
				t.Skipf("%s is not installed", test.shell)
			}
			cmd := exec.Command(path, test.args...)
			cmd.Stdin = strings.NewReader(completionScript(t, test.shell))
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("generated %s script does not parse: %v\n%s", test.shell, err, output)
			}
		})
	}
}

func TestBashCompletionGrammar(t *testing.T) {
	tests := []struct {
		name   string
		words  []string
		want   []string
		absent []string
	}{
		{
			name:  "root",
			words: []string{"sb", ""},
			want:  []string{"plugins", "-mode", "help"},
		},
		{
			name:   "plugin actions",
			words:  []string{"sb", "plugins", ""},
			want:   completionActions["plugins"],
			absent: []string{"-mode", "doctor"},
		},
		{
			name:   "mcp actions",
			words:  []string{"sb", "mcp", ""},
			want:   completionActions["mcp"],
			absent: []string{"install", "-sandbox", "doctor"},
		},
		{
			name:   "flags before subcommand",
			words:  []string{"sb", "-workspace", "repo with spaces", "plugins", ""},
			want:   completionActions["plugins"],
			absent: []string{"-mode", "doctor"},
		},
		{
			name:   "session flag blocks subcommands",
			words:  []string{"sb", "-sessions", ""},
			want:   []string{"-workspace", "--help"},
			absent: []string{"plugins", "doctor"},
		},
		{
			name:   "workflow value blocks subcommands",
			words:  []string{"sb", "-workflow", "survey", ""},
			want:   []string{"-workspace", "--help"},
			absent: []string{"plugins", "doctor"},
		},
		{
			name:   "blocked subcommand has no actions",
			words:  []string{"sb", "-mode", "auto", "plugins", ""},
			absent: append([]string{"-h", "--help"}, completionActions["plugins"]...),
		},
		{
			name:   "unknown flag invalidates root",
			words:  []string{"sb", "-not-a-flag", ""},
			absent: []string{"plugins", "-mode", "--help"},
		},
		{
			name:   "version is terminal",
			words:  []string{"sb", "-version", ""},
			absent: []string{"plugins", "-mode", "--help"},
		},
		{
			name:  "false version still dispatches",
			words: []string{"sb", "-version=false", ""},
			want:  []string{"plugins", "doctor"},
		},
		{
			name:   "end of options permits only commands",
			words:  []string{"sb", "--", ""},
			want:   []string{"plugins", "doctor"},
			absent: []string{"-mode", "--help"},
		},
		{
			name:   "end of options suppresses inline flag values",
			words:  []string{"sb", "--", "-mode=a"},
			absent: []string{"-mode=auto", "-mode=acceptEdits"},
		},
		{
			name:   "mode value",
			words:  []string{"sb", "-mode", "a"},
			want:   []string{"acceptEdits", "auto"},
			absent: []string{"default", "plugins", "-output"},
		},
		{
			name:   "sandbox equals value",
			words:  []string{"sb", "-sandbox=a"},
			want:   []string{"-sandbox=auto"},
			absent: []string{"-sandbox=off", "plugins"},
		},
		{
			name:   "open value suppresses root candidates",
			words:  []string{"sb", "-workspace", ""},
			absent: []string{"plugins", "-mode", "help"},
		},
		{
			name:   "bare sandbox does not consume a value",
			words:  []string{"sb", "-sandbox", "off", "plugins", ""},
			absent: []string{"list", "inspect", "plugins", "mcp", "-mode", "help"},
		},
		{
			name:   "output value",
			words:  []string{"sb", "-output", "j"},
			want:   []string{"json"},
			absent: []string{"text", "plugins"},
		},
		{
			name:   "think value",
			words:  []string{"sb", "-think", "m"},
			want:   []string{"medium", "max"},
			absent: []string{"low", "plugins"},
		},
		{
			name:   "ordinary subcommand stops globals",
			words:  []string{"sb", "doctor", ""},
			want:   []string{"-h", "--help"},
			absent: []string{"-mode", "plugins"},
		},
		{
			name:   "inline enum after subcommand stays local",
			words:  []string{"sb", "plugins", "-mode=a"},
			absent: []string{"-mode=auto", "-mode=acceptEdits", "doctor"},
		},
		{
			name:   "help plugin action",
			words:  []string{"sb", "help", "plugins", ""},
			want:   completionActions["plugins"],
			absent: []string{"-mode", "doctor"},
		},
		{
			name:   "unknown plugin action has no help candidate",
			words:  []string{"sb", "plugins", "not-an-action", ""},
			absent: []string{"-h", "--help", "list"},
		},
		{
			name:  "nested plugin help accepts help flag",
			words: []string{"sb", "plugins", "help", "list", ""},
			want:  []string{"-h", "--help"},
		},
		{
			name:  "help plugin action accepts help flag",
			words: []string{"sb", "help", "plugins", "list", ""},
			want:  []string{"-h", "--help"},
		},
		{
			name:   "nonnested help topic has no action candidates",
			words:  []string{"sb", "help", "auth", ""},
			want:   []string{"-h", "--help"},
			absent: completionArguments["auth"],
		},
		{
			name:   "completion shells",
			words:  []string{"sb", "completion", ""},
			want:   completionArguments["completion"],
			absent: []string{"-mode", "doctor"},
		},
		{
			name:   "auth actions",
			words:  []string{"sb", "auth", ""},
			want:   completionArguments["auth"],
			absent: []string{"-mode", "doctor"},
		},
		{
			name:   "oauth actions",
			words:  []string{"sb", "auth", "oauth", ""},
			want:   completionArguments["auth/oauth"],
			absent: []string{"status", "-mode", "doctor"},
		},
		{
			name:   "static all scope",
			words:  []string{"sb", "stats", ""},
			want:   []string{"all"},
			absent: []string{"-mode", "doctor"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := bashCompletions(t, test.words)
			for _, want := range test.want {
				if !containsWord(got, want) {
					t.Errorf("completion %v missing %q; got %v", test.words, want, got)
				}
			}
			for _, absent := range test.absent {
				if containsWord(got, absent) {
					t.Errorf("completion %v leaked %q; got %v", test.words, absent, got)
				}
			}
		})
	}
}

func TestZshCompletionGrammar(t *testing.T) {
	tests := []struct {
		name   string
		words  []string
		want   []string
		absent []string
	}{
		{
			name:   "plugin actions",
			words:  []string{"sb", "plugins", ""},
			want:   completionActions["plugins"],
			absent: []string{"-mode", "doctor"},
		},
		{
			name:   "flags before subcommand",
			words:  []string{"sb", "-workspace", "repo with spaces", "mcp", ""},
			want:   completionActions["mcp"],
			absent: []string{"-sandbox", "doctor", "install"},
		},
		{
			name:   "session flag blocks subcommands",
			words:  []string{"sb", "-sessions", ""},
			want:   []string{"-workspace", "--help"},
			absent: []string{"plugins", "doctor"},
		},
		{
			name:   "workflow value blocks subcommands",
			words:  []string{"sb", "-workflow", "survey", ""},
			want:   []string{"-workspace", "--help"},
			absent: []string{"plugins", "doctor"},
		},
		{
			name:   "unknown flag invalidates root",
			words:  []string{"sb", "-not-a-flag", ""},
			absent: []string{"plugins", "-mode", "--help"},
		},
		{
			name:   "end of options permits only commands",
			words:  []string{"sb", "--", ""},
			want:   []string{"plugins", "doctor"},
			absent: []string{"-mode", "--help"},
		},
		{
			name:   "end of options suppresses inline flag values",
			words:  []string{"sb", "--", "-mode=a"},
			absent: []string{"-mode=auto", "-mode=acceptEdits"},
		},
		{
			name:   "ordinary subcommand stops globals",
			words:  []string{"sb", "doctor", ""},
			want:   []string{"-h", "--help"},
			absent: []string{"-mode", "plugins"},
		},
		{
			name:   "open value suppresses root candidates",
			words:  []string{"sb", "-p", ""},
			absent: []string{"plugins", "-mode", "help"},
		},
		{
			name:   "bare sandbox does not consume a value",
			words:  []string{"sb", "-sandbox", "off", "mcp", ""},
			absent: []string{"list", "inspect", "plugins", "mcp", "-mode", "help"},
		},
		{
			name:   "help plugin action",
			words:  []string{"sb", "help", "plugins", ""},
			want:   completionActions["plugins"],
			absent: []string{"-mode", "doctor"},
		},
		{
			name:   "unknown plugin action has no help candidate",
			words:  []string{"sb", "plugins", "not-an-action", ""},
			absent: []string{"-h", "--help", "list"},
		},
		{
			name:  "nested plugin help accepts help flag",
			words: []string{"sb", "plugins", "help", "list", ""},
			want:  []string{"-h", "--help"},
		},
		{
			name:  "help plugin action accepts help flag",
			words: []string{"sb", "help", "plugins", "list", ""},
			want:  []string{"-h", "--help"},
		},
		{
			name:   "completion shells",
			words:  []string{"sb", "completion", ""},
			want:   completionArguments["completion"],
			absent: []string{"-mode", "doctor"},
		},
		{
			name:   "auth actions",
			words:  []string{"sb", "auth", ""},
			want:   completionArguments["auth"],
			absent: []string{"-mode", "doctor"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := zshCompletions(t, test.words)
			for _, want := range test.want {
				if !containsWord(got, want) {
					t.Errorf("completion %v missing %q; got %v", test.words, want, got)
				}
			}
			for _, absent := range test.absent {
				if containsWord(got, absent) {
					t.Errorf("completion %v leaked %q; got %v", test.words, absent, got)
				}
			}
		})
	}
}

func TestCompletionShellQuoting(t *testing.T) {
	path, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	want := "space and ' quote $HOME"
	cmd := exec.Command(path)
	cmd.Stdin = strings.NewReader("value=" + shellSingleQuote(want) + `
printf '%s' "$value"
`)
	got, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("quoted value = %q, want %q", got, want)
	}
}

func TestFishCompletionUsesTokenAwareRootConditions(t *testing.T) {
	script := completionScript(t, "fish")
	for _, want := range []string{
		"function __sb_root_flags",
		"function __sb_root_commands",
		"function __sb_has_command",
		"function __sb_awaiting_value",
		"function __sb_needs_argument",
		"function __sb_needs_nested_action",
		"function __sb_accepts_help_flag",
		"__sb_root_flags; or __sb_awaiting_value mode",
		"complete -c sb -n '__sb_accepts_help_flag' -s h -l help",
		"complete -c sb -n '__sb_needs_argument auth'",
		"complete -c sb -n '__sb_needs_nested_action plugins'",
		"case '-model' '--model' '-tier' '--tier' '-mode' '--mode' '-think' '--think' '-p' '--p' '-output' '--output' '-resume' '--resume' '-workflow' '--workflow'",
		"'-workflow=*' '--workflow=*'",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("fish completion missing %q", want)
		}
	}
	for _, absent := range []string{"__fish_seen_subcommand_from", "function __sb_needs_action"} {
		if strings.Contains(script, absent) {
			t.Errorf("fish completion retained token-blind grammar %q", absent)
		}
	}
}

// These digests are compact golden snapshots. Behavioral tests above explain
// important differences; this catches every other byte so install comments,
// quoting, and shell grammar cannot drift silently.
func TestCompletionScriptGoldens(t *testing.T) {
	goldens := map[string]string{
		"bash": "20ab2fc3000eace43e4a01331bd8c09bb93c896389764f1a919fa797c50a8d31",
		"zsh":  "4649b12b23aeb8bcd29e847b02089a86c0e76ee3e79dd29b11c55a1f005c77ed",
		"fish": "f2c34f7400eaf7d807086c8e6381107d65d259dce2d32ee3f02ce52cf64eaebc",
	}
	for shell, want := range goldens {
		got := fmt.Sprintf("%x", sha256.Sum256([]byte(completionScript(t, shell))))
		if got != want {
			t.Errorf("%s completion golden = %s, want %s", shell, got, want)
		}
	}
}

func completionScript(t *testing.T, shell string) string {
	t.Helper()
	var out strings.Builder
	if err := runCompletionCLI(&out, shell); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func bashCompletions(t *testing.T, words []string) []string {
	t.Helper()
	path, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	var probe strings.Builder
	probe.WriteString(completionScript(t, "bash"))
	probe.WriteString("\nCOMP_WORDS=(")
	for _, word := range words {
		probe.WriteString(shellSingleQuote(word))
		probe.WriteByte(' ')
	}
	probe.WriteString(")\n")
	fmt.Fprintf(&probe, "COMP_CWORD=%d\n", len(words)-1)
	probe.WriteString("_sb_complete\nprintf '%s\\n' \"${COMPREPLY[@]}\"\n")

	cmd := exec.Command(path)
	cmd.Stdin = strings.NewReader(probe.String())
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("bash completion probe: %v\n%s", err, stderr.String())
	}
	return strings.Fields(stdout.String())
}

func zshCompletions(t *testing.T, words []string) []string {
	t.Helper()
	path, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh is not installed")
	}
	var probe strings.Builder
	probe.WriteString(`
_describe() {
  local array_name="$2"
  eval "print -rl -- \${${array_name}[@]}"
}
compadd() {
  local value
  for value in "$@"; do
    [[ $value == -* ]] || print -r -- "$value"
  done
}
compset() { return 0 }
words=(`)
	for _, word := range words {
		probe.WriteString(shellSingleQuote(word))
		probe.WriteByte(' ')
	}
	probe.WriteString(")\n")
	fmt.Fprintf(&probe, "CURRENT=%d\n", len(words))
	probe.WriteString(completionScript(t, "zsh"))

	cmd := exec.Command(path)
	cmd.Stdin = strings.NewReader(probe.String())
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("zsh completion probe: %v\n%s", err, stderr.String())
	}
	return strings.Fields(stdout.String())
}

func assertSameWords(t *testing.T, label string, got, want []string) {
	t.Helper()
	got = append([]string(nil), got...)
	want = append([]string(nil), want...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("%s drifted:\n got  %v\n want %v", label, got, want)
	}
}
