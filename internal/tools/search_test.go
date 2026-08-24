package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

func TestGlobBasenamePatternMatchesAnywhere(t *testing.T) {
	r, root := newRegistry(t)
	writeFile(t, filepath.Join(root, "main.go"), "package main\n")
	writeFile(t, filepath.Join(root, "internal", "deep", "x.go"), "package deep\n")
	writeFile(t, filepath.Join(root, "notes.txt"), "notes\n")

	res := run(t, r, "glob", map[string]any{"pattern": "*.go"})
	if res.IsError {
		t.Fatalf("glob failed: %s", res.Content)
	}
	for _, want := range []string{"main.go", "internal/deep/x.go"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("missing %s in %q", want, res.Content)
		}
	}
	if strings.Contains(res.Content, "notes.txt") {
		t.Errorf("notes.txt should not match *.go: %q", res.Content)
	}
}

func TestGlobPathPatternWithDoublestar(t *testing.T) {
	r, root := newRegistry(t)
	writeFile(t, filepath.Join(root, "a_test.go"), "package a\n")
	writeFile(t, filepath.Join(root, "internal", "pkg", "b_test.go"), "package pkg\n")
	writeFile(t, filepath.Join(root, "internal", "pkg", "b.go"), "package pkg\n")

	res := run(t, r, "glob", map[string]any{"pattern": "internal/**/*_test.go"})
	if res.IsError {
		t.Fatalf("glob failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "internal/pkg/b_test.go") {
		t.Errorf("doublestar missed nested test file: %q", res.Content)
	}
	// A path pattern is anchored: the root-level test file is outside internal/.
	if strings.Contains(res.Content, "a_test.go\n") || strings.HasPrefix(res.Content, "a_test.go") {
		t.Errorf("anchored pattern matched outside its prefix: %q", res.Content)
	}
	if strings.Contains(res.Content, "b.go\n") {
		t.Errorf("matched a non-test file: %q", res.Content)
	}
}

func TestGlobSkipsGitAndReportsEmpty(t *testing.T) {
	r, root := newRegistry(t)
	writeFile(t, filepath.Join(root, ".git", "config.go"), "not really go\n")

	for _, input := range []map[string]any{
		{"pattern": "*.go"},
		{"pattern": "*.go", "path": ".git"},
	} {
		res := run(t, r, "glob", input)
		if res.IsError {
			t.Fatalf("glob failed: %s", res.Content)
		}
		if !strings.Contains(res.Content, "no files match") || strings.Contains(res.Content, "config.go") {
			t.Errorf("want the policy-excluded no-match message, got %q", res.Content)
		}
		if strings.Contains(res.Content, "[partial search:") {
			t.Errorf("a declared policy exclusion is not unknown coverage: %q", res.Content)
		}
	}

	grep := run(t, r, "grep", map[string]any{"pattern": "really", "path": ".git"})
	if grep.IsError || !strings.Contains(grep.Content, "no matches") || strings.Contains(grep.Content, "config.go") {
		t.Fatalf("grep did not apply the policy exclusion to its base: %+v", grep)
	}
	if strings.Contains(grep.Content, "[partial search:") {
		t.Fatalf("a declared policy exclusion is not partial coverage: %q", grep.Content)
	}
}

func TestGlobRejectsBadPatternAndOutsidePath(t *testing.T) {
	r, _ := newRegistry(t)

	if _, err := tryRun(r, "glob", map[string]any{"pattern": "[unclosed"}); err == nil {
		t.Error("malformed pattern must fail at Plan time")
	}
	if _, err := tryRun(r, "glob", map[string]any{"pattern": "*.go", "path": "../elsewhere"}); err == nil {
		t.Error("a path outside the workspace must be refused")
	}
}

func TestGlobRejectsOversizedAndOversegmentedPatternsBeforeWalking(t *testing.T) {
	r, _ := newRegistry(t)
	if err := checkGlob(strings.Repeat("a", maxGlobPatternBytes)); err != nil {
		t.Fatalf("exact pattern byte cap rejected: %v", err)
	}
	if _, err := tryRun(r, "glob", map[string]any{"pattern": strings.Repeat("a", maxGlobPatternBytes+1)}); err == nil {
		t.Fatal("oversized glob pattern was accepted")
	}
	tooMany := strings.Repeat("**/", maxGlobSegments) + "x"
	if _, err := tryRun(r, "glob", map[string]any{"pattern": tooMany}); err == nil {
		t.Fatal("oversegmented glob pattern was accepted")
	}
	if err := checkGlob(strings.Repeat("**/", maxGlobSegments-1) + "x"); err != nil {
		t.Fatalf("exact segment cap rejected: %v", err)
	}
}

func TestSearchInputByteCapsAreExactAndErrorsDoNotEchoContent(t *testing.T) {
	for _, tc := range []struct {
		field string
		limit int
	}{
		{field: "pattern", limit: maxGrepPatternBytes},
		{field: "path", limit: maxSearchPathBytes},
		{field: "glob", limit: maxGlobPatternBytes},
		{field: "mode", limit: maxSearchModeBytes},
	} {
		if err := checkSearchInputBytes(tc.field, strings.Repeat("x", tc.limit), tc.limit); err != nil {
			t.Errorf("%s exact cap rejected: %v", tc.field, err)
		}
		secret := "ghp_" + strings.Repeat("A", tc.limit-len("ghp_")+1)
		err := checkSearchInputBytes(tc.field, secret, tc.limit)
		if err == nil || strings.Contains(err.Error(), "ghp_") {
			t.Errorf("%s +1 error is not content-safe: %v", tc.field, err)
		}
	}

	r, _ := newRegistry(t)
	secretPath := "ghp_" + strings.Repeat("P", maxSearchPathBytes-len("ghp_")+1)
	secretPattern := "ghp_" + strings.Repeat("R", maxGrepPatternBytes-len("ghp_")+1)
	secretGlob := "ghp_" + strings.Repeat("G", maxGlobPatternBytes-len("ghp_")+1)
	for name, call := range map[string]struct {
		tool  string
		input map[string]any
	}{
		"glob path":    {tool: "glob", input: map[string]any{"pattern": "*", "path": secretPath}},
		"glob pattern": {tool: "glob", input: map[string]any{"pattern": secretGlob}},
		"grep path":    {tool: "grep", input: map[string]any{"pattern": "x", "path": secretPath}},
		"grep pattern": {tool: "grep", input: map[string]any{"pattern": secretPattern}},
		"grep glob":    {tool: "grep", input: map[string]any{"pattern": "x", "glob": secretGlob}},
	} {
		_, err := tryRun(r, call.tool, call.input)
		if err == nil {
			t.Errorf("%s +1 input was accepted", name)
			continue
		}
		if strings.Contains(err.Error(), "ghp_") {
			t.Errorf("%s error echoed input content: %v", name, err)
		}
	}
}

func TestSearchSyntaxAndBoundaryErrorsDoNotEchoInput(t *testing.T) {
	r, _ := newRegistry(t)
	for name, call := range map[string]struct {
		tool  string
		input map[string]any
	}{
		"glob": {tool: "glob", input: map[string]any{"pattern": "[ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}},
		"grep": {tool: "grep", input: map[string]any{"pattern": "(ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}},
		"path": {tool: "grep", input: map[string]any{"pattern": "x", "path": "../ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}},
	} {
		_, err := tryRun(r, call.tool, call.input)
		if err == nil {
			t.Errorf("%s invalid input was accepted", name)
			continue
		}
		if strings.Contains(err.Error(), "ghp_") {
			t.Errorf("%s error echoed input content: %v", name, err)
		}
	}
}

func TestGlobManyDoublestarsIsBounded(t *testing.T) {
	pattern := strings.Repeat("**/", maxGlobSegments-1) + "never"
	relative := strings.Repeat("segment/", maxSearchWalkDepth) + "target"
	done := make(chan struct {
		matched bool
		err     error
	}, 1)
	go func() {
		matched, err := matchGlob(pattern, relative)
		done <- struct {
			matched bool
			err     error
		}{matched: matched, err: err}
	}()
	select {
	case got := <-done:
		if got.err != nil || got.matched {
			t.Fatalf("match=%t err=%v", got.matched, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("many doublestar segments caused unbounded matching")
	}
}

func TestGlobTruncatesAtTheCap(t *testing.T) {
	r, root := newRegistry(t)
	for i := maxGlobResults + 49; i >= 0; i-- {
		writeFile(t, filepath.Join(root, fmt.Sprintf("f%04d.txt", i)), "x")
	}

	res := run(t, r, "glob", map[string]any{"pattern": "*.txt"})
	if res.IsError {
		t.Fatalf("glob failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, fmt.Sprintf("first %d matches", maxGlobResults)) {
		t.Errorf("an over-cap result must say it truncated: %q", res.Content[len(res.Content)-200:])
	}
	if got := strings.Count(res.Content, "\n") + 1; got > maxGlobResults+1 {
		t.Errorf("returned %d lines, cap is %d plus the notice", got, maxGlobResults)
	}
	if !strings.Contains(res.Content, "f0000.txt") || !strings.Contains(res.Content, "f0499.txt") || strings.Contains(res.Content, "f0500.txt") {
		t.Fatalf("capped glob did not retain the stable lexical prefix: %q", res.Content[len(res.Content)-300:])
	}
}

func TestSearchTraversalLimitIsExactAndPartialCoverageIsExplicit(t *testing.T) {
	for _, tc := range []struct {
		name        string
		files       int
		wantPartial bool
	}{
		{name: "exact", files: 2},
		{name: "plus one", files: 3, wantPartial: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, root := newRegistry(t)
			for n := 0; n < tc.files; n++ {
				writeFile(t, filepath.Join(root, fmt.Sprintf("nonmatch-%d.txt", n)), "hay\n")
			}
			limits := rootedfs.WalkLimits{MaxEntries: 2, MaxDirectories: 4, MaxDepth: 2, ReadDirBatch: 1}

			glob, err := (&globTool{r: r}).globWithLimits(context.Background(), root, "*.go", nil, limits)
			if err != nil || glob.IsError {
				t.Fatalf("glob failed: result=%+v err=%v", glob, err)
			}
			grep, err := (&grepTool{r: r}).grepWithLimits(context.Background(), root, regexp.MustCompile("needle"),
				grepInput{Pattern: "needle"}, nil, nil, limits)
			if err != nil || grep.IsError {
				t.Fatalf("grep failed: result=%+v err=%v", grep, err)
			}
			for name, content := range map[string]string{"glob": glob.Content, "grep": grep.Content} {
				hasPartial := strings.Contains(content, "[partial search:")
				if hasPartial != tc.wantPartial {
					t.Fatalf("%s partial=%t, want %t: %q", name, hasPartial, tc.wantPartial, content)
				}
				if tc.wantPartial && !strings.Contains(content, "2-entry traversal limit") {
					t.Fatalf("%s did not explain the unknown tail: %q", name, content)
				}
			}
		})
	}
}

func TestSearchCompleteResultsAreSortedAcrossReadDirBatches(t *testing.T) {
	r, root := newRegistry(t)
	for _, name := range []string{"z.txt", "a.txt", "m.txt"} {
		writeFile(t, filepath.Join(root, name), "needle\n")
	}
	limits := rootedfs.WalkLimits{MaxEntries: 10, MaxDirectories: 4, MaxDepth: 2, ReadDirBatch: 1}
	glob, err := (&globTool{r: r}).globWithLimits(context.Background(), root, "*.txt", nil, limits)
	if err != nil || glob.IsError {
		t.Fatalf("glob result=%+v err=%v", glob, err)
	}
	grep, err := (&grepTool{r: r}).grepWithLimits(context.Background(), root, regexp.MustCompile("needle"),
		grepInput{Pattern: "needle"}, nil, nil, limits)
	if err != nil || grep.IsError {
		t.Fatalf("grep result=%+v err=%v", grep, err)
	}
	for name, content := range map[string]string{"glob": glob.Content, "grep": grep.Content} {
		a := strings.Index(content, "a.txt")
		m := strings.Index(content, "m.txt")
		z := strings.Index(content, "z.txt")
		if a < 0 || m <= a || z <= m || strings.Contains(content, "[partial search:") {
			t.Fatalf("%s complete output is not sorted: %q", name, content)
		}
	}
}

func TestGlobHonorsCancellationBeforeFanoutTraversal(t *testing.T) {
	r, root := newRegistry(t)
	writeFile(t, filepath.Join(root, "file.txt"), "x")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (&globTool{r: r}).globWithLimits(ctx, root, "*.txt", nil, defaultSearchWalkLimits())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled glob error=%v", err)
	}
}

func TestGrepContentModeReturnsPathLineText(t *testing.T) {
	r, root := newRegistry(t)
	writeFile(t, filepath.Join(root, "a.go"), "package a\n\nfunc Hello() {}\n")
	writeFile(t, filepath.Join(root, "sub", "b.go"), "package b\n\nfunc hello() {}\n")

	res := run(t, r, "grep", map[string]any{"pattern": "func Hello"})
	if res.IsError {
		t.Fatalf("grep failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "a.go:3: func Hello() {}") {
		t.Errorf("want path:line: text, got %q", res.Content)
	}
	if strings.Contains(res.Content, "b.go") {
		t.Errorf("case-sensitive search matched the wrong case: %q", res.Content)
	}
}

func TestGrepRedactsWholeLineBeforeLineCap(t *testing.T) {
	r, root := newRegistry(t)
	line := "MATCH " + strings.Repeat("x", maxGrepLine-len("MATCH ")-len(truncationBoundaryToken)+1) + truncationBoundaryToken
	writeFile(t, filepath.Join(root, "secret.txt"), line+"\n")

	res := run(t, r, "grep", map[string]any{"pattern": "MATCH"})
	if strings.Contains(res.Content, truncationBoundaryToken) || strings.Contains(res.Content, "ghp_") {
		t.Fatalf("line cap exposed a credential fragment: %q", res.Content)
	}
	if !strings.Contains(res.Content, "[redacted: a GitHub token]") {
		t.Fatalf("whole-line redaction was not applied before the cap: %q", res.Content)
	}
}

func TestGrepIgnoreCase(t *testing.T) {
	r, root := newRegistry(t)
	writeFile(t, filepath.Join(root, "a.go"), "// TODO: later\n// todo: sooner\n")

	res := run(t, r, "grep", map[string]any{"pattern": "todo", "ignore_case": true})
	if res.IsError {
		t.Fatalf("grep failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "a.go:1:") || !strings.Contains(res.Content, "a.go:2:") {
		t.Errorf("ignore_case must match both lines: %q", res.Content)
	}
}

func TestGrepFilesModeCountsMatches(t *testing.T) {
	r, root := newRegistry(t)
	writeFile(t, filepath.Join(root, "a.txt"), "x\nx\nx\n")
	writeFile(t, filepath.Join(root, "b.txt"), "x\n")

	res := run(t, r, "grep", map[string]any{"pattern": "x", "mode": "files"})
	if res.IsError {
		t.Fatalf("grep failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "a.txt (3)") || !strings.Contains(res.Content, "b.txt (1)") {
		t.Errorf("files mode must list files with counts: %q", res.Content)
	}
	if strings.Contains(res.Content, ":1:") {
		t.Errorf("files mode must not include line content: %q", res.Content)
	}
}

func TestGrepGlobFilterAndSingleFile(t *testing.T) {
	r, root := newRegistry(t)
	writeFile(t, filepath.Join(root, "a.go"), "match\n")
	writeFile(t, filepath.Join(root, "a.md"), "match\n")

	res := run(t, r, "grep", map[string]any{"pattern": "match", "glob": "*.go"})
	if res.IsError {
		t.Fatalf("grep failed: %s", res.Content)
	}
	if strings.Contains(res.Content, "a.md") {
		t.Errorf("glob filter leaked a non-matching file: %q", res.Content)
	}

	res = run(t, r, "grep", map[string]any{"pattern": "match", "path": "a.md"})
	if res.IsError {
		t.Fatalf("grep on one file failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "a.md:1: match") {
		t.Errorf("single-file search missed: %q", res.Content)
	}
}

func TestGrepSkipsBinaryFiles(t *testing.T) {
	r, root := newRegistry(t)
	writeFile(t, filepath.Join(root, "text.txt"), "needle\n")
	if err := os.WriteFile(filepath.Join(root, "blob.bin"), []byte("needle\x00needle"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := run(t, r, "grep", map[string]any{"pattern": "needle"})
	if res.IsError {
		t.Fatalf("grep failed: %s", res.Content)
	}
	if strings.Contains(res.Content, "blob.bin") {
		t.Errorf("a file with NUL bytes must be skipped: %q", res.Content)
	}
	if !strings.Contains(res.Content, "text.txt:1: needle") {
		t.Errorf("the text file must still match: %q", res.Content)
	}
	if !strings.Contains(res.Content, "[partial search:") {
		t.Errorf("a skipped binary file must make coverage explicit: %q", res.Content)
	}
}

func TestGrepRejectsBadRegexAndBadMode(t *testing.T) {
	r, _ := newRegistry(t)

	if _, err := tryRun(r, "grep", map[string]any{"pattern": "("}); err == nil {
		t.Error("an invalid regex must fail at Plan time")
	}
	if _, err := tryRun(r, "grep", map[string]any{"pattern": "x", "mode": "lines"}); err == nil {
		t.Error("an unknown mode must fail at Plan time")
	}
	if _, err := tryRun(r, "grep", map[string]any{"pattern": "x", "glob": strings.Repeat("**/", maxGlobSegments) + "x"}); err == nil {
		t.Error("an oversegmented grep glob must fail at Plan time")
	}
}

func TestGrepTruncatesAtTheMatchCap(t *testing.T) {
	r, root := newRegistry(t)
	var b strings.Builder
	for i := 0; i < maxGrepMatches+100; i++ {
		b.WriteString("needle\n")
	}
	writeFile(t, filepath.Join(root, "big.txt"), b.String())

	res := run(t, r, "grep", map[string]any{"pattern": "needle"})
	if res.IsError {
		t.Fatalf("grep failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, fmt.Sprintf("first %d matching lines", maxGrepMatches)) {
		t.Errorf("an over-cap result must say it truncated: %q", res.Content[len(res.Content)-200:])
	}
}

func TestGrepReportsNoMatches(t *testing.T) {
	r, root := newRegistry(t)
	writeFile(t, filepath.Join(root, "a.txt"), "hay\n")

	res := run(t, r, "grep", map[string]any{"pattern": "needle"})
	if res.IsError {
		t.Fatalf("grep failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "no matches") {
		t.Errorf("want the no-match message, got %q", res.Content)
	}
}

func TestGrepLabelsOversizedFileAsPartialCoverage(t *testing.T) {
	r, root := newRegistry(t)
	path := filepath.Join(root, "oversized.txt")
	if err := os.WriteFile(path, make([]byte, maxGrepFileSize+1), 0o600); err != nil {
		t.Fatal(err)
	}

	res := run(t, r, "grep", map[string]any{"pattern": "needle"})
	if res.IsError {
		t.Fatalf("grep failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "[partial search:") || !strings.Contains(res.Content, "exceeded") {
		t.Fatalf("oversized omission was silent: %q", res.Content)
	}
}

func TestGrepClassifiesSameFileGrowthAsChangedNotOversized(t *testing.T) {
	r, root := newRegistry(t)
	path := filepath.Join(root, "growing.txt")
	writeFile(t, path, "hay\n")
	var growErr error
	res, err := (&grepTool{r: r}).grepWithHook(context.Background(), root, regexp.MustCompile("needle"),
		grepInput{Pattern: "needle"}, nil, func() {
			growErr = os.WriteFile(path, []byte("needle GROWN_SECRET\n"), 0o600)
		})
	if growErr != nil {
		t.Fatal(growErr)
	}
	if err != nil || res.IsError {
		t.Fatalf("grep result=%+v err=%v", res, err)
	}
	if strings.Contains(res.Content, "GROWN_SECRET") {
		t.Fatalf("bytes from a file that changed after enumeration were returned: %q", res.Content)
	}
	if !strings.Contains(res.Content, "[partial search:") || !strings.Contains(res.Content, "changed or could not be inspected") {
		t.Fatalf("same-file growth was not classified as changed coverage: %q", res.Content)
	}
	if strings.Contains(res.Content, "exceeded") {
		t.Fatalf("ordinary growth was misreported as the %d-byte file limit: %q", maxGrepFileSize, res.Content)
	}
}

func TestGrepDiscardsOrphanedParentContent(t *testing.T) {
	r, root := newRegistry(t)
	base := filepath.Join(root, "nested")
	moved := filepath.Join(root, "nested-moved")
	outside := t.TempDir()
	probe := filepath.Join(t.TempDir(), "symlink-probe")
	if err := os.Symlink(outside, probe); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	const secret = "ORPHAN_SECRET\n"
	writeFile(t, filepath.Join(base, "source.txt"), strings.Repeat("x", len(secret)-1)+"\n")
	writeFile(t, filepath.Join(outside, "source.txt"), secret)

	var swapErr error
	res, err := (&grepTool{r: r}).grepWithHook(context.Background(), base, regexp.MustCompile("ORPHAN_SECRET"),
		grepInput{Pattern: "ORPHAN_SECRET"}, nil, func() {
			swapErr = os.Rename(base, moved)
			if swapErr == nil {
				swapErr = os.WriteFile(filepath.Join(moved, "source.txt"), []byte(secret), 0o644)
			}
			if swapErr == nil {
				swapErr = os.Symlink(outside, base)
			}
		})
	if swapErr != nil {
		t.Fatal(swapErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content, "workspace paths changed") {
		t.Fatalf("orphaned parent was not refused: %+v", res)
	}
	if strings.Contains(res.Content, "ORPHAN_SECRET") {
		t.Fatalf("orphaned content reached the result: %q", res.Content)
	}
}

func TestSearchSkipsSymlinkedFiles(t *testing.T) {
	r, root := newRegistry(t)
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret.txt"), "needle\n")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	res := run(t, r, "grep", map[string]any{"pattern": "needle"})
	if res.IsError {
		t.Fatalf("grep failed: %s", res.Content)
	}
	if strings.Contains(res.Content, "link.txt") {
		t.Errorf("a symlinked file is a door out of the workspace: %q", res.Content)
	}
	if !strings.Contains(res.Content, "[partial search:") {
		t.Errorf("a skipped symlink must make grep coverage explicit: %q", res.Content)
	}

	res = run(t, r, "glob", map[string]any{"pattern": "*.txt"})
	if strings.Contains(res.Content, "link.txt") {
		t.Errorf("glob must not list symlinked files: %q", res.Content)
	}
	if !strings.Contains(res.Content, "[partial search:") {
		t.Errorf("a skipped symlink must make glob coverage explicit: %q", res.Content)
	}
}

func TestGlobMatchWorkLimitIsExactAndExplicit(t *testing.T) {
	compiled, err := compileGlob("*.txt")
	if err != nil {
		t.Fatal(err)
	}
	cost := int64(len("*.txt")+1) * int64(len("a.txt")+1)
	remaining := cost
	matched, limited, err := compiled.match("a.txt", &remaining)
	if err != nil || !matched || limited || remaining != 0 {
		t.Fatalf("exact work budget: matched=%t limited=%t remaining=%d err=%v", matched, limited, remaining, err)
	}
	matched, limited, err = compiled.match("b.txt", &remaining)
	if err != nil || matched || !limited || remaining != 0 {
		t.Fatalf("plus-one work: matched=%t limited=%t remaining=%d err=%v", matched, limited, remaining, err)
	}

	for _, tc := range []struct {
		name        string
		files       int
		wantPartial bool
	}{
		{name: "exact", files: 1},
		{name: "plus one", files: 2, wantPartial: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, root := newRegistry(t)
			for n := 0; n < tc.files; n++ {
				writeFile(t, filepath.Join(root, fmt.Sprintf("%c.txt", 'a'+n)), "x")
			}
			res, runErr := (&globTool{r: r}).globWithWorkLimit(context.Background(), root, "*.txt", nil, defaultSearchWalkLimits(), cost)
			if runErr != nil || res.IsError {
				t.Fatalf("glob result=%+v err=%v", res, runErr)
			}
			hasPartial := strings.Contains(res.Content, "[partial search:")
			if hasPartial != tc.wantPartial {
				t.Fatalf("partial=%t, want %t: %q", hasPartial, tc.wantPartial, res.Content)
			}
			if tc.wantPartial && !strings.Contains(res.Content, "glob matching work limit") {
				t.Fatalf("work-limited result did not explain its coverage: %q", res.Content)
			}
		})
	}
}

func TestGlobWorkChargesPatternCandidateProductBeforeMatching(t *testing.T) {
	pattern := strings.Repeat("*", maxGlobPatternBytes)
	compiled, err := compileGlob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	name := strings.Repeat("x", 255)
	cost := int64(len(pattern)+1) * int64(len(name)+1)
	remaining := cost - 1
	matched, limited, err := compiled.match(name, &remaining)
	if err != nil || matched || !limited || remaining != cost-1 {
		t.Fatalf("product preflight: matched=%t limited=%t remaining=%d err=%v", matched, limited, remaining, err)
	}
	remaining = cost
	matched, limited, err = compiled.match(name, &remaining)
	if err != nil || !matched || limited || remaining != 0 {
		t.Fatalf("exact product budget: matched=%t limited=%t remaining=%d err=%v", matched, limited, remaining, err)
	}
}

func TestGrepAggregateWorkLimitsAreExactAndExplicit(t *testing.T) {
	instructions, err := regexpInstructionCount("needle")
	if err != nil {
		t.Fatal(err)
	}
	const fileBytes = int64(len("hay\n"))
	globCost := int64(len("*.txt")+1) * int64(len("a.txt")+1)

	tests := []struct {
		name   string
		input  grepInput
		limits grepWorkLimits
		reason string
	}{
		{
			name:  "file count",
			input: grepInput{Pattern: "needle"},
			limits: grepWorkLimits{MaxFiles: 2, MaxBytes: 1 << 20,
				MaxRegexWork: 1 << 20, MaxGlobWork: 1 << 20},
			reason: "file content scan limit",
		},
		{
			name:  "aggregate bytes",
			input: grepInput{Pattern: "needle"},
			limits: grepWorkLimits{MaxFiles: 100, MaxBytes: 2 * fileBytes,
				MaxRegexWork: 1 << 20, MaxGlobWork: 1 << 20},
			reason: "aggregate content scan limit",
		},
		{
			name:  "regular expression work",
			input: grepInput{Pattern: "needle"},
			limits: grepWorkLimits{MaxFiles: 100, MaxBytes: 1 << 20,
				MaxRegexWork: int64(instructions) * 2 * fileBytes, MaxGlobWork: 1 << 20},
			reason: "regular-expression work limit",
		},
		{
			name:  "glob work",
			input: grepInput{Pattern: "needle", Glob: "*.txt"},
			limits: grepWorkLimits{MaxFiles: 100, MaxBytes: 1 << 20,
				MaxRegexWork: 1 << 20, MaxGlobWork: 2 * globCost},
			reason: "glob matching work limit",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, count := range []int{2, 3} {
				name := "exact"
				if count == 3 {
					name = "plus one"
				}
				t.Run(name, func(t *testing.T) {
					r, root := newRegistry(t)
					for n := 0; n < count; n++ {
						writeFile(t, filepath.Join(root, fmt.Sprintf("%c.txt", 'a'+n)), "hay\n")
					}
					res, runErr := (&grepTool{r: r}).grepWithWorkLimits(
						context.Background(), root, regexp.MustCompile("needle"), tc.input,
						nil, nil, defaultSearchWalkLimits(), tc.limits,
					)
					if runErr != nil || res.IsError {
						t.Fatalf("grep result=%+v err=%v", res, runErr)
					}
					hasPartial := strings.Contains(res.Content, "[partial search:")
					wantPartial := count == 3
					if hasPartial != wantPartial {
						t.Fatalf("partial=%t, want %t: %q", hasPartial, wantPartial, res.Content)
					}
					if wantPartial && !strings.Contains(res.Content, tc.reason) {
						t.Fatalf("work-limited result did not explain %s: %q", tc.reason, res.Content)
					}
				})
			}
		})
	}
}

func TestGrepRegexWorkScalesWithCompiledProgram(t *testing.T) {
	simple := "needle"
	complex := "(?:needle0|needle1|needle2|needle3|needle4|needle5|needle6|needle7)"
	simpleInstructions, err := regexpInstructionCount(simple)
	if err != nil {
		t.Fatal(err)
	}
	complexInstructions, err := regexpInstructionCount(complex)
	if err != nil {
		t.Fatal(err)
	}
	if complexInstructions <= simpleInstructions {
		t.Fatalf("test pattern did not increase program size: simple=%d complex=%d", simpleInstructions, complexInstructions)
	}

	r, root := newRegistry(t)
	const content = "haystack\n"
	writeFile(t, filepath.Join(root, "one.txt"), content)
	work := int64(simpleInstructions * len(content))
	limits := grepWorkLimits{
		MaxFiles:     10,
		MaxBytes:     1 << 20,
		MaxRegexWork: work,
		MaxGlobWork:  1 << 20,
	}
	for _, tc := range []struct {
		name        string
		pattern     string
		wantPartial bool
	}{
		{name: "simple exact", pattern: simple},
		{name: "larger program", pattern: complex, wantPartial: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, runErr := (&grepTool{r: r}).grepWithWorkLimits(
				context.Background(), root, regexp.MustCompile(tc.pattern), grepInput{Pattern: tc.pattern},
				nil, nil, defaultSearchWalkLimits(), limits,
			)
			if runErr != nil || res.IsError {
				t.Fatalf("grep result=%+v err=%v", res, runErr)
			}
			hasPartial := strings.Contains(res.Content, "regular-expression work limit")
			if hasPartial != tc.wantPartial {
				t.Fatalf("regex partial=%t, want %t: %q", hasPartial, tc.wantPartial, res.Content)
			}
		})
	}
}

func TestGrepCancellationInterruptsFileScan(t *testing.T) {
	r, root := newRegistry(t)
	writeFile(t, filepath.Join(root, "large.txt"), strings.Repeat("haystack\n", 1<<16))
	ctx, cancel := context.WithCancel(context.Background())
	_, err := (&grepTool{r: r}).grepWithHook(ctx, root, regexp.MustCompile("needle"),
		grepInput{Pattern: "needle"}, nil, cancel)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled grep error=%v", err)
	}
}

func TestSearchOutputCapsAndCredentialBoundaries(t *testing.T) {
	t.Run("append redacts before fitting", func(t *testing.T) {
		redacted := safeSearchComponent(truncationBoundaryToken)
		if redacted == truncationBoundaryToken || !strings.Contains(redacted, "[redacted:") {
			t.Fatalf("test token was not recognized: %q", redacted)
		}
		limit := len(redacted) + 8
		var exact strings.Builder
		exact.WriteString(strings.Repeat("x", 7))
		if !appendSearchLine(&exact, truncationBoundaryToken, limit) {
			t.Fatal("exact redacted component did not fit")
		}
		if strings.Contains(exact.String(), "ghp_") || !strings.Contains(exact.String(), redacted) {
			t.Fatalf("exact cap exposed or lost the redaction: %q", exact.String())
		}

		var plusOne strings.Builder
		plusOne.WriteString(strings.Repeat("x", 8))
		before := plusOne.String()
		if appendSearchLine(&plusOne, truncationBoundaryToken, limit) {
			t.Fatal("plus-one component unexpectedly fit")
		}
		if plusOne.String() != before || strings.Contains(plusOne.String(), "ghp_") {
			t.Fatalf("rejected component was partially appended: %q", plusOne.String())
		}
	})

	t.Run("credential filename", func(t *testing.T) {
		r, root := newRegistry(t)
		name := truncationBoundaryToken + ".txt"
		writeFile(t, filepath.Join(root, name), "needle\n")
		for tool, input := range map[string]map[string]any{
			"glob": {"pattern": "*.txt"},
			"grep": {"pattern": "needle", "mode": "files"},
		} {
			res := run(t, r, tool, input)
			if res.IsError {
				t.Fatalf("%s failed: %q", tool, res.Content)
			}
			if strings.Contains(res.Content, truncationBoundaryToken) || strings.Contains(res.Content, "ghp_") {
				t.Fatalf("%s exposed a credential filename: %q", tool, res.Content)
			}
			if !strings.Contains(res.Content, "[redacted:") {
				t.Fatalf("%s did not retain an explicit redaction: %q", tool, res.Content)
			}
		}
	})

	t.Run("glob and files mode", func(t *testing.T) {
		r, root := newRegistry(t)
		longPath := filepath.Join(strings.Repeat("a", 200), strings.Repeat("b", 180))
		for n := 0; n < maxGrepMatches+20; n++ {
			writeFile(t, filepath.Join(root, longPath, fmt.Sprintf("f%03d.txt", n)), "needle\n")
		}
		glob := run(t, r, "glob", map[string]any{"pattern": "*.txt"})
		grep := run(t, r, "grep", map[string]any{"pattern": "needle", "mode": "files"})
		for name, result := range map[string]struct {
			result Result
			limit  int
		}{
			"glob": {result: glob, limit: maxGlobOutput},
			"grep": {result: grep, limit: maxGrepOutput},
		} {
			if result.result.IsError || !strings.Contains(result.result.Content, "[output limit reached") {
				t.Fatalf("%s did not report its output cap: %+v", name, result.result)
			}
			if len(result.result.Content) > result.limit+512 {
				t.Fatalf("%s output escaped its bounded payload: bytes=%d limit=%d", name, len(result.result.Content), result.limit)
			}
		}
	})
}

func TestSearchRuntimeErrorsRedactPathComponents(t *testing.T) {
	r, root := newRegistry(t)
	missing := filepath.Join(root, truncationBoundaryToken)
	glob, globErr := (&globTool{r: r}).glob(context.Background(), missing, "*")
	grep, grepErr := (&grepTool{r: r}).grep(context.Background(), missing, regexp.MustCompile("x"), grepInput{Pattern: "x"})
	for name, got := range map[string]string{
		"glob": glob.Content + fmt.Sprint(globErr),
		"grep": grep.Content + fmt.Sprint(grepErr),
	} {
		if strings.Contains(got, truncationBoundaryToken) || strings.Contains(got, "ghp_") {
			t.Fatalf("%s runtime error exposed a path component: %q", name, got)
		}
		if !strings.Contains(got, "[redacted:") {
			t.Fatalf("%s runtime error lost the redaction: %q", name, got)
		}
	}
}

func TestMatchGlobSemantics(t *testing.T) {
	cases := []struct {
		pattern, rel string
		want         bool
	}{
		{"*.go", "deep/nested/x.go", true},
		{"*.go", "x.txt", false},
		{"**/*.go", "x.go", true},
		{"**/*.go", "a/b/x.go", true},
		{"internal/**/*_test.go", "internal/x_test.go", true},
		{"internal/**/*_test.go", "internal/a/b/x_test.go", true},
		{"internal/**/*_test.go", "cmd/x_test.go", false},
		{"a/*.go", "a/x.go", true},
		{"a/*.go", "a/b/x.go", false},
	}
	for _, c := range cases {
		got, err := matchGlob(c.pattern, c.rel)
		if err != nil {
			t.Errorf("matchGlob(%q, %q): %v", c.pattern, c.rel, err)
			continue
		}
		if got != c.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", c.pattern, c.rel, got, c.want)
		}
	}
}
