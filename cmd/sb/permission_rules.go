package main

// /permissions and `sb permissions`: what has been written down, and what it
// would answer.
//
// A standing rule is only worth having if it can be read back. The engine
// answers requests and says "matched a rule", which is the right thing to say
// mid-turn and not enough to audit a file by: the two surfaces here show the
// rules and let a command be put to them before it is relied on.
//
// The dry-run states its own scope every time, and that is not a courtesy. It
// sees the config's rules and nothing else: MCP allow lists arrive from servers
// that a check outside a session has not started, and a session's remembered
// answers do not exist yet. A dry-run that read as full coverage while covering
// one of three sources would be exactly the confident wrong answer this program
// is built to refuse.

import (
	"fmt"
	"io"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/permission"
)

const permissionsCheckUsage = "usage: sb permissions [-mode <mode>] [-- <command> [args…]]"

func cmdPermissions(m *tuiModel, args string) tea.Cmd {
	if strings.TrimSpace(args) != "" {
		return noticeCmd("", "/permissions takes no arguments; rules are written in "+m.app.config.Path+
			" and `sb permissions -- <command>` says what they would answer")
	}
	m.addInfo(renderPermissionRules(m.app.config, m.app.loop.Perms.Mode()))
	return nil
}

// renderPermissionRules is the shared body of both surfaces, so the list a user
// reads in a session and the list they read from a shell are the same list.
func renderPermissionRules(cfg *config.Config, mode permission.Mode) string {
	var b strings.Builder
	fmt.Fprintf(&b, "permissions  mode %s\n", mode)

	if len(cfg.Permissions) == 0 {
		b.WriteString("\nNo rules are written down, so every answer this session gives it forgets at exit.\n")
		b.WriteString("Write them under [[permissions]] in " + cliText(cfg.Path) + ":\n\n")
		b.WriteString("  [[permissions]]\n  decision = \"allow\"\n  tool = \"exec\"\n  argv_prefix = [\"go\", \"test\"]\n")
		return strings.TrimRight(b.String(), "\n")
	}

	fmt.Fprintf(&b, "\nfrom %s, in the order they answer:\n", cliText(cfg.Path))
	for i, rule := range cfg.Permissions {
		fmt.Fprintf(&b, "  %2d  %s\n", i+1, cliText(config.RenderPermissionRule(rule)))
	}
	b.WriteString("\nA deny answers wherever it sits; among the rest the first match wins, and\n")
	b.WriteString("these are consulted before any allow list an MCP server declared for itself.\n")
	b.WriteString("A rule grants what a typed yes grants: where no sandbox is configured a\n")
	b.WriteString("command it allows runs on the host, with this account's reach.\n")
	return strings.TrimRight(b.String(), "\n")
}

// runPermissionsCLI answers `sb permissions …` without assembling a session.
//
// Nothing here starts a server, opens a workspace, or spends anything: the
// point is to settle whether a rule says what its author meant before a turn
// depends on it.
func runPermissionsCLI(w io.Writer, cfg *config.Config, args []string) error {
	mode := permission.ModeDefault
	rest := args
	if len(rest) >= 2 && rest[0] == "-mode" {
		parsed, err := permission.ParseMode(rest[1])
		if err != nil {
			return err
		}
		mode, rest = parsed, rest[2:]
	}

	// Bare, or a mode with nothing to ask about, lists. The command to check
	// comes after --, because a command has its own flags and this one must
	// not try to read them.
	if len(rest) == 0 {
		fmt.Fprintln(w, renderPermissionRules(cfg, mode))
		return nil
	}
	if rest[0] != "--" {
		return fmt.Errorf("a command to check goes after --, so its own flags stay its own\n%s", permissionsCheckUsage)
	}
	rest = rest[1:]
	if len(rest) == 0 {
		return fmt.Errorf("%s", permissionsCheckUsage)
	}

	// A check has no verified confinement to report on, and saying so is part
	// of the answer: the same command under a configured sandbox may be
	// decided differently, and a dry-run that hid that would be describing a
	// posture the user does not have.
	engine := permission.NewEngineWithExecution(mode,
		execution.NewDefaultController(execution.Capability{}), cfg.Permissions...)
	req := permission.Request{Tool: "exec", Effect: permission.EffectExecute, Argv: rest}
	out := engine.Check(req)

	fmt.Fprintf(w, "command   %s\n", cliText(strings.Join(rest, " ")))
	fmt.Fprintf(w, "mode      %s\n", mode)
	fmt.Fprintf(w, "decision  %s (%s)\n", out.Decision, cliText(out.Reason))
	if rule, ok := matchingRule(cfg.Permissions, req); ok {
		fmt.Fprintf(w, "rule      %s, from %s\n", cliText(config.RenderPermissionRule(rule)), cliText(cfg.Path))
	} else {
		fmt.Fprintf(w, "rule      none matched; the answer is mode %s's own\n", mode)
	}
	fmt.Fprintln(w, "scope     config rules only. An MCP server's allow list needs the server running,")
	fmt.Fprintln(w, "          and a session's remembered answers do not exist outside one, so neither")
	fmt.Fprintln(w, "          is in this answer. No sandbox is assumed; a configured one can change it.")
	return nil
}

// matchingRule reports which rule the engine would have answered with, so the
// output can name it. It repeats the engine's order rather than reaching into
// it: deny first and anywhere, then the first of the rest.
func matchingRule(rules []permission.Rule, req permission.Request) (permission.Rule, bool) {
	for _, rule := range rules {
		if rule.Decision == permission.Deny && permission.RuleMatches(rule, req) {
			return rule, true
		}
	}
	for _, rule := range rules {
		if rule.Decision != permission.Deny && permission.RuleMatches(rule, req) {
			return rule, true
		}
	}
	return permission.Rule{}, false
}
