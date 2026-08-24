package skills

import (
	"net/url"
	"strings"

	"github.com/switchboard-code/switchboard/internal/credential"
)

// redactSkillEgress removes recognized credentials from prompt material that
// is about to enter a provider request or tool result. Discovery deliberately
// retains the source bytes and Skill values unchanged: native invocation
// controls and explicit user inventory must continue to describe what was on
// disk, while no repository-authored credential may ride those values out to
// a model.
func redactSkillEgress(text string) string {
	return credential.Redact(text, credential.ScanPrompt(text))
}

// redactSkillSelector also scans each selector component's semantic spelling.
// Canonical selectors URL-escape path segments; an encoded space immediately
// before a credential turns its raw predecessor into the word character in
// "%20" and can erase the scanner's intentional boundary. Literal ':' and '/'
// are the selector's structural delimiters, while embedded delimiters and
// controls are escaped inside a component. Re-escaping a changed component
// preserves that grammar and prevents semantic newlines or slashes from
// becoming provider-metadata syntax.
func redactSkillSelector(selector string) string {
	var out strings.Builder
	out.Grow(len(selector))
	start := 0
	for i := 0; i <= len(selector); i++ {
		if i < len(selector) && selector[i] != ':' && selector[i] != '/' {
			continue
		}
		out.WriteString(redactSkillSelectorComponent(selector[start:i]))
		if i < len(selector) {
			out.WriteByte(selector[i])
		}
		start = i + 1
	}
	return out.String()
}

func redactSkillSelectorComponent(encoded string) string {
	decoded, err := url.PathUnescape(encoded)
	if err != nil {
		// Discovery produces valid escapes. A programmatic malformed selector is
		// left structurally unchanged unless its raw component itself contains a
		// recognizable credential; in that case escape the redacted replacement.
		if redacted := redactSkillEgress(encoded); redacted != encoded {
			return escapeSelectorSegment(redacted)
		}
		return encoded
	}
	if redacted := redactSkillEgress(decoded); redacted != decoded {
		return escapeSelectorSegment(redacted)
	}
	return encoded
}
