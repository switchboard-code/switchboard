package main

// Native Codex MCP execution needs the same effective configuration view as
// Codex itself. Reading a subset of TOML files cannot account for package,
// system, managed, cloud, profile, project, and session layers. This adapter
// asks an installed Codex app-server for its bounded read-only snapshots, and
// is invoked only after a Switchboard activation requires Codex semantics (or
// by the explicit `sb mcp` command).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/mcpnative"
	"github.com/switchboard-code/switchboard/internal/safeexec"
)

const (
	codexSnapshotTimeout   = 8 * time.Second
	codexSnapshotMaxOutput = 8 << 20
	codexSnapshotMessages  = 128
)

type codexAppServerSnapshot struct {
	config              *mcpnative.CodexSnapshot
	requirementsChecked bool
}

type codexAppServerResponse struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func readCodexAppServerSnapshot(parent context.Context, workspace string) (codexAppServerSnapshot, error) {
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return codexAppServerSnapshot{}, errors.New("Codex snapshot workspace cannot be resolved")
	}
	workspace, err = filepath.EvalSymlinks(workspace)
	if err != nil {
		return codexAppServerSnapshot{}, errors.New("Codex snapshot workspace cannot be canonicalized")
	}
	executable, err := trustedCodexExecutable(workspace)
	if err != nil {
		return codexAppServerSnapshot{}, err
	}

	ctx, cancel := context.WithTimeout(parent, codexSnapshotTimeout)
	defer cancel()
	command, err := executable.CommandContext(ctx, "app-server", "--stdio")
	if err != nil {
		return codexAppServerSnapshot{}, errors.New("Codex executable changed before app-server startup")
	}
	command.Env, err = codexAppServerEnvironment(workspace)
	if err != nil {
		return codexAppServerSnapshot{}, err
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		return codexAppServerSnapshot{}, errors.New("Codex app-server input is unavailable")
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return codexAppServerSnapshot{}, errors.New("Codex app-server output is unavailable")
	}
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		return codexAppServerSnapshot{}, errors.New("Codex app-server could not start")
	}
	defer func() {
		_ = stdin.Close()
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		_ = command.Wait()
	}()

	encoder := json.NewEncoder(stdin)
	decoder := json.NewDecoder(io.LimitReader(stdout, codexSnapshotMaxOutput+1))
	if err := encoder.Encode(map[string]any{
		"id": 1, "method": "initialize",
		"params": map[string]any{
			"clientInfo":   map[string]any{"name": "switchboard", "version": version},
			"capabilities": map[string]any{"experimentalApi": true},
		},
	}); err != nil {
		return codexAppServerSnapshot{}, errors.New("Codex app-server initialization could not be sent")
	}
	if _, err := readCodexAppServerResult(decoder, 1); err != nil {
		return codexAppServerSnapshot{}, err
	}
	if err := encoder.Encode(map[string]any{
		"id": 2, "method": "config/read",
		"params": map[string]any{"cwd": workspace, "includeLayers": true},
	}); err != nil {
		return codexAppServerSnapshot{}, errors.New("Codex config/read could not be sent")
	}
	configResult, err := readCodexAppServerResult(decoder, 2)
	if err != nil {
		return codexAppServerSnapshot{}, err
	}
	if err := encoder.Encode(map[string]any{"id": 3, "method": "configRequirements/read"}); err != nil {
		return codexAppServerSnapshot{}, errors.New("Codex configRequirements/read could not be sent")
	}
	requirementsResult, err := readCodexAppServerResult(decoder, 3)
	if err != nil {
		return codexAppServerSnapshot{}, err
	}

	snapshot, err := mcpnative.NewCodexSnapshot(configResult, workspace)
	if err != nil {
		return codexAppServerSnapshot{}, errors.New("Codex returned an invalid effective configuration snapshot")
	}
	checked, err := codexRequirementsAbsent(requirementsResult)
	if err != nil {
		return codexAppServerSnapshot{}, err
	}
	return codexAppServerSnapshot{config: snapshot, requirementsChecked: checked}, nil
}

func readCodexAppServerResult(decoder *json.Decoder, wantID int) (json.RawMessage, error) {
	for count := 0; count < codexSnapshotMessages; count++ {
		var response codexAppServerResponse
		if err := decoder.Decode(&response); err != nil {
			return nil, errors.New("Codex app-server closed before returning its snapshot")
		}
		if string(response.ID) != fmt.Sprint(wantID) {
			continue
		}
		if len(response.Error) != 0 && string(response.Error) != "null" {
			return nil, fmt.Errorf("Codex app-server request %d failed", wantID)
		}
		if len(response.Result) == 0 || string(response.Result) == "null" {
			return nil, fmt.Errorf("Codex app-server request %d returned no result", wantID)
		}
		return append(json.RawMessage(nil), response.Result...), nil
	}
	return nil, errors.New("Codex app-server emitted too many messages before its snapshot")
}

func codexRequirementsAbsent(raw []byte) (bool, error) {
	if len(raw) == 0 || len(raw) > mcpnative.MaxTotalConfigBytes {
		return false, errors.New("Codex returned an invalid requirements snapshot")
	}
	if err := rejectNativeMCPDuplicateJSONKeys(raw); err != nil {
		return false, errors.New("Codex returned an invalid requirements snapshot")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || len(object) != 1 {
		return false, errors.New("Codex returned an invalid requirements snapshot")
	}
	requirements, ok := object["requirements"]
	if !ok {
		return false, errors.New("Codex returned an invalid requirements snapshot")
	}
	// Null is an authoritative app-server statement that no requirements are
	// configured. Any non-null requirements object remains fail-closed until
	// Switchboard supports its exact MCP policy projection.
	return string(requirements) == "null", nil
}

func trustedCodexExecutable(workspace string) (safeexec.Executable, error) {
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return safeexec.Executable{}, errors.New("Codex workspace path cannot be resolved")
	}
	workspace, err = filepath.EvalSymlinks(workspace)
	if err != nil {
		return safeexec.Executable{}, errors.New("Codex workspace path cannot be canonicalized")
	}
	roots, err := safeexec.WorkspaceAndCurrentAuthorityRoots(workspace)
	if err != nil {
		return safeexec.Executable{}, errors.New("Codex workspace authority cannot be established")
	}
	executable, err := safeexec.ResolveOutside("codex", roots...)
	if err != nil {
		if errors.Is(err, safeexec.ErrUntrustedPath) {
			return safeexec.Executable{}, errors.New("refusing to execute a workspace-local Codex binary for native MCP discovery")
		}
		return safeexec.Executable{}, errors.New("Codex executable is unavailable at a stable path outside workspace authority")
	}
	return executable, nil
}

func codexAppServerEnvironment(workspace string) ([]string, error) {
	roots, err := safeexec.WorkspaceAndCurrentAuthorityRoots(workspace)
	if err != nil {
		return nil, errors.New("Codex workspace authority cannot be established")
	}
	environ, err := safeexec.FilterEnvironmentPath(execution.ScrubbedChildEnv(), roots...)
	if err != nil {
		return nil, errors.New("Codex app-server has no trusted interpreter search path outside workspace authority")
	}
	return environ, nil
}
