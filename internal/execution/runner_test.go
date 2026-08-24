//go:build unix

package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunArgv(t *testing.T) {
	res, err := Run(context.Background(), Command{Argv: []string{"echo", "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Output) != "hello" {
		t.Errorf("output = %q", res.Output)
	}
	if res.ExitCode != 0 || res.Truncated || res.TimedOut {
		t.Errorf("result = %+v", res)
	}
}

func TestRunCapturesFailureOutput(t *testing.T) {
	res, err := Run(context.Background(), Command{
		Argv:  []string{"sh -c 'echo to-stdout; echo to-stderr >&2; exit 3'"},
		Shell: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
	// The model needs both streams interleaved: a build writes its diagnosis to
	// stderr and its progress to stdout, and either alone is misleading.
	if !strings.Contains(res.Output, "to-stdout") || !strings.Contains(res.Output, "to-stderr") {
		t.Errorf("output lost a stream: %q", res.Output)
	}
}

// Direct argv execution must not word-split or expand. This is the difference
// between running a program and handing untrusted model output to a shell.
func TestArgvModeDoesNotInterpretShellSyntax(t *testing.T) {
	res, err := Run(context.Background(), Command{Argv: []string{"echo", "a; rm -rf /tmp/nope", "$HOME", "*"}})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(res.Output)
	want := "a; rm -rf /tmp/nope $HOME *"
	if got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestShellModeInterpretsPipes(t *testing.T) {
	res, err := Run(context.Background(), Command{
		Argv:  []string{"printf 'b\\na\\nc\\n' | sort | tr '\\n' ' '"},
		Shell: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Output) != "a b c" {
		t.Errorf("output = %q, want %q", res.Output, "a b c")
	}
}

func TestShellModeRejectsMultipleArguments(t *testing.T) {
	_, err := Run(context.Background(), Command{Argv: []string{"echo", "hi"}, Shell: true})
	if err == nil {
		t.Fatal("shell mode takes a single script string")
	}
}

// A timeout must reach the whole process group. Killing only the direct child
// leaves the compiler or test runner it spawned holding the workspace.
func TestTimeoutKillsDescendants(t *testing.T) {
	res, err := Run(context.Background(), Command{
		Argv:    []string{"sleep 60 & echo CHILD:$!; wait"},
		Shell:   true,
		Timeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut {
		t.Fatalf("expected a timeout, got %+v", res)
	}
	if res.ExitCode != -1 {
		t.Errorf("exit code = %d, want -1 for a timeout", res.ExitCode)
	}

	_, rest, ok := strings.Cut(res.Output, "CHILD:")
	if !ok {
		t.Fatalf("test fixture did not report the grandchild pid: %q", res.Output)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(strings.SplitN(rest, "\n", 2)[0]))
	if err != nil {
		t.Fatalf("parsing grandchild pid from %q: %v", rest, err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processIsRunning(t, pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	syscall.Kill(pid, syscall.SIGKILL)
	t.Errorf("grandchild %d survived the timeout: the group was not signalled", pid)
}

// processIsRunning reports whether pid names a process that is still executing.
//
// Signal 0 alone cannot answer that. It asks whether the pid exists, and a
// zombie exists: the kernel keeps the entry until someone reaps it. Where
// nothing does, which is any container running without an init process, a
// descendant the runner killed correctly answers "still here" forever, and a
// working kill path reports itself broken. Diagnosing that from the failure
// message costs an hour, because every word of it is about process groups and
// none of the problem is.
func processIsRunning(t *testing.T, pid int) bool {
	t.Helper()
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	if runtime.GOOS != "linux" {
		// Elsewhere the platform's init reaps promptly and a test binary cannot
		// be pid 1, so an existing pid is a running one.
		return true
	}

	state, ok := procState(pid)
	if !ok {
		// The pid exists but its state could not be read, which is the race
		// between the two reads rather than an answer. Reporting it as running
		// keeps a probe that measured nothing from passing the test.
		return true
	}
	return state != 'Z'
}

// procState reads the run state from /proc/<pid>/stat.
//
// The parse starts from the last close paren rather than splitting on spaces,
// because the second field is the executable name in parens and may itself
// contain spaces and parens.
func procState(pid int) (byte, bool) {
	body, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, false
	}
	end := strings.LastIndexByte(string(body), ')')
	if end < 0 {
		return 0, false
	}
	rest := strings.TrimLeft(string(body[end+1:]), " ")
	if rest == "" {
		return 0, false
	}
	return rest[0], true
}

func TestCancellationReturnsContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	_, err := Run(ctx, Command{Argv: []string{"sleep", "30"}, Timeout: time.Minute})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestOutputCapKeepsBothEnds(t *testing.T) {
	res, err := Run(context.Background(), Command{
		Argv:      []string{"printf 'START'; for i in $(seq 1 4000); do printf 'xxxxxxxxxx'; done; printf 'END'"},
		Shell:     true,
		MaxOutput: 2000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Fatal("40KB of output through a 2KB cap must report truncation")
	}
	if !strings.HasPrefix(res.Output, "START") {
		t.Error("the head of the output was lost")
	}
	if !strings.HasSuffix(res.Output, "END") {
		t.Error("the tail was lost; a compiler puts its errors there")
	}
	if !strings.Contains(res.Output, "bytes of output omitted") {
		t.Error("truncation must be visible in the output the model reads")
	}
	if len(res.Output) > 2000+200 {
		t.Errorf("captured %d bytes against a 2000 byte cap", len(res.Output))
	}
}

func TestProviderCredentialsAreNotInherited(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-should-not-leak")
	t.Setenv("KIMI_API_KEY", "kimi-should-not-leak")
	t.Setenv("SB_ANTHROPIC_FIRST_PARTY_API_KEY", "namespaced-should-not-leak")
	t.Setenv("openai_api_key", "mixed-case-should-not-leak")
	t.Setenv("SB_HARMLESS", "visible")

	res, err := Run(context.Background(), Command{
		Argv:  []string{"echo \"key=[${ANTHROPIC_API_KEY}] kimi=[${KIMI_API_KEY}] namespaced=[${SB_ANTHROPIC_FIRST_PARTY_API_KEY}] mixed=[${openai_api_key}] other=[${SB_HARMLESS}]\""},
		Shell: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Output, "should-not-leak") {
		t.Error("a model-requested command read the harness's provider credential")
	}
	if !strings.Contains(res.Output, "other=[visible]") {
		t.Errorf("the rest of the environment must pass through: %q", res.Output)
	}
}

func TestGenericCredentialsAreNotInherited(t *testing.T) {
	t.Setenv("AUTH", "generic-auth-secret")
	t.Setenv("SESSION_ID", "generic-session-secret")
	t.Setenv("DATABASE_URL", "postgres://generic-database-secret")
	t.Setenv("SB_HARMLESS", "visible")

	res, err := Run(context.Background(), Command{
		Argv:  []string{"echo \"auth=[${AUTH}] session=[${SESSION_ID}] database=[${DATABASE_URL}] other=[${SB_HARMLESS}]\""},
		Shell: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Output, "generic-") {
		t.Errorf("a model-requested command inherited a generic credential: %q", res.Output)
	}
	if !strings.Contains(res.Output, "other=[visible]") {
		t.Errorf("the rest of the environment must pass through: %q", res.Output)
	}
}

func TestConfinedLoopbackEnvironmentCannotInheritOrReintroduceProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:8888")
	t.Setenv("npm_config_proxy", "http://127.0.0.1:8889")
	t.Setenv("SB_HARMLESS", "visible")

	env := commandEnv(NetworkLoopback, []string{
		"ALL_PROXY=http://127.0.0.1:8890",
		"GOPROXY=http://127.0.0.1:8891",
		"SB_EXTRA=kept",
	})
	joined := strings.Join(env, "\n")
	for _, forbidden := range []string{"HTTPS_PROXY=", "npm_config_proxy=", "ALL_PROXY=", "GOPROXY="} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("loopback child environment retained %s", forbidden)
		}
	}
	for _, want := range []string{"SB_HARMLESS=visible", "SB_EXTRA=kept"} {
		if !strings.Contains(joined, want) {
			t.Errorf("loopback child environment dropped harmless %s", want)
		}
	}

	full := strings.Join(commandEnv(NetworkFull, nil), "\n")
	if !strings.Contains(full, "HTTPS_PROXY=http://127.0.0.1:8888") {
		t.Error("full-network command unexpectedly lost the host proxy")
	}
}

func TestScrubbedChildEnvironmentDropsLoaderInjection(t *testing.T) {
	for key, value := range map[string]string{
		"LD_PRELOAD": "/workspace/evil.so", "DYLD_INSERT_LIBRARIES": "/workspace/evil.dylib",
		"GCONV_PATH": "/workspace/gconv", "LOCPATH": "/workspace/locale", "NLSPATH": "/workspace/messages",
		"BASH_ENV": "/workspace/bashrc", "ENV": "/workspace/shellrc",
		"GIO_MODULE_DIR": "/workspace/gio", "GIO_EXTRA_MODULES": "/workspace/gio-extra",
		"GI_TYPELIB_PATH": "/workspace/typelib", "GTK_PATH": "/workspace/gtk",
		"GTK_MODULES": "/workspace/gtk-module.so", "GTK_IM_MODULE_FILE": "/workspace/gtk-im.cache",
		"GDK_PIXBUF_MODULE_FILE": "/workspace/pixbuf.cache", "GDK_PIXBUF_MODULEDIR": "/workspace/pixbuf",
		"QT_PLUGIN_PATH": "/workspace/qt", "QT_QPA_PLATFORM_PLUGIN_PATH": "/workspace/qt-platform",
		"QML_IMPORT_PATH": "/workspace/qml", "QML2_IMPORT_PATH": "/workspace/qml2",
		"GST_PLUGIN_PATH": "/workspace/gst", "GST_PLUGIN_PATH_1_0": "/workspace/gst1",
		"GST_PLUGIN_SYSTEM_PATH": "/workspace/gst-system", "GST_PLUGIN_SYSTEM_PATH_1_0": "/workspace/gst-system1",
		"LUA_PATH": "/workspace/lua", "LUA_CPATH": "/workspace/lua-native",
		"TCLLIBPATH": "/workspace/tcl", "TCL_LIBRARY": "/workspace/tcl-lib", "TK_LIBRARY": "/workspace/tk-lib",
		"PHPRC":            "/workspace/php.ini",
		"PHP_INI_SCAN_DIR": "/workspace/php.d", "VLC_PLUGIN_PATH": "/workspace/vlc",
		"OPENSSL_CONF": "/workspace/openssl.cnf", "OPENSSL_CONF_INCLUDE": "/workspace/openssl.d",
		"OPENSSL_MODULES": "/workspace/openssl-modules", "OPENSSL_ENGINES": "/workspace/openssl-engines",
		"NODE_OPTIONS": "--require=/workspace/evil.js", "NODE_PATH": "/workspace/node",
		"PYTHONPATH": "/workspace/python", "PYTHONHOME": "/workspace/python-home", "PYTHONSTARTUP": "/workspace/startup.py",
		"PYTHONUSERBASE": "/workspace/python-user-base",
		"PERL5OPT":       "-M/workspace/evil.pm", "PERL5LIB": "/workspace/perl",
		"RUBYOPT": "-r/workspace/evil.rb", "RUBYLIB": "/workspace/ruby",
		"GEM_HOME": "/workspace/gems", "GEM_PATH": "/workspace/gem-path",
		"JAVA_TOOL_OPTIONS": "-javaagent:/workspace/evil.jar", "JDK_JAVA_OPTIONS": "-javaagent:/workspace/evil.jar",
		"_JAVA_OPTIONS": "-javaagent:/workspace/evil.jar", "CLASSPATH": "/workspace/java",
		"DOTNET_STARTUP_HOOKS": "/workspace/evil.dll", "ZDOTDIR": "/workspace/zsh",
	} {
		t.Setenv(key, value)
	}
	got := make(map[string]bool)
	for _, entry := range ScrubbedChildEnv() {
		name, _, ok := strings.Cut(entry, "=")
		if ok {
			got[strings.ToUpper(name)] = true
		}
	}
	for _, key := range []string{
		"LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "GCONV_PATH", "LOCPATH", "NLSPATH",
		"BASH_ENV", "ENV", "ZDOTDIR",
		"GIO_MODULE_DIR", "GIO_EXTRA_MODULES",
		"GI_TYPELIB_PATH", "GTK_PATH", "GTK_MODULES", "GTK_IM_MODULE_FILE", "GDK_PIXBUF_MODULE_FILE", "GDK_PIXBUF_MODULEDIR",
		"QT_PLUGIN_PATH", "QT_QPA_PLATFORM_PLUGIN_PATH", "QML_IMPORT_PATH", "QML2_IMPORT_PATH",
		"GST_PLUGIN_PATH", "GST_PLUGIN_PATH_1_0", "GST_PLUGIN_SYSTEM_PATH", "GST_PLUGIN_SYSTEM_PATH_1_0",
		"LUA_PATH", "LUA_CPATH", "TCLLIBPATH", "TCL_LIBRARY", "TK_LIBRARY",
		"PHPRC", "PHP_INI_SCAN_DIR", "VLC_PLUGIN_PATH",
		"OPENSSL_CONF", "OPENSSL_CONF_INCLUDE", "OPENSSL_MODULES", "OPENSSL_ENGINES",
		"NODE_OPTIONS", "NODE_PATH", "PYTHONPATH", "PYTHONHOME", "PYTHONSTARTUP", "PYTHONUSERBASE",
		"PERL5OPT", "PERL5LIB", "RUBYOPT", "RUBYLIB",
		"GEM_HOME", "GEM_PATH",
		"JAVA_TOOL_OPTIONS", "JDK_JAVA_OPTIONS", "_JAVA_OPTIONS", "CLASSPATH",
		"DOTNET_STARTUP_HOOKS",
	} {
		if got[key] {
			t.Fatalf("scrubbed child environment retained %s", key)
		}
	}
}

func TestCommandOutputUsesTheFixedChildEnvironment(t *testing.T) {
	t.Setenv("DYLD_INSERT_LIBRARIES", "/workspace/evil.dylib")
	t.Setenv("GIO_EXTRA_MODULES", "/workspace/gio")
	out := commandOutput("/usr/bin/env")
	for _, forbidden := range []string{"DYLD_INSERT_LIBRARIES=", "GIO_EXTRA_MODULES="} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("informational fixed child retained %s", forbidden)
		}
	}
}

func TestRunInWorkspaceDirectory(t *testing.T) {
	dir := t.TempDir()
	res, err := Run(context.Background(), Command{Argv: []string{"pwd"}, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}

	// macOS hands out temp directories behind a symlink, so compare resolved
	// paths rather than the literal strings.
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(res.Output); got != want {
		t.Errorf("cwd = %q, want %q", got, want)
	}
}

func TestAlreadyCancelledContextDoesNotLaunchCommand(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "must-not-exist")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, Command{
		Argv:  []string{"printf launched > " + marker},
		Shell: true,
		Dir:   dir,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("pre-cancelled command launched: stat err=%v", statErr)
	}
}

func TestMissingBinaryIsAnError(t *testing.T) {
	_, err := Run(context.Background(), Command{Argv: []string{"switchboard-no-such-binary"}})
	if err == nil {
		t.Fatal("expected an error for a binary that does not exist")
	}
}
