//go:build unix

package lsp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestDocumentSnapshotRejectsFIFOAndOversizeWithoutBlocking(t *testing.T) {
	t.Run("direct FIFO", func(t *testing.T) {
		root := t.TempDir()
		authority := openSnapshotAuthority(t, root)
		path := filepath.Join(root, "source.go")
		if err := unix.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() {
			_, err := readDocumentSnapshot(context.Background(), authority, path)
			result <- err
		}()
		select {
		case err := <-result:
			if err == nil || !strings.Contains(err.Error(), "not a regular file") {
				t.Fatalf("FIFO snapshot error = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("language-server snapshot blocked on a FIFO")
		}
	})

	t.Run("retained parent swapped to outside symlink", func(t *testing.T) {
		root := t.TempDir()
		outside := t.TempDir()
		insideDir := filepath.Join(root, "pkg")
		if err := os.Mkdir(insideDir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(insideDir, "source.go")
		if err := os.WriteFile(path, []byte("package safe"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outside, "source.go"), []byte("package outside"), 0o600); err != nil {
			t.Fatal(err)
		}
		authority := openSnapshotAuthority(t, root)
		if err := os.Rename(insideDir, filepath.Join(root, "pkg-old")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, insideDir); err != nil {
			t.Fatal(err)
		}
		if _, err := readDocumentSnapshot(context.Background(), authority, path); err == nil || !strings.Contains(err.Error(), "outside the workspace") {
			t.Fatalf("ancestor symlink snapshot error = %v", err)
		}
	})

	t.Run("oversize", func(t *testing.T) {
		root := t.TempDir()
		authority := openSnapshotAuthority(t, root)
		path := filepath.Join(root, "source.go")
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(maxDocumentBytes + 1); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := readDocumentSnapshot(context.Background(), authority, path); err == nil || !strings.Contains(err.Error(), "document limit") {
			t.Fatalf("oversize snapshot error = %v", err)
		}
	})
}

func TestWorkspaceSymbolsRejectsAndClearsRetainedDocumentAfterAncestorEscape(t *testing.T) {
	c, server, root := newScriptedServer(t)
	insideDir := filepath.Join(root, "pkg")
	if err := os.Mkdir(insideDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(insideDir, "x.go")
	if err := os.WriteFile(path, []byte("package pkg\nvar Inside = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	opened := asyncDefinition(c, path)
	assertMethod(t, server.recv(t), "textDocument/didOpen")
	request := server.recv(t)
	assertMethod(t, request, "textDocument/definition")
	server.send(t, map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": nil})
	if err := awaitTestError(t, opened); err != nil {
		t.Fatal(err)
	}

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "x.go"), []byte("package stolen\nvar HostSecret = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(insideDir, filepath.Join(root, "pkg-old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, insideDir); err != nil {
		t.Fatal(err)
	}
	c.setCapabilities(Capabilities{
		PositionEncoding: PositionEncodingUTF16,
		Sync:             SyncOptions{OpenClose: true, Change: SyncFull},
		WorkspaceSymbols: true,
	})

	done := make(chan error, 1)
	go func() {
		_, _, err := c.WorkspaceSymbols(context.Background(), "HostSecret", 10)
		done <- err
	}()
	closed := server.recv(t)
	assertMethod(t, closed, "textDocument/didClose")
	if err := awaitTestError(t, done); err == nil || !strings.Contains(err.Error(), "outside the workspace") {
		t.Fatalf("workspace symbols ancestor-escape error = %v", err)
	}

	c.documentsMu.Lock()
	_, retained := c.documents[path]
	c.documentsMu.Unlock()
	if retained {
		t.Fatal("escaped retained document remained in the client after refusal")
	}
}
