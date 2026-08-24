package eval

// The corpus doubles as a baseline instrument (§8.6): the same tasks and the
// same verifier, materialised for an agent that is not this one. An external
// CLI cannot call Task.Setup, so these two helpers put the corpus on disk
// and read the verdicts back off it; both skip unless their environment
// variable names a directory, so the ordinary suite never runs them.
//
// The selection is one task per package, first in corpus order — a
// deterministic cut chosen before any tool ran, because picking tasks after
// seeing results is how a comparison stops being one.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// benchTasks is the cut of the corpus a lane runs, skipping tasks another
// platform owns. The default is one task per package; SB_BENCH_CUT=all
// widens to every spec, for a run that wants the corpus's full breadth
// rather than its spread.
func benchTasks(root string) []Task {
	all := os.Getenv("SB_BENCH_CUT") == "all"
	seen := map[string]bool{}
	var out []Task
	for _, s := range specs {
		if (!all && seen[s.pkg]) || (s.goos != "" && s.goos != runtime.GOOS) {
			continue
		}
		seen[s.pkg] = true
		out = append(out, taskFor(root, s))
	}
	return out
}

// TestMaterializeBench writes one broken workspace per selected task under
// $SB_BENCH_MATERIALIZE, with a manifest line naming each task's prompt.
func TestMaterializeBench(t *testing.T) {
	root := os.Getenv("SB_BENCH_MATERIALIZE")
	if root == "" {
		t.Skip("set SB_BENCH_MATERIALIZE to a directory to materialise the bench corpus")
	}
	manifest, err := os.Create(filepath.Join(root, "manifest.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer manifest.Close()

	for _, task := range benchTasks(repoRoot(t)) {
		dir := filepath.Join(root, task.ID)
		if err := task.Setup(dir); err != nil {
			t.Fatalf("materialising %s: %v", task.ID, err)
		}
		line, err := json.Marshal(map[string]string{"id": task.ID, "prompt": task.Prompt})
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(manifest, "%s\n", line)
	}
}

// TestVerifyBench judges every task directory under $SB_BENCH_VERIFY with
// the corpus's own verifier — the tests plus the mustContain checks that
// keep "delete the failing test" from counting as a solve — and writes one
// verdict line per task to verdicts.jsonl in the same directory.
func TestVerifyBench(t *testing.T) {
	root := os.Getenv("SB_BENCH_VERIFY")
	if root == "" {
		t.Skip("set SB_BENCH_VERIFY to a materialised bench directory to judge it")
	}
	out, err := os.Create(filepath.Join(root, "verdicts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()

	for _, task := range benchTasks(repoRoot(t)) {
		dir := filepath.Join(root, task.ID)
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		solved, detail, err := task.Verify(context.Background(), dir)
		if err != nil {
			t.Fatalf("verifying %s: %v", task.ID, err)
		}
		line, jerr := json.Marshal(map[string]any{"id": task.ID, "solved": solved, "detail": detail})
		if jerr != nil {
			t.Fatal(jerr)
		}
		fmt.Fprintf(out, "%s\n", line)
		t.Logf("%s solved=%v %s", task.ID, solved, detail)
	}
}
