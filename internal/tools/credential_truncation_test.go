package tools

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/switchboard-code/switchboard/internal/execution"
)

const truncationBoundaryToken = "ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestExecWithholdsCaptureFragmentsAcrossCredentialBoundary(t *testing.T) {
	// This is the shape capture.String returns when the omitted middle removes
	// one byte from a token: neither retained fragment is independently a leak.
	fragmented := truncationBoundaryToken[:20] +
		"\n[switchboard: 1 bytes of output omitted from the middle]\n" +
		truncationBoundaryToken[21:]
	result := execResult(execution.Result{Output: fragmented, ExitCode: 1, Truncated: true})
	if strings.Contains(result.Content, "ghp_") || strings.Contains(result.Content, "aaaaaaaa") {
		t.Fatalf("truncated command fragments reached the result: %q", result.Content)
	}
	if !result.IsError || !strings.Contains(result.Content, "output withheld") || !strings.Contains(result.Content, "exit status 1") {
		t.Fatalf("truncated command lost its safe status diagnostic: %+v", result)
	}
}

func TestProcRedactsCompleteOutputBeforeTailCap(t *testing.T) {
	text := strings.Repeat("x", maxProcOutput-8) + truncationBoundaryToken + "\nlatest"
	got := renderProcRead(execution.BackgroundStatus{ID: "bg1", Running: true}, text, false)
	if strings.Contains(got, truncationBoundaryToken) || strings.Contains(got, "ghp_") {
		t.Fatalf("proc tail cap exposed a credential fragment: %q", got)
	}
	if !strings.Contains(got, "[redacted: a GitHub token]") || !strings.Contains(got, "latest") {
		t.Fatalf("proc lost safe recent output: %q", got)
	}
}

func TestProcWithholdsTruncatedAndRepairsInvalidUTF8(t *testing.T) {
	fragmented := truncationBoundaryToken[:18] + " omitted " + truncationBoundaryToken[19:]
	if got := renderProcRead(execution.BackgroundStatus{ID: "bg1"}, fragmented, true); strings.Contains(got, "ghp_") {
		t.Fatalf("truncated background fragments reached the result: %q", got)
	}
	invalid := strings.Repeat("x", maxProcOutput) + string([]byte{0xff, 0xfe}) + "tail"
	if got := renderProcRead(execution.BackgroundStatus{ID: "bg1"}, invalid, false); !utf8.ValidString(got) {
		t.Fatalf("proc returned invalid UTF-8: %q", got)
	}
}

func TestAstGrepWithholdsTruncatedFailureBeforeUsingItsExitOutput(t *testing.T) {
	fragmented := truncationBoundaryToken[:20] + " omitted " + truncationBoundaryToken[21:]
	result, stop := astGrepCaptureFailure(execution.Result{
		Output: fragmented, ExitCode: 2, Truncated: true,
	})
	if !stop || !result.IsError || !strings.Contains(result.Content, "too large") {
		t.Fatalf("truncated ast-grep capture was not refused: stop=%t result=%+v", stop, result)
	}
	if strings.Contains(result.Content, "ghp_") || strings.Contains(result.Content, "aaaaaaaa") {
		t.Fatalf("truncated ast-grep fragments reached its error: %q", result.Content)
	}
}
