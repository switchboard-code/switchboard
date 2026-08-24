package execution

// Background commands: a process that outlives the tool call that started it.
//
// exec is synchronous, which is right for almost everything an agent runs and
// wrong for the one shape it cannot express at all: a dev server, a watch
// build, a long migration. Those either block the turn until the ceiling or
// are simply not startable, and an agent that cannot start a server cannot
// check whether the page it just changed renders.
//
// Everything that makes Run safe applies here unchanged, because this reuses
// it rather than reimplementing it. The confinement is applied by the same
// code and fails closed the same way; the whole process group is signalled on
// stop, not just the direct child, so a shell's descendants go with it; output
// is captured into the same bounded buffer.
//
// What is new is a lifetime, and a lifetime is what leaks. Three bounds hold
// it. A background command has a hard ceiling and is killed at it, because a
// process this program started and forgot is this program's fault. The number
// that may run at once is capped, so a loop that starts servers cannot fill
// the machine. And the set is stopped when the session ends, which is the only
// moment the program can still be sure it is the one holding them.

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

const (
	// MaxBackgroundLifetime is the ceiling a background command is killed at.
	// It is long enough for a dev server to outlast the work that wanted it
	// and short enough that a forgotten process is not a permanent resident.
	MaxBackgroundLifetime = time.Hour

	// MaxBackground bounds how many may run at once. The limit exists because
	// a model that has learned to start a server will start one per attempt.
	MaxBackground = 8
)

// BackgroundStatus is one background command as a reader sees it. It carries
// no handle to the process: stopping goes through the set, which is the only
// thing that knows the group is still its own to signal.
type BackgroundStatus struct {
	ID       string
	Argv     []string
	Shell    bool
	Started  time.Time
	Running  bool
	ExitCode int
	TimedOut bool

	// Killed marks a process the set stopped, so "exit 137" reads as an answer
	// rather than a mystery.
	Killed bool
}

type background struct {
	id      string
	argv    []string
	shell   bool
	started time.Time

	cmd    *exec.Cmd
	out    *capture
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	running  bool
	exitCode int
	timedOut bool
	killed   bool
}

// BackgroundSet owns every background command one session started.
//
// It is the session's, not a package global: two Switchboards in one shell
// must not be able to stop each other's processes, and a set that outlived its
// session would be a handle to processes nobody is left to reap.
type BackgroundSet struct {
	mu      sync.Mutex
	byID    map[string]*background
	order   []string
	next    int
	stopped bool
}

func NewBackgroundSet() *BackgroundSet {
	return &BackgroundSet{byID: map[string]*background{}}
}

// Start launches a command and returns immediately with its id.
//
// The context governs the whole set's lifetime rather than one call's: a
// background command that died when the tool call returned would be a
// synchronous command with extra steps.
func (s *BackgroundSet) Start(ctx context.Context, c Command) (BackgroundStatus, error) {
	if len(c.Argv) == 0 {
		return BackgroundStatus{}, errors.New("no command given")
	}
	if c.Shell && len(c.Argv) != 1 {
		return BackgroundStatus{}, fmt.Errorf("shell mode takes one script string, got %d arguments", len(c.Argv))
	}
	if c.MaxOutput <= 0 {
		c.MaxOutput = DefaultMaxOutput
	}

	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return BackgroundStatus{}, errors.New("this session is shutting down and is not starting new processes")
	}
	if s.liveCountLocked() >= MaxBackground {
		s.mu.Unlock()
		return BackgroundStatus{}, fmt.Errorf(
			"%d background commands are already running, which is the limit; stop one before starting another", MaxBackground)
	}
	s.next++
	id := "bg" + strconv.Itoa(s.next)
	s.mu.Unlock()

	name, args := c.Argv[0], c.Argv[1:]
	if c.Shell {
		name, args = shellCommand(c.Argv[0])
	}
	policy, trustedEnv := bindGoToolchain(c)
	if c.Confine != nil {
		wrapped, err := c.Confine.apply(policy, append([]string{name}, args...))
		if err != nil {
			// The same refusal Run makes, for the same reason: a sandbox that
			// quietly fell back to running the command would leave the UI
			// reporting containment that is not there.
			return BackgroundStatus{}, fmt.Errorf("refusing to run unconfined: %w", err)
		}
		name, args = wrapped[0], wrapped[1:]
	}

	runCtx, cancel := context.WithTimeout(ctx, MaxBackgroundLifetime)

	cmd := exec.Command(name, args...)
	cmd.Dir = c.Dir
	envNetwork := NetworkFull
	if c.Confine != nil && c.Policy.Network == NetworkLoopback {
		envNetwork = NetworkLoopback
	}
	cmd.Env = commandEnv(envNetwork, c.ExtraEnv)
	cmd.Env = append(cmd.Env, trustedEnv...)
	out := newCapture(c.MaxOutput)
	cmd.Stdout = out
	cmd.Stderr = out
	setProcessGroup(cmd)
	cmd.WaitDelay = processWaitDelay()

	if err := cmd.Start(); err != nil {
		cancel()
		return BackgroundStatus{}, err
	}

	b := &background{
		id: id, argv: c.Argv, shell: c.Shell, started: time.Now(),
		cmd: cmd, out: out, cancel: cancel, done: make(chan struct{}), running: true,
	}
	s.mu.Lock()
	s.byID[id] = b
	s.order = append(s.order, id)
	s.mu.Unlock()

	go b.wait(runCtx)
	return b.status(), nil
}

// wait reaps the process and records how it ended. It is the only writer of
// the terminal fields, so a reader never sees a half-finished ending.
func (b *background) wait(ctx context.Context) {
	defer close(b.done)
	defer b.cancel()

	waited := make(chan error, 1)
	go func() { waited <- b.cmd.Wait() }()

	var waitErr error
	select {
	case waitErr = <-waited:
		// A descendant may still hold the group even though the direct child
		// returned, which is why the group is signalled either way.
		terminateGroup(b.cmd, terminateGrace)
	case <-ctx.Done():
		terminateGroup(b.cmd, terminateGrace)
		select {
		case waitErr = <-waited:
		case <-time.After(terminateGrace + processWaitDelay()):
			waitErr = errors.New("the process did not exit after its group was signalled")
		}
		b.mu.Lock()
		b.timedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
		b.mu.Unlock()
	}

	b.mu.Lock()
	b.running = false
	b.exitCode = exitCodeOf(waitErr)
	b.mu.Unlock()
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func (b *background) status() BackgroundStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	return BackgroundStatus{
		ID: b.id, Argv: b.argv, Shell: b.shell, Started: b.started,
		Running: b.running, ExitCode: b.exitCode, TimedOut: b.timedOut, Killed: b.killed,
	}
}

// Output returns what the command has written so far and whether the capture
// dropped anything. Reading does not consume: the same bytes answer again,
// because a caller that lost its only copy of a server's startup line has no
// way to ask for it back.
func (s *BackgroundSet) Output(id string) (string, bool, BackgroundStatus, error) {
	b, err := s.get(id)
	if err != nil {
		return "", false, BackgroundStatus{}, err
	}
	text, truncated := b.out.String()
	return text, truncated, b.status(), nil
}

// Stop signals the process group and waits for the reaper to record the end,
// so a caller that stops something and immediately lists it does not see it
// still running.
func (s *BackgroundSet) Stop(id string) (BackgroundStatus, error) {
	b, err := s.get(id)
	if err != nil {
		return BackgroundStatus{}, err
	}
	b.mu.Lock()
	if !b.running {
		b.mu.Unlock()
		return b.status(), nil
	}
	b.killed = true
	b.mu.Unlock()

	b.cancel()
	<-b.done
	return b.status(), nil
}

// StopAll ends every live command and refuses new ones. The session calls it
// on the way out, which is the last moment this program can be sure these
// processes are still its own to signal.
func (s *BackgroundSet) StopAll() {
	s.mu.Lock()
	s.stopped = true
	live := make([]*background, 0, len(s.byID))
	for _, id := range s.order {
		live = append(live, s.byID[id])
	}
	s.mu.Unlock()

	for _, b := range live {
		b.mu.Lock()
		running := b.running
		if running {
			b.killed = true
		}
		b.mu.Unlock()
		if !running {
			continue
		}
		b.cancel()
		<-b.done
	}
}

// List reports every command this session started, in the order they started,
// finished ones included: "it exited two minutes ago with status 1" is the
// answer to the same question as "it is still running".
func (s *BackgroundSet) List() []BackgroundStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]BackgroundStatus, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.byID[id].status())
	}
	return out
}

func (s *BackgroundSet) get(id string) (*background, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.byID[id]
	if !ok {
		return nil, fmt.Errorf("no background command named %q; this session started %d", id, len(s.order))
	}
	return b, nil
}

func (s *BackgroundSet) liveCountLocked() int {
	live := 0
	for _, b := range s.byID {
		if b.status().Running {
			live++
		}
	}
	return live
}
