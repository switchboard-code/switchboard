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
	kind          string
	needle        string
	minMatchBytes int
	re            *regexp.Regexp
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
	{"an Anthropic API key", "sk-ant-", 27, regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`)},
	{"an OpenAI API key", "sk-", 43, regexp.MustCompile(`sk-(?:proj-)?[A-Za-z0-9_-]{40,}`)},
	{"a GitHub token", "gh", 40, regexp.MustCompile(`(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36,}`)},
	{"a GitHub fine-grained token", "github_pat_", 33, regexp.MustCompile(`github_pat_[A-Za-z0-9_]{22,}`)},
	{"a GitLab token", "glpat-", 26, regexp.MustCompile(`glpat-[A-Za-z0-9_-]{20,}`)},
	{"a Slack token", "xox", 15, regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`)},
	{"an AWS access key ID", "AKIA", 20, regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
	{"a Google API key", "AIza", 39, regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`)},
	{"a Stripe live key", "k_live_", 28, regexp.MustCompile(`[sr]k_live_[A-Za-z0-9]{20,}`)},
	{"an npm token", "npm_", 40, regexp.MustCompile(`npm_[A-Za-z0-9]{36}`)},
	{"a Hugging Face token", "hf_", 33, regexp.MustCompile(`hf_[A-Za-z0-9]{30,}`)},
	// The whole block, not the header: a redaction that replaced only the
	// BEGIN line would send the key body it was asked to hold back. The
	// non-greedy body stops at the first END line, and a block whose END
	// was cut off in the paste redacts through to the end of the text,
	// because a truncated key is still a key.
	{"a private key block", "-----BEGIN ", 0, regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|\z)`)},
}

const (
	privateKeyBegin  = "-----BEGIN "
	privateKeySuffix = "PRIVATE KEY-----"
)

type leakScanShape struct {
	firstBytes        [256]bool
	boundaryLookahead int
}

var scanShape = func() leakScanShape {
	var shape leakScanShape
	for _, pattern := range leakPatterns {
		if pattern.needle == "" {
			panic("credential leak pattern has no required needle")
		}
		shape.firstBytes[pattern.needle[0]] = true
		if pattern.minMatchBytes == 0 {
			if pattern.needle != privateKeyBegin {
				panic("credential leak pattern has no boundary length")
			}
			continue
		}
		if lookahead := pattern.minMatchBytes - 1; lookahead > shape.boundaryLookahead {
			shape.boundaryLookahead = lookahead
		}
	}
	return shape
}()

// ScanPrompt reports every key-shaped string in outbound content, in
// pattern order, deduplicated on the exact match so a key pasted twice is
// one finding.
func ScanPrompt(text string) []Leak {
	if !couldContainLeak(text) {
		return nil
	}
	var out []Leak
	var seen map[string]bool
	for _, p := range leakPatterns {
		// Each needle is a literal required by every match of its pattern. The
		// cheap check matters for bounded-but-numerous inputs such as language
		// server diagnostics: regexp misses under the race detector are orders
		// of magnitude more expensive than a literal scan.
		if !strings.Contains(text, p.needle) {
			continue
		}
		for _, match := range p.re.FindAllString(text, -1) {
			if seen == nil {
				seen = make(map[string]bool)
			}
			if seen[match] {
				continue
			}
			seen[match] = true
			out = append(out, Leak{Kind: p.kind, match: match})
		}
	}
	return out
}

// couldContainLeak is a one-pass rejection for text that contains none of the
// possible first bytes of a recognized credential. The table is derived from
// leakPatterns so adding a pattern cannot silently create a false-negative
// fast path. The per-pattern required needles remain the authoritative
// prefilters.
func couldContainLeak(text string) bool {
	for i := 0; i < len(text); i++ {
		if scanShape.firstBytes[text[i]] {
			return true
		}
	}
	return false
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

	// A match beginning one byte before the boundary needs at most its
	// minimum length minus one byte of lookahead to become recognizable. No
	// bytes beyond that window can change whether a non-PEM credential crosses
	// the boundary. The private-key grammar has an unbounded label; an
	// ambiguous label at the edge is handled conservatively below instead of
	// making a diagnostic-sized prefix scan an attacker-sized tail.
	scanEnd := limit + scanShape.boundaryLookahead
	if scanEnd < limit || scanEnd > len(text) {
		scanEnd = len(text)
	}
	scanText := text[:scanEnd]

	type span struct {
		start int
		end   int
		kind  string
	}
	var spans []span
	if couldContainLeak(scanText) {
		for _, pattern := range leakPatterns {
			if !strings.Contains(scanText, pattern.needle) {
				continue
			}
			for _, match := range pattern.re.FindAllStringIndex(scanText, -1) {
				spans = append(spans, span{start: match[0], end: match[1], kind: pattern.kind})
			}
		}
	}

	// [A-Z ]* makes the accepted private-key label intentionally unbounded.
	// If a BEGIN label starts before the cut and is still syntactically capable
	// of becoming a private-key header when the bounded window ends, treating
	// it as a crossing block is the only bounded choice that cannot expose a
	// fragment of a valid key. Ordinary non-key PEM labels disambiguate on
	// their first '-' and are left alone.
	for search := 0; search < len(scanText); {
		relative := strings.Index(scanText[search:], privateKeyBegin)
		if relative < 0 {
			break
		}
		start := search + relative
		if start >= limit {
			break
		}
		position := start + len(privateKeyBegin)
		ambiguous := false
		for {
			remaining := scanText[position:]
			switch {
			case strings.HasPrefix(remaining, privateKeySuffix):
				// The regexp scan above has the complete header and therefore
				// already recorded this block.
			case scanEnd < len(text) && strings.HasPrefix(privateKeySuffix, remaining):
				// The window ended partway through the required suffix. It is
				// still capable of being a complete private-key header.
				ambiguous = true
			default:
				if position < len(scanText) && (scanText[position] == ' ' || scanText[position] >= 'A' && scanText[position] <= 'Z') {
					position++
					continue
				}
			}
			break
		}
		if ambiguous {
			spans = append(spans, span{start: start, end: scanEnd, kind: "a private key block"})
		}
		search = start + 1
	}

	cut = limit
	boundaryKind := ""
	for {
		start, kind := -1, ""
		for _, candidate := range spans {
			if candidate.start < cut && candidate.end > cut && (start < 0 || candidate.start < start) {
				start, kind = candidate.start, candidate.kind
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
