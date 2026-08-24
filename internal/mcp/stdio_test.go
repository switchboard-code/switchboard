package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain doubles as the MCP server the stdio tests spawn: the test binary
// re-executes itself with SB_MCP_STDIO_HELPER set and becomes a real child
// process speaking real JSON-RPC over real pipes. The transport is exercised
// against an actual subprocess, not a description of one, per the testdata
// rule: wire behavior gets captured where it happens.
func TestMain(m *testing.M) {
	if os.Getenv("SB_MCP_DESCENDANT_HELPER") == "1" {
		for {
			time.Sleep(time.Second)
		}
	}
	if os.Getenv("SB_MCP_STDIO_HELPER") == "1" {
		runHelperServer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runHelperServer() {
	recordHelperStart()
	spawnHelperDescendant()
	if os.Getenv("SB_MCP_STDIO_MODE") == "oversized-output" {
		_, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), 128<<10))
		for {
			time.Sleep(time.Second)
		}
	}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 64<<10), 1<<20)
	out := bufio.NewWriter(os.Stdout)
	reply := func(id int64, result string) {
		fmt.Fprintf(out, `{"jsonrpc":"2.0","id":%d,"result":%s}`+"\n", id, result)
		out.Flush()
	}
	for in.Scan() {
		var req struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(in.Bytes(), &req) != nil || req.ID == nil {
			continue
		}
		if os.Getenv("SB_MCP_STDIO_MODE") == "stderr-secret-exit" {
			fmt.Fprintln(os.Stderr, os.Getenv("SB_TEST_LOG_SECRET"))
			return
		}
		switch req.Method {
		case "server/discover":
			if os.Getenv("SB_MCP_STDIO_MODE") == "secret-result-type" {
				result, _ := json.Marshal(map[string]any{
					"resultType":        os.Getenv("SB_TEST_RESULT_SECRET"),
					"supportedVersions": []string{modernProtocolVersion},
				})
				reply(*req.ID, string(result))
				continue
			}
			if os.Getenv("SB_MCP_STDIO_MODE") == "exit-on-discover" {
				return
			}
			if os.Getenv("SB_MCP_STDIO_MODE") == "hang-on-discover" {
				for {
					time.Sleep(time.Second)
				}
			}
			reply(*req.ID, `{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{"tools":{}},"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"helper","version":"0.0"}}}`)
		case "initialize":
			reply(*req.ID, `{"protocolVersion":"2025-06-18","serverInfo":{"name":"helper","version":"0.0"}}`)
		case "tools/list":
			if os.Getenv("SB_MCP_STDIO_MODE") == "hang-on-tools-list" {
				for {
					time.Sleep(time.Second)
				}
			}
			reply(*req.ID, `{"resultType":"complete","tools":[{"name":"env","description":"reports selected environment variables","inputSchema":{"type":"object"}}]}`)
		case "tools/call":
			cwd, _ := os.Getwd()
			cwdInfo, cwdErr := os.Stat(cwd)
			expectedCWDInfo, expectedCWDErr := os.Stat(os.Getenv("SB_EXPECT_CWD"))
			cwdMatch := cwdErr == nil && expectedCWDErr == nil && os.SameFile(cwdInfo, expectedCWDInfo)
			seen := fmt.Sprintf("anthropic=%q sb=%q token=%q secret=%q password=%q credential=%q sshsock=%q databaseurl=%q regranted=%q auth=%q sessionid=%q cookie=%q harmless=%q safe=%q extra=%q forwarded=%q override=%q tmpsecret=%q cwd=%q cwdmatch=%t",
				os.Getenv("ANTHROPIC_API_KEY"), os.Getenv("SB_OLLAMA_API_KEY"),
				os.Getenv("INHERITED_MODEL_TOKEN"), os.Getenv("mIxEd_SeCrEt"),
				os.Getenv("DB_PASSWORD"), os.Getenv("CLIENT_CREDENTIAL"),
				os.Getenv("SSH_AUTH_SOCK"), os.Getenv("DATABASE_URL"),
				os.Getenv("MODEL_TOKEN"), os.Getenv("AUTH"), os.Getenv("SESSION_ID"), os.Getenv("COOKIE"), os.Getenv("HARMLESS_VALUE"),
				os.Getenv("SAFE_VISIBLE"), os.Getenv("SB_TEST_EXTRA"),
				os.Getenv("FORWARDED_TOKEN"), os.Getenv("STATIC_OVERRIDE"), os.Getenv("TMP_SECRET"), cwd, cwdMatch)
			body, _ := json.Marshal(seen)
			reply(*req.ID, fmt.Sprintf(`{"resultType":"complete","content":[{"type":"text","text":%s}]}`, body))
		}
	}
}

func recordHelperStart() {
	path := os.Getenv("SB_MCP_START_COUNTER")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(f, os.Getpid())
	_ = f.Close()
}

func spawnHelperDescendant() {
	path := os.Getenv("SB_MCP_DESCENDANT_PID_FILE")
	if path == "" {
		return
	}
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "SB_MCP_STDIO_HELPER=", "SB_MCP_DESCENDANT_HELPER=1")
	if cmd.Start() != nil {
		return
	}
	_ = os.WriteFile(path, []byte(fmt.Sprintf("%d", cmd.Process.Pid)), 0o600)
}

func TestStdioEndToEndFiltersCredentials(t *testing.T) {
	// These are the parent's model credentials; the child must not see them.
	t.Setenv("ANTHROPIC_API_KEY", "parent-anthropic-secret")
	t.Setenv("SB_OLLAMA_API_KEY", "parent-sb-secret")
	t.Setenv("INHERITED_MODEL_TOKEN", "parent-model-token")
	t.Setenv("mIxEd_SeCrEt", "parent-mixed-secret")
	t.Setenv("DB_PASSWORD", "parent-password")
	t.Setenv("CLIENT_CREDENTIAL", "parent-credential")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/parent-ssh-agent.sock")
	t.Setenv("DATABASE_URL", "postgres://user:password@database.invalid/app")
	t.Setenv("MODEL_TOKEN", "parent-token-to-replace")
	t.Setenv("SAFE_VISIBLE", "ambient-safe-value")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c, err := Connect(ctx, Spec{
		Name:    "helper",
		Command: os.Args[0],
		Env: map[string]string{
			"SB_MCP_STDIO_HELPER": "1",
			"SB_TEST_EXTRA":       "explicitly-granted",
			"MODEL_TOKEN":         "explicitly-granted-token",
			"AUTH":                "explicit-auth-credential",
			"SESSION_ID":          "explicit-session-credential",
			"COOKIE":              "explicit-cookie-credential",
			"HARMLESS_VALUE":      "ordinary-config-value",
			"DATABASE_URL":        "postgres://user:password@database.invalid/app",
		},
	}, func(string, string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	tools := c.Tools()
	if len(tools) != 1 || tools[0].Name != "env" {
		t.Fatalf("tools = %+v", tools)
	}

	res, err := c.Call(ctx, "env", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		"parent-anthropic-secret",
		"parent-sb-secret",
		"parent-model-token",
		"parent-mixed-secret",
		"parent-password",
		"parent-credential",
		"parent-token-to-replace", "/tmp/parent-ssh-agent.sock", "postgres://user:password@database.invalid/app",
	} {
		if strings.Contains(res.Content, secret) {
			t.Errorf("the server saw inherited credential %q: %s", secret, res.Content)
		}
	}
	if !strings.Contains(res.Content, `anthropic=""`) || !strings.Contains(res.Content, `sb=""`) ||
		!strings.Contains(res.Content, `token=""`) || !strings.Contains(res.Content, `secret=""`) ||
		!strings.Contains(res.Content, `password=""`) || !strings.Contains(res.Content, `credential=""`) ||
		!strings.Contains(res.Content, `sshsock=""`) {
		t.Errorf("sensitive inherited variables should read empty in the child: %s", res.Content)
	}
	if !strings.Contains(res.Content, `databaseurl="[redacted]"`) {
		t.Errorf("explicit credential URL reached the child but was not redacted from its successful result: %s", res.Content)
	}
	if strings.Contains(res.Content, "explicitly-granted-token") || !strings.Contains(res.Content, `regranted="[redacted]"`) {
		t.Errorf("an explicitly re-granted credential must reach the child but be redacted from its result: %s", res.Content)
	}
	for _, field := range []string{"auth", "sessionid", "cookie"} {
		if !strings.Contains(res.Content, field+`="[redacted]"`) {
			t.Errorf("credential-shaped %s env was not redacted from a successful result: %s", field, res.Content)
		}
	}
	for _, credential := range []string{"explicit-auth-credential", "explicit-session-credential", "explicit-cookie-credential"} {
		if strings.Contains(res.Content, credential) {
			t.Errorf("successful result leaked %q: %s", credential, res.Content)
		}
	}
	if !strings.Contains(res.Content, `harmless="ordinary-config-value"`) {
		t.Errorf("non-credential env was falsely redacted: %s", res.Content)
	}
	if !strings.Contains(res.Content, `safe="ambient-safe-value"`) {
		t.Errorf("non-sensitive inherited variables must remain available: %s", res.Content)
	}
	if !strings.Contains(res.Content, `extra="explicitly-granted"`) {
		t.Errorf("a non-credential configured value must remain usable as tool data: %s", res.Content)
	}
}

func TestSensitiveEnvNameIsCaseInsensitive(t *testing.T) {
	for _, name := range []string{
		"API_KEY",
		"github_token",
		"ClientSecret",
		"Db_PaSsWoRd",
		"service_CREDENTIAL",
		"AUTH",
		"SESSION_ID",
		"COOKIE",
		"SSH_AUTH_SOCK",
		"DATABASE_URL",
		"SERVICE_DSN",
	} {
		if !sensitiveEnvName(name) {
			t.Errorf("sensitiveEnvName(%q) = false", name)
		}
	}
	for _, name := range []string{"PATH", "HOME", "SAFE_VISIBLE"} {
		if sensitiveEnvName(name) {
			t.Errorf("sensitiveEnvName(%q) = true", name)
		}
	}
}

func TestCredentialBearingNameIsTokenAware(t *testing.T) {
	for _, name := range []string{"AUTH", "SESSION_ID", "COOKIE", "X-API-Key", "ClientSecret", "AWS_ACCESS_KEY_ID", "SSH_AUTH_SOCK", "DATABASE_URL", "SERVICE_DSN", "REDIS_URL"} {
		if !credentialBearingName(name) {
			t.Errorf("credentialBearingName(%q) = false", name)
		}
	}
	for _, name := range []string{"X-Monkey", "X-Hockey-Team", "HARMLESS_VALUE", "Sessional-Mode"} {
		if credentialBearingName(name) {
			t.Errorf("credentialBearingName(%q) = true", name)
		}
	}
}

func TestRestrictedServerEnvUsesBaselineAndExplicitGrants(t *testing.T) {
	t.Setenv("SAFE_VISIBLE", "ambient-safe")
	t.Setenv("FORWARDED_TOKEN", "explicit-sensitive-inherit")
	t.Setenv("STATIC_OVERRIDE", "ambient-value")
	t.Setenv("TMPDIR", "/tmp/switchboard-test")
	t.Setenv("TMP_SECRET", "must-not-pass")

	env := environmentMap(serverEnv(Spec{
		RestrictedEnv: true,
		InheritEnv:    []string{"FORWARDED_TOKEN", "FORWARDED_TOKEN"},
		Env:           map[string]string{"STATIC_OVERRIDE": "static-value"},
	}))
	if env["SAFE_VISIBLE"] != "" {
		t.Fatalf("restricted environment inherited arbitrary host state: %q", env["SAFE_VISIBLE"])
	}
	if env["FORWARDED_TOKEN"] != "explicit-sensitive-inherit" {
		t.Fatalf("explicit inherited credential = %q", env["FORWARDED_TOKEN"])
	}
	if env["STATIC_OVERRIDE"] != "static-value" {
		t.Fatalf("static override = %q", env["STATIC_OVERRIDE"])
	}
	if env["TMPDIR"] != "/tmp/switchboard-test" {
		t.Fatalf("baseline TMPDIR = %q", env["TMPDIR"])
	}
	if env["TMP_SECRET"] != "" {
		t.Fatalf("secret-shaped baseline variable leaked: %q", env["TMP_SECRET"])
	}
	if runtime.GOOS == "windows" {
		for _, name := range []string{"USERPROFILE", "HOMEDRIVE", "HOMEPATH", "APPDATA", "LOCALAPPDATA"} {
			value := "switchboard-" + strings.ToLower(name)
			t.Setenv(name, value)
			if got := environmentMap(serverEnv(Spec{RestrictedEnv: true}))[name]; got != value {
				t.Fatalf("Windows baseline %s = %q, want %q", name, got, value)
			}
		}
	}
}

func TestLegacyServerEnvKeepsNonSensitiveAmbientCompatibility(t *testing.T) {
	t.Setenv("SAFE_VISIBLE", "ambient-safe")
	t.Setenv("MODEL_TOKEN", "must-not-pass")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/ssh-agent.sock")
	t.Setenv("DATABASE_URL", "postgres://user:password@database.invalid/app")
	t.Setenv("SERVICE_DSN", "service://credential")
	env := environmentMap(serverEnv(Spec{}))
	if env["SAFE_VISIBLE"] != "ambient-safe" {
		t.Fatalf("legacy environment lost safe ambient variable: %q", env["SAFE_VISIBLE"])
	}
	if env["MODEL_TOKEN"] != "" {
		t.Fatalf("legacy environment leaked credential: %q", env["MODEL_TOKEN"])
	}
	for _, name := range []string{"SSH_AUTH_SOCK", "DATABASE_URL", "SERVICE_DSN"} {
		if env[name] != "" {
			t.Fatalf("legacy environment leaked %s capability/credential: %q", name, env[name])
		}
	}
}

func environmentMap(entries []string) map[string]string {
	values := make(map[string]string, len(entries))
	for _, entry := range entries {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			values[name] = value
		}
	}
	return values
}

func TestStdioUsesConfiguredWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	canonicalDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Connect(ctx, Spec{
		Name:          "cwd-helper",
		Command:       os.Args[0],
		CWD:           dir,
		RestrictedEnv: true,
		Env: map[string]string{
			"SB_MCP_STDIO_HELPER": "1",
			"SB_EXPECT_CWD":       canonicalDir,
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	result, err := c.Call(ctx, "env", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "cwdmatch=true") {
		t.Fatalf("server cwd was not applied: %s", result.Content)
	}
}

func TestModernStdioProbeReusesTheLiveProcess(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "starts")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Connect(ctx, Spec{
		Name:    "single-start-helper",
		Command: os.Args[0],
		Env: map[string]string{
			"SB_MCP_STDIO_HELPER":  "1",
			"SB_MCP_START_COUNTER": counter,
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	data, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if starts := len(strings.Fields(string(data))); starts != 1 {
		t.Fatalf("stdio server starts = %d, want one modern process", starts)
	}
}

func TestStdioErrorsRedactConfiguredSecrets(t *testing.T) {
	const secret = "stdio-secret-that-must-not-leak"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Connect(ctx, Spec{
		Name:    "stderr-helper",
		Command: os.Args[0],
		Env: map[string]string{
			"SB_MCP_STDIO_HELPER": "1",
			"SB_MCP_STDIO_MODE":   "stderr-secret-exit",
			"SB_TEST_LOG_SECRET":  secret,
		},
	}, nil)
	if err == nil {
		t.Fatal("Connect unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("stdio error leaked configured secret: %v", err)
	}
}

func TestStdioResultDerivedErrorsRedactConfiguredSecrets(t *testing.T) {
	const secret = "stdio-result-derived-secret"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Connect(ctx, Spec{
		Name:    "stdio-result-redaction",
		Command: os.Args[0],
		Env: map[string]string{
			"SB_MCP_STDIO_HELPER":   "1",
			"SB_MCP_STDIO_MODE":     "secret-result-type",
			"SB_TEST_RESULT_SECRET": secret,
		},
	}, nil)
	if err == nil {
		t.Fatal("Connect unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("stdio derived error leaked configured secret: %v", err)
	}
}

type observedBlockingWriter struct {
	writer  *io.PipeWriter
	started chan struct{}
	once    sync.Once
}

func (w *observedBlockingWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	return w.writer.Write(p)
}

func (w *observedBlockingWriter) Close() error { return w.writer.Close() }

func TestStdioSendCancellationInterruptsBlockedWrite(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	blocking := &observedBlockingWriter{writer: writer, started: make(chan struct{})}
	transport := &stdioTransport{
		stdin:     blocking,
		writeGate: make(chan struct{}, 1),
	}
	transport.writeGate <- struct{}{}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- transport.Send(ctx, make([]byte, 1<<20))
	}()
	select {
	case <-blocking.started:
	case <-time.After(time.Second):
		t.Fatal("stdio write did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Send error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Send remained blocked after context cancellation")
	}
	if err := transport.aborted(); !errors.Is(err, context.Canceled) {
		t.Fatalf("transport abort error = %v, want context.Canceled", err)
	}
}

func TestStdioSendCancellationWhileWaitingForWriter(t *testing.T) {
	transport := &stdioTransport{writeGate: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- transport.Send(ctx, nil) }()
	select {
	case err := <-result:
		t.Fatalf("Send returned before the writer gate was released or canceled: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Send error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Send remained blocked on the writer gate after cancellation")
	}
}

func TestStdioCloseTerminatesTheServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c, err := Connect(ctx, Spec{
		Name:    "helper",
		Command: os.Args[0],
		Env:     map[string]string{"SB_MCP_STDIO_HELPER": "1"},
	}, func(string, string) {})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		c.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return; the child was not reaped")
	}
}

func TestStdioServerExitDuringProbeDoesNotPoisonLegacySession(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "starts")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c, err := Connect(ctx, Spec{
		Name:    "legacy-helper",
		Command: os.Args[0],
		Env: map[string]string{
			"SB_MCP_STDIO_HELPER":  "1",
			"SB_MCP_STDIO_MODE":    "exit-on-discover",
			"SB_MCP_START_COUNTER": counter,
		},
	}, func(string, string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if c.protocol != legacyProtocolVersion {
		t.Fatalf("protocol = %q, want legacy fallback %q", c.protocol, legacyProtocolVersion)
	}
	tools := c.Tools()
	if len(tools) != 1 || tools[0].Name != "env" {
		t.Fatalf("tools = %+v; the real legacy session did not survive the disposable probe", tools)
	}
	data, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if starts := len(strings.Fields(string(data))); starts != 2 {
		t.Fatalf("stdio server starts = %d, want isolated probe plus legacy session", starts)
	}
}

func TestFatalStdioReadClosesAndReapsProcess(t *testing.T) {
	tr, err := startStdio(Spec{
		Name:    "fatal-output-helper",
		Command: os.Args[0],
		Env: map[string]string{
			"SB_MCP_STDIO_HELPER": "1",
			"SB_MCP_STDIO_MODE":   "oversized-output",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	tr.maxMessageBytes = 1 << 10
	c := newClient(Spec{Name: "fatal-output-helper"}, tr, nil)
	select {
	case <-tr.closeDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("fatal read did not close/reap process: client error=%v", c.Err())
	}
	if c.Err() == nil || tr.cmd.ProcessState == nil {
		t.Fatalf("fatal read did not publish failure and reap: client error=%v state=%v", c.Err(), tr.cmd.ProcessState)
	}
	if tr.cmd.ProcessState.Success() {
		t.Fatalf("oversized-output process unexpectedly exited successfully: %v", tr.cmd.ProcessState)
	}
}

func TestStdioRecvEnforcesLimitWhenNewlineFitsReaderBuffer(t *testing.T) {
	const limit = 16
	transport := func(body string) *stdioTransport {
		return &stdioTransport{
			stdout:          bufio.NewReaderSize(strings.NewReader(body), 64<<10),
			maxMessageBytes: limit,
		}
	}

	exact := strings.Repeat("x", limit)
	got, err := transport(exact + "\n").Recv()
	if err != nil || string(got) != exact {
		t.Fatalf("exact limit: got %q, err %v", got, err)
	}
	if _, err := transport(exact + "x\n").Recv(); err == nil || !strings.Contains(err.Error(), "exceeds 16 bytes") {
		t.Fatalf("one byte over: err = %v, want bounded refusal", err)
	}
}

func TestConnectRejectsAMissingCommand(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Connect(ctx, Spec{Name: "ghost", Command: "definitely-not-a-real-binary-xyz"}, nil)
	if err == nil {
		t.Fatal("connecting to a nonexistent command must fail, not hang")
	}
}

func TestStdioProbeCancellationKillsDisposableChildPromptly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := Connect(ctx, Spec{
		Name:    "hanging-probe",
		Command: os.Args[0],
		Env: map[string]string{
			"SB_MCP_STDIO_HELPER": "1",
			"SB_MCP_STDIO_MODE":   "hang-on-discover",
		},
	}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Connect error = %v, want context deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("canceled disposable probe took %v; child was not killed promptly", elapsed)
	}
}

func TestConfiguredStartupTimeoutBoundsStdioProbe(t *testing.T) {
	started := time.Now()
	_, err := Connect(context.Background(), Spec{
		Name:           "configured-hanging-probe",
		Command:        os.Args[0],
		StartupTimeout: 100 * time.Millisecond,
		Env: map[string]string{
			"SB_MCP_STDIO_HELPER": "1",
			"SB_MCP_STDIO_MODE":   "hang-on-discover",
		},
	}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Connect error = %v, want configured startup deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("configured startup timeout took %v", elapsed)
	}
}

func TestConfiguredStartupTimeoutBoundsRealStdioSession(t *testing.T) {
	started := time.Now()
	_, err := Connect(context.Background(), Spec{
		Name:           "configured-hanging-list",
		Command:        os.Args[0],
		StartupTimeout: 150 * time.Millisecond,
		Env: map[string]string{
			"SB_MCP_STDIO_HELPER": "1",
			"SB_MCP_STDIO_MODE":   "hang-on-tools-list",
		},
	}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Connect error = %v, want configured startup deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("real-session startup timeout took %v", elapsed)
	}
}

func TestCanceledConnectDoesNotTryToSpawn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Connect(ctx, Spec{Name: "canceled", Command: "definitely-not-a-real-binary-xyz"}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Connect error = %v, want context.Canceled before process spawn", err)
	}
}
