//go:build unix

package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	fifoHelperEnv       = "SWITCHBOARD_TEST_SESSION_FIFO_HELPER"
	fifoHelperRootEnv   = "SWITCHBOARD_TEST_SESSION_FIFO_ROOT"
	fifoHelperWorkEnv   = "SWITCHBOARD_TEST_SESSION_FIFO_WORKSPACE"
	fifoHelperIDEnv     = "SWITCHBOARD_TEST_SESSION_FIFO_ID"
	fifoHelperActionEnv = "SWITCHBOARD_TEST_SESSION_FIFO_ACTION"
)

func TestSessionFIFOHelperProcess(t *testing.T) {
	if os.Getenv(fifoHelperEnv) != "1" {
		t.Skip("subprocess helper")
	}
	store, err := NewStore(os.Getenv(fifoHelperRootEnv))
	if err != nil {
		t.Fatal(err)
	}
	workspace := os.Getenv(fifoHelperWorkEnv)
	id := os.Getenv(fifoHelperIDEnv)
	path := filepath.Join(store.root, workspaceKey(workspace), id+".log")

	switch os.Getenv(fifoHelperActionEnv) {
	case "list":
		infos, err := store.List(workspace)
		if err != nil || len(infos) != 0 {
			t.Fatalf("List FIFO = %+v, %v", infos, err)
		}
	case "list-all":
		all, err := store.ListAll()
		if err != nil || len(all[workspace]) != 0 {
			t.Fatalf("ListAll FIFO = %+v, %v", all[workspace], err)
		}
	case "open":
		if opened, err := store.Open(id); err == nil {
			_ = opened.Close()
			t.Fatal("Open accepted FIFO")
		}
	case "read":
		if _, err := ReadState(path); err == nil {
			t.Fatal("ReadState accepted FIFO")
		}
	case "status":
		if published, err := PublicationStatus(path); err == nil || published {
			t.Fatalf("PublicationStatus accepted FIFO: published=%v err=%v", published, err)
		}
	case "fork":
		if fork, err := store.Fork(id, 1); err == nil {
			_ = fork.Close()
			t.Fatal("Fork accepted FIFO")
		}
	default:
		t.Fatalf("unknown helper action %q", os.Getenv(fifoHelperActionEnv))
	}
}

func runSessionFIFOChecks(t *testing.T, store *Store, workspace, id string) {
	t.Helper()
	for _, action := range []string{"list", "list-all", "open", "read", "status", "fork"} {
		t.Run(action, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSessionFIFOHelperProcess$")
			cmd.Env = append(os.Environ(),
				fifoHelperEnv+"=1",
				fifoHelperRootEnv+"="+store.root,
				fifoHelperWorkEnv+"="+workspace,
				fifoHelperIDEnv+"="+id,
				fifoHelperActionEnv+"="+action,
			)
			output, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("%s blocked on a FIFO: %v", action, ctx.Err())
			}
			if err != nil {
				t.Fatalf("%s helper failed: %v\n%s", action, err, output)
			}
		})
	}
}

func TestSessionFIFOCannotBlockDiscoveryOpenReadOrFork(t *testing.T) {
	store, workspace := newStore(t)
	dir, err := store.WorkspaceDir(workspace)
	if err != nil {
		t.Fatal(err)
	}
	id := invalidCandidateID
	path := filepath.Join(dir, id+".log")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Skipf("FIFO unavailable: %v", err)
	}
	runSessionFIFOChecks(t, store, workspace, id)
}

func TestPublicationMarkerFIFOCannotBlockDiscoveryStatusOrRead(t *testing.T) {
	store, workspace := newStore(t)
	staged, err := store.CreateStaged(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	id, markerPath := staged.ID(), publicationMarkerPath(staged.Path())
	if err := staged.Close(); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(markerPath, 0o600); err != nil {
		t.Skipf("FIFO unavailable: %v", err)
	}
	runSessionFIFOChecks(t, store, workspace, id)
}
