package skills

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

type skillMetadata struct {
	name                   string
	description            string
	whenToUse              string
	argumentHint           string
	argumentNames          []string
	disableModelInvocation bool
	userInvocable          bool
	modelBlockers          []string
	invocationBlockers     []string

	// notes record a native control this build does not apply but which is
	// safe to leave unapplied. Recorded rather than dropped, because the rule
	// here is that a host control is never silently discarded.
	notes []string
}

type documentParseOptions struct {
	deriveDescription bool
	ignoreName        bool
	ignorePaths       bool
}

type invocationPolicyError struct {
	field string
	err   error
}

func (e *invocationPolicyError) Error() string {
	if strings.HasPrefix(e.err.Error(), "invalid "+e.field+":") {
		return e.err.Error()
	}
	return fmt.Sprintf("invalid %s: %v", e.field, e.err)
}

func (e *invocationPolicyError) Unwrap() error { return e.err }

// parseDocument reads the portable part of SKILL.md frontmatter and returns
// whether the definition is manual-only. There is no YAML package in the
// dependency graph, so this parser intentionally handles only the scalar and
// block-scalar forms used by skill metadata. Unknown keys remain portable. A
// malformed invocation-control value is an error so it can never fail open.
func parseDocument(fallbackName, content string) (Skill, bool, error) {
	return parseDocumentWithOptions(fallbackName, content, false)
}

func parseDocumentForEcosystem(fallbackName, content string, ecosystem Ecosystem) (Skill, bool, error) {
	return parseDocumentWithOptions(fallbackName, content, ecosystem == EcosystemClaude)
}

func parseDocumentWithOptions(fallbackName, content string, deriveDescription bool) (Skill, bool, error) {
	return parseDocumentWithParseOptions(fallbackName, content, documentParseOptions{deriveDescription: deriveDescription})
}

func parseClaudeCommandDocument(fallbackName, content string) (Skill, bool, error) {
	return parseDocumentWithParseOptions(fallbackName, content, documentParseOptions{
		deriveDescription: true,
		ignoreName:        true,
		ignorePaths:       true,
	})
}

func parseDocumentWithParseOptions(fallbackName, content string, options documentParseOptions) (Skill, bool, error) {
	content = normalizeYAMLText(content)
	front, body, hasFront := splitFrontmatter(content)
	if !hasFront && hasFrontmatterOpening(content) {
		err := fmt.Errorf("unterminated YAML frontmatter")
		if containsUnquotedYAMLField(content, "disable-model-invocation") {
			return Skill{}, false, &invocationPolicyError{field: "disable-model-invocation", err: err}
		}
		if containsUnquotedYAMLField(content, "user-invocable") {
			return Skill{}, false, &invocationPolicyError{field: "user-invocable", err: err}
		}
		return Skill{}, false, err
	}
	meta := skillMetadata{name: fallbackName, userInvocable: true}
	if hasFront {
		if hasDynamicYAMLKey(front) {
			return Skill{}, false, &invocationPolicyError{
				field: "disable-model-invocation",
				err:   fmt.Errorf("dynamic YAML keys cannot be checked safely"),
			}
		}
		var err error
		meta, err = parseSkillMetadataWithOptions(fallbackName, front, options)
		if err != nil {
			return Skill{}, false, err
		}
		// A native control hidden in a merge source or nested mapping is still
		// behavior-bearing YAML. The scalar parser cannot resolve merges, so
		// conservatively retain the skill as blocked instead of silently
		// discarding a tool, model, or execution constraint.
		for _, key := range blockingControls {
			if yamlHasField(front, key) {
				meta.invocationBlockers = append(meta.invocationBlockers, fmt.Sprintf("unsupported control %q", key))
			}
		}
		for _, key := range grantingControls {
			if yamlHasField(front, key) {
				meta.notes = append(meta.notes, fmt.Sprintf("%s is not applied; its tools are asked for instead", key))
			}
		}
		if !options.ignorePaths && yamlHasField(front, "paths") {
			meta.modelBlockers = append(meta.modelBlockers, "unsupported paths activation control")
		}
		if yamlHasField(front, "arguments") && !yamlHasTopLevelField(front, "arguments") {
			meta.invocationBlockers = append(meta.invocationBlockers, "unsupported merged or nested arguments metadata")
		}
		values, err := yamlBoolFieldValues(front, "disable-model-invocation")
		if err != nil {
			return Skill{}, false, &invocationPolicyError{field: "disable-model-invocation", err: err}
		}
		// Scan every YAML nesting level conservatively. This catches aliases
		// and merge-key sources that a small scalar parser cannot resolve; a
		// nested true may suppress an otherwise safe skill, but can never let
		// a manual-only skill become model-visible.
		for _, value := range values {
			meta.disableModelInvocation = meta.disableModelInvocation || value
		}
		values, err = yamlBoolFieldValues(front, "user-invocable")
		if err != nil {
			return Skill{}, false, &invocationPolicyError{field: "user-invocable", err: err}
		}
		for _, value := range values {
			meta.userInvocable = meta.userInvocable && value
		}
	}

	sk := Skill{
		Name:                   meta.name,
		Description:            meta.description,
		Body:                   strings.TrimSpace(body),
		ImplicitDisabled:       meta.disableModelInvocation,
		UserInvocationDisabled: !meta.userInvocable,
		ArgumentHint:           meta.argumentHint,
		ArgumentNames:          append([]string(nil), meta.argumentNames...),
		ModelBlockers:          uniqueSorted(meta.modelBlockers),
		InvocationBlockers:     uniqueSorted(meta.invocationBlockers),
		Notes:                  uniqueSorted(meta.notes),
	}
	if strings.TrimSpace(sk.Name) == "" || strings.ContainsAny(sk.Name, "\r\n") {
		return Skill{}, false, fmt.Errorf("has an invalid name; names must be non-empty and stay on one line")
	}
	if sk.Body == "" {
		return Skill{}, false, fmt.Errorf("has no body; the body is the skill's instructions")
	}
	if options.deriveDescription && strings.TrimSpace(sk.Description) == "" {
		sk.Description = firstBodyParagraph(sk.Body)
	}
	if meta.whenToUse != "" {
		sk.Description = strings.TrimSpace(sk.Description + " " + meta.whenToUse)
	}
	if strings.TrimSpace(sk.Description) == "" {
		return Skill{}, false, fmt.Errorf("has no description; the description is how the model decides when to use it")
	}
	return sk, meta.disableModelInvocation, nil
}

func hasFrontmatterOpening(content string) bool {
	line, _, _ := strings.Cut(content, "\n")
	return leadingSpaces(line) == 0 && strings.TrimSpace(trimYAMLComment(line)) == "---"
}

func firstBodyParagraph(body string) string {
	var paragraph []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			if len(paragraph) > 0 {
				break
			}
			continue
		}
		paragraph = append(paragraph, line)
	}
	return strings.Join(paragraph, " ")
}

func normalizeYAMLText(s string) string {
	s = strings.TrimPrefix(s, "\ufeff")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

func splitFrontmatter(content string) (front, body string, ok bool) {
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || leadingSpaces(lines[0]) != 0 || strings.TrimSpace(trimYAMLComment(lines[0])) != "---" {
		return "", content, false
	}
	for i := 1; i < len(lines); i++ {
		marker := strings.TrimSpace(trimYAMLComment(lines[i]))
		if leadingSpaces(lines[i]) == 0 && (marker == "---" || marker == "...") {
			return strings.Join(lines[1:i], "\n"), strings.Join(lines[i+1:], "\n"), true
		}
	}
	return "", content, false
}

func parseSkillMetadata(fallbackName, front string) (skillMetadata, error) {
	return parseSkillMetadataWithOptions(fallbackName, front, documentParseOptions{})
}

func parseSkillMetadataWithOptions(fallbackName, front string, options documentParseOptions) (skillMetadata, error) {
	meta := skillMetadata{name: fallbackName, userInvocable: true}
	lines := strings.Split(front, "\n")
	for i := 0; i < len(lines); i++ {
		line := trimYAMLComment(lines[i])
		if strings.TrimSpace(line) == "" || leadingSpaces(line) != 0 {
			continue
		}
		keyRaw, valueRaw, ok := cutYAMLPair(line)
		if !ok {
			continue
		}
		key, err := parseYAMLScalar(keyRaw)
		if err != nil {
			continue // an unknown, non-scalar YAML key is irrelevant here
		}
		if key == "name" && options.ignoreName {
			continue
		}
		switch key {
		case "name", "description", "when_to_use", "argument-hint":
			var value string
			if isBlockIndicator(valueRaw) {
				value, i, err = parseBlockScalar(lines, i+1, valueRaw)
			} else {
				value, err = parseYAMLScalar(valueRaw)
			}
			if err != nil {
				return skillMetadata{}, fmt.Errorf("invalid %s: %w", key, err)
			}
			if key == "name" && value != "" {
				meta.name = value
			}
			if key == "description" {
				meta.description = value
			}
			if key == "when_to_use" {
				meta.whenToUse = value
			}
			if key == "argument-hint" {
				meta.argumentHint = value
			}
		case "arguments":
			var names []string
			names, i, err = parseYAMLStringList(lines, i, valueRaw)
			if err != nil {
				meta.invocationBlockers = append(meta.invocationBlockers, "invalid arguments metadata: "+err.Error())
				continue
			}
			seenNames := make(map[string]bool, len(names))
			for _, name := range names {
				if !validArgumentName(name) {
					meta.invocationBlockers = append(meta.invocationBlockers, fmt.Sprintf("invalid named argument %q", name))
					continue
				}
				if seenNames[name] {
					meta.invocationBlockers = append(meta.invocationBlockers, fmt.Sprintf("duplicate named argument %q", name))
					continue
				}
				seenNames[name] = true
				meta.argumentNames = append(meta.argumentNames, name)
			}
		case "disable-model-invocation":
			value, err := parseYAMLBool(valueRaw)
			if err != nil {
				return skillMetadata{}, &invocationPolicyError{field: "disable-model-invocation", err: err}
			}
			// Duplicate safety controls fail closed: one true value cannot be
			// cancelled by a later false value.
			meta.disableModelInvocation = meta.disableModelInvocation || value
		case "user-invocable":
			value, err := parseYAMLBool(valueRaw)
			if err != nil {
				return skillMetadata{}, &invocationPolicyError{field: "user-invocable", err: err}
			}
			meta.userInvocable = meta.userInvocable && value
		case "paths":
			if options.ignorePaths {
				continue
			}
			// Path filters alter automatic activation. Switchboard has no
			// per-file activation context, so explicit invocation stays safe
			// while model advertisement fails closed.
			meta.modelBlockers = append(meta.modelBlockers, "unsupported paths activation control")
		case "allowed-tools":
			// A permission grant, not a restriction: it pre-approves tools so
			// the skill's turn is not interrupted. Not applying it can only
			// ask more often than the author intended, never less, so it is
			// the one native control whose absence is already the safe state
			// and the skill stays usable.
			meta.notes = append(meta.notes, key+" is not applied; its tools are asked for instead")
		case "disallowed-tools", "model", "effort", "context", "agent", "background", "hooks", "shell":
			meta.invocationBlockers = append(meta.invocationBlockers, fmt.Sprintf("unsupported control %q", key))
		}
	}
	return meta, nil
}

// blockingControls are native controls whose absence changes behavior in a
// direction this build cannot vouch for: a restriction it would not enforce,
// or an execution shape it does not have.
var blockingControls = []string{"disallowed-tools", "model", "effort", "context", "agent", "background", "hooks", "shell"}

// grantingControls only widen what a skill may do without being asked. Not
// applying one is the conservative direction, so it costs a note rather than
// the skill.
var grantingControls = []string{"allowed-tools"}

func parseYAMLStringList(lines []string, current int, raw string) ([]string, int, error) {
	raw = strings.TrimSpace(trimYAMLComment(raw))
	if raw == "" {
		var out []string
		i := current + 1
		for ; i < len(lines); i++ {
			line := trimYAMLComment(lines[i])
			if strings.TrimSpace(line) == "" {
				continue
			}
			if leadingSpaces(line) == 0 {
				break
			}
			item := strings.TrimSpace(line)
			if !strings.HasPrefix(item, "-") || (len(item) > 1 && item[1] != ' ' && item[1] != '\t') {
				return nil, i - 1, fmt.Errorf("want a string or YAML string list")
			}
			value, err := parseYAMLScalar(strings.TrimSpace(strings.TrimPrefix(item, "-")))
			if err != nil || value == "" {
				if err == nil {
					err = fmt.Errorf("empty list item")
				}
				return nil, i, err
			}
			out = append(out, value)
		}
		return out, i - 1, nil
	}
	if strings.HasPrefix(raw, "[") {
		if !strings.HasSuffix(raw, "]") {
			return nil, current, fmt.Errorf("unterminated inline list")
		}
		inside := strings.TrimSpace(raw[1 : len(raw)-1])
		if inside == "" {
			return nil, current, nil
		}
		var out []string
		for _, field := range splitInlineFields(inside) {
			value, err := parseYAMLScalar(field)
			if err != nil || value == "" {
				if err == nil {
					err = fmt.Errorf("empty list item")
				}
				return nil, current, err
			}
			out = append(out, value)
		}
		return out, current, nil
	}
	value, err := parseYAMLScalar(raw)
	if err != nil {
		return nil, current, err
	}
	return strings.Fields(value), current, nil
}

func validArgumentName(name string) bool {
	if name == "" || !((name[0] >= 'a' && name[0] <= 'z') || (name[0] >= 'A' && name[0] <= 'Z') || name[0] == '_') {
		return false
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

func isBlockIndicator(raw string) bool {
	raw = strings.TrimSpace(trimYAMLComment(raw))
	if raw == "" || (raw[0] != '|' && raw[0] != '>') {
		return false
	}
	seenChomp, seenIndent := false, false
	for _, c := range raw[1:] {
		switch {
		case (c == '-' || c == '+') && !seenChomp:
			seenChomp = true
		case c >= '1' && c <= '9' && !seenIndent:
			seenIndent = true
		default:
			return false
		}
	}
	return true
}

func parseBlockScalar(lines []string, start int, indicator string) (string, int, error) {
	end := start
	minIndent := -1
	for end < len(lines) {
		line := lines[end]
		if strings.TrimSpace(line) != "" {
			indent := leadingSpaces(line)
			if indent == 0 {
				break
			}
			if minIndent < 0 || indent < minIndent {
				minIndent = indent
			}
		}
		end++
	}
	if minIndent < 0 {
		return "", end - 1, nil
	}

	block := make([]string, 0, end-start)
	for _, line := range lines[start:end] {
		if strings.TrimSpace(line) == "" {
			block = append(block, "")
			continue
		}
		if leadingSpaces(line) < minIndent {
			return "", end - 1, fmt.Errorf("inconsistent indentation")
		}
		block = append(block, line[minIndent:])
	}

	var value string
	if strings.HasPrefix(strings.TrimSpace(indicator), ">") {
		value = foldYAMLLines(block)
	} else {
		value = strings.Join(block, "\n")
	}
	return strings.TrimSpace(value), end - 1, nil
}

func foldYAMLLines(lines []string) string {
	var b strings.Builder
	previousBlank := true
	for _, line := range lines {
		blank := strings.TrimSpace(line) == ""
		if blank {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			previousBlank = true
			continue
		}
		if b.Len() > 0 && !previousBlank {
			b.WriteByte(' ')
		}
		b.WriteString(line)
		previousBlank = false
	}
	return b.String()
}

func leadingSpaces(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] != ' ' {
			return i
		}
	}
	return len(s)
}

func cutYAMLPair(line string) (key, value string, ok bool) {
	single, double, escaped := false, false, false
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case escaped:
			escaped = false
		case double && c == '\\':
			escaped = true
		case !double && c == '\'':
			single = !single
		case !single && c == '"':
			double = !double
		case !single && !double && c == ':':
			return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:]), true
		}
	}
	return "", "", false
}

func trimYAMLComment(s string) string {
	single, double, escaped := false, false, false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case escaped:
			escaped = false
		case double && c == '\\':
			escaped = true
		case !double && c == '\'':
			single = !single
		case !single && c == '"':
			double = !double
		case !single && !double && c == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t'):
			return strings.TrimRight(s[:i], " \t")
		}
	}
	return strings.TrimRight(s, " \t")
}

func parseYAMLScalar(raw string) (string, error) {
	raw = strings.TrimSpace(trimYAMLComment(raw))
	if raw == "" || raw == "~" || strings.EqualFold(raw, "null") {
		return "", nil
	}
	if raw[0] == '\'' {
		if len(raw) < 2 || raw[len(raw)-1] != '\'' {
			return "", fmt.Errorf("unterminated single-quoted scalar")
		}
		return strings.ReplaceAll(raw[1:len(raw)-1], "''", "'"), nil
	}
	if raw[0] == '"' {
		value, err := strconv.Unquote(raw)
		if err != nil {
			return "", err
		}
		return value, nil
	}
	return raw, nil
}

func parseYAMLBool(raw string) (bool, error) {
	value, err := parseYAMLScalar(raw)
	if err != nil {
		return false, err
	}
	switch strings.ToLower(value) {
	case "true", "yes", "on", "1":
		return true, nil
	case "false", "no", "off", "0":
		return false, nil
	default:
		return false, fmt.Errorf("want a boolean, got %q", value)
	}
}

// hasDynamicYAMLKey identifies YAML key forms whose resolved scalar cannot be
// determined by this package's deliberately small metadata parser. Aliases,
// tags, anchors, and explicit complex keys can all resolve to the invocation
// control name without spelling it at the mapping site. Rejecting such a
// document is conservative but prevents a valid YAML representation from
// bypassing a model-invocation opt-out.
func hasDynamicYAMLKey(front string) bool {
	for _, rawLine := range strings.Split(front, "\n") {
		line := strings.TrimSpace(trimYAMLComment(rawLine))
		if line == "" {
			continue
		}
		if line == "?" || strings.HasPrefix(line, "? ") || strings.HasPrefix(line, "?\t") {
			return true
		}
		keyRaw, _, ok := cutYAMLPair(line)
		if !ok {
			continue
		}
		keyRaw = strings.TrimSpace(keyRaw)
		if keyRaw == "" {
			continue
		}
		switch keyRaw[0] {
		case '*', '&', '!':
			return true
		}
		if open, close := strings.IndexByte(line, '{'), strings.LastIndexByte(line, '}'); open >= 0 && close > open {
			for _, field := range splitInlineFields(line[open+1 : close]) {
				keyRaw, _, ok := cutYAMLPair(field)
				if !ok {
					continue
				}
				keyRaw = strings.TrimSpace(keyRaw)
				if keyRaw != "" && strings.ContainsRune("*&!", rune(keyRaw[0])) {
					return true
				}
			}
		}
	}
	return false
}

// codexAllowsImplicit reads the optional native Codex metadata beside a
// directory-shaped skill. Missing metadata means the documented default,
// true. Present but malformed safety metadata fails closed.
func codexAllowsImplicit(skillDir string) (bool, error) {
	root, err := rootedfs.OpenRoot(skillDir)
	if err != nil {
		return false, err
	}
	defer root.Close()
	allowed, _, err := codexInvocationMetadataFromRoot(root)
	return allowed, err
}

func codexAllowsImplicitFromRoot(root *os.Root) (bool, error) {
	allowed, _, err := codexInvocationMetadataFromRoot(root)
	return allowed, err
}

func codexInvocationMetadataFromRoot(root *os.Root) (bool, []string, error) {
	const path = "agents/openai.yaml"
	data, err := readFileFromRoot(root, path, maxDefinitionBytes)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil, nil
		}
		return false, nil, &invocationPolicyError{
			field: "allow_implicit_invocation",
			err:   fmt.Errorf("unsafe agents/openai.yaml: %w", err),
		}
	}
	content := string(data)
	value, found, err := findYAMLBoolField(content, "allow_implicit_invocation")
	if err != nil {
		return false, nil, &invocationPolicyError{field: "allow_implicit_invocation", err: err}
	}
	var blockers []string
	if yamlHasField(content, "dependencies") {
		blockers = append(blockers, "unsupported Codex tool dependencies")
	}
	// interface.default_prompt is UI affordance metadata: Codex may offer it
	// when a person deliberately invokes the skill, but it is not part of the
	// skill's model-visible instructions or an invocation constraint. Ignoring
	// that suggestion is therefore lossless for automatic discovery. The body
	// remains the sole prompt Switchboard loads.
	if !found {
		return true, blockers, nil
	}
	return value, blockers, nil
}

func yamlHasTopLevelField(content, target string) bool {
	for _, rawLine := range strings.Split(normalizeYAMLText(content), "\n") {
		line := trimYAMLComment(rawLine)
		if strings.TrimSpace(line) == "" || leadingSpaces(line) != 0 {
			continue
		}
		keyRaw, _, ok := cutYAMLPair(line)
		if !ok {
			continue
		}
		key, err := parseYAMLScalar(keyRaw)
		if err == nil && key == target {
			return true
		}
	}
	return false
}

func yamlHasField(content, target string) bool {
	for _, rawLine := range strings.Split(normalizeYAMLText(content), "\n") {
		line := trimYAMLComment(rawLine)
		if strings.TrimSpace(line) == "" {
			continue
		}
		keyRaw, _, ok := cutYAMLPair(line)
		if ok {
			key, err := parseYAMLScalar(keyRaw)
			if err == nil && key == target {
				return true
			}
		}
		if open, close := strings.IndexByte(line, '{'), strings.LastIndexByte(line, '}'); open >= 0 && close > open {
			for _, field := range splitInlineFields(line[open+1 : close]) {
				keyRaw, _, ok := cutYAMLPair(field)
				if !ok {
					continue
				}
				key, err := parseYAMLScalar(keyRaw)
				if err == nil && key == target {
					return true
				}
			}
		}
	}
	return false
}

func findYAMLBoolField(content, target string) (value, found bool, err error) {
	if hasDynamicYAMLKey(normalizeYAMLText(content)) {
		return false, false, fmt.Errorf("dynamic YAML keys cannot be checked safely")
	}
	values, err := yamlBoolFieldValues(content, target)
	if err != nil {
		return false, false, err
	}
	if len(values) == 0 {
		return false, false, nil
	}
	// allow_implicit_invocation is an allow flag, so one opt-out wins a
	// conflicting duplicate or merge source.
	for _, value := range values {
		if !value {
			return false, true, nil
		}
	}
	return true, true, nil
}

func yamlBoolFieldValues(content, target string) ([]bool, error) {
	content = normalizeYAMLText(content)
	var values []bool
	for _, rawLine := range strings.Split(content, "\n") {
		line := trimYAMLComment(rawLine)
		if line == "" {
			continue
		}
		handled := false
		keyRaw, valueRaw, ok := cutYAMLPair(line)
		if ok {
			key, keyErr := parseYAMLScalar(keyRaw)
			if keyErr == nil && key == target {
				parsed, parseErr := parseYAMLBool(strings.TrimRight(valueRaw, ",}"))
				if parseErr != nil {
					return nil, fmt.Errorf("invalid %s: %w", target, parseErr)
				}
				values = append(values, parsed)
				handled = true
				continue
			}
		}

		// The official example uses a nested block, but accepting a small
		// inline mapping keeps the safety control fail-closed across common
		// YAML formatting without pretending to be a general YAML decoder.
		if open, close := strings.IndexByte(line, '{'), strings.LastIndexByte(line, '}'); open >= 0 && close > open {
			for _, field := range splitInlineFields(line[open+1 : close]) {
				keyRaw, valueRaw, ok := cutYAMLPair(field)
				if !ok {
					continue
				}
				key, keyErr := parseYAMLScalar(keyRaw)
				if keyErr != nil || key != target {
					continue
				}
				parsed, parseErr := parseYAMLBool(valueRaw)
				if parseErr != nil {
					return nil, fmt.Errorf("invalid %s: %w", target, parseErr)
				}
				values = append(values, parsed)
				handled = true
			}
		}

		if containsUnquotedYAMLField(line, target) && !handled {
			return nil, fmt.Errorf("could not safely parse %s", target)
		}
	}
	return values, nil
}

func containsUnquotedYAMLField(line, target string) bool {
	single, double, escaped := false, false, false
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case escaped:
			escaped = false
		case double && c == '\\':
			escaped = true
		case !double && c == '\'':
			single = !single
		case !single && c == '"':
			double = !double
		case !single && !double && strings.HasPrefix(line[i:], target):
			before := strings.TrimSpace(line[:i])
			after := strings.TrimSpace(line[i+len(target):])
			if strings.HasPrefix(after, ":") || before == "" || before == "?" {
				return true
			}
			i += len(target) - 1
		}
	}
	return false
}

func splitInlineFields(s string) []string {
	var fields []string
	start := 0
	single, double, escaped := false, false, false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case escaped:
			escaped = false
		case double && c == '\\':
			escaped = true
		case !double && c == '\'':
			single = !single
		case !single && c == '"':
			double = !double
		case !single && !double && c == ',':
			fields = append(fields, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	return append(fields, strings.TrimSpace(s[start:]))
}
