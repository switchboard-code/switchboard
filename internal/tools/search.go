package tools

import (
	"bufio"
	"container/heap"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"regexp/syntax"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

// The caps below bound what a search can put into the context. A search that
// returns everything it found is a search that evicts the conversation that
// asked for it, so over-large results truncate with a note saying how to
// narrow, and the note is part of the contract rather than an apology.
const (
	maxGlobResults   = 500
	maxGlobOutput    = 64 << 10
	maxGrepMatches   = 200
	maxGrepOutput    = 64 << 10
	maxGrepFileSize  = 4 << 20
	maxGrepLine      = 500
	maxGrepScanFiles = 10_000
	maxGrepScanBytes = 64 << 20
	maxGrepRegexWork = 256 << 20

	// These are work caps, not result caps. They keep a nonmatching attacker-
	// controlled tree from making a search allocate or enumerate forever.
	maxSearchWalkEntries = 100_000
	maxSearchWalkDirs    = 10_000
	maxSearchWalkDepth   = 64
	searchReadDirBatch   = 256
	maxGlobPatternBytes  = 4 << 10
	maxGlobSegments      = 256
	maxGlobPathSegments  = maxSearchWalkDepth + 1
	maxGrepPatternBytes  = 4 << 10
	maxSearchPathBytes   = 4 << 10
	maxSearchModeBytes   = 16
	maxGlobMatchWork     = 25_000_000
)

func defaultSearchWalkLimits() rootedfs.WalkLimits {
	return rootedfs.WalkLimits{
		MaxEntries:     maxSearchWalkEntries,
		MaxDirectories: maxSearchWalkDirs,
		MaxDepth:       maxSearchWalkDepth,
		ReadDirBatch:   searchReadDirBatch,
	}
}

// walkFiles visits physical regular files under base. The retained parent
// capability handed to visit is the only authority a content reader should
// use; reopening relative through root could follow a parent renamed during
// the walk. .git is a policy exclusion; symlinks and special files are
// observed omissions and therefore make coverage explicitly partial.
func (r *Registry) walkFiles(
	ctx context.Context,
	root *os.Root,
	base string,
	limits rootedfs.WalkLimits,
	visit func(relative string, parent *os.Root, name string, info fs.FileInfo) error,
) (rootedfs.WalkStatus, error) {
	return rootedfs.WalkRegularFiles(ctx, root, base, limits,
		func(relative string, _ fs.FileInfo) bool { return filepath.Base(relative) == ".git" },
		visit,
	)
}

func searchCoverageNote(status rootedfs.WalkStatus, oversized int, limits rootedfs.WalkLimits, extra ...string) string {
	var reasons []string
	if status.EntryLimit {
		reasons = append(reasons, fmt.Sprintf("the %d-entry traversal limit was reached", limits.MaxEntries))
	}
	if status.DirectoryLimit {
		reasons = append(reasons, fmt.Sprintf("the %d-directory traversal limit was reached", limits.MaxDirectories))
	}
	if status.DepthLimit {
		reasons = append(reasons, fmt.Sprintf("the %d-level traversal limit was reached", limits.MaxDepth))
	}
	if status.Omitted > 0 {
		reasons = append(reasons, fmt.Sprintf("%d entries changed or could not be inspected", status.Omitted))
	}
	if oversized > 0 {
		reasons = append(reasons, fmt.Sprintf("%d files exceeded the %d-byte search limit", oversized, maxGrepFileSize))
	}
	reasons = append(reasons, extra...)
	if len(reasons) == 0 {
		return ""
	}
	return "[partial search: " + strings.Join(reasons, "; ") + "; narrow path and run again]"
}

func searchAuthorityChanged() (Result, error) {
	return errorf("workspace paths changed during search; no search results were returned")
}

func checkSearchInputBytes(field, value string, limit int) error {
	if len(value) > limit {
		return fmt.Errorf("%s is %d bytes; limit is %d", field, len(value), limit)
	}
	return nil
}

func safeSearchComponent(text string) string {
	return credential.Redact(text, credential.ScanPrompt(text))
}

func appendSearchLine(builder *strings.Builder, line string, limit int) bool {
	line = safeSearchComponent(line)
	required := len(line)
	if builder.Len() > 0 {
		required++
	}
	if required > limit-builder.Len() {
		return false
	}
	if builder.Len() > 0 {
		builder.WriteByte('\n')
	}
	builder.WriteString(line)
	return true
}

func regexpInstructionCount(expression string) (int, error) {
	parsed, err := syntax.Parse(expression, syntax.Perl)
	if err != nil {
		return 0, err
	}
	program, err := syntax.Compile(parsed.Simplify())
	if err != nil {
		return 0, err
	}
	return max(1, len(program.Inst)), nil
}

// matchGlob reports whether a slash-separated relative path matches pattern.
// Segments match with path.Match; a segment of ** matches any number of
// segments, including none. A pattern with no slash matches the base name
// anywhere in the tree, because that is what every caller who writes *.go
// means, and making them write **/*.go teaches nothing.
func matchGlob(pattern, rel string) (bool, error) {
	compiled, err := compileGlob(pattern)
	if err != nil {
		return false, err
	}
	matched, _, err := compiled.match(rel, nil)
	return matched, err
}

func matchSegments(pat, segs []string) (bool, error) {
	if len(pat) > maxGlobSegments {
		return false, fmt.Errorf("glob has %d path segments; limit is %d", len(pat), maxGlobSegments)
	}
	if len(segs) > maxGlobPathSegments {
		return false, fmt.Errorf("candidate has %d path segments; limit is %d", len(segs), maxGlobPathSegments)
	}
	for _, segment := range pat {
		if segment == "**" {
			continue
		}
		if _, err := path.Match(segment, ""); err != nil {
			return false, err
		}
	}
	return matchCompiledSegments(pat, segs)
}

type compiledGlob struct {
	basename      bool
	segments      []string
	patternBytes  int
	segmentWeight int64
}

func compileGlob(pattern string) (compiledGlob, error) {
	segments, err := checkedGlobSegments(pattern)
	if err != nil {
		return compiledGlob{}, err
	}
	compiled := compiledGlob{
		basename:     !strings.Contains(pattern, "/"),
		segments:     segments,
		patternBytes: len(pattern),
	}
	for _, segment := range segments {
		if segment != "**" {
			compiled.segmentWeight += int64(len(segment) + 1)
		}
	}
	return compiled, nil
}

func (g compiledGlob) match(relative string, remainingWork *int64) (matched, limited bool, err error) {
	if g.basename {
		base := path.Base(relative)
		// path.Match may retry a wildcard chunk at each name byte. Charge the
		// full pattern/name product before calling it rather than treating two
		// attacker-controlled lengths as additive work.
		cost := int64(g.patternBytes+1) * int64(len(base)+1)
		if remainingWork != nil && cost > *remainingWork {
			return false, true, nil
		}
		if remainingWork != nil {
			*remainingWork -= cost
		}
		matched, err := path.Match(g.segments[0], base)
		return matched, false, err
	}
	segments := strings.Split(relative, "/")
	if len(segments) > maxGlobPathSegments {
		return false, false, fmt.Errorf("candidate has %d path segments; limit is %d", len(segments), maxGlobPathSegments)
	}
	candidateWeight := int64(0)
	for _, segment := range segments {
		candidateWeight += int64(len(segment) + 1)
	}
	// The product bounds path.Match work if ** makes every candidate segment
	// reachable from every ordinary pattern segment. The DP state term covers
	// the ** transitions themselves.
	cost := g.segmentWeight*candidateWeight +
		int64(len(g.segments)*(len(segments)+1)+g.patternBytes+len(relative))
	if remainingWork != nil && cost > *remainingWork {
		return false, true, nil
	}
	if remainingWork != nil {
		*remainingWork -= cost
	}
	matched, err = matchCompiledSegments(g.segments, segments)
	return matched, false, err
}

func matchCompiledSegments(pat, segs []string) (bool, error) {
	// dp[n] means the pattern prefix consumed exactly n path segments. A **
	// either consumes none (dp[n]) or one more (next[n-1]); each state is
	// evaluated once, avoiding the exponential branch tree of recursion. The
	// two rows are reused so an adversarial pattern cannot cause one allocation
	// per segment for every candidate in a large directory.
	dp := make([]bool, len(segs)+1)
	next := make([]bool, len(segs)+1)
	dp[0] = true
	for _, patternSegment := range pat {
		clear(next)
		if patternSegment == "**" {
			next[0] = dp[0]
			for n := 1; n <= len(segs); n++ {
				next[n] = dp[n] || next[n-1]
			}
		} else {
			for n := 0; n < len(segs); n++ {
				if !dp[n] {
					continue
				}
				matched, err := path.Match(patternSegment, segs[n])
				if err != nil {
					return false, err
				}
				next[n+1] = matched
			}
		}
		dp, next = next, dp
	}
	return dp[len(segs)], nil
}

// checkGlob validates a pattern at Plan time, so a malformed one is reported
// as a bad call instead of surfacing as zero matches.
func checkGlob(pattern string) error {
	_, err := compileGlob(pattern)
	return err
}

func checkedGlobSegments(pattern string) ([]string, error) {
	if pattern == "" {
		return nil, fmt.Errorf("pattern is required")
	}
	if len(pattern) > maxGlobPatternBytes {
		return nil, fmt.Errorf("pattern is %d bytes; limit is %d", len(pattern), maxGlobPatternBytes)
	}
	segments := strings.Split(pattern, "/")
	if len(segments) > maxGlobSegments {
		return nil, fmt.Errorf("pattern has %d path segments; limit is %d", len(segments), maxGlobSegments)
	}
	for _, segment := range segments {
		if segment == "**" {
			continue
		}
		if _, err := path.Match(segment, "x"); err != nil {
			return nil, fmt.Errorf("invalid glob pattern")
		}
	}
	return segments, nil
}

type globTool struct{ r *Registry }

type maxStringHeap []string

func (h maxStringHeap) Len() int           { return len(h) }
func (h maxStringHeap) Less(i, j int) bool { return h[i] > h[j] }
func (h maxStringHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *maxStringHeap) Push(value any)    { *h = append(*h, value.(string)) }
func (h *maxStringHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

func retainLexicalPrefix(values *maxStringHeap, candidate string, limit int) (overLimit bool) {
	if values.Len() < limit {
		heap.Push(values, candidate)
		return false
	}
	if candidate < (*values)[0] {
		heap.Pop(values)
		heap.Push(values, candidate)
	}
	return true
}

func (t *globTool) Name() string { return "glob" }

func (t *globTool) Description() string {
	return "Find workspace files by name. A pattern without a slash matches file names " +
		"anywhere: *.go finds every Go file. A pattern with a slash matches the whole " +
		"workspace-relative path, with ** crossing directories: internal/**/*_test.go. " +
		"Results are sorted paths, and incomplete filesystem coverage is labeled. " +
		"Scope with path to search one directory."
}

func (t *globTool) ParallelSafe() bool { return true }

func (t *globTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {"type": "string", "description": "Glob pattern. Without a slash it matches file names anywhere; with a slash it matches the path relative to the searched directory, and ** matches across directories."},
    "path": {"type": "string", "description": "Directory to search, relative to the workspace root. Defaults to the whole workspace."}
  },
  "required": ["pattern"]
}`)
}

type globInput struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

func (t *globTool) Plan(input json.RawMessage) (Plan, error) {
	var in globInput
	if err := json.Unmarshal(input, &in); err != nil {
		return Plan{}, fmt.Errorf("glob: %w", err)
	}
	if err := checkGlob(in.Pattern); err != nil {
		return Plan{}, fmt.Errorf("glob: %w", err)
	}
	if err := checkSearchInputBytes("path", in.Path, maxSearchPathBytes); err != nil {
		return Plan{}, fmt.Errorf("glob: %w", err)
	}
	base := in.Path
	if base == "" {
		base = "."
	}
	abs, err := t.r.resolve(base)
	if err != nil {
		return Plan{}, fmt.Errorf("glob: path is not a workspace path")
	}

	return Plan{
		Request: permission.Request{Tool: t.Name(), Effect: permission.EffectRead, Path: t.r.display(abs)},
		Run: func(ctx context.Context) (Result, error) {
			return t.glob(ctx, abs, in.Pattern)
		},
	}, nil
}

func (t *globTool) glob(ctx context.Context, base, pattern string) (Result, error) {
	return t.globWithHook(ctx, base, pattern, nil)
}

func (t *globTool) globWithHook(ctx context.Context, base, pattern string, beforeWalk func()) (Result, error) {
	return t.globWithLimits(ctx, base, pattern, beforeWalk, defaultSearchWalkLimits())
}

func (t *globTool) globWithLimits(ctx context.Context, base, pattern string, beforeWalk func(), limits rootedfs.WalkLimits) (Result, error) {
	return t.globWithWorkLimit(ctx, base, pattern, beforeWalk, limits, maxGlobMatchWork)
}

func (t *globTool) globWithWorkLimit(ctx context.Context, base, pattern string, beforeWalk func(), limits rootedfs.WalkLimits, matchWorkLimit int64) (Result, error) {
	if matchWorkLimit <= 0 {
		return errorf("glob work limit is invalid")
	}
	compiled, err := compileGlob(pattern)
	if err != nil {
		return errorf("glob pattern is invalid")
	}
	root, baseRelative, err := t.r.openResolvedWorkspace(base)
	if err != nil {
		return errorf("%s is not a searchable directory", safeSearchComponent(t.r.display(base)))
	}
	defer root.Close()
	if info, err := root.Lstat(baseRelative); err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errorf("%s is not a searchable directory", safeSearchComponent(t.r.display(base)))
	}
	if beforeWalk != nil {
		beforeWalk()
	}

	matches := &maxStringHeap{}
	heap.Init(matches)
	resultTruncated := false
	matchWorkLimited := false
	remainingMatchWork := matchWorkLimit
	coverage, err := t.r.walkFiles(ctx, root, baseRelative, limits, func(relative string, _ *os.Root, _ string, _ fs.FileInfo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(baseRelative, relative)
		if err != nil {
			return nil
		}
		ok, limited, err := compiled.match(filepath.ToSlash(rel), &remainingMatchWork)
		if limited {
			matchWorkLimited = true
			return fs.SkipAll
		}
		if err != nil || !ok {
			return err
		}
		candidate := safeSearchComponent(t.r.display(filepath.Join(t.r.root, relative)))
		resultTruncated = retainLexicalPrefix(matches, candidate, maxGlobResults) || resultTruncated
		return nil
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Result{}, ctxErr
		}
		return errorf("could not safely enumerate %s", safeSearchComponent(t.r.display(base)))
	}
	if coverage.AuthorityChanged || t.r.verifyWorkspaceRoot(root) != nil {
		return searchAuthorityChanged()
	}
	var extra []string
	if matchWorkLimited {
		extra = append(extra, fmt.Sprintf("the %d-unit glob matching work limit was reached", matchWorkLimit))
	}
	note := searchCoverageNote(coverage, 0, limits, extra...)

	if matches.Len() == 0 {
		content := fmt.Sprintf("no files match %s under %s", safeSearchComponent(pattern), safeSearchComponent(t.r.display(base)))
		if note != "" {
			content += "\n" + note
		}
		return Result{Content: content}, nil
	}
	ordered := append([]string(nil), (*matches)...)
	sort.Strings(ordered)

	var b strings.Builder
	outputTruncated := false
	for _, match := range ordered {
		if !appendSearchLine(&b, match, maxGlobOutput) {
			outputTruncated = true
			break
		}
	}
	if resultTruncated {
		fmt.Fprintf(&b, "\n[first %d matches; narrow the pattern or set path]", maxGlobResults)
	}
	if outputTruncated {
		b.WriteString("\n[output limit reached; narrow the pattern or set path]")
	}
	if note != "" {
		b.WriteByte('\n')
		b.WriteString(note)
	}
	return Result{Content: b.String()}, nil
}

type grepTool struct{ r *Registry }

func (t *grepTool) Name() string { return "grep" }

func (t *grepTool) Description() string {
	return "Search file contents with a regular expression (Go RE2 syntax). Returns " +
		"matching lines as path:line: text; mode \"files\" lists only the files that " +
		"match. Scope with path (a directory or one file) and glob (a file name " +
		"pattern). Binary files and .git are skipped, and incomplete coverage is labeled."
}

func (t *grepTool) ParallelSafe() bool { return true }

func (t *grepTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {"type": "string", "description": "Regular expression, Go RE2 syntax."},
    "path": {"type": "string", "description": "Directory or file to search, relative to the workspace root. Defaults to the whole workspace."},
    "glob": {"type": "string", "description": "Only search files matching this glob, e.g. *.go or src/**/*.ts."},
    "ignore_case": {"type": "boolean", "description": "Match case-insensitively."},
    "mode": {"type": "string", "enum": ["content", "files"], "description": "content (default) returns matching lines; files returns one line per matching file with its match count."}
  },
  "required": ["pattern"]
}`)
}

type grepInput struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	IgnoreCase bool   `json:"ignore_case"`
	Mode       string `json:"mode"`
}

func (t *grepTool) Plan(input json.RawMessage) (Plan, error) {
	var in grepInput
	if err := json.Unmarshal(input, &in); err != nil {
		return Plan{}, fmt.Errorf("grep: %w", err)
	}
	if in.Pattern == "" {
		return Plan{}, fmt.Errorf("grep: pattern is required")
	}
	if err := checkSearchInputBytes("pattern", in.Pattern, maxGrepPatternBytes); err != nil {
		return Plan{}, fmt.Errorf("grep: %w", err)
	}
	if err := checkSearchInputBytes("path", in.Path, maxSearchPathBytes); err != nil {
		return Plan{}, fmt.Errorf("grep: %w", err)
	}
	if err := checkSearchInputBytes("glob", in.Glob, maxGlobPatternBytes); err != nil {
		return Plan{}, fmt.Errorf("grep: %w", err)
	}
	if err := checkSearchInputBytes("mode", in.Mode, maxSearchModeBytes); err != nil {
		return Plan{}, fmt.Errorf("grep: %w", err)
	}
	if in.Glob != "" {
		if err := checkGlob(in.Glob); err != nil {
			return Plan{}, fmt.Errorf("grep: %w", err)
		}
	}
	switch in.Mode {
	case "", "content", "files":
	default:
		return Plan{}, fmt.Errorf("grep: mode must be content or files")
	}
	expr := in.Pattern
	if in.IgnoreCase {
		expr = "(?i)" + expr
	}
	if _, err := regexpInstructionCount(expr); err != nil {
		return Plan{}, fmt.Errorf("grep: invalid regular expression")
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return Plan{}, fmt.Errorf("grep: invalid regular expression")
	}
	base := in.Path
	if base == "" {
		base = "."
	}
	abs, err := t.r.resolve(base)
	if err != nil {
		return Plan{}, fmt.Errorf("grep: path is not a workspace path")
	}

	return Plan{
		Request: permission.Request{Tool: t.Name(), Effect: permission.EffectRead, Path: t.r.display(abs)},
		Run: func(ctx context.Context) (Result, error) {
			return t.grep(ctx, abs, re, in)
		},
	}, nil
}

// grepHit is one file's outcome, kept so the files mode can report counts
// without a second pass.
type grepHit struct {
	display string
	count   int
	lines   []string
}

type grepWorkLimits struct {
	MaxFiles     int
	MaxBytes     int64
	MaxRegexWork int64
	MaxGlobWork  int64
}

func defaultGrepWorkLimits() grepWorkLimits {
	return grepWorkLimits{
		MaxFiles:     maxGrepScanFiles,
		MaxBytes:     maxGrepScanBytes,
		MaxRegexWork: maxGrepRegexWork,
		MaxGlobWork:  maxGlobMatchWork,
	}
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func (t *grepTool) grep(ctx context.Context, base string, re *regexp.Regexp, in grepInput) (Result, error) {
	return t.grepWithHook(ctx, base, re, in, nil, nil)
}

func (t *grepTool) grepWithHook(ctx context.Context, base string, re *regexp.Regexp, in grepInput, beforeWalk, beforeOpen func()) (Result, error) {
	return t.grepWithLimits(ctx, base, re, in, beforeWalk, beforeOpen, defaultSearchWalkLimits())
}

func (t *grepTool) grepWithLimits(
	ctx context.Context,
	base string,
	re *regexp.Regexp,
	in grepInput,
	beforeWalk, beforeOpen func(),
	limits rootedfs.WalkLimits,
) (Result, error) {
	return t.grepWithWorkLimits(ctx, base, re, in, beforeWalk, beforeOpen, limits, defaultGrepWorkLimits())
}

func (t *grepTool) grepWithWorkLimits(
	ctx context.Context,
	base string,
	re *regexp.Regexp,
	in grepInput,
	beforeWalk, beforeOpen func(),
	limits rootedfs.WalkLimits,
	workLimits grepWorkLimits,
) (Result, error) {
	if workLimits.MaxFiles <= 0 || workLimits.MaxBytes <= 0 || workLimits.MaxRegexWork <= 0 || workLimits.MaxGlobWork <= 0 {
		return errorf("grep work limits are invalid")
	}
	expression := in.Pattern
	if in.IgnoreCase {
		expression = "(?i)" + expression
	}
	regexInstructions, complexityErr := regexpInstructionCount(expression)
	if complexityErr != nil {
		return errorf("grep pattern is invalid")
	}
	regexByteLimit := workLimits.MaxRegexWork / int64(regexInstructions)
	effectiveByteLimit := min(workLimits.MaxBytes, regexByteLimit)
	regexWorkBound := regexByteLimit < workLimits.MaxBytes
	var filter compiledGlob
	if in.Glob != "" {
		compiled, compileErr := compileGlob(in.Glob)
		if compileErr != nil {
			return errorf("grep glob is invalid")
		}
		filter = compiled
	}
	root, baseRelative, err := t.r.openResolvedWorkspace(base)
	if err != nil {
		return errorf("cannot safely search %s", safeSearchComponent(t.r.display(base)))
	}
	defer root.Close()
	info, err := root.Lstat(baseRelative)
	if err != nil {
		return errorf("cannot safely search %s", safeSearchComponent(t.r.display(base)))
	}
	if beforeWalk != nil {
		beforeWalk()
	}

	var hits []grepHit
	budget := maxGrepMatches
	oversized := 0
	scanOmitted := 0
	coverage := rootedfs.WalkStatus{}
	scannedFiles := 0
	remainingScanBytes := effectiveByteLimit
	remainingGlobWork := workLimits.MaxGlobWork
	scanFileLimited := false
	scanByteLimited := false
	regexWorkLimited := false
	globWorkLimited := false

	scanOne := func(relative string, parent *os.Root, name string, expected fs.FileInfo) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if budget <= 0 {
			return true, nil
		}
		if expected == nil || !expected.Mode().IsRegular() {
			scanOmitted++
			return false, nil
		}
		if expected.Size() > maxGrepFileSize {
			oversized++
			return false, nil
		}
		if scannedFiles >= workLimits.MaxFiles {
			scanFileLimited = true
			return true, nil
		}
		if expected.Size() < 0 || expected.Size() > remainingScanBytes {
			if regexWorkBound {
				regexWorkLimited = true
			} else {
				scanByteLimited = true
			}
			return true, nil
		}
		scannedFiles++
		remainingScanBytes -= expected.Size()
		outcome := t.scanBoundFile(ctx, parent, name, relative, expected, re, budget, expected.Size(), beforeOpen)
		if outcome.err != nil {
			return false, outcome.err
		}
		if outcome.oversized {
			oversized++
		}
		if outcome.omitted {
			scanOmitted++
		}
		if outcome.hit.count == 0 {
			return false, nil
		}
		budget -= len(outcome.hit.lines)
		hits = append(hits, outcome.hit)
		return false, nil
	}

	if info.IsDir() {
		coverage, err = t.r.walkFiles(ctx, root, baseRelative, limits, func(relative string, parent *os.Root, name string, expected fs.FileInfo) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if budget <= 0 {
				return fs.SkipAll
			}
			if in.Glob != "" {
				rel, err := filepath.Rel(baseRelative, relative)
				if err != nil {
					return err
				}
				ok, limited, err := filter.match(filepath.ToSlash(rel), &remainingGlobWork)
				if limited {
					globWorkLimited = true
					return fs.SkipAll
				}
				if err != nil || !ok {
					return err
				}
			}
			stop, scanErr := scanOne(relative, parent, name, expected)
			if scanErr != nil {
				return scanErr
			}
			if stop {
				return fs.SkipAll
			}
			return nil
		})
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return Result{}, ctxErr
			}
			return errorf("could not safely enumerate %s", safeSearchComponent(t.r.display(base)))
		}
	} else {
		parentRelative := filepath.Dir(baseRelative)
		parent, parentInfo, bindErr := bindWorkspaceParent(root, parentRelative, false)
		if bindErr != nil {
			return errorf("cannot safely search %s", safeSearchComponent(t.r.display(base)))
		}
		leaf := filepath.Base(baseRelative)
		linked, linkErr := parent.Lstat(leaf)
		if linkErr != nil || linked.Mode()&os.ModeSymlink != 0 || !linked.Mode().IsRegular() {
			_ = parent.Close()
			return errorf("%s is not a searchable regular file", safeSearchComponent(t.r.display(base)))
		}
		_, scanErr := scanOne(baseRelative, parent, leaf, linked)
		parentStable := t.r.verifyWorkspaceParent(root, parentRelative, parentInfo) == nil
		_ = parent.Close()
		if scanErr != nil {
			return Result{}, scanErr
		}
		if !parentStable {
			return searchAuthorityChanged()
		}
	}
	coverage.Omitted += scanOmitted
	if coverage.AuthorityChanged || t.r.verifyWorkspaceRoot(root) != nil {
		return searchAuthorityChanged()
	}
	var extra []string
	if scanFileLimited {
		extra = append(extra, fmt.Sprintf("the %d-file content scan limit was reached", workLimits.MaxFiles))
	}
	if scanByteLimited {
		extra = append(extra, fmt.Sprintf("the %d-byte aggregate content scan limit was reached", workLimits.MaxBytes))
	}
	if regexWorkLimited {
		extra = append(extra, fmt.Sprintf("the %d-unit regular-expression work limit was reached", workLimits.MaxRegexWork))
	}
	if globWorkLimited {
		extra = append(extra, fmt.Sprintf("the %d-unit glob matching work limit was reached", workLimits.MaxGlobWork))
	}
	note := searchCoverageNote(coverage, oversized, limits, extra...)

	if len(hits) == 0 {
		content := fmt.Sprintf("no matches for %s in %s", safeSearchComponent(in.Pattern), safeSearchComponent(t.r.display(base)))
		if note != "" {
			content += "\n" + note
		}
		return Result{Content: content}, nil
	}

	sort.Slice(hits, func(i, j int) bool { return hits[i].display < hits[j].display })
	var b strings.Builder
	outputTruncated := false
	if in.Mode == "files" {
		for _, h := range hits {
			if !appendSearchLine(&b, fmt.Sprintf("%s (%d)", h.display, h.count), maxGrepOutput) {
				outputTruncated = true
				break
			}
		}
	} else {
	contentLoop:
		for _, h := range hits {
			for _, line := range h.lines {
				if !appendSearchLine(&b, line, maxGrepOutput) {
					outputTruncated = true
					break contentLoop
				}
			}
		}
	}
	if outputTruncated {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("[output limit reached; narrow the pattern, set path, or use mode files]")
	}
	if budget <= 0 {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "[first %d matching lines; narrow the pattern, set path, or use mode files]", maxGrepMatches)
	}
	if note != "" {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(note)
	}
	return Result{Content: b.String()}, nil
}

type grepScanOutcome struct {
	hit       grepHit
	omitted   bool
	oversized bool
	err       error
}

// scanFile reports up to lineBudget matching lines from one file. It remains
// as the low-level test seam; tree search uses scanBoundFile with the retained
// parent capability supplied by rootedfs.WalkRegularFiles.
func (t *grepTool) scanFile(root *os.Root, relative string, re *regexp.Regexp, lineBudget int) grepHit {
	return t.scanFileWithHook(root, relative, re, lineBudget, nil)
}

func (t *grepTool) scanFileWithHook(root *os.Root, relative string, re *regexp.Regexp, lineBudget int, beforeOpen func()) grepHit {
	parentRelative := filepath.Dir(relative)
	parent, _, err := bindWorkspaceParent(root, parentRelative, false)
	if err != nil {
		return grepHit{}
	}
	defer parent.Close()
	name := filepath.Base(relative)
	info, err := parent.Lstat(name)
	if err != nil {
		return grepHit{}
	}
	return t.scanBoundFile(context.Background(), parent, name, relative, info, re, lineBudget, info.Size(), beforeOpen).hit
}

func (t *grepTool) scanBoundFile(
	ctx context.Context,
	parent *os.Root,
	name, workspaceRelative string,
	expected fs.FileInfo,
	re *regexp.Regexp,
	lineBudget int,
	readLimit int64,
	beforeOpen func(),
) grepScanOutcome {
	if err := ctx.Err(); err != nil {
		return grepScanOutcome{err: err}
	}
	if expected == nil || !expected.Mode().IsRegular() {
		return grepScanOutcome{omitted: true}
	}
	if expected.Size() > maxGrepFileSize {
		return grepScanOutcome{oversized: true}
	}
	display := safeSearchComponent(t.r.display(filepath.Join(t.r.root, workspaceRelative)))
	f, opened, err := openRegularWorkspaceFile(parent, name, display, expected, beforeOpen)
	if err != nil {
		return grepScanOutcome{omitted: true}
	}
	defer f.Close()
	if opened.Size() > maxGrepFileSize {
		return grepScanOutcome{oversized: true}
	}
	if opened.Size() != expected.Size() || !opened.ModTime().Equal(expected.ModTime()) || opened.Size() > readLimit {
		return grepScanOutcome{omitted: true}
	}

	hit := grepHit{display: display}
	limited := &io.LimitedReader{R: contextReader{ctx: ctx, reader: f}, N: readLimit + 1}
	sc := bufio.NewScanner(limited)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	lineno := 0
	binary := false
	for sc.Scan() {
		lineno++
		line := sc.Text()
		if strings.IndexByte(line, 0) >= 0 {
			binary = true
			break
		}
		if !re.MatchString(line) {
			continue
		}
		hit.count++
		if len(hit.lines) >= lineBudget {
			continue
		}
		// A match is one semantic component. Redact it whole before the display
		// cap so the cap cannot cut a credential below its detection floor.
		line = credential.Redact(line, credential.ScanPrompt(line))
		if len(line) > maxGrepLine {
			line = truncateValidUTF8Bytes(line, maxGrepLine) + "…"
		}
		hit.lines = append(hit.lines, fmt.Sprintf("%s:%d: %s", hit.display, lineno, line))
	}
	scanErr := sc.Err()
	if err := ctx.Err(); err != nil {
		return grepScanOutcome{err: err}
	}
	if err := validateRegularWorkspaceFile(parent, name, display, f, opened, opened.Size()); err != nil {
		return grepScanOutcome{omitted: true}
	}
	if limited.N == 0 {
		return grepScanOutcome{omitted: true}
	}
	if binary {
		return grepScanOutcome{omitted: true}
	}
	return grepScanOutcome{hit: hit, omitted: scanErr != nil}
}

func truncateValidUTF8Bytes(text string, limit int) string {
	text = strings.ToValidUTF8(text, "�")
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.ValidString(text[:limit]) {
		limit--
	}
	return text[:limit]
}
