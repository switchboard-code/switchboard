package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/switchboard-code/switchboard/internal/mcpnative"
)

// nativeMCPInventory is read-only native configuration joined to
// Switchboard's independent activation ledger. Native enabled state remains a
// constraint and never becomes an activation by itself.
type nativeMCPInventory struct {
	result                   mcpnative.Result
	state                    *nativeMCPActivationState
	workspace                string
	notes                    []mcpNote
	codexRequirementsChecked bool
	codexSnapshotErr         error
}

func openNativeMCPInventory(ctx context.Context, workspace string, forceCodexSnapshot bool) *nativeMCPInventory {
	home, homeErr := os.UserHomeDir()
	state, stateErr := openNativeMCPActivationState()
	opts := nativeMCPOptions(home, workspace, workspace)
	var snapshotNotes []mcpNote
	var requirementsChecked bool
	if forceCodexSnapshot || state != nil && state.hasDialect(mcpnative.DialectCodex, workspace) {
		snapshot, snapshotErr := readCodexAppServerSnapshot(ctx, workspace)
		if snapshotErr != nil {
			snapshotNotes = append(snapshotNotes, mcpNote{"error", "native Codex MCP snapshot is unavailable; Codex servers stay off: " + snapshotErr.Error()})
		} else {
			opts.CodexSnapshot = snapshot.config
			requirementsChecked = snapshot.requirementsChecked
			if !requirementsChecked {
				snapshotNotes = append(snapshotNotes, mcpNote{"error", "native Codex MCP requirements are present but cannot yet be projected exactly; Codex servers stay off"})
			}
		}
	}
	inv := discoverNativeMCP(opts, state)
	inv.codexRequirementsChecked = requirementsChecked
	if state != nil && state.hasDialect(mcpnative.DialectCodex, workspace) && opts.CodexSnapshot == nil {
		if state.snapshotFailureRequired(mcpnative.DialectCodex, workspace) {
			inv.codexSnapshotErr = errors.New("a required or legacy Codex MCP activation cannot be assembled without the authoritative Codex snapshot")
		} else {
			inv.notes = append(inv.notes, mcpNote{"warn", "optional Codex MCP activations cannot be checked without the authoritative snapshot; they stay off for this run"})
		}
	}
	inv.notes = append(snapshotNotes, inv.notes...)
	if homeErr != nil {
		inv.notes = append(inv.notes, mcpNote{"error", "native MCP: home directory unavailable: " + homeErr.Error()})
	}
	if stateErr != nil {
		inv.state = nil
		inv.notes = append(inv.notes, mcpNote{"error", "native MCP: activation state is unavailable; native servers stay off: " + stateErr.Error()})
	}
	return inv
}

func discoverNativeMCP(opts mcpnative.Options, state *nativeMCPActivationState) *nativeMCPInventory {
	result := mcpnative.Discover(opts)
	inv := &nativeMCPInventory{result: result, state: state, workspace: opts.Workspace}
	for _, diagnostic := range result.Diagnostics {
		level := "warn"
		if diagnostic.Severity == mcpnative.SeverityError {
			level = "error"
		}
		text := fmt.Sprintf("native MCP %s: %s", diagnostic.Code, diagnostic.Message)
		if diagnostic.Entry != "" {
			text += " [" + diagnostic.Entry + "]"
		}
		if diagnostic.Field != "" {
			text += " field " + diagnostic.Field
		}
		if diagnostic.Path != "" {
			text += " (" + diagnostic.Path + ")"
		}
		inv.notes = append(inv.notes, mcpNote{level, text})
	}
	return inv
}

func nativeMCPOptions(home, workspace, current string) mcpnative.Options {
	opts := mcpnative.Options{
		HomeDir:    home,
		Workspace:  workspace,
		CurrentDir: current,
	}
	if configured := strings.TrimSpace(os.Getenv("CODEX_HOME")); configured != "" {
		opts.CodexConfigDir = configured
	}
	if configured := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); configured != "" {
		opts.ClaudeStatePath = filepath.Join(configured, ".claude.json")
	}
	opts.ClaudeManagedMCPPath = nativeClaudeManagedMCPPath(runtime.GOOS, os.Getenv("ProgramFiles"))
	return opts
}

func nativeClaudeManagedMCPPath(goos, programFiles string) string {
	switch goos {
	case "darwin":
		return "/Library/Application Support/ClaudeCode/managed-mcp.json"
	case "linux":
		return "/etc/claude-code/managed-mcp.json"
	case "windows":
		if strings.TrimSpace(programFiles) == "" {
			programFiles = `C:\Program Files`
		}
		return filepath.Join(programFiles, "ClaudeCode", "managed-mcp.json")
	default:
		return ""
	}
}

func runMCPCLI(w io.Writer, workspace string, args []string) error {
	return runMCPCLIContext(context.Background(), w, workspace, args)
}

func runMCPCLIContext(ctx context.Context, w io.Writer, workspace string, args []string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// `sb mcp` is an explicit native-integration request, so it may launch the
	// installed Codex app-server to obtain the authoritative read-only view.
	inv := openNativeMCPInventory(ctx, workspace, true)
	if err := ctx.Err(); err != nil {
		return err
	}
	return runNativeMCPActionContext(ctx, w, workspace, inv, args)
}

func runNativeMCPAction(w io.Writer, workspace string, inv *nativeMCPInventory, args []string) error {
	return runNativeMCPActionContext(context.Background(), w, workspace, inv, args)
}

func runNativeMCPActionContext(ctx context.Context, w io.Writer, workspace string, inv *nativeMCPInventory, args []string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if inv == nil {
		return fmt.Errorf("native MCP inventory is unavailable")
	}
	action := "list"
	if len(args) > 0 {
		action = strings.ToLower(strings.TrimSpace(args[0]))
		args = args[1:]
	}
	switch action {
	case "", "list":
		if len(args) != 0 {
			return fmt.Errorf("sb mcp list takes no argument; %q is extra", args[0])
		}
		writeNativeMCPList(w, inv)
		return nil
	case "inspect":
		if len(args) != 1 {
			return fmt.Errorf("sb mcp inspect takes exactly one server selector")
		}
		server, err := inv.resolve(args[0])
		if err != nil {
			return err
		}
		writeNativeMCPInspect(w, inv, server)
		return nil
	case "enable", "disable":
		if len(args) != 1 {
			return fmt.Errorf("sb mcp %s takes exactly one server selector", action)
		}
		if inv.state == nil {
			return fmt.Errorf("native MCP activation state is unavailable; refusing to %s anything", action)
		}
		selector := strings.TrimSpace(args[0])
		var server mcpnative.Server
		var request mcpnative.ActivationRequest
		var requestErr error
		var reference nativeMCPActivationReference
		var saved bool
		var err error
		if action == "disable" {
			reference, saved, err = inv.resolveSavedActivation(workspace, selector)
			if err != nil {
				return err
			}
		}
		if !saved {
			server, err = inv.resolve(selector)
			if err != nil {
				return err
			}
			request, requestErr = inv.result.ActivationRequest(server.ID)
		}
		if action == "enable" {
			if requestErr != nil {
				return fmt.Errorf("enable %s: %w", server.ID, requestErr)
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			err = inv.state.enableWithRequiredContext(ctx, request, server.Required)
		} else {
			// Disabling is a recovery operation: it remains possible after the
			// native entry is disabled, malformed, or changed. Definition bytes
			// are unnecessary because disable removes every digest for this exact
			// source identity.
			if cancelErr := ctx.Err(); cancelErr != nil {
				return cancelErr
			}
			if saved {
				err = inv.state.disableReferenceContext(ctx, reference)
				server.ID = reference.ID
			} else {
				if requestErr != nil {
					request = mcpnative.ActivationRequest{
						ID: server.ID, RealPath: server.Provenance.RealPath, TrustRoot: server.TrustRoot,
					}
				}
				err = inv.state.disableReferenceContext(ctx, nativeMCPActivationReference{
					ID: request.ID, RealPath: request.RealPath, TrustRoot: request.TrustRoot,
				})
			}
		}
		if err != nil {
			return fmt.Errorf("%s %s: %w", action, server.ID, err)
		}
		past := "enabled"
		if action == "disable" {
			past = "disabled"
		}
		fmt.Fprintf(w, "%s %s in Switchboard; the change applies on the next Switchboard run\n", past, cliText(server.ID))
		if action == "enable" && server.ExecutionTrustRequired {
			fmt.Fprintf(w, "workspace execution trust is separate; grant it with /trust grant before this project server may start\n")
		}
		return nil
	default:
		return fmt.Errorf("unknown mcp action %q: use list, inspect, enable, or disable", action)
	}
}

// resolveSavedActivation provides the recovery path for definitions that are
// no longer present in native configuration. Exact dialect-qualified IDs and
// canonical config paths remain convenient when unique; the opaque saved:
// token selects the complete ID/path/trust-root identity when they are not.
// Short names still resolve exclusively through live native inventory.
func (inv *nativeMCPInventory) resolveSavedActivation(workspace, selector string) (nativeMCPActivationReference, bool, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return nativeMCPActivationReference{}, false, fmt.Errorf("native MCP selector is empty")
	}
	var matches []nativeMCPActivationReference
	for _, reference := range inv.state.references(workspace) {
		if reference.ID == selector || reference.RealPath == selector || reference.RecoveryToken == selector {
			matches = append(matches, reference)
		}
	}
	switch len(matches) {
	case 0:
		return nativeMCPActivationReference{}, false, nil
	case 1:
		return matches[0], true, nil
	default:
		return nativeMCPActivationReference{}, false, fmt.Errorf("native MCP selector %q is ambiguous across %d saved activations; use the saved: recovery selector shown by sb mcp list", selector, len(matches))
	}
}

func (inv *nativeMCPInventory) resolve(selector string) (mcpnative.Server, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return mcpnative.Server{}, fmt.Errorf("native MCP selector is empty")
	}
	var matches []mcpnative.Server
	for _, server := range inv.result.Servers {
		if server.ID == selector || server.Name == selector || server.Provenance.RealPath == selector {
			matches = append(matches, server)
		}
	}
	switch len(matches) {
	case 0:
		return mcpnative.Server{}, fmt.Errorf("no native MCP server matches %q", selector)
	case 1:
		return matches[0], nil
	default:
		return mcpnative.Server{}, fmt.Errorf("native MCP selector %q is ambiguous across %d entries; use the dialect-qualified ID", selector, len(matches))
	}
}

func writeNativeMCPList(w io.Writer, inv *nativeMCPInventory) {
	stale := inv.staleActivationReferences()
	if len(inv.result.Servers) == 0 && len(stale) == 0 {
		fmt.Fprintln(w, "no native Codex or Claude MCP servers discovered")
	} else {
		fmt.Fprintln(w, "SERVER\tSTATE\tNATIVE\tTRANSPORT\tSOURCE\tRECOVERY")
		for _, server := range inv.result.Servers {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", cliText(server.ID),
				cliText(nativeMCPActivationLabel(inv, server)), cliText(nativeMCPEnabledLabel(server)),
				cliText(valueOrDash(string(server.Transport))), cliText(valueOrDash(server.Provenance.RealPath)),
				cliText(valueOrDash(inv.recoveryToken(server))))
		}
		for _, reference := range stale {
			fmt.Fprintf(w, "%s\tsaved-unavailable\tunknown\t-\t%s\t%s\n", cliText(reference.ID), cliText(reference.RealPath), cliText(reference.RecoveryToken))
		}
	}
	for _, note := range inv.notes {
		fmt.Fprintf(w, "%s: %s\n", cliText(note.level), cliText(note.text))
	}
}

func (inv *nativeMCPInventory) recoveryToken(server mcpnative.Server) string {
	if inv == nil || inv.state == nil {
		return ""
	}
	identity := mcpnative.ActivationIdentity{
		ID: server.ID, RealPath: server.Provenance.RealPath, TrustRoot: server.TrustRoot,
	}
	want := nativeMCPActivationKey(identity)
	for _, reference := range inv.state.references(inv.workspace) {
		candidate := mcpnative.ActivationIdentity{
			ID: reference.ID, RealPath: reference.RealPath, TrustRoot: reference.TrustRoot,
		}
		if nativeMCPActivationKey(candidate) == want {
			return reference.RecoveryToken
		}
	}
	return ""
}

func (inv *nativeMCPInventory) staleActivationReferences() []nativeMCPActivationReference {
	if inv == nil || inv.state == nil {
		return nil
	}
	represented := make(map[string]struct{}, len(inv.result.Servers))
	for _, server := range inv.result.Servers {
		identity := mcpnative.ActivationIdentity{
			ID: server.ID, RealPath: server.Provenance.RealPath, TrustRoot: server.TrustRoot,
		}
		represented[nativeMCPActivationKey(identity)] = struct{}{}
	}
	var stale []nativeMCPActivationReference
	for _, reference := range inv.state.references(inv.workspace) {
		identity := mcpnative.ActivationIdentity{
			ID: reference.ID, RealPath: reference.RealPath, TrustRoot: reference.TrustRoot,
		}
		if _, ok := represented[nativeMCPActivationKey(identity)]; !ok {
			stale = append(stale, reference)
		}
	}
	return stale
}

func writeNativeMCPInspect(w io.Writer, inv *nativeMCPInventory, server mcpnative.Server) {
	fmt.Fprintf(w, "server: %s\nname: %s\ndialect: %s\nscope: %s\nsource: %s\npath: %s\n",
		cliText(server.ID), cliText(server.Name), cliText(string(server.Provenance.Dialect)), cliText(string(server.Provenance.Scope)),
		cliText(string(server.Provenance.Source)), cliText(server.Provenance.RealPath))
	fmt.Fprintf(w, "state: %s\nnative: %s\nsupported: %t\ntransport: %s\nrequired: %t\n",
		cliText(nativeMCPActivationLabel(inv, server)), cliText(nativeMCPEnabledLabel(server)), server.Supported,
		cliText(valueOrDash(string(server.Transport))), server.Required)
	if server.ExecutionTrustRequired {
		fmt.Fprintf(w, "workspace trust: required for %s\n", cliText(server.TrustRoot))
	} else {
		fmt.Fprintln(w, "workspace trust: not required")
	}
	features := server.RequiredFeatures()
	if len(features) == 0 {
		fmt.Fprintln(w, "runtime features: baseline")
	} else {
		parts := make([]string, len(features))
		for i, feature := range features {
			parts[i] = string(feature)
		}
		sort.Strings(parts)
		fmt.Fprintf(w, "runtime features: %s\n", cliText(strings.Join(parts, ", ")))
	}
	if len(server.UnsupportedFields) > 0 {
		fmt.Fprintf(w, "unsupported fields: %s\n", cliText(strings.Join(server.UnsupportedFields, ", ")))
	}
	if len(server.Env) > 0 {
		fmt.Fprintf(w, "environment names: %s\n", cliText(strings.Join(sortedNativeMCPMapKeys(server.Env), ", ")))
	}
	if len(server.Headers) > 0 {
		fmt.Fprintf(w, "header names: %s\n", cliText(strings.Join(sortedNativeMCPMapKeys(server.Headers), ", ")))
	}
	if len(server.HeaderEnv) > 0 {
		names := make([]string, 0, len(server.HeaderEnv))
		for name := range server.HeaderEnv {
			names = append(names, name)
		}
		sort.Strings(names)
		fmt.Fprintf(w, "environment-backed headers: %s\n", cliText(strings.Join(names, ", ")))
	}
}

func nativeMCPActivationLabel(inv *nativeMCPInventory, server mcpnative.Server) string {
	if !server.Supported {
		return "unsupported"
	}
	request, err := inv.result.ActivationRequest(server.ID)
	if err != nil {
		if !server.Enabled {
			return "native-disabled"
		}
		return "unavailable"
	}
	status := inv.state.status(request)
	if status.Changed {
		return "changed"
	}
	if status.Enabled {
		return "enabled"
	}
	return "disabled"
}

func nativeMCPEnabledLabel(server mcpnative.Server) string {
	if server.Enabled {
		return "enabled"
	}
	return "disabled"
}

func sortedNativeMCPMapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
