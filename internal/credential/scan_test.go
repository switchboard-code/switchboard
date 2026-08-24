package credential

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestScanPromptFindsKnownTokenShapes(t *testing.T) {
	// Every fixture token is split after its prefix so no contiguous
	// key-shaped literal exists in this file: repository secret scanners
	// read source, and a pattern-valid dummy raises the same alarm a real
	// key would. The runtime strings are unchanged. Do not rejoin them.
	cases := []struct {
		text string
		kind string
	}{
		{"here is sk-ant-api03-" + "abcdefghijklmnopqrstuvwx my key", "an Anthropic API key"},
		{"OPENAI_API_KEY=sk-proj-" + "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN", "an OpenAI API key"},
		{"token: ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789", "a GitHub token"},
		{"github_pat_" + "11ABCDEFG0abcdefghijklm", "a GitHub fine-grained token"},
		{"glpat-" + "abcdefghij0123456789", "a GitLab token"},
		{"xoxb-" + "1234567890-abcdef", "a Slack token"},
		{"aws_access_key_id = AKIAIOSFODNN7EXAMPLE", "an AWS access key ID"},
		{"key=AIza" + "SyA-abcdefghijklmnopqrstuvwxyz01234", "a Google API key"},
		{"sk_live_" + "abcdefghij0123456789", "a Stripe live key"},
		{"npm_" + "abcdefghijklmnopqrstuvwxyz0123456789", "an npm token"},
		{"hf_" + "abcdefghijklmnopqrstuvwxyz01234", "a Hugging Face token"},
		{"-----BEGIN RSA PRIVATE KEY-----", "a private key block"},
	}
	for _, c := range cases {
		leaks := ScanPrompt(c.text)
		if len(leaks) == 0 {
			t.Errorf("scan missed %s in %q", c.kind, c.text)
			continue
		}
		if leaks[0].Kind != c.kind {
			t.Errorf("scan of %q called it %q, want %q", c.text, leaks[0].Kind, c.kind)
		}
	}
}

// The floors are the precision: a prefix mentioned in prose, without the
// token attached, is conversation about keys, not a key.
func TestScanPromptLeavesProseAlone(t *testing.T) {
	for _, text := range []string{
		"set sk-ant-... in the environment",
		"a GitHub token starts with ghp_",
		"the AKIA prefix marks an access key id",
		"rotate your sk_live key in the dashboard",
		"review this diff for anything key-shaped",
	} {
		if leaks := ScanPrompt(text); len(leaks) != 0 {
			t.Errorf("scan flagged prose %q as %v", text, leaks)
		}
	}
}

func TestScanPromptDeduplicatesRepeatedPastes(t *testing.T) {
	key := "ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789"
	if leaks := ScanPrompt(key + " and again " + key); len(leaks) != 1 {
		t.Errorf("one key pasted twice reported %d findings", len(leaks))
	}
}

func TestScanPromptDoesNotDependOnTheByteBeforeAnIssuerPrefix(t *testing.T) {
	key := "ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789"
	text := strings.Repeat("x", 4096) + key
	leaks := ScanPrompt(text)
	if len(leaks) != 1 || leaks[0].Kind != "a GitHub token" {
		t.Fatalf("scan missed a credential adjacent to capture padding: %v", leaks)
	}
	if redacted := Redact(text, leaks); strings.Contains(redacted, key) ||
		!strings.Contains(redacted, "[redacted: a GitHub token]") {
		t.Fatalf("redaction retained an adjacent credential: %q", redacted)
	}
}

func TestSafePrefixForTruncationKeepsCompleteTokensForTheGate(t *testing.T) {
	complete := "ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789"
	crossing := "ghp_" + "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	text := complete + " safe padding " + crossing + " tail"
	limit := strings.Index(text, crossing) + len(crossing) - 1

	got, cut := SafePrefixForTruncation(text, limit)
	if cut != strings.Index(text, crossing) {
		t.Fatalf("safe cut = %d, want crossing token start %d", cut, strings.Index(text, crossing))
	}
	if !strings.Contains(got, complete) || len(ScanPrompt(got)) != 1 {
		t.Fatalf("complete in-bound token did not remain available to the gate: %q", got)
	}
	if strings.Contains(got, crossing) || strings.Contains(got, "ghp_ABC") ||
		!strings.Contains(got, "[redacted: a GitHub token]") {
		t.Fatalf("boundary-crossing token was not safely replaced: %q", got)
	}
}

func TestSafePrefixForTruncationLeavesOrdinaryAndExcludedTokensAlone(t *testing.T) {
	secret := "ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789"
	for _, tc := range []struct {
		name  string
		text  string
		limit int
		want  string
		cut   int
	}{
		{name: "ordinary", text: "abcdef", limit: 4, want: "abcd", cut: 4},
		{name: "whole input", text: "abcdef", limit: 8, want: "abcdef", cut: 6},
		{name: "token begins at boundary", text: "prefix" + secret, limit: len("prefix"), want: "prefix", cut: len("prefix")},
		{name: "zero", text: secret, limit: 0, want: "", cut: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, cut := SafePrefixForTruncation(tc.text, tc.limit)
			if got != tc.want || cut != tc.cut {
				t.Fatalf("SafePrefixForTruncation = %q, %d; want %q, %d", got, cut, tc.want, tc.cut)
			}
		})
	}
}

func TestSafePrefixForTruncationRedactsEveryCredentialAcrossEveryBoundary(t *testing.T) {
	cases := []struct {
		name   string
		secret string
		kind   string
	}{
		{"anthropic", "sk-ant-api03-" + "abcdefghijklmnopqrstuvwx", "an Anthropic API key"},
		{"openai", "sk-proj-" + "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN", "an OpenAI API key"},
		{"github", "ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789", "a GitHub token"},
		{"github fine grained", "github_pat_" + "11ABCDEFG0abcdefghijklm", "a GitHub fine-grained token"},
		{"gitlab", "glpat-" + "abcdefghij0123456789", "a GitLab token"},
		{"slack", "xoxb-" + "1234567890-abcdef", "a Slack token"},
		{"aws", "AKIAIOSFODNN7EXAMPLE", "an AWS access key ID"},
		{"google", "AIza" + "SyA-abcdefghijklmnopqrstuvwxyz01234", "a Google API key"},
		{"stripe", "sk_live_" + "abcdefghij0123456789", "a Stripe live key"},
		{"npm", "npm_" + "abcdefghijklmnopqrstuvwxyz0123456789", "an npm token"},
		{"hugging face", "hf_" + "abcdefghijklmnopqrstuvwxyz01234", "a Hugging Face token"},
		{"private key", "-----BEGIN RSA PRIVATE KEY-----\nbody\n-----END RSA PRIVATE KEY-----", "a private key block"},
	}
	const before = "safe prefix: "
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text := before + tc.secret + " harmless tail"
			for boundary := len(before) + 1; boundary < len(before)+len(tc.secret); boundary++ {
				got, cut := SafePrefixForTruncation(text, boundary)
				if cut != len(before) {
					t.Fatalf("boundary %d retained %d source bytes, want %d", boundary, cut, len(before))
				}
				want := before + "[redacted: " + tc.kind + "]"
				if got != want {
					t.Fatalf("boundary %d = %q, want %q", boundary, got, want)
				}
			}
		})
	}
}

func TestSafePrefixForTruncationConservativelyBoundsAnAmbiguousPrivateKeyHeader(t *testing.T) {
	const before = "safe prefix: "
	longLabel := strings.Repeat("A", scanShape.boundaryLookahead+64)
	text := before + privateKeyBegin + longLabel + privateKeySuffix + "\nbody"
	boundary := len(before) + len(privateKeyBegin) + 1
	got, cut := SafePrefixForTruncation(text, boundary)
	if cut != len(before) || got != before+"[redacted: a private key block]" {
		t.Fatalf("long boundary-spanning header = %q, %d", got, cut)
	}

	// Put the bounded scan's end inside the suffix hyphens. Seeing '-' is
	// normally enough to reject a non-key PEM label, but a partial copy of the
	// required suffix must remain ambiguous until the bytes after the cap are
	// considered.
	boundary = len(before) + 1
	scanEnd := boundary + scanShape.boundaryLookahead
	suffixPrefix := "PRIVATE KEY--"
	labelBytes := scanEnd - len(before) - len(privateKeyBegin) - len(suffixPrefix)
	straddlingSuffix := before + privateKeyBegin + strings.Repeat("A", labelBytes) + privateKeySuffix + "\nbody"
	got, cut = SafePrefixForTruncation(straddlingSuffix, boundary)
	if cut != len(before) || got != before+"[redacted: a private key block]" {
		t.Fatalf("suffix-straddling private header = %q, %d", got, cut)
	}

	certificate := before + "-----BEGIN CERTIFICATE-----\nbody"
	got, cut = SafePrefixForTruncation(certificate, boundary)
	if cut != boundary || got != certificate[:boundary] {
		t.Fatalf("ordinary PEM label was treated as a private key: %q, %d", got, cut)
	}
}

func TestSafePrefixForTruncationBoundsOrdinaryTailWork(t *testing.T) {
	if testing.Short() {
		t.Skip("performance regression check")
	}
	// The retained prefix is 4 KiB; making the omitted tail four MiB must not
	// make each call scan that tail. The generous threshold only distinguishes
	// bounded prefix work from the prior all-pattern, whole-tail regexp scan,
	// particularly under -race.
	text := strings.Repeat("é", 2<<20)
	started := time.Now()
	for i := 0; i < 8; i++ {
		got, cut := SafePrefixForTruncation(text, 4<<10)
		if cut != 4<<10 || len(got) != 4<<10 {
			t.Fatalf("bounded prefix = %d bytes, cut %d", len(got), cut)
		}
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("bounded truncation scanned the ordinary tail: %s", elapsed)
	}
}

func BenchmarkSafePrefixForTruncationBoundedTail(b *testing.B) {
	text := strings.Repeat("é", 2<<20)
	b.ReportAllocs()
	b.SetBytes(4 << 10)
	for i := 0; i < b.N; i++ {
		SafePrefixForTruncation(text, 4<<10)
	}
}

// The Secret rule applies to findings too: no rendering shows the match.
func TestLeakHasNoRenderingThatShowsTheSecret(t *testing.T) {
	secret := "ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789"
	leaks := ScanPrompt("token " + secret)
	if len(leaks) != 1 {
		t.Fatalf("expected one finding, got %v", leaks)
	}
	for _, rendered := range []string{
		fmt.Sprintf("%v", leaks[0]),
		fmt.Sprintf("%+v", leaks[0]),
		fmt.Sprintf("%#v", leaks[0]),
		fmt.Sprintf("%s", leaks[0]),
		leaks[0].Masked(),
	} {
		if strings.Contains(rendered, secret) {
			t.Errorf("a rendering shows the secret: %q", rendered)
		}
	}
}

// The property the gate promises: after redact, no key material remains
// outbound. For a PEM that means the body and END line go with the header,
// and a block whose END was lost in the paste is stripped to the end of
// the text, because a truncated key is still a key.
func TestRedactStripsAWholePrivateKeyBlock(t *testing.T) {
	body := "MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7"
	pem := "-----BEGIN RSA PRIVATE KEY-----\n" + body + "\n-----END RSA PRIVATE KEY-----"
	for name, text := range map[string]string{
		"complete":  "my key is\n" + pem + "\nplease review",
		"truncated": "my key is\n-----BEGIN RSA PRIVATE KEY-----\n" + body,
	} {
		out := Redact(text, ScanPrompt(text))
		if strings.Contains(out, body) {
			t.Errorf("%s: redaction left the key body outbound: %q", name, out)
		}
		if strings.Contains(out, "-----END") {
			t.Errorf("%s: redaction left the block's tail: %q", name, out)
		}
		if !strings.Contains(out, "[redacted: a private key block]") {
			t.Errorf("%s: redaction does not say what stood there: %q", name, out)
		}
	}
	if !strings.Contains(Redact("my key is\n"+pem+"\nplease review", ScanPrompt(pem)), "please review") {
		t.Error("redaction took the prose after the block with it")
	}
}

func TestRedactReplacesTheMatchAndNamesTheKind(t *testing.T) {
	secret := "sk-ant-api03-abcdefghijklmnopqrstuvwx"
	text := "use " + secret + " for auth"
	leaks := ScanPrompt(text)
	out := Redact(text, leaks)
	if strings.Contains(out, secret) {
		t.Errorf("redaction left the secret in place: %q", out)
	}
	if !strings.Contains(out, "[redacted: an Anthropic API key]") {
		t.Errorf("redaction does not say what stood there: %q", out)
	}
}
