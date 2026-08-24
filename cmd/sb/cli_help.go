package main

import (
	"fmt"
	"io"
	"strings"
)

// commandHelp is deliberately static. Help must be available before config,
// session, extension, and provider assembly, including when all of those are
// broken. completion_test.go pins this table to completionSubcommands so the
// dispatcher, help, and generated shell scripts cannot drift independently.
type commandHelpSpec struct {
	usage   string
	summary string
	detail  string
}

var commandHelp = map[string]commandHelpSpec{
	"auth": {
		usage:   "sb auth <status|login|logout|oauth> ...",
		summary: "inspect or change provider credentials",
		detail:  authUsage,
	},
	"update": {
		usage:   "sb update",
		summary: "install the newest verified Switchboard release",
	},
	"doctor": {
		usage:   "sb doctor",
		summary: "check providers, credentials, sandboxing, tools, and MCP",
	},
	"permissions": {
		usage:   "sb permissions [-mode <mode>] [-- <command>]",
		summary: "list the standing permission rules, or ask what they answer for a command",
	},
	"cost": {
		usage:   "sb cost",
		summary: "show this workspace's recorded spend and counterfactual rung costs",
	},
	"find": {
		usage:   "sb find [all] <text>",
		summary: "search recorded sessions in this workspace or every workspace",
	},
	"stats": {
		usage:   "sb stats [all]",
		summary: "summarize recorded sessions, calls, tokens, and routing",
	},
	"races": {
		usage:   "sb races [all]",
		summary: "summarize recorded side-by-side model races",
	},
	"blame": {
		usage:   "sb blame [file[:line]]",
		summary: "trace surviving file lines back to agent turns and rungs",
	},
	"mistakes": {
		usage:   "sb mistakes",
		summary: "show failures repeated across this workspace's sessions",
	},
	"ladder": {
		usage:   "sb ladder",
		summary: "show how this workspace has moved through the model ladder",
	},
	"recap": {
		usage:   "sb recap [session-id]",
		summary: "show where a recorded session left off",
	},
	"export": {
		usage:   "sb export [session-id]",
		summary: "write a recorded session as Markdown",
	},
	"plugins": {
		usage:   "sb plugins <action> [selector]",
		summary: "inspect and control native Claude and Codex plugin inventory",
	},
	"mcp": {
		usage:   "sb mcp <action> [selector]",
		summary: "inspect and control native MCP server activation",
	},
	"completion": {
		usage:   "sb completion <zsh|bash|fish>",
		summary: "generate a shell completion script",
	},
	"help": {
		usage:   "sb help [subcommand [action]]",
		summary: "show root, subcommand, or action help",
	},
}

type actionHelpSpec struct {
	operand string
	summary string
}

var nestedActionHelp = map[string]map[string]actionHelpSpec{
	"plugins": {
		"list":    {summary: "list discovered plugins and their activation state"},
		"inspect": {operand: "<selector>", summary: "show one plugin's provenance and capabilities"},
		"install": {operand: "<selector>", summary: "copy an available plugin into Switchboard's cache and enable it"},
		"enable":  {operand: "<selector>", summary: "enable an installed plugin for the next run"},
		"disable": {operand: "<selector>", summary: "disable a plugin for the next run"},
		"trust":   {operand: "<selector>", summary: "trust the enabled plugin's exact executable digest"},
		"untrust": {operand: "<selector>", summary: "revoke a plugin's executable trust"},
	},
	"mcp": {
		"list":    {summary: "list native MCP servers and their activation state"},
		"inspect": {operand: "<selector>", summary: "show one native MCP server's redacted provenance"},
		"enable":  {operand: "<selector>", summary: "enable a native MCP server for the next run"},
		"disable": {operand: "<selector>", summary: "disable a native MCP server for the next run"},
	},
}

// handleCLIHelp recognizes only help grammar and performs no discovery. The
// bool distinguishes "not help" from a handled request that should return an
// error, allowing run to keep the ordinary command paths unchanged.
func handleCLIHelp(w io.Writer, args []string) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	// Session flags precede a subcommand in the shell grammar. Walk only the
	// closed static flag shape here; never parse config-backed values or invoke
	// the standard FlagSet just to answer help.
	if commandIndex, rootHelp := helpCommandIndex(args); commandIndex > 0 {
		args = args[commandIndex:]
	} else if rootHelp {
		writeRootHelp(w)
		return true, nil
	}
	if isHelpFlag(args[0]) {
		writeRootHelp(w)
		return true, nil
	}
	if args[0] == "help" {
		return true, writeHelpPath(w, args[1:])
	}

	command := args[0]
	if _, known := commandHelp[command]; !known {
		return false, nil
	}
	if actions, nested := nestedActionHelp[command]; nested {
		return handleNestedHelp(w, command, actions, args[1:])
	}
	if hasHelpFlag(args[1:]) {
		writeCommandHelp(w, command)
		return true, nil
	}
	return false, nil
}

func helpCommandIndex(args []string) (index int, rootHelp bool) {
	valueFlags := stringSet(completionValueFlags)
	expectValue := false
	expectWorkflowValue := false
	workflowSelected := false
	for i, arg := range args {
		if expectValue {
			expectValue = false
			if expectWorkflowValue {
				workflowSelected = true
				expectWorkflowValue = false
			}
			continue
		}
		if isHelpFlag(arg) {
			return 0, true
		}
		// Once -workflow has its required value, the first positional word is
		// a workflow argument even when it happens to spell "help" or another
		// real subcommand. Flags may still follow before that word, matching the
		// standard parser, and a help flag in that position remains help.
		if workflowSelected && (arg == "--" || !strings.HasPrefix(arg, "-")) {
			return 0, false
		}
		if _, known := commandHelp[arg]; known {
			return i, false
		}
		flagName, _, hasValue := strings.Cut(arg, "=")
		if strings.HasPrefix(flagName, "--") {
			flagName = flagName[1:]
		}
		if valueFlags[flagName] && !hasValue {
			expectValue = true
			expectWorkflowValue = flagName == "-workflow"
			continue
		}
		if flagName == "-workflow" && hasValue {
			workflowSelected = true
		}
		if containsWord(completionFlags, flagName) || flagName == "-sandbox" {
			continue
		}
		// An unknown positional word belongs to the session, not help. Stop so
		// prompt text that happens to contain a command name is never reparsed.
		if !strings.HasPrefix(arg, "-") {
			return 0, false
		}
	}
	return 0, false
}

func writeHelpPath(w io.Writer, path []string) error {
	clean := make([]string, 0, len(path))
	for _, topic := range path {
		if !isHelpFlag(topic) {
			clean = append(clean, topic)
		}
	}
	path = clean
	if len(path) == 0 {
		writeRootHelp(w)
		return nil
	}
	command := strings.ToLower(strings.TrimSpace(path[0]))
	if _, known := commandHelp[command]; !known {
		return fmt.Errorf("unknown help topic %q; run sb help to list commands", path[0])
	}
	if actions, nested := nestedActionHelp[command]; nested && len(path) > 1 {
		action := strings.ToLower(strings.TrimSpace(path[1]))
		if isHelpFlag(action) && len(path) == 2 {
			writeCommandHelp(w, command)
			return nil
		}
		if _, known := actions[action]; !known {
			return fmt.Errorf("unknown %s action %q; run sb help %s to list actions", command, path[1], command)
		}
		if len(path) > 2 {
			return fmt.Errorf("sb help %s %s takes no more topics", command, action)
		}
		writeActionHelp(w, command, action, actions[action])
		return nil
	}
	if len(path) > 1 {
		return fmt.Errorf("sb help %s takes no action topic", command)
	}
	writeCommandHelp(w, command)
	return nil
}

func handleNestedHelp(w io.Writer, command string, actions map[string]actionHelpSpec, args []string) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	if isHelpFlag(args[0]) {
		writeCommandHelp(w, command)
		return true, nil
	}
	if args[0] == "help" {
		path := make([]string, 0, len(args)-1)
		for _, topic := range args[1:] {
			if !isHelpFlag(topic) {
				path = append(path, topic)
			}
		}
		if len(path) == 0 {
			writeCommandHelp(w, command)
			return true, nil
		}
		action := strings.ToLower(strings.TrimSpace(path[0]))
		spec, known := actions[action]
		if !known {
			return true, fmt.Errorf("unknown %s action %q; run sb help %s to list actions", command, path[0], command)
		}
		if len(path) > 1 {
			return true, fmt.Errorf("sb %s help %s takes no more topics", command, action)
		}
		writeActionHelp(w, command, action, spec)
		return true, nil
	}
	if !hasHelpFlag(args[1:]) {
		return false, nil
	}
	action := strings.ToLower(strings.TrimSpace(args[0]))
	spec, known := actions[action]
	if !known {
		return true, fmt.Errorf("unknown %s action %q; run sb help %s to list actions", command, args[0], command)
	}
	writeActionHelp(w, command, action, spec)
	return true, nil
}

func writeRootHelp(w io.Writer) {
	fmt.Fprintln(w, "Switchboard routes coding work across a model ladder with bounded cost and permissions.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "usage:")
	fmt.Fprintln(w, "  sb [flags]                    start an interactive session")
	fmt.Fprintln(w, "  sb -p \"prompt\"               run exactly one prompt")
	fmt.Fprintln(w, "  sb -workflow name [arguments] run an unattended workflow")
	fmt.Fprintln(w, "  sb <subcommand> [arguments]   run one bounded command")
	fmt.Fprintln(w, "  sb help [subcommand [action]] show scoped help")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "subcommands:")
	for _, command := range completionSubcommands {
		spec := commandHelp[command]
		fmt.Fprintf(w, "  %-12s %s\n", command, spec.summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "session flags:")
	fmt.Fprintln(w, " "+strings.Join(completionFlags, "  "))
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Run sb help <subcommand> for command usage; mode, sandbox, output, and think are closed enums.")
}

func writeCommandHelp(w io.Writer, command string) {
	spec := commandHelp[command]
	if spec.detail != "" {
		fmt.Fprintln(w, strings.TrimSpace(spec.detail))
		return
	}
	if actions, nested := nestedActionHelp[command]; nested {
		fmt.Fprintln(w, "usage:")
		for _, action := range completionActions[command] {
			actionSpec := actions[action]
			line := "  sb " + command + " " + action
			if actionSpec.operand != "" {
				line += " " + actionSpec.operand
			}
			fmt.Fprintf(w, "%-42s %s\n", line, actionSpec.summary)
		}
		fmt.Fprintf(w, "\nSelectors come from sb %s list. Help never opens inventory or changes activation.\n", command)
		return
	}
	fmt.Fprintf(w, "usage: %s\n\n%s.\n", spec.usage, spec.summary)
}

func writeActionHelp(w io.Writer, command, action string, spec actionHelpSpec) {
	usage := "sb " + command + " " + action
	if spec.operand != "" {
		usage += " " + spec.operand
	}
	fmt.Fprintf(w, "usage: %s\n\n%s.\n", usage, spec.summary)
}

func isHelpFlag(arg string) bool {
	name, _, _ := strings.Cut(arg, "=")
	return name == "-h" || name == "--help" || name == "-help"
}

func hasHelpFlag(args []string) bool {
	for _, arg := range args {
		if isHelpFlag(arg) {
			return true
		}
	}
	return false
}

func containsWord(words []string, want string) bool {
	for _, word := range words {
		if word == want {
			return true
		}
	}
	return false
}
