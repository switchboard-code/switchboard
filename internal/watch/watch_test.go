//go:build unix

package watch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// scripted builds a watch whose verdict is whatever the test last wrote:
// the command prints out.txt and exits with the code in code.txt, so a test
// moves the "suite" between states by rewriting two files, the way real runs
// differ between invocations.
func scripted(t *testing.T) (*Watch, func(output string, code string)) {
	t.Helper()
	dir := t.TempDir()
	set := func(output, code string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "out.txt"), []byte(output), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "code.txt"), []byte(code), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return New("cat out.txt; exit $(cat code.txt)", dir), set
}

func TestFirstFailingRunReportsEverythingAsNew(t *testing.T) {
	w, set := scripted(t)
	set("--- FAIL: TestAlpha\n--- FAIL: TestBeta\nok  other\n", "1")

	rep := w.Run(context.Background())
	if rep.Err != nil {
		t.Fatal(rep.Err)
	}
	if rep.Passed {
		t.Fatal("exit 1 reported as a pass")
	}
	if !rep.FirstRun {
		t.Error("the first run did not say so")
	}
	if len(rep.New) != 2 || rep.Persisting != 0 {
		t.Fatalf("want 2 new and 0 persisting, got %d new %d persisting", len(rep.New), rep.Persisting)
	}
	if rep.New[0].Line != "--- FAIL: TestAlpha" {
		t.Errorf("the failing line did not survive verbatim: %q", rep.New[0].Line)
	}
	if !rep.Changed() {
		t.Error("new failures did not count as a change")
	}
}

func TestTheSameFailureTwiceIsSilence(t *testing.T) {
	w, set := scripted(t)
	set("--- FAIL: TestAlpha\n", "1")
	w.Run(context.Background())

	rep := w.Run(context.Background())
	if len(rep.New) != 0 || rep.Persisting != 1 {
		t.Fatalf("a repeat run reported news: %d new %d persisting", len(rep.New), rep.Persisting)
	}
	if rep.Changed() {
		t.Error("an unchanged verdict counted as a change")
	}
	if len(rep.Signatures) != 1 {
		t.Errorf("the detector feed lost the repeat: %d signatures", len(rep.Signatures))
	}
}

func TestOnlyTheFreshFailureIsNew(t *testing.T) {
	w, set := scripted(t)
	set("--- FAIL: TestAlpha\n", "1")
	w.Run(context.Background())

	set("--- FAIL: TestAlpha\n--- FAIL: TestGamma\n", "1")
	rep := w.Run(context.Background())
	if len(rep.New) != 1 || rep.New[0].Line != "--- FAIL: TestGamma" {
		t.Fatalf("want only TestGamma as new, got %+v", rep.New)
	}
	if rep.Persisting != 1 {
		t.Errorf("TestAlpha stopped persisting: %d", rep.Persisting)
	}
}

func TestGreenAfterRedIsTheOneGreenWorthAnnouncing(t *testing.T) {
	w, set := scripted(t)
	set("--- FAIL: TestAlpha\n", "1")
	w.Run(context.Background())

	set("ok\n", "0")
	rep := w.Run(context.Background())
	if !rep.Passed || !rep.WentGreen {
		t.Fatalf("red to green not reported: passed=%v wentGreen=%v", rep.Passed, rep.WentGreen)
	}
	if !rep.Changed() {
		t.Error("going green did not count as a change")
	}

	rep = w.Run(context.Background())
	if rep.WentGreen || rep.Changed() {
		t.Error("a pass after a pass was announced")
	}
}

func TestAFixedFailureThatComesBackIsNewAgain(t *testing.T) {
	w, set := scripted(t)
	set("--- FAIL: TestAlpha\n", "1")
	w.Run(context.Background())
	set("ok\n", "0")
	w.Run(context.Background())

	set("--- FAIL: TestAlpha\n", "1")
	rep := w.Run(context.Background())
	if len(rep.New) != 1 {
		t.Fatalf("a failure returning after a fix was not news: %+v", rep)
	}
}

func TestUnrecognizableFailureOutputStandsInWithItsFirstLine(t *testing.T) {
	w, set := scripted(t)
	set("something went sideways\n", "3")

	rep := w.Run(context.Background())
	if rep.Passed {
		t.Fatal("exit 3 reported as a pass")
	}
	if len(rep.New) != 1 || rep.New[0].Line != "something went sideways" {
		t.Fatalf("the stand-in line is wrong: %+v", rep.New)
	}

	// The stand-in reduces like any failure, so the same silence repeats as
	// a repeat.
	rep = w.Run(context.Background())
	if len(rep.New) != 0 || rep.Persisting != 1 {
		t.Fatalf("the synthesized failure did not dedupe: %+v", rep)
	}
}

func TestNoOutputAtAllStillProducesAFailure(t *testing.T) {
	w, set := scripted(t)
	set("", "2")

	rep := w.Run(context.Background())
	if len(rep.New) != 1 || rep.New[0].Line == "" {
		t.Fatalf("an empty failing run produced no failure: %+v", rep)
	}
}

func TestRedReflectsTheLastRun(t *testing.T) {
	w, set := scripted(t)
	if w.Red() {
		t.Error("red before any run")
	}
	set("--- FAIL: TestAlpha\n", "1")
	w.Run(context.Background())
	if !w.Red() {
		t.Error("not red after a failing run")
	}
	set("ok\n", "0")
	w.Run(context.Background())
	if w.Red() {
		t.Error("still red after a pass")
	}
}

func TestObservationDoesNotAdvanceTheBaselineUntilCommitted(t *testing.T) {
	w, set := scripted(t)
	set("--- FAIL: TestStaged\n", "1")

	observation := w.Observe(context.Background())
	if w.Red() {
		t.Fatal("an uncommitted observation changed the watch baseline")
	}
	rep := w.Commit(observation)
	if !w.Red() || !rep.FirstRun || len(rep.New) != 1 {
		t.Fatalf("committing the staged observation did not advance exactly once: %+v", rep)
	}
}

func TestResetBaselineMakesTheCurrentFailureNewToTheNextConversation(t *testing.T) {
	w, set := scripted(t)
	set("--- FAIL: TestAgain\n", "1")
	w.Run(context.Background())
	if rep := w.Run(context.Background()); len(rep.New) != 0 || rep.Persisting != 1 {
		t.Fatalf("precondition: repeated failure was not deduplicated: %+v", rep)
	}

	w.ResetBaseline()
	rep := w.Run(context.Background())
	if !rep.FirstRun || len(rep.New) != 1 || rep.Persisting != 0 {
		t.Fatalf("reset baseline leaked the prior conversation's delta: %+v", rep)
	}
}
