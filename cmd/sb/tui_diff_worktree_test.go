package main

import (
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/scm"
	"github.com/switchboard-code/switchboard/internal/trust"
)

func TestOpenDiffIncludesOnlyUntrackedFile(t *testing.T) {
	root := initTUIDiffRepo(t)
	writeTUIDiffFile(t, root, "base.txt", []byte("base\n"))
	commitTUIDiffFiles(t, root)
	writeTUIDiffFile(t, root, "notes.txt", []byte("untracked text\n"))

	msg := runOpenDiff(t, root)
	if msg.err != nil {
		t.Fatal(msg.err)
	}
	text := plainTUIDiff(msg)
	if strings.Contains(text, "working tree clean") {
		t.Fatalf("an untracked-only worktree was reported clean:\n%s", text)
	}
	for _, want := range []string{"diff --git", "notes.txt", "+untracked text"} {
		if !strings.Contains(text, want) {
			t.Fatalf("untracked diff is missing %q:\n%s", want, text)
		}
	}
}

func TestOpenDiffIncludesMixedChangesAndScopesNestedWorkspace(t *testing.T) {
	root := initTUIDiffRepo(t)
	writeTUIDiffFile(t, root, "inside/mixed.txt", []byte("base\n"))
	writeTUIDiffFile(t, root, "outside.txt", []byte("outside base\n"))
	commitTUIDiffFiles(t, root)

	writeTUIDiffFile(t, root, "inside/mixed.txt", []byte("staged value\n"))
	runTUIDiffGit(t, root, "add", "--", "inside/mixed.txt")
	writeTUIDiffFile(t, root, "inside/mixed.txt", []byte("worktree value\n"))
	writeTUIDiffFile(t, root, "inside/notes.txt", []byte("notes value\n"))
	writeTUIDiffFile(t, root, "inside/image.bin", []byte{0, 1, 2, 3, 0xff, 0xfe})
	writeTUIDiffFile(t, root, "outside.txt", []byte("must stay out\n"))
	wantIndex := tuiDiffIndexChecksum(t, root)

	msg := runOpenDiff(t, filepath.Join(root, "inside"))
	if msg.err != nil {
		t.Fatal(msg.err)
	}
	if got := tuiDiffIndexChecksum(t, root); got != wantIndex {
		t.Fatalf("/diff changed the Git index: got %x, want %x", got, wantIndex)
	}
	text := plainTUIDiff(msg)
	for _, want := range []string{
		"inside/mixed.txt",
		"+worktree value",
		"inside/notes.txt",
		"+notes value",
		"inside/image.bin",
		"GIT binary patch",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("mixed diff is missing %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"staged value", "outside.txt", "must stay out"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("nested workspace diff unexpectedly contains %q:\n%s", unwanted, text)
		}
	}
}

func TestOpenDiffReportsCleanWorktree(t *testing.T) {
	root := initTUIDiffRepo(t)
	writeTUIDiffFile(t, root, "clean.txt", []byte("clean\n"))
	commitTUIDiffFiles(t, root)

	msg := runOpenDiff(t, root)
	if msg.err != nil {
		t.Fatal(msg.err)
	}
	if got := strings.TrimSpace(plainTUIDiff(msg)); got != "working tree clean" {
		t.Fatalf("clean diff = %q", got)
	}
}

func TestOpenDiffNonRepositoryRetainsGitDiagnostic(t *testing.T) {
	msg := runOpenDiff(t, t.TempDir())
	if msg.err == nil {
		t.Fatal("non-repository diff unexpectedly succeeded")
	}
	got := strings.ToLower(msg.err.Error())
	for _, want := range []string{"not a git worktree", "not a git repository"} {
		if !strings.Contains(got, want) {
			t.Fatalf("non-repository error %q does not contain %q", msg.err, want)
		}
	}
}

func TestRenderSCMDiffMarksTruncationAndNonTextChanges(t *testing.T) {
	truncated := renderSCMDiff(scm.DiffResult{
		Text:      []byte("partial"),
		Truncated: true,
		Omitted: []scm.PathState{
			{Path: "zzz-last.txt", Untracked: true},
			{Path: "aaa-partial.txt", Tracked: true, Unstaged: true},
		},
	})
	if !strings.Contains(truncated, "diff truncated at 1 MiB") {
		t.Fatalf("truncated diff has no explicit marker: %q", truncated)
	}
	for _, want := range []string{
		"files not fully shown (2)",
		"unstaged  aaa-partial.txt",
		"untracked  zzz-last.txt",
	} {
		if !strings.Contains(truncated, want) {
			t.Fatalf("truncated diff inventory is missing %q: %q", want, truncated)
		}
	}
	if strings.Index(truncated, "aaa-partial.txt") > strings.Index(truncated, "zzz-last.txt") {
		t.Fatalf("truncated diff inventory is not deterministic: %q", truncated)
	}

	withoutPatch := renderSCMDiff(scm.DiffResult{Files: []scm.PathState{{
		Path:      "empty.txt",
		Untracked: true,
	}}})
	if strings.Contains(withoutPatch, "working tree clean") || !strings.Contains(withoutPatch, "untracked  empty.txt") {
		t.Fatalf("non-text change was rendered dishonestly: %q", withoutPatch)
	}
}

func TestOpenDiffNamesPartialAndLaterMixedFilesAfterCap(t *testing.T) {
	root := initTUIDiffRepo(t)
	writeTUIDiffFile(t, root, "aaa-large.txt", []byte("base\n"))
	writeTUIDiffFile(t, root, "middle-tracked.txt", []byte("base\n"))
	commitTUIDiffFiles(t, root)

	writeTUIDiffFile(t, root, "aaa-large.txt", []byte(strings.Repeat(strings.Repeat("x", 2048)+"\n", 600)))
	writeTUIDiffFile(t, root, "middle-tracked.txt", []byte("changed\n"))
	writeTUIDiffFile(t, root, "zzz-image.bin", []byte{0, 1, 2, 3, 0xff, 0xfe})
	wantIndex := tuiDiffIndexChecksum(t, root)

	msg := runOpenDiff(t, root)
	if msg.err != nil {
		t.Fatal(msg.err)
	}
	if got := tuiDiffIndexChecksum(t, root); got != wantIndex {
		t.Fatalf("/diff changed the Git index: got %x, want %x", got, wantIndex)
	}
	text := plainTUIDiff(msg)
	for _, want := range []string{
		"diff truncated at 1 MiB",
		"files not fully shown (3)",
		"aaa-large.txt",
		"middle-tracked.txt",
		"untracked  zzz-image.bin",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("capped mixed diff is missing %q:\n%s", want, text)
		}
	}
}

func TestRenderDiffOmittedIsBoundedDeterministicAndTerminalSafe(t *testing.T) {
	files := make([]scm.PathState, 200)
	for i := range files {
		files[i] = scm.PathState{
			Path:      itoa(199-i) + "\n\x1b]52;c;spoof\a\u202e-" + strings.Repeat("long-name-", 80) + ".txt",
			Untracked: true,
		}
	}

	first := renderDiffOmitted(files)
	reversed := append([]scm.PathState(nil), files...)
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	second := renderDiffOmitted(reversed)
	if first != second {
		t.Fatal("omitted inventory depends on input order")
	}
	if len(first) > tuiDiffInventoryMaxBytes {
		t.Fatalf("omitted inventory is %d bytes, cap %d", len(first), tuiDiffInventoryMaxBytes)
	}
	rendered := renderSCMDiff(scm.DiffResult{
		Text:      []byte(strings.Repeat("x", tuiDiffPatchMaxBytes)),
		Truncated: true,
		Omitted:   files,
	})
	if len(rendered) > tuiDiffMaxBytes {
		t.Fatalf("rendered diff is %d bytes, cap %d", len(rendered), tuiDiffMaxBytes)
	}
	for _, unsafe := range []string{"\n\x1b]52;c;spoof", "\a", "\u202e"} {
		if strings.Contains(first, unsafe) {
			t.Fatalf("omitted inventory retained terminal control %q: %q", unsafe, first)
		}
	}
	for _, want := range []string{"files not fully shown (200)", "total not fully shown", `\x0a\x1b]52;c;spoof\x07\u202e-`} {
		if !strings.Contains(first, want) {
			t.Fatalf("bounded inventory is missing %q: %q", want, first)
		}
	}
}

func runOpenDiff(t *testing.T, workspace string) diffLoadedMsg {
	t.Helper()
	cmd := openDiff(workspace, false, grantTUIDiffTrust(t, workspace))
	if cmd == nil {
		t.Fatal("openDiff returned no command")
	}
	msg, ok := cmd().(diffLoadedMsg)
	if !ok {
		t.Fatalf("openDiff returned %T, want diffLoadedMsg", msg)
	}
	return msg
}

func grantTUIDiffTrust(t *testing.T, workspace string) *trust.Store {
	t.Helper()
	store, err := trust.OpenFile(filepath.Join(t.TempDir(), "trust.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Grant(workspace); err != nil {
		t.Fatal(err)
	}
	return store
}

func plainTUIDiff(msg diffLoadedMsg) string {
	return stripANSI(strings.Join(msg.lines, "\n"))
}

func initTUIDiffRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	runTUIDiffGit(t, root, "init", "--quiet")
	runTUIDiffGit(t, root, "symbolic-ref", "HEAD", "refs/heads/main")
	runTUIDiffGit(t, root, "config", "user.name", "Switchboard Test")
	runTUIDiffGit(t, root, "config", "user.email", "switchboard@example.invalid")
	return root
}

func runTUIDiffGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "LC_ALL=C", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func writeTUIDiffFile(t *testing.T, root, name string, content []byte) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitTUIDiffFiles(t *testing.T, root string) {
	t.Helper()
	runTUIDiffGit(t, root, "add", "--all")
	runTUIDiffGit(t, root, "commit", "--quiet", "-m", "fixture")
}

func tuiDiffIndexChecksum(t *testing.T, root string) [sha256.Size]byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}
