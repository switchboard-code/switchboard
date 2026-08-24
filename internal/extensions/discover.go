package extensions

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

const (
	codexManifest    = ".codex-plugin/plugin.json"
	claudeManifest   = ".claude-plugin/plugin.json"
	maxManifest      = 1 << 20
	maxDigestEntries = 20_000
	maxDigestBytes   = 256 << 20
	maxDigestDepth   = 128
)

type dialectSpec struct {
	dialect      Dialect
	manifest     string
	manifestless bool
}

var dialects = []dialectSpec{
	{dialect: DialectClaude, manifest: claudeManifest, manifestless: true},
	{dialect: DialectCodex, manifest: codexManifest},
}

type rawManifest map[string]json.RawMessage

type componentDeclaration struct {
	kind   ComponentKind
	source ComponentSource
	path   string
	inline bool
}

type candidateRoot struct {
	root     string
	realPath string
	scope    Scope
	dialect  Dialect
}

// Discover inspects exact local plugin roots. It never enables a plugin,
// starts a process, loads a hook, contacts a marketplace, or chooses a winner
// among duplicate IDs.
func Discover(candidates []Candidate) Result {
	roots, diagnostics := normalizeCandidates(candidates)
	var plugins []Plugin
	seenRoots := make(map[string]string)

	for _, root := range roots {
		for _, spec := range dialects {
			if root.dialect != "" && root.dialect != spec.dialect {
				continue
			}
			key := strings.Join([]string{string(root.scope), string(spec.dialect), root.realPath}, "\x00")
			if first, ok := seenRoots[key]; ok {
				diagnostics = append(diagnostics, Diagnostic{
					Severity: SeverityWarning,
					Code:     "duplicate-root",
					Path:     root.root,
					Message:  fmt.Sprintf("same %s plugin root and scope already discovered through %s", spec.dialect, first),
				})
				continue
			}

			plugin, found, diagnostic := inspectDialect(root, spec)
			if diagnostic != nil {
				diagnostics = append(diagnostics, *diagnostic)
				continue
			}
			if !found {
				continue
			}
			seenRoots[key] = root.root
			plugins = append(plugins, plugin)
		}
	}

	sortPlugins(plugins)
	addDuplicateIDDiagnostics(plugins, &diagnostics)
	for i := range plugins {
		sortComponents(plugins[i].Components)
		sortWarnings(plugins[i].Warnings)
	}
	sortDiagnostics(diagnostics)
	if plugins == nil {
		plugins = []Plugin{}
	}
	return Result{Plugins: plugins, Diagnostics: diagnostics}
}

func normalizeCandidates(candidates []Candidate) ([]candidateRoot, []Diagnostic) {
	roots := make([]candidateRoot, 0, len(candidates))
	var diagnostics []Diagnostic
	for _, candidate := range candidates {
		if candidate.Scope == "" {
			diagnostics = append(diagnostics, Diagnostic{
				Severity: SeverityError,
				Code:     "scope-required",
				Path:     candidate.Root,
				Message:  "plugin scope is required; discovery does not guess precedence",
			})
			continue
		}
		if hasControl(string(candidate.Scope)) {
			diagnostics = append(diagnostics, Diagnostic{
				Severity: SeverityError,
				Code:     "invalid-scope",
				Path:     candidate.Root,
				Message:  "plugin scope contains a control character",
			})
			continue
		}
		if candidate.Dialect != "" && candidate.Dialect != DialectCodex && candidate.Dialect != DialectClaude {
			diagnostics = append(diagnostics, Diagnostic{
				Severity: SeverityError,
				Code:     "invalid-dialect",
				Path:     candidate.Root,
				Message:  fmt.Sprintf("unsupported plugin dialect %q", candidate.Dialect),
			})
			continue
		}
		if strings.TrimSpace(candidate.Root) == "" {
			diagnostics = append(diagnostics, Diagnostic{
				Severity: SeverityError,
				Code:     "root-required",
				Message:  "plugin root is required",
			})
			continue
		}

		abs, err := filepath.Abs(candidate.Root)
		if err != nil {
			diagnostics = append(diagnostics, rootDiagnostic(candidate.Root, "invalid-root", err))
			continue
		}
		abs = filepath.Clean(abs)
		realPath, err := filepath.EvalSymlinks(abs)
		if err != nil {
			diagnostics = append(diagnostics, rootDiagnostic(abs, "unreadable-root", err))
			continue
		}
		realPath, err = filepath.Abs(realPath)
		if err != nil {
			diagnostics = append(diagnostics, rootDiagnostic(abs, "invalid-real-root", err))
			continue
		}
		info, err := os.Stat(realPath)
		if err != nil {
			diagnostics = append(diagnostics, rootDiagnostic(abs, "unreadable-root", err))
			continue
		}
		if !info.IsDir() {
			diagnostics = append(diagnostics, Diagnostic{
				Severity: SeverityError,
				Code:     "root-not-directory",
				Path:     abs,
				Message:  "plugin root is not a directory",
			})
			continue
		}
		roots = append(roots, candidateRoot{
			root:     abs,
			realPath: filepath.Clean(realPath),
			scope:    candidate.Scope,
			dialect:  candidate.Dialect,
		})
	}
	sort.SliceStable(roots, func(i, j int) bool {
		if roots[i].scope != roots[j].scope {
			return roots[i].scope < roots[j].scope
		}
		if roots[i].realPath != roots[j].realPath {
			return roots[i].realPath < roots[j].realPath
		}
		if roots[i].dialect != roots[j].dialect {
			return roots[i].dialect < roots[j].dialect
		}
		return roots[i].root < roots[j].root
	})
	return roots, diagnostics
}

func rootDiagnostic(root, code string, err error) Diagnostic {
	return Diagnostic{
		Severity: SeverityError,
		Code:     code,
		Path:     root,
		Message:  err.Error(),
	}
}

func inspectDialect(candidate candidateRoot, spec dialectSpec) (Plugin, bool, *Diagnostic) {
	root, err := rootedfs.OpenRoot(candidate.realPath)
	if err != nil {
		diagnostic := rootDiagnostic(candidate.root, "unreadable-root", err)
		return Plugin{}, false, &diagnostic
	}
	defer root.Close()

	manifestExists, err := safeExists(root, spec.manifest)
	if err != nil {
		diagnostic := dialectError(candidate, spec, "unsafe-manifest", err)
		return Plugin{}, false, &diagnostic
	}
	if !manifestExists && !spec.manifestless {
		return Plugin{}, false, nil
	}

	manifest := rawManifest{}
	namespace := ""
	warnings := []Warning{}
	manifestPath := ""
	if manifestExists {
		raw, err := readBounded(root, spec.manifest, maxManifest)
		if err != nil {
			diagnostic := dialectError(candidate, spec, "invalid-manifest", err)
			return Plugin{}, false, &diagnostic
		}
		manifest, err = decodeManifest(raw)
		if err != nil {
			diagnostic := dialectError(candidate, spec, "invalid-manifest", err)
			return Plugin{}, false, &diagnostic
		}
		namespace, err = manifestName(spec.dialect, manifest)
		if err != nil {
			diagnostic := dialectError(candidate, spec, "invalid-name", err)
			return Plugin{}, false, &diagnostic
		}
		manifestPath = filepath.Join(candidate.root, filepath.FromSlash(spec.manifest))
	} else {
		namespace = filepath.Base(candidate.realPath)
	}

	declarations, declarationWarnings, executable, err := parseDeclarations(spec.dialect, manifest)
	warnings = append(warnings, declarationWarnings...)
	if err != nil {
		diagnostic := dialectError(candidate, spec, "invalid-component-declaration", err)
		return Plugin{}, false, &diagnostic
	}

	_, manifestDeclaresSkills := manifest["skills"]
	defaultDeclarations, defaultWarnings, defaultExecutable, recognizable, err := scanDefaults(root, spec.dialect, manifestDeclaresSkills)
	warnings = append(warnings, defaultWarnings...)
	executable = executable || defaultExecutable
	if err != nil {
		diagnostic := dialectError(candidate, spec, "unsafe-default-component", err)
		return Plugin{}, false, &diagnostic
	}
	if !manifestExists && !recognizable {
		return Plugin{}, false, nil
	}
	if !manifestExists {
		if err := validateNamespace(namespace); err != nil {
			diagnostic := dialectError(candidate, spec, "invalid-name", fmt.Errorf("manifestless directory name: %w", err))
			return Plugin{}, false, &diagnostic
		}
		if spec.dialect == DialectClaude && !isKebabCase(namespace) {
			diagnostic := dialectError(candidate, spec, "invalid-name", errors.New("manifestless directory name must be lowercase kebab-case"))
			return Plugin{}, false, &diagnostic
		}
		warnings = append(warnings, Warning{
			Code:    "manifestless",
			Path:    candidate.root,
			Message: "Claude manifest is absent; namespace comes from the plugin directory name",
		})
	}
	declarations = append(declarations, defaultDeclarations...)

	components, componentWarnings, componentExecutable, err := normalizeComponents(root, candidate, spec.dialect, declarations)
	warnings = append(warnings, componentWarnings...)
	executable = executable || componentExecutable
	if err != nil {
		diagnostic := dialectError(candidate, spec, "unsafe-component", err)
		return Plugin{}, false, &diagnostic
	}

	digest, err := digestPlugin(root, spec.dialect)
	if err != nil {
		diagnostic := dialectError(candidate, spec, "invalid-component-tree", err)
		return Plugin{}, false, &diagnostic
	}

	plugin := Plugin{
		Dialect:    spec.dialect,
		Kind:       KindPlugin,
		Scope:      candidate.scope,
		Root:       candidate.root,
		RealPath:   candidate.realPath,
		Manifest:   manifestPath,
		Namespace:  namespace,
		ID:         string(spec.dialect) + ":" + namespace,
		Components: components,
		Executable: executable,
		Digest:     digest,
		Warnings:   warnings,
	}
	return plugin, true, nil
}

func dialectError(candidate candidateRoot, spec dialectSpec, code string, err error) Diagnostic {
	return Diagnostic{
		Severity: SeverityError,
		Code:     code,
		Path:     candidate.root,
		Message:  fmt.Sprintf("%s plugin: %v", spec.dialect, err),
	}
}

func decodeManifest(raw []byte) (rawManifest, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return nil, errors.New("manifest must be a JSON object")
	}

	manifest := rawManifest{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("manifest contains a non-string key")
		}
		if _, duplicate := manifest[key]; duplicate {
			return nil, fmt.Errorf("manifest contains duplicate key %q", key)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("manifest field %q: %w", key, err)
		}
		manifest[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if token, err := decoder.Token(); err != io.EOF {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("unexpected JSON value after manifest: %v", token)
	}
	return manifest, nil
}

func manifestName(dialect Dialect, manifest rawManifest) (string, error) {
	raw, ok := manifest["name"]
	if !ok {
		return "", errors.New("manifest requires a name")
	}
	var name string
	if err := json.Unmarshal(raw, &name); err != nil {
		return "", fmt.Errorf("name must be a string: %w", err)
	}
	name = strings.TrimSpace(name)
	if err := validateNamespace(name); err != nil {
		return "", err
	}
	if dialect == DialectClaude && !isKebabCase(name) {
		return "", errors.New("Claude plugin name must be lowercase kebab-case")
	}
	return name, nil
}

func validateNamespace(name string) error {
	if name == "" || name == "." || name == ".." {
		return errors.New("name must not be empty")
	}
	if hasControl(name) {
		return errors.New("name contains a control character")
	}
	if strings.ContainsAny(name, "/\\") {
		return errors.New("name must not contain a path separator")
	}
	return nil
}

func hasControl(value string) bool {
	return strings.IndexFunc(value, unicode.IsControl) >= 0
}

func isKebabCase(value string) bool {
	if value == "" || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	previousDash := false
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z', character >= '0' && character <= '9':
			previousDash = false
		case character == '-' && !previousDash:
			previousDash = true
		default:
			return false
		}
	}
	return true
}

func parseDeclarations(dialect Dialect, manifest rawManifest) ([]componentDeclaration, []Warning, bool, error) {
	if len(manifest) == 0 {
		return nil, nil, false, nil
	}
	var declarations []componentDeclaration
	var warnings []Warning
	executable := false
	fields := []struct {
		name string
		kind ComponentKind
	}{
		{name: "skills", kind: ComponentSkill},
		{name: "mcpServers", kind: ComponentMCP},
		{name: "hooks", kind: ComponentHook},
	}
	for _, field := range fields {
		raw, ok := manifest[field.name]
		if !ok {
			continue
		}
		paths, inline, err := componentValue(raw)
		if err != nil {
			return nil, nil, false, fmt.Errorf("%s: %w", field.name, err)
		}
		for _, declaredPath := range paths {
			declarations = append(declarations, componentDeclaration{
				kind:   field.kind,
				source: SourceManifest,
				path:   declaredPath,
			})
		}
		if inline {
			isExecutable := componentExecutable(field.kind)
			declarations = append(declarations, componentDeclaration{
				kind:   field.kind,
				source: SourceManifest,
				inline: true,
			})
			executable = executable || isExecutable
			warnings = append(warnings, Warning{
				Code:    "inline-component-requires-adapter",
				Path:    field.name,
				Message: fmt.Sprintf("inline %s semantics are recorded for an explicit adapter; discovery itself does not enable them", field.kind),
			})
		}
	}

	metadataFields := map[string]bool{
		"name": true, "version": true, "description": true, "author": true,
		"homepage": true, "repository": true, "license": true, "keywords": true,
	}
	if dialect == DialectClaude {
		for _, field := range []string{"$schema", "displayName", "metadata", "defaultEnabled"} {
			metadataFields[field] = true
		}
	} else {
		metadataFields["interface"] = true
	}
	for _, field := range []string{"skills", "mcpServers", "hooks"} {
		metadataFields[field] = true
	}
	unsupportedNonExecutable := map[string]bool{
		"agents":       true,
		"commands":     true,
		"outputStyles": true,
	}
	keys := make([]string, 0, len(manifest))
	for key := range manifest {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if metadataFields[key] {
			continue
		}
		// Unknown future semantics fail closed. Only native declarations known
		// to be prompt/display content avoid the executable capability flag.
		exec := !unsupportedNonExecutable[key]
		executable = executable || exec
		warnings = append(warnings, Warning{
			Code:    "unsupported-manifest-field",
			Path:    key,
			Message: fmt.Sprintf("%s manifest field %q is detected but not interpreted or enabled%s", dialect, key, executableSuffix(exec)),
		})
	}
	return declarations, warnings, executable, nil
}

func executableSuffix(executable bool) string {
	if executable {
		return "; it may start code and marks the plugin executable"
	}
	return ""
}

func componentValue(raw json.RawMessage) (paths []string, inline bool, err error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, false, errors.New("empty JSON value")
	}
	switch raw[0] {
	case '"':
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, false, err
		}
		if strings.TrimSpace(value) == "" {
			return nil, false, errors.New("path must not be empty")
		}
		return []string{value}, false, nil
	case '[':
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return nil, false, err
		}
		for i, value := range values {
			value = bytes.TrimSpace(value)
			if len(value) == 0 {
				return nil, false, fmt.Errorf("item %d is empty", i)
			}
			if value[0] != '"' {
				inline = true
				continue
			}
			var declaredPath string
			if err := json.Unmarshal(value, &declaredPath); err != nil {
				return nil, false, fmt.Errorf("item %d: %w", i, err)
			}
			if strings.TrimSpace(declaredPath) == "" {
				return nil, false, fmt.Errorf("item %d path must not be empty", i)
			}
			paths = append(paths, declaredPath)
		}
		return paths, inline, nil
	case '{':
		var value map[string]json.RawMessage
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, false, err
		}
		return nil, true, nil
	default:
		return nil, false, errors.New("must be a path string, path array, or inline object")
	}
}

func scanDefaults(root *os.Root, dialect Dialect, manifestDeclaresSkills bool) ([]componentDeclaration, []Warning, bool, bool, error) {
	defaults := []struct {
		kind ComponentKind
		path string
	}{
		{kind: ComponentSkill, path: "skills"},
		{kind: ComponentMCP, path: ".mcp.json"},
		{kind: ComponentHook, path: "hooks/hooks.json"},
	}
	var declarations []componentDeclaration
	var warnings []Warning
	executable := false
	recognizable := false
	hasSkillsDirectory := false
	for _, item := range defaults {
		exists, err := safeExists(root, item.path)
		if err != nil {
			return nil, nil, false, false, fmt.Errorf("%s: %w", item.path, err)
		}
		if !exists {
			continue
		}
		recognizable = true
		if item.kind == ComponentSkill {
			hasSkillsDirectory = true
		}
		declarations = append(declarations, componentDeclaration{
			kind:   item.kind,
			source: SourceDefault,
			path:   "./" + item.path,
		})
		executable = executable || componentExecutable(item.kind)
	}

	if dialect == DialectClaude {
		rootSkill, err := safeInfo(root, "SKILL.md")
		if err != nil {
			return nil, nil, false, false, fmt.Errorf("SKILL.md: %w", err)
		}
		if rootSkill != nil && !hasSkillsDirectory && !manifestDeclaresSkills {
			if !rootSkill.Mode().IsRegular() {
				return nil, nil, false, false, errors.New("SKILL.md is not a regular file")
			}
			recognizable = true
			declarations = append(declarations, componentDeclaration{
				kind:   ComponentSkill,
				source: SourceDefault,
				path:   "./",
			})
		}

		unsupportedDefaults := []struct {
			path       string
			executable bool
		}{
			{path: "commands"},
			{path: "agents"},
			{path: "workflows", executable: true},
			{path: "bin", executable: true},
			{path: "output-styles"},
			{path: "themes"},
			{path: "settings.json"},
			{path: ".lsp.json", executable: true},
			{path: "monitors/monitors.json", executable: true},
		}
		for _, item := range unsupportedDefaults {
			exists, err := safeExists(root, item.path)
			if err != nil {
				return nil, nil, false, false, fmt.Errorf("%s: %w", item.path, err)
			}
			if !exists {
				continue
			}
			recognizable = true
			executable = executable || item.executable
			warnings = append(warnings, Warning{
				Code:    "unsupported-default-component",
				Path:    "./" + item.path,
				Message: fmt.Sprintf("Claude default component %q is detected but not interpreted or enabled%s", item.path, executableSuffix(item.executable)),
			})
		}
	}
	return declarations, warnings, executable, recognizable, nil
}

func normalizeComponents(root *os.Root, candidate candidateRoot, dialect Dialect, declarations []componentDeclaration) ([]Component, []Warning, bool, error) {
	var components []Component
	var warnings []Warning
	seen := make(map[string]int)
	executable := false
	for _, declaration := range declarations {
		isExecutable := componentExecutable(declaration.kind)
		executable = executable || isExecutable
		if declaration.inline {
			key := string(declaration.kind) + "\x00inline"
			if _, duplicate := seen[key]; duplicate {
				warnings = append(warnings, Warning{
					Code:    "duplicate-component",
					Path:    string(declaration.kind),
					Message: "duplicate inline component declaration was ignored",
				})
				continue
			}
			seen[key] = len(components)
			components = append(components, Component{
				Kind:       declaration.kind,
				Source:     declaration.source,
				Inline:     true,
				Executable: isExecutable,
			})
			continue
		}

		relative, err := nativeComponentPath(dialect, declaration)
		if err != nil {
			return nil, nil, false, fmt.Errorf("%s path %q: %w", declaration.kind, declaration.path, err)
		}
		key := string(declaration.kind) + "\x00" + relative
		if index, duplicate := seen[key]; duplicate {
			if components[index].Source != declaration.source {
				// A manifest commonly names the same conventional path that the
				// default scan finds. It is one component, not a collision.
				if components[index].Source == SourceDefault && declaration.source == SourceManifest {
					components[index].Source = SourceManifest
					components[index].DeclaredPath = declaration.path
				}
				continue
			}
			warnings = append(warnings, Warning{
				Code:    "duplicate-component",
				Path:    declaration.path,
				Message: fmt.Sprintf("duplicate %s component path resolves to %q", declaration.kind, relative),
			})
			continue
		}

		var info os.FileInfo
		if relative == "." {
			info, err = root.Stat(".")
		} else {
			info, err = safeInfo(root, relative)
		}
		if err != nil {
			return nil, nil, false, fmt.Errorf("%s path %q: %w", declaration.kind, declaration.path, err)
		}
		if info == nil {
			return nil, nil, false, fmt.Errorf("%s path %q does not exist", declaration.kind, declaration.path)
		}
		if err := validateComponentType(declaration.kind, declaration.path, info); err != nil {
			return nil, nil, false, err
		}
		if declaration.kind == ComponentMCP || declaration.kind == ComponentHook {
			if err := validateJSONComponent(root, relative); err != nil {
				return nil, nil, false, fmt.Errorf("%s path %q: %w", declaration.kind, declaration.path, err)
			}
		}

		absolutePath := filepath.Join(candidate.root, filepath.FromSlash(relative))
		realPath := filepath.Join(candidate.realPath, filepath.FromSlash(relative))
		seen[key] = len(components)
		components = append(components, Component{
			Kind:         declaration.kind,
			Source:       declaration.source,
			DeclaredPath: declaration.path,
			Path:         absolutePath,
			RealPath:     realPath,
			Executable:   isExecutable,
		})
	}
	if components == nil {
		components = []Component{}
	}
	return components, warnings, executable, nil
}

func nativeComponentPath(dialect Dialect, declaration componentDeclaration) (string, error) {
	if hasGitPathSegment(declaration.path) {
		return "", errors.New("component paths must not enter .git metadata excluded from the plugin digest")
	}
	if declaration.source == SourceManifest && !strings.HasPrefix(declaration.path, "./") {
		if dialect == DialectClaude && declaration.kind == ComponentSkill && declaration.path == "." {
			return ".", nil
		}
		return "", fmt.Errorf("%s manifest paths must begin with ./", dialect)
	}
	if dialect == DialectClaude && declaration.kind == ComponentSkill && declaration.path == "./" {
		return ".", nil
	}
	return safeRelativePath(declaration.path)
}

func hasGitPathSegment(declared string) bool {
	for _, segment := range strings.Split(declared, "/") {
		if segment == ".git" {
			return true
		}
	}
	return false
}

func componentExecutable(kind ComponentKind) bool {
	return kind == ComponentMCP || kind == ComponentHook
}

func safeRelativePath(declared string) (string, error) {
	if declared == "" {
		return "", errors.New("path is empty")
	}
	if hasControl(declared) {
		return "", errors.New("path contains a control character")
	}
	if strings.Contains(declared, "\\") {
		return "", errors.New("backslash is not a portable plugin path separator")
	}
	if strings.HasPrefix(declared, "/") || filepath.IsAbs(declared) || filepath.VolumeName(declared) != "" || looksLikeWindowsDrivePath(declared) {
		return "", errors.New("absolute paths are not allowed")
	}
	for _, segment := range strings.Split(declared, "/") {
		if segment == ".." {
			return "", errors.New("parent traversal is not allowed")
		}
	}
	clean := path.Clean(declared)
	clean = strings.TrimPrefix(clean, "./")
	if clean == "." || clean == "" {
		return "", errors.New("path must name a component below the plugin root")
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("path escapes the plugin root")
	}
	return clean, nil
}

func looksLikeWindowsDrivePath(value string) bool {
	if len(value) < 2 || value[1] != ':' {
		return false
	}
	letter := value[0]
	return letter >= 'a' && letter <= 'z' || letter >= 'A' && letter <= 'Z'
}

// safeInfo rejects a symlink in any existing component of rel. os.Root is the
// second containment boundary: a concurrent path replacement still cannot make
// the operation escape the physical plugin root.
func safeInfo(root *os.Root, rel string) (os.FileInfo, error) {
	clean, err := safeRelativePath(rel)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(clean, "/")
	for i := range parts {
		prefix := strings.Join(parts[:i+1], "/")
		info, err := root.Lstat(filepath.FromSlash(prefix))
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("symlink component %q is not allowed", prefix)
		}
		if i < len(parts)-1 && !info.IsDir() {
			return nil, fmt.Errorf("path component %q is not a directory", prefix)
		}
		if i == len(parts)-1 {
			return info, nil
		}
	}
	return nil, nil
}

func safeExists(root *os.Root, rel string) (bool, error) {
	info, err := safeInfo(root, rel)
	return info != nil, err
}

func validateComponentType(kind ComponentKind, declared string, info os.FileInfo) error {
	switch kind {
	case ComponentSkill:
		if !info.IsDir() {
			return fmt.Errorf("skill path %q is not a directory", declared)
		}
	case ComponentMCP, ComponentHook:
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s path %q is not a regular file", kind, declared)
		}
	}
	return nil
}

func validateJSONComponent(root *os.Root, rel string) error {
	raw, err := readBounded(root, rel, maxManifest)
	if err != nil {
		return err
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err != nil {
			return err
		}
		return errors.New("multiple JSON values")
	}
	if _, ok := value.(map[string]any); !ok {
		return errors.New("JSON component must be an object")
	}
	return nil
}

func readBounded(root *os.Root, rel string, limit int64) ([]byte, error) {
	return readBoundedWithHook(root, rel, limit, nil)
}

func readBoundedWithHook(root *os.Root, rel string, limit int64, beforeOpen func()) ([]byte, error) {
	info, err := safeInfo(root, rel)
	if err != nil {
		return nil, err
	}
	if info == nil {
		return nil, os.ErrNotExist
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("file is %d bytes; limit is %d", info.Size(), limit)
	}
	if beforeOpen != nil {
		beforeOpen()
	}
	file, err := openExtensionRootRead(root, filepath.FromSlash(rel))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, errors.New("file changed while it was opened")
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("file grew beyond %d-byte limit while reading", limit)
	}
	finished, err := file.Stat()
	if err != nil {
		return nil, err
	}
	linked, linkErr := safeInfo(root, rel)
	if linkErr != nil || linked == nil || !linked.Mode().IsRegular() ||
		!os.SameFile(opened, finished) || !os.SameFile(finished, linked) ||
		opened.Size() != finished.Size() || finished.Size() != int64(len(raw)) ||
		!opened.ModTime().Equal(finished.ModTime()) {
		return nil, errors.Join(linkErr, errors.New("file changed while it was read"))
	}
	return raw, nil
}

type digestFile struct {
	rel  string
	mode os.FileMode
	data []byte
}

type digestBudget struct {
	entries int
	bytes   int64
}

func digestPlugin(root *os.Root, dialect Dialect) (string, error) {
	files := make(map[string]digestFile)
	budget := digestBudget{}
	entries, err := readDigestDirectory(root, ".", maxDigestEntries)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.Name() == ".git" {
			if err := validateExcludedGit(root, ".git"); err != nil {
				return "", err
			}
			continue
		}
		if err := collectDigestPath(root, entry.Name(), files, &budget); err != nil {
			return "", err
		}
	}

	paths := make([]string, 0, len(files))
	for rel := range files {
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	hash := sha256.New()
	writeDigestField(hash, []byte("switchboard-extension-v1"))
	writeDigestField(hash, []byte(dialect))
	for _, rel := range paths {
		file := files[rel]
		writeDigestField(hash, []byte(rel))
		if file.mode.IsDir() {
			writeDigestField(hash, []byte("directory"))
		} else if file.mode.Perm()&0o111 != 0 {
			writeDigestField(hash, []byte("executable"))
		} else {
			writeDigestField(hash, []byte("regular"))
		}
		writeDigestField(hash, file.data)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func writeDigestField(writer io.Writer, value []byte) {
	var size [8]byte
	binary.LittleEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = writer.Write(size[:])
	_, _ = writer.Write(value)
}

func collectDigestPath(root *os.Root, rel string, files map[string]digestFile, budget *digestBudget) error {
	rel, err := safeRelativePath(rel)
	if err != nil {
		return err
	}
	if strings.Count(rel, "/")+1 > maxDigestDepth {
		return fmt.Errorf("plugin path exceeds %d-directory depth limit: %q", maxDigestDepth, rel)
	}
	info, err := safeInfo(root, rel)
	if err != nil {
		return err
	}
	if info == nil {
		return os.ErrNotExist
	}
	budget.entries++
	if budget.entries > maxDigestEntries {
		return fmt.Errorf("plugin has more than %d filesystem entries", maxDigestEntries)
	}
	if info.Mode().IsRegular() {
		if _, exists := files[rel]; exists {
			return nil
		}
		remaining := int64(maxDigestBytes) - budget.bytes
		if remaining < 0 || info.Size() > remaining {
			return fmt.Errorf("plugin content exceeds %d-byte digest limit", maxDigestBytes)
		}
		data, err := readBounded(root, rel, remaining)
		if err != nil {
			return err
		}
		budget.bytes += int64(len(data))
		files[rel] = digestFile{rel: rel, mode: info.Mode(), data: data}
		return nil
	}
	if !info.IsDir() {
		return fmt.Errorf("special file %q is not allowed", rel)
	}
	files[rel] = digestFile{rel: rel, mode: info.Mode()}

	entries, err := readDigestDirectory(root, rel, maxDigestEntries-budget.entries)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == ".git" {
			if err := validateExcludedGit(root, path.Join(rel, entry.Name())); err != nil {
				return err
			}
			continue
		}
		child := path.Join(rel, entry.Name())
		if err := collectDigestPath(root, child, files, budget); err != nil {
			return err
		}
	}
	return nil
}

func validateExcludedGit(root *os.Root, rel string) error {
	info, err := safeInfo(root, rel)
	if err != nil {
		return err
	}
	if info == nil {
		return os.ErrNotExist
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return fmt.Errorf("special file %q is not allowed", rel)
	}
	return nil
}

func readDigestDirectory(root *os.Root, rel string, remaining int) ([]os.DirEntry, error) {
	return readDigestDirectoryWithHook(root, rel, remaining, nil)
}

func readDigestDirectoryWithHook(root *os.Root, rel string, remaining int, beforeOpen func()) ([]os.DirEntry, error) {
	before, err := root.Lstat(filepath.FromSlash(rel))
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, fmt.Errorf("plugin digest path %q is not a real directory", rel)
	}
	if beforeOpen != nil {
		beforeOpen()
	}
	directory, err := openExtensionRootRead(root, filepath.FromSlash(rel))
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	opened, err := directory.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.IsDir() || !os.SameFile(before, opened) {
		return nil, fmt.Errorf("plugin digest directory %q changed while it was opened", rel)
	}

	entries := make([]os.DirEntry, 0, min(remaining, 256))
	for {
		batch, readErr := directory.ReadDir(256)
		entries = append(entries, batch...)
		if len(entries) > remaining {
			return nil, fmt.Errorf("plugin has more than %d filesystem entries", maxDigestEntries)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	finished, err := directory.Stat()
	if err != nil {
		return nil, err
	}
	linked, linkErr := root.Lstat(filepath.FromSlash(rel))
	if linkErr != nil || !linked.IsDir() || linked.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(opened, finished) || !os.SameFile(finished, linked) ||
		!opened.ModTime().Equal(finished.ModTime()) {
		return nil, errors.Join(linkErr, fmt.Errorf("plugin digest directory %q changed while it was read", rel))
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}

func addDuplicateIDDiagnostics(plugins []Plugin, diagnostics *[]Diagnostic) {
	byID := make(map[string][]int)
	for i := range plugins {
		byID[plugins[i].ID] = append(byID[plugins[i].ID], i)
	}
	ids := make([]string, 0, len(byID))
	for id, indexes := range byID {
		if len(indexes) > 1 {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		indexes := byID[id]
		locations := make([]string, 0, len(indexes))
		for _, index := range indexes {
			locations = append(locations, fmt.Sprintf("%s:%s", plugins[index].Scope, plugins[index].RealPath))
		}
		message := fmt.Sprintf("extension ID %q appears at %s; discovery does not choose precedence", id, strings.Join(locations, ", "))
		*diagnostics = append(*diagnostics, Diagnostic{
			Severity: SeverityWarning,
			Code:     "duplicate-id",
			Path:     plugins[indexes[0]].Root,
			Message:  message,
		})
		for _, index := range indexes {
			plugins[index].Warnings = append(plugins[index].Warnings, Warning{
				Code:    "duplicate-id",
				Path:    plugins[index].Root,
				Message: message,
			})
		}
	}
}

func sortPlugins(plugins []Plugin) {
	sort.SliceStable(plugins, func(i, j int) bool {
		left, right := plugins[i], plugins[j]
		if left.Dialect != right.Dialect {
			return left.Dialect < right.Dialect
		}
		if left.Scope != right.Scope {
			return left.Scope < right.Scope
		}
		if left.ID != right.ID {
			return left.ID < right.ID
		}
		if left.RealPath != right.RealPath {
			return left.RealPath < right.RealPath
		}
		return left.Root < right.Root
	})
}

func sortComponents(components []Component) {
	sort.SliceStable(components, func(i, j int) bool {
		left, right := components[i], components[j]
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		if left.RealPath != right.RealPath {
			return left.RealPath < right.RealPath
		}
		if left.Inline != right.Inline {
			return !left.Inline
		}
		if left.Source != right.Source {
			return left.Source < right.Source
		}
		return left.DeclaredPath < right.DeclaredPath
	})
}

func sortWarnings(warnings []Warning) {
	sort.SliceStable(warnings, func(i, j int) bool {
		if warnings[i].Code != warnings[j].Code {
			return warnings[i].Code < warnings[j].Code
		}
		if warnings[i].Path != warnings[j].Path {
			return warnings[i].Path < warnings[j].Path
		}
		return warnings[i].Message < warnings[j].Message
	})
}

func sortDiagnostics(diagnostics []Diagnostic) {
	sort.SliceStable(diagnostics, func(i, j int) bool {
		left, right := diagnostics[i], diagnostics[j]
		if left.Severity != right.Severity {
			return severityRank(left.Severity) < severityRank(right.Severity)
		}
		if left.Code != right.Code {
			return left.Code < right.Code
		}
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		return left.Message < right.Message
	})
}

func severityRank(severity Severity) int {
	if severity == SeverityError {
		return 0
	}
	return 1
}
