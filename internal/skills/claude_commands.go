package skills

// Claude command files are the legacy, flat-file predecessor to Claude
// skills. Claude still discovers them recursively and exposes each file by
// its basename. Switchboard adapts them to explicit-only skills so the user
// can keep an existing command library without adding repository prompts to
// the model-visible tool schema.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

const (
	// These limits apply independently to each .claude/commands tree. A bad
	// repository tree therefore cannot consume the user's personal-tree
	// budget, and rejecting an over-limit tree avoids a filesystem-order-
	// dependent partial inventory.
	maxClaudeCommandDepth      = 16
	maxClaudeCommandEntries    = 512
	maxClaudeCommandTotalBytes = int64(16 << 20)
	commandReadDirBatch        = 64
)

var (
	errClaudeCommandEntryLimit = errors.New("Claude command entry limit exceeded")
	errClaudeCommandByteLimit  = errors.New("Claude command byte limit exceeded")
)

type claudeCommandSource struct {
	source
	logicalRoot  string
	resolvedRoot string
	rootInfo     os.FileInfo
}

type claudeCommandCandidate struct {
	logicalPath  string
	resolvedPath string
	name         string
	info         os.FileInfo
}

type claudeCommandConflictKey struct {
	scope Scope
	name  string
}

func loadClaudeCommands(workspace string, loadedSkills []Skill) (list []Skill, notes []string) {
	conflicts := claudeSkillCommandNames(loadedSkills)
	seenPaths := make(map[string]bool)

	for _, original := range claudeCommandSources(workspace) {
		src := original
		candidates, walkNotes, err := discoverClaudeCommandCandidates(&src)
		notes = append(notes, walkNotes...)
		if err != nil {
			if !os.IsNotExist(err) {
				notes = append(notes, fmt.Sprintf("Claude commands %s: %v", src.logicalRoot, err))
			}
			continue
		}

		remainingBytes := maxClaudeCommandTotalBytes
		sourceSeen := make(map[string]bool)
		var sourceSkills []Skill
		var sourceNotes []string
		limitExceeded := false
		for _, candidate := range candidates {
			if seenPaths[candidate.resolvedPath] {
				continue
			}

			if winners := conflicts[claudeCommandConflictKey{scope: src.scope, name: candidate.name}]; len(winners) > 0 {
				sourceNotes = append(sourceNotes, fmt.Sprintf(
					"Claude command %s: omitted because same-%s-scope skill %s wins native command name %q",
					candidate.logicalPath, src.scope, strings.Join(winners, ", "), candidate.name,
				))
				sourceSeen[candidate.resolvedPath] = true
				continue
			}

			if candidate.info == nil || !candidate.info.Mode().IsRegular() {
				sourceNotes = append(sourceNotes, fmt.Sprintf("Claude command %s: definition is not a regular file", candidate.logicalPath))
				continue
			}
			if candidate.info.Size() > maxDefinitionBytes {
				sourceNotes = append(sourceNotes, fmt.Sprintf(
					"Claude command %s: %s exceeds the %d-byte limit",
					candidate.logicalPath, filepath.Base(candidate.logicalPath), maxDefinitionBytes,
				))
				continue
			}
			if candidate.info.Size() > remainingBytes {
				limitExceeded = true
				break
			}

			data, err := readClaudeCommandDefinition(src, candidate)
			if err != nil {
				sourceNotes = append(sourceNotes, fmt.Sprintf("Claude command %s: %v", candidate.logicalPath, err))
				continue
			}
			if int64(len(data)) > remainingBytes {
				limitExceeded = true
				break
			}
			remainingBytes -= int64(len(data))

			sk, err := buildClaudeCommand(src, candidate, data)
			if err != nil {
				sourceNotes = append(sourceNotes, fmt.Sprintf("Claude command %s: %v", candidate.logicalPath, err))
				continue
			}
			sourceSeen[candidate.resolvedPath] = true
			if len(sk.InvocationBlockers) > 0 {
				sourceNotes = append(sourceNotes, fmt.Sprintf(
					"Claude command %s: invocation blocked: %s",
					candidate.logicalPath, strings.Join(sk.InvocationBlockers, ", "),
				))
			}
			sourceSkills = append(sourceSkills, sk)
		}
		if limitExceeded {
			// Reject the whole tree. Keeping a prefix would make a safety limit an
			// invisible precedence rule between commands later in lexical order.
			notes = append(notes, sourceNotes...)
			notes = append(notes, fmt.Sprintf(
				"Claude commands %s: %v (%d bytes per tree)",
				src.logicalRoot, errClaudeCommandByteLimit, maxClaudeCommandTotalBytes,
			))
			continue
		}
		for path := range sourceSeen {
			seenPaths[path] = true
		}
		list = append(list, sourceSkills...)
		notes = append(notes, sourceNotes...)
	}

	sort.Slice(list, func(i, j int) bool { return list[i].Key() < list[j].Key() })
	return list, notes
}

func claudeCommandSources(workspace string) []claudeCommandSource {
	roots := nativeProjectRoots(workspace)
	workspaceRoot, err := filepath.Abs(workspace)
	if err != nil {
		workspaceRoot = filepath.Clean(workspace)
	}
	repoRoot := workspaceRoot
	if len(roots) > 0 {
		repoRoot = roots[len(roots)-1]
	}

	var sources []claudeCommandSource
	for _, root := range roots {
		commands := filepath.Join(root, ".claude", "commands")
		sources = append(sources, claudeCommandSource{
			source: source{
				dir: commands, selectorRoot: repoRoot,
				ecosystem: EcosystemClaude, scope: ScopeWorkspace,
			},
			logicalRoot: commands,
		})
	}
	if home, err := os.UserHomeDir(); err == nil {
		commands := filepath.Join(home, ".claude", "commands")
		sources = append(sources, claudeCommandSource{
			source: source{
				dir: commands, selectorRoot: commands,
				ecosystem: EcosystemClaude, scope: ScopeUser,
			},
			logicalRoot: commands,
		})
	}
	return sources
}

func discoverClaudeCommandCandidates(src *claudeCommandSource) ([]claudeCommandCandidate, []string, error) {
	resolvedRoot, err := filepath.EvalSymlinks(src.logicalRoot)
	if err != nil {
		return nil, nil, err
	}
	info, err := os.Stat(resolvedRoot)
	if err != nil {
		return nil, nil, err
	}
	if !info.IsDir() {
		return nil, nil, fmt.Errorf("command root is not a directory")
	}
	src.resolvedRoot = resolvedRoot

	root, err := rootedfs.OpenRoot(resolvedRoot)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	src.rootInfo, err = root.Stat(".")
	if err != nil {
		return nil, nil, err
	}
	if !src.rootInfo.IsDir() {
		return nil, nil, fmt.Errorf("command root is not a directory")
	}

	walker := claudeCommandWalker{
		root: root, logicalRoot: src.logicalRoot, resolvedRoot: resolvedRoot,
		remainingEntries: maxClaudeCommandEntries,
		visitedDirs:      make(map[string]bool),
		seenFiles:        make(map[string]bool),
	}
	if err := walker.walk(src.logicalRoot, resolvedRoot, 0); err != nil {
		if errors.Is(err, errClaudeCommandEntryLimit) {
			return nil, walker.notes, fmt.Errorf("%w (%d entries per tree)", err, maxClaudeCommandEntries)
		}
		return nil, walker.notes, err
	}
	sort.Slice(walker.candidates, func(i, j int) bool {
		return walker.candidates[i].logicalPath < walker.candidates[j].logicalPath
	})
	return walker.candidates, walker.notes, nil
}

type claudeCommandWalker struct {
	root             *os.Root
	logicalRoot      string
	resolvedRoot     string
	remainingEntries int
	visitedDirs      map[string]bool
	seenFiles        map[string]bool
	candidates       []claudeCommandCandidate
	notes            []string
}

func (w *claudeCommandWalker) walk(logicalDir, resolvedDir string, depth int) error {
	resolvedDir = filepath.Clean(resolvedDir)
	if w.visitedDirs[resolvedDir] {
		return nil
	}
	w.visitedDirs[resolvedDir] = true

	rel, err := relativeWithin(w.resolvedRoot, resolvedDir)
	if err != nil {
		return fmt.Errorf("command directory %s leaves its command root", logicalDir)
	}
	entries, err := readCommandDir(w.root, rel, &w.remainingEntries)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		logicalPath := filepath.Join(logicalDir, entry.Name())
		resolvedPath, err := filepath.EvalSymlinks(logicalPath)
		if err != nil {
			if entry.Type()&os.ModeSymlink != 0 {
				w.notes = append(w.notes, fmt.Sprintf("Claude command path %s: unsafe or broken symlink: %v", logicalPath, err))
			}
			continue
		}
		resolvedRel, err := relativeWithin(w.resolvedRoot, resolvedPath)
		if err != nil {
			w.notes = append(w.notes, fmt.Sprintf("Claude command path %s: symlink leaves its command root", logicalPath))
			continue
		}
		info, err := w.root.Stat(resolvedRel)
		if err != nil {
			w.notes = append(w.notes, fmt.Sprintf("Claude command path %s: %v", logicalPath, err))
			continue
		}
		switch {
		case info.IsDir():
			if depth >= maxClaudeCommandDepth {
				w.notes = append(w.notes, fmt.Sprintf(
					"Claude command directory %s: exceeds the %d-directory depth limit",
					logicalPath, maxClaudeCommandDepth,
				))
				continue
			}
			if err := w.walk(logicalPath, resolvedPath, depth+1); err != nil {
				return err
			}
		case info.Mode().IsRegular() && strings.HasSuffix(entry.Name(), ".md"):
			resolvedPath = filepath.Clean(resolvedPath)
			if w.seenFiles[resolvedPath] {
				continue
			}
			w.seenFiles[resolvedPath] = true
			w.candidates = append(w.candidates, claudeCommandCandidate{
				logicalPath:  logicalPath,
				resolvedPath: resolvedPath,
				name:         strings.TrimSuffix(entry.Name(), ".md"),
				info:         info,
			})
		case entry.Type()&os.ModeSymlink != 0:
			w.notes = append(w.notes, fmt.Sprintf("Claude command path %s: symlink target is not a regular file or directory", logicalPath))
		}
	}
	return nil
}

// readCommandDir bounds directory enumeration before it can select a partial
// lexical prefix. os.File.ReadDir's underlying order is unspecified, so an
// over-limit tree is rejected in full; successful trees are sorted here.
func readCommandDir(root *os.Root, rel string, remaining *int) ([]os.DirEntry, error) {
	dir, err := openRootedRead(root, rel)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	info, err := dir.Stat()
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", rel)
	}

	var entries []os.DirEntry
	for {
		want := commandReadDirBatch
		if *remaining < want {
			want = *remaining + 1
		}
		batch, readErr := dir.ReadDir(want)
		if len(batch) > *remaining {
			return nil, errClaudeCommandEntryLimit
		}
		*remaining -= len(batch)
		entries = append(entries, batch...)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
		if len(batch) == 0 {
			break
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}

// readClaudeCommandDefinition reopens the canonical tree, proves it is still
// the tree that discovery walked, and then proves the file is still the same
// filesystem object. os.Root keeps both checks and the read confined if a
// writable repository races discovery by replacing a symlink or directory.
func readClaudeCommandDefinition(src claudeCommandSource, candidate claudeCommandCandidate) ([]byte, error) {
	return readClaudeCommandDefinitionWithHook(src, candidate, nil)
}

func readClaudeCommandDefinitionWithHook(src claudeCommandSource, candidate claudeCommandCandidate, beforeOpen func()) ([]byte, error) {
	root, err := rootedfs.OpenRoot(src.resolvedRoot)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	rootInfo, err := root.Stat(".")
	if err != nil {
		return nil, err
	}
	if src.rootInfo == nil || !os.SameFile(src.rootInfo, rootInfo) {
		return nil, fmt.Errorf("command root changed after discovery")
	}
	rel, err := relativeWithin(src.resolvedRoot, candidate.resolvedPath)
	if err != nil {
		return nil, err
	}
	if beforeOpen != nil {
		beforeOpen()
	}
	file, err := openRootedRead(root, rel)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if candidate.info == nil || !os.SameFile(candidate.info, info) {
		return nil, fmt.Errorf("definition changed after discovery")
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("definition is not a regular file")
	}
	if info.Size() > maxDefinitionBytes {
		return nil, fmt.Errorf("%s exceeds the %d-byte limit", rel, maxDefinitionBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxDefinitionBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxDefinitionBytes {
		return nil, fmt.Errorf("%s exceeds the %d-byte limit", rel, maxDefinitionBytes)
	}
	afterFD, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !afterFD.Mode().IsRegular() || !os.SameFile(info, afterFD) ||
		info.Size() != afterFD.Size() || !info.ModTime().Equal(afterFD.ModTime()) ||
		int64(len(data)) != afterFD.Size() {
		return nil, fmt.Errorf("definition changed while it was read")
	}
	afterPath, err := root.Stat(rel)
	if err != nil || !afterPath.Mode().IsRegular() || !os.SameFile(afterFD, afterPath) {
		return nil, fmt.Errorf("definition changed while it was read")
	}
	return data, nil
}

func buildClaudeCommand(src claudeCommandSource, candidate claudeCommandCandidate, data []byte) (Skill, error) {
	sk, _, err := parseClaudeCommandDocument(candidate.name, string(data))
	if err != nil {
		return Skill{}, err
	}
	// The path, not frontmatter, is the native command identity. Commands are
	// always explicit-only even if their legacy metadata would let Claude
	// invoke them in its own host.
	sk.Name = candidate.name
	sk.ImplicitDisabled = true
	sk.InvocationBlockers = append(sk.InvocationBlockers, claudeBodyBlockers(sk.Body)...)
	sk.InvocationBlockers = uniqueSorted(sk.InvocationBlockers)
	sk.Dir = filepath.Dir(candidate.logicalPath)
	sk.FromHome = src.scope == ScopeUser
	sk.Origin = Origin{
		Ecosystem:   EcosystemClaude,
		Scope:       src.scope,
		LogicalPath: candidate.logicalPath,
		Path:        candidate.resolvedPath,
	}
	sk.Selector, err = canonicalSelector(src.source, candidate.logicalPath)
	if err != nil {
		return Skill{}, fmt.Errorf("selector: %w", err)
	}
	return sk, nil
}

func claudeSkillCommandNames(list []Skill) map[claudeCommandConflictKey][]string {
	// Scope is part of the key deliberately. Switchboard keeps cross-scope
	// definitions explicitly selectable instead of importing Claude's silent
	// personal-over-project precedence. Within one native scope, however, a
	// skill and command are two spellings of the same slash name and the skill
	// wins just as it does in Claude.
	conflicts := make(map[claudeCommandConflictKey][]string)
	for _, sk := range list {
		name, ok := nativeClaudeSkillCommandName(sk)
		if !ok {
			continue
		}
		key := claudeCommandConflictKey{scope: sk.Origin.Scope, name: name}
		conflicts[key] = append(conflicts[key], sk.Key())
	}
	for key := range conflicts {
		sort.Strings(conflicts[key])
	}
	return conflicts
}

func nativeClaudeSkillCommandName(sk Skill) (string, bool) {
	if sk.Origin.Ecosystem != EcosystemClaude || sk.Origin.Namespace != "" {
		return "", false
	}
	logical := sk.Origin.LogicalPath
	if logical == "" || filepath.Base(logical) != "SKILL.md" {
		return "", false
	}
	name := filepath.Base(filepath.Dir(logical))
	if name == "." || name == string(filepath.Separator) || name == "" {
		return "", false
	}
	return name, true
}
