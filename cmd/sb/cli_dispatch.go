package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/session"
)

type cliParseError struct{ cause error }

func (e *cliParseError) Error() string { return e.cause.Error() }
func (e *cliParseError) Unwrap() error { return e.cause }

// parseCLIOptions uses a private FlagSet so tests and embedding callers do not
// inherit process-global parse state. The standard parser stops at the first
// positional word: global flags may precede a subcommand, while everything
// after the subcommand belongs exclusively to that command.
func parseCLIOptions(args []string) (options, []string, error) {
	var opts options
	flags := flag.NewFlagSet("sb", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.model, "model", os.Getenv("SB_MODEL"), "Ollama model to bind directly, bypassing the configured tiers")
	flags.StringVar(&opts.tier, "tier", "", "tier to start on, for example t2 (default: the lowest configured tier)")
	flags.StringVar(&opts.host, "host", "", "Ollama base URL (default [providers.ollama] base_url, $OLLAMA_HOST, or http://localhost:11434)")
	flags.StringVar(&opts.mode, "mode", "default", "permission mode: plan, default, acceptEdits, auto, yolo, or bypass")
	flags.Var(sandboxFlag{target: &opts.sandbox}, "sandbox", "command confinement: bare flag means on; also accepts off, on, or auto (default: config, then off)")
	flags.StringVar(&opts.think, "think", "", "reasoning effort: the target's own levels (low, medium, high, and where the model states them, xhigh, max, or ultra)")
	flags.StringVar(&opts.workspace, "workspace", "", "workspace root (default: current directory)")
	flags.StringVar(&opts.profile, "profile", "", "run on a named alternate ladder from [profiles.<name>] in the config")
	flags.StringVar(&opts.prompt, "p", "", "run a single prompt and exit; piped stdin is attached to it")
	flags.StringVar(&opts.workflow, "workflow", "", "run a workflow by name and exit, for example -workflow \"survey internal/agent\"")
	flags.StringVar(&opts.output, "output", "text", "what a -p run prints: text, json for one machine-readable result line, or stream-json for one JSON object per event as it happens")
	flags.StringVar(&opts.resume, "resume", "", "resume a session by id")
	flags.BoolVar(&opts.cont, "continue", false, "resume the most recent session for this workspace")
	flags.BoolVar(&opts.list, "sessions", false, "list sessions for this workspace and exit")
	flags.BoolVar(&opts.showTiers, "tiers", false, "list the configured tiers and exit")
	flags.BoolVar(&opts.repl, "repl", false, "use the line-oriented REPL instead of the TUI")
	flags.BoolVar(&opts.version, "version", false, "print the version and exit")
	flags.BoolVar(&opts.allowSecrets, "allow-secrets", false, "send a -p prompt even when it contains something key-shaped")
	if err := flags.Parse(args); err != nil {
		return options{}, nil, &cliParseError{cause: err}
	}
	opts.cliSetFlags = make(map[string]bool)
	flags.Visit(func(parsed *flag.Flag) {
		opts.cliSetFlags[parsed.Name] = true
	})
	return opts, flags.Args(), nil
}

// subcommandSessionFlags are meaningful only while assembling a session.
var subcommandSessionFlags = []string{
	"model", "tier", "mode", "sandbox", "think",
	"p", "workflow", "output", "resume", "continue", "sessions", "tiers", "repl", "allow-secrets",
}

// validateSessionInvocation rejects two independently terminal ways to run a
// session. Letting one silently win would be especially misleading with
// -output json: the flag describes a single -p result, not workflow output.
func validateSessionInvocation(opts options) error {
	promptSet := opts.prompt != "" || opts.cliSetFlags != nil && opts.cliSetFlags["p"]
	workflowSet := opts.workflow != "" || opts.cliSetFlags != nil && opts.cliSetFlags["workflow"]
	if promptSet && workflowSet {
		return fmt.Errorf("-p and -workflow cannot be combined; run either one prompt or one workflow")
	}
	if workflowSet && strings.TrimSpace(opts.workflow) == "" {
		return fmt.Errorf("-workflow needs a non-empty workflow name")
	}
	if workflowSet {
		switch {
		case opts.list:
			return fmt.Errorf("-workflow and -sessions cannot be combined")
		case opts.showTiers:
			return fmt.Errorf("-workflow and -tiers cannot be combined")
		case opts.repl:
			return fmt.Errorf("-workflow and -repl cannot be combined; omit -repl for the unattended workflow surface")
		}
	}
	return nil
}

// consumeWorkflowArguments gives an explicitly selected workflow ownership of
// the remaining positional words. The standard flag parser stops at the first
// one; leaving them in args would either dispatch an accidental subcommand or
// silently drop the workflow's arguments.
func consumeWorkflowArguments(opts *options, args []string) []string {
	if opts == nil || len(args) == 0 || opts.cliSetFlags == nil || !opts.cliSetFlags["workflow"] {
		return args
	}
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, opts.workflow)
	parts = append(parts, args...)
	opts.workflow = strings.TrimSpace(strings.Join(parts, " "))
	return nil
}

// validateSubcommandFlags rejects flags that select or modify a session when
// positional input resolves to a bounded subcommand. Silently discarding one
// side is dangerous: `-sessions plugins disable x`, for example, must never
// disable a plugin merely because command dispatch happened first.
func validateSubcommandFlags(opts options, args []string) error {
	if len(args) == 0 || !containsWord(completionSubcommands, args[0]) {
		return nil
	}
	for _, name := range subcommandSessionFlags {
		if opts.cliSetFlags[name] {
			return fmt.Errorf("-%s cannot be combined with subcommand %q", name, args[0])
		}
	}
	return nil
}

// runCLISubcommand is the bounded, session-free command dispatcher. It receives
// already-parsed leading globals, so `sb -workspace path plugins list` follows
// the same grammar the generated completions advertise. The bool lets unknown
// positional input retain the interactive-session behavior it had before the
// dispatcher was split out.
func runCLISubcommand(ctx context.Context, w io.Writer, opts options, args []string) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	command, commandArgs := args[0], args[1:]
	switch command {
	case codexCredentialHelperCommand:
		if len(opts.cliSetFlags) != 0 {
			return true, fmt.Errorf("internal credential-helper dispatch accepts no flags")
		}
		return true, runCredentialHelperDispatch(w, commandArgs)
	case "completion":
		if len(commandArgs) > 1 {
			return true, fmt.Errorf("sb completion takes one shell; %q is extra", commandArgs[1])
		}
		shell := ""
		if len(commandArgs) == 1 {
			shell = commandArgs[0]
		}
		return true, runCompletionCLI(w, shell)
	case "plugins":
		workspace, err := cliWorkspace(opts.workspace)
		if err != nil {
			return true, err
		}
		return true, runPluginsCLIContext(ctx, w, workspace, commandArgs)
	case "mcp":
		workspace, err := cliWorkspace(opts.workspace)
		if err != nil {
			return true, err
		}
		return true, runMCPCLIContext(ctx, w, workspace, commandArgs)
	case "permissions":
		cfg, err := loadCLIConfig(opts.profile)
		if err != nil {
			return true, err
		}
		return true, runPermissionsCLI(w, cfg, commandArgs)
	case "auth":
		cfg, err := loadCLIConfig(opts.profile)
		if err != nil {
			return true, err
		}
		return true, runAuth(ctx, commandArgs, cfg)
	case "update":
		if len(commandArgs) != 0 {
			return true, fmt.Errorf("sb update takes no argument; %q was ignored by nothing", commandArgs[0])
		}
		cfg, err := config.Load()
		if err != nil {
			return true, err
		}
		return true, runUpdateCLI(ctx, cfg)
	case "doctor":
		if len(commandArgs) != 0 {
			return true, fmt.Errorf("sb doctor takes no argument; %q is not one", commandArgs[0])
		}
		workspace, err := cliWorkspace(opts.workspace)
		if err != nil {
			return true, err
		}
		cfg, err := loadCLIConfig(opts.profile)
		if err != nil {
			return true, err
		}
		cat, err := catalog.Load()
		if err != nil {
			return true, err
		}
		return true, runDoctorCLI(ctx, w, cfg, cat, newProviders(opts.host, cfg), workspace)
	case "cost":
		if len(commandArgs) != 0 {
			return true, fmt.Errorf("sb cost takes no argument; %q is not one", commandArgs[0])
		}
		workspace, err := cliWorkspace(opts.workspace)
		if err != nil {
			return true, err
		}
		store, err := session.DefaultStore()
		if err != nil {
			return true, err
		}
		cat, err := catalog.Load()
		if err != nil {
			return true, err
		}
		return true, runCostCLI(w, store, cat, workspace)
	case "find":
		workspace, err := cliWorkspace(opts.workspace)
		if err != nil {
			return true, err
		}
		store, err := session.DefaultStore()
		if err != nil {
			return true, err
		}
		return true, runFindCLI(w, store, workspace, strings.Join(commandArgs, " "))
	case "stats":
		workspace, err := cliWorkspace(opts.workspace)
		if err != nil {
			return true, err
		}
		cfg, err := loadCLIConfig(opts.profile)
		if err != nil {
			return true, err
		}
		store, err := session.DefaultStore()
		if err != nil {
			return true, err
		}
		cat, err := catalog.Load()
		if err != nil {
			return true, err
		}
		scope := ""
		if len(commandArgs) > 0 {
			scope = commandArgs[0]
		}
		return true, runStatsCLI(w, store, cat, cfg, workspace, scope)
	case "races":
		workspace, err := cliWorkspace(opts.workspace)
		if err != nil {
			return true, err
		}
		store, err := session.DefaultStore()
		if err != nil {
			return true, err
		}
		if len(commandArgs) > 0 {
			if commandArgs[0] != "all" {
				return true, fmt.Errorf("sb races takes no argument, or all; %q is neither", commandArgs[0])
			}
			return true, runRacesAllCLI(w, store)
		}
		return true, runRacesCLI(w, store, workspace)
	case "recap", "export":
		if len(commandArgs) > 1 {
			return true, fmt.Errorf("sb %s takes one session id, or none for the most recent; %q is extra", command, commandArgs[1])
		}
		workspace, err := cliWorkspace(opts.workspace)
		if err != nil {
			return true, err
		}
		store, err := session.DefaultStore()
		if err != nil {
			return true, err
		}
		id := ""
		if len(commandArgs) == 1 {
			id = commandArgs[0]
		}
		if command == "recap" {
			return true, runRecapCLI(w, store, workspace, id)
		}
		return true, runExportCLI(w, store, workspace, id)
	case "ladder":
		if len(commandArgs) != 0 {
			return true, fmt.Errorf("sb ladder takes no argument; %q is not one", commandArgs[0])
		}
		workspace, err := cliWorkspace(opts.workspace)
		if err != nil {
			return true, err
		}
		cfg, err := loadCLIConfig(opts.profile)
		if err != nil {
			return true, err
		}
		store, err := session.DefaultStore()
		if err != nil {
			return true, err
		}
		return true, runLadderCLI(w, cfg.Tiers, store, workspace)
	case "mistakes":
		if len(commandArgs) != 0 {
			return true, fmt.Errorf("sb mistakes takes no argument; %q is not one", commandArgs[0])
		}
		workspace, err := cliWorkspace(opts.workspace)
		if err != nil {
			return true, err
		}
		store, err := session.DefaultStore()
		if err != nil {
			return true, err
		}
		return true, runMistakesCLI(w, store, workspace)
	case "blame":
		if len(commandArgs) > 1 {
			return true, fmt.Errorf("sb blame takes one file, or none for the workspace receipt; %q is extra", commandArgs[1])
		}
		workspace, err := cliWorkspace(opts.workspace)
		if err != nil {
			return true, err
		}
		store, err := session.DefaultStore()
		if err != nil {
			return true, err
		}
		cat, err := catalog.Load()
		if err != nil {
			return true, err
		}
		path := ""
		if len(commandArgs) == 1 {
			path = commandArgs[0]
		}
		return true, runBlameCLI(w, store, cat, workspace, path)
	case "help":
		// The pure prelude handles this before parsing. Keeping the case makes
		// direct dispatcher calls total without opening any runtime state.
		return true, writeHelpPath(w, commandArgs)
	default:
		return false, nil
	}
}

func cliWorkspace(configured string) (string, error) {
	workspace := configured
	if strings.TrimSpace(workspace) == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		workspace = cwd
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	return abs, nil
}

func loadCLIConfig(profile string) (*config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if profile != "" {
		if err := cfg.ApplyProfile(profile); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}
