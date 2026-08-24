package skills

// Skills: standing instructions the model pulls in when the task matches,
// instead of instructions that ride every request. A skill is a markdown
// file with a name and a short description; the descriptions travel in
// the skill tool's own description, the bodies stay on disk until asked
// for, and the pull is a tool call the transcript shows. Native Codex and
// Claude skill trees use the same directory-plus-SKILL.md shape and are
// discovered in place alongside Switchboard's own tree.
//
// The trust posture is the named agents' (§13): these prompt-only directories
// load without a grant because nothing executes at read time — a skill is a
// prompt, and whatever it persuades the model to do passes the permission
// engine on its own merits. Discovery is once, at session assembly, sorted by
// canonical selector, because the descriptions ride the tool schema into the frozen zone
// (§6.1).

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"unicode"

	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

const (
	maxDefinitionBytes       = int64(1 << 20) // one MiB of instructions/frontmatter
	maxSupportingBytes       = int64(4 << 20) // four MiB per explicitly requested resource
	maxSkillDirectoryEntries = 1024
)

// Ecosystem identifies the directory convention that supplied a skill.
// It is provenance only: every loaded skill is served through the same
// read-only tool and remains subject to Switchboard's permission engine.
type Ecosystem string

const (
	EcosystemSwitchboard Ecosystem = "switchboard"
	EcosystemCodex       Ecosystem = "codex"
	EcosystemClaude      Ecosystem = "claude"
)

// Scope identifies the caller-visible configuration level that supplied a
// skill. Direct discovery uses workspace and user; explicit plugin roots may
// additionally preserve local and managed scope.
type Scope string

const (
	ScopeWorkspace Scope = "workspace"
	ScopeUser      Scope = "user"
	ScopeLocal     Scope = "local"
	ScopeManaged   Scope = "managed"
)

// Origin records where a loaded definition came from. Path is the resolved
// definition path, so two symlink aliases have one stable identity.
type Origin struct {
	Ecosystem Ecosystem
	Scope     Scope
	// Namespace is non-empty for an explicitly supplied plugin component.
	// It is caller-owned identity, never inferred from a filesystem path.
	Namespace string
	// LogicalPath is the discovered path before symlink resolution. Path is
	// the resolved definition path used for identity and safe reads.
	LogicalPath string
	Path        string
}

// Skill is one loaded definition.
type Skill struct {
	Name        string
	Description string

	// Selector is the stable, source-qualified name used for explicit and
	// model tool invocation. Native sources can legitimately contain equal
	// display names, so Name alone is never an authority boundary.
	Selector string

	// Body is the instructions, served when the model asks.
	Body string

	// Dir is where supporting files beside the skill live: the skill's own
	// directory for the <name>/SKILL.md shape, the skills directory for a
	// flat <name>.md. The tool serves those files from here, so a pack that
	// references its own references/ works wherever it was copied from.
	Dir string

	// FromHome preserves the original API's user-versus-workspace signal for
	// every ecosystem. Prefer Origin.Scope in new code.
	FromHome bool

	// Origin distinguishes otherwise identical native layouts and makes
	// precedence decisions inspectable by callers.
	Origin Origin

	// ImplicitDisabled keeps a skill out of model-visible schemas while
	// retaining it in the inventory for explicit user invocation.
	ImplicitDisabled bool
	ModelBlockers    []string

	// UserInvocationDisabled implements Claude's user-invocable:false.
	UserInvocationDisabled bool

	// ArgumentHint and ArgumentNames carry Claude's static invocation
	// metadata. No other host substitution or execution behavior is implied.
	ArgumentHint  string
	ArgumentNames []string

	// InvocationBlockers name native host behaviors Switchboard cannot honor
	// safely. Blocked skills remain inspectable but neither the model nor the
	// explicit /skill command can load them.
	InvocationBlockers []string

	// Notes name native controls this build does not apply but which are safe
	// to leave unapplied. The skill loads and runs; the note exists so the
	// difference from its authoring host is stated rather than discovered.
	Notes []string

	// rootDir is the resolved resource root captured at discovery. Dir stays
	// logical for API/display compatibility, while serving cannot be retargeted
	// later by swapping a skill-directory symlink.
	rootDir string

	// rootInfo pins the filesystem identity that supplied the definition.
	// Supporting-file reads compare their opened root with this identity, so
	// replacing even the canonical directory after discovery fails closed.
	rootInfo os.FileInfo
}

type source struct {
	dir          string
	selectorRoot string
	namespace    string
	ecosystem    Ecosystem
	scope        Scope
	flat         bool
}

// AdditionalRoot is one already-discovered plugin skill component. Path is
// either a component directory containing skill subdirectories or a single
// skill directory containing SKILL.md. Namespace is the caller's canonical
// plugin ID. LoadAdditional never searches for roots, reads activation state,
// or enables executable plugin components.
type AdditionalRoot struct {
	Path      string
	Namespace string
	Dialect   Ecosystem
	Scope     Scope
}

// Load reads Switchboard, Codex, and Claude skill directories plus Claude's
// legacy command directories once. It retains every valid, source-qualified
// definition; equal display names are native and never resolved by an invisible
// cross-ecosystem precedence rule. Within a workspace, native trees are walked
// from the start directory to the repository root. Duplicate aliases of the
// same resolved definition are collapsed only within one ecosystem.
//
// Switchboard keeps accepting both <name>.md and <name>/SKILL.md. Native
// .agents/skills and .claude/skills trees accept only the standard directory
// shape. Claude .claude/commands/**/*.md files are adapted to manual-only
// entries. Load returns the complete valid inventory, including manual-only and
// equal-name definitions; ModelVisible selects the safe schema subset.
func Load(workspace string) (list []Skill, notes []string) {
	sources := skillSources(workspace)
	seenPaths := map[string]bool{}
	for _, src := range sources {
		entries, err := readSkillDirectory(src.dir, maxSkillDirectoryEntries)
		if err != nil {
			if !os.IsNotExist(err) {
				notes = append(notes, fmt.Sprintf("skills %s: %v", src.dir, err))
			}
			continue
		}
		for _, e := range entries {
			var path, fallback, dir string
			packed := false
			entryPath := filepath.Join(src.dir, e.Name())
			info, statErr := os.Stat(entryPath) // follow supported skill-directory symlinks
			if statErr != nil {
				if e.Type()&os.ModeSymlink != 0 {
					notes = append(notes, fmt.Sprintf("skill %s: %v", entryPath, statErr))
				}
				continue
			}
			switch {
			case info.IsDir():
				packed = true
				dir = entryPath
				path = filepath.Join(dir, "SKILL.md")
				fallback = e.Name()
				definitionInfo, err := os.Stat(path)
				if err != nil {
					if !os.IsNotExist(err) {
						notes = append(notes, fmt.Sprintf("skill %s: %v", path, err))
					}
					continue
				}
				if !definitionInfo.Mode().IsRegular() {
					continue // a directory without a SKILL.md is not a skill
				}
			case info.Mode().IsRegular() && src.flat && strings.HasSuffix(e.Name(), ".md"):
				path = filepath.Join(src.dir, e.Name())
				fallback = strings.TrimSuffix(e.Name(), ".md")
				dir = src.dir
			default:
				continue
			}

			resolvedDir, resolvedPath, err := resolveDefinition(dir, path)
			if err != nil {
				notes = append(notes, fmt.Sprintf("skill %s: %v", path, err))
				continue
			}
			identity := string(src.ecosystem) + "\x00" + resolvedPath
			if seenPaths[identity] {
				continue
			}
			seenPaths[identity] = true

			sk, skillNotes, err := buildSkill(src, entryPath, dir, path, fallback, packed, resolvedDir, resolvedPath)
			if err != nil {
				notes = append(notes, fmt.Sprintf("skill %s: %v", resolvedPath, err))
				continue
			}
			notes = append(notes, skillNotes...)
			list = append(list, sk)
		}
	}
	commands, commandNotes := loadClaudeCommands(workspace, list)
	list = append(list, commands...)
	notes = append(notes, commandNotes...)

	// Sorted so the tool description, which the schema carries into the
	// frozen zone, never depends on directory read order.
	sort.Slice(list, func(i, j int) bool { return list[i].Key() < list[j].Key() })
	return list, notes
}

// LoadAdditional loads only the exact plugin component roots supplied by the
// caller. A root-level SKILL.md is one skill; otherwise immediate child
// directories with SKILL.md are loaded. No parent, home, repository, manifest,
// marketplace, or network discovery occurs here. Callers must pass only
// components from plugins they have independently chosen to enable.
func LoadAdditional(roots []AdditionalRoot) (list []Skill, notes []string) {
	ordered := append([]AdditionalRoot(nil), roots...)
	sort.Slice(ordered, func(i, j int) bool {
		left := string(ordered[i].Dialect) + "\x00" + ordered[i].Namespace + "\x00" + string(ordered[i].Scope) + "\x00" + ordered[i].Path
		right := string(ordered[j].Dialect) + "\x00" + ordered[j].Namespace + "\x00" + string(ordered[j].Scope) + "\x00" + ordered[j].Path
		return left < right
	})
	seenPaths := map[string]bool{}
	for _, root := range ordered {
		if err := validateAdditionalRoot(root); err != nil {
			notes = append(notes, fmt.Sprintf("plugin skill root %s: %v", root.Path, err))
			continue
		}
		absolute, err := filepath.Abs(root.Path)
		if err != nil {
			notes = append(notes, fmt.Sprintf("plugin skill root %s: %v", root.Path, err))
			continue
		}
		src := source{
			dir: absolute, selectorRoot: absolute, namespace: root.Namespace,
			ecosystem: root.Dialect, scope: root.Scope,
		}
		candidates, err := additionalCandidates(absolute)
		if err != nil {
			notes = append(notes, fmt.Sprintf("plugin skill root %s: %v", absolute, err))
			continue
		}
		for _, candidate := range candidates {
			resolvedDir, resolvedPath, err := resolveDefinition(candidate.dir, candidate.path)
			if err != nil {
				notes = append(notes, fmt.Sprintf("plugin skill %s: %v", candidate.path, err))
				continue
			}
			identity := root.Namespace + "\x00" + string(root.Dialect) + "\x00" + string(root.Scope) + "\x00" + resolvedPath
			if seenPaths[identity] {
				continue
			}
			seenPaths[identity] = true
			sk, skillNotes, err := buildSkill(src, candidate.entryPath, candidate.dir, candidate.path, candidate.fallback, true, resolvedDir, resolvedPath)
			if err != nil {
				notes = append(notes, fmt.Sprintf("plugin skill %s: %v", resolvedPath, err))
				continue
			}
			notes = append(notes, skillNotes...)
			list = append(list, sk)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Key() < list[j].Key() })
	return list, notes
}

type skillCandidate struct {
	entryPath string
	dir       string
	path      string
	fallback  string
}

func additionalCandidates(root string) ([]skillCandidate, error) {
	rootDefinition := filepath.Join(root, "SKILL.md")
	if info, err := os.Stat(rootDefinition); err == nil {
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file", rootDefinition)
		}
		return []skillCandidate{{entryPath: root, dir: root, path: rootDefinition, fallback: filepath.Base(root)}}, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	entries, err := readSkillDirectory(root, maxSkillDirectoryEntries)
	if err != nil {
		return nil, err
	}
	var candidates []skillCandidate
	for _, entry := range entries {
		entryPath := filepath.Join(root, entry.Name())
		info, err := os.Stat(entryPath)
		if err != nil {
			if entry.Type()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("%s: %w", entryPath, err)
			}
			continue
		}
		if !info.IsDir() {
			continue
		}
		definition := filepath.Join(entryPath, "SKILL.md")
		definitionInfo, err := os.Stat(definition)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("%s: %w", definition, err)
			}
			continue
		}
		if !definitionInfo.Mode().IsRegular() {
			continue
		}
		candidates = append(candidates, skillCandidate{entryPath: entryPath, dir: entryPath, path: definition, fallback: entry.Name()})
	}
	return candidates, nil
}

func validateAdditionalRoot(root AdditionalRoot) error {
	if strings.TrimSpace(root.Path) == "" {
		return fmt.Errorf("path is empty")
	}
	if !filepath.IsAbs(root.Path) {
		return fmt.Errorf("path must be absolute")
	}
	if root.Namespace == "" || len(root.Namespace) > 256 || strings.TrimSpace(root.Namespace) != root.Namespace || strings.ContainsAny(root.Namespace, "/\\") {
		return fmt.Errorf("namespace must be a non-empty canonical plugin ID without whitespace or path separators")
	}
	for _, r := range root.Namespace {
		if unicode.IsSpace(r) || r < 0x20 || r == 0x7f {
			return fmt.Errorf("namespace contains whitespace or a control character")
		}
	}
	if root.Dialect != EcosystemCodex && root.Dialect != EcosystemClaude {
		return fmt.Errorf("dialect must be %q or %q", EcosystemCodex, EcosystemClaude)
	}
	switch root.Scope {
	case ScopeWorkspace, ScopeUser, ScopeLocal, ScopeManaged:
	default:
		return fmt.Errorf("unsupported scope %q", root.Scope)
	}
	return nil
}

func buildSkill(src source, entryPath, dir, path, fallback string, packed bool, resolvedDir, resolvedPath string) (Skill, []string, error) {
	data, implicit, metadataBlockers, rootInfo, err := readSkillFiles(resolvedDir, resolvedPath, packed)
	if err != nil {
		return Skill{}, nil, err
	}
	sk, manualOnly, err := parseDocumentForEcosystem(fallback, string(data), src.ecosystem)
	if err != nil {
		return Skill{}, nil, err
	}
	if packed {
		manualOnly = manualOnly || !implicit
	}
	sk.ImplicitDisabled = sk.ImplicitDisabled || manualOnly
	sk.InvocationBlockers = append(sk.InvocationBlockers, metadataBlockers...)
	if src.ecosystem == EcosystemClaude {
		sk.InvocationBlockers = append(sk.InvocationBlockers, claudeBodyBlockers(sk.Body)...)
	}
	sk.InvocationBlockers = uniqueSorted(sk.InvocationBlockers)

	sk.Dir = dir
	sk.rootDir = resolvedDir
	sk.rootInfo = rootInfo
	sk.FromHome = src.scope == ScopeUser
	sk.Origin = Origin{
		Ecosystem: src.ecosystem, Scope: src.scope, Namespace: src.namespace,
		LogicalPath: path, Path: resolvedPath,
	}
	if src.namespace != "" {
		sk.Selector = pluginSelector(src.namespace, sk.Name)
	} else {
		sk.Selector, err = canonicalSelector(src, entryPath)
		if err != nil {
			return Skill{}, nil, fmt.Errorf("selector: %w", err)
		}
	}
	var notes []string
	if len(sk.InvocationBlockers) > 0 {
		notes = append(notes, fmt.Sprintf("skill %s: invocation blocked: %s", resolvedPath, strings.Join(sk.InvocationBlockers, ", ")))
	}
	return sk, notes, nil
}

func pluginSelector(namespace, name string) string {
	return "plugin:" + escapeSelectorSegment(namespace) + ":" + escapeSelectorSegment(name)
}

func escapeSelectorSegment(value string) string {
	// PathEscape intentionally leaves ':' unescaped because it is legal in a
	// URL segment; ':' is our selector delimiter, so encode it explicitly.
	return strings.ReplaceAll(url.PathEscape(value), ":", "%3A")
}

// readSkillFiles anchors the definition and optional invocation metadata to
// one open directory handle. This keeps discovery internally consistent if a
// writable repository changes the directory while a session is assembling.
func readSkillFiles(rootDir, definitionPath string, packed bool) ([]byte, bool, []string, os.FileInfo, error) {
	root, err := rootedfs.OpenRoot(rootDir)
	if err != nil {
		return nil, false, nil, nil, err
	}
	defer root.Close()

	rootInfo, err := root.Stat(".")
	if err != nil {
		return nil, false, nil, nil, err
	}
	if !rootInfo.IsDir() {
		return nil, false, nil, nil, fmt.Errorf("skill root is not a directory")
	}
	rel, err := relativeWithin(rootDir, definitionPath)
	if err != nil {
		return nil, false, nil, nil, err
	}
	data, err := readFileFromRoot(root, rel, maxDefinitionBytes)
	if err != nil {
		return nil, false, nil, nil, err
	}
	implicit := true
	var blockers []string
	if packed {
		implicit, blockers, err = codexInvocationMetadataFromRoot(root)
		if err != nil {
			return nil, false, nil, nil, err
		}
	}
	return data, implicit, blockers, rootInfo, nil
}

func skillSources(workspace string) []source {
	roots := nativeProjectRoots(workspace)
	workspaceRoot, err := filepath.Abs(workspace)
	if err != nil {
		workspaceRoot = filepath.Clean(workspace)
	}
	repoRoot := workspaceRoot
	if len(roots) > 0 {
		repoRoot = roots[len(roots)-1]
	}
	switchboardDir := filepath.Join(workspaceRoot, ".switchboard", "skills")
	sources := []source{{
		dir:          switchboardDir,
		selectorRoot: switchboardDir,
		ecosystem:    EcosystemSwitchboard,
		scope:        ScopeWorkspace,
		flat:         true,
	}}
	for _, root := range roots {
		sources = append(sources,
			source{dir: filepath.Join(root, ".agents", "skills"), selectorRoot: repoRoot, ecosystem: EcosystemCodex, scope: ScopeWorkspace},
			source{dir: filepath.Join(root, ".claude", "skills"), selectorRoot: repoRoot, ecosystem: EcosystemClaude, scope: ScopeWorkspace},
		)
	}
	if home, err := os.UserHomeDir(); err == nil {
		switchboardHome := filepath.Join(home, ".switchboard", "skills")
		codexHome := filepath.Join(home, ".agents", "skills")
		claudeHome := filepath.Join(home, ".claude", "skills")
		sources = append(sources,
			source{dir: switchboardHome, selectorRoot: switchboardHome, ecosystem: EcosystemSwitchboard, scope: ScopeUser, flat: true},
			source{dir: codexHome, selectorRoot: codexHome, ecosystem: EcosystemCodex, scope: ScopeUser},
			source{dir: claudeHome, selectorRoot: claudeHome, ecosystem: EcosystemClaude, scope: ScopeUser},
		)
	}
	// Codex's administrator-managed skill root is deliberately consulted last
	// and retained as its own canonical origin. It is a Unix convention; on
	// Windows there is no /etc/codex path to interpret.
	if runtime.GOOS != "windows" {
		const managed = "/etc/codex/skills"
		sources = append(sources, source{
			dir: managed, selectorRoot: managed,
			ecosystem: EcosystemCodex, scope: ScopeManaged,
		})
	}
	return sources
}

func canonicalSelector(src source, entryPath string) (string, error) {
	rel, err := filepath.Rel(src.selectorRoot, entryPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("path is outside its selector root")
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	scope := "repo"
	switch src.scope {
	case ScopeUser:
		scope = "user"
	case ScopeManaged:
		scope = "admin"
	case ScopeLocal:
		scope = "local"
	}
	return string(src.ecosystem) + ":" + scope + ":" + strings.Join(parts, "/"), nil
}

// Key returns the canonical selector, falling back to the historical name
// for Skill literals constructed by existing callers.
func (s Skill) Key() string {
	if s.Selector != "" {
		return s.Selector
	}
	return s.Name
}

// ModelVisible returns a deterministic copy containing only skills whose
// metadata and body can be honored without weakening invocation controls.
func ModelVisible(list []Skill) []Skill {
	out := make([]Skill, 0, len(list))
	for _, sk := range list {
		if sk.ImplicitDisabled || len(sk.ModelBlockers) > 0 || len(sk.InvocationBlockers) > 0 {
			continue
		}
		if sk.Origin.Ecosystem == EcosystemClaude && len(claudeBodyBlockers(sk.Body)) > 0 {
			continue
		}
		out = append(out, sk)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

// nativeProjectRoots returns the workspace followed by its parents through
// the first Git root. Outside a repository only the workspace is consulted;
// opening an arbitrary directory must not discover prompts all the way to the
// filesystem root.
func nativeProjectRoots(workspace string) []string {
	root, err := filepath.Abs(workspace)
	if err != nil {
		return []string{filepath.Clean(workspace)}
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}

	var candidates []string
	for cur := root; ; cur = filepath.Dir(cur) {
		candidates = append(candidates, cur)
		if _, err := os.Lstat(filepath.Join(cur, ".git")); err == nil {
			return candidates
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return candidates[:1]
		}
	}
}

func resolveDefinition(dir, path string) (string, string, error) {
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", "", err
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", "", err
	}
	info, err := os.Stat(resolvedPath)
	if err != nil {
		return "", "", err
	}
	if !info.Mode().IsRegular() {
		return "", "", fmt.Errorf("definition is not a regular file")
	}
	if _, err := relativeWithin(resolvedDir, resolvedPath); err != nil {
		return "", "", fmt.Errorf("definition leaves its skill directory")
	}
	return resolvedDir, resolvedPath, nil
}

func readRootedFile(root, path string, limit int64) ([]byte, error) {
	rel, err := relativeWithin(root, path)
	if err != nil {
		return nil, err
	}
	opened, err := rootedfs.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer opened.Close()
	return readFileFromRoot(opened, rel, limit)
}

func readSkillDirectory(path string, maxEntries int) ([]os.DirEntry, error) {
	return readSkillDirectoryWithHook(path, maxEntries, nil)
}

func readSkillDirectoryWithHook(path string, maxEntries int, beforeOpen func()) ([]os.DirEntry, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	before, err := os.Lstat(resolved)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, fmt.Errorf("%s is not a real directory", path)
	}
	if beforeOpen != nil {
		beforeOpen()
	}
	directory, err := openSkillPathRead(resolved)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	opened, err := directory.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.IsDir() || !os.SameFile(before, opened) {
		return nil, fmt.Errorf("%s changed identity while it was opened", path)
	}
	entries, readErr := directory.ReadDir(maxEntries + 1)
	if readErr != nil && readErr != io.EOF {
		return nil, readErr
	}
	if len(entries) > maxEntries {
		return nil, fmt.Errorf("%s has more than %d entries", path, maxEntries)
	}
	finished, err := directory.Stat()
	if err != nil {
		return nil, err
	}
	linked, linkErr := os.Stat(path)
	if linkErr != nil || !linked.IsDir() || !os.SameFile(opened, finished) ||
		!os.SameFile(finished, linked) || !opened.ModTime().Equal(finished.ModTime()) {
		return nil, fmt.Errorf("%s changed while it was read", path)
	}
	return entries, nil
}

func readFileFromRoot(root *os.Root, rel string, limit int64) ([]byte, error) {
	return readFileFromRootWithHook(root, rel, limit, nil)
}

func readFileFromRootWithHook(root *os.Root, rel string, limit int64, beforeOpen func()) ([]byte, error) {
	info, err := root.Stat(rel)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", rel)
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("%s exceeds the %d-byte limit", rel, limit)
	}
	if beforeOpen != nil {
		beforeOpen()
	}
	file, err := openRootedRead(root, rel)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !openedInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", rel)
	}
	if !os.SameFile(info, openedInfo) {
		return nil, fmt.Errorf("%s changed while it was opened", rel)
	}
	if openedInfo.Size() > limit {
		return nil, fmt.Errorf("%s exceeds the %d-byte limit", rel, limit)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds the %d-byte limit", rel, limit)
	}
	afterFD, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !afterFD.Mode().IsRegular() || !os.SameFile(openedInfo, afterFD) ||
		openedInfo.Size() != afterFD.Size() || !openedInfo.ModTime().Equal(afterFD.ModTime()) ||
		int64(len(data)) != afterFD.Size() {
		return nil, fmt.Errorf("%s changed while it was read", rel)
	}
	afterPath, err := root.Stat(rel)
	if err != nil || !afterPath.Mode().IsRegular() || !os.SameFile(afterFD, afterPath) {
		return nil, fmt.Errorf("%s changed while it was read", rel)
	}
	return data, nil
}

func relativeWithin(root, path string) (string, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("path leaves root")
	}
	return rel, nil
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := values[:0]
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// parse reads one definition for callers that only need its instructions.
// Invocation controls are enforced by Load before a skill becomes visible.
func parse(fallbackName, content string) (Skill, error) {
	sk, _, err := parseDocument(fallbackName, content)
	return sk, err
}
