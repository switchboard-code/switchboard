package main

// LSP assembly. Three things have to line up before semantic navigation
// joins the suite: a workspace whose marker names an ecosystem,
// that ecosystem's server on the machine, and a trust grant to this
// checkout — the same grant a repository's declared processes need,
// because a language server runs what the workspace directs (a Go module's
// toolchain directives, a TypeScript project's plugins), unconfined.
// Opening a repository is not permission to run what its build graph
// implies; /trust grant is.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/lsp"
	"github.com/switchboard-code/switchboard/internal/safeexec"
	"github.com/switchboard-code/switchboard/internal/tools"
	"github.com/switchboard-code/switchboard/internal/trust"
)

// lspCandidates maps workspace markers to the server that speaks for them.
// Order is precedence: the first marker present whose server detects wins,
// one server per session. Every entry was verified live against the real
// server before it was listed — argv, handshake, and a cross-file
// definition and references answer (internal/lsp/live_test.go); a server
// nobody has run against for real does not belong in the table, which is
// the §5.2 profile rule applied to language servers.
type lspCandidateSpec struct {
	marker        string
	detect        func() ([]string, bool)
	probe         func(lspResolvedCandidate) bool
	openCloseSync bool
}

type lspResolvedCandidate struct {
	executable  safeexec.Executable
	argv        []string
	environment []string
	workingDir  string
}

var lspCandidates = []lspCandidateSpec{
	{marker: "go.mod", detect: plainServer("gopls")},
	{marker: "tsconfig.json", detect: typescriptNative, probe: probeTypeScriptNative},
	{marker: "package.json", detect: typescriptNative, probe: probeTypeScriptNative},
	{marker: "pyproject.toml", detect: plainServer("pyright-langserver", "--stdio"), openCloseSync: true},
	{marker: "setup.py", detect: plainServer("pyright-langserver", "--stdio"), openCloseSync: true},
	// The marker is the compilation database, not a source extension:
	// without it clangd guesses flags, and a session built on guessed
	// flags would answer with guessed symbols. rust-analyzer is absent
	// under the same rule that keeps the TS5 wrapper out — the
	// verification machine's binary is a rustup shim that never speaks
	// LSP, so the handshake has not been demonstrated (live_test.go).
	{marker: "compile_commands.json", detect: plainServer("clangd")},
}

func plainServer(binary string, args ...string) func() ([]string, bool) {
	return func() ([]string, bool) {
		path, err := exec.LookPath(binary)
		if err != nil {
			return nil, false
		}
		return append([]string{path}, args...), true
	}
}

// typescriptNative detects the installed tsc without executing it. The
// version probe is separate because trust previews must remain data-only:
// a PATH entry may itself come from the workspace.
func typescriptNative() ([]string, bool) {
	path, err := exec.LookPath("tsc")
	if err != nil {
		return nil, false
	}
	return []string{path, "--lsp", "-stdio"}, true
}

// probeTypeScriptNative executes only after the workspace trust gate. tsc's
// native language server is present from TypeScript 7 onward; the older
// wrapper is deliberately not substituted.
func probeTypeScriptNative(candidate lspResolvedCandidate) bool {
	if len(candidate.argv) == 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd, err := candidate.executable.CommandContext(ctx, "--version")
	if err != nil {
		return false
	}
	cmd.Dir = candidate.workingDir
	cmd.Env = append([]string(nil), candidate.environment...)
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	version, ok := strings.CutPrefix(strings.TrimSpace(string(out)), "Version ")
	if !ok {
		return false
	}
	major, _, _ := strings.Cut(version, ".")
	if n, err := strconv.Atoi(major); err != nil || n < 7 {
		return false
	}
	return true
}

func resolveLSPCandidate(workspace string, argv []string) (lspResolvedCandidate, error) {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return lspResolvedCandidate{}, errors.New("language server command is empty")
	}
	roots, err := safeexec.WorkspaceAndCurrentAuthorityRoots(workspace)
	if err != nil {
		return lspResolvedCandidate{}, err
	}
	executable, err := safeexec.ResolvePathOutside(argv[0], roots...)
	if err != nil {
		return lspResolvedCandidate{}, err
	}
	environment, err := safeexec.FilterEnvironmentPath(execution.ScrubbedChildEnv(), roots...)
	if err != nil {
		return lspResolvedCandidate{}, err
	}
	boundArgv := append([]string(nil), argv...)
	boundArgv[0] = executable.Path()
	return lspResolvedCandidate{
		executable: executable, argv: boundArgv, environment: environment,
		workingDir: roots[0],
	}, nil
}

// lspCandidate answers which server would speak for this workspace: the
// first marker present whose server the machine has, the same precedence
// setup applies. It runs nothing - /trust uses it to say what a grant
// covers before the grant is given.
func lspCandidate(workspace string) ([]string, string, bool) {
	for _, c := range lspCandidates {
		if _, err := os.Stat(filepath.Join(workspace, c.marker)); err != nil {
			continue
		}
		if argv, ok := c.detect(); ok {
			candidate, err := resolveLSPCandidate(workspace, argv)
			if err == nil {
				return candidate.argv, c.marker, true
			}
		}
	}
	return nil, "", false
}

// setupLSP registers the tools when everything lines up, and returns the
// server for shutdown plus a note explaining whichever line did not.
func setupLSP(workspace string, trustStore *trust.Store, registry *tools.Registry) (*lsp.Server, string) {
	for _, c := range lspCandidates {
		if _, err := os.Stat(filepath.Join(workspace, c.marker)); err != nil {
			continue
		}
		argv, ok := c.detect()
		if !ok {
			continue // the marker's server is not on this machine; try the next marker
		}
		candidate, err := resolveLSPCandidate(workspace, argv)
		if err != nil {
			continue
		}
		name := filepath.Base(candidate.argv[0])
		if trustStore == nil || !trustStore.Trusted(workspace) {
			return nil, name + " is installed for this workspace's " + c.marker +
				"; /trust grant lets Switchboard verify and start it for definitions, references, outlines, and symbols"
		}
		if c.probe != nil && !c.probe(candidate) {
			continue
		}
		server := &lsp.Server{
			Argv: candidate.argv, Root: workspace, OpenCloseSync: c.openCloseSync,
			Executable: candidate.executable, Environment: candidate.environment,
		}
		for _, tool := range []tools.Tool{
			lsp.NewDefinition(server, registry),
			lsp.NewReferences(server, registry),
			lsp.NewOutline(server, registry),
			lsp.NewSymbols(server, registry),
		} {
			if err := registry.AddExternal(tool); err != nil {
				return server, "language server tools unavailable: " + err.Error()
			}
		}
		return server, name + " serves definitions, references, outlines, symbols, and diagnostics for this workspace"
	}
	return nil, "" // no ecosystem marker with a server on the machine; absent, not broken
}
