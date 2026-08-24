package provider

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
)

const apiErrorBoundaryToken = "ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestAPIErrorSanitizesCompleteCredentialText(t *testing.T) {
	err := (&APIError{Provider: "test", StatusCode: 400, Body: "rejected " + apiErrorBoundaryToken}).Error()
	if strings.Contains(err, apiErrorBoundaryToken) || strings.Contains(err, "ghp_") {
		t.Fatalf("API error rendered a credential: %q", err)
	}
	if !strings.Contains(err, "[redacted: a GitHub token]") {
		t.Fatalf("API error lost its useful redacted diagnostic: %q", err)
	}
}

func TestReadAPIErrorBodyFailsClosedAcrossCapBoundary(t *testing.T) {
	// The last byte is the one beyond the cap; keeping the prefix would leave
	// almost a whole token that no longer satisfies the scanner's length floor.
	prefix := strings.Repeat("x", MaxAPIErrorBodyBytes-len(apiErrorBoundaryToken)+1)
	got := string(ReadAPIErrorBody(strings.NewReader(prefix + apiErrorBoundaryToken)))
	if strings.Contains(got, "ghp_") || strings.Contains(got, apiErrorBoundaryToken) {
		t.Fatalf("over-cap provider body returned a credential fragment: %q", got)
	}
	if !strings.Contains(got, "withheld") {
		t.Fatalf("over-cap provider body did not explain the refusal: %q", got)
	}
}

func TestSanitizeAPIErrorTextRepairsInvalidUTF8(t *testing.T) {
	got := SanitizeAPIErrorText(string(bytes.Repeat([]byte{0xff}, 4)) + apiErrorBoundaryToken)
	if !utf8.ValidString(got) || strings.Contains(got, "ghp_") {
		t.Fatalf("sanitized API error = %q, valid=%v", got, utf8.ValidString(got))
	}
}
