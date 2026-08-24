package lsp

import (
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/switchboard-code/switchboard/internal/credential"
)

const (
	maxProblemsPerDocument = 1_000
	maxProblemsTotal       = 10_000
	maxProblemMessageBytes = 4_096
	maxRelatedPerProblem   = 32
	maxProblemSourceBytes  = 256
	maxProblemCodeBytes    = 256
)

// Severity is the severity assigned by the language server. The values are
// the values on the LSP wire; zero means that the server did not assign one.
type Severity uint8

const (
	SeverityError       Severity = 1
	SeverityWarning     Severity = 2
	SeverityInformation Severity = 3
	SeverityHint        Severity = 4
)

// Freshness says what the store can prove about a document's problems.
type Freshness uint8

const (
	// Pending preserves the last result while the server catches up with a
	// newer local document version.
	Pending Freshness = iota
	// Fresh means a versioned publication agrees with the client document, or
	// no client document version was available to contradict it.
	Fresh
	// Stale preserves the last result after the language-server runtime became
	// unavailable.
	Stale
	// Unversioned is a current publication for which the server supplied no
	// document version, so exact freshness cannot be proved.
	Unversioned
)

// RelatedProblem is one location the server attached to a diagnostic.
// Positions are 1-based for the same reason Location lines are 1-based: the
// value is ready to render as file:line:column, but columns are UTF-16 code
// units and editor navigation must convert when its text model uses another
// indexing convention.
type RelatedProblem struct {
	URI       string
	Path      string
	Navigable bool
	Line      int
	Column    int
	EndLine   int
	EndColumn int
	Message   string
}

// Problem is one language-server diagnostic. URI is always the original wire
// value. Path is populated only when that URI names a file below the store's
// canonical workspace root, and Navigable makes that boundary explicit.
// Positions are 1-based; columns retain the LSP UTF-16 unit.
type Problem struct {
	URI       string
	Path      string
	Navigable bool
	Line      int
	Column    int
	EndLine   int
	EndColumn int
	Severity  Severity
	Code      string
	Source    string
	Message   string

	Related        []RelatedProblem
	RelatedDropped int
}

// DocumentProblems is the latest accepted replacement set for one URI.
// Version is the server publication's version. CurrentVersion is the client
// document version against which that publication was judged.
type DocumentProblems struct {
	URI            string
	Path           string
	Navigable      bool
	Version        *int
	CurrentVersion *int
	Freshness      Freshness
	Problems       []Problem
	Dropped        int
}

// ProblemFilter narrows a snapshot. Empty fields match everything. Path may
// be absolute or workspace-relative. Severity and freshness lists are sets.
type ProblemFilter struct {
	URI           string
	Path          string
	Severities    []Severity
	Freshness     []Freshness
	NavigableOnly bool
}

// ProblemSnapshot is an immutable, deterministic view of the store.
// ProtocolIssues counts rejected malformed or future-version publications;
// LastProtocolIssue is bounded and safe to display in runtime status.
type ProblemSnapshot struct {
	Generation        uint64
	Available         bool
	Documents         []DocumentProblems
	Total             int
	Dropped           int
	ProtocolIssues    uint64
	LastProtocolIssue string
}

type storedDocumentProblems struct {
	uri            string
	path           string
	navigable      bool
	version        *int
	currentVersion *int
	freshness      Freshness
	problems       []Problem
	dropped        int
}

// ProblemStore owns the latest diagnostics published by one language-server
// runtime. All methods are safe to call concurrently.
type ProblemStore struct {
	root string

	mu                sync.Mutex
	documents         map[string]*storedDocumentProblems
	total             int
	generation        uint64
	available         bool
	protocolIssues    uint64
	lastProtocolIssue string
	subscribers       map[uint64]chan uint64
	nextSubscriber    uint64
}

// NewProblemStore creates an empty, available store rooted at root. The root
// is made absolute and symlink-resolved when possible; a diagnostic never
// becomes navigable merely through lexical ".." containment.
func NewProblemStore(root string) *ProblemStore {
	return &ProblemStore{
		root:        canonicalRoot(root),
		documents:   make(map[string]*storedDocumentProblems),
		available:   true,
		subscribers: make(map[uint64]chan uint64),
	}
}

// problemPublish is the package-private seam used by the protocol reader.
// Problems carry 1-based positions; URI, Path, and Navigable are overwritten
// from the publication URI so untrusted server data cannot grant navigation.
type problemPublish struct {
	URI                 string
	Version             *int
	Problems            []Problem
	CurrentVersion      int
	CurrentVersionKnown bool
}

// publish atomically replaces one URI's accepted result set. An older
// publication is ignored. A publication ahead of a known client version is a
// protocol issue and is both returned and recorded, without changing results.
func (s *ProblemStore) publish(update problemPublish) error {
	version := cloneInt(update.Version)
	path, navigable := s.pathForURI(update.URI)

	s.mu.Lock()
	defer s.mu.Unlock()

	if update.URI == "" {
		return s.protocolIssueLocked("publishDiagnostics omitted its URI")
	}

	existing := s.documents[update.URI]
	if version != nil && update.CurrentVersionKnown {
		switch {
		case *version < update.CurrentVersion:
			return nil
		case *version > update.CurrentVersion:
			return s.protocolIssueLocked(fmt.Sprintf(
				"publishDiagnostics for %s reported future version %d; client version is %d",
				update.URI, *version, update.CurrentVersion))
		}
	}
	if version != nil && existing != nil && existing.version != nil && *version < *existing.version {
		return nil
	}

	// Receiving a valid notification proves that the runtime is reachable.
	// Other documents stay stale until their own replacement arrives.
	becameAvailable := !s.available
	s.available = true

	if len(update.Problems) == 0 {
		if existing == nil {
			if becameAvailable {
				s.changedLocked()
			}
			return nil
		}
		s.total -= len(existing.problems)
		delete(s.documents, update.URI)
		s.changedLocked()
		return nil
	}

	limit := len(update.Problems)
	if limit > maxProblemsPerDocument {
		limit = maxProblemsPerDocument
	}
	capacity := maxProblemsTotal - s.total
	if existing != nil {
		capacity += len(existing.problems)
	}
	if limit > capacity {
		limit = capacity
	}
	if limit < 0 {
		limit = 0
	}

	// A full store does not retain dropped-only document metadata. That keeps
	// the store bounded even when a misbehaving server publishes infinitely
	// many distinct URIs.
	if limit == 0 {
		if existing == nil {
			if becameAvailable {
				s.changedLocked()
			}
			return nil
		}
		s.total -= len(existing.problems)
		delete(s.documents, update.URI)
		s.changedLocked()
		return nil
	}

	problems := make([]Problem, limit)
	for i := 0; i < limit; i++ {
		problems[i] = s.normalizeProblem(update.URI, path, navigable, update.Problems[i])
	}
	sortProblems(problems)

	currentVersion := (*int)(nil)
	if update.CurrentVersionKnown {
		currentVersion = intPointer(update.CurrentVersion)
	}
	freshness := Fresh
	if version == nil {
		freshness = Unversioned
	}

	if existing != nil {
		s.total -= len(existing.problems)
	}
	s.documents[update.URI] = &storedDocumentProblems{
		uri:            update.URI,
		path:           path,
		navigable:      navigable,
		version:        version,
		currentVersion: currentVersion,
		freshness:      freshness,
		problems:       problems,
		dropped:        len(update.Problems) - limit,
	}
	s.total += len(problems)
	s.changedLocked()
	return nil
}

// invalidate records a newer local document version. Existing results remain
// visible as pending while the live runtime catches up. There is deliberately
// no didClose analogue: closing an editor view is not evidence that the last
// published diagnostics became false.
func (s *ProblemStore) invalidate(uri string, version int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	document := s.documents[uri]
	if document == nil {
		return
	}
	if document.currentVersion != nil && version <= *document.currentVersion {
		return
	}
	document.currentVersion = intPointer(version)
	if s.available {
		document.freshness = Pending
	} else {
		document.freshness = Stale
	}
	s.changedLocked()
}

// reopen starts a new didOpen version epoch for one URI. Last-known problems
// stay visible as pending, but their old server version must not prevent a
// fresh version 1 publication from replacing or clearing them.
func (s *ProblemStore) reopen(uri string, version int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	document := s.documents[uri]
	if document == nil {
		return
	}
	document.version = nil
	document.currentVersion = intPointer(version)
	if s.available {
		document.freshness = Pending
	} else {
		document.freshness = Stale
	}
	s.changedLocked()
}

// unavailable preserves every result while marking the runtime and documents
// stale. Repeating the same transition is a no-op.
func (s *ProblemStore) unavailable() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.available {
		return
	}
	s.available = false
	for _, document := range s.documents {
		document.freshness = Stale
	}
	s.changedLocked()
}

// available records that a runtime completed initialize. Existing stale
// results remain stale until that new runtime publishes a replacement.
func (s *ProblemStore) markAvailable() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.available {
		return
	}
	s.available = true
	s.changedLocked()
}

// Snapshot returns a deep copy. Documents and diagnostics have a stable sort
// independent of map iteration and publication order.
func (s *ProblemStore) Snapshot(filter ProblemFilter) ProblemSnapshot {
	pathFilter := s.canonicalFilterPath(filter.Path)
	severityFilter := severitySet(filter.Severities)
	freshnessFilter := freshnessSet(filter.Freshness)

	s.mu.Lock()
	defer s.mu.Unlock()

	snapshot := ProblemSnapshot{
		Generation:        s.generation,
		Available:         s.available,
		ProtocolIssues:    s.protocolIssues,
		LastProtocolIssue: s.lastProtocolIssue,
	}
	for _, stored := range s.documents {
		if filter.URI != "" && stored.uri != filter.URI {
			continue
		}
		if pathFilter != "" && stored.path != pathFilter {
			continue
		}
		if filter.NavigableOnly && !stored.navigable {
			continue
		}
		if len(freshnessFilter) != 0 && !freshnessFilter[stored.freshness] {
			continue
		}

		document := DocumentProblems{
			URI:            stored.uri,
			Path:           stored.path,
			Navigable:      stored.navigable,
			Version:        cloneInt(stored.version),
			CurrentVersion: cloneInt(stored.currentVersion),
			Freshness:      stored.freshness,
			Dropped:        stored.dropped,
		}
		for _, problem := range stored.problems {
			if len(severityFilter) != 0 && !severityFilter[problem.Severity] {
				continue
			}
			document.Problems = append(document.Problems, cloneProblem(problem))
		}
		if len(severityFilter) != 0 && len(document.Problems) == 0 {
			continue
		}
		snapshot.Total += len(document.Problems)
		snapshot.Dropped += document.Dropped
		snapshot.Documents = append(snapshot.Documents, document)
	}

	sort.Slice(snapshot.Documents, func(i, j int) bool {
		a, b := snapshot.Documents[i], snapshot.Documents[j]
		if a.Navigable != b.Navigable {
			return a.Navigable
		}
		if a.Path != b.Path {
			return filepath.ToSlash(a.Path) < filepath.ToSlash(b.Path)
		}
		return a.URI < b.URI
	})
	return snapshot
}

// Subscribe returns a capacity-one stream of generations and an idempotent
// cancellation function. A slow subscriber never blocks publication; queued
// generations are replaced so the one value waiting is always the newest.
func (s *ProblemStore) Subscribe() (<-chan uint64, func()) {
	s.mu.Lock()
	s.nextSubscriber++
	id := s.nextSubscriber
	ch := make(chan uint64, 1)
	s.subscribers[id] = ch
	s.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			s.mu.Lock()
			if active, ok := s.subscribers[id]; ok {
				delete(s.subscribers, id)
				close(active)
			}
			s.mu.Unlock()
		})
	}
	return ch, cancel
}

func (s *ProblemStore) normalizeProblem(uri, path string, navigable bool, problem Problem) Problem {
	problem.URI = uri
	problem.Path = path
	problem.Navigable = navigable
	normalizePosition(&problem.Line, &problem.Column, &problem.EndLine, &problem.EndColumn)
	if problem.Severity < SeverityError || problem.Severity > SeverityHint {
		problem.Severity = 0
	}
	problem.Code = redactThenTruncateUTF8(problem.Code, maxProblemCodeBytes)
	problem.Source = redactThenTruncateUTF8(problem.Source, maxProblemSourceBytes)
	problem.Message = redactThenTruncateUTF8(problem.Message, maxProblemMessageBytes)

	relatedLimit := len(problem.Related)
	if relatedLimit > maxRelatedPerProblem {
		relatedLimit = maxRelatedPerProblem
	}
	related := make([]RelatedProblem, relatedLimit)
	for i := 0; i < relatedLimit; i++ {
		item := problem.Related[i]
		item.Path, item.Navigable = s.pathForURI(item.URI)
		normalizePosition(&item.Line, &item.Column, &item.EndLine, &item.EndColumn)
		item.Message = redactThenTruncateUTF8(item.Message, maxProblemMessageBytes)
		related[i] = item
	}
	problem.RelatedDropped = len(problem.Related) - relatedLimit
	problem.Related = related
	return problem
}

func (s *ProblemStore) protocolIssueLocked(message string) error {
	message = redactThenTruncateUTF8(message, maxProblemMessageBytes)
	s.protocolIssues++
	s.lastProtocolIssue = message
	s.changedLocked()
	return fmt.Errorf("language-server protocol issue: %s", message)
}

func (s *ProblemStore) protocolIssue(message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.protocolIssueLocked(message)
}

func (s *ProblemStore) changedLocked() {
	s.generation++
	for _, subscriber := range s.subscribers {
		select {
		case subscriber <- s.generation:
			continue
		default:
		}
		// Coalesce to the newest generation without ever waiting for the
		// consumer. Cancellation also takes mu, so the channel cannot close
		// while this replacement is in flight.
		select {
		case <-subscriber:
		default:
		}
		select {
		case subscriber <- s.generation:
		default:
		}
	}
}

func (s *ProblemStore) pathForURI(uri string) (string, bool) {
	parsed, err := url.Parse(uri)
	if err != nil || !strings.EqualFold(parsed.Scheme, "file") || parsed.Opaque != "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Host != "" && !strings.EqualFold(parsed.Host, "localhost")) {
		return "", false
	}
	path, err := filePath(uri)
	if err != nil {
		return "", false
	}
	if path == "" {
		return "", false
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", false
	}
	canonical = filepath.Clean(canonical)
	relative, err := filepath.Rel(s.root, canonical)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", false
	}
	return canonical, true
}

func (s *ProblemStore) canonicalFilterPath(path string) string {
	if path == "" {
		return ""
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.root, path)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	if canonical, err := filepath.EvalSymlinks(absolute); err == nil {
		return filepath.Clean(canonical)
	}
	return filepath.Clean(absolute)
}

func canonicalRoot(root string) string {
	absolute, err := filepath.Abs(root)
	if err != nil {
		absolute = root
	}
	if canonical, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = canonical
	}
	return filepath.Clean(absolute)
}

func normalizePosition(line, column, endLine, endColumn *int) {
	if *line < 1 {
		*line = 1
	}
	if *column < 1 {
		*column = 1
	}
	if *endLine < *line {
		*endLine = *line
	}
	if *endColumn < 1 || (*endLine == *line && *endColumn < *column) {
		*endColumn = *column
	}
}

func truncateUTF8(value string, limit int) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	if len(value) <= limit {
		return value
	}
	end := limit
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end]
}

func redactThenTruncateUTF8(value string, limit int) string {
	value = credential.Redact(value, credential.ScanPrompt(value))
	return truncateUTF8(value, limit)
}

func sortProblems(problems []Problem) {
	sort.SliceStable(problems, func(i, j int) bool {
		a, b := problems[i], problems[j]
		switch {
		case a.Line != b.Line:
			return a.Line < b.Line
		case a.Column != b.Column:
			return a.Column < b.Column
		case a.EndLine != b.EndLine:
			return a.EndLine < b.EndLine
		case a.EndColumn != b.EndColumn:
			return a.EndColumn < b.EndColumn
		case a.Severity != b.Severity:
			return severitySortValue(a.Severity) < severitySortValue(b.Severity)
		case a.Source != b.Source:
			return a.Source < b.Source
		case a.Code != b.Code:
			return a.Code < b.Code
		default:
			return a.Message < b.Message
		}
	})
}

func severitySortValue(severity Severity) Severity {
	if severity == 0 {
		return SeverityHint + 1
	}
	return severity
}

func cloneProblem(problem Problem) Problem {
	problem.Related = append([]RelatedProblem(nil), problem.Related...)
	return problem
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	return intPointer(*value)
}

func intPointer(value int) *int { return &value }

func severitySet(values []Severity) map[Severity]bool {
	if len(values) == 0 {
		return nil
	}
	set := make(map[Severity]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

func freshnessSet(values []Freshness) map[Freshness]bool {
	if len(values) == 0 {
		return nil
	}
	set := make(map[Freshness]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}
