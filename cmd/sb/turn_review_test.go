package main

import (
	"bytes"
	"crypto/sha256"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/checkpoint"
)

func commitReviewMutation(t *testing.T, rec *checkpoint.Recorder, path string, before checkpoint.FileState, after checkpoint.FileState) {
	t.Helper()
	if before.Existed {
		if err := os.WriteFile(path, before.Content, before.Mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, before.Mode); err != nil {
			t.Fatal(err)
		}
	} else if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	rec.RecordState(path, before.Existed, before.Mode, before.Content)
	if after.Existed {
		if err := os.WriteFile(path, after.Content, after.Mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, after.Mode); err != nil {
			t.Fatal(err)
		}
		rec.Commit(path, true, after.Mode, sha256.Sum256(after.Content))
	} else {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		rec.Commit(path, false, 0, [sha256.Size]byte{})
	}
}

func TestLoadTurnReviewRepresentsTextFileStates(t *testing.T) {
	tests := []struct {
		name       string
		before     checkpoint.FileState
		after      checkpoint.FileState
		kind       turnReviewKind
		renderWant []string
	}{
		{
			name:   "create",
			before: checkpoint.FileState{},
			after:  checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("new\n")},
			kind:   turnReviewCreated,
			renderWant: []string{
				"new file mode " + turnReviewModeString(testRecordedMode(0o644)), "--- /dev/null", "+++ b/file.txt", "+new",
			},
		},
		{
			name:   "edit",
			before: checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("one\n")},
			after:  checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("two\n")},
			kind:   turnReviewModified,
			renderWant: []string{
				"--- a/file.txt", "+++ b/file.txt", "-one", "+two",
			},
		},
		{
			name:   "delete",
			before: checkpoint.FileState{Existed: true, Mode: 0o600, Content: []byte("gone\n")},
			after:  checkpoint.FileState{},
			kind:   turnReviewDeleted,
			renderWant: []string{
				"deleted file mode " + turnReviewModeString(testRecordedMode(0o600)), "--- a/file.txt", "+++ /dev/null", "-gone",
			},
		},
		{
			name:   "empty create",
			before: checkpoint.FileState{},
			after:  checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte{}},
			kind:   turnReviewCreated,
			renderWant: []string{
				"new file mode " + turnReviewModeString(testRecordedMode(0o644)), "empty file created",
			},
		},
		{
			name:   "truncate",
			before: checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("erase me\n")},
			after:  checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte{}},
			kind:   turnReviewTruncated,
			renderWant: []string{
				"@@ -1 +0,0 @@", "-erase me",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "file.txt")
			rec := checkpoint.NewRecorder()
			rec.Begin(tt.name)
			commitReviewMutation(t, rec, path, tt.before, tt.after)

			review, err := loadTurnReview(rec, 0, dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(review.Files) != 1 {
				t.Fatalf("files=%+v", review.Files)
			}
			file := review.Files[0]
			if file.Kind != tt.kind || file.Stale {
				t.Fatalf("file=%+v", file)
			}
			if file.Before.Existed != tt.before.Existed || file.After.Existed != tt.after.Existed ||
				string(file.Before.Content) != string(tt.before.Content) || string(file.After.Content) != string(tt.after.Content) {
				t.Fatalf("before=%+v after=%+v", file.Before, file.After)
			}
			rendered := review.Render(200, 32<<10)
			for _, want := range tt.renderWant {
				if !strings.Contains(rendered, want) {
					t.Errorf("render missing %q:\n%s", want, rendered)
				}
			}
		})
	}
}

func testRecordedMode(mode fs.FileMode) fs.FileMode {
	if runtime.GOOS == "windows" {
		if mode == 0 {
			return 0
		}
		if mode.Perm()&0o222 == 0 {
			return 0o444
		}
		return 0o666
	}
	return mode & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
}

func TestLoadTurnReviewRepresentsModeOnlyChange(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not preserve Unix executable mode bits")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "script")
	rec := checkpoint.NewRecorder()
	rec.Begin("make executable")
	commitReviewMutation(t, rec, path,
		checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("#!/bin/sh\n")},
		checkpoint.FileState{Existed: true, Mode: 0o755, Content: []byte("#!/bin/sh\n")})

	review, err := loadTurnReview(rec, 0, dir)
	if err != nil {
		t.Fatal(err)
	}
	file := review.Files[0]
	if file.Kind != turnReviewModeOnly || !file.Mode.Changed || file.Mode.Before.Perm() != 0o644 || file.Mode.After.Perm() != 0o755 {
		t.Fatalf("mode review=%+v", file)
	}
	rendered := review.Render(100, 8<<10)
	for _, want := range []string{"old mode 100644", "new mode 100755"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("render missing %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "@@") {
		t.Fatalf("mode-only change rendered a content hunk:\n%s", rendered)
	}
}

func TestLoadTurnReviewUsesExplicitBinaryMarker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.bin")
	rec := checkpoint.NewRecorder()
	rec.Begin("binary edit")
	commitReviewMutation(t, rec, path,
		checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte{0, 1, 2}},
		checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte{0, 1, 3}})

	review, err := loadTurnReview(rec, 0, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !review.Files[0].Binary || review.Files[0].Kind != turnReviewModified {
		t.Fatalf("binary file=%+v", review.Files[0])
	}
	rendered := review.Render(100, 8<<10)
	if !strings.Contains(rendered, "Binary files a/data.bin and b/data.bin differ") {
		t.Fatalf("binary marker missing:\n%s", rendered)
	}
	if strings.ContainsRune(rendered, 0) || strings.Contains(rendered, "@@") {
		t.Fatalf("binary bytes were rendered as text:\n%q", rendered)
	}
}

func TestTurnReviewDoesNotClaimBinaryContentDiffForModeOnlyChange(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not preserve Unix executable mode bits")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "data.bin")
	content := []byte{0, 1, 2}
	rec := checkpoint.NewRecorder()
	rec.Begin("binary mode")
	commitReviewMutation(t, rec, path,
		checkpoint.FileState{Existed: true, Mode: 0o644, Content: content},
		checkpoint.FileState{Existed: true, Mode: 0o755, Content: content})

	review, err := loadTurnReview(rec, 0, dir)
	if err != nil {
		t.Fatal(err)
	}
	if review.Files[0].Kind != turnReviewModeOnly || !review.Files[0].Binary {
		t.Fatalf("binary mode file=%+v", review.Files[0])
	}
	rendered := review.Render(100, 8<<10)
	if !strings.Contains(rendered, "old mode 100644") || !strings.Contains(rendered, "new mode 100755") {
		t.Fatalf("mode markers missing:\n%s", rendered)
	}
	if strings.Contains(rendered, "Binary files") {
		t.Fatalf("mode-only binary change claimed content differed:\n%s", rendered)
	}
}

func TestLoadTurnReviewNamesSkippedPreimageWithoutReadingIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.txt")
	before := []byte(strings.Repeat("x", (4<<20)+1))
	if err := os.WriteFile(path, before, 0o644); err != nil {
		t.Fatal(err)
	}
	rec := checkpoint.NewRecorder()
	rec.Begin("oversize")
	rec.RecordState(path, true, 0o644, before)
	if err := os.WriteFile(path, []byte("small current secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec.Commit(path, true, 0o644, sha256.Sum256([]byte("small current secret")))

	review, err := loadTurnReview(rec, 0, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !review.Partial || len(review.Files) != 1 || !review.Files[0].Skipped {
		t.Fatalf("partial review=%+v", review)
	}
	rendered := review.Render(100, 8<<10)
	if !strings.Contains(rendered, "exact review is unavailable") || strings.Contains(rendered, "small current secret") {
		t.Fatalf("partial render disclosed bytes or omitted marker:\n%s", rendered)
	}
}

func TestLoadTurnReviewRefusesStaleCurrentFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	rec := checkpoint.NewRecorder()
	rec.Begin("edit")
	commitReviewMutation(t, rec, path,
		checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("before\n")},
		checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("after\n")})
	if err := os.WriteFile(path, []byte("external secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	review, err := loadTurnReview(rec, 0, dir)
	if err != nil {
		t.Fatal(err)
	}
	file := review.Files[0]
	if !file.Stale || file.Kind != turnReviewStale || file.After.Content != nil {
		t.Fatalf("stale file=%+v", file)
	}
	rendered := review.Render(100, 8<<10)
	if !strings.Contains(rendered, checkpoint.ErrStale.Error()) || strings.Contains(rendered, "external secret") {
		t.Fatalf("stale render disclosed or omitted evidence:\n%s", rendered)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "external secret\n" {
		t.Fatalf("stale review changed file: %q err=%v", got, err)
	}
}

func TestLoadTurnReviewRefusesOutsideAndSymlinkParents(t *testing.T) {
	t.Run("outside workspace", func(t *testing.T) {
		workspace := t.TempDir()
		outside := t.TempDir()
		path := filepath.Join(outside, "secret.txt")
		rec := checkpoint.NewRecorder()
		rec.Begin("outside")
		commitReviewMutation(t, rec, path,
			checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("before")},
			checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("outside secret")})

		review, err := loadTurnReview(rec, 0, workspace)
		if err != nil {
			t.Fatal(err)
		}
		if !review.Files[0].Stale || !strings.Contains(review.Files[0].Error, "outside workspace") {
			t.Fatalf("outside file=%+v", review.Files[0])
		}
		if strings.Contains(review.Render(100, 8<<10), "outside secret") {
			t.Fatal("outside bytes reached the review")
		}
	})

	t.Run("replaced parent symlink", func(t *testing.T) {
		workspace := t.TempDir()
		dir := filepath.Join(workspace, "dir")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "file.txt")
		rec := checkpoint.NewRecorder()
		rec.Begin("edit")
		commitReviewMutation(t, rec, path,
			checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("before")},
			checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("after")})

		moved := filepath.Join(workspace, "moved")
		if err := os.Rename(dir, moved); err != nil {
			t.Fatal(err)
		}
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "file.txt"), []byte("outside secret"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, dir); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}

		review, err := loadTurnReview(rec, 0, workspace)
		if err != nil {
			t.Fatal(err)
		}
		if !review.Files[0].Stale || !strings.Contains(review.Files[0].Error, "symlink") {
			t.Fatalf("symlink-parent file=%+v", review.Files[0])
		}
		if strings.Contains(review.Render(100, 8<<10), "outside secret") {
			t.Fatal("symlink target bytes reached the review")
		}
	})
}

func TestTurnReviewRenderIsSortedDeterministicAndBounded(t *testing.T) {
	dir := t.TempDir()
	rec := checkpoint.NewRecorder()
	rec.Begin("large deterministic review")
	for _, name := range []string{"z.txt", "a.txt"} {
		path := filepath.Join(dir, name)
		before := strings.Repeat("before line\n", 80)
		after := strings.Repeat("after line\n", 80)
		commitReviewMutation(t, rec, path,
			checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte(before)},
			checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte(after)})
	}

	review, err := loadTurnReview(rec, 0, dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{review.Files[0].DisplayPath, review.Files[1].DisplayPath}; got[0] != "a.txt" || got[1] != "z.txt" {
		t.Fatalf("file order=%v", got)
	}
	first := review.Render(10, 180)
	second := review.Render(10, 180)
	if first != second {
		t.Fatalf("render is nondeterministic:\nfirst=%q\nsecond=%q", first, second)
	}
	if len(first) > 180 || strings.Count(first, "\n") > 10 {
		t.Fatalf("render crossed bounds: bytes=%d lines=%d\n%s", len(first), strings.Count(first, "\n"), first)
	}
	if !strings.Contains(first, "turn review truncated") {
		t.Fatalf("bounded render omitted truncation marker:\n%s", first)
	}
	if strings.Contains(first, "@@") || strings.Contains(first, "--- ") || strings.Contains(first, "+++ ") {
		t.Fatalf("bounded render emitted a partial unified hunk:\n%s", first)
	}
}

func TestTurnReviewRenderBoundsNewlineDenseImages(t *testing.T) {
	before := bytes.Repeat([]byte{'\n'}, 4<<20)
	after := append([]byte("changed\n"), before...)
	review := turnReview{
		Index: 1,
		Label: "dense",
		Files: []turnReviewFile{{
			DisplayPath: "dense.txt",
			Kind:        turnReviewModified,
			Before:      checkpoint.FileState{Existed: true, Mode: 0o644, Content: before},
			After:       checkpoint.FileState{Existed: true, Mode: 0o644, Content: after},
		}},
	}
	first := review.Render(20, 2048)
	second := review.Render(20, 2048)
	if first != second {
		t.Fatal("newline-dense render is nondeterministic")
	}
	if len(first) > 2048 || strings.Count(first, "\n") > 20 || !strings.Contains(first, "turn review truncated") {
		t.Fatalf("newline-dense render crossed or hid bounds: bytes=%d lines=%d\n%s", len(first), strings.Count(first, "\n"), first)
	}
	if strings.Contains(first, "@@") {
		t.Fatalf("newline-dense render emitted a partial hunk:\n%s", first)
	}
}

func TestTurnReviewRenderCountsEmbeddedNewlinesAgainstBound(t *testing.T) {
	review := turnReview{
		Index: 1,
		Label: "unsafe error",
		Files: []turnReviewFile{{
			DisplayPath: "file.txt",
			Kind:        turnReviewStale,
			Stale:       true,
			Error:       strings.Repeat("redirected\n", 20),
		}},
	}
	rendered := review.Render(5, 512)
	if got := strings.Count(rendered, "\n"); got > 5 {
		t.Fatalf("embedded newlines crossed line bound: %d\n%s", got, rendered)
	}
	if len(rendered) > 512 {
		t.Fatalf("embedded newlines crossed byte bound: %d", len(rendered))
	}
}

func TestTurnReviewRefusalCannotInjectReviewLines(t *testing.T) {
	review := turnReview{
		Index: 1,
		Label: "stale",
		Files: []turnReviewFile{{
			DisplayPath: "file.txt",
			Kind:        turnReviewStale,
			Stale:       true,
			Error:       "changed\n+++ forged\n@@ -1 +1 @@",
		}},
	}
	rendered := review.Render(20, 2048)
	if strings.Contains(rendered, "\n+++ forged") || strings.Contains(rendered, "\n@@ -1 +1 @@") {
		t.Fatalf("refusal metadata forged unified-diff lines:\n%s", rendered)
	}
	if !strings.Contains(rendered, `changed\x0a+++ forged\x0a@@ -1 +1 @@`) {
		t.Fatalf("refusal metadata was not visibly escaped:\n%s", rendered)
	}
}

func TestTurnReviewTruncationEvictsOnlyWholeFileSections(t *testing.T) {
	review := turnReview{
		Index: 1,
		Label: "two files",
		Files: []turnReviewFile{
			{
				DisplayPath: "a.txt",
				Kind:        turnReviewModified,
				Before:      checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("before\n")},
				After:       checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("after\n")},
			},
			{
				DisplayPath: "b.txt",
				Kind:        turnReviewModified,
				Before:      checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("before\n")},
				After:       checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("after\n")},
			},
		},
	}
	// Three review headers plus the first file's seven-line section exactly
	// fill this bound. The truncation marker must evict that complete section,
	// never pop just its final diff body line.
	rendered := review.Render(10, 16<<10)
	if !strings.Contains(rendered, "turn review truncated") {
		t.Fatalf("missing truncation marker:\n%s", rendered)
	}
	for _, partial := range []string{"file a.txt", "--- a/a.txt", "@@ ", "-before", "+after"} {
		if strings.Contains(rendered, partial) {
			t.Fatalf("truncation retained partial file section %q:\n%s", partial, rendered)
		}
	}
}

func TestTurnReviewRenderEscapesTerminalControls(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	rec := checkpoint.NewRecorder()
	rec.Begin("label\x1b[2J\r\u202e")
	commitReviewMutation(t, rec, path,
		checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("before\x1b]0;title\a\r\u202e\n")},
		checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("after\x1b[2J\a\r\u202e\n")})
	review, err := loadTurnReview(rec, 0, dir)
	if err != nil {
		t.Fatal(err)
	}
	rendered := review.Render(100, 16<<10)
	for _, unsafe := range []string{"\x1b", "\a", "\r", "\u202e"} {
		if strings.Contains(rendered, unsafe) {
			t.Fatalf("render retained terminal control %q: %q", unsafe, rendered)
		}
	}
	for _, visible := range []string{`\x1b`, `\x07`, `\x0d`, `\u202e`} {
		if !strings.Contains(rendered, visible) {
			t.Errorf("render omitted visible escape %q: %q", visible, rendered)
		}
	}
}

func TestTurnReviewRenderEscapesTabsBeforeLayout(t *testing.T) {
	review := turnReview{
		Index: 1,
		Label: "tabs",
		Files: []turnReviewFile{{
			DisplayPath: "tabs.txt",
			Kind:        turnReviewModified,
			Before:      checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("a\tb\n")},
			After:       checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("c\td\n")},
		}},
	}
	rendered := review.Render(100, 8<<10)
	if strings.ContainsRune(rendered, '\t') || !strings.Contains(rendered, `-a\tb`) || !strings.Contains(rendered, `+c\td`) {
		t.Fatalf("tab review was not visibly escaped: %q", rendered)
	}
}

func TestLoadTurnReviewBoundsAggregatePreimagesAndPostimages(t *testing.T) {
	t.Run("preimages", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "large-before.txt")
		before := []byte(strings.Repeat("b", turnReviewLoadMaxBytes+1))
		rec := checkpoint.NewRecorder()
		rec.Begin("large preimage")
		commitReviewMutation(t, rec, path,
			checkpoint.FileState{Existed: true, Mode: 0o644, Content: before},
			checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("small after")})

		review, err := loadTurnReview(rec, 0, dir)
		if err != nil {
			t.Fatal(err)
		}
		if review.Omitted != 1 || !review.Partial || len(review.Files) != 0 {
			t.Fatalf("bounded preimage review=%+v", review)
		}
		if rendered := review.Render(100, 8<<10); !strings.Contains(rendered, "1 additional recorded path(s) omitted") {
			t.Fatalf("bounded preimage marker missing:\n%s", rendered)
		}
	})

	t.Run("postimages", func(t *testing.T) {
		dir := t.TempDir()
		rec := checkpoint.NewRecorder()
		rec.Begin("aggregate after")
		content := []byte(strings.Repeat("x", 150<<10))
		for _, name := range []string{"a.txt", "b.txt"} {
			commitReviewMutation(t, rec, filepath.Join(dir, name), checkpoint.FileState{},
				checkpoint.FileState{Existed: true, Mode: 0o644, Content: content})
		}

		review, err := loadTurnReview(rec, 0, dir)
		if err != nil {
			t.Fatal(err)
		}
		loaded := 0
		large := 0
		for _, file := range review.Files {
			loaded += len(file.Before.Content) + len(file.After.Content)
			if file.Large {
				large++
			}
		}
		if loaded > turnReviewLoadMaxBytes || large != 1 {
			t.Fatalf("postimage bytes=%d large=%d files=%+v", loaded, large, review.Files)
		}
	})
}

func TestTurnReviewQuotesWholeDiffLabels(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows filename grammar differs")
	}
	dir := t.TempDir()
	name := "odd name\n\"slash\\.txt"
	path := filepath.Join(dir, name)
	rec := checkpoint.NewRecorder()
	rec.Begin("odd path")
	commitReviewMutation(t, rec, path,
		checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("before\n")},
		checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("after\n")})
	review, err := loadTurnReview(rec, 0, dir)
	if err != nil {
		t.Fatal(err)
	}
	rendered := review.Render(100, 16<<10)
	for _, want := range []string{
		`--- "a/odd name\n\"slash\\.txt"`,
		`+++ "b/odd name\n\"slash\\.txt"`,
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("quoted full label missing %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, `a/"`) || strings.Contains(rendered, `b/"`) {
		t.Fatalf("render prefixed an already-quoted path fragment:\n%s", rendered)
	}
}

func TestTurnReviewUsesGitByteQuotingForControlAndBidiPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows filename grammar differs")
	}
	dir := t.TempDir()
	name := "control\x1b\u202e.txt"
	path := filepath.Join(dir, name)
	rec := checkpoint.NewRecorder()
	rec.Begin("odd path")
	commitReviewMutation(t, rec, path,
		checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("before\n")},
		checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("after\n")})
	review, err := loadTurnReview(rec, 0, dir)
	if err != nil {
		t.Fatal(err)
	}
	rendered := review.Render(100, 16<<10)
	for _, want := range []string{
		`--- "a/control\033\342\200\256.txt"`,
		`+++ "b/control\033\342\200\256.txt"`,
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("Git-style byte-quoted label missing %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "\x1b") || strings.Contains(rendered, "\u202e") {
		t.Fatalf("raw terminal control remained in quoted path: %q", rendered)
	}
}

func TestLoadTurnReviewRejectsInvalidTurnAndWorkspace(t *testing.T) {
	if _, err := loadTurnReview(nil, 0, t.TempDir()); err == nil {
		t.Fatal("nil recorder accepted")
	}
	rec := checkpoint.NewRecorder()
	if _, err := loadTurnReview(rec, 0, t.TempDir()); err == nil {
		t.Fatal("empty recorder accepted")
	}
	rec.Begin("create")
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	commitReviewMutation(t, rec, path, checkpoint.FileState{}, checkpoint.FileState{Existed: true, Mode: fs.FileMode(0o644)})
	if _, err := loadTurnReview(rec, 2, dir); err == nil {
		t.Fatal("out-of-range turn accepted")
	}
	if _, err := loadTurnReview(rec, 1, filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing workspace accepted")
	}
}

func TestLoadTurnReviewLatestDoesNotReuseOlderTurnAfterNoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	rec := checkpoint.NewRecorder()
	rec.Begin("mutating turn")
	commitReviewMutation(t, rec, path, checkpoint.FileState{}, checkpoint.FileState{Existed: true, Mode: 0o644, Content: []byte("new")})
	rec.Begin("no-op current turn")

	if _, err := loadTurnReview(rec, 0, dir); err == nil || !strings.Contains(err.Error(), "current turn has no recorded write/edit mutations") {
		t.Fatalf("implicit review reused an older turn: %v", err)
	}
	review, err := loadTurnReview(rec, 1, dir)
	if err != nil {
		t.Fatalf("explicit older turn: %v", err)
	}
	if review.Label != "mutating turn" || review.Index != 1 || len(review.Files) != 1 {
		t.Fatalf("explicit older review=%+v", review)
	}
}
