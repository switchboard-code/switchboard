//go:build unix

package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/tools"
)

const retryIntentCrashHelper = "SB_RETRY_INTENT_CRASH_HELPER"

func TestRetryIntentSurvivesSIGKILLAtOpeningStartAndCompletion(t *testing.T) {
	for _, boundary := range []string{"opening", "started", "completed"} {
		t.Run(boundary, func(t *testing.T) {
			root := t.TempDir()
			ready := filepath.Join(root, "ready")
			cmd := exec.Command(os.Args[0], "-test.run=^TestRetryIntentCrashHelper$")
			cmd.Env = append(os.Environ(),
				retryIntentCrashHelper+"=1",
				"SB_RETRY_INTENT_ROOT="+root,
				"SB_RETRY_INTENT_BOUNDARY="+boundary,
			)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, err := os.Stat(ready); err == nil {
					break
				} else if !errors.Is(err, os.ErrNotExist) {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
					t.Fatal(err)
				}
				if time.Now().After(deadline) {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
					t.Fatalf("helper did not reach %s", boundary)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err == nil {
				t.Fatal("SIGKILL helper exited successfully")
			}

			workspace := filepath.Join(root, "workspace")
			store, err := session.NewStore(filepath.Join(root, "sessions"))
			if err != nil {
				t.Fatal(err)
			}
			id, status, found, err := store.UnresolvedRetry(workspace)
			if err != nil {
				t.Fatal(err)
			}
			sess, err := store.Latest(workspace)
			if err != nil {
				t.Fatal(err)
			}
			defer sess.Close()
			switch boundary {
			case "opening":
				if !found || id != sess.ID() || status != session.RetryIntentPending {
					t.Fatalf("opening boundary = id %q status %q found %v latest %q", id, status, found, sess.ID())
				}
				registry, err := tools.NewRegistry(workspace, execution.Capability{})
				if err != nil {
					t.Fatal(err)
				}
				p := &scriptedProvider{turns: []scriptTurn{textTurn("resumed exactly once")}}
				loop := &Loop{Session: sess, Provider: p,
					Target: provider.RouteTarget{Provider: "test", Surface: "local", ModelID: "model"},
					Tools:  registry, Perms: permission.NewEngine(permission.ModeDefault, execution.Capability{})}
				if err := loop.ResumeRetryTurn(context.Background(), sess.State().RetryIntent.ID); err != nil {
					t.Fatal(err)
				}
				if p.calls != 1 || len(sess.State().Messages) != 2 {
					t.Fatalf("opening recovery calls=%d messages=%d", p.calls, len(sess.State().Messages))
				}
			case "started":
				if !found || id != sess.ID() || status != session.RetryIntentStarted {
					t.Fatalf("started boundary = id %q status %q found %v latest %q", id, status, found, sess.ID())
				}
				registry, err := tools.NewRegistry(workspace, execution.Capability{})
				if err != nil {
					t.Fatal(err)
				}
				p := &scriptedProvider{turns: []scriptTurn{textTurn("must not run")}}
				loop := &Loop{Session: sess, Provider: p,
					Target: provider.RouteTarget{Provider: "test", Surface: "local", ModelID: "model"}, Tools: registry}
				if err := loop.ResumeRetryTurn(context.Background(), sess.State().RetryIntent.ID); err == nil {
					t.Fatal("started retry auto-resumed")
				}
				if p.calls != 0 {
					t.Fatalf("started recovery called provider %d times", p.calls)
				}
			case "completed":
				if found || status != "" || sess.State().RetryIntent != nil || len(sess.State().Messages) != 2 {
					t.Fatalf("completed boundary = id %q status %q found %v state %#v", id, status, found, sess.State().RetryIntent)
				}
			}
		})
	}
}

func TestRetryIntentCrashHelper(t *testing.T) {
	if os.Getenv(retryIntentCrashHelper) != "1" {
		return
	}
	root := os.Getenv("SB_RETRY_INTENT_ROOT")
	boundary := os.Getenv("SB_RETRY_INTENT_BOUNDARY")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	target := provider.RouteTarget{Provider: "test", Surface: "local", ModelID: "model"}
	source, err := store.Create(workspace, target.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	opening := provider.UserText("crash-safe retry opening")
	if err := source.AppendMessage(opening); err != nil {
		t.Fatal(err)
	}
	if err := source.AppendMessage(provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{provider.Text{Text: "set aside"}}}); err != nil {
		t.Fatal(err)
	}
	child, err := store.ForkSessionForRetryStaged(source, 0)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := child.AppendRetryIntent(source.ID(), 0, opening, "t1", string(target.ID()), strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := child.PublishDurably(); err != nil || !outcome.Visible || !outcome.Durable {
		t.Fatalf("publishing child = %+v, %v", outcome, err)
	}
	switch boundary {
	case "opening", "started":
		marked := provider.CloneMessage(opening)
		marked.RetryIntentID = intent.ID
		if err := child.AppendMessage(marked); err != nil {
			t.Fatal(err)
		}
		if boundary == "started" {
			if err := child.StartRetryIntent(intent.ID); err != nil {
				t.Fatal(err)
			}
		}
	case "completed":
		registry, err := tools.NewRegistry(workspace, execution.Capability{})
		if err != nil {
			t.Fatal(err)
		}
		p := &scriptedProvider{turns: []scriptTurn{textTurn("completed before crash")}}
		loop := &Loop{Session: child, Provider: p, Target: target, Tools: registry,
			Perms: permission.NewEngine(permission.ModeDefault, execution.Capability{})}
		if err := loop.RetryTurnMessage(context.Background(), opening, intent.ID); err != nil {
			t.Fatal(err)
		}
		if err := child.CompleteRetryIntent(intent.ID); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown boundary %q", boundary)
	}
	markRetryIntentCrashReady(t, filepath.Join(root, "ready"))
	select {}
}

func markRetryIntentCrashReady(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("ready\n"); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		t.Fatal(err)
	}
}
