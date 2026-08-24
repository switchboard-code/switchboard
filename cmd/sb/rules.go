package main

// .switchboard/rules/*.md: instructions that arrive when they become relevant.
//
// Everything a repository wants to say currently goes into one file that rides
// the frozen zone, paid for on every cold cache whether or not the session
// ever touches what it is about. So a long AGENTS.md is the tax for having
// rules about the migration directory, the generated protobufs, and the one
// service with a different test command — and the pressure is always to say
// less than is true.
//
// A rule here names the paths it is about and costs nothing until the session
// touches one. What counts as touching is evidence the loop already keeps: the
// registry's recorded reads and the checkpoint recorder's captured mutations.
// Neither is a guess about what a turn is "really" about, which is the same
// standard the escalation triggers hold to.
//
// It is a round-boundary message and never a system block. The frozen zone has
// to render byte-identically for a whole session, so a rule that appeared in
// it would invalidate the prefix the moment it fired; as a message it is
// append-only, which is what the cache wants anyway.
//
// The limit is real and stated rather than glossed: a rule fires after the
// model has already read or written the file. A rule that should have stopped
// an edit arrives after it. That makes this good for "when you touch these,
// remember X" and useless as a guard, and a guard is what hooks and the
// permission engine are for.

import (
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/provider"
)

const (
	// rulesDir is under .switchboard because it is Switchboard's own
	// convention. Reading another tool's rules directory would mean adopting
	// its activation semantics too, and those are not written down.
	rulesDir = ".switchboard/rules"

	maxRuleFiles            = 32
	maxRuleBytes            = 8 << 10
	maxRuleDirectoryEntries = 256

	// maxRulesPerSession bounds what the whole session can inject this way. A
	// checkout with thirty rules that all match on the first turn would be the
	// long AGENTS.md it was meant to replace, arriving later.
	maxRulesPerSession = 8
)

// pathRule is one file: the globs it is about and what it wants said.
type pathRule struct {
	label string
	globs []string
	body  string
}

type ruleSet struct {
	mu    sync.Mutex
	rules []pathRule
	fired map[string]bool
	count int

	// workspace is kept for matching, since a rule's globs are written
	// workspace-relative and the evidence arrives as absolute paths.
	workspace string
}

// resetSession starts a fresh delivery ledger without reloading repository
// rules. The rule definitions belong to the workspace, but fired and count
// describe what one session has already received; carrying them across an
// adopted log can silently withhold instructions from its model context.
func (s *ruleSet) resetSession() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fired = make(map[string]bool)
	s.count = 0
}

// loadRules reads the rules directory once, at assembly, sorted by name.
//
// It needs no trust grant for the reason a skill needs none: nothing executes
// at read time, a rule is a prompt, and whatever it persuades the model to do
// passes the permission engine on its own merits.
func loadRules(workspace string) (*ruleSet, []string) {
	set := &ruleSet{fired: map[string]bool{}, workspace: filepath.Clean(workspace)}
	dir := filepath.Join(workspace, filepath.FromSlash(rulesDir))
	entries, err := readWorkspaceDirectoryBounded(workspace, dir, maxRuleDirectoryEntries, nil)
	if err != nil {
		if os.IsNotExist(err) {
			return set, nil
		}
		return set, []string{fmt.Sprintf("%s was not loaded: %v", rulesDir, err)}
	}

	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".md") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)

	var notes []string
	if len(names) > maxRuleFiles {
		notes = append(notes, fmt.Sprintf("%s holds %d rules; only the first %d by name are loaded",
			rulesDir, len(names), maxRuleFiles))
		names = names[:maxRuleFiles]
	}
	for _, name := range names {
		rule, err := readRule(workspace, filepath.Join(dir, name))
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s/%s was not loaded: %v", rulesDir, name, err))
			continue
		}
		set.rules = append(set.rules, rule)
	}
	return set, notes
}

// readRule parses the frontmatter and body. The format is deliberately two
// keys and no more: anything that looked like a program would want a runtime,
// and this is a file that says a paragraph when a path is touched.
func readRule(workspace, path string) (pathRule, error) {
	return readRuleWithHook(workspace, path, nil)
}

func readRuleWithHook(workspace, path string, beforeOpen func()) (pathRule, error) {
	data, err := readWorkspaceFileBounded(workspace, path, maxRuleBytes, beforeOpen)
	if err != nil {
		return pathRule{}, err
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return pathRule{}, fmt.Errorf("it has no frontmatter; a rule needs a paths: list")
	}
	end := strings.Index(text[4:], "\n---")
	if end < 0 {
		return pathRule{}, fmt.Errorf("its frontmatter is not closed")
	}
	front, body := text[4:4+end], text[4+end:]
	if cut := strings.Index(body, "\n"); cut >= 0 {
		body = body[cut+1:]
	}
	body = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(body), "---"))

	rule := pathRule{label: filepath.Base(path), body: body}
	for _, line := range strings.Split(front, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), ":")
		if !found || strings.TrimSpace(key) != "paths" {
			continue
		}
		for _, glob := range strings.Split(strings.Trim(strings.TrimSpace(value), "[]"), ",") {
			glob = strings.Trim(strings.TrimSpace(glob), `"'`)
			if glob != "" {
				rule.globs = append(rule.globs, glob)
			}
		}
	}
	if len(rule.globs) == 0 {
		return pathRule{}, fmt.Errorf("it names no paths, so it would never fire")
	}
	if strings.TrimSpace(rule.body) == "" {
		return pathRule{}, fmt.Errorf("it has no body, so it would have nothing to say")
	}
	return rule, nil
}

// matched reports the rules whose globs cover any of the touched paths and
// which have not fired yet. Each fires once: a session that keeps editing one
// directory should not be told the same paragraph on every round.
func (s *ruleSet) matched(paths []string) []pathRule {
	if s == nil || len(s.rules) == 0 || len(paths) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.count >= maxRulesPerSession {
		return nil
	}

	relative := make([]string, 0, len(paths))
	for _, path := range paths {
		rel, err := filepath.Rel(s.workspace, path)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		relative = append(relative, filepath.ToSlash(rel))
	}

	var out []pathRule
	for _, rule := range s.rules {
		if s.fired[rule.label] || s.count+len(out) >= maxRulesPerSession {
			continue
		}
		if !ruleCovers(rule, relative) {
			continue
		}
		s.fired[rule.label] = true
		out = append(out, rule)
	}
	s.count += len(out)
	return out
}

// ruleCovers matches a glob against the path and against each of its leading
// directories, so "migrations/*" covers "migrations/2026/up.sql" the way a
// person writing it expects. path.Match alone does not cross a separator.
func ruleCovers(rule pathRule, paths []string) bool {
	for _, glob := range rule.globs {
		for _, path := range paths {
			if matchRuleGlob(glob, path) {
				return true
			}
		}
	}
	return false
}

func matchRuleGlob(glob, path string) bool {
	glob = strings.TrimSuffix(filepath.ToSlash(glob), "/")
	if ok, err := pathpkg.Match(glob, path); err == nil && ok {
		return true
	}
	// A directory-shaped glob covers everything under it.
	prefix := strings.TrimSuffix(glob, "/**")
	prefix = strings.TrimSuffix(prefix, "/*")
	if prefix != glob || !strings.ContainsAny(glob, "*?[") {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	for dir := path; ; {
		parent := filepathDir(dir)
		if parent == dir {
			return false
		}
		if ok, err := pathpkg.Match(glob, parent); err == nil && ok {
			return true
		}
		dir = parent
	}
}

func filepathDir(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return p
}

// ruleRound is the injection seam's half. The evidence is the registry's
// recorded reads and the recorder's captured mutations, both of which the loop
// already keeps for other reasons.
func (a *tuiApp) ruleRound() []provider.Message {
	if a.rules == nil || a.loop == nil || a.loop.Tools == nil {
		return nil
	}
	touched := a.loop.Tools.ReadPaths()
	if a.undo != nil {
		if snapshot, _, ok := a.undo.CurrentSnapshot(); ok {
			for _, file := range snapshot.Files {
				touched = append(touched, file.Path)
			}
		}
	}

	fired := a.rules.matched(touched)
	if len(fired) == 0 {
		return nil
	}

	var b strings.Builder
	b.WriteString("This repository keeps instructions that apply to files you have now touched:\n")
	for _, rule := range fired {
		fmt.Fprintf(&b, "\n%s (%s):\n%s\n", rule.label, strings.Join(rule.globs, ", "), rule.body)
	}
	// Machine-composed and injected with nobody to ask, so it redacts
	// unconditionally, the same posture as a watch report.
	text := b.String()
	if leaks := credential.ScanPrompt(text); len(leaks) > 0 {
		text = credential.Redact(text, leaks)
	}
	return []provider.Message{provider.UserText(text)}
}
