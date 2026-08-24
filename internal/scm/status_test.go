package scm

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestParsePorcelainV2Z(t *testing.T) {
	data := []byte(
		"1 MM N... 100644 100644 100644 a b normal.txt\x00" +
			"2 R. S.M. 160000 160000 160000 c d R100 nested/new\nname\x00nested/old name\x00" +
			"u UU N... 100644 100644 100644 100644 e f g conflict.txt\x00" +
			"? -dash.txt\x00" +
			"! ignored.log\x00",
	)

	states, err := ParsePorcelainV2Z(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 5 {
		t.Fatalf("len(states) = %d, want 5", len(states))
	}

	normal := stateByPath(t, states, "normal.txt")
	if !normal.Tracked || !normal.Staged || !normal.Unstaged || normal.XY != "MM" {
		t.Fatalf("ordinary state = %+v", normal)
	}
	rename := stateByPath(t, states, "nested/new\nname")
	if rename.Kind != EntryRename || !rename.Renamed || rename.Copied || !rename.Nested || rename.OriginalPath != "nested/old name" || rename.RenameScore != "R100" {
		t.Fatalf("rename state = %+v", rename)
	}
	conflict := stateByPath(t, states, "conflict.txt")
	if conflict.Kind != EntryUnmerged || !conflict.Unmerged || !conflict.Staged || !conflict.Unstaged {
		t.Fatalf("conflict state = %+v", conflict)
	}
	untracked := stateByPath(t, states, "-dash.txt")
	if untracked.Kind != EntryUntracked || !untracked.Untracked || untracked.Tracked {
		t.Fatalf("untracked state = %+v", untracked)
	}
	ignored := stateByPath(t, states, "ignored.log")
	if ignored.Kind != EntryIgnored || !ignored.Ignored {
		t.Fatalf("ignored state = %+v", ignored)
	}
}

func TestParsePorcelainV2ZRejectsMalformedInput(t *testing.T) {
	tests := [][]byte{
		[]byte("1 M. N... 100644 100644 100644 a b missing-nul"),
		[]byte("1 M N... 100644 100644 100644 a b short-xy\x00"),
		[]byte("1 M. bad 100644 100644 100644 a b bad-submodule\x00"),
		[]byte("1 M. SXYZ 100644 100644 100644 a b bad-submodule-flags\x00"),
		[]byte("2 R. N... 100644 100644 100644 a b broken renamed\x00old\x00"),
		[]byte("2 R. N... 100644 100644 100644 a b R101 renamed\x00old\x00"),
		[]byte("2 R. N... 100644 100644 100644 a b R100 renamed\x00"),
		[]byte("2 R. N... 100644 100644 100644 a b R100 renamed\x00\x00"),
		[]byte("x unknown\x00"),
		[]byte("? \x00"),
	}
	for _, input := range tests {
		if _, err := ParsePorcelainV2Z(input); !errors.Is(err, ErrMalformedStatus) {
			t.Errorf("ParsePorcelainV2Z(%q) error = %v, want ErrMalformedStatus", input, err)
		}
	}
}

func TestParsePorcelainV2ZBoundsEntryCount(t *testing.T) {
	record := []byte("? x\x00")
	input := bytes.Repeat(record, maxStatusEntries+1)
	if _, err := ParsePorcelainV2Z(input); !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("error = %v, want ErrOutputLimit", err)
	}
}

func FuzzParsePorcelainV2Z(f *testing.F) {
	f.Add([]byte(""))
	f.Add([]byte("? untracked.txt\x00"))
	f.Add([]byte("1 M. N... 100644 100644 100644 a b tracked.txt\x00"))
	f.Add([]byte("2 R. N... 100644 100644 100644 a b R100 new.txt\x00old.txt\x00"))
	f.Fuzz(func(t *testing.T, input []byte) {
		states, err := ParsePorcelainV2Z(input)
		if err != nil {
			return
		}
		if len(states) > maxStatusEntries {
			t.Fatalf("parser returned %d entries", len(states))
		}
		for _, state := range states {
			if state.Path == "" {
				t.Fatal("parser returned an empty path")
			}
		}
	})
}

func TestStatusMixedStatesAndOddPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Win32 paths cannot represent this odd-filename fixture")
	}
	root := initTestRepo(t)
	writeTestFile(t, root, ".gitignore", []byte("ignored.log\n"))
	writeTestFile(t, root, "mixed.txt", []byte("base\n"))
	writeTestFile(t, root, "old name.txt", []byte("rename me\n"))
	commitTestFiles(t, root, "base")

	writeTestFile(t, root, "mixed.txt", []byte("staged\n"))
	gitTest(t, root, "add", "--", "mixed.txt")
	writeTestFile(t, root, "mixed.txt", []byte("worktree\n"))
	gitTest(t, root, "mv", "--", "old name.txt", "new\nname.txt")
	odd := []string{"-dash.txt", ":(glob)*.txt", "line\nbreak.txt", " leading.txt"}
	for _, name := range odd {
		writeTestFile(t, root, name, []byte(name+"\n"))
	}
	writeTestFile(t, root, "ignored.log", []byte("ignored\n"))

	repo := openTestRepo(t, root)
	states, err := repo.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mixed := stateByPath(t, states, "mixed.txt")
	if mixed.XY != "MM" || !mixed.Staged || !mixed.Unstaged {
		t.Fatalf("mixed state = %+v", mixed)
	}
	rename := stateByPath(t, states, "new\nname.txt")
	if !rename.Renamed || rename.OriginalPath != "old name.txt" {
		t.Fatalf("rename state = %+v", rename)
	}
	for _, name := range odd {
		state := stateByPath(t, states, name)
		if !state.Untracked {
			t.Fatalf("odd path state = %+v", state)
		}
	}
	if state := stateByPath(t, states, "ignored.log"); !state.Ignored {
		t.Fatalf("ignored state = %+v", state)
	}

	selected, err := repo.StatusPaths(context.Background(), odd)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != len(odd) {
		t.Fatalf("literal status returned %d paths, want %d: %+v", len(selected), len(odd), selected)
	}
	for _, name := range odd {
		stateByPath(t, selected, name)
	}
}

func TestStatusUnmerged(t *testing.T) {
	root := initTestRepo(t)
	writeTestFile(t, root, "conflict.txt", []byte("base\n"))
	commitTestFiles(t, root, "base")
	gitTest(t, root, "checkout", "--quiet", "-b", "other")
	writeTestFile(t, root, "conflict.txt", []byte("other\n"))
	commitTestFiles(t, root, "other")
	gitTest(t, root, "checkout", "--quiet", "main")
	writeTestFile(t, root, "conflict.txt", []byte("main\n"))
	commitTestFiles(t, root, "main")
	gitTestFailure(t, root, "merge", "--no-edit", "other")

	states, err := openTestRepo(t, root).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state := stateByPath(t, states, "conflict.txt")
	if state.Kind != EntryUnmerged || !state.Unmerged || state.XY != "UU" {
		t.Fatalf("conflict state = %+v", state)
	}
}

func TestStatusDoesNotRefreshIndex(t *testing.T) {
	root := initTestRepo(t)
	writeTestFile(t, root, "tracked.txt", []byte("base\n"))
	commitTestFiles(t, root, "base")
	writeTestFile(t, root, "tracked.txt", []byte("changed\n"))
	want := indexChecksum(t, root)

	if _, err := openTestRepo(t, root).Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := indexChecksum(t, root); got != want {
		t.Fatalf("index checksum changed: got %x, want %x", got, want)
	}
	requireNoIndexLock(t, root)
}

func stateByPath(t *testing.T, states []PathState, path string) PathState {
	t.Helper()
	for _, state := range states {
		if state.Path == path {
			return state
		}
	}
	t.Fatalf("path %q not found in states %+v", path, states)
	return PathState{}
}

func TestStatusIgnoredDirectory(t *testing.T) {
	root := initTestRepo(t)
	writeTestFile(t, root, ".gitignore", []byte("cache/\n"))
	commitTestFiles(t, root, "ignore")
	writeTestFile(t, root, "cache/value", []byte("ignored\n"))
	states, err := openTestRepo(t, root).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, state := range states {
		if state.Ignored && (state.Path == "cache/" || filepath.Dir(state.Path) == "cache") {
			found = true
		}
	}
	if !found {
		t.Fatalf("ignored directory absent from states: %+v", states)
	}
	if _, err := os.Stat(filepath.Join(root, "cache", "value")); err != nil {
		t.Fatal(err)
	}
}
