package lsp

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	workspacefs "github.com/switchboard-code/switchboard/internal/workspace"
)

func openSnapshotAuthority(t *testing.T, root string) *workspacefs.Root {
	t.Helper()
	authority, err := workspacefs.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func TestReadDocumentSnapshotBoundsTypeSizeEncodingAndContext(t *testing.T) {
	root := t.TempDir()
	authority := openSnapshotAuthority(t, root)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readDocumentSnapshot(canceled, authority, filepath.Join(root, "missing")); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled snapshot error = %v", err)
	}

	if _, err := readDocumentSnapshot(context.Background(), authority, root); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory snapshot error = %v", err)
	}

	invalid := filepath.Join(root, "invalid.go")
	if err := os.WriteFile(invalid, []byte{0xff, 0xfe}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readDocumentSnapshot(context.Background(), authority, invalid); err == nil || !strings.Contains(err.Error(), "not valid UTF-8") {
		t.Fatalf("invalid UTF-8 snapshot error = %v", err)
	}

	huge := filepath.Join(root, "huge.go")
	file, err := os.Create(huge)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxDocumentBytes + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readDocumentSnapshot(context.Background(), authority, huge); err == nil || !strings.Contains(err.Error(), "document limit") {
		t.Fatalf("oversized snapshot error = %v", err)
	}
}

func TestWriteRejectsJSONExpandedFramesBeforeWriting(t *testing.T) {
	writer := &failFirstWriteCloser{}
	c := &Client{in: writer}
	payload := bytes.Repeat([]byte{'x'}, maxLSPMessageBytes+1)
	if err := c.write(payload); err == nil || !strings.Contains(err.Error(), "request is") {
		t.Fatalf("oversized write error = %v", err)
	}
	if writer.Len() != 0 {
		t.Fatalf("oversized payload wrote %d bytes before rejection", writer.Len())
	}
}

func TestReadDocumentSnapshotFeedsOneImmutableByteSlice(t *testing.T) {
	root := t.TempDir()
	authority := openSnapshotAuthority(t, root)
	path := filepath.Join(root, "a.go")
	want := []byte("package a\nvar 😀Thing = 1\n")
	if err := os.WriteFile(path, want, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readDocumentSnapshot(context.Background(), authority, path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("snapshot = %q, want %q", got, want)
	}
	if err := os.WriteFile(path, []byte("changed later"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("returned snapshot aliased later disk content: %q", got)
	}
}

func TestReadDocumentSnapshotRejectsOutsideAndReplacedWorkspaceAuthority(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	authority := openSnapshotAuthority(t, root)
	outside := filepath.Join(parent, "outside.go")
	if err := os.WriteFile(outside, []byte("package outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readDocumentSnapshot(context.Background(), authority, outside); !errors.Is(err, workspacefs.ErrOutsideRoot) {
		t.Fatalf("outside snapshot error = %v, want ErrOutsideRoot", err)
	}

	moved := filepath.Join(parent, "workspace-moved")
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(root, "a.go")
	if err := os.WriteFile(replacement, []byte("package replacement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readDocumentSnapshot(context.Background(), authority, replacement); !errors.Is(err, workspacefs.ErrStaleLocation) {
		t.Fatalf("replaced-root snapshot error = %v, want ErrStaleLocation", err)
	}
}
