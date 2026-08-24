package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestParseCLIOptionsKeepsLeadingFlagsBeforeSubcommand(t *testing.T) {
	tests := []struct {
		name        string
		input       []string
		wantCommand []string
		check       func(t *testing.T, opts options)
	}{
		{
			name:        "separate values",
			input:       []string{"-workspace", "repo with spaces", "-mode", "auto", "plugins", "list"},
			wantCommand: []string{"plugins", "list"},
			check: func(t *testing.T, opts options) {
				if opts.workspace != "repo with spaces" || opts.mode != "auto" {
					t.Fatalf("options = %#v", opts)
				}
			},
		},
		{
			name:        "equals values",
			input:       []string{"--profile=review", "-sandbox=auto", "completion", "zsh"},
			wantCommand: []string{"completion", "zsh"},
			check: func(t *testing.T, opts options) {
				if opts.profile != "review" || opts.sandbox != "auto" {
					t.Fatalf("options = %#v", opts)
				}
			},
		},
		{
			name:        "bare sandbox does not consume command",
			input:       []string{"-sandbox", "doctor"},
			wantCommand: []string{"doctor"},
			check: func(t *testing.T, opts options) {
				if opts.sandbox != "on" {
					t.Fatalf("sandbox = %q", opts.sandbox)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			opts, command, err := parseCLIOptions(test.input)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(command, "\x00") != strings.Join(test.wantCommand, "\x00") {
				t.Fatalf("command = %q, want %q", command, test.wantCommand)
			}
			test.check(t, opts)
		})
	}
}

func TestSessionInvocationFlagsCannotFallThroughToSubcommands(t *testing.T) {
	for _, name := range subcommandSessionFlags {
		name := name
		t.Run(name, func(t *testing.T) {
			input := []string{"-" + name}
			switch name {
			case "model", "tier", "p", "workflow", "resume":
				input = append(input, "value")
			case "mode":
				input = append(input, "auto")
			case "think":
				input = append(input, "high")
			case "output":
				input = append(input, "json")
			}
			input = append(input, "plugins", "disable", "must-not-run")
			opts, args, err := parseCLIOptions(input)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateSubcommandFlags(opts, args); err == nil || !strings.Contains(err.Error(), "-"+name) {
				t.Fatalf("validateSubcommandFlags(%v) = %v", input, err)
			}
		})
	}
}

func TestPromptAndWorkflowAreMutuallyExclusive(t *testing.T) {
	opts, args, err := parseCLIOptions([]string{"-p", "fix it", "-workflow", "survey internal/agent"})
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 0 {
		t.Fatalf("unexpected positional arguments: %v", args)
	}
	err = validateSessionInvocation(opts)
	if err == nil || !strings.Contains(err.Error(), "-p and -workflow") {
		t.Fatalf("validateSessionInvocation = %v", err)
	}
}

func TestExplicitEmptyPromptStillConflictsWithWorkflow(t *testing.T) {
	opts, _, err := parseCLIOptions([]string{"-p=", "-workflow", "survey"})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSessionInvocation(opts); err == nil {
		t.Fatal("explicit -p was silently discarded in favor of -workflow")
	}
}

func TestExplicitEmptyWorkflowCannotFallIntoAnInteractiveSession(t *testing.T) {
	opts, _, err := parseCLIOptions([]string{"-workflow="})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSessionInvocation(opts); err == nil || !strings.Contains(err.Error(), "non-empty workflow name") {
		t.Fatalf("empty workflow validation = %v", err)
	}
}

func TestUnquotedWorkflowArgumentsCannotBeDroppedOrDispatched(t *testing.T) {
	opts, args, err := parseCLIOptions([]string{"-workflow", "review", "internal/agent", "plugins"})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSessionInvocation(opts); err != nil {
		t.Fatal(err)
	}
	args = consumeWorkflowArguments(&opts, args)
	if len(args) != 0 || opts.workflow != "review internal/agent plugins" {
		t.Fatalf("normalized workflow = %q, args = %v", opts.workflow, args)
	}
}

func TestWorkflowArgumentNamedHelpIsNotReparsedAsCLIHelp(t *testing.T) {
	var out strings.Builder
	for _, args := range [][]string{
		{"-workflow", "review", "help"},
		{"-workflow=review", "plugins", "--help"},
		{"-workflow", "review", "--", "-h"},
	} {
		handled, err := handleCLIHelp(&out, args)
		if handled || err != nil {
			t.Fatalf("handleCLIHelp(%v) = handled %v, err %v", args, handled, err)
		}
	}
}

func TestWorkflowRejectsCompetingTerminalSessionSurfaces(t *testing.T) {
	for _, flag := range []string{"-sessions", "-tiers", "-repl"} {
		t.Run(flag, func(t *testing.T) {
			opts, _, err := parseCLIOptions([]string{"-workflow", "review", flag})
			if err != nil {
				t.Fatal(err)
			}
			if err := validateSessionInvocation(opts); err == nil || !strings.Contains(err.Error(), flag) {
				t.Fatalf("workflow with %s validation = %v", flag, err)
			}
		})
	}
}

func TestHeadlessWorkflowNeverEntersInteractiveOnboarding(t *testing.T) {
	terminal := func(opts options) bool {
		return shouldRunOnboarding(opts, 0, true, true)
	}
	if !terminal(options{}) {
		t.Fatal("a plain first interactive launch should still offer onboarding")
	}
	if terminal(options{workflow: "survey internal/agent"}) {
		t.Fatal("a headless workflow would enter the interactive onboarding wizard")
	}
	if terminal(options{prompt: "inspect this"}) {
		t.Fatal("a headless prompt would enter the interactive onboarding wizard")
	}
	if terminal(options{repl: true}) {
		t.Fatal("an explicit REPL would enter the interactive onboarding wizard")
	}
}

func TestOrdinaryLeadingFlagsStillDispatchSubcommand(t *testing.T) {
	opts, args, err := parseCLIOptions([]string{"-workspace", "ignored here", "completion", "bash"})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSubcommandFlags(opts, args); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	handled, err := runCLISubcommand(context.Background(), &out, opts, args)
	if !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if !strings.HasPrefix(out.String(), "# Switchboard shell completion") {
		t.Fatalf("completion output = %q", out.String())
	}
}

func TestSessionFlagsKeepUnknownPositionalSessionInput(t *testing.T) {
	opts, args, err := parseCLIOptions([]string{"-p", "question", "ordinary", "session", "words"})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSubcommandFlags(opts, args); err != nil {
		t.Fatalf("ordinary positional session input was treated as a subcommand: %v", err)
	}
}

func TestVersionRemainsTerminalBeforeSubcommand(t *testing.T) {
	oldArgs, oldStdout := os.Args, os.Stdout
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Args = []string{"sb", "-version", "update"}
	os.Stdout = writeEnd
	runErr := run()
	_ = writeEnd.Close()
	os.Args, os.Stdout = oldArgs, oldStdout
	out, readErr := io.ReadAll(readEnd)
	_ = readEnd.Close()
	if runErr != nil || readErr != nil {
		t.Fatalf("run=%v read=%v", runErr, readErr)
	}
	if !strings.HasPrefix(string(out), "sb ") {
		t.Fatalf("-version yielded to update: %q", out)
	}
}

func TestPrivateCLIFlagSetCanParseMoreThanOnce(t *testing.T) {
	for _, args := range [][]string{
		{"-mode", "plan"},
		{"-mode", "auto", "completion", "fish"},
	} {
		if _, _, err := parseCLIOptions(args); err != nil {
			t.Fatalf("parse %v: %v", args, err)
		}
	}
}

func TestCLIBinaryFlagExitSemantics(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantExit   int
		wantStdout string
		wantStderr []string
	}{
		{
			name:       "unknown flag",
			args:       []string{"-definitely-invalid"},
			wantExit:   2,
			wantStderr: []string{"flag provided but not defined", "usage:", "subcommands:"},
		},
		{
			name:       "missing flag value",
			args:       []string{"-mode"},
			wantExit:   2,
			wantStderr: []string{"flag needs an argument", "usage:", "session flags:"},
		},
		{
			name:       "legacy help",
			args:       []string{"-help"},
			wantExit:   0,
			wantStdout: "usage:",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestCLIProcessHelper$", "--")
			cmd.Args = append(cmd.Args, test.args...)
			cmd.Env = append(os.Environ(), "SB_CLI_PROCESS_HELPER=1")
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			gotExit := 0
			if exitErr, ok := err.(*exec.ExitError); ok {
				gotExit = exitErr.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if gotExit != test.wantExit {
				t.Fatalf("exit = %d, want %d; stdout=%q stderr=%q", gotExit, test.wantExit, stdout.String(), stderr.String())
			}
			if test.wantStdout != "" && !strings.Contains(stdout.String(), test.wantStdout) {
				t.Errorf("stdout %q does not contain %q", stdout.String(), test.wantStdout)
			}
			for _, want := range test.wantStderr {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr %q does not contain %q", stderr.String(), want)
				}
			}
			if test.wantExit == 0 && stderr.Len() != 0 {
				t.Errorf("successful help wrote stderr: %q", stderr.String())
			}
		})
	}
}

func TestCLIProcessHelper(t *testing.T) {
	if os.Getenv("SB_CLI_PROCESS_HELPER") != "1" {
		return
	}
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 {
		t.Fatal("CLI helper argument separator is missing")
	}
	os.Args = append([]string{"sb"}, os.Args[separator+1:]...)
	main()
}
