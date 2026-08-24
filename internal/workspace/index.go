package workspace

import (
	"bufio"
	"bytes"
	"container/heap"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/rootedfs"
	"github.com/switchboard-code/switchboard/internal/safeexec"
)

const (
	DefaultFileLimit    = 200_000
	DefaultSearchLimit  = 500
	DefaultSearchBytes  = 4 << 20
	defaultGitListBytes = 64 << 20
	defaultGitPathBytes = 64 << 10
	defaultGitListTime  = 10 * time.Second
	defaultWalkEntries  = 400_000
	defaultWalkDirs     = 50_000
	defaultWalkDepth    = 128
	defaultWalkBatch    = 256
)

var skippedDirectories = map[string]bool{
	".git": true, ".hg": true, ".svn": true,
	"node_modules": true, "vendor": true, "dist": true, "build": true,
	"target": true, ".venv": true, "__pycache__": true,
}

type File struct {
	Path string `json:"path"`
	Size int64  `json:"size"`

	searchKey string
	baseKey   string
}

type Snapshot struct {
	Generation uint64 `json:"generation"`
	Files      []File `json:"files"`
	Truncated  bool   `json:"truncated"`
	// Skipped counts individual entries or excluded subtrees observed but not
	// indexed. Truncated separately says the collector stopped before EOF.
	Skipped int `json:"skipped"`
}

func (s Snapshot) Clone() Snapshot {
	s.Files = append([]File(nil), s.Files...)
	return s
}

type FileMatch struct {
	File  File `json:"file"`
	Score int  `json:"score"`
}

// Filter is allocation-bounded by limit and performs no filesystem I/O, so
// it is safe to call for every command-palette keystroke.
func (s Snapshot) Filter(query string, limit int) []FileMatch {
	if limit <= 0 {
		limit = 50
	}
	query = strings.TrimSpace(strings.ToLower(query))
	matches := make(matchHeap, 0, min(limit, len(s.Files)))
	for _, file := range s.Files {
		key, base := file.searchKey, file.baseKey
		if key == "" {
			key = strings.ToLower(file.Path)
			base = strings.ToLower(filepath.Base(file.Path))
		}
		score, ok := fuzzyScore(query, key, base)
		if !ok {
			continue
		}
		match := FileMatch{File: file, Score: score}
		if len(matches) < limit {
			heap.Push(&matches, match)
		} else if betterMatch(match, matches[0]) {
			heap.Pop(&matches)
			heap.Push(&matches, match)
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		return betterMatch(matches[i], matches[j])
	})
	return []FileMatch(matches)
}

func fuzzyScore(query, candidate, base string) (int, bool) {
	if query == "" {
		return len(candidate), true
	}
	switch {
	case candidate == query:
		return 0, true
	case base == query:
		return 1, true
	case strings.HasPrefix(base, query):
		return 10 + len(base) - len(query), true
	case strings.HasPrefix(candidate, query):
		return 20 + len(candidate) - len(query), true
	case strings.Contains(base, query):
		return 40 + strings.Index(base, query), true
	case strings.Contains(candidate, query):
		return 60 + strings.Index(candidate, query), true
	}
	qi, gaps := 0, 0
	for i := 0; i < len(candidate) && qi < len(query); i++ {
		if candidate[i] == query[qi] {
			qi++
		} else if qi > 0 {
			gaps++
		}
	}
	if qi != len(query) {
		return 0, false
	}
	return 100 + gaps + len(candidate) - len(query), true
}

func betterMatch(a, b FileMatch) bool {
	if a.Score != b.Score {
		return a.Score < b.Score
	}
	if len(a.File.Path) != len(b.File.Path) {
		return len(a.File.Path) < len(b.File.Path)
	}
	return a.File.Path < b.File.Path
}

// matchHeap keeps the worst retained match at index zero, so Filter stores at
// most the requested number instead of allocating for every file in a large
// repository.
type matchHeap []FileMatch

func (h matchHeap) Len() int           { return len(h) }
func (h matchHeap) Less(i, j int) bool { return betterMatch(h[j], h[i]) }
func (h matchHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *matchHeap) Push(value any)    { *h = append(*h, value.(FileMatch)) }
func (h *matchHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

type Index struct {
	root *Root
	cap  int

	gitCommand   func(context.Context, ...string) *exec.Cmd
	gitListBytes int64
	walkLimits   rootedfs.WalkLimits

	mu       sync.RWMutex
	snapshot Snapshot
	dirty    bool
	next     atomic.Uint64
}

func NewIndex(root *Root, fileLimit int) *Index {
	if fileLimit <= 0 {
		fileLimit = DefaultFileLimit
	}
	return &Index{
		root: root, cap: fileLimit, dirty: true,
		gitListBytes: defaultGitListBytes,
		walkLimits: rootedfs.WalkLimits{
			MaxEntries: defaultWalkEntries, MaxDirectories: defaultWalkDirs,
			MaxDepth: defaultWalkDepth, ReadDirBatch: defaultWalkBatch,
		},
	}
}

func (i *Index) Snapshot() Snapshot {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.snapshot.Clone()
}

func (i *Index) Invalidate() {
	i.mu.Lock()
	i.dirty = true
	i.mu.Unlock()
}

func (i *Index) Refresh(ctx context.Context) (Snapshot, error) {
	files, truncated, skipped, err := i.list(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot := Snapshot{Generation: i.next.Add(1), Files: files, Truncated: truncated, Skipped: skipped}
	i.mu.Lock()
	i.snapshot = snapshot
	i.dirty = false
	i.mu.Unlock()
	return snapshot.Clone(), nil
}

func (i *Index) Ensure(ctx context.Context) (Snapshot, error) {
	i.mu.RLock()
	dirty := i.dirty
	snapshot := i.snapshot.Clone()
	i.mu.RUnlock()
	if !dirty && snapshot.Generation != 0 {
		return snapshot, nil
	}
	return i.Refresh(ctx)
}

func (i *Index) list(ctx context.Context) ([]File, bool, int, error) {
	if files, truncated, skipped, err := i.listGit(ctx); err == nil {
		return files, truncated, skipped, nil
	}
	return i.listWalk(ctx)
}

type gitListBudget struct {
	entries int
	bytes   int64
}

func (i *Index) listGit(ctx context.Context) ([]File, bool, int, error) {
	// Git's own untracked scan is outside the descriptor walker, so byte and
	// entry caps alone cannot bound CPU spent on ignored/non-output fanout.
	// Fall back to the rooted walker if it cannot produce an inventory promptly.
	gitCtx, cancel := context.WithTimeout(ctx, defaultGitListTime)
	defer cancel()
	capability, err := i.root.openCapability()
	if err != nil {
		return nil, false, 0, err
	}
	defer capability.Close()
	var git safeexec.Executable
	if i.gitCommand == nil {
		roots, rootsErr := safeexec.WorkspaceAndCurrentAuthorityRoots(i.root.Path())
		if rootsErr != nil {
			return nil, false, 0, fmt.Errorf("resolving Git workspace authority: %w", rootsErr)
		}
		git, err = safeexec.ResolveOutside("git", roots...)
		if err != nil {
			return nil, false, 0, fmt.Errorf("resolving trusted git executable: %w", err)
		}
	}
	seen := make(map[string]struct{}, min(i.cap, 4096))
	files := make([]File, 0, min(i.cap, 4096))
	budget := gitListBudget{}
	skipped := 0
	consume := func(raw []byte, tracked bool) {
		if len(raw) == 0 || !utf8.Valid(raw) {
			skipped++
			return
		}
		name := filepath.ToSlash(string(raw))
		// Conventional generated trees stay out of the untracked scan, but a
		// path Git already tracks is source and must remain searchable.
		if !tracked && excludedPath(name) {
			skipped++
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		info, err := capability.Lstat(filepath.FromSlash(name))
		if err != nil || !info.Mode().IsRegular() {
			skipped++
			return
		}
		seen[name] = struct{}{}
		files = append(files, indexedFile(name, info.Size()))
	}

	// ls-files consults core.fsmonitor and will execute a repository-configured
	// hook even though this is nominally an inventory read. Repository code may
	// not run merely because the user opened /files, so override that executable
	// config at the command line for both inventory passes.
	trackedArgs := []string{"-c", "core.fsmonitor=false", "-C", i.root.Path(), "ls-files", "--cached", "-z", "--"}
	truncated, omitted, err := i.streamGitNames(gitCtx, git, trackedArgs, &budget, func(raw []byte) {
		consume(raw, true)
	})
	skipped += omitted
	if err != nil {
		return nil, false, 0, err
	}
	if !truncated {
		untrackedArgs := []string{"-c", "core.fsmonitor=false", "-C", i.root.Path(), "ls-files", "--others", "--exclude-standard", "-z", "--"}
		truncated, omitted, err = i.streamGitNames(gitCtx, git, untrackedArgs, &budget, func(raw []byte) {
			consume(raw, false)
		})
		skipped += omitted
		if err != nil {
			return nil, false, 0, err
		}
	}
	if err := i.root.verifyCapability(capability); err != nil {
		return nil, false, 0, err
	}
	sort.Slice(files, func(a, b int) bool { return files[a].Path < files[b].Path })
	return files, truncated, skipped, nil
}

// streamGitNames consumes Git's NUL-delimited output without first retaining
// it all. The entry and byte budgets cover both tracked and untracked calls.
// Once either budget has evidence of more output, the child is killed and
// reaped; the retained prefix remains a usable, explicitly truncated snapshot.
func (i *Index) streamGitNames(
	ctx context.Context,
	git safeexec.Executable,
	args []string,
	budget *gitListBudget,
	consume func([]byte),
) (truncated bool, skipped int, err error) {
	var cmd *exec.Cmd
	if i.gitCommand != nil {
		cmd = i.gitCommand(ctx, args...)
	} else {
		var err error
		// execution.RunProcess owns cancellation so it can stop the complete
		// process group. CommandContext kills only Git itself; a wrapper's
		// descendant can otherwise retain stdout and keep Wait blocked forever.
		cmd, err = git.Command(args...)
		if err != nil {
			return false, 0, err
		}
	}
	if cmd.Env == nil {
		roots, rootsErr := safeexec.WorkspaceAndCurrentAuthorityRoots(i.root.Path())
		if rootsErr != nil {
			return false, 0, fmt.Errorf("resolving Git workspace authority: %w", rootsErr)
		}
		environ, envErr := safeexec.FilterEnvironmentPath(execution.ScrubbedChildEnv(), roots...)
		if envErr != nil {
			return false, 0, fmt.Errorf("preparing trusted Git interpreter path: %w", envErr)
		}
		cmd.Env = stableGitEnv(environ)
	} else {
		cmd.Env = append(cmd.Env, "GIT_OPTIONAL_LOCKS=0", "GIT_PAGER=cat", "LC_ALL=C")
	}
	// Feed the parser through Cmd.Stdout instead of reading StdoutPipe directly.
	// RunProcess can then wait for the parser's copy goroutine while owning the
	// process group and its cancellation. A blocking ReadSlice has no way to
	// observe ctx while a descendant retains the pipe after Git exits.
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	parser := gitNameStream{
		byteLimit:  i.gitListBytes,
		entryLimit: i.cap,
		budget:     budget,
		consume:    consume,
		stop:       stop,
	}
	cmd.Stdout = &parser
	runErr := execution.RunProcess(runCtx, cmd)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, parser.skipped, ctxErr
	}
	if parser.truncated {
		// The parser initiated this cancellation after proving there was more
		// output than the snapshot can retain. That is a successful truncated
		// inventory, not a failed Git command.
		return true, parser.skipped, nil
	}
	if runErr != nil {
		return false, parser.skipped, runErr
	}
	if parser.unterminated() {
		return false, parser.skipped, errors.New("git ls-files returned an unterminated path")
	}
	return false, parser.skipped, nil
}

// gitNameStream parses NUL-delimited names as os/exec copies stdout. It keeps
// only one bounded candidate name; cancellation on a cap means the runner can
// terminate and reap Git's entire process group instead of waiting for a
// wrapper descendant to close the pipe.
type gitNameStream struct {
	byteLimit  int64
	entryLimit int
	budget     *gitListBudget
	consume    func([]byte)
	stop       context.CancelFunc

	name          []byte
	oversizedName bool
	skipped       int
	truncated     bool
}

func (s *gitNameStream) Write(data []byte) (int, error) {
	for _, b := range data {
		if s.budget.bytes >= s.byteLimit {
			s.truncate()
			return len(data), nil
		}
		s.budget.bytes++
		if !s.oversizedName {
			if len(s.name) == defaultGitPathBytes {
				s.name = nil
				s.oversizedName = true
			} else {
				s.name = append(s.name, b)
			}
		}
		if b != 0 {
			continue
		}
		if s.budget.entries >= s.entryLimit {
			s.truncate()
			return len(data), nil
		}
		s.budget.entries++
		if s.oversizedName {
			s.skipped++
		} else {
			s.consume(s.name[:len(s.name)-1])
		}
		s.name = s.name[:0]
		s.oversizedName = false
	}
	return len(data), nil
}

func (s *gitNameStream) truncate() {
	if !s.truncated {
		s.truncated = true
		s.stop()
	}
}

func (s *gitNameStream) unterminated() bool {
	return len(s.name) != 0 || s.oversizedName
}

func stableGitEnv(environ []string) []string {
	overrides := map[string]string{
		"GIT_OPTIONAL_LOCKS": "0", "GIT_PAGER": "cat", "PAGER": "cat",
		"LC_ALL": "C", "LANG": "C", "GIT_TERMINAL_PROMPT": "0",
		"GIT_NO_LAZY_FETCH": "1",
	}
	remove := map[string]bool{
		"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_INDEX_FILE": true,
		"GIT_OBJECT_DIRECTORY": true, "GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
		"GIT_COMMON_DIR": true, "GIT_NAMESPACE": true, "GIT_PREFIX": true,
		"GIT_EXEC_PATH": true, "GIT_CEILING_DIRECTORIES": true,
		"GIT_DISCOVERY_ACROSS_FILESYSTEM": true, "GIT_CONFIG": true,
	}
	out := make([]string, 0, len(environ)+len(overrides))
	for _, entry := range environ {
		key, _, ok := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		if !ok || remove[upper] || strings.HasPrefix(upper, "GIT_CONFIG_") {
			continue
		}
		if _, replaced := overrides[upper]; replaced {
			continue
		}
		out = append(out, entry)
	}
	for key, value := range overrides {
		out = append(out, key+"="+value)
	}
	return out
}

func excludedPath(name string) bool {
	for _, part := range strings.Split(filepath.ToSlash(name), "/") {
		if skippedDirectories[part] {
			return true
		}
	}
	return false
}

func (i *Index) listWalk(ctx context.Context) ([]File, bool, int, error) {
	capability, err := i.root.openCapability()
	if err != nil {
		return nil, false, 0, err
	}
	defer capability.Close()
	files := make([]File, 0, min(i.cap, 4096))
	skipped := 0
	status, err := rootedfs.WalkRegularFiles(ctx, capability, ".", i.walkLimits,
		func(relative string, _ os.FileInfo) bool {
			if skippedDirectories[filepath.Base(relative)] {
				skipped++
				return true
			}
			return false
		},
		func(relative string, _ *os.Root, _ string, info os.FileInfo) error {
			if len(files) >= i.cap {
				return fs.SkipAll
			}
			files = append(files, indexedFile(filepath.ToSlash(relative), info.Size()))
			return nil
		})
	if err != nil {
		return nil, false, 0, err
	}
	if err := i.root.verifyCapability(capability); err != nil {
		return nil, false, 0, err
	}
	skipped += status.Omitted
	sort.Slice(files, func(a, b int) bool { return files[a].Path < files[b].Path })
	return files, status.Partial(), skipped, nil
}

func indexedFile(path string, size int64) File {
	key := strings.ToLower(path)
	return File{Path: path, Size: size, searchKey: key, baseKey: strings.ToLower(filepath.Base(path))}
}

type SearchOptions struct {
	Limit         int
	MaxFileBytes  int64
	CaseSensitive bool
}

type TextMatch struct {
	Location Location `json:"location"`
	Preview  string   `json:"preview"`
}

type SearchStatus struct {
	Truncated bool `json:"truncated"`
	// Skipped includes index omissions plus binary or disappeared files seen
	// while searching. Oversized is separate because its configured limit is
	// useful evidence to the caller.
	Skipped   int `json:"skipped"`
	Oversized int `json:"oversized"`
}

func (s SearchStatus) Partial() bool {
	return s.Truncated || s.Skipped > 0 || s.Oversized > 0
}

// Search performs a bounded literal search over the current file snapshot.
// It is intended to run in a tea.Cmd, never in Bubble Tea's Update method.
func (i *Index) Search(ctx context.Context, query string, options SearchOptions) ([]TextMatch, SearchStatus, error) {
	if strings.TrimSpace(query) == "" {
		return nil, SearchStatus{}, errors.New("search query is required")
	}
	if options.Limit <= 0 {
		options.Limit = DefaultSearchLimit
	}
	if options.MaxFileBytes <= 0 {
		options.MaxFileBytes = DefaultSearchBytes
	}
	expression := regexp.QuoteMeta(query)
	if !options.CaseSensitive {
		expression = "(?i:" + expression + ")"
	}
	matcher, err := regexp.Compile(expression)
	if err != nil {
		return nil, SearchStatus{}, err
	}
	snapshot, err := i.Ensure(ctx)
	if err != nil {
		return nil, SearchStatus{}, err
	}
	status := SearchStatus{Truncated: snapshot.Truncated, Skipped: snapshot.Skipped}
	for _, file := range snapshot.Files {
		if file.Size > options.MaxFileBytes {
			status.Oversized++
		}
	}

	type result struct {
		matches []TextMatch
		err     error
	}
	workers := min(max(runtime.GOMAXPROCS(0), 2), 8)
	jobs := make(chan File)
	results := make(chan result, workers)
	parentCtx := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for file := range jobs {
				matches, err := i.searchFile(ctx, file, matcher, options)
				select {
				case results <- result{matches: matches, err: err}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, file := range snapshot.Files {
			if file.Size > options.MaxFileBytes {
				continue
			}
			select {
			case jobs <- file:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	var matches []TextMatch
	for result := range results {
		switch {
		case result.err == nil:
		case errors.Is(result.err, ErrTooLarge):
			status.Oversized++
		case errors.Is(result.err, ErrBinary), errors.Is(result.err, fs.ErrNotExist):
			status.Skipped++
		case errors.Is(result.err, context.Canceled):
			// Reaching the match cap cancels the remaining workers. Truncated
			// already carries that incomplete-coverage evidence.
		default:
			cancel()
			return nil, SearchStatus{}, result.err
		}
		matches = append(matches, result.matches...)
		if len(matches) >= options.Limit {
			status.Truncated = true
			cancel()
		}
	}
	if err := parentCtx.Err(); err != nil {
		return nil, SearchStatus{}, err
	}
	sort.Slice(matches, func(a, b int) bool {
		if matches[a].Location.Path != matches[b].Location.Path {
			return matches[a].Location.Path < matches[b].Location.Path
		}
		if matches[a].Location.Range.Start.Line != matches[b].Location.Range.Start.Line {
			return matches[a].Location.Range.Start.Line < matches[b].Location.Range.Start.Line
		}
		return matches[a].Location.Range.Start.Column < matches[b].Location.Range.Start.Column
	})
	if len(matches) > options.Limit {
		matches = matches[:options.Limit]
		status.Truncated = true
	}
	return matches, status, nil
}

func (i *Index) searchFile(ctx context.Context, file File, matcher *regexp.Regexp, options SearchOptions) ([]TextMatch, error) {
	if file.Size > options.MaxFileBytes {
		return nil, ErrTooLarge
	}
	doc, err := i.root.Read(file.Path, options.MaxFileBytes)
	if err != nil {
		return nil, err
	}
	reader := bufio.NewReader(bytes.NewReader(doc.Content))
	lineNumber := 0
	var matches []TextMatch
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line, readErr := reader.ReadString('\n')
		if len(line) > 0 {
			lineNumber++
			text := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			remaining := options.Limit - len(matches)
			for _, bounds := range matcher.FindAllStringIndex(text, remaining) {
				byteCol := bounds[0]
				column := utf8.RuneCountInString(text[:byteCol]) + 1
				endColumn := column + utf8.RuneCountInString(text[bounds[0]:bounds[1]])
				loc := doc.Location
				loc.Range = Range{Start: Position{Line: lineNumber, Column: column}, End: Position{Line: lineNumber, Column: endColumn}}
				matches = append(matches, TextMatch{Location: loc, Preview: trimPreview(text, byteCol)})
			}
			if len(matches) >= options.Limit {
				return matches, nil
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	return matches, nil
}

func trimPreview(line string, focus int) string {
	const maxRunes = 240
	line = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\t' {
			return ' '
		}
		return r
	}, line)
	if utf8.RuneCountInString(line) <= maxRunes {
		return line
	}
	runes := []rune(line)
	focusRunes := utf8.RuneCountInString(line[:min(focus, len(line))])
	start := max(focusRunes-maxRunes/3, 0)
	end := min(start+maxRunes, len(runes))
	if end-start < maxRunes {
		start = max(end-maxRunes, 0)
	}
	text := string(runes[start:end])
	if start > 0 {
		text = "…" + text
	}
	if end < len(runes) {
		text += "…"
	}
	return text
}

func (i *Index) String() string {
	snapshot := i.Snapshot()
	return fmt.Sprintf("workspace index generation %d (%d files)", snapshot.Generation, len(snapshot.Files))
}
