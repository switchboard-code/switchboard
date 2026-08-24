package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/checkpoint"
	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/permission"
)

func TestMain(m *testing.M) {
	switch os.Getenv("SB_TOOLS_EXEC_HELPER") {
	case "safe":
		_, _ = os.Stdout.WriteString("safe")
		os.Exit(0)
	case "list":
		_, _ = os.Stdout.WriteString("marker.txt\n")
		os.Exit(0)
	case "fail":
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func newRegistry(t *testing.T) (*Registry, string) {
	t.Helper()
	root := t.TempDir()
	r, err := NewRegistry(root, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	recorder := newCheckpointRecorder(t, r.Root())
	recorder.Begin("test mutation")
	r.SetCheckpoints(recorder)
	return r, r.Root()
}

func newCheckpointRecorder(t *testing.T, workspace string) *checkpoint.Recorder {
	t.Helper()
	recorder := checkpoint.NewRecorder()
	if err := recorder.ConfigureRestoreCleanup(t.TempDir(), workspace); err != nil {
		t.Fatal(err)
	}
	return recorder
}

func TestRegistryDisplayResolvesWorkspaceAliasWithoutHidingEscapes(t *testing.T) {
	workspace := t.TempDir()
	existing := filepath.Join(workspace, "src", "existing.go")
	writeFile(t, existing, "package src\n")
	alias := filepath.Join(t.TempDir(), "workspace-alias")
	if err := os.Symlink(workspace, alias); err != nil {
		t.Skipf("workspace aliases are unavailable: %v", err)
	}

	registry, err := NewRegistry(alias, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"logical existing suffix":   filepath.Join(alias, "src", "existing.go"),
		"logical new suffix":        filepath.Join(alias, "src", "new.go"),
		"canonical existing suffix": filepath.Join(registry.root, "src", "existing.go"),
	} {
		t.Run(name, func(t *testing.T) {
			if got := registry.display(path); got != "src/"+filepath.Base(path) {
				t.Fatalf("display(%q) = %q", path, got)
			}
		})
	}
	if got := registry.Branch(nil).display(filepath.Join(alias, "src", "branch-new.go")); got != "src/branch-new.go" {
		t.Fatalf("branch lost launch-root display identity: %q", got)
	}

	outside := filepath.Join(t.TempDir(), "outside.go")
	want, err := filepath.Rel(registry.root, outside)
	if err != nil {
		t.Fatal(err)
	}
	if got := registry.display(outside); got != filepath.ToSlash(want) {
		t.Fatalf("external display(%q) = %q, want unchanged relative escape %q", outside, got, filepath.ToSlash(want))
	}
}

func run(t *testing.T, r *Registry, tool string, input any) Result {
	t.Helper()
	res, err := tryRun(r, tool, input)
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return res
}

func tryRun(r *Registry, tool string, input any) (Result, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return Result{}, err
	}
	tl, ok := r.Get(tool)
	if !ok {
		return Result{}, os.ErrNotExist
	}
	plan, err := tl.Plan(raw)
	if err != nil {
		return Result{}, err
	}
	return plan.Run(context.Background())
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadReturnsExactBytes(t *testing.T) {
	r, root := newRegistry(t)
	const content = "package main\n\nfunc main() {\n\tprintln(\"hi\")\n}\n"
	writeFile(t, filepath.Join(root, "main.go"), content)

	res := run(t, r, "read", map[string]any{"path": "main.go"})
	if res.IsError {
		t.Fatalf("read failed: %s", res.Content)
	}
	// Exact bytes, with no line numbers: anything added here would end up
	// pasted into an edit's old_string and fail to match.
	if res.Content != content {
		t.Errorf("read returned %q, want the file verbatim", res.Content)
	}
}

func TestReadOffsetAndLimit(t *testing.T) {
	r, root := newRegistry(t)
	writeFile(t, filepath.Join(root, "lines.txt"), "one\ntwo\nthree\nfour\nfive\n")

	res := run(t, r, "read", map[string]any{"path": "lines.txt", "offset": 2, "limit": 2})
	if res.IsError {
		t.Fatalf("read failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "two\nthree") {
		t.Errorf("content = %q, want lines two and three", res.Content)
	}
	if strings.Contains(res.Content, "four") {
		t.Errorf("limit was not respected: %q", res.Content)
	}
	if !strings.Contains(res.Content, "[lines 2-3 of 6]") {
		t.Errorf("a partial read must say which slice it returned: %q", res.Content)
	}
}

func TestReadRejectsDirectoriesAndMissingFiles(t *testing.T) {
	r, root := newRegistry(t)
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	if res := run(t, r, "read", map[string]any{"path": "sub"}); !res.IsError {
		t.Error("reading a directory should be a tool error")
	}
	if res := run(t, r, "read", map[string]any{"path": "nope.go"}); !res.IsError {
		t.Error("reading a missing file should be a tool error")
	}
}

func TestWorkspaceBoundary(t *testing.T) {
	r, root := newRegistry(t)
	outside := filepath.Join(filepath.Dir(root), "outside.txt")
	writeFile(t, outside, "secret")

	for _, path := range []string{"../outside.txt", outside, "sub/../../outside.txt"} {
		if _, err := tryRun(r, "read", map[string]any{"path": path}); err == nil {
			t.Errorf("path %q escaped the workspace", path)
		}
	}
}

// A symlink inside the workspace pointing out of it is the case a naive prefix
// check misses: the literal path looks contained and the file it opens is not.
func TestSymlinkEscapeIsRejected(t *testing.T) {
	r, root := newRegistry(t)

	outsideDir := t.TempDir()
	secret := filepath.Join(outsideDir, "credentials")
	writeFile(t, secret, "sk-live-key")

	if err := os.Symlink(secret, filepath.Join(root, "innocent.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(root, "elsewhere")); err != nil {
		t.Fatal(err)
	}

	if _, err := tryRun(r, "read", map[string]any{"path": "innocent.txt"}); err == nil {
		t.Error("a symlink to a file outside the workspace was followed")
	}
	// Also covers writing through a symlinked directory to a path that does not
	// exist yet, where only the ancestor can be resolved.
	if _, err := tryRun(r, "write", map[string]any{"path": "elsewhere/planted.txt", "content": "x"}); err == nil {
		t.Error("a write through a symlinked directory escaped the workspace")
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "planted.txt")); err == nil {
		t.Error("the escaping write actually created a file outside the workspace")
	}
}

func TestWriteRequiresAPriorReadOfAnExistingFile(t *testing.T) {
	r, root := newRegistry(t)
	path := filepath.Join(root, "existing.go")
	writeFile(t, path, "original\n")

	res := run(t, r, "write", map[string]any{"path": "existing.go", "content": "replaced\n"})
	if !res.IsError {
		t.Fatal("overwriting an unread file must fail")
	}
	if got, _ := os.ReadFile(path); string(got) != "original\n" {
		t.Fatal("the file was overwritten anyway")
	}

	run(t, r, "read", map[string]any{"path": "existing.go"})
	if res := run(t, r, "write", map[string]any{"path": "existing.go", "content": "replaced\n"}); res.IsError {
		t.Fatalf("write after read failed: %s", res.Content)
	}
	if got, _ := os.ReadFile(path); string(got) != "replaced\n" {
		t.Errorf("file = %q, want replaced", got)
	}
}

func TestWriteCreatesNewFilesWithoutAPriorRead(t *testing.T) {
	r, root := newRegistry(t)

	res := run(t, r, "write", map[string]any{"path": "pkg/new.go", "content": "package pkg\n"})
	if res.IsError {
		t.Fatalf("creating a new file failed: %s", res.Content)
	}
	got, err := os.ReadFile(filepath.Join(root, "pkg", "new.go"))
	if err != nil || string(got) != "package pkg\n" {
		t.Errorf("file = %q, err = %v", got, err)
	}
}

// The staleness check exists for exactly this: something else touched the file
// between the agent's read and its write.
func TestConcurrentModificationBlocksTheWrite(t *testing.T) {
	r, root := newRegistry(t)
	path := filepath.Join(root, "raced.go")
	writeFile(t, path, "version one\n")

	run(t, r, "read", map[string]any{"path": "raced.go"})
	writeFile(t, path, "someone else edited this\n")

	res := run(t, r, "write", map[string]any{"path": "raced.go", "content": "agent's version\n"})
	if !res.IsError {
		t.Fatal("a write over a changed file must fail")
	}
	if !strings.Contains(res.Content, "changed since it was read") {
		t.Errorf("the message must say why: %q", res.Content)
	}

	// Retrying without a fresh read must fail again, or the check is a speed
	// bump rather than a guarantee.
	if res := run(t, r, "write", map[string]any{"path": "raced.go", "content": "agent's version\n"}); !res.IsError {
		t.Error("the retry succeeded without re-reading")
	}
	if got, _ := os.ReadFile(path); string(got) != "someone else edited this\n" {
		t.Errorf("the other change was clobbered: %q", got)
	}
}

func TestEdit(t *testing.T) {
	r, root := newRegistry(t)
	path := filepath.Join(root, "app.go")
	writeFile(t, path, "a := 1\nb := 2\na := 1\n")
	run(t, r, "read", map[string]any{"path": "app.go"})

	t.Run("ambiguous match is refused", func(t *testing.T) {
		res := run(t, r, "edit", map[string]any{"path": "app.go", "old_string": "a := 1", "new_string": "a := 9"})
		if !res.IsError {
			t.Fatal("two matches without replace_all must fail")
		}
		if !strings.Contains(res.Content, "appears 2 times") {
			t.Errorf("message = %q", res.Content)
		}
	})

	t.Run("unique match with context", func(t *testing.T) {
		res := run(t, r, "edit", map[string]any{
			"path": "app.go", "old_string": "b := 2", "new_string": "b := 22",
		})
		if res.IsError {
			t.Fatalf("edit failed: %s", res.Content)
		}
		got, _ := os.ReadFile(path)
		if string(got) != "a := 1\nb := 22\na := 1\n" {
			t.Errorf("file = %q", got)
		}
	})

	t.Run("replace all", func(t *testing.T) {
		res := run(t, r, "edit", map[string]any{
			"path": "app.go", "old_string": "a := 1", "new_string": "a := 3", "replace_all": true,
		})
		if res.IsError {
			t.Fatalf("edit failed: %s", res.Content)
		}
		got, _ := os.ReadFile(path)
		if strings.Contains(string(got), "a := 1") {
			t.Errorf("replace_all left occurrences behind: %q", got)
		}
	})

	t.Run("no match explains why", func(t *testing.T) {
		res := run(t, r, "edit", map[string]any{
			"path": "app.go", "old_string": "not in the file", "new_string": "x",
		})
		if !res.IsError {
			t.Fatal("a missing old_string must fail")
		}
		if !strings.Contains(res.Content, "byte for byte") {
			t.Errorf("the message should say what exact matching means: %q", res.Content)
		}
	})
}

func TestEditPreservesFileMode(t *testing.T) {
	r, root := newRegistry(t)
	path := filepath.Join(root, "script.sh")
	writeFile(t, path, "#!/bin/sh\necho old\n")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, r, "read", map[string]any{"path": "script.sh"})

	if res := run(t, r, "edit", map[string]any{
		"path": "script.sh", "old_string": "echo old", "new_string": "echo new",
	}); res.IsError {
		t.Fatalf("edit failed: %s", res.Content)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := restorableFileMode(0o755); fi.Mode().Perm() != want.Perm() {
		t.Errorf("mode = %o, want %o; an edit must preserve every mode bit this platform can restore", fi.Mode().Perm(), want.Perm())
	}
}

func TestEditRejectsDegenerateInput(t *testing.T) {
	r, _ := newRegistry(t)
	for name, input := range map[string]map[string]any{
		"empty old_string": {"path": "x.go", "old_string": "", "new_string": "a"},
		"identical":        {"path": "x.go", "old_string": "a", "new_string": "a"},
	} {
		if _, err := tryRun(r, "edit", input); err == nil {
			t.Errorf("%s should be rejected at plan time", name)
		}
	}
}

func TestExecPlanDescribesThePermissionRequest(t *testing.T) {
	r, _ := newRegistry(t)
	tool, _ := r.Get("exec")

	plan, err := tool.Plan(json.RawMessage(`{"command":["go","test","./..."]}`))
	if err != nil {
		t.Fatal(err)
	}
	req := plan.Request
	if req.Effect != permission.EffectExecute {
		t.Errorf("effect = %s", req.Effect)
	}
	if req.Shell {
		t.Error("argv mode must not be reported as shell mode")
	}
	if strings.Join(req.Argv, " ") != "go test ./..." {
		t.Errorf("argv = %v", req.Argv)
	}
	if req.Path != "." {
		t.Errorf("command cwd = %q, want workspace-relative dot", req.Path)
	}
	if req.Execution == nil || !req.Execution.FullAccess && req.Execution.Network != execution.NetworkFull {
		t.Errorf("permission request omitted effective execution policy: %+v", req.Execution)
	}
}

func TestExecPermissionArgvCannotRewriteRun(t *testing.T) {
	t.Setenv("SB_TOOLS_EXEC_HELPER", "safe")
	r, _ := newRegistry(t)
	tool, _ := r.Get("exec")
	raw, err := json.Marshal(map[string]any{"command": []string{os.Args[0]}})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := tool.Plan(raw)
	if err != nil {
		t.Fatal(err)
	}
	plan.Request.Argv[0] = "false"
	result, err := plan.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || result.Content != "safe" {
		t.Fatalf("request argv mutation rewrote executable closure: %+v", result)
	}
}

func TestExecDescriptionEscapesTerminalControlSequences(t *testing.T) {
	got := Describe([]string{"printf", "\x1b[2J\x1b]0;APPROVED\x07"}, false)
	if strings.ContainsAny(got, "\x1b\x07") || !strings.Contains(got, `\x1b[2J`) || !strings.Contains(got, `\x07`) {
		t.Fatalf("unsafe command description %q", got)
	}
}

func TestExecDescriptionPreservesArgvBoundariesAfterEscaping(t *testing.T) {
	got := Describe([]string{"printf", "safe\x1b[0m --danger", ""}, false)
	if strings.Contains(got, "\x1b") {
		t.Fatalf("description retained raw escape: %q", got)
	}
	if !strings.Contains(got, `"safe\\x1b[0m --danger"`) {
		t.Fatalf("escaped spaced argument lost its quotes: %q", got)
	}
	if !strings.HasSuffix(got, ` ""`) {
		t.Fatalf("empty argument disappeared: %q", got)
	}
}

func TestExecRefusesWhenReachChangesAfterPermissionSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after execution.SandboxMode
	}{
		{"off-to-on", execution.SandboxOff, execution.SandboxOn},
		{"on-to-off", execution.SandboxOn, execution.SandboxOff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controller, err := execution.NewController(execution.TestingVerifiedCapability(), tc.before)
			if err != nil {
				t.Fatal(err)
			}
			registry, err := NewRegistryWithExecution(t.TempDir(), controller)
			if err != nil {
				t.Fatal(err)
			}
			tool, _ := registry.Get("exec")
			plan, err := tool.Plan(json.RawMessage(`{"command":["sh","-c","exit 99"]}`))
			if err != nil {
				t.Fatal(err)
			}
			if err := controller.SetSandbox(tc.after); err != nil {
				t.Fatal(err)
			}
			result, err := plan.Run(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !result.IsError || !strings.Contains(result.Content, "changed after permission") {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestExecRuns(t *testing.T) {
	r, root := newRegistry(t)
	writeFile(t, filepath.Join(root, "marker.txt"), "x")

	t.Setenv("SB_TOOLS_EXEC_HELPER", "list")
	res := run(t, r, "exec", map[string]any{"command": []string{os.Args[0]}})
	if res.IsError {
		t.Fatalf("exec failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "marker.txt") {
		t.Errorf("command did not run in the workspace: %q", res.Content)
	}

	t.Setenv("SB_TOOLS_EXEC_HELPER", "fail")
	failed := run(t, r, "exec", map[string]any{"command": []string{os.Args[0]}})
	if !failed.IsError {
		t.Error("a non-zero exit must be reported as a tool error")
	}
	if !strings.Contains(failed.Content, "exit status 1") {
		t.Errorf("content = %q, want the exit status", failed.Content)
	}
}

func TestExecRejectsBadInputAtPlanTime(t *testing.T) {
	r, _ := newRegistry(t)
	cases := map[string]map[string]any{
		"empty command":        {"command": []string{}},
		"shell with argv":      {"command": []string{"echo", "hi"}, "shell": true},
		"timeout over ceiling": {"command": []string{"sleep", "1"}, "timeout_seconds": 100000},
	}
	for name, input := range cases {
		if _, err := tryRun(r, "exec", input); err == nil {
			t.Errorf("%s should be rejected at plan time", name)
		}
	}
}

// Tool definitions sit in the frozen zone of the context layout. A set that
// reshuffles between requests would invalidate the cached prefix every turn.
func TestDefinitionsAreDeterministic(t *testing.T) {
	r, _ := newRegistry(t)

	first := r.Definitions()
	for range 5 {
		next := r.Definitions()
		for i := range first {
			if first[i].Name != next[i].Name {
				t.Fatalf("definition order changed: %s then %s", first[i].Name, next[i].Name)
			}
		}
	}

	var names []string
	for _, d := range first {
		names = append(names, d.Name)
		if d.Description == "" {
			t.Errorf("%s has no description", d.Name)
		}
		if !json.Valid(d.Schema) {
			t.Errorf("%s has an invalid schema", d.Name)
		}
	}
	if got := strings.Join(names, ","); got != "ask,edit,exec,glob,grep,proc,read,todo,webfetch,websearch,write" {
		t.Errorf("tools = %s, want the core suite in sorted order", got)
	}
}

func TestOnlyReadsAreParallelSafe(t *testing.T) {
	r, _ := newRegistry(t)
	for _, name := range []string{"read", "write", "edit", "exec", "glob", "grep", "todo", "websearch", "webfetch", "ask", "proc"} {
		tool, _ := r.Get(name)
		want := name == "read" || name == "glob" || name == "grep" ||
			name == "websearch" || name == "webfetch"
		if tool.ParallelSafe() != want {
			t.Errorf("%s.ParallelSafe() = %v, want %v", name, tool.ParallelSafe(), want)
		}
	}
}

// TestCoreNamesMatchesTheRegistry is what lets assembly validate an agent's
// tool grant against the static list instead of building a registry: the two
// drift, this fails.
func TestCoreNamesMatchesTheRegistry(t *testing.T) {
	r, _ := newRegistry(t)
	var names []string
	for _, def := range r.Definitions() {
		names = append(names, def.Name)
	}
	if got, want := strings.Join(CoreNames(), ","), strings.Join(names, ","); got != want {
		t.Errorf("CoreNames() = %s, registry holds %s", got, want)
	}
}

func TestRestrictNarrowsAndOnlyNarrows(t *testing.T) {
	r, _ := newRegistry(t)
	if err := r.Restrict([]string{"read", "grep"}); err != nil {
		t.Fatal(err)
	}
	defs := r.Definitions()
	if len(defs) != 2 || defs[0].Name != "grep" || defs[1].Name != "read" {
		t.Errorf("Definitions() = %v, want grep and read in sorted order", defs)
	}
	if _, ok := r.Get("write"); ok {
		t.Error("write survived a restriction that excluded it")
	}

	r2, _ := newRegistry(t)
	if err := r2.Restrict([]string{"read", "delegate"}); err == nil {
		t.Error("a name outside the suite must be an error, never an addition")
	}
}

// TestForgetAllVersionsReimposesReadBeforeWrite pins what a session swap
// relies on: a fresh context must read a file again before it may write it,
// whatever the registry remembered from the context that is gone.
func TestForgetAllVersionsReimposesReadBeforeWrite(t *testing.T) {
	r, root := newRegistry(t)
	path := filepath.Join(root, "kept.txt")
	writeFile(t, path, "original")

	run(t, r, "read", map[string]any{"path": "kept.txt"})
	if res := run(t, r, "write", map[string]any{"path": "kept.txt", "content": "updated"}); res.IsError {
		t.Fatalf("write after read failed: %s", res.Content)
	}

	r.ForgetAllVersions()
	res := run(t, r, "write", map[string]any{"path": "kept.txt", "content": "again"})
	if !res.IsError || !strings.Contains(res.Content, "not been read") {
		t.Fatalf("write after the swap = %+v, want a refusal demanding a fresh read", res)
	}
}

// The §6.7 re-injection skip: a full read of a byte-identical file answers
// with a marker instead of the content, and everything that could make the
// marker a lie — a partial first read, a mutation, an external change, a
// context swap — degrades to full injection.
func TestUnchangedFullReadSkipsReinjection(t *testing.T) {
	r, root := newRegistry(t)
	writeFile(t, filepath.Join(root, "a.txt"), "the contents")

	first := run(t, r, "read", map[string]any{"path": "a.txt"})
	if first.Content != "the contents" {
		t.Fatalf("first read = %q, want the bytes", first.Content)
	}
	again := run(t, r, "read", map[string]any{"path": "a.txt"})
	if !strings.Contains(again.Content, "unchanged since you last read it") || strings.Contains(again.Content, "the contents") {
		t.Fatalf("second read = %q, want the marker and not the bytes", again.Content)
	}
	// The skipped read still refreshes the stale check, so writing after it
	// works the way it would after a full injection.
	if res := run(t, r, "write", map[string]any{"path": "a.txt", "content": "new"}); res.IsError {
		t.Fatalf("write after a skipped read failed: %s", res.Content)
	}
}

func TestPartialReadNeverArmsTheSkip(t *testing.T) {
	r, root := newRegistry(t)
	writeFile(t, filepath.Join(root, "a.txt"), "one\ntwo\nthree")

	run(t, r, "read", map[string]any{"path": "a.txt", "offset": 1, "limit": 1})
	full := run(t, r, "read", map[string]any{"path": "a.txt"})
	if !strings.Contains(full.Content, "three") {
		t.Fatalf("full read after a slice = %q, want the whole file: the context never held it", full.Content)
	}
}

func TestMutationAndExternalChangeDisarmTheSkip(t *testing.T) {
	r, root := newRegistry(t)
	path := filepath.Join(root, "a.txt")
	writeFile(t, path, "original")

	run(t, r, "read", map[string]any{"path": "a.txt"})
	run(t, r, "edit", map[string]any{"path": "a.txt", "old_string": "original", "new_string": "edited"})
	if res := run(t, r, "read", map[string]any{"path": "a.txt"}); res.Content != "edited" {
		t.Fatalf("read after edit = %q, want the new bytes injected", res.Content)
	}

	writeFile(t, path, "changed outside")
	if res := run(t, r, "read", map[string]any{"path": "a.txt"}); res.Content != "changed outside" {
		t.Fatalf("read after external change = %q, want the new bytes injected", res.Content)
	}
}

func TestSessionSwapDisarmsTheSkip(t *testing.T) {
	r, root := newRegistry(t)
	writeFile(t, filepath.Join(root, "a.txt"), "the contents")

	run(t, r, "read", map[string]any{"path": "a.txt"})
	r.ForgetAllVersions()
	if res := run(t, r, "read", map[string]any{"path": "a.txt"}); res.Content != "the contents" {
		t.Fatalf("read after the swap = %q, want full injection: the new context holds nothing", res.Content)
	}
}
