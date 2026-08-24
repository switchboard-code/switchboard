package rootedfs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeWalkFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func openWalkRoot(t *testing.T, path string) *os.Root {
	t.Helper()
	root, err := OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

func walkTestLimits(entries int) WalkLimits {
	return WalkLimits{MaxEntries: entries, MaxDirectories: 32, MaxDepth: 8, ReadDirBatch: 3}
}

func TestWalkRegularFilesDistinguishesExactEntryCapFromUnknownTail(t *testing.T) {
	for _, tc := range []struct {
		name        string
		files       int
		wantPartial bool
	}{
		{name: "exact", files: 4},
		{name: "plus one", files: 5, wantPartial: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for n := 0; n < tc.files; n++ {
				writeWalkFile(t, filepath.Join(dir, string(rune('a'+n))))
			}
			var visited []string
			status, err := WalkRegularFiles(context.Background(), openWalkRoot(t, dir), ".", walkTestLimits(4), nil,
				func(relative string, _ *os.Root, _ string, _ fs.FileInfo) error {
					visited = append(visited, relative)
					return nil
				})
			if err != nil {
				t.Fatal(err)
			}
			if len(visited) != 4 || status.Entries != 4 {
				t.Fatalf("visited=%v status=%+v", visited, status)
			}
			if status.Partial() != tc.wantPartial || status.EntryLimit != tc.wantPartial {
				t.Fatalf("status=%+v, want partial=%t", status, tc.wantPartial)
			}
		})
	}
}

func TestWalkRegularFilesDirectoryAndDepthLimitsAreExplicit(t *testing.T) {
	t.Run("directory exact and plus one", func(t *testing.T) {
		for _, tc := range []struct {
			name        string
			directories []string
			wantPartial bool
		}{
			{name: "exact", directories: []string{"a"}},
			{name: "plus one", directories: []string{"a", "b"}, wantPartial: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dir := t.TempDir()
				for _, child := range tc.directories {
					writeWalkFile(t, filepath.Join(dir, child, "file"))
				}
				limits := walkTestLimits(20)
				limits.MaxDirectories = 2 // base plus one child
				status, err := WalkRegularFiles(context.Background(), openWalkRoot(t, dir), ".", limits, nil,
					func(string, *os.Root, string, fs.FileInfo) error { return nil })
				if err != nil {
					t.Fatal(err)
				}
				if status.Partial() != tc.wantPartial || status.DirectoryLimit != tc.wantPartial {
					t.Fatalf("status=%+v, want partial=%t", status, tc.wantPartial)
				}
			})
		}
	})

	t.Run("depth", func(t *testing.T) {
		dir := t.TempDir()
		writeWalkFile(t, filepath.Join(dir, "a", "deep", "file"))
		limits := walkTestLimits(20)
		limits.MaxDepth = 1
		status, err := WalkRegularFiles(context.Background(), openWalkRoot(t, dir), ".", limits, nil,
			func(string, *os.Root, string, fs.FileInfo) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if !status.Partial() || !status.DepthLimit {
			t.Fatalf("depth-limited status=%+v", status)
		}
	})
}

func TestWalkRegularFilesBoundsHugeNonmatchingFanout(t *testing.T) {
	dir := t.TempDir()
	const files = 2048
	for n := 0; n < files; n++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("nonmatch-%04d", n)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	limits := walkTestLimits(129)
	limits.ReadDirBatch = 17
	visited := 0
	status, err := WalkRegularFiles(context.Background(), openWalkRoot(t, dir), ".", limits, nil,
		func(string, *os.Root, string, fs.FileInfo) error {
			visited++
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if visited != limits.MaxEntries || status.Entries != limits.MaxEntries || !status.EntryLimit || !status.Partial() {
		t.Fatalf("visited=%d status=%+v", visited, status)
	}
}

func TestWalkRegularFilesPolicySkipAndEarlyStopHaveDistinctCoverage(t *testing.T) {
	dir := t.TempDir()
	writeWalkFile(t, filepath.Join(dir, "ignored", "file"))
	writeWalkFile(t, filepath.Join(dir, "kept"))
	root := openWalkRoot(t, dir)

	var visited []string
	status, err := WalkRegularFiles(context.Background(), root, ".", walkTestLimits(20),
		func(relative string, _ fs.FileInfo) bool { return filepath.Base(relative) == "ignored" },
		func(relative string, _ *os.Root, _ string, _ fs.FileInfo) error {
			visited = append(visited, filepath.Base(relative))
			return nil
		})
	if err != nil || status.Partial() || !slices.Equal(visited, []string{"kept"}) {
		t.Fatalf("policy skip: visited=%v status=%+v err=%v", visited, status, err)
	}

	status, err = WalkRegularFiles(context.Background(), root, ".", walkTestLimits(20), nil,
		func(string, *os.Root, string, fs.FileInfo) error { return fs.SkipAll })
	if err != nil || !status.StoppedEarly || !status.Partial() {
		t.Fatalf("early stop status=%+v err=%v", status, err)
	}
}

func TestWalkRegularFilesAppliesPolicySkipToBase(t *testing.T) {
	dir := t.TempDir()
	writeWalkFile(t, filepath.Join(dir, "ignored", "file"))
	root := openWalkRoot(t, dir)
	visited := false
	status, err := WalkRegularFiles(context.Background(), root, "ignored", walkTestLimits(20),
		func(relative string, _ fs.FileInfo) bool { return relative == "ignored" },
		func(string, *os.Root, string, fs.FileInfo) error {
			visited = true
			return nil
		})
	if err != nil || visited || status.Partial() {
		t.Fatalf("base policy skip: visited=%t status=%+v err=%v", visited, status, err)
	}
}

func TestWalkRegularFilesDetectsMutationInsideSkipCallback(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	probe := filepath.Join(t.TempDir(), "symlink-probe")
	if err := os.Symlink(outside, probe); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	writeWalkFile(t, filepath.Join(dir, "ignored", "inside"))
	writeWalkFile(t, filepath.Join(outside, "outside-secret"))
	var swapErr error
	visited := false
	status, err := WalkRegularFiles(context.Background(), openWalkRoot(t, dir), ".", walkTestLimits(20),
		func(relative string, _ fs.FileInfo) bool {
			if filepath.Base(relative) != "ignored" {
				return false
			}
			swapErr = os.Rename(filepath.Join(dir, "ignored"), filepath.Join(dir, "ignored-moved"))
			if swapErr == nil {
				swapErr = os.Symlink(outside, filepath.Join(dir, "ignored"))
			}
			return true
		},
		func(string, *os.Root, string, fs.FileInfo) error {
			visited = true
			return nil
		})
	if swapErr != nil {
		t.Fatal(swapErr)
	}
	if err != nil || visited || !status.Partial() || !status.AuthorityChanged || status.Omitted == 0 {
		t.Fatalf("skip race: visited=%t status=%+v err=%v", visited, status, err)
	}
}

func TestWalkRegularFilesRefusesChildReplacementSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	probe := filepath.Join(t.TempDir(), "symlink-probe")
	if err := os.Symlink(outside, probe); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	writeWalkFile(t, filepath.Join(dir, "child", "inside"))
	writeWalkFile(t, filepath.Join(outside, "outside-secret"))
	var swapErr error
	var visited []string
	status, err := WalkRegularFiles(context.Background(), openWalkRoot(t, dir), ".", walkTestLimits(20),
		func(relative string, _ fs.FileInfo) bool {
			if filepath.Base(relative) != "child" {
				return false
			}
			swapErr = os.Rename(filepath.Join(dir, "child"), filepath.Join(dir, "child-moved"))
			if swapErr == nil {
				swapErr = os.Symlink(outside, filepath.Join(dir, "child"))
			}
			return false
		},
		func(relative string, _ *os.Root, _ string, _ fs.FileInfo) error {
			visited = append(visited, relative)
			return nil
		})
	if swapErr != nil {
		t.Fatal(swapErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if !status.Partial() || status.Omitted == 0 {
		t.Fatalf("replacement was not reported: %+v", status)
	}
	for _, path := range visited {
		if strings.Contains(path, "outside-secret") {
			t.Fatalf("walk followed replacement symlink: %v", visited)
		}
	}
}

func TestWalkRegularFilesRejectsInvalidInputs(t *testing.T) {
	dir := t.TempDir()
	writeWalkFile(t, filepath.Join(dir, "file"))
	root := openWalkRoot(t, dir)
	visit := func(string, *os.Root, string, fs.FileInfo) error { return nil }
	if _, err := WalkRegularFiles(context.Background(), root, "../outside", walkTestLimits(1), nil, visit); err == nil {
		t.Fatal("outside base accepted")
	}
	if _, err := WalkRegularFiles(context.Background(), root, ".", WalkLimits{}, nil, visit); err == nil {
		t.Fatal("zero limits accepted")
	}
	if _, err := WalkRegularFiles(context.Background(), root, ".", walkTestLimits(1), nil, nil); err == nil {
		t.Fatal("nil visitor accepted")
	}
	if _, err := WalkRegularFiles(context.Background(), root, ".", walkTestLimits(1), nil,
		func(string, *os.Root, string, fs.FileInfo) error { return errors.New("visitor") }); err == nil {
		t.Fatal("visitor error was dropped")
	}
}
