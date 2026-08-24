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

func TestDiffHEADIncludesTrackedAndUntrackedWithoutChangingIndex(t *testing.T) {
	root := initTestRepo(t)
	writeTestFile(t, root, ".gitignore", []byte("ignored.log\n"))
	writeTestFile(t, root, "mixed.txt", []byte("base\n"))
	commitTestFiles(t, root, "base")

	writeTestFile(t, root, "mixed.txt", []byte("staged-only\n"))
	gitTest(t, root, "add", "--", "mixed.txt")
	writeTestFile(t, root, "mixed.txt", []byte("final-worktree\n"))
	writeTestFile(t, root, "notes.txt", []byte("untracked text\n"))
	writeTestFile(t, root, "image.bin", []byte{0, 1, 2, 3, 0xff, 0xfe})
	writeTestFile(t, root, "ignored.log", []byte("do not include\n"))
	wantIndex := indexChecksum(t, root)
	lockPath := filepath.Join(root, ".git", "index.lock")
	lockContent := []byte("pre-existing lock must remain untouched\n")
	if err := os.WriteFile(lockPath, lockContent, 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := openTestRepo(t, root).DiffHEAD(context.Background(), DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Base != "HEAD" || result.Unborn || result.Truncated {
		t.Fatalf("result metadata = %+v", result)
	}
	if got := indexChecksum(t, root); got != wantIndex {
		t.Fatalf("index checksum changed: got %x, want %x", got, wantIndex)
	}
	if got, err := os.ReadFile(lockPath); err != nil {
		t.Fatalf("pre-existing index lock was removed: %v", err)
	} else if !bytes.Equal(got, lockContent) {
		t.Fatalf("pre-existing index lock changed: got %q, want %q", got, lockContent)
	}

	mixed := sectionByPath(t, result, "mixed.txt")
	if mixed.Kind != DiffTracked || !mixed.Status.Staged || !mixed.Status.Unstaged {
		t.Fatalf("mixed section = %+v", mixed)
	}
	mixedPatch := sectionText(t, result, mixed)
	if !bytes.Contains(mixedPatch, []byte("+final-worktree")) || bytes.Contains(mixedPatch, []byte("staged-only")) {
		t.Fatalf("mixed patch does not represent HEAD to final worktree:\n%s", mixedPatch)
	}
	text := sectionByPath(t, result, "notes.txt")
	if text.Kind != DiffUntrackedText || text.Binary || !bytes.Contains(sectionText(t, result, text), []byte("+untracked text")) {
		t.Fatalf("untracked text section = %+v\n%s", text, sectionText(t, result, text))
	}
	binary := sectionByPath(t, result, "image.bin")
	if binary.Kind != DiffUntrackedBinary || !binary.Binary || !bytes.Contains(sectionText(t, result, binary), []byte("GIT binary patch")) {
		t.Fatalf("untracked binary section = %+v\n%s", binary, sectionText(t, result, binary))
	}
	for _, section := range result.Sections {
		if section.Path == "ignored.log" {
			t.Fatal("ignored file was included in the diff")
		}
	}
	if !stateByPath(t, result.Files, "ignored.log").Ignored {
		t.Fatal("ignored status metadata was not retained")
	}
}

func TestDiffHEADUnbornRepository(t *testing.T) {
	root := initTestRepo(t)
	writeTestFile(t, root, "staged.txt", []byte("staged in unborn repo\n"))
	gitTest(t, root, "add", "--", "staged.txt")
	writeTestFile(t, root, "untracked.txt", []byte("untracked in unborn repo\n"))

	result, err := openTestRepo(t, root).DiffHEAD(context.Background(), DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Unborn || result.Base != "empty tree" {
		t.Fatalf("unborn metadata = %+v", result)
	}
	for _, path := range []string{"staged.txt", "untracked.txt"} {
		section := sectionByPath(t, result, path)
		if section.Kind != DiffUntrackedText || !bytes.Contains(sectionText(t, result, section), []byte("new file mode")) {
			t.Fatalf("unborn section %q = %+v\n%s", path, section, sectionText(t, result, section))
		}
	}
}

func TestDiffHEADLiteralOddPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Win32 filenames cannot represent the literal pathspec fixtures")
	}
	root := initTestRepo(t)
	writeTestFile(t, root, "ordinary.txt", []byte("ordinary\n"))
	commitTestFiles(t, root, "base")
	odd := []string{"-dash.txt", ":(glob)*.txt", "line\nbreak.txt", " leading.txt"}
	for _, path := range odd {
		writeTestFile(t, root, path, []byte("content for "+path+"\n"))
	}

	repo := openTestRepo(t, root)
	for _, path := range odd {
		result, err := repo.DiffHEAD(context.Background(), DiffOptions{Paths: []string{path}})
		if err != nil {
			t.Fatalf("DiffHEAD(%q): %v", path, err)
		}
		if len(result.Sections) != 1 || result.Sections[0].Path != path {
			t.Fatalf("DiffHEAD(%q) sections = %+v", path, result.Sections)
		}
	}
}

func TestDiffHEADDisablesExternalDriversTextconvAndFSMonitor(t *testing.T) {
	root := initTestRepo(t)
	writeTestFile(t, root, ".gitattributes", []byte("*.external diff=external\n*.textconv diff=textconv\n"))
	writeTestFile(t, root, "file.external", []byte("external base\n"))
	writeTestFile(t, root, "file.textconv", []byte("textconv base\n"))
	commitTestFiles(t, root, "base")

	externalMarker := filepath.Join(root, "external-invoked")
	textconvMarker := filepath.Join(root, "textconv-invoked")
	fsmonitorMarker := filepath.Join(root, "fsmonitor-invoked")
	environmentMarker := filepath.Join(root, "environment-invoked")
	external := executableScript(t, root, "external-driver.sh", externalMarker)
	textconv := executableScript(t, root, "textconv-driver.sh", textconvMarker)
	fsmonitor := executableScript(t, root, "fsmonitor.sh", fsmonitorMarker)
	environment := executableScript(t, root, "environment-driver.sh", environmentMarker)
	gitTest(t, root, "config", "diff.external.command", external)
	gitTest(t, root, "config", "diff.textconv.textconv", textconv)
	gitTest(t, root, "config", "core.fsmonitor", fsmonitor)
	t.Setenv("GIT_EXTERNAL_DIFF", environment)
	writeTestFile(t, root, "file.external", []byte("external changed\n"))
	writeTestFile(t, root, "file.textconv", []byte("textconv changed\n"))

	result, err := openTestRepo(t, root).DiffHEAD(context.Background(), DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"file.external", "file.textconv"} {
		sectionByPath(t, result, path)
	}
	for _, marker := range []string{externalMarker, textconvMarker, fsmonitorMarker, environmentMarker} {
		if _, err := os.Lstat(marker); err == nil {
			t.Fatalf("malicious Git helper executed and created %s", marker)
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}

func TestDiffHEADBoundedOutput(t *testing.T) {
	root := initTestRepo(t)
	writeTestFile(t, root, "base.txt", []byte("base\n"))
	commitTestFiles(t, root, "base")
	writeTestFile(t, root, "large.txt", bytes.Repeat([]byte("abcdefghij\n"), 1000))

	const limit = 128
	result, err := openTestRepo(t, root).DiffHEAD(context.Background(), DiffOptions{MaxBytes: limit})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Text) > limit {
		t.Fatalf("diff length = %d, limit %d", len(result.Text), limit)
	}
	if !result.Truncated || len(result.Sections) != 1 || !result.Sections[0].Truncated {
		t.Fatalf("bounded diff metadata = %+v", result)
	}
	if len(result.Omitted) != 1 || result.Omitted[0].Path != "large.txt" {
		t.Fatalf("omitted files = %+v, want the partially rendered file", result.Omitted)
	}
	if _, err := openTestRepo(t, root).DiffHEAD(context.Background(), DiffOptions{MaxBytes: MaxDiffBytes + 1}); err == nil {
		t.Fatal("oversized MaxBytes was accepted")
	}
	if _, err := openTestRepo(t, root).DiffHEAD(context.Background(), DiffOptions{MaxBytes: -1}); err == nil {
		t.Fatal("negative MaxBytes was accepted")
	}
}

func TestDiffHEADInventoriesPartialAndLaterFilesAtOutputCap(t *testing.T) {
	root := initTestRepo(t)
	writeTestFile(t, root, ".gitignore", []byte("ignored.log\n"))
	writeTestFile(t, root, "aaa-large.txt", []byte("base\n"))
	writeTestFile(t, root, "middle-tracked.txt", []byte("base\n"))
	commitTestFiles(t, root, "base")

	writeTestFile(t, root, "aaa-large.txt", bytes.Repeat([]byte("changed line\n"), 256))
	writeTestFile(t, root, "middle-tracked.txt", []byte("changed\n"))
	writeTestFile(t, root, "zzz-image.bin", []byte{0, 1, 2, 3, 0xff, 0xfe})
	writeTestFile(t, root, "ignored.log", []byte("not part of the diff\n"))
	wantIndex := indexChecksum(t, root)

	result, err := openTestRepo(t, root).DiffHEAD(context.Background(), DiffOptions{MaxBytes: 128})
	if err != nil {
		t.Fatal(err)
	}
	if got := indexChecksum(t, root); got != wantIndex {
		t.Fatalf("index checksum changed: got %x, want %x", got, wantIndex)
	}
	if !result.Truncated {
		t.Fatalf("large diff was not truncated: %+v", result)
	}
	want := []string{"aaa-large.txt", "middle-tracked.txt", "zzz-image.bin"}
	if len(result.Omitted) != len(want) {
		t.Fatalf("omitted files = %+v, want %q", result.Omitted, want)
	}
	for i, path := range want {
		if result.Omitted[i].Path != path {
			t.Fatalf("omitted[%d] = %q, want %q", i, result.Omitted[i].Path, path)
		}
	}
	for _, file := range result.Omitted {
		if file.Ignored || file.Path == "ignored.log" {
			t.Fatalf("ignored file entered omitted inventory: %+v", result.Omitted)
		}
	}
}

func TestDiffHEADUnmergedFile(t *testing.T) {
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

	result, err := openTestRepo(t, root).DiffHEAD(context.Background(), DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	section := sectionByPath(t, result, "conflict.txt")
	if !section.Status.Unmerged || !bytes.Contains(sectionText(t, result, section), []byte("<<<<<<<")) {
		t.Fatalf("conflict section = %+v\n%s", section, sectionText(t, result, section))
	}
}

func TestDiffHEADNonRepository(t *testing.T) {
	repo := openTestRepo(t, initTestRepo(t))
	repo.Root = t.TempDir()
	_, err := repo.DiffHEAD(context.Background(), DiffOptions{})
	var gitErr *GitError
	if !errors.As(err, &gitErr) {
		t.Fatalf("error = %T %v, want *GitError", err, err)
	}
	if !bytes.Contains(bytes.ToLower([]byte(gitErr.Stderr)), []byte("not a git repository")) {
		t.Fatalf("stderr = %q", gitErr.Stderr)
	}
}

func sectionByPath(t *testing.T, result DiffResult, path string) DiffSection {
	t.Helper()
	for _, section := range result.Sections {
		if section.Path == path {
			return section
		}
	}
	t.Fatalf("path %q not found in sections %+v", path, result.Sections)
	return DiffSection{}
}

func sectionText(t *testing.T, result DiffResult, section DiffSection) []byte {
	t.Helper()
	end := section.Offset + section.Length
	if section.Offset < 0 || section.Length < 0 || end > len(result.Text) {
		t.Fatalf("invalid section range offset=%d length=%d diff=%d", section.Offset, section.Length, len(result.Text))
	}
	return result.Text[section.Offset:end]
}
