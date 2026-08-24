package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/tools"
)

func TestMain(m *testing.M) {
	if os.Getenv("SWITCHBOARD_AGENT_BUILD_HELPER") == "1" {
		marker := os.Getenv("SWITCHBOARD_BUILD_MARKER")
		if marker == "" {
			os.Exit(2)
		}
		if err := os.WriteFile(marker, []byte("ran"), 0o600); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type buildAllowReviewer struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (r *buildAllowReviewer) Review(ctx context.Context, _ permission.ReviewRequest) (permission.ReviewResult, error) {
	r.calls.Add(1)
	if r.entered != nil {
		close(r.entered)
	}
	if r.release != nil {
		select {
		case <-r.release:
		case <-ctx.Done():
			return permission.ReviewResult{}, ctx.Err()
		}
	}
	return permission.ReviewResult{
		Decision: permission.ReviewAllow,
		Reviewer: "t1/cheap",
		Reason:   "ordinary workspace build",
	}, nil
}

func autoBuildLoop(t *testing.T, controller *execution.Controller, reviewer permission.Reviewer, asker permission.Asker) (*Loop, string) {
	t.Helper()
	root := t.TempDir()
	marker := filepath.Join(root, "build-ran")
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	helperName := "go"
	if runtime.GOOS == "windows" {
		helperName += ".exe"
	}
	copyTestExecutable(t, filepath.Join(bin, helperName))
	t.Setenv("SWITCHBOARD_AGENT_BUILD_HELPER", "1")
	t.Setenv("SWITCHBOARD_BUILD_MARKER", marker)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	registry, err := tools.NewRegistryWithExecution(root, controller)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.Create(root, "scripted/local/test", "test-revision")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	perms := permission.NewEngineWithExecution(permission.ModeAuto, controller)
	perms.SetReviewer(reviewer)
	return &Loop{Tools: registry, Perms: perms, Asker: asker, Session: sess}, marker
}

func copyTestExecutable(t *testing.T, destination string) {
	t.Helper()
	sourcePath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	dest, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dest, source); err != nil {
		_ = dest.Close()
		t.Fatal(err)
	}
	if err := dest.Close(); err != nil {
		t.Fatal(err)
	}
}

func runAutoBuild(loop *Loop) (provider.ToolResult, error) {
	results, err := loop.runTools(context.Background(), []provider.ToolUse{{
		ID:    "workspace-build",
		Name:  "exec",
		Input: json.RawMessage(`{"command":["go","test","./..."]}`),
	}}, NopObserver{})
	if err != nil {
		return provider.ToolResult{}, err
	}
	if len(results) != 1 {
		return provider.ToolResult{}, errors.New("workspace build did not return exactly one tool result")
	}
	result, ok := results[0].(provider.ToolResult)
	if !ok {
		return provider.ToolResult{}, errors.New("workspace build returned a non-tool result")
	}
	return result, nil
}

func assertBuildDidNotRun(t *testing.T, marker string) {
	t.Helper()
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("host-direct workspace build ran without human approval: stat error = %v", err)
	}
}

func TestAutoHostDirectWorkspaceBuildNeverReachesReviewerOrRuns(t *testing.T) {
	reviewer := &buildAllowReviewer{}
	human := &autoAsker{approve: false}
	loop, marker := autoBuildLoop(t, execution.NewDefaultController(execution.TestingVerifiedCapability()), reviewer, human)

	result, err := runAutoBuild(loop)
	if err != nil {
		t.Fatal(err)
	}
	if reviewer.calls.Load() != 0 {
		t.Fatalf("host-direct build reached model reviewer %d times", reviewer.calls.Load())
	}
	if human.calls != 1 {
		t.Fatalf("host-direct build asked human %d times, want 1", human.calls)
	}
	if !result.IsError || !strings.Contains(result.Content, "not approved") {
		t.Fatalf("host-direct build result = %+v", result)
	}
	assertBuildDidNotRun(t, marker)
}

func TestAutoReviewerApprovalCannotCrossSandboxOrModeTransition(t *testing.T) {
	for _, test := range []struct {
		name       string
		transition func(*permission.Engine, *execution.Controller) error
		want       string
	}{
		{
			name: "sandbox on to off",
			transition: func(_ *permission.Engine, controller *execution.Controller) error {
				return controller.SetSandbox(execution.SandboxOff)
			},
			want: "execution posture changed",
		},
		{
			name: "auto to yolo",
			transition: func(engine *permission.Engine, _ *execution.Controller) error {
				engine.SetMode(permission.ModeYOLO)
				return nil
			},
			want: "permission mode changed",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			controller, err := execution.NewController(execution.TestingVerifiedCapability(), execution.SandboxOn)
			if err != nil {
				t.Fatal(err)
			}
			reviewer := &buildAllowReviewer{entered: make(chan struct{}), release: make(chan struct{})}
			human := &autoAsker{approve: false}
			loop, marker := autoBuildLoop(t, controller, reviewer, human)

			type buildResult struct {
				result provider.ToolResult
				err    error
			}
			result := make(chan buildResult, 1)
			go func() {
				got, runErr := runAutoBuild(loop)
				result <- buildResult{result: got, err: runErr}
			}()
			<-reviewer.entered
			if err := test.transition(loop.Perms, controller); err != nil {
				t.Fatal(err)
			}
			close(reviewer.release)
			run := <-result
			if run.err != nil {
				t.Fatal(run.err)
			}
			got := run.result

			if reviewer.calls.Load() != 1 || human.calls != 0 {
				t.Fatalf("transition reviewer=%d human=%d", reviewer.calls.Load(), human.calls)
			}
			if !got.IsError || !strings.Contains(got.Content, test.want) {
				t.Fatalf("transition build result = %+v, want %q", got, test.want)
			}
			assertBuildDidNotRun(t, marker)
		})
	}
}
