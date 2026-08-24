package skills

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type namedArgument struct {
	name  string
	index int
}

var (
	claudeInlineShell = regexp.MustCompile("(?m)(^|[ \\t])!`[^`\\n]+`")
	claudeFencedShell = regexp.MustCompile("(?m)^[ \\t]*`{3,}!")
)

// Resolve finds one exact canonical selector. It intentionally does not fall
// back to a display name: equal native names are valid, and guessing would
// turn source precedence into an invisible authority decision.
func Resolve(list []Skill, selector string) (Skill, error) {
	var matches []Skill
	for _, sk := range list {
		if sk.Key() == selector {
			matches = append(matches, sk)
		}
	}
	switch len(matches) {
	case 0:
		return Skill{}, fmt.Errorf("no skill with canonical selector %q; use /skills to list selectors", selector)
	case 1:
		return matches[0], nil
	default:
		return Skill{}, fmt.Errorf("canonical selector %q is ambiguous across %d definitions", selector, len(matches))
	}
}

// RenderExplicit returns the static prompt body for a deliberate user
// invocation. It never executes commands, reads dynamic context, changes
// tools/models, or silently drops host controls.
func RenderExplicit(sk Skill, args string) (string, error) {
	if sk.UserInvocationDisabled {
		return "", fmt.Errorf("skill %s sets user-invocable:false", sk.Key())
	}
	blockers := append([]string(nil), sk.InvocationBlockers...)
	if sk.Origin.Ecosystem == EcosystemClaude {
		blockers = append(blockers, claudeBodyBlockers(sk.Body)...)
	}
	blockers = uniqueSorted(blockers)
	if len(blockers) > 0 {
		return "", fmt.Errorf("skill %s cannot be invoked safely: %s", sk.Key(), strings.Join(blockers, "; "))
	}
	// Invocation checks use the source body above. Redact only the value that
	// will be rendered into a prompt so callers retain an unchanged inventory
	// and a repository-authored credential can never rely on an interactive
	// prompt gate to keep it from the provider.
	body := redactSkillEgress(sk.Body)
	if sk.Origin.Ecosystem == EcosystemClaude {
		return renderClaudeArguments(body, args, sk.ArgumentNames)
	}
	if strings.TrimSpace(args) != "" {
		body += "\n\nARGUMENTS: " + args
	}
	return body, nil
}

func claudeBodyBlockers(body string) []string {
	var blockers []string
	if claudeInlineShell.MatchString(body) || claudeFencedShell.MatchString(body) {
		blockers = append(blockers, "unsupported Claude shell injection")
	}
	if strings.Contains(body, "${CLAUDE_") {
		blockers = append(blockers, "unsupported Claude dynamic context substitution")
	}
	if hasClaudeAttachment(body) {
		blockers = append(blockers, "unsupported Claude @ file attachment")
	}
	return uniqueSorted(blockers)
}

func hasClaudeAttachment(body string) bool {
	for i := 0; i < len(body); i++ {
		if body[i] != '@' || i+1 == len(body) {
			continue
		}
		next, _ := utf8.DecodeRuneInString(body[i+1:])
		if unicode.IsSpace(next) {
			continue
		}
		if i == 0 {
			return true
		}
		previous, _ := utf8.DecodeLastRuneInString(body[:i])
		// Keep ordinary email addresses as prose. Claude file mentions may be
		// wrapped in Markdown punctuation (for example **@path**), so every
		// non-identifier boundary is conservatively attachment syntax.
		if unicode.IsSpace(previous) ||
			(!unicode.IsLetter(previous) && !unicode.IsNumber(previous) && !strings.ContainsRune("_.+%", previous)) {
			return true
		}
	}
	return false
}

func renderClaudeArguments(body, raw string, names []string) (string, error) {
	args, err := splitInvocationArguments(raw)
	if err != nil {
		return "", fmt.Errorf("invalid skill arguments: %w", err)
	}
	named := make([]namedArgument, len(names))
	for i, name := range names {
		named[i] = namedArgument{name: name, index: i}
	}
	sort.Slice(named, func(i, j int) bool {
		if len(named[i].name) == len(named[j].name) {
			return named[i].name < named[j].name
		}
		return len(named[i].name) > len(named[j].name)
	})

	var out strings.Builder
	used := false
	for i := 0; i < len(body); {
		if body[i] == '\\' {
			start := i
			for i < len(body) && body[i] == '\\' {
				i++
			}
			count := i - start
			if i < len(body) && body[i] == '$' {
				consumed, replacement, recognized := claudePlaceholder(body, i, raw, args, named)
				if recognized {
					if count == 1 {
						out.WriteString(body[i : i+consumed])
					} else {
						out.WriteString(body[start:i])
						out.WriteString(replacement)
						used = true
					}
					i += consumed
					continue
				}
			}
			out.WriteString(body[start:i])
			continue
		}
		if body[i] == '$' {
			consumed, replacement, recognized := claudePlaceholder(body, i, raw, args, named)
			if recognized {
				out.WriteString(replacement)
				used = true
				i += consumed
				continue
			}
		}
		out.WriteByte(body[i])
		i++
	}
	if strings.TrimSpace(raw) != "" && !used {
		out.WriteString("\n\nARGUMENTS: ")
		out.WriteString(raw)
	}
	return out.String(), nil
}

func claudePlaceholder(body string, start int, raw string, args []string, names []namedArgument) (int, string, bool) {
	const all = "$ARGUMENTS"
	if strings.HasPrefix(body[start:], all+"[") {
		indexStart := start + len(all) + 1
		end := indexStart
		for end < len(body) && body[end] >= '0' && body[end] <= '9' {
			end++
		}
		if end > indexStart && end < len(body) && body[end] == ']' {
			index, _ := strconv.Atoi(body[indexStart:end])
			tokenEnd := end + 1
			if index < len(args) {
				return tokenEnd - start, args[index], true
			}
			return tokenEnd - start, body[start:tokenEnd], true
		}
	}
	if strings.HasPrefix(body[start:], all) {
		end := start + len(all)
		if end == len(body) || !argumentNameByte(body[end]) {
			return len(all), raw, true
		}
	}
	if start+1 < len(body) && body[start+1] >= '0' && body[start+1] <= '9' {
		end := start + 2
		for end < len(body) && body[end] >= '0' && body[end] <= '9' {
			end++
		}
		index, _ := strconv.Atoi(body[start+1 : end])
		if index < len(args) {
			return end - start, args[index], true
		}
		return end - start, body[start:end], true
	}
	for _, named := range names {
		token := "$" + named.name
		if !strings.HasPrefix(body[start:], token) {
			continue
		}
		end := start + len(token)
		if end < len(body) && argumentNameByte(body[end]) {
			continue
		}
		if named.index < len(args) {
			return len(token), args[named.index], true
		}
		return len(token), "", true
	}
	return 0, "", false
}

func argumentNameByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-'
}

func splitInvocationArguments(raw string) ([]string, error) {
	var args []string
	var current strings.Builder
	quote := rune(0)
	escaped := false
	started := false
	flush := func() {
		if started {
			args = append(args, current.String())
			current.Reset()
			started = false
		}
	}
	for _, r := range raw {
		if escaped {
			current.WriteRune(r)
			started = true
			escaped = false
			continue
		}
		if quote != '\'' && r == '\\' {
			escaped = true
			started = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
				started = true
			} else {
				current.WriteRune(r)
				started = true
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			started = true
			continue
		}
		if unicode.IsSpace(r) {
			flush()
			continue
		}
		current.WriteRune(r)
		started = true
	}
	if escaped {
		return nil, fmt.Errorf("trailing escape")
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote")
	}
	flush()
	return args, nil
}
