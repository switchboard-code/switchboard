package mcp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// maxLine bounds one JSON-RPC message. A server that emits more than this in
// a single line is treated as broken rather than buffered without limit.
const maxLine = 32 << 20

type stdioProcessTree interface {
	terminate() error
	close() error
}

// stdioTransport is a child process speaking newline-delimited JSON-RPC on
// its pipes. Stderr is drained into a bounded tail for diagnostics: a server
// that logs there must not block on a full pipe, and the last lines are the
// ones that explain a death.
type stdioTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	tree   stdioProcessTree

	maxMessageBytes int

	writeGate chan struct{}
	inputOnce sync.Once
	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error

	abortMu  sync.Mutex
	abortErr error

	stderrMu    sync.Mutex
	stderr      []string
	secrets     []string
	credentials []string
}

func startStdio(spec Spec) (*stdioTransport, error) {
	cmd := exec.Command(spec.Command, spec.Args...)
	cmd.Dir = spec.CWD
	cmd.Env = serverEnv(spec)
	configureStdioProcess(cmd)
	secrets := stdioSecretValues(spec, cmd.Env)
	credentials := stdioCredentialValues(spec, cmd.Env)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("preparing mcp server %s stdin failed", spec.Name)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("preparing mcp server %s stdout failed", spec.Name)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("preparing mcp server %s stderr failed", spec.Name)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting mcp server %s failed", spec.Name)
	}
	tree, err := attachStdioProcess(cmd)
	if err != nil {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("isolating mcp server %s process tree failed", spec.Name)
	}

	t := &stdioTransport{
		cmd:             cmd,
		stdin:           stdin,
		stdout:          bufio.NewReaderSize(stdout, 64<<10),
		tree:            tree,
		maxMessageBytes: maxLine,
		writeGate:       make(chan struct{}, 1),
		closeDone:       make(chan struct{}),
		secrets:         secrets,
		credentials:     credentials,
	}
	t.writeGate <- struct{}{}
	go t.drainStderr(stderr)
	return t, nil
}

func stdioSecretValues(spec Spec, childEnv []string) []string {
	secrets := append([]string{spec.Command, spec.CWD}, spec.Args...)
	explicit := make(map[string]struct{}, len(spec.Env)+len(spec.InheritEnv))
	for name := range spec.Env {
		explicit[environmentKey(name)] = struct{}{}
	}
	for _, name := range spec.InheritEnv {
		explicit[environmentKey(name)] = struct{}{}
	}
	for _, entry := range childEnv {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || value == "" {
			continue
		}
		if _, configured := explicit[environmentKey(name)]; configured {
			secrets = append(secrets, value)
		}
	}
	return secrets
}

func stdioCredentialValues(spec Spec, childEnv []string) []string {
	credentialNames := make(map[string]struct{}, len(spec.Env)+len(spec.InheritEnv))
	for name := range spec.Env {
		if credentialBearingName(name) {
			credentialNames[environmentKey(name)] = struct{}{}
		}
	}
	for _, name := range spec.InheritEnv {
		if credentialBearingName(name) {
			credentialNames[environmentKey(name)] = struct{}{}
		}
	}
	var credentials []string
	for _, entry := range childEnv {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || value == "" {
			continue
		}
		if _, credential := credentialNames[environmentKey(name)]; credential {
			credentials = append(credentials, value)
		}
	}
	return credentials
}

func (t *stdioTransport) secretValues() []string {
	return append([]string(nil), t.secrets...)
}

func (t *stdioTransport) credentialValues() []string {
	return append([]string(nil), t.credentials...)
}

// serverEnv always withholds ambient credential-shaped names. Legacy specs
// otherwise retain their ambient compatibility. Native specs opt into the
// restricted policy: a minimal launch baseline, exact inherited names, then
// static overrides. Either explicit path intentionally re-grants a secret.
func serverEnv(spec Spec) []string {
	type entry struct {
		name  string
		value string
	}
	values := make(map[string]entry)
	put := func(name, value string) {
		values[environmentKey(name)] = entry{name: name, value: value}
	}

	for _, kv := range os.Environ() {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || sensitiveEnvName(name) {
			continue
		}
		if spec.RestrictedEnv && !baselineEnvironmentName(name) {
			continue
		}
		put(name, value)
	}

	seenInherited := make(map[string]struct{}, len(spec.InheritEnv))
	for _, name := range spec.InheritEnv {
		key := environmentKey(name)
		if _, duplicate := seenInherited[key]; duplicate {
			continue
		}
		seenInherited[key] = struct{}{}
		if value, exists := os.LookupEnv(name); exists {
			put(name, value)
		}
	}
	for _, name := range sortedMapKeys(spec.Env) {
		put(name, spec.Env[name])
	}

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		item := values[key]
		env = append(env, item.name+"="+item.value)
	}
	return env
}

func environmentKey(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}

func baselineEnvironmentName(name string) bool {
	candidate := name
	if runtime.GOOS == "windows" {
		candidate = strings.ToUpper(name)
	}
	switch candidate {
	case "HOME", "LANG", "PATH":
		return true
	}
	if strings.HasPrefix(candidate, "LC_") || strings.HasPrefix(candidate, "TMP") {
		return true
	}
	if runtime.GOOS == "windows" {
		switch candidate {
		case "APPDATA", "COMSPEC", "HOMEDRIVE", "HOMEPATH", "LOCALAPPDATA", "PATHEXT", "SYSTEMROOT", "TEMP", "USERPROFILE":
			return true
		}
	}
	return false
}

// sensitiveEnvName deliberately errs toward withholding. MCP child processes
// run outside Switchboard's sandbox, and ambient credentials were entrusted to
// the host rather than every configured server. Matching is case-insensitive
// for Windows and for mixed-case names; a false positive can still be granted
// deliberately through Spec.Env, which serverEnv appends after filtering.
func sensitiveEnvName(name string) bool {
	if credentialBearingName(name) {
		return true
	}
	name = strings.ToLower(name)
	for _, marker := range [...]string{"secret", "token", "key", "password", "credential"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

func (t *stdioTransport) Send(ctx context.Context, msg []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.writeGate:
	}
	defer func() { t.writeGate <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := t.aborted(); err != nil {
		return err
	}

	cancelDone := make(chan struct{})
	stopCancel := context.AfterFunc(ctx, func() {
		t.abort(ctx.Err())
		close(cancelDone)
	})
	_, writeErr := t.stdin.Write(append(msg, '\n'))
	if !stopCancel() {
		<-cancelDone
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if writeErr != nil {
		return fmt.Errorf("writing to mcp server: %w", writeErr)
	}
	return nil
}

func (t *stdioTransport) abort(cause error) {
	if cause == nil {
		cause = errors.New("stdio transport aborted")
	}
	t.abortMu.Lock()
	if t.abortErr == nil {
		t.abortErr = cause
	}
	t.abortMu.Unlock()
	t.closeInput()
	t.terminateProcessTree()
}

func (t *stdioTransport) aborted() error {
	t.abortMu.Lock()
	defer t.abortMu.Unlock()
	if t.abortErr != nil {
		return fmt.Errorf("stdio transport aborted: %w", t.abortErr)
	}
	return nil
}

func (t *stdioTransport) closeInput() {
	t.inputOnce.Do(func() {
		if t.stdin != nil {
			_ = t.stdin.Close()
		}
	})
}

func (t *stdioTransport) Recv() ([]byte, error) {
	limit := t.maxMessageBytes
	if limit <= 0 {
		limit = maxLine
	}
	var buf bytes.Buffer
	for {
		chunk, err := t.stdout.ReadSlice('\n')
		buf.Write(chunk)
		if err == nil {
			raw := buf.Bytes()
			messageBytes := len(raw)
			if messageBytes > 0 && raw[messageBytes-1] == '\n' {
				messageBytes--
				if messageBytes > 0 && raw[messageBytes-1] == '\r' {
					messageBytes--
				}
			}
			if messageBytes > limit {
				return nil, fmt.Errorf("mcp message exceeds %d bytes", limit)
			}
			line := bytes.TrimSpace(raw[:messageBytes])
			if len(line) == 0 {
				buf.Reset()
				continue
			}
			return append([]byte(nil), line...), nil
		}
		if err == bufio.ErrBufferFull {
			if buf.Len() > limit {
				return nil, fmt.Errorf("mcp message exceeds %d bytes", limit)
			}
			continue
		}
		if tail := t.stderrTail(); tail != "" && err == io.EOF {
			return nil, fmt.Errorf("%w; server said: %s", err, tail)
		}
		return nil, err
	}
}

// Close ends the conversation politely and then definitively: stdin closes,
// which is the protocol's shutdown signal, and a server still alive shortly
// after is killed. The wait prevents zombie accumulation either way.
func (t *stdioTransport) Close() error {
	t.closeOnce.Do(func() {
		if t.closeDone != nil {
			defer close(t.closeDone)
		}
		t.closeInput()
		if t.cmd == nil {
			t.closeProcessTree()
			return
		}

		done := make(chan error, 1)
		go func() { done <- t.cmd.Wait() }()
		select {
		case t.closeErr = <-done:
		case <-time.After(3 * time.Second):
			t.terminateProcessTree()
			t.closeErr = <-done
		}
		// A server can exit while leaving children behind. The direct Wait only
		// reaps that server, so terminate the process group/job before releasing
		// its controller even after an apparently graceful shutdown.
		t.terminateProcessTree()
		t.closeProcessTree()
	})
	return t.closeErr
}

func (t *stdioTransport) terminateProcessTree() {
	if t.tree != nil {
		if err := t.tree.terminate(); err == nil {
			return
		}
	}
	if t.cmd != nil && t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
}

func (t *stdioTransport) closeProcessTree() {
	if t.tree != nil {
		_ = t.tree.close()
	}
}

func (t *stdioTransport) drainStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 8<<10), 64<<10)
	for sc.Scan() {
		t.stderrMu.Lock()
		t.stderr = append(t.stderr, sc.Text())
		if len(t.stderr) > 20 {
			t.stderr = t.stderr[len(t.stderr)-20:]
		}
		t.stderrMu.Unlock()
	}
}

func (t *stdioTransport) stderrTail() string {
	t.stderrMu.Lock()
	defer t.stderrMu.Unlock()
	if len(t.stderr) == 0 {
		return ""
	}
	n := len(t.stderr)
	if n > 3 {
		n = 3
	}
	return redactSecrets(strings.Join(t.stderr[len(t.stderr)-n:], " | "), t.secrets)
}
