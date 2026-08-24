package lsp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func TestProblemStoreReplacesAndClearsOneURI(t *testing.T) {
	root := t.TempDir()
	path := writeProblemFile(t, root, "a.go")
	uri := problemFileURI(path)
	store := NewProblemStore(root)

	v1 := 1
	err := store.publish(problemPublish{
		URI: uri, Version: &v1, CurrentVersion: 1, CurrentVersionKnown: true,
		Problems: []Problem{
			{Line: 8, Column: 2, EndLine: 8, EndColumn: 3, Severity: SeverityWarning, Message: "later"},
			{Line: 2, Column: 4, EndLine: 2, EndColumn: 5, Severity: SeverityError, Message: "first"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := store.Snapshot(ProblemFilter{})
	if snapshot.Total != 2 || len(snapshot.Documents) != 1 {
		t.Fatalf("first snapshot = %+v, want one document with two problems", snapshot)
	}
	document := snapshot.Documents[0]
	if document.URI != uri || document.Path != path || !document.Navigable || document.Freshness != Fresh {
		t.Fatalf("document identity/freshness = %+v", document)
	}
	if document.Version == nil || *document.Version != 1 || document.CurrentVersion == nil || *document.CurrentVersion != 1 {
		t.Fatalf("document versions = published %v, current %v", document.Version, document.CurrentVersion)
	}
	if document.Problems[0].Message != "first" || document.Problems[1].Message != "later" {
		t.Fatalf("problems were not stably position-sorted: %+v", document.Problems)
	}

	v2 := 2
	if err := store.publish(problemPublish{
		URI: uri, Version: &v2, CurrentVersion: 2, CurrentVersionKnown: true,
		Problems: []Problem{{Line: 3, Column: 1, EndLine: 3, EndColumn: 2, Message: "replacement"}},
	}); err != nil {
		t.Fatal(err)
	}
	snapshot = store.Snapshot(ProblemFilter{})
	if snapshot.Total != 1 || len(snapshot.Documents[0].Problems) != 1 || snapshot.Documents[0].Problems[0].Message != "replacement" {
		t.Fatalf("replacement snapshot = %+v", snapshot)
	}

	v3 := 3
	if err := store.publish(problemPublish{URI: uri, Version: &v3, CurrentVersion: 3, CurrentVersionKnown: true}); err != nil {
		t.Fatal(err)
	}
	snapshot = store.Snapshot(ProblemFilter{})
	if snapshot.Total != 0 || len(snapshot.Documents) != 0 {
		t.Fatalf("empty publication did not clear URI: %+v", snapshot)
	}
}

func TestProblemStoreRejectsOldAndFutureVersions(t *testing.T) {
	root := t.TempDir()
	uri := problemFileURI(writeProblemFile(t, root, "a.go"))
	store := NewProblemStore(root)

	v2 := 2
	if err := store.publish(problemPublish{
		URI: uri, Version: &v2, CurrentVersion: 2, CurrentVersionKnown: true,
		Problems: []Problem{{Line: 1, Column: 1, Message: "current"}},
	}); err != nil {
		t.Fatal(err)
	}
	generation := store.Snapshot(ProblemFilter{}).Generation

	v1 := 1
	if err := store.publish(problemPublish{
		URI: uri, Version: &v1, CurrentVersion: 2, CurrentVersionKnown: true,
		Problems: []Problem{{Line: 1, Column: 1, Message: "old"}},
	}); err != nil {
		t.Fatalf("old publication should be ignored without a protocol error: %v", err)
	}
	snapshot := store.Snapshot(ProblemFilter{})
	if snapshot.Generation != generation || snapshot.Documents[0].Problems[0].Message != "current" {
		t.Fatalf("old publication changed the snapshot: %+v", snapshot)
	}

	v3 := 3
	if err := store.publish(problemPublish{
		URI: uri, Version: &v3, CurrentVersion: 2, CurrentVersionKnown: true,
		Problems: []Problem{{Line: 1, Column: 1, Message: "future"}},
	}); err == nil {
		t.Fatal("future publication must report a protocol issue")
	}
	snapshot = store.Snapshot(ProblemFilter{})
	if snapshot.ProtocolIssues != 1 || !strings.Contains(snapshot.LastProtocolIssue, "future version 3") {
		t.Fatalf("protocol issue was not retained: %+v", snapshot)
	}
	if snapshot.Documents[0].Problems[0].Message != "current" {
		t.Fatalf("future publication replaced current results: %+v", snapshot.Documents[0])
	}

	// The comparison with the last accepted server version also protects a
	// document for which the client no longer has an open-version record.
	if err := store.publish(problemPublish{
		URI: uri, Version: &v1,
		Problems: []Problem{{Line: 1, Column: 1, Message: "old without client version"}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := store.Snapshot(ProblemFilter{}).Documents[0].Problems[0].Message; got != "current" {
		t.Fatalf("older server version replaced accepted data: %q", got)
	}
}

func TestProblemStoreFreshnessTransitionsPreserveResults(t *testing.T) {
	root := t.TempDir()
	uri := problemFileURI(writeProblemFile(t, root, "a.go"))
	store := NewProblemStore(root)
	v1 := 1
	if err := store.publish(problemPublish{
		URI: uri, Version: &v1, CurrentVersion: 1, CurrentVersionKnown: true,
		Problems: []Problem{{Line: 4, Column: 2, Message: "keep me"}},
	}); err != nil {
		t.Fatal(err)
	}

	store.invalidate(uri, 2)
	document := onlyProblemDocument(t, store.Snapshot(ProblemFilter{}))
	if document.Freshness != Pending || document.CurrentVersion == nil || *document.CurrentVersion != 2 || document.Problems[0].Message != "keep me" {
		t.Fatalf("invalidated document = %+v", document)
	}

	store.unavailable()
	snapshot := store.Snapshot(ProblemFilter{})
	document = onlyProblemDocument(t, snapshot)
	if snapshot.Available || document.Freshness != Stale || document.Problems[0].Message != "keep me" {
		t.Fatalf("unavailable snapshot = %+v", snapshot)
	}

	// While unavailable, another local version remains stale rather than
	// implying that a live server is working on it.
	store.invalidate(uri, 3)
	document = onlyProblemDocument(t, store.Snapshot(ProblemFilter{}))
	if document.Freshness != Stale || document.CurrentVersion == nil || *document.CurrentVersion != 3 {
		t.Fatalf("unavailable invalidation = %+v", document)
	}

	v3 := 3
	if err := store.publish(problemPublish{
		URI: uri, Version: &v3, CurrentVersion: 3, CurrentVersionKnown: true,
		Problems: []Problem{{Line: 5, Column: 1, Message: "new"}},
	}); err != nil {
		t.Fatal(err)
	}
	snapshot = store.Snapshot(ProblemFilter{})
	document = onlyProblemDocument(t, snapshot)
	if !snapshot.Available || document.Freshness != Fresh || document.Problems[0].Message != "new" {
		t.Fatalf("fresh replacement = %+v", snapshot)
	}

	if err := store.publish(problemPublish{
		URI: uri, CurrentVersion: 3, CurrentVersionKnown: true,
		Problems: []Problem{{Line: 6, Column: 1, Message: "no version"}},
	}); err != nil {
		t.Fatal(err)
	}
	document = onlyProblemDocument(t, store.Snapshot(ProblemFilter{}))
	if document.Freshness != Unversioned || document.Version != nil {
		t.Fatalf("unversioned publication = %+v", document)
	}
}

func TestProblemStoreReopenStartsANewVersionEpoch(t *testing.T) {
	root := t.TempDir()
	uri := problemFileURI(writeProblemFile(t, root, "a.go"))
	store := NewProblemStore(root)
	v2 := 2
	if err := store.publish(problemPublish{
		URI: uri, Version: &v2, CurrentVersion: 2, CurrentVersionKnown: true,
		Problems: []Problem{{Line: 1, Column: 1, Message: "old lifecycle"}},
	}); err != nil {
		t.Fatal(err)
	}

	store.reopen(uri, 1)
	document := onlyProblemDocument(t, store.Snapshot(ProblemFilter{}))
	if document.Version != nil || document.CurrentVersion == nil || *document.CurrentVersion != 1 || document.Freshness != Pending {
		t.Fatalf("reopened document = %+v", document)
	}
	v1 := 1
	if err := store.publish(problemPublish{
		URI: uri, Version: &v1, CurrentVersion: 1, CurrentVersionKnown: true,
		Problems: []Problem{{Line: 2, Column: 1, Message: "new lifecycle"}},
	}); err != nil {
		t.Fatalf("version 1 after reopen was rejected as older than the prior lifecycle: %v", err)
	}
	document = onlyProblemDocument(t, store.Snapshot(ProblemFilter{}))
	if document.Problems[0].Message != "new lifecycle" || document.Version == nil || *document.Version != 1 {
		t.Fatalf("new lifecycle publication = %+v", document)
	}

	store.reopen(uri, 1)
	if err := store.publish(problemPublish{URI: uri, Version: &v1, CurrentVersion: 1, CurrentVersionKnown: true}); err != nil {
		t.Fatal(err)
	}
	if snapshot := store.Snapshot(ProblemFilter{}); snapshot.Total != 0 || len(snapshot.Documents) != 0 {
		t.Fatalf("version 1 clear after reopen did not remove old diagnostics: %+v", snapshot)
	}
}

func TestProblemStoreBoundsMessagesRelatedAndTotals(t *testing.T) {
	root := t.TempDir()
	store := NewProblemStore(root)
	longMessage := strings.Repeat("é", maxProblemMessageBytes)
	related := make([]RelatedProblem, maxRelatedPerProblem+7)
	for i := range related {
		related[i] = RelatedProblem{URI: "untitled:related", Message: longMessage}
	}
	problems := make([]Problem, maxProblemsPerDocument+5)
	for i := range problems {
		problems[i] = Problem{
			Line: i + 1, Column: 1, Severity: SeverityWarning,
			Code: strings.Repeat("c", maxProblemCodeBytes+10), Source: strings.Repeat("s", maxProblemSourceBytes+10),
			Message: longMessage, Related: related,
		}
	}
	v1 := 1
	if err := store.publish(problemPublish{URI: "untitled:first", Version: &v1, Problems: problems}); err != nil {
		t.Fatal(err)
	}
	snapshot := store.Snapshot(ProblemFilter{})
	document := onlyProblemDocument(t, snapshot)
	if len(document.Problems) != maxProblemsPerDocument || document.Dropped != 5 {
		t.Fatalf("per-document bounds = kept %d dropped %d", len(document.Problems), document.Dropped)
	}
	problem := document.Problems[0]
	if len(problem.Message) > maxProblemMessageBytes || !utf8.ValidString(problem.Message) {
		t.Fatalf("message boundary = %d bytes, valid=%v", len(problem.Message), utf8.ValidString(problem.Message))
	}
	if len(problem.Code) != maxProblemCodeBytes || len(problem.Source) != maxProblemSourceBytes {
		t.Fatalf("code/source lengths = %d/%d", len(problem.Code), len(problem.Source))
	}
	if len(problem.Related) != maxRelatedPerProblem || problem.RelatedDropped != 7 {
		t.Fatalf("related bounds = kept %d dropped %d", len(problem.Related), problem.RelatedDropped)
	}
	if len(problem.Related[0].Message) > maxProblemMessageBytes || !utf8.ValidString(problem.Related[0].Message) {
		t.Fatal("related message was not bounded at a valid UTF-8 boundary")
	}

	for documentIndex := 1; documentIndex < maxProblemsTotal/maxProblemsPerDocument; documentIndex++ {
		uri := fmt.Sprintf("untitled:document-%02d", documentIndex)
		if err := store.publish(problemPublish{URI: uri, Version: &v1, Problems: problems[:maxProblemsPerDocument]}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot = store.Snapshot(ProblemFilter{})
	if snapshot.Total != maxProblemsTotal {
		t.Fatalf("total = %d, want global cap %d", snapshot.Total, maxProblemsTotal)
	}
	if err := store.publish(problemPublish{
		URI: "untitled:beyond-cap", Version: &v1,
		Problems: []Problem{{Line: 1, Column: 1, Message: "must not make the store grow"}},
	}); err != nil {
		t.Fatal(err)
	}
	snapshot = store.Snapshot(ProblemFilter{})
	if snapshot.Total != maxProblemsTotal || len(snapshot.Documents) != maxProblemsTotal/maxProblemsPerDocument {
		t.Fatalf("global bound snapshot = total %d, documents %d", snapshot.Total, len(snapshot.Documents))
	}
}

func TestProblemStoreRedactsCredentialFieldsBeforeTheirCaps(t *testing.T) {
	token := "ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	prefix := strings.Repeat("x", maxProblemMessageBytes-len(token)+1)
	store := NewProblemStore(t.TempDir())
	if err := store.publish(problemPublish{URI: "untitled:credential", Problems: []Problem{{
		Line: 1, Column: 1,
		Code:    strings.Repeat("c", maxProblemCodeBytes-len(token)+1) + token,
		Source:  strings.Repeat("s", maxProblemSourceBytes-len(token)+1) + token,
		Message: prefix + token,
		Related: []RelatedProblem{{URI: "untitled:related", Message: prefix + token}},
	}}}); err != nil {
		t.Fatal(err)
	}
	problem := onlyProblemDocument(t, store.Snapshot(ProblemFilter{})).Problems[0]
	for name, value := range map[string]string{
		"code": problem.Code, "source": problem.Source, "message": problem.Message,
		"related": problem.Related[0].Message,
	} {
		if strings.Contains(value, token) || strings.Contains(value, "ghp_") {
			t.Fatalf("%s cap exposed a credential fragment: %q", name, value)
		}
		if !strings.Contains(value, "[redacted: a GitHub token]") {
			t.Fatalf("%s was not redacted before its cap: %q", name, value)
		}
	}
}

func TestProblemStoreSubscriptionCoalescesAndCancels(t *testing.T) {
	store := NewProblemStore(t.TempDir())
	changes, cancel := store.Subscribe()
	if cap(changes) != 1 {
		t.Fatalf("subscription capacity = %d, want 1", cap(changes))
	}

	v1, v2 := 1, 2
	if err := store.publish(problemPublish{
		URI: "untitled:a", Version: &v1,
		Problems: []Problem{{Line: 1, Column: 1, Message: "one"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.publish(problemPublish{
		URI: "untitled:a", Version: &v2,
		Problems: []Problem{{Line: 1, Column: 1, Message: "two"}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := <-changes; got != 2 {
		t.Fatalf("coalesced generation = %d, want newest generation 2", got)
	}
	select {
	case got := <-changes:
		t.Fatalf("subscription had an extra queued generation %d", got)
	default:
	}

	cancel()
	cancel()
	if _, ok := <-changes; ok {
		t.Fatal("cancel did not close the subscription")
	}
}

func TestProblemStoreOnlyNavigatesCanonicalWorkspaceFiles(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	outsideRoot := filepath.Join(parent, "outside")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outsideRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	inside := writeProblemFile(t, root, "dir/name #1.go")
	outside := writeProblemFile(t, outsideRoot, "outside.go")

	insideURI := problemFileURI(inside)
	updates := []string{insideURI, problemFileURI(outside), "https://example.test/a.go"}
	store := NewProblemStore(root)
	for i, uri := range updates {
		if err := store.publish(problemPublish{
			URI:      uri,
			Problems: []Problem{{Line: i + 1, Column: 1, Message: fmt.Sprintf("problem-%d", i)}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	snapshot := store.Snapshot(ProblemFilter{})
	if len(snapshot.Documents) != len(updates) {
		t.Fatalf("documents = %d, want %d", len(snapshot.Documents), len(updates))
	}
	navigable := store.Snapshot(ProblemFilter{NavigableOnly: true})
	if len(navigable.Documents) != 1 || navigable.Documents[0].URI != insideURI || navigable.Documents[0].Path != inside {
		t.Fatalf("navigable documents = %+v, want only the canonical in-root file", navigable.Documents)
	}
	if got := navigable.Documents[0].Problems[0].URI; got != insideURI {
		t.Fatalf("problem URI = %q, want original %q", got, insideURI)
	}
	filtered := store.Snapshot(ProblemFilter{Path: filepath.Join("dir", "name #1.go")})
	if len(filtered.Documents) != 1 || filtered.Documents[0].URI != insideURI {
		t.Fatalf("relative path filter = %+v", filtered.Documents)
	}

	t.Run("symlink escape", func(t *testing.T) {
		symlink := filepath.Join(root, "escape.go")
		if err := os.Symlink(outside, symlink); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		symlinkStore := NewProblemStore(root)
		if err := symlinkStore.publish(problemPublish{
			URI:      problemFileURI(symlink),
			Problems: []Problem{{Line: 1, Column: 1, Message: "symlink escape"}},
		}); err != nil {
			t.Fatal(err)
		}
		if got := symlinkStore.Snapshot(ProblemFilter{NavigableOnly: true}); len(got.Documents) != 0 {
			t.Fatalf("symlink escape was navigable: %+v", got.Documents)
		}
	})
}

func TestProblemStoreSnapshotFiltersSortsAndDoesNotAlias(t *testing.T) {
	root := t.TempDir()
	aPath := writeProblemFile(t, root, "a.go")
	bPath := writeProblemFile(t, root, "b.go")
	aURI, bURI := problemFileURI(aPath), problemFileURI(bPath)
	store := NewProblemStore(root)
	v1 := 1
	if err := store.publish(problemPublish{
		URI: bURI, Version: &v1, CurrentVersion: 1, CurrentVersionKnown: true,
		Problems: []Problem{{Line: 2, Column: 1, Severity: SeverityHint, Message: "hint"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.publish(problemPublish{
		URI: aURI, Version: &v1, CurrentVersion: 1, CurrentVersionKnown: true,
		Problems: []Problem{
			{Line: 3, Column: 2, Severity: SeverityWarning, Message: "warning", Related: []RelatedProblem{{URI: bURI, Line: 1, Column: 1, Message: "related"}}},
			{Line: 1, Column: 1, Severity: SeverityError, Message: "error"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	snapshot := store.Snapshot(ProblemFilter{})
	if len(snapshot.Documents) != 2 || snapshot.Documents[0].Path != aPath || snapshot.Documents[1].Path != bPath {
		t.Fatalf("document sort = %+v", snapshot.Documents)
	}
	if snapshot.Documents[0].Problems[0].Message != "error" {
		t.Fatalf("problem sort = %+v", snapshot.Documents[0].Problems)
	}
	errors := store.Snapshot(ProblemFilter{Severities: []Severity{SeverityError}})
	if errors.Total != 1 || len(errors.Documents) != 1 || errors.Documents[0].Problems[0].Message != "error" {
		t.Fatalf("severity filter = %+v", errors)
	}
	if fresh := store.Snapshot(ProblemFilter{Freshness: []Freshness{Fresh}}); fresh.Total != 3 {
		t.Fatalf("freshness filter total = %d, want 3", fresh.Total)
	}
	if byURI := store.Snapshot(ProblemFilter{URI: bURI}); byURI.Total != 1 || byURI.Documents[0].URI != bURI {
		t.Fatalf("URI filter = %+v", byURI)
	}

	// Mutating every reference-bearing field in the result must not mutate
	// the next snapshot or its version pointers.
	snapshot.Documents[0].Problems[1].Message = "changed"
	snapshot.Documents[0].Problems[1].Related[0].Message = "changed"
	*snapshot.Documents[0].Version = 99
	again := store.Snapshot(ProblemFilter{URI: aURI})
	if again.Documents[0].Problems[1].Message != "warning" || again.Documents[0].Problems[1].Related[0].Message != "related" || *again.Documents[0].Version != 1 {
		t.Fatalf("snapshot aliases store state: %+v", again.Documents[0])
	}
}

func TestProblemStoreConcurrentPublishSnapshotAndSubscribe(t *testing.T) {
	store := NewProblemStore(t.TempDir())
	changes, cancel := store.Subscribe()
	defer cancel()

	const workers = 20
	const updates = 50
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			uri := fmt.Sprintf("untitled:worker-%02d", worker)
			for version := 1; version <= updates; version++ {
				v := version
				if err := store.publish(problemPublish{
					URI: uri, Version: &v, CurrentVersion: version, CurrentVersionKnown: true,
					Problems: []Problem{{Line: version, Column: 1, Message: fmt.Sprintf("%d", version)}},
				}); err != nil {
					t.Errorf("publish: %v", err)
					return
				}
				_ = store.Snapshot(ProblemFilter{URI: uri})
			}
		}()
	}
	wg.Wait()

	snapshot := store.Snapshot(ProblemFilter{})
	if snapshot.Total != workers || len(snapshot.Documents) != workers {
		t.Fatalf("concurrent snapshot = total %d documents %d", snapshot.Total, len(snapshot.Documents))
	}
	for _, document := range snapshot.Documents {
		if document.Version == nil || *document.Version != updates || document.Problems[0].Message != fmt.Sprint(updates) {
			t.Fatalf("document did not retain latest version: %+v", document)
		}
	}
	select {
	case generation := <-changes:
		if generation != snapshot.Generation {
			t.Fatalf("coalesced concurrent generation = %d, snapshot = %d", generation, snapshot.Generation)
		}
	default:
		t.Fatal("subscriber was not notified")
	}
}

func onlyProblemDocument(t *testing.T, snapshot ProblemSnapshot) DocumentProblems {
	t.Helper()
	if len(snapshot.Documents) != 1 {
		t.Fatalf("documents = %d, want 1: %+v", len(snapshot.Documents), snapshot)
	}
	return snapshot.Documents[0]
}

func writeProblemFile(t *testing.T, root, relative string) string {
	t.Helper()
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("package test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func problemFileURI(path string) string {
	return fileURI(path)
}
