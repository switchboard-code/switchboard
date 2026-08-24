package watch

import (
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/execution"
)

func TestCommitWithholdsTruncatedVerifierFragments(t *testing.T) {
	token := "ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	fragmented := token[:17] + "\n[omitted]\n" + token[18:]
	w := New("unused", t.TempDir())
	report := w.Commit(Observation{result: execution.Result{
		Output: fragmented, ExitCode: 1, Truncated: true,
	}})
	if len(report.New) != 1 || !strings.Contains(report.New[0].Line, "output exceeded") {
		t.Fatalf("truncated verifier did not produce a generic failure: %+v", report)
	}
	for _, failure := range report.New {
		if strings.Contains(failure.Line, "ghp_") || strings.Contains(failure.Signature, "ghp_") {
			t.Fatalf("verifier fragments reached its report: %+v", failure)
		}
	}
}
