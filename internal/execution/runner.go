package execution

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/switchboard-code/switchboard/internal/childenv"
)

const (
	DefaultTimeout   = 2 * time.Minute
	DefaultMaxOutput = 64 << 10

	// terminateGrace is how long a process group gets to exit on SIGTERM before
	// SIGKILL. Long enough for a test runner to flush, short enough that a
	// cancelled turn returns promptly.
	terminateGrace = 2 * time.Second

	// reapTimeout bounds the wait after a group kill. A descendant that survives
	// SIGKILL is stuck in the kernel, and blocking on it forever would hang the
	// agent instead of the command.
	reapTimeout = 2 * time.Second
)

// Command is one execution request. Shell mode is a separate field rather than
// an argv convention so permission rules can match on it: a shell string is
// untrusted model output that gets word splitting, expansion, and redirection,
// and that is a materially different request from running a binary directly.
type Command struct {
	Argv      []string
	Shell     bool
	Dir       string
	Timeout   time.Duration
	MaxOutput int

	// Confine, when non-nil, confines the command. It comes from
	// Capability.Confinement, which is the same value that decides whether
	// automatic execution was allowed, so a command cannot be approved as
	// contained and then run unconfined.
	//
	// If it is set and cannot be applied, Run fails. It never falls back to
	// running the command unconfined.
	Confine *Confinement
	Policy  Policy

	// ExtraEnv appends to the hygienic child environment. It exists for
	// hook payloads; it is not a way back in for the credential variables
	// childEnv strips, so those names are rejected here too.
	ExtraEnv []string

	// Stdin, when non-empty, is fed to the child's standard input. The
	// default remains a closed stdin: a command that waits for input would
	// otherwise wait forever.
	Stdin []byte
}

type Result struct {
	Output    string
	ExitCode  int
	TimedOut  bool
	Truncated bool
	Duration  time.Duration
}

func Run(ctx context.Context, c Command) (Result, error) {
	if len(c.Argv) == 0 {
		return Result{}, errors.New("no command given")
	}
	if c.Shell && len(c.Argv) != 1 {
		return Result{}, fmt.Errorf("shell mode takes one script string, got %d arguments", len(c.Argv))
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.MaxOutput <= 0 {
		c.MaxOutput = DefaultMaxOutput
	}

	name, args := c.Argv[0], c.Argv[1:]
	if c.Shell {
		name, args = shellCommand(c.Argv[0])
	}
	policy, trustedEnv := bindGoToolchain(c)

	if c.Confine != nil {
		wrapped, err := c.Confine.apply(policy, append([]string{name}, args...))
		if err != nil {
			// Failing closed is the whole point. A sandbox that quietly falls
			// back to running the command is worse than no sandbox, because the
			// UI goes on reporting containment.
			return Result{}, fmt.Errorf("refusing to run unconfined: %w", err)
		}
		name, args = wrapped[0], wrapped[1:]
	}

	runCtx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	// exec.CommandContext is deliberately not used: it kills only the direct
	// child, which leaves a shell's descendants running and holding the output
	// pipe open. The whole group has to go.
	cmd := exec.Command(name, args...)
	cmd.Dir = c.Dir
	envNetwork := NetworkFull
	if c.Confine != nil && c.Policy.Network == NetworkLoopback {
		envNetwork = NetworkLoopback
	}
	cmd.Env = commandEnv(envNetwork, c.ExtraEnv)
	cmd.Env = append(cmd.Env, trustedEnv...)
	if len(c.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(c.Stdin)
	}
	out := newCapture(c.MaxOutput)
	cmd.Stdout = out
	cmd.Stderr = out

	started := time.Now()
	waitErr, contextErr, processStarted := runProcess(runCtx, cmd)
	if !processStarted {
		if contextErr != nil {
			return Result{Duration: time.Since(started)}, contextErr
		}
		return Result{Duration: time.Since(started)}, waitErr
	}
	timedOut := errors.Is(contextErr, context.DeadlineExceeded)

	text, truncated := out.String()
	res := Result{
		Output:    text,
		TimedOut:  timedOut,
		Truncated: truncated,
		Duration:  time.Since(started),
	}

	switch {
	case timedOut:
		res.ExitCode = -1
	case waitErr == nil:
		res.ExitCode = 0
	default:
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
		} else {
			// The process ran but could not be reaped normally. Report it as a
			// failure with the output intact rather than discarding the turn.
			res.ExitCode = -1
			res.Output = appendLine(res.Output, "switchboard: "+waitErr.Error())
		}
	}

	// A cancelled turn is the user's decision, not a command failure.
	if contextErr != nil && !timedOut {
		return res, contextErr
	}
	return res, nil
}

// RunProcess starts cmd and waits for it with the same cancellation boundary
// as Run. On Unix the command gets its own process group and cancellation
// reaches every descendant in that group. Platforms without group signalling
// can stop only the direct process; callers must preserve that limitation in
// anything they tell the user.
//
// Callers must construct cmd with exec.Command, not exec.CommandContext. The
// latter installs its own direct-child kill path and can race this package's
// process-tree cleanup. Stdout and stderr may be ordinary io.Writers; the
// bounded WaitDelay on unsupported platforms closes os/exec's copy pipes when
// a surviving descendant keeps them open after the direct process exits.
func RunProcess(ctx context.Context, cmd *exec.Cmd) error {
	waitErr, contextErr, _ := runProcess(ctx, cmd)
	if contextErr == nil {
		return waitErr
	}
	if waitErr == nil {
		return contextErr
	}
	return errors.Join(contextErr, waitErr)
}

// runProcess keeps the wait result, cancellation cause, and successful Start
// separate so Run can preserve its public distinctions among launch failure, a
// timed-out Result, and owner cancellation returned as an error.
func runProcess(ctx context.Context, cmd *exec.Cmd) (waitErr, contextErr error, started bool) {
	if ctx == nil {
		return errors.New("nil process context"), nil, false
	}
	if cmd == nil {
		return errors.New("nil command"), nil, false
	}
	if err := ctx.Err(); err != nil {
		return nil, err, false
	}

	setProcessGroup(cmd)
	// Wait otherwise follows inherited stdout/stderr pipes forever after the
	// direct child exits on a platform where descendant cleanup is unavailable.
	// Unix keeps its old zero-delay behavior: an ordinary background process is
	// allowed to retain the shell's pipes until the command timeout, at which
	// point group cancellation closes them rather than misreporting a clean
	// shell exit as ErrWaitDelay.
	cmd.WaitDelay = processWaitDelay()
	if err := cmd.Start(); err != nil {
		return err, nil, false
	}
	started = true

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case waitErr = <-done:
		if err := ctx.Err(); err == nil {
			return waitErr, nil, true
		} else {
			// Cancellation won concurrently with the direct child's exit. A
			// detached descendant may still occupy the process group, so the
			// completed Wait does not make cleanup optional.
			terminateGroup(cmd, terminateGrace)
			return waitErr, err, true
		}
	case <-ctx.Done():
		contextErr = ctx.Err()
	}

	terminateGroup(cmd, terminateGrace)
	// On platforms that need it, WaitDelay bounds inherited-pipe cleanup after
	// the direct process exits. This outer wait bounds every platform and leaves
	// a little scheduler room beyond that delay before giving the caller back
	// control; even an unkillable process cannot hold the prompt forever.
	timer := time.NewTimer(reapTimeout + 250*time.Millisecond)
	defer timer.Stop()
	select {
	case waitErr = <-done:
		return waitErr, contextErr, true
	case <-timer.C:
		return errors.New("process did not exit after cancellation cleanup"), contextErr, true
	}
}

func commandEnv(network NetworkAccess, extra []string) []string {
	keep := func(kv string) bool {
		key, _, ok := strings.Cut(kv, "=")
		if !ok || childenv.Sensitive(key) || childLoaderInjection(key) {
			return false
		}
		// macOS Seatbelt permits host loopback so test fixtures work. An
		// inherited HTTP(S) proxy on 127.0.0.1 would otherwise turn that into
		// off-machine egress without the separate network grant. Strip proxy-
		// related variables, including tool-specific variants and ExtraEnv,
		// whenever loopback is the effective confined policy.
		if network == NetworkLoopback && strings.Contains(strings.ToUpper(key), "PROXY") {
			return false
		}
		return true
	}

	env := childenv.Current()
	kept := make([]string, 0, len(env)+len(extra))
	for _, kv := range env {
		if keep(kv) {
			kept = append(kept, kv)
		}
	}
	for _, kv := range extra {
		if keep(kv) {
			kept = append(kept, kv)
		}
	}
	return kept
}

// childLoaderInjection identifies ambient process controls that can make a
// fixed executable load attacker-selected code before main. Explicit user `!`
// shells do not use commandEnv; agent commands and first-party helpers do.
func childLoaderInjection(key string) bool {
	upper := strings.ToUpper(strings.TrimSpace(key))
	if strings.HasPrefix(upper, "LD_") || strings.HasPrefix(upper, "DYLD_") {
		return true
	}
	switch upper {
	case "GCONV_PATH", "LOCPATH", "NLSPATH", "BASH_ENV", "ENV", "ZDOTDIR",
		"GIO_MODULE_DIR", "GIO_EXTRA_MODULES", "GI_TYPELIB_PATH",
		"GTK_PATH", "GTK_MODULES", "GTK_IM_MODULE_FILE", "GDK_PIXBUF_MODULE_FILE", "GDK_PIXBUF_MODULEDIR",
		"QT_PLUGIN_PATH", "QT_QPA_PLATFORM_PLUGIN_PATH", "QML_IMPORT_PATH", "QML2_IMPORT_PATH",
		"GST_PLUGIN_PATH", "GST_PLUGIN_PATH_1_0", "GST_PLUGIN_SYSTEM_PATH", "GST_PLUGIN_SYSTEM_PATH_1_0",
		"LUA_PATH", "LUA_CPATH", "TCLLIBPATH", "TCL_LIBRARY", "TK_LIBRARY",
		"PHPRC", "PHP_INI_SCAN_DIR", "VLC_PLUGIN_PATH",
		"OPENSSL_CONF", "OPENSSL_CONF_INCLUDE", "OPENSSL_MODULES", "OPENSSL_ENGINES",
		"NODE_OPTIONS", "NODE_PATH", "GOROOT",
		"PYTHONHOME", "PYTHONPATH", "PYTHONSTARTUP", "PYTHONUSERBASE",
		"PERL5LIB", "PERL5OPT", "RUBYLIB", "RUBYOPT",
		"GEM_HOME", "GEM_PATH",
		"JAVA_TOOL_OPTIONS", "JDK_JAVA_OPTIONS", "_JAVA_OPTIONS", "CLASSPATH",
		"DOTNET_STARTUP_HOOKS":
		return true
	default:
		return false
	}
}

func childEnv() []string { return commandEnv(NetworkFull, nil) }

// ScrubbedChildEnv returns the environment for fixed first-party subprocesses:
// ordinary process context without credential-shaped variables or ambient
// loader controls. Explicit user shells and editors deliberately do not use
// this posture.
func ScrubbedChildEnv() []string { return childEnv() }

func appendLine(s, line string) string {
	if s == "" {
		return line
	}
	if strings.HasSuffix(s, "\n") {
		return s + line
	}
	return s + "\n" + line
}
