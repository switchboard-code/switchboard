package credential

// Outbound scanning: the inverse of everything else in this package. The
// rest of these files keep credentials the program was entrusted with from
// leaking out; this one catches credentials the user is about to leak
// themselves — a key pasted into a prompt, an @mentioned .env, a `!env`
// transcript riding into the next turn. A prompt is about to be written to
// the session log and sent to a provider, and both are places this package
// promises a credential never appears.
//
// The patterns are deliberately narrow: known issuer prefixes only, no
// entropy guessing, because a gate that cries wolf trains the user to wave
// everything through. A string shaped like `ghp_` plus thirty-six
// alphanumerics is a GitHub token; there is no innocent collision worth
// accommodating. What this list does not recognize it does not flag, and
// that miss is stated in the README rather than papered over with
// heuristics.

import (
	"regexp"
	"strings"
)

// Leak is one key-shaped string found in outbound content. The match is
// unexported and has no accessor: redaction happens in this package, so the
// raw text never travels, and the Secret rule about renderings holds — a
// Leak that reaches a log line or an error prints its kind and a stub, not
// the credential.
type Leak struct {
	Kind  string // "a GitHub token", "an Anthropic API key", ...
	match string
}

// Masked identifies the finding without reproducing it: the issuer prefix
// the user already knows, and nothing after it.
func (l Leak) Masked() string {
	head := l.match
	if strings.HasPrefix(head, "-----BEGIN") {
		return "-----BEGIN …"
	}
	if i := strings.IndexAny(head, "-_"); i >= 0 && i < len(head)-1 {
		head = head[:i+1]
	} else if len(head) > 6 {
		head = head[:6]
	}
	return head + "…"
}

func (l Leak) String() string   { return l.Kind + " (" + l.Masked() + ")" }
func (l Leak) GoString() string { return l.String() }

type leakPattern struct {
	kind string
	re   *regexp.Regexp
}

// Every pattern here was checked against the issuer's published token
// format; the length floors are what keep prose that mentions a prefix
// ("set sk-ant-... in the env") from matching without the token attached.
var leakPatterns = []leakPattern{
	// Do not require a word boundary before a distinctive issuer prefix. Tool
	// output is an arbitrary byte stream: a credential can directly follow a
	// bounded capture's padding or another identifier byte. The issuer prefix
	// plus its published length floor supplies the precision; a lexical
	// boundary would make the outbound guarantee depend on the preceding byte.
	{"an Anthropic API key", regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`)},
	{"an OpenAI API key", regexp.MustCompile(`sk-(?:proj-)?[A-Za-z0-9_-]{40,}`)},
	{"a GitHub token", regexp.MustCompile(`(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36,}`)},
	{"a GitHub fine-grained token", regexp.MustCompile(`github_pat_[A-Za-z0-9_]{22,}`)},
	{"a GitLab token", regexp.MustCompile(`glpat-[A-Za-z0-9_-]{20,}`)},
	{"a Slack token", regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`)},
	{"an AWS access key ID", regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
	{"a Google API key", regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`)},
	{"a Stripe live key", regexp.MustCompile(`[sr]k_live_[A-Za-z0-9]{20,}`)},
	{"an npm token", regexp.MustCompile(`npm_[A-Za-z0-9]{36}`)},
	{"a Hugging Face token", regexp.MustCompile(`hf_[A-Za-z0-9]{30,}`)},
	// The whole block, not the header: a redaction that replaced only the
	// BEGIN line would send the key body it was asked to hold back. The
	// non-greedy body stops at the first END line, and a block whose END
	// was cut off in the paste redacts through to the end of the text,
	// because a truncated key is still a key.
	{"a private key block", regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|\z)`)},
}

// ScanPrompt reports every key-shaped string in outbound content, in
// pattern order, deduplicated on the exact match so a key pasted twice is
// one finding.
func ScanPrompt(text string) []Leak {
	var out []Leak
	seen := map[string]bool{}
	for _, p := range leakPatterns {
		for _, match := range p.re.FindAllString(text, -1) {
			if seen[match] {
				continue
			}
			seen[match] = true
			out = append(out, Leak{Kind: p.kind, match: match})
		}
	}
	return out
}

// SafePrefixForTruncation returns a byte-bounded prefix without cutting a
// recognized credential into an unrecognizable fragment. Complete credentials
// wholly before the boundary stay intact so the caller's ordinary outbound
// ScanPrompt can still offer its redact/send/drop decision. If a credential
// crosses the boundary, the prefix moves back to the start of that match and a
// visible redaction marker replaces the omitted tail. cut is the number of
// original source bytes retained before any marker.
func SafePrefixForTruncation(text string, limit int) (prefix string, cut int) {
	if limit <= 0 {
		return "", 0
	}
	if len(text) <= limit {
		return text, len(text)
	}

	cut = limit
	boundaryKind := ""
	for {
		start, kind := -1, ""
		for _, pattern := range leakPatterns {
			for _, span := range pattern.re.FindAllStringIndex(text, -1) {
				if span[0] < cut && span[1] > cut && (start < 0 || span[0] < start) {
					start, kind = span[0], pattern.kind
				}
			}
		}
		if start < 0 {
			break
		}
		cut, boundaryKind = start, kind
	}

	prefix = text[:cut]
	if boundaryKind != "" {
		prefix += "[redacted: " + boundaryKind + "]"
	}
	return prefix, cut
}

// Redact replaces each finding with a placeholder naming what stood there,
// so the model is told a credential existed rather than left to wonder
// about a gap. An overlap (the OpenAI pattern inside a longer Anthropic
// match) resolves in favor of whichever finding replaces first.
func Redact(text string, leaks []Leak) string {
	for _, l := range leaks {
		text = strings.ReplaceAll(text, l.match, "[redacted: "+l.Kind+"]")
	}
	return text
}
