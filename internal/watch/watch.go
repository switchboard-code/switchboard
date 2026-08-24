// Package watch runs the user's declared verifier at the seams of a turn and
// reports only what changed.
//
// The idea is §8.4's: a task-specific verifier — the test suite, a build, a
// linter — is stronger evidence than the harness's own completion signal.
// Without a declaration, that evidence arrives only when the model happens to
// run the tests itself. A watch command makes it ambient: after the loop's
// own mutation record says files changed, the verifier runs, and the delta
// travels — a failure no earlier run produced, or the run going green after
// being red. An unchanged verdict is silence, because a verifier that
// repeats itself every round teaches its reader to stop reading it.
//
// The command is typed by the user into their own session, which is the hook
// posture (§13): the user's standing policy, run unconfined and unprompted.
// There is deliberately no repository-declared form — a checkout must not
// get a command executed by the act of being opened — so nothing here sits
// behind the trust gate; the only way in is the user typing it.
package watch

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/switchboard-code/switchboard/internal/execution"
	route "github.com/switchboard-code/switchboard/internal/router"
)

const (
	// DefaultTimeout is generous for a scoped suite and a bound on an
	// unscoped one. A verifier that hangs has answered, the same way a hook
	// has: the timeout is reported as the failure it is, not waited out.
	DefaultTimeout = 2 * time.Minute

	// maxOutput caps what one run can hand back. The report extracts failing
	// lines from this, so the cap bounds work, not correctness: a suite whose
	// first failure is past 32KB of output has bigger problems than the tail
	// being cut.
	maxOutput = 32 << 10
)

// Watch is one declared verifier. It keeps the signature baseline the delta
// is computed against, across runs and across turns — remembering what has
// already been said is the whole mechanism.
type Watch struct {
	command string
	dir     string

	mu   sync.Mutex
	ran  bool
	red  bool
	seen map[string]bool
}

// Report is what one run adds to the record. Passed and the delta fields are
// meaningful only when Err is nil; Err means the command could not run at
// all, which is a fact about the harness, not the workspace.
type Report struct {
	Passed   bool
	ExitCode int
	TimedOut bool
	FirstRun bool

	// New are failures whose signature no earlier run of this watch
	// produced. Persisting counts the ones already reported, which stay out
	// of the delta: the same failure twice is one problem observed twice.
	New        []route.Failure
	Persisting int

	// Signatures is every signature this run produced, for feeding the
	// escalation detector, whose dedupe window is the turn rather than the
	// watch's lifetime.
	Signatures []string

	// WentGreen marks a pass immediately after a failing run — the one green
	// worth announcing. A pass that follows a pass is silence.
	WentGreen bool

	Took time.Duration
	Err  error
}

// Observation is one verifier execution before it changes the watch's delta
// baseline. Keeping execution and commitment separate lets callers discard a
// run whose owning session disappeared while the command was still running.
// Its fields stay private: an observation is produced only by Observe and is
// meaningful only to Commit on the same Watch.
type Observation struct {
	result execution.Result
	err    error
}

// Changed reports whether this run moved the record at all: a new failure,
// or red turning green. Everything else is a repeat, and repeats are silence.
func (r Report) Changed() bool {
	return r.Err == nil && (len(r.New) > 0 || r.WentGreen)
}

func New(command, dir string) *Watch {
	return &Watch{command: command, dir: dir, seen: map[string]bool{}}
}

func (w *Watch) Command() string { return w.command }

// Red reports whether the last run failed, for a status display.
func (w *Watch) Red() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ran && w.red
}

// Observe executes the verifier without changing the delta baseline. The
// command runs unconfined through the user's shell in the workspace, the same
// authority as typing it into the terminal next door.
func (w *Watch) Observe(ctx context.Context) Observation {
	res, err := execution.Run(ctx, execution.Command{
		Argv:      []string{w.command},
		Shell:     true,
		Dir:       w.dir,
		Timeout:   DefaultTimeout,
		MaxOutput: maxOutput,
	})
	return Observation{result: res, err: err}
}

// Commit computes the delta and advances the baseline for an observation.
// A caller that no longer owns the run must discard the observation instead.
func (w *Watch) Commit(observation Observation) Report {
	w.mu.Lock()
	defer w.mu.Unlock()
	res, err := observation.result, observation.err
	rep := Report{FirstRun: !w.ran, Took: res.Duration}
	if err != nil {
		rep.Err = err
		return rep
	}
	w.ran = true
	rep.ExitCode = res.ExitCode
	rep.TimedOut = res.TimedOut

	if res.ExitCode == 0 && !res.TimedOut {
		rep.Passed = true
		rep.WentGreen = w.red
		w.red = false
		// A pass clears the baseline: a failure that comes back after being
		// fixed broke again, and reporting it as new is the truth.
		w.seen = map[string]bool{}
		return rep
	}

	var failures []route.Failure
	if res.Truncated {
		// The capture retained only a head and tail. Do not inspect or forward
		// either: a credential can cross the omitted region and no scan of the
		// fragments can prove the returned text safe.
		line := fmt.Sprintf("verifier output exceeded the %d-byte capture and was withheld", maxOutput)
		failures = []route.Failure{{Signature: route.SignatureOf(line), Line: line}}
	} else {
		failures = route.ExtractFailures(res.Output)
	}
	if len(failures) == 0 {
		// A verifier can fail without printing anything the failure pattern
		// recognizes. The first non-empty line stands in, and with no output
		// at all the timeout or exit code is the whole story. Either way the
		// signature comes from the same reduction every failure gets, so a
		// repeat of the same silence stays a repeat.
		line := firstNonEmptyLine(res.Output)
		if line == "" {
			line = fmt.Sprintf("exited %d with no output", res.ExitCode)
		}
		if res.TimedOut {
			line = fmt.Sprintf("timed out after %s", DefaultTimeout)
		}
		failures = []route.Failure{{Signature: route.SignatureOf(line), Line: line}}
	}

	w.red = true
	for _, f := range failures {
		rep.Signatures = append(rep.Signatures, f.Signature)
		if w.seen[f.Signature] {
			rep.Persisting++
			continue
		}
		w.seen[f.Signature] = true
		rep.New = append(rep.New, f)
	}
	return rep
}

// ResetBaseline starts the declaration's delta accounting over. The command
// itself is unchanged; this is used when the application keeps a standing
// watch declaration while replacing the conversation it reports into.
func (w *Watch) ResetBaseline() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ran = false
	w.red = false
	w.seen = map[string]bool{}
}

// Run is the ordinary synchronous form: execute and immediately commit.
func (w *Watch) Run(ctx context.Context) Report {
	return w.Commit(w.Observe(ctx))
}

func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
