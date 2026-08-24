package delegate

// Named subagent definitions (§13): a markdown file per agent, frontmatter
// naming it, the body its standing instructions. .switchboard/agents/ in the
// repository and ~/.switchboard/agents/ globally, project winning a name
// clash, the same shape custom commands use — a definition is a prompt plus
// two defaults (a rung, a tool grant), so it earns the same trust posture:
// readable without a grant, because nothing executes at read time, and every
// call the agent then makes passes the shared permission engine on its own
// merits.
//
// Discovery is once, at session assembly. The definitions surface in the
// delegate tool's schema, which sits in the frozen zone (§6.1), so a file
// added mid-process is picked up by the next Switchboard run, not this one.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

const (
	maxAgentDefinitionBytes  = int64(1 << 20)
	maxAgentAggregateBytes   = int64(8 << 20)
	maxAgentDefinitions      = 256
	maxAgentDirectoryEntries = 1024
	maxAgentDirectoryDepth   = 8
	maxAgentNameBytes        = 128
	maxAgentDescriptionBytes = 4096
	maxAgentTierBytes        = 128
	maxAgentTools            = 64
)

// AgentDialect and AgentScope preserve the authority that supplied a named
// agent. A short name is not enough provenance when Switchboard also adapts a
// neighboring client's definition format.
type AgentDialect string
type AgentScope string

const (
	AgentDialectSwitchboard AgentDialect = "switchboard"
	AgentDialectClaude      AgentDialect = "claude"
	AgentScopeWorkspace     AgentScope   = "workspace"
	AgentScopeUser          AgentScope   = "user"
)

// AgentOrigin records both the path the loader inspected and the resolved
// regular file it actually read. LogicalPath is for diagnostics; Path is the
// stable provenance identity.
type AgentOrigin struct {
	Dialect     AgentDialect
	Scope       AgentScope
	LogicalPath string
	Path        string
}

// Agent is one loaded definition.
type Agent struct {
	Name        string
	Description string

	// Tier is the default rung, applied when a call names the agent but no
	// tier. Empty falls to the ladder's bottom, same as a bare delegate.
	Tier string

	// Tools narrows the subagent's registry to these names. Empty means the
	// full core suite. The grant can only narrow — the sub-registry never
	// held delegate or the bridged MCP tools to begin with.
	Tools []string

	// ToolsSet distinguishes an omitted grant from one written explicitly.
	// Switchboard's own format defines either empty shape as the full core
	// suite. A native Claude definition with an explicitly empty grant is
	// refused at discovery because Claude treats omission as inheritance and
	// the runtime cannot currently preserve a zero-tool native agent.
	ToolsSet bool

	// Prompt is the body: standing instructions appended to the subagent's
	// system blocks after the delegate preamble.
	Prompt string

	// FromHome preserves the original user-versus-workspace signal. Origin is
	// the authoritative provenance for new code.
	FromHome bool

	Origin AgentOrigin
}

// SourceLabel returns the provenance /agents should show. The fallback keeps
// programmatically constructed Agents readable without inventing an origin.
func (a Agent) SourceLabel(workspace string) string {
	if a.Origin.LogicalPath == "" {
		if a.FromHome {
			return "switchboard ~/.switchboard/agents"
		}
		return "switchboard .switchboard/agents"
	}
	path := a.Origin.LogicalPath
	base := workspace
	if absolute, err := filepath.Abs(base); err == nil {
		base = absolute
	}
	prefix := ""
	if a.Origin.Scope == AgentScopeUser {
		if home, err := os.UserHomeDir(); err == nil {
			base = home
			prefix = "~/"
		}
	}
	if rel, err := filepath.Rel(base, path); err == nil && rel != ".." &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
		path = prefix + filepath.ToSlash(rel)
	}
	dialect := a.Origin.Dialect
	if dialect == "" {
		dialect = AgentDialectSwitchboard
	}
	return sanitizeDefinitionDiagnostic(string(dialect) + " " + path)
}

// LoadAgents reads the bounded definition inventory once. validTools is the
// suite a subagent can actually hold; a definition granting anything else is
// skipped with a note rather than loaded broken. Workspace scope precedes user
// scope, but malformed higher-precedence files still reserve every identity we
// can safely recover from them. That prevents a rejected safety control from
// silently activating an older definition with the same name.
func LoadAgents(workspace string, validTools []string) (agents []Agent, notes []string) {
	valid := map[string]bool{}
	for _, name := range validTools {
		valid[name] = true
	}

	sources := agentSources(workspace)
	groups := map[AgentScope][]agentCandidate{}
	limits := agentLoadLimits{}
	for _, src := range sources {
		candidates, sourceNotes, fatal := loadAgentSource(src, &limits)
		notes = append(notes, sourceNotes...)
		if fatal {
			return nil, sanitizeAgentNotes(notes)
		}
		groups[src.scope] = append(groups[src.scope], candidates...)
	}

	type precedenceClaim struct {
		paths    []string
		rejected bool
	}
	higher := map[string]precedenceClaim{}
	for _, scope := range []AgentScope{AgentScopeWorkspace, AgentScopeUser} {
		candidates := groups[scope]
		claims := make(map[string][]int)
		for i := range candidates {
			candidate := &candidates[i]
			if candidate.err == nil {
				ag, err := parseAgentDefinition(candidate.filename, string(candidate.data), valid, candidate.source.format)
				if err != nil {
					candidate.err = err
				} else {
					ag.FromHome = scope == AgentScopeUser
					ag.Origin = candidate.origin
					candidate.agent = &ag
				}
			}
			if candidate.agent != nil {
				candidate.claims = []string{candidate.agent.Name}
			} else {
				candidate.claims = reservedAgentNames(candidate.filename, string(candidate.data), candidate.source.format)
				notes = append(notes, fmt.Sprintf("agent %s: %v", candidate.origin.LogicalPath, candidate.err))
			}
			for _, name := range candidate.claims {
				claims[name] = append(claims[name], i)
			}
		}

		duplicates := map[string]bool{}
		for name, owners := range claims {
			if len(owners) < 2 {
				continue
			}
			duplicates[name] = true
			paths := make([]string, 0, len(owners))
			for _, index := range owners {
				paths = append(paths, candidates[index].origin.LogicalPath)
			}
			sort.Strings(paths)
			notes = append(notes, fmt.Sprintf(
				"agent name %q is ambiguous in %s scope (%s); none of those definitions loaded",
				name, scope, strings.Join(paths, ", ")))
		}

		for i := range candidates {
			candidate := &candidates[i]
			if candidate.agent == nil || duplicates[candidate.agent.Name] {
				continue
			}
			if claim, exists := higher[candidate.agent.Name]; exists {
				if claim.rejected {
					notes = append(notes, fmt.Sprintf(
						"agent %s: name %q is reserved by higher-precedence definition %s, which was rejected",
						candidate.origin.LogicalPath, candidate.agent.Name, claim.paths[0]))
				}
				continue
			}
			agents = append(agents, *candidate.agent)
		}
		for name, owners := range claims {
			if _, exists := higher[name]; exists {
				continue
			}
			claim := precedenceClaim{rejected: duplicates[name]}
			for _, index := range owners {
				claim.paths = append(claim.paths, candidates[index].origin.LogicalPath)
				if candidates[index].agent == nil {
					claim.rejected = true
				}
			}
			higher[name] = claim
		}
	}

	// Sorted so the tool description, which the schema carries into the
	// frozen zone, never depends on directory read order.
	sort.Slice(agents, func(i, j int) bool { return agents[i].Name < agents[j].Name })
	return agents, sanitizeAgentNotes(notes)
}

func sanitizeAgentNotes(notes []string) []string {
	for i := range notes {
		notes[i] = sanitizeDefinitionDiagnostic(notes[i])
	}
	sort.Strings(notes)
	return notes
}

func sanitizeDefinitionDiagnostic(text string) string {
	text = redactCrossAgent(text)
	return strings.Map(func(r rune) rune {
		if unsafeDefinitionControl(r) {
			return '\ufffd'
		}
		return r
	}, text)
}

type agentSource struct {
	anchor      string
	relativeDir string
	logicalDir  string
	format      agentFormat
	dialect     AgentDialect
	scope       AgentScope
	recursive   bool
}

type agentCandidate struct {
	source   agentSource
	filename string
	data     []byte
	origin   AgentOrigin
	agent    *Agent
	claims   []string
	err      error
}

type agentLoadLimits struct {
	definitions int
	bytes       int64
}

func agentSources(workspace string) []agentSource {
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		workspace = filepath.Clean(workspace)
	}
	sources := []agentSource{
		newAgentSource(workspace, filepath.Join(".switchboard", "agents"), switchboardAgent,
			AgentDialectSwitchboard, AgentScopeWorkspace, false),
		newAgentSource(workspace, filepath.Join(".claude", "agents"), claudeAgent,
			AgentDialectClaude, AgentScopeWorkspace, true),
	}
	if home, err := os.UserHomeDir(); err == nil {
		if absolute, absErr := filepath.Abs(home); absErr == nil {
			home = absolute
		}
		sources = append(sources,
			newAgentSource(home, filepath.Join(".switchboard", "agents"), switchboardAgent,
				AgentDialectSwitchboard, AgentScopeUser, false),
			newAgentSource(home, filepath.Join(".claude", "agents"), claudeAgent,
				AgentDialectClaude, AgentScopeUser, true))
	}
	return sources
}

func newAgentSource(anchor, relativeDir string, format agentFormat, dialect AgentDialect, scope AgentScope, recursive bool) agentSource {
	return agentSource{
		anchor: anchor, relativeDir: relativeDir, logicalDir: filepath.Join(anchor, relativeDir),
		format: format, dialect: dialect, scope: scope, recursive: recursive,
	}
}

func loadAgentSource(src agentSource, limits *agentLoadLimits) ([]agentCandidate, []string, bool) {
	anchor, err := rootedfs.OpenRoot(src.anchor)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, false
		}
		return nil, []string{fmt.Sprintf("agent root %s: %v", src.anchor, err)}, true
	}
	defer anchor.Close()
	dir, err := rootedfs.OpenRootAt(anchor, src.relativeDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, false
		}
		return nil, []string{fmt.Sprintf("agent root %s: %v", src.logicalDir, err)}, true
	}
	defer dir.Close()

	paths, pathNotes, err := boundedAgentPaths(dir, src.recursive)
	if err != nil {
		pathNotes = append(pathNotes, fmt.Sprintf("agent root %s: %v", src.logicalDir, err))
		return nil, pathNotes, true
	}
	if limits.definitions+len(paths) > maxAgentDefinitions {
		pathNotes = append(pathNotes, fmt.Sprintf(
			"agent inventory exceeds the %d-definition limit; no named agents loaded", maxAgentDefinitions))
		return nil, pathNotes, true
	}

	candidates := make([]agentCandidate, 0, len(paths))
	for _, rel := range paths {
		limits.definitions++
		logical := filepath.Join(src.logicalDir, rel)
		candidate := agentCandidate{
			source: src, filename: filepath.Base(rel),
			origin: AgentOrigin{Dialect: src.dialect, Scope: src.scope, LogicalPath: logical},
		}
		data, resolved, err := readAgentDefinition(dir, rel, logical)
		if err != nil {
			candidate.err = err
			candidates = append(candidates, candidate)
			continue
		}
		if limits.bytes+int64(len(data)) > maxAgentAggregateBytes {
			pathNotes = append(pathNotes, fmt.Sprintf(
				"agent inventory exceeds the %d-byte aggregate limit; no named agents loaded", maxAgentAggregateBytes))
			return nil, pathNotes, true
		}
		limits.bytes += int64(len(data))
		candidate.data = data
		candidate.origin.Path = resolved
		candidates = append(candidates, candidate)
	}
	return candidates, pathNotes, false
}

type agentDir struct {
	path  string
	depth int
}

func boundedAgentPaths(root *os.Root, recursive bool) ([]string, []string, error) {
	queue := []agentDir{{path: "."}}
	entriesSeen := 0
	var paths, notes []string
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		directory, err := openRootedRead(root, current.path)
		if err != nil {
			return nil, notes, err
		}
		directoryInfo, err := directory.Stat()
		if err != nil {
			_ = directory.Close()
			return nil, notes, err
		}
		if !directoryInfo.IsDir() {
			_ = directory.Close()
			return nil, notes, fmt.Errorf("%s is not a directory", current.path)
		}
		remaining := maxAgentDirectoryEntries - entriesSeen
		entries, readErr := directory.ReadDir(remaining + 1)
		closeErr := directory.Close()
		if readErr != nil && readErr != io.EOF {
			return nil, notes, readErr
		}
		if closeErr != nil {
			return nil, notes, closeErr
		}
		if len(entries) > remaining {
			return nil, notes, fmt.Errorf("contains more than %d entries", maxAgentDirectoryEntries)
		}
		entriesSeen += len(entries)
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			rel := filepath.Join(current.path, entry.Name())
			info, err := root.Lstat(rel)
			if err != nil {
				return nil, notes, fmt.Errorf("inspect %s: %w", rel, err)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				if strings.HasSuffix(entry.Name(), ".md") {
					paths = append(paths, rel) // the anchored reader returns the diagnostic and tombstone
				} else if recursive {
					return nil, notes, fmt.Errorf("symbolic link %s prevents complete recursive discovery", rel)
				}
				continue
			}
			if info.IsDir() {
				if recursive {
					if current.depth >= maxAgentDirectoryDepth {
						return nil, notes, fmt.Errorf("directory %s exceeds the depth limit", rel)
					} else {
						queue = append(queue, agentDir{path: rel, depth: current.depth + 1})
					}
				}
				continue
			}
			if strings.HasSuffix(entry.Name(), ".md") {
				paths = append(paths, rel)
			}
		}
	}
	sort.Strings(paths)
	return paths, notes, nil
}

func readAgentDefinition(root *os.Root, rel, logical string) ([]byte, string, error) {
	return readAnchoredDefinition(root, rel, logical, maxAgentDefinitionBytes)
}

// readAnchoredDefinition reads a regular file through an os.Root handle and
// verifies that the path still names the opened file after the bounded read.
// Agent and workflow discovery share this boundary so neither can drift into
// a weaker path-replacement or symlink posture.
func readAnchoredDefinition(root *os.Root, rel, logical string, byteLimit int64) ([]byte, string, error) {
	return readAnchoredDefinitionWithHook(root, rel, logical, byteLimit, nil)
}

func readAnchoredDefinitionWithHook(root *os.Root, rel, logical string, byteLimit int64, beforeOpen func()) ([]byte, string, error) {
	before, err := root.Lstat(rel)
	if err != nil {
		return nil, "", err
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return nil, "", fmt.Errorf("symbolic-link definitions are not loaded")
	}
	if !before.Mode().IsRegular() {
		return nil, "", fmt.Errorf("definition is not a regular file")
	}
	if before.Size() > byteLimit {
		return nil, "", fmt.Errorf("definition exceeds the %d-byte limit", byteLimit)
	}
	if beforeOpen != nil {
		beforeOpen()
	}
	file, err := openRootedRead(root, rel)
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, "", err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, "", fmt.Errorf("definition changed while it was opened")
	}
	data, err := io.ReadAll(io.LimitReader(file, byteLimit+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > byteLimit {
		return nil, "", fmt.Errorf("definition exceeds the %d-byte limit", byteLimit)
	}
	after, err := root.Lstat(rel)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(opened, after) {
		return nil, "", fmt.Errorf("definition changed while it was read")
	}
	resolved, err := filepath.EvalSymlinks(logical)
	if err != nil {
		return nil, "", fmt.Errorf("resolve definition: %w", err)
	}
	resolvedInfo, err := os.Stat(resolved)
	if err != nil || !os.SameFile(opened, resolvedInfo) {
		return nil, "", fmt.Errorf("definition path changed while it was read")
	}
	return data, resolved, nil
}

func reservedAgentNames(filename, content string, format agentFormat) []string {
	set := map[string]bool{}
	fallback := strings.TrimSuffix(filename, ".md")
	lines := strings.Split(normalizeAgentDocument(content), "\n")
	if len(lines) > 0 && lines[0] == "---" {
		for i := 1; i < len(lines) && lines[i] != "---"; i++ {
			line := lines[i]
			if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") ||
				line[0] == ' ' || line[0] == '\t' {
				continue
			}
			key, raw, ok := strings.Cut(line, ":")
			if !ok || strings.TrimSpace(key) != "name" {
				continue
			}
			name, err := decodeYAMLScalar(stripYAMLValueComment(raw))
			if err != nil || name == "" || len(name) > maxAgentNameBytes || redactCrossAgent(name) != name {
				continue
			}
			if format == claudeAgent {
				if err := validClaudeAgentName(name); err != nil {
					continue
				}
			}
			set[name] = true
		}
	}
	if len(set) == 0 && fallback != "" {
		set[fallback] = true
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

type agentFormat uint8

const (
	switchboardAgent agentFormat = iota
	claudeAgent
)

// parseAgent reads Switchboard's own definition format. Every top-level field
// is checked: a misspelled grant such as `tool` must not collapse to omitted
// `tools`, whose meaning is the full suite.
func parseAgent(filename, content string, valid map[string]bool) (Agent, error) {
	return parseAgentDefinition(filename, content, valid, switchboardAgent)
}

// parseClaudeAgent is kept separate for tests so compatibility behavior is
// pinned without making the source dialect an inferred property of a path.
func parseClaudeAgent(filename, content string, valid map[string]bool) (Agent, error) {
	return parseAgentDefinition(filename, content, valid, claudeAgent)
}

// Native Claude fields that Switchboard cannot preserve are refused rather
// than ignored. Several grant additional authority (MCP servers, hooks,
// permission modes, isolation); the rest materially change the requested
// model, context, lifetime, persistence, or execution posture. Unknown native
// fields are refused too, so a newly introduced safety control cannot turn
// into a silent allow on an older Switchboard.
var supportedAgentFields = map[string]bool{
	"description": true,
	"name":        true,
	"tier":        true, // Switchboard extension; it has exact local semantics.
	"tools":       true,
}

func parseAgentDefinition(filename, content string, valid map[string]bool, format agentFormat) (Agent, error) {
	if err := validateAgentDocument(content); err != nil {
		return Agent{}, err
	}
	ag := Agent{Name: strings.TrimSuffix(filename, ".md")}
	fields, body, hasFrontmatter, err := readAgentFrontmatter(content)
	if err != nil {
		return Agent{}, err
	}
	var unsupported []string
	for key := range fields {
		if !supportedAgentFields[key] {
			unsupported = append(unsupported, key)
		}
	}
	sort.Strings(unsupported)
	if len(unsupported) > 0 {
		if format == claudeAgent {
			return Agent{}, fmt.Errorf("uses native field(s) Switchboard cannot preserve: %s",
				strings.Join(unsupported, ", "))
		}
		return Agent{}, fmt.Errorf("uses unrecognized Switchboard field(s): %s",
			strings.Join(unsupported, ", "))
	}
	if format == claudeAgent {
		if !hasFrontmatter {
			return Agent{}, fmt.Errorf("native Claude definition requires YAML frontmatter")
		}
		if _, ok := fields["name"]; !ok {
			return Agent{}, fmt.Errorf("native Claude definition has no name")
		}
		if _, ok := fields["description"]; !ok {
			return Agent{}, fmt.Errorf("native Claude definition has no description")
		}
	}

	if field, ok := fields["name"]; ok {
		value, err := scalarField("name", field)
		if err != nil {
			return Agent{}, err
		}
		if redactCrossAgent(value) != value {
			return Agent{}, fmt.Errorf("agent name contains credential-like text")
		}
		if value != "" {
			ag.Name = value
		} else if format == claudeAgent {
			return Agent{}, fmt.Errorf("native Claude definition has an empty name")
		}
	}
	if redactCrossAgent(ag.Name) != ag.Name {
		return Agent{}, fmt.Errorf("agent name contains credential-like text")
	}
	if err := validateAgentMetadata("name", ag.Name); err != nil {
		return Agent{}, err
	}
	if len(ag.Name) > maxAgentNameBytes {
		return Agent{}, fmt.Errorf("agent name exceeds the %d-byte limit", maxAgentNameBytes)
	}
	if format == claudeAgent {
		if err := validClaudeAgentName(ag.Name); err != nil {
			return Agent{}, err
		}
	}
	if field, ok := fields["description"]; ok {
		ag.Description, err = scalarField("description", field)
		if err != nil {
			return Agent{}, err
		}
		if format == claudeAgent && strings.TrimSpace(ag.Description) == "" {
			return Agent{}, fmt.Errorf("native Claude definition has an empty description")
		}
		ag.Description = redactCrossAgent(ag.Description)
		if len(ag.Description) > maxAgentDescriptionBytes {
			return Agent{}, fmt.Errorf("agent description exceeds the %d-byte limit", maxAgentDescriptionBytes)
		}
	}
	if field, ok := fields["tier"]; ok {
		ag.Tier, err = scalarField("tier", field)
		if err != nil {
			return Agent{}, err
		}
		if redactCrossAgent(ag.Tier) != ag.Tier {
			return Agent{}, fmt.Errorf("agent tier contains credential-like text")
		}
		if len(ag.Tier) > maxAgentTierBytes {
			return Agent{}, fmt.Errorf("agent tier exceeds the %d-byte limit", maxAgentTierBytes)
		}
	}
	if field, ok := fields["tools"]; ok {
		ag.ToolsSet = true
		ag.Tools, err = toolField(field)
		if err != nil {
			return Agent{}, err
		}
		if format == claudeAgent && len(ag.Tools) == 0 {
			return Agent{}, fmt.Errorf("native Claude definition explicitly grants zero tools; omit tools to inherit the suite")
		}
		if len(ag.Tools) > maxAgentTools {
			return Agent{}, fmt.Errorf("agent tool grant exceeds the %d-tool limit", maxAgentTools)
		}
	}

	for _, name := range ag.Tools {
		if redactCrossAgent(name) != name {
			return Agent{}, fmt.Errorf("agent tool grant contains credential-like text")
		}
		if !valid[name] {
			return Agent{}, fmt.Errorf("grants %q, which is not in the subagent suite (%s)",
				name, strings.Join(sortedNames(valid), ", "))
		}
	}
	ag.Prompt = strings.TrimSpace(redactCrossAgent(body))
	if ag.Prompt == "" {
		return Agent{}, fmt.Errorf("has no body; the body is the agent's instructions")
	}
	return ag, nil
}

func validateAgentDocument(content string) error {
	if !utf8.ValidString(content) {
		return fmt.Errorf("agent definition is not valid UTF-8")
	}
	for _, r := range content {
		if unsafeDefinitionControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return fmt.Errorf("agent definition contains a control character")
		}
	}
	return nil
}

func validateAgentMetadata(field, value string) error {
	for _, r := range value {
		if unsafeDefinitionControl(r) {
			return fmt.Errorf("agent %s contains a control character", field)
		}
	}
	return nil
}

func unsafeDefinitionControl(r rune) bool {
	return r < 0x20 || (r >= 0x7f && r <= 0x9f)
}

type frontmatterField struct {
	raw   string
	block []string
	line  int
}

// readAgentFrontmatter implements the deliberately small YAML subset agent
// definitions need. It rejects duplicate keys and ambiguous indentation, and
// returns field presence separately from value so `tools` and `tools: []` do
// not collapse into the same grant. Unsupported native fields are identified
// from their top-level key without parsing their potentially executable body.
func readAgentFrontmatter(content string) (map[string]frontmatterField, string, bool, error) {
	content = normalizeAgentDocument(content)
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || lines[0] != "---" {
		return map[string]frontmatterField{}, content, false, nil
	}

	end := -1
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return nil, "", true, fmt.Errorf("unterminated agent frontmatter")
	}

	fields := make(map[string]frontmatterField)
	for i := 1; i < end; {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			i++
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			return fields, "", true, fmt.Errorf("frontmatter line %d: unexpected indentation", i+1)
		}
		key, raw, ok := strings.Cut(line, ":")
		key = strings.TrimSpace(key)
		if !ok || !validFrontmatterKey(key) {
			return fields, "", true, fmt.Errorf("frontmatter line %d: expected a top-level field", i+1)
		}
		if _, exists := fields[key]; exists {
			return fields, "", true, fmt.Errorf("frontmatter line %d: duplicate field %q", i+1, key)
		}

		field := frontmatterField{raw: stripYAMLValueComment(raw), line: i + 1}
		i++
		for i < end {
			next := lines[i]
			nextTrimmed := strings.TrimSpace(next)
			if nextTrimmed == "" || strings.HasPrefix(nextTrimmed, "#") {
				field.block = append(field.block, next)
				i++
				continue
			}
			if next[0] != ' ' && next[0] != '\t' {
				break
			}
			field.block = append(field.block, next)
			i++
		}
		fields[key] = field
	}

	body := strings.Join(lines[end+1:], "\n")
	return fields, body, true, nil
}

func normalizeAgentDocument(content string) string {
	content = strings.TrimPrefix(content, "\ufeff")
	content = strings.ReplaceAll(content, "\r\n", "\n")
	return strings.ReplaceAll(content, "\r", "\n")
}

func validFrontmatterKey(key string) bool {
	if key == "" {
		return false
	}
	for _, r := range key {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func scalarField(name string, field frontmatterField) (string, error) {
	for _, line := range field.block {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			return "", fmt.Errorf("frontmatter line %d field %q: nested or multiline values are unsupported",
				field.line, name)
		}
	}
	value, err := decodeYAMLScalar(field.raw)
	if err != nil {
		return "", fmt.Errorf("frontmatter line %d field %q: %w", field.line, name, err)
	}
	return value, nil
}

func toolField(field frontmatterField) ([]string, error) {
	if field.raw != "" {
		for _, line := range field.block {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
				return nil, fmt.Errorf("frontmatter line %d field %q: cannot mix inline and block values",
					field.line, "tools")
			}
		}
		if strings.HasPrefix(strings.TrimSpace(field.raw), "[") {
			items, err := inlineYAMLList(field.raw)
			if err != nil {
				return nil, fmt.Errorf("frontmatter line %d field %q: %w", field.line, "tools", err)
			}
			return normalizeToolItems(items), nil
		}
		value, err := decodeYAMLScalar(field.raw)
		if err != nil {
			return nil, fmt.Errorf("frontmatter line %d field %q: %w", field.line, "tools", err)
		}
		return splitTools(value), nil
	}

	var (
		indent = -1
		items  []string
	)
	for _, line := range field.block {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		spaces := 0
		for spaces < len(line) && line[spaces] == ' ' {
			spaces++
		}
		if spaces < len(line) && line[spaces] == '\t' {
			return nil, fmt.Errorf("frontmatter line %d field %q: tabs are not valid list indentation",
				field.line, "tools")
		}
		if indent < 0 {
			indent = spaces
		} else if spaces != indent {
			return nil, fmt.Errorf("frontmatter line %d field %q: inconsistent list indentation",
				field.line, "tools")
		}
		item := strings.TrimSpace(line[spaces:])
		if !strings.HasPrefix(item, "-") || (len(item) > 1 && item[1] != ' ' && item[1] != '\t') {
			return nil, fmt.Errorf("frontmatter line %d field %q: expected a YAML list item",
				field.line, "tools")
		}
		item = stripYAMLValueComment(strings.TrimSpace(strings.TrimPrefix(item, "-")))
		if item == "" {
			return nil, fmt.Errorf("frontmatter line %d field %q: empty list item", field.line, "tools")
		}
		value, err := decodeYAMLScalar(item)
		if err != nil {
			return nil, fmt.Errorf("frontmatter line %d field %q: %w", field.line, "tools", err)
		}
		items = append(items, value)
	}
	return normalizeToolItems(items), nil
}

func inlineYAMLList(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '[' || raw[len(raw)-1] != ']' {
		return nil, fmt.Errorf("malformed inline list")
	}
	inner := strings.TrimSpace(raw[1 : len(raw)-1])
	if inner == "" {
		return []string{}, nil
	}
	parts, err := splitYAMLCommas(inner)
	if err != nil {
		return nil, err
	}
	items := make([]string, 0, len(parts))
	for i, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			if i == len(parts)-1 {
				continue // YAML permits a trailing comma in a flow sequence.
			}
			return nil, fmt.Errorf("empty inline list item")
		}
		value, err := decodeYAMLScalar(part)
		if err != nil {
			return nil, err
		}
		items = append(items, value)
	}
	return items, nil
}

func splitYAMLCommas(value string) ([]string, error) {
	var (
		parts   []string
		start   int
		quote   byte
		escaped bool
	)
	for i := 0; i < len(value); i++ {
		c := value[i]
		if quote == '"' {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		if quote == '\'' {
			if c == '\'' {
				if i+1 < len(value) && value[i+1] == '\'' {
					i++
					continue
				}
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case ',':
			parts = append(parts, value[start:i])
			start = i + 1
		}
	}
	if quote != 0 || escaped {
		return nil, fmt.Errorf("unterminated quoted list item")
	}
	parts = append(parts, value[start:])
	return parts, nil
}

func decodeYAMLScalar(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if raw == "~" || strings.EqualFold(raw, "null") {
		return "", fmt.Errorf("null is not a string")
	}
	var (
		value string
		err   error
	)
	switch raw[0] {
	case '"':
		value, err = decodeYAMLDoubleQuoted(raw)
	case '\'':
		value, err = decodeYAMLSingleQuoted(raw)
	case '[', '{', '|', '>':
		return "", fmt.Errorf("expected a scalar string")
	default:
		value, err = decodeYAMLPlain(raw)
	}
	if err != nil {
		return "", err
	}
	for _, r := range value {
		if unsafeDefinitionControl(r) {
			return "", fmt.Errorf("control character in scalar string")
		}
	}
	return value, nil
}

func decodeYAMLDoubleQuoted(raw string) (string, error) {
	if len(raw) < 2 {
		return "", fmt.Errorf("invalid double-quoted string")
	}
	var out strings.Builder
	for i := 1; i < len(raw); {
		if raw[i] == '"' {
			if i != len(raw)-1 {
				return "", fmt.Errorf("characters follow a quoted string")
			}
			return out.String(), nil
		}
		if raw[i] != '\\' {
			r, size := utf8.DecodeRuneInString(raw[i:])
			if r == utf8.RuneError && size == 1 {
				return "", fmt.Errorf("string is not valid UTF-8")
			}
			if r < 0x20 && r != '\t' {
				return "", fmt.Errorf("unescaped control character in string")
			}
			out.WriteRune(r)
			i += size
			continue
		}
		i++
		if i >= len(raw) {
			return "", fmt.Errorf("unterminated escape in double-quoted string")
		}
		switch raw[i] {
		case '0':
			out.WriteByte(0)
		case 'a':
			out.WriteByte('\a')
		case 'b':
			out.WriteByte('\b')
		case 't':
			out.WriteByte('\t')
		case 'n':
			out.WriteByte('\n')
		case 'v':
			out.WriteByte('\v')
		case 'f':
			out.WriteByte('\f')
		case 'r':
			out.WriteByte('\r')
		case 'e':
			out.WriteByte(0x1b)
		case ' ':
			out.WriteByte(' ')
		case '"', '/', '\\':
			out.WriteByte(raw[i])
		case 'N':
			out.WriteRune('\u0085')
		case '_':
			out.WriteRune('\u00a0')
		case 'L':
			out.WriteRune('\u2028')
		case 'P':
			out.WriteRune('\u2029')
		case 'x', 'u', 'U':
			digits := 2
			if raw[i] == 'u' {
				digits = 4
			} else if raw[i] == 'U' {
				digits = 8
			}
			if i+digits >= len(raw) {
				return "", fmt.Errorf("short hexadecimal escape in double-quoted string")
			}
			value, err := strconv.ParseUint(raw[i+1:i+1+digits], 16, 32)
			if err != nil {
				return "", fmt.Errorf("invalid hexadecimal escape in double-quoted string")
			}
			r := rune(value)
			if !utf8.ValidRune(r) || (r >= 0xd800 && r <= 0xdfff) {
				return "", fmt.Errorf("invalid Unicode escape in double-quoted string")
			}
			out.WriteRune(r)
			i += digits
		default:
			return "", fmt.Errorf("unsupported YAML escape \\%c", raw[i])
		}
		i++
	}
	return "", fmt.Errorf("unterminated double-quoted string")
}

func decodeYAMLSingleQuoted(raw string) (string, error) {
	if len(raw) < 2 || !utf8.ValidString(raw) {
		return "", fmt.Errorf("invalid single-quoted string")
	}
	var out strings.Builder
	for i := 1; i < len(raw); {
		if raw[i] == '\'' {
			if i+1 < len(raw) && raw[i+1] == '\'' {
				out.WriteByte('\'')
				i += 2
				continue
			}
			if i != len(raw)-1 {
				return "", fmt.Errorf("characters follow a quoted string")
			}
			return out.String(), nil
		}
		r, size := utf8.DecodeRuneInString(raw[i:])
		if r < 0x20 && r != '\t' {
			return "", fmt.Errorf("unescaped control character in string")
		}
		out.WriteRune(r)
		i += size
	}
	return "", fmt.Errorf("unterminated single-quoted string")
}

func decodeYAMLPlain(raw string) (string, error) {
	if !utf8.ValidString(raw) {
		return "", fmt.Errorf("string is not valid UTF-8")
	}
	if strings.ContainsRune("!&*{}[],#|>@`%", rune(raw[0])) ||
		((raw[0] == '-' || raw[0] == '?' || raw[0] == ':') &&
			(len(raw) == 1 || raw[1] == ' ' || raw[1] == '\t')) {
		return "", fmt.Errorf("plain scalar starts with a reserved YAML indicator")
	}
	if plainYAMLNonString(raw) {
		return "", fmt.Errorf("plain scalar resolves to a non-string YAML value; quote it")
	}
	for i, r := range raw {
		if r < 0x20 && r != '\t' {
			return "", fmt.Errorf("control character in plain scalar")
		}
		if r == ':' {
			next := i + 1
			if next == len(raw) || (next < len(raw) && (raw[next] == ' ' || raw[next] == '\t')) {
				return "", fmt.Errorf("mapping indicator in plain scalar; quote the value")
			}
		}
	}
	return raw, nil
}

func plainYAMLNonString(raw string) bool {
	lower := strings.ToLower(raw)
	switch lower {
	case "null", "~", "true", "false", "yes", "no", "on", "off",
		".nan", ".inf", "+.inf", "-.inf":
		return true
	}
	number := strings.ReplaceAll(raw, "_", "")
	if _, err := strconv.ParseInt(number, 0, 64); err == nil {
		return true
	}
	if _, err := strconv.ParseUint(number, 0, 64); err == nil {
		return true
	}
	_, err := strconv.ParseFloat(number, 64)
	return err == nil
}

// stripYAMLValueComment strips only a YAML value's trailing comment. Quotes
// inside a plain scalar (notably contractions such as "don't") are ordinary
// text; only a scalar that begins quoted, or quoted items inside a flow list,
// suppress a comment marker.
func stripYAMLValueComment(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if value[0] != '\'' && value[0] != '"' && value[0] != '[' {
		return stripPlainYAMLComment(value)
	}
	var (
		quote   byte
		escaped bool
	)
	for i := 0; i < len(value); i++ {
		c := value[i]
		if quote == '"' {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		if quote == '\'' {
			if c == '\'' {
				if i+1 < len(value) && value[i+1] == '\'' {
					i++
					continue
				}
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			if i == 0 || value[0] == '[' {
				quote = c
				continue
			}
		}
		if c == '#' && (i == 0 || value[i-1] == ' ' || value[i-1] == '\t') {
			return strings.TrimSpace(value[:i])
		}
	}
	return value
}

func stripPlainYAMLComment(value string) string {
	for i := 0; i < len(value); i++ {
		if value[i] == '#' && (i == 0 || value[i-1] == ' ' || value[i-1] == '\t') {
			return strings.TrimSpace(value[:i])
		}
	}
	return value
}

func normalizeToolItems(items []string) []string {
	var out []string
	for _, item := range items {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, normalizeToolName(item))
		}
	}
	return out
}

func validClaudeAgentName(name string) error {
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		return fmt.Errorf("native Claude agent name %q must start with a lowercase letter", name)
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || r == '-' {
			continue
		}
		return fmt.Errorf("native Claude agent name %q must contain only lowercase letters and hyphens", name)
	}
	return nil
}

// splitTools accepts the two list shapes people write: "read, grep" and
// "[read, grep]". Names are lowercased because the suite's are.
func splitTools(value string) []string {
	value = strings.Trim(value, "[]")
	var out []string
	for _, f := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' }) {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, normalizeToolName(f))
		}
	}
	return out
}

func normalizeToolName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if mapped, ok := nativeToolNames[name]; ok {
		return mapped
	}
	return name
}

// nativeToolNames translates the neighboring tool's spelling for a capability
// this suite also has. A tools list is a restriction, so a name that cannot be
// translated must keep failing the check rather than being dropped: honoring
// a narrowing means applying it, and an exact correspondence is applying it,
// while a guess would hand the subagent something its author withheld.
var nativeToolNames = map[string]string{
	"bash": "exec",
}

func sortedNames(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
