package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/switchboard-code/switchboard/internal/safeexec"
)

const (
	codexCredentialHelperCommand = "__credential-helper"
	codexCredentialHelperKind    = "codex-access-token"
	maxCodexAuthBytes            = int64(1 << 20)
	maxCodexAccessTokenBytes     = 64 << 10
)

// codexCredentialHelperArgv binds the helper to the binary the user is
// currently running. Looking up "sb" through PATH, or persisting a binary the
// checkout can later replace, would let workspace-controlled bytes become the
// credential reader on the next launch.
func codexCredentialHelperArgv(workspace string) ([]string, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, errors.New("the Switchboard executable path is unavailable")
	}
	return codexCredentialHelperArgvForPath(workspace, executable)
}

// codexCredentialHelperArgvForPath is split out for adversarial namespace
// tests. ResolvePathOutside checks both the lexical path and canonical target,
// including workspace-owned symlink ancestors, and binds the executable's
// identity while it is selected. The config stores only the resulting
// canonical path after that proof succeeds.
func codexCredentialHelperArgvForPath(workspace, executable string) ([]string, error) {
	if strings.TrimSpace(workspace) == "" {
		return nil, errors.New("the workspace path is unavailable for credential-helper trust validation")
	}
	roots, err := safeexec.WorkspaceAndCurrentAuthorityRoots(workspace)
	if err != nil {
		return nil, errors.New("the workspace authority cannot be established for credential-helper validation")
	}
	resolved, err := safeexec.ResolvePathOutside(executable, roots...)
	if err != nil {
		if errors.Is(err, safeexec.ErrUntrustedPath) {
			return nil, errors.New("refusing to persist a workspace-controlled Switchboard executable as a credential helper; install or run sb from outside the checkout")
		}
		return nil, errors.New("the Switchboard executable path cannot be bound safely")
	}
	return []string{resolved.Path(), codexCredentialHelperCommand, codexCredentialHelperKind}, nil
}

// runCredentialHelperDispatch is deliberately absent from help and shell
// completion. It exists only as stable argv persisted by /setup and accepts no
// general command, path, or secret argument.
func runCredentialHelperDispatch(w io.Writer, args []string) error {
	if len(args) != 1 || args[0] != codexCredentialHelperKind {
		return errors.New("invalid internal credential-helper invocation")
	}
	token, err := readCodexAccessToken()
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, token+"\n"); err != nil {
		return errors.New("writing the credential-helper result failed")
	}
	return nil
}

// readCodexAccessToken reads only the bounded private file rooted beneath the
// current user's home descriptor. The platform opener refuses symlinks and
// special files before this common path parses any bytes.
func readCodexAccessToken() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("the current user's home directory is unavailable")
	}
	file, err := openCodexAuthFile(home)
	if err != nil {
		return "", errors.New("the Codex login file is unavailable")
	}
	defer file.Close()

	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() {
		return "", errors.New("the Codex login file is not a regular file")
	}
	if before.Size() < 0 || before.Size() > maxCodexAuthBytes {
		return "", fmt.Errorf("the Codex login file exceeds the %d-byte limit", maxCodexAuthBytes)
	}
	private, err := codexAuthFileIsAcceptable(file)
	if err != nil {
		return "", errors.New("the Codex login file privacy could not be verified")
	}
	if !private {
		return "", errors.New("the Codex login file is not owned or protected for this user")
	}

	data, err := io.ReadAll(io.LimitReader(file, maxCodexAuthBytes+1))
	if err != nil {
		return "", errors.New("reading the Codex login file failed")
	}
	if int64(len(data)) > maxCodexAuthBytes {
		return "", fmt.Errorf("the Codex login file exceeds the %d-byte limit", maxCodexAuthBytes)
	}
	after, err := file.Stat()
	if err != nil || before.Size() != after.Size() || before.ModTime() != after.ModTime() || int64(len(data)) != after.Size() {
		return "", errors.New("the Codex login file changed while it was read")
	}

	var auth struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &auth); err != nil {
		return "", errors.New("the Codex login file is invalid JSON")
	}
	token := auth.Tokens.AccessToken
	if token == "" || len(token) > maxCodexAccessTokenBytes || token != strings.TrimSpace(token) || strings.IndexFunc(token, func(r rune) bool {
		return r < 0x20 || r == 0x7f
	}) >= 0 {
		return "", errors.New("the Codex login file has no valid access token")
	}
	return token, nil
}
