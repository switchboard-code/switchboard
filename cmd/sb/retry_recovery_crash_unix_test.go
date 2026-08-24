//go:build unix

package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/checkpoint"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

const retryPublicationCrashHelper = "SB_RETRY_PUBLICATION_CRASH_HELPER"

func TestRetryRecoveryUsesActualPublicationCommitAfterSIGKILL(t *testing.T) {
	for _, test := range []struct {
		boundary  string
		published bool
		wantFile  string
	}{
		{boundary: "before-publish", wantFile: "after"},
		{boundary: "after-publish", published: true, wantFile: "before"},
	} {
		t.Run(test.boundary, func(t *testing.T) {
			root := t.TempDir()
			ready := filepath.Join(root, "ready")
			var output bytes.Buffer
			command := exec.Command(os.Args[0], "-test.run=^TestRetryPublicationCrashHelper$")
			command.Env = append(os.Environ(),
				retryPublicationCrashHelper+"=1",
				"SB_RETRY_PUBLICATION_ROOT="+root,
				"SB_RETRY_PUBLICATION_BOUNDARY="+test.boundary,
			)
			command.Stdout = &output
			command.Stderr = &output
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, err := os.Stat(ready); err == nil {
					break
				} else if !errors.Is(err, os.ErrNotExist) {
					_ = command.Process.Kill()
					_ = command.Wait()
					t.Fatal(err)
				}
				if time.Now().After(deadline) {
					_ = command.Process.Kill()
					_ = command.Wait()
					t.Fatalf("helper did not reach %s:\n%s", test.boundary, output.String())
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := command.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := command.Wait(); err == nil {
				t.Fatal("SIGKILL helper exited successfully")
			}

			workspace := filepath.Join(root, "workspace")
			store, err := session.NewStore(filepath.Join(root, "sessions"))
			if err != nil {
				t.Fatal(err)
			}
			recovery, err := recoverInterruptedRetry(store, workspace)
			if err != nil {
				t.Fatal(err)
			}
			if !recovery.Found || recovery.Published != test.published {
				t.Fatalf("recovery = %+v, want published=%v", recovery, test.published)
			}
			id, status, found, unresolvedErr := store.UnresolvedRetry(workspace)
			if unresolvedErr != nil {
				t.Fatal(unresolvedErr)
			}
			if test.published {
				if !found || id == "" || status != session.RetryIntentPending {
					t.Fatalf("published retry handoff = id %q status %q found %v", id, status, found)
				}
			} else if found {
				t.Fatalf("unpublished retry intent became discoverable as %s", id)
			}
			data, err := os.ReadFile(filepath.Join(workspace, "target.txt"))
			if err != nil || string(data) != test.wantFile {
				t.Fatalf("workspace file = %q, err=%v, want %q", data, err, test.wantFile)
			}
		})
	}
}

func TestRetryPublicationCrashHelper(t *testing.T) {
	if os.Getenv(retryPublicationCrashHelper) != "1" {
		return
	}
	root := os.Getenv("SB_RETRY_PUBLICATION_ROOT")
	boundary := os.Getenv("SB_RETRY_PUBLICATION_BOUNDARY")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	targetID := provider.RouteTargetID("test/local/model")
	source, err := store.Create(workspace, targetID, "test")
	if err != nil {
		t.Fatal(err)
	}
	opening := provider.UserText("retry publication crash")
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
	if _, err := child.AppendRetryIntent(source.ID(), 0, opening, "t1", string(targetID), strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(workspace, "target.txt")
	if err := os.WriteFile(target, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	recorder := checkpoint.NewRecorder()
	identity := checkpoint.TurnIdentity{SessionID: source.ID(), OpeningMessage: 0}
	recorder.BeginTurn(identity.SessionID, identity.OpeningMessage, "retry publication crash")
	recorder.Record(target)
	if err := os.WriteFile(target, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	journalDir, err := store.WorkspaceDir(workspace)
	if err != nil {
		t.Fatal(err)
	}
	childIdentity, err := child.PublicationRecoveryIdentity()
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := recorder.PrepareDurableUndoCurrent(identity, journalDir, workspace, child.Path(), childIdentity)
	if err != nil {
		t.Fatal(err)
	}
	_, err = prepared.ApplyAndCommit(func() error {
		if boundary == "before-publish" {
			markRetryPublicationCrashBoundary(t, filepath.Join(root, "ready"))
			select {}
		}
		if err := child.Publish(); err != nil {
			return err
		}
		if boundary == "after-publish" {
			markRetryPublicationCrashBoundary(t, filepath.Join(root, "ready"))
			select {}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("helper completed without reaching boundary %q", boundary)
}

func markRetryPublicationCrashBoundary(t *testing.T, path string) {
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
