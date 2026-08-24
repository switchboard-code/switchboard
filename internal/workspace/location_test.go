package workspace

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLocationReadVerifyAndStaleGuard(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "src", "hello.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("package hello\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	created, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := w.Read("src/hello.go", 0)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Location.Path != "src/hello.go" || doc.Location.Revision.Size != int64(len(doc.Content)) || doc.Mode.Perm() != created.Mode().Perm() {
		t.Fatalf("document = %+v mode %v", doc.Location, doc.Mode)
	}
	if err := w.Verify(doc.Location); err != nil {
		t.Fatalf("fresh location refused: %v", err)
	}
	if err := os.WriteFile(path, []byte("package changed\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := w.Verify(doc.Location); !errors.Is(err, ErrStaleLocation) {
		t.Fatalf("Verify after change = %v", err)
	}
}

func TestRootRefusesSymlinkEscapeAndBinary(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink unavailable: %v", err)
		}
		t.Fatal(err)
	}
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Read("escape/secret", 0); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("escape read = %v", err)
	}
	if _, err := w.ReadBinary("escape/secret", 0); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("binary escape read = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "binary"), []byte{'x', 0, 'y'}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Read("binary", 0); !errors.Is(err, ErrBinary) {
		t.Fatalf("binary read = %v", err)
	}
}

func TestReadBinaryPreservesInternalSymlink(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	want := []byte{'b', 'i', 'n', 0, 1, 2}
	if err := os.WriteFile(filepath.Join(realDir, "data.bin"), want, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(root, "current")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := w.ReadBinary("current/data.bin", 64)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(doc.Content, want) || doc.Location.Path != "real/data.bin" {
		t.Fatalf("binary snapshot = path %q content %v", doc.Location.Path, doc.Content)
	}
}

func TestReadBinaryRefusesParentSwapAfterResolution(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	live := filepath.Join(root, "live")
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(live, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "shot.png"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	const secret = "outside-parent-swap-secret"
	if err := os.WriteFile(filepath.Join(outside, "shot.png"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	alternate := filepath.Join(root, "alternate")
	if err := os.Symlink(outside, alternate); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := w.resolveReadPath("live/shot.png")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(live, filepath.Join(root, "parked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(alternate, live); err != nil {
		t.Fatal(err)
	}

	doc, err := w.readBinaryResolved("live/shot.png", rel, 1024)
	if err == nil {
		t.Fatalf("parent swap was accepted: %q", doc.Content)
	}
	if bytes.Contains(doc.Content, []byte(secret)) {
		t.Fatalf("parent swap returned outside bytes: %q", doc.Content)
	}
}

func TestReadBinaryRefusesWorkspaceRootReplacement(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "shot.png"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, filepath.Join(parent, "original-workspace")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	const secret = "replacement-root-secret"
	if err := os.WriteFile(filepath.Join(root, "shot.png"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := w.ReadBinary("shot.png", 1024)
	if !errors.Is(err, ErrStaleLocation) {
		t.Fatalf("replacement root read = %v, content %q", err, doc.Content)
	}
	if bytes.Contains(doc.Content, []byte(secret)) {
		t.Fatalf("replacement root returned outside identity bytes: %q", doc.Content)
	}
}

func TestReadBinaryRejectsSpecialFileAndInitialOversize(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "large.bin"), []byte("123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.ReadBinary("directory", 8); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("directory read = %v", err)
	}
	if _, err := w.ReadBinary("large.bin", 8); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize read = %v", err)
	}
}

func TestReadBinaryCapsGrowthOnTheOpenedDescriptor(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "growing.bin")
	if err := os.WriteFile(path, []byte("12345678"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := w.resolveReadPath("growing.bin")
	if err != nil {
		t.Fatal(err)
	}
	file, err := openWorkspaceReadFile(w.path, w.identity, rel)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	appender, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := appender.Write([]byte("9")); err != nil {
		_ = appender.Close()
		t.Fatal(err)
	}
	if err := appender.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readInspectedFile(file, info, "growing.bin", rel, 8); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("post-stat growth read = %v", err)
	}
}
