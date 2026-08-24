package main

// `sb doctor`: every gate between this machine and a working session,
// checked live and reported with its remedy. The checks are the same ones a
// session performs — the tier probe, the credential chain, the sandbox
// self-test, the binary lookups — run once, in order, with nothing sent to
// any model. A row that fails names the next action rather than only the
// diagnosis, because "not set" plus "run /login" is what turns a report
// into a fix.

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/hooks"
	"github.com/switchboard-code/switchboard/internal/mcp"
	"github.com/switchboard-code/switchboard/internal/skills"
	"github.com/switchboard-code/switchboard/internal/tools"
	"github.com/switchboard-code/switchboard/internal/trust"
)

const doctorProbeTimeout = 5 * time.Second

type doctorRow struct {
	label  string
	detail string
	bad    bool
}

func printDoctorSection(w io.Writer, title string, rows []doctorRow) {
	if len(rows) == 0 {
		return
	}
	title = cliText(title)
	for i := range rows {
		rows[i].label = cliText(rows[i].label)
		rows[i].detail = cliText(rows[i].detail)
	}
	fmt.Fprintln(w, title)
	width := 0
	for _, r := range rows {
		if len(r.label) > width {
			width = len(r.label)
		}
	}
	for _, r := range rows {
		mark := "  "
		if r.bad {
			mark = "! "
		}
		fmt.Fprintf(w, "%s%-*s  %s\n", mark, width, r.label, r.detail)
	}
	fmt.Fprintln(w)
}

func runDoctorCLI(ctx context.Context, w io.Writer, cfg *config.Config, cat *catalog.Catalog, reg *providers, workspace string) error {
	return runDoctor(ctx, w, cfg, cat, reg, workspace, true)
}

// runDoctor is the shared body: the CLI takes every section; the in-session
// /doctor skips the MCP probe, because probing would spawn a second instance
// of each declared server beside the ones this session already runs, and
// /mcp answers the in-session question from live state instead.
func runDoctor(ctx context.Context, w io.Writer, cfg *config.Config, cat *catalog.Catalog, reg *providers, workspace string, probeMCP bool) error {
	version := currentVersion()
	if version == "" {
		version = "dev"
	}
	head := []doctorRow{
		{label: "version", detail: version},
		{label: "config", detail: fmt.Sprintf("%s, %d tiers", cfg.Path, len(cfg.Tiers))},
		{label: "catalog", detail: cat.Revision + " (" + cat.Source + ")"},
	}
	printDoctorSection(w, "switchboard", head)

	trustStore, trustErr := trust.Open()

	bad := 0
	count := func(rows []doctorRow) []doctorRow {
		for _, r := range rows {
			if r.bad {
				bad++
			}
		}
		return rows
	}

	printDoctorSection(w, "ladder", count(doctorLadderRows(ctx, cfg, reg)))
	printDoctorSection(w, "credentials", count(doctorCredentialRows(ctx, cfg)))
	printDoctorSection(w, "sandbox", doctorSandboxRows(execution.Detect()))
	printDoctorSection(w, "workspace", count(doctorWorkspaceRows(workspace, trustStore, trustErr)))
	printDoctorSection(w, "tools", count(doctorToolRows(ctx, workspace, trustStore)))
	if probeMCP {
		printDoctorSection(w, "mcp", count(doctorMCPRows(ctx, workspace, trustStore)))
	} else {
		printDoctorSection(w, "mcp", []doctorRow{{label: "servers",
			detail: "not probed from inside a session - a probe would spawn each declared server twice; /mcp shows the live connections"}})
	}

	switch bad {
	case 0:
		fmt.Fprintln(w, "everything a session needs answers")
	case 1:
		fmt.Fprintln(w, "1 check needs attention, marked ! above")
	default:
		fmt.Fprintf(w, "%d checks need attention, marked ! above\n", bad)
	}
	return nil
}

// doctorLadderRows probes every rung the way session start probes one. A
// primary that cannot be served is only a failure when its fallbacks cannot
// serve either, because the rung's promise is availability, not a
// particular server being up.
func doctorLadderRows(ctx context.Context, cfg *config.Config, reg *providers) []doctorRow {
	if len(cfg.Tiers) == 0 {
		return []doctorRow{{label: "-", detail: "no tiers bound; run sb in a terminal to set up, or edit " + cfg.Path}}
	}
	var rows []doctorRow
	for _, tier := range cfg.Tiers {
		label := tier.ID
		if tier.Label != "" {
			label += " " + tier.Label
		}
		pctx, cancel := context.WithTimeout(ctx, doctorProbeTimeout)
		probed, _, note, err := reg.probeTierFallback(pctx, tier)
		cancel()
		switch {
		case err != nil:
			rows = append(rows, doctorRow{label: label, detail: err.Error(), bad: true})
		case note != "":
			// A fallback answered: the rung serves, and the substitution is
			// said here the same way it would be said in a session.
			rows = append(rows, doctorRow{label: label, detail: note})
		default:
			rows = append(rows, doctorRow{label: label, detail: probed.Target.Display() + " answers"})
		}
	}
	return rows
}

// doctorCredentialRows covers what the ladder needs, not everything the
// catalog knows: /login and /setup browse the whole surface list, and the
// doctor's question is whether the configured session can run.
func doctorCredentialRows(ctx context.Context, cfg *config.Config) []doctorRow {
	var rows []doctorRow
	for _, ref := range refsInUse(cfg) {
		standing := credentialStanding(ctx, cfg, ref)
		row := doctorRow{label: ref.String(), detail: standing}
		if standing == "not set" {
			row.detail = "not set; /login " + ref.String() + " stores one, or set " + firstEnvName(ref)
			row.bad = true
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		rows = append(rows, doctorRow{label: "-", detail: "every configured rung is local; no credential is needed"})
	}
	return rows
}

func doctorSandboxRows(capability execution.Capability) []doctorRow {
	return []doctorRow{
		{label: "platform", detail: capability.Platform},
		{label: "mechanism", detail: string(capability.Mechanism)},
		{label: "standing", detail: capability.Summary()},
	}
}

func doctorWorkspaceRows(workspace string, ts *trust.Store, trustErr error) []doctorRow {
	var rows []doctorRow
	if trustErr != nil {
		rows = append(rows, doctorRow{label: "trust", detail: "store unavailable: " + trustErr.Error(), bad: true})
	} else if ts != nil && ts.Trusted(workspace) {
		rows = append(rows, doctorRow{label: "trust", detail: "granted; this checkout's declared servers and hooks may run"})
	} else {
		rows = append(rows, doctorRow{label: "trust", detail: "not granted; repository-declared MCP servers and hooks stay off"})
	}

	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		if _, err := os.Stat(filepath.Join(workspace, name)); err == nil {
			rows = append(rows, doctorRow{label: "instructions", detail: name + " is read into every session's system prompt"})
			break
		}
	}
	return rows
}

// doctorToolRows reports the conditional tools: present, or absent with what
// would make them present. Absence is a standing, not a failure — the same
// framing assembly itself uses.
func doctorToolRows(ctx context.Context, workspace string, ts *trust.Store) []doctorRow {
	var rows []doctorRow

	webCtx, cancel := context.WithTimeout(ctx, doctorProbeTimeout)
	defer cancel()
	if err := tools.ProbeWeb(webCtx); err != nil {
		rows = append(rows, doctorRow{label: "web", bad: true,
			detail: "the search backend is unreachable: " + redactCredentialTextBeforeTruncate(err.Error(), 80) + "; websearch and webfetch will error until the network answers"})
	} else {
		rows = append(rows, doctorRow{label: "web", detail: "the search backend answers; websearch and webfetch are in the suite"})
	}

	if _, err := exec.LookPath("ast-grep"); err == nil {
		rows = append(rows, doctorRow{label: "astgrep", detail: "ast-grep found; structural search is in the suite"})
	} else {
		rows = append(rows, doctorRow{label: "astgrep", detail: "ast-grep not found; installing it adds structural search"})
	}

	if row, ok := doctorComputerRow(ctx); ok {
		rows = append(rows, row)
	}

	rows = append(rows, doctorLSPRow(workspace, ts))

	if list, skillNotes := skills.Load(workspace); len(list) > 0 || len(skillNotes) > 0 {
		detail := fmt.Sprintf("%d loaded", len(list))
		if len(skillNotes) > 0 {
			detail += fmt.Sprintf(", %d skipped: %s", len(skillNotes), skillNotes[0])
		}
		rows = append(rows, doctorRow{label: "skills", detail: detail, bad: len(skillNotes) > 0})
	}

	if home, err := os.UserHomeDir(); err == nil {
		set, _ := hooks.LoadRooted(home, filepath.Join(".switchboard", hooks.FileName), workspace)
		if !set.Empty() {
			rows = append(rows, doctorRow{label: "hooks", detail: fmt.Sprintf("%d loaded from ~/.switchboard/%s", len(set.Hooks()), hooks.FileName)})
		}
	}
	return rows
}

func doctorLSPRow(workspace string, ts *trust.Store) doctorRow {
	for _, c := range lspCandidates {
		if _, err := os.Stat(filepath.Join(workspace, c.marker)); err != nil {
			continue
		}
		argv, ok := c.detect()
		if !ok {
			return doctorRow{label: "lsp", detail: "this workspace's " + c.marker + " names an ecosystem, but its server is not on this machine"}
		}
		candidate, err := resolveLSPCandidate(workspace, argv)
		if err != nil {
			return doctorRow{label: "lsp", detail: "this workspace's " + c.marker + " names an ecosystem, but its server is not available from a trusted installation path", bad: true}
		}
		name := filepath.Base(candidate.argv[0])
		if ts == nil || !ts.Trusted(workspace) {
			return doctorRow{label: "lsp", detail: name + " can serve this workspace; /trust grant lets it answer definition and references"}
		}
		return doctorRow{label: "lsp", detail: name + " serves definition and references for this workspace"}
	}
	return doctorRow{label: "lsp", detail: "no ecosystem marker with a verified server; symbol lookup stays off"}
}

// doctorMCPRows connects each declared server the way session assembly does,
// then disconnects: a server that answers the handshake and lists its tools
// is one that will serve, and one that does not is named with its error.
func doctorMCPRows(ctx context.Context, workspace string, ts *trust.Store) []doctorRow {
	var rows []doctorRow
	var specs []mcp.Spec

	if home, err := os.UserHomeDir(); err == nil {
		userSpecs, err := mcp.LoadSpecsRooted(home, filepath.Join(".switchboard", mcp.SpecFileName))
		if err != nil {
			rows = append(rows, doctorRow{label: "config", detail: err.Error(), bad: true})
		}
		specs = append(specs, userSpecs...)
	}
	repoPath := filepath.Join(workspace, ".switchboard", mcp.SpecFileName)
	if _, err := os.Stat(repoPath); err == nil {
		if ts != nil && ts.Trusted(workspace) {
			repoSpecs, err := mcp.LoadSpecsRooted(workspace, filepath.Join(".switchboard", mcp.SpecFileName))
			if err != nil {
				rows = append(rows, doctorRow{label: "config", detail: err.Error(), bad: true})
			}
			specs = append(specs, repoSpecs...)
		} else {
			rows = append(rows, doctorRow{label: "repo", detail: "declares servers; they stay off until /trust grant"})
		}
	}

	for _, spec := range specs {
		cctx, cancel := context.WithTimeout(ctx, doctorProbeTimeout)
		c, err := mcp.Connect(cctx, spec, func(string, string) {})
		cancel()
		if err != nil {
			rows = append(rows, doctorRow{label: spec.Name, detail: "did not connect: " + err.Error(), bad: true})
			continue
		}
		rows = append(rows, doctorRow{label: spec.Name, detail: fmt.Sprintf("connected, %d tools", len(c.BridgedTools()))})
		c.Close()
	}

	if len(rows) == 0 {
		rows = append(rows, doctorRow{label: "-", detail: "no servers declared; ~/.switchboard/" + mcp.SpecFileName + " adds them"})
	}
	return rows
}

func firstEnvName(ref credential.Ref) string {
	names := credential.EnvNames(ref)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}
