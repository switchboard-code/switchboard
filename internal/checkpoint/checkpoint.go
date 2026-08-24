// Package checkpoint records what files looked like before each turn
// changed them, so a turn's edits can be taken back.
//
// The scope is deliberately files, not conversation. Rewinding messages
// would rewrite the append-only prefix and invalidate the provider cache
// from that point (§6.1); restoring a file invalidates nothing, because the
// model is required to re-read before it may write again — undo leans on
// the same read-before-write contract the tools already enforce.
//
// Capture is before-first-mutation, per turn: the first time a turn touches
// a file, its prior bytes are kept; later touches in the same turn are the
// turn's own churn and restore to the pre-turn state. Only the write and
// edit tools capture. A shell command that mutates the workspace is outside
// the boundary, and saying so plainly beats a checkpoint that sometimes
// covers it.
package checkpoint

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const (
	// maxTurns bounds memory across a long session; the oldest checkpoint
	// falls off. Fifty user turns of undo depth is a session's working past.
	maxTurns = 50

	// maxFileBytes bounds one captured file. A file over the cap is not
	// captured, and the turn is marked partial so an undo can say what it
	// could not restore instead of silently restoring half a turn.
	maxFileBytes = 4 << 20

	// Standalone state-file publication may carry a larger explicitly bounded
	// source than an undo capture. It is never retained in checkpoint history.
	maxStandaloneFilePublicationBytes = 8 << 20

	// A transactional retry retains current post-images in addition to the
	// recorder's pre-images. Bound both dimensions so one generated-file turn
	// cannot double an unbounded amount of process memory while adoption waits.
	maxPreparedUndoFiles = 1024
	maxPreparedUndoBytes = 32 << 20
)

type fileState struct {
	existed bool
	mode    fs.FileMode
	content []byte
	after   fingerprint
	parent  fs.FileInfo
	parents []ancestorIdentity

	// committed distinguishes a successful mutation from a capture that is
	// only prepared. activeKind distinguishes legacy one-call Record captures
	// from two-phase RecordState captures that Begin and undo must wait for.
	// This also lets a later mutation update the expected post-image without
	// replacing the turn's first pre-image or its original parent identity.
	committed       bool
	parentSet       bool
	publicationOnly bool
	activeKind      captureKind
	active          int
}

type captureKind uint8

const (
	captureIdle captureKind = iota
	captureLegacy
	captureTwoPhase
)

type fingerprint struct {
	existed bool
	mode    fs.FileMode
	size    int64
	digest  [sha256.Size]byte
}

type ancestorIdentity struct {
	path string
	info fs.FileInfo
}

type skippedState struct {
	committed  bool
	activeKind captureKind
	active     int
}

// FileState is one captured file's bytes-and-existence, exported for the
// surfaces that reconstruct past states rather than pop them — /bisect
// above all. Existed false means the file was not there: restoring that
// state is deleting the file.
type FileState struct {
	Existed bool
	Mode    fs.FileMode
	Content []byte
}

// FileFingerprint is the exact, bounded identity an undo compare-and-swap
// expects at the target. Digest is SHA-256 of the complete file bytes. A
// non-existent state has a zero Mode and Digest.
type FileFingerprint struct {
	Existed bool
	Mode    fs.FileMode
	Digest  [sha256.Size]byte
}

// MutationSnapshot is one successful path mutation shaped for read-only
// review. Before is cloned and cannot mutate recorder state; After is the
// committed guard that makes a later current-state read stale-safe.
type MutationSnapshot struct {
	Path   string
	Before FileState
	After  FileFingerprint

	// turn and state bind the otherwise cloned value to live recorder evidence.
	// They are deliberately private: callers can ask Recorder to read the
	// matching current post-image, but cannot mint authority for an arbitrary
	// path.
	turn  *Turn
	state *fileState
}

// TurnSnapshot is the recorder's non-consuming review surface. Files contains
// successful mutations only. Skipped names successful mutations whose
// pre-images exceeded the memory bound and therefore cannot be reviewed or
// restored exactly.
type TurnSnapshot struct {
	Label   string
	Files   []MutationSnapshot
	Skipped []string
	Partial bool
	Open    bool
}

// TurnIdentity binds an in-memory checkpoint scope to the exact durable user
// opening that caused it. Label is deliberately not part of the identity: it
// is truncated display text, so two turns may share it without being the same
// turn.
type TurnIdentity struct {
	SessionID      string
	OpeningMessage int
}

// PreparedUndo is a non-mutating, bounded snapshot of one exact current turn's
// inverse. Preparing verifies and retains every current post-image needed to
// roll a failed commit forward again. Apply is one-shot: it either consumes the
// turn after every pre-image is published, or restores every published path to
// its prepared post-image and leaves the recorder evidence available.
//
// The token deliberately keeps its fields private. A caller can carry it
// across an asynchronous pause or fork, but cannot redirect its filesystem
// authority to another path or turn.
type PreparedUndo struct {
	mu       sync.Mutex
	recorder *Recorder
	identity TurnIdentity
	revision uint64
	label    string
	files    []preparedUndoFile
	used     bool
	journal  *durableUndoHandle
}

type preparedUndoFile struct {
	path     string
	turn     *Turn
	state    *fileState // live identity only; accessed under Recorder.mu
	restore  *fileState // immutable pre-image and prepared compare-and-swap guard
	post     FileState
	tempName string // persisted by durable retry before any restore can start
}

// PreparedUndoResult describes a successful prepared restore. Restored paths
// existed before the turn; Removed paths were created by it.
type PreparedUndoResult struct {
	Restored       []string
	Removed        []string
	Label          string
	CleanupWarning error
	// RecoveryRequired is non-nil only when the staged child became visible
	// before its publication durability was proven. The operation committed and
	// must not roll back, but no further workspace mutation is safe until the
	// retained journal is resolved after restart.
	RecoveryRequired error
}

// DurableCommitOutcome is the publication half of a retry transaction.
// Published is the no-rollback point; Durable permits journal retirement.
type DurableCommitOutcome struct {
	Published bool
	Durable   bool
}

// ReviewCursor is opaque authority for one exact recorder turn at one exact
// checkpoint revision. It lets an asynchronous read-only surface bind its
// selection before leaving the UI goroutine without cloning any pre-images.
type ReviewCursor struct {
	turn     *Turn
	revision uint64
	index    int
	open     bool
}

// Turn is one user turn's capture set.
type Turn struct {
	identity TurnIdentity
	label    string
	files    map[string]*fileState
	skipped  map[string]*skippedState // paths over the cap, named rather than half-covered
}

// Info describes a turn for display.
type Info struct {
	Label   string
	Files   int
	Partial bool
	Skipped []string
}

// UndoFileOutcome separates the point-of-no-return from the checks that run
// after it. Published means the target was removed or replaced, so callers
// must invalidate any read authority for the path even when UndoFile also
// returns a durability or final-state verification error. Removed identifies
// which inverse operation was published.
type UndoFileOutcome struct {
	Published bool
	Removed   bool
}

// Recorder is safe for concurrent use: parallel-safe tools do not mutate,
// but the loop and a surface may inspect while a turn runs.
type Recorder struct {
	mu                 sync.Mutex
	restoreMu          sync.Mutex
	idle               *sync.Cond
	activeTransactions int
	activeRestores     int
	transitionWaiters  int // deterministic concurrency tests; guarded by mu
	turns              []*Turn
	cur                *Turn
	revision           uint64

	// restoreHook is deterministic fault injection for tests that prove a
	// mutation cannot enter RecordState while a restore is in flight.
	restoreHook func()

	// snapshotAfterOpenHook is deterministic fault injection for tests that
	// prove snapshot I/O does not hold the recorder lifecycle lock.
	snapshotAfterOpenHook func()

	// These hooks inject failures immediately after the irreversible filesystem
	// operation. They pin the distinction between publication and the later
	// durability/verification checks without relying on filesystem quirks.
	beforeRemoveHook  func() error
	beforeReplaceHook func() error
	// publicationSeamHook runs after the ordinary pre-publication hook and is
	// followed by one final descriptor-identity check. It exists only for the
	// deterministic substitution regressions; production leaves it nil.
	publicationSeamHook func() error
	afterRemoveHook     func() error
	afterReplaceHook    func() error

	// durableUndoHook is a process-crash boundary used only by subprocess
	// tests. Production recorders leave it nil.
	durableUndoHook          func(durableUndoBoundary, int)
	beforeDurableJournalHook func() error
	restoreCleanup           *restoreCleanupConfig
	restoreTempLedgerHook    func(restoreTempLedgerBoundary)
}

func NewRecorder() *Recorder {
	r := &Recorder{}
	r.idle = sync.NewCond(&r.mu)
	return r
}

// ErrStale means an undo target no longer matches the successful mutation's
// post-image. Refusing is intentional: restoring over an editor, formatter,
// shell command, or later overlapping agent edit would turn undo into data
// loss. The capture remains available when the refusal happens before
// publication. If a final check finds staleness after remove/replace succeeded,
// UndoFileOutcome.Published reports that point-of-no-return and the capture is
// consumed.
var ErrStale = errors.New("checkpoint post-image no longer matches")

// ErrSnapshotTooLarge means a current regular file has the committed
// existence, mode, and size, but its digest cannot be reverified within the
// review I/O bound. A review surface must render an explicit unverified marker
// instead of a text diff.
var ErrSnapshotTooLarge = errors.New("checkpoint post-image is over the review byte limit")

// ErrPreparedUndoTooLarge means a turn has more files or aggregate current
// bytes than /retry can retain transactionally. The checkpoint remains intact;
// callers may use ordinary /undo, whose inverse does not retain post-images.
var ErrPreparedUndoTooLarge = errors.New("checkpoint turn is over the transactional retry limit")

// Begin opens a new turn scope. An open scope with no captures is
// discarded rather than stacked, so /undo never pops a turn that changed
// nothing.
func (r *Recorder) Begin(label string) {
	r.beginTurn(TurnIdentity{}, label)
}

// BeginTurn opens a checkpoint scope for one exact durable conversation turn.
// Begin remains for callers that do not own a session opening; such anonymous
// scopes can be used by /undo but can never be mistaken for /retry's turn.
func (r *Recorder) BeginTurn(sessionID string, openingMessage int, label string) {
	r.beginTurn(TurnIdentity{SessionID: sessionID, OpeningMessage: openingMessage}, label)
}

func (r *Recorder) beginTurn(identity TurnIdentity, label string) {
	label = strings.TrimSpace(label)
	if len(label) > 60 {
		label = label[:60] + "…"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.waitForTransactionsLocked()
	r.waitForRestoresLocked()
	r.commitLocked()
	r.cur = newTurn(identity, label)
	r.revision++
}

func (r *Recorder) conditionLocked() *sync.Cond {
	// Preserve the useful zero value for embedders that construct Recorder
	// directly instead of calling NewRecorder.
	if r.idle == nil {
		r.idle = sync.NewCond(&r.mu)
	}
	return r.idle
}

func (r *Recorder) waitForTransactionsLocked() {
	for r.activeTransactions > 0 {
		r.transitionWaiters++
		r.conditionLocked().Wait()
		r.transitionWaiters--
	}
}

func (r *Recorder) waitForRestoresLocked() {
	for r.activeRestores > 0 {
		r.transitionWaiters++
		r.conditionLocked().Wait()
		r.transitionWaiters--
	}
}

func (r *Recorder) startRestoreLocked() func() {
	r.activeRestores++
	return r.restoreHook
}

func (r *Recorder) finishRestore() {
	r.mu.Lock()
	if r.activeRestores > 0 {
		r.activeRestores--
	}
	if r.activeRestores == 0 {
		r.conditionLocked().Broadcast()
	}
	r.mu.Unlock()
}

func (r *Recorder) startTransactionLocked(kind *captureKind, active *int) {
	*kind = captureTwoPhase
	(*active)++
	r.activeTransactions++
}

func (r *Recorder) finishTransactionLocked(kind *captureKind, active *int) {
	if *kind != captureTwoPhase || *active <= 0 {
		*kind = captureIdle
		return
	}
	*active--
	if *active == 0 {
		*kind = captureIdle
	}
	if r.activeTransactions > 0 {
		r.activeTransactions--
	}
	if r.activeTransactions == 0 {
		r.conditionLocked().Broadcast()
	}
}

func newTurn(identity TurnIdentity, label string) *Turn {
	return &Turn{
		identity: identity,
		label:    label,
		files:    map[string]*fileState{},
		skipped:  map[string]*skippedState{},
	}
}

func (r *Recorder) commitLocked() {
	r.finalizeLegacyLocked()
	if r.cur == nil || (len(r.cur.files) == 0 && len(r.cur.skipped) == 0) {
		r.cur = nil
		return
	}
	r.turns = append(r.turns, r.cur)
	if len(r.turns) > maxTurns {
		r.turns = r.turns[len(r.turns)-maxTurns:]
	}
	r.cur = nil
}

// finalizeLegacyLocked preserves Record's original one-call contract for
// embedders that have not adopted Commit/Abort yet. First-party mutations use
// the two-phase API and therefore record the expected post-image immediately;
// this compatibility path samples it only when the scope is closed or undone.
func (r *Recorder) finalizeLegacyLocked() {
	if r.cur == nil {
		return
	}
	for path, st := range r.cur.files {
		if st.activeKind == captureTwoPhase {
			// Begin and undo wait before reaching this function. Keeping this
			// guard makes the invariant fail closed if another caller is added.
			continue
		}
		if st.committed && st.activeKind == captureIdle {
			continue
		}
		fp, err := fingerprintPath(path)
		if err != nil {
			if !st.committed {
				delete(r.cur.files, path)
			} else {
				st.activeKind = captureIdle
			}
			continue
		}
		if !st.parentSet {
			st.parent, st.parents, st.parentSet = parentIdentity(path)
		}
		st.after = fp
		st.committed = true
		st.activeKind = captureIdle
	}
	for _, st := range r.cur.skipped {
		if st.activeKind == captureTwoPhase {
			continue
		}
		st.committed = true
		st.activeKind = captureIdle
	}
}

// Record captures a file's current state, once per turn, before a mutation.
// Outside any turn scope it does nothing: a mutation with no turn is a
// caller bug, and inventing an anonymous checkpoint would file the capture
// where no undo can honestly describe it.
func (r *Recorder) Record(abs string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.waitForRestoresLocked()
	if r.cur == nil {
		return
	}
	r.revision++
	if st, seen := r.cur.files[abs]; seen {
		if st.activeKind != captureTwoPhase {
			st.activeKind = captureLegacy
		}
		return
	}
	if st, seen := r.cur.skipped[abs]; seen {
		if st.activeKind != captureTwoPhase {
			st.activeKind = captureLegacy
		}
		return
	}

	info, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			parent, parents, parentSet := parentIdentity(abs)
			r.cur.files[abs] = &fileState{
				existed: false, parent: parent, parents: parents, parentSet: parentSet,
				activeKind: captureLegacy,
			}
		}
		// Any other stat failure: leave uncaptured; the mutation itself will
		// surface the real error.
		return
	}
	if !info.Mode().IsRegular() {
		return
	}
	if info.Size() > maxFileBytes {
		r.cur.skipped[abs] = &skippedState{activeKind: captureLegacy}
		return
	}
	content, err := readCheckpointPathContent(abs, info, maxFileBytes, nil)
	if err != nil {
		return
	}
	parent, parents, parentSet := parentIdentity(abs)
	r.cur.files[abs] = &fileState{
		existed:    true,
		mode:       restorableMode(info.Mode()),
		content:    content,
		parent:     parent,
		parents:    parents,
		parentSet:  parentSet,
		activeKind: captureLegacy,
	}
}

func readCheckpointPathContent(path string, before fs.FileInfo, maxBytes int64, beforeOpen func()) ([]byte, error) {
	if beforeOpen != nil {
		beforeOpen()
	}
	file, err := openCheckpointPathRead(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, fmt.Errorf("%w: %s changed identity while it was opened", ErrStale, path)
	}
	if opened.Size() > maxBytes {
		return nil, fmt.Errorf("%w: %s exceeds the %d-byte snapshot bound", ErrSnapshotTooLarge, path, maxBytes)
	}
	content, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > maxBytes {
		return nil, fmt.Errorf("%w: %s grew beyond the %d-byte snapshot bound", ErrSnapshotTooLarge, path, maxBytes)
	}
	finished, err := file.Stat()
	if err != nil {
		return nil, err
	}
	linked, linkErr := os.Lstat(path)
	if linkErr != nil || !finished.Mode().IsRegular() || !linked.Mode().IsRegular() ||
		!os.SameFile(opened, finished) || !os.SameFile(finished, linked) ||
		opened.Size() != finished.Size() || finished.Size() != int64(len(content)) ||
		!opened.ModTime().Equal(finished.ModTime()) {
		return nil, errors.Join(linkErr, fmt.Errorf("%w: %s changed while it was read", ErrStale, path))
	}
	return content, nil
}

// RecordState is Record with the exact bytes already read by the mutation
// transaction. It avoids a second read and makes the capture and the
// transaction's source identity the same observation. Record remains for API
// compatibility; new mutation callers should prefer RecordState.
func (r *Recorder) RecordState(abs string, existed bool, mode fs.FileMode, content []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.waitForRestoresLocked()
	if r.cur == nil {
		return
	}
	r.revision++
	if st, seen := r.cur.files[abs]; seen {
		r.startTransactionLocked(&st.activeKind, &st.active)
		return
	}
	if st, seen := r.cur.skipped[abs]; seen {
		r.startTransactionLocked(&st.activeKind, &st.active)
		return
	}
	if existed && len(content) > maxFileBytes {
		st := &skippedState{}
		r.startTransactionLocked(&st.activeKind, &st.active)
		r.cur.skipped[abs] = st
		return
	}
	parent, parents, parentSet := parentIdentity(abs)
	st := &fileState{
		existed:   existed,
		mode:      restorableMode(mode),
		content:   append([]byte(nil), content...),
		parent:    parent,
		parents:   parents,
		parentSet: parentSet,
	}
	r.startTransactionLocked(&st.activeKind, &st.active)
	r.cur.files[abs] = st
}

func (r *Recorder) recordStateForFilePublication(abs string, existed bool, mode fs.FileMode, content []byte, maxBytes int64) error {
	if maxBytes <= 0 || maxBytes > maxStandaloneFilePublicationBytes {
		return fmt.Errorf("standalone file publication bound must be between 1 and %d bytes", maxStandaloneFilePublicationBytes)
	}
	if int64(len(content)) > maxBytes {
		return fmt.Errorf("standalone file publication source exceeds %d bytes", maxBytes)
	}
	if !existed && len(content) != 0 {
		return errors.New("absent standalone file publication source has content")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.waitForRestoresLocked()
	if r.cur == nil {
		return errors.New("standalone file publication has no open checkpoint scope")
	}
	if _, seen := r.cur.files[abs]; seen {
		return errors.New("standalone file publication path is already prepared")
	}
	if _, seen := r.cur.skipped[abs]; seen {
		return errors.New("standalone file publication path is already skipped")
	}
	parent, parents, parentSet := parentIdentity(abs)
	if !parentSet || parent == nil {
		return fmt.Errorf("%w: standalone file publication parent is unavailable", ErrStale)
	}
	st := &fileState{
		existed:         existed,
		mode:            restorableMode(mode),
		content:         append([]byte(nil), content...),
		parent:          parent,
		parents:         parents,
		parentSet:       true,
		publicationOnly: true,
	}
	r.startTransactionLocked(&st.activeKind, &st.active)
	r.cur.files[abs] = st
	r.revision++
	return nil
}

// Commit records the post-image of a successful mutation. The first
// pre-image remains unchanged across repeated edits in a turn; each commit
// only advances the compare-and-swap guard to the newest successful bytes.
// digest must be SHA-256 of the complete post-image when existed is true.
func (r *Recorder) Commit(abs string, existed bool, mode fs.FileMode, digest [sha256.Size]byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == nil {
		return
	}
	r.revision++
	if st, ok := r.cur.files[abs]; ok {
		if st.publicationOnly {
			r.finishTransactionLocked(&st.activeKind, &st.active)
			delete(r.cur.files, abs)
			return
		}
		after := fingerprint{existed: existed, mode: restorableMode(mode), size: committedSize(abs, existed), digest: digest}
		if !st.parentSet {
			st.parent, st.parents, st.parentSet = parentIdentity(abs)
		}
		st.after = after
		st.committed = true
		r.finishTransactionLocked(&st.activeKind, &st.active)
		return
	}
	if st, ok := r.cur.skipped[abs]; ok {
		st.committed = true
		r.finishTransactionLocked(&st.activeKind, &st.active)
	}
}

// Abort discards a prepared capture when no mutation was published. If the
// file was changed successfully earlier in the same turn, that committed
// capture and its original pre-image remain intact.
func (r *Recorder) Abort(abs string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == nil {
		return
	}
	r.revision++
	if st, ok := r.cur.files[abs]; ok {
		if st.activeKind == captureTwoPhase {
			r.finishTransactionLocked(&st.activeKind, &st.active)
		}
		if !st.committed && st.active == 0 {
			delete(r.cur.files, abs)
		}
		return
	}
	if st, ok := r.cur.skipped[abs]; ok {
		if st.activeKind == captureTwoPhase {
			r.finishTransactionLocked(&st.activeKind, &st.active)
		}
		if !st.committed && st.active == 0 {
			delete(r.cur.skipped, abs)
		}
	}
}

// PendingFiles counts what the open turn scope has captured so far,
// paths over the snapshot cap included: those were mutations too, just ones
// undo cannot cover. It is the loop's own evidence that the current turn has
// changed files — the same evidence /undo restores from — which is what lets
// a surface ask "has anything changed since I last looked" without the loop
// keeping an edit history it does not have.
func (r *Recorder) PendingFiles() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == nil {
		return 0
	}
	return len(r.cur.files) + len(r.cur.skipped)
}

// Turns lists checkpoints oldest first, including the still-open scope if
// it has captures.
func (r *Recorder) Turns() []Info {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Info, 0, len(r.turns)+1)
	for _, t := range r.turns {
		out = append(out, Info{Label: t.label, Files: len(t.files), Partial: len(t.skipped) > 0})
	}
	if r.cur != nil && (len(r.cur.files) > 0 || len(r.cur.skipped) > 0) {
		out = append(out, Info{Label: r.cur.label, Files: len(r.cur.files), Partial: len(r.cur.skipped) > 0})
	}
	return out
}

// CurrentTurn reports the exact open checkpoint scope identified by the
// caller, including an empty one. Turns intentionally omits empty scopes for
// /undo's mutation history, but /retry needs that empty scope as evidence that
// the current conversation turn changed no files and must not fall through to
// an older mutating turn.
func (r *Recorder) CurrentTurn(identity TurnIdentity) (Info, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == nil || r.cur.identity != identity {
		return Info{}, false
	}
	info := Info{Label: r.cur.label, Files: len(r.cur.files), Partial: len(r.cur.skipped) > 0}
	for path := range r.cur.skipped {
		info.Skipped = append(info.Skipped, path)
	}
	// UndoFile commits the open scope so it can consume one capture while
	// retaining the others, then reopens an empty scope with the same identity.
	// Those retained captures still belong to this exact turn and must remain
	// visible to a later /retry.
	for _, turn := range r.turns {
		if turn.identity == identity {
			info.Files += len(turn.files)
			info.Partial = info.Partial || len(turn.skipped) > 0
			for path := range turn.skipped {
				info.Skipped = append(info.Skipped, path)
			}
		}
	}
	sort.Strings(info.Skipped)
	return info, true
}

// TurnDetail is one turn's capture set for display: which files, not just
// how many. Paths are absolute and sorted; Skipped names what the snapshot
// cap kept uncovered.
type TurnDetail struct {
	Label   string
	Paths   []string
	Skipped []string
}

// Details lists checkpoints oldest first with their captured paths,
// including the still-open scope when it has captures. It is the same
// evidence Undo restores from, shaped for a surface that wants to say what
// a session touched rather than take it back.
func (r *Recorder) Details() []TurnDetail {
	r.mu.Lock()
	defer r.mu.Unlock()
	detail := func(t *Turn) TurnDetail {
		d := TurnDetail{Label: t.label}
		for path := range t.files {
			d.Paths = append(d.Paths, path)
		}
		for path := range t.skipped {
			d.Skipped = append(d.Skipped, path)
		}
		sort.Strings(d.Paths)
		sort.Strings(d.Skipped)
		return d
	}
	out := make([]TurnDetail, 0, len(r.turns)+1)
	for _, t := range r.turns {
		out = append(out, detail(t))
	}
	if r.cur != nil && (len(r.cur.files) > 0 || len(r.cur.skipped) > 0) {
		out = append(out, detail(r.cur))
	}
	return out
}

// Snapshots returns exact cloned evidence without finalizing legacy pending
// captures, consuming an undo entry, or otherwise changing recorder state.
// First-party transactions Commit synchronously, so their successful edits
// appear immediately even while the turn remains open.
func (r *Recorder) Snapshots() []TurnSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]TurnSnapshot, 0, len(r.turns)+1)
	for _, turn := range r.turns {
		out = append(out, snapshotTurnLocked(turn, false))
	}
	if r.cur != nil {
		snapshot := snapshotTurnLocked(r.cur, true)
		if len(snapshot.Files) > 0 || len(snapshot.Skipped) > 0 {
			out = append(out, snapshot)
		}
	}
	return out
}

// CurrentSnapshot reports the open turn even when it has no committed
// mutations. This lets a read-only current-turn surface distinguish a no-op
// turn from the previous closed mutating turn without changing Snapshots'
// historical filtering semantics.
func (r *Recorder) CurrentSnapshot() (TurnSnapshot, int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == nil {
		return TurnSnapshot{}, 0, false
	}
	return snapshotTurnLocked(r.cur, true), len(r.turns) + 1, true
}

// CurrentReviewCursor binds the currently open turn without cloning its file
// bytes. hasMutations reports committed write/edit evidence; ok is false when
// no turn scope is open. The cursor becomes stale on any checkpoint mutation.
func (r *Recorder) CurrentReviewCursor() (cursor ReviewCursor, index int, hasMutations, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == nil {
		return ReviewCursor{}, 0, false, false
	}
	index = len(r.turns) + 1
	return ReviewCursor{turn: r.cur, revision: r.revision, index: index, open: true}, index,
		hasReviewEvidenceLocked(r.cur), true
}

// ReviewCursorAt binds one one-based recorded mutation turn without cloning
// the other retained turns. total is the number of mutation turns addressable
// at that instant, including a mutating open turn.
func (r *Recorder) ReviewCursorAt(turn int) (cursor ReviewCursor, total int, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	total = len(r.turns)
	currentIncluded := r.cur != nil && hasReviewEvidenceLocked(r.cur)
	if currentIncluded {
		total++
	}
	if turn < 1 || turn > total {
		return ReviewCursor{}, total, false
	}
	if turn <= len(r.turns) {
		return ReviewCursor{turn: r.turns[turn-1], revision: r.revision, index: turn}, total, true
	}
	return ReviewCursor{turn: r.cur, revision: r.revision, index: turn, open: true}, total, true
}

// ReviewSnapshot clones only a bounded prefix of the exact turn selected by
// cursor. Paths are considered in bytewise order; omitted reports additional
// committed paths excluded by the file or aggregate pre-image byte limit.
func (r *Recorder) ReviewSnapshot(cursor ReviewCursor, maxFiles, maxBytes int) (TurnSnapshot, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if maxFiles < 1 || maxBytes < 0 || !r.reviewCursorCurrentLocked(cursor) {
		return TurnSnapshot{}, 0, fmt.Errorf("%w: review turn changed before it was loaded", ErrStale)
	}
	return snapshotTurnBoundedLocked(cursor.turn, cursor.open, maxFiles, maxBytes)
}

// ReviewCursorValid reports whether cursor still names the same idle recorder
// revision. It is the final guard after a bounded loader performs file I/O.
func (r *Recorder) ReviewCursorValid(cursor ReviewCursor) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reviewCursorCurrentLocked(cursor)
}

func (r *Recorder) reviewCursorCurrentLocked(cursor ReviewCursor) bool {
	if cursor.turn == nil || cursor.revision != r.revision || r.activeTransactions != 0 || r.activeRestores != 0 {
		return false
	}
	if cursor.open {
		return r.cur == cursor.turn && cursor.index == len(r.turns)+1
	}
	return cursor.index >= 1 && cursor.index <= len(r.turns) && r.turns[cursor.index-1] == cursor.turn
}

func hasReviewEvidenceLocked(turn *Turn) bool {
	if turn == nil {
		return false
	}
	for _, state := range turn.files {
		if state.committed {
			return true
		}
	}
	for _, state := range turn.skipped {
		if state.committed {
			return true
		}
	}
	return false
}

type reviewSnapshotCandidate struct {
	path    string
	skipped bool
}

func snapshotTurnBoundedLocked(turn *Turn, open bool, maxFiles, maxBytes int) (TurnSnapshot, int, error) {
	candidates := make([]reviewSnapshotCandidate, 0, maxFiles)
	total := 0
	consider := func(candidate reviewSnapshotCandidate) {
		total++
		at := sort.Search(len(candidates), func(i int) bool {
			if candidates[i].path == candidate.path {
				return !candidates[i].skipped || candidate.skipped
			}
			return candidates[i].path >= candidate.path
		})
		if len(candidates) == maxFiles && at == len(candidates) {
			return
		}
		candidates = append(candidates, reviewSnapshotCandidate{})
		copy(candidates[at+1:], candidates[at:])
		candidates[at] = candidate
		if len(candidates) > maxFiles {
			candidates = candidates[:maxFiles]
		}
	}
	for path, state := range turn.files {
		if state.committed {
			consider(reviewSnapshotCandidate{path: path})
		}
	}
	for path, state := range turn.skipped {
		if state.committed {
			consider(reviewSnapshotCandidate{path: path, skipped: true})
		}
	}

	out := TurnSnapshot{Label: turn.label, Open: open}
	omitted := total - len(candidates)
	remaining := maxBytes
	for _, candidate := range candidates {
		if candidate.skipped {
			out.Skipped = append(out.Skipped, candidate.path)
			continue
		}
		state := turn.files[candidate.path]
		if len(state.content) > remaining {
			omitted++
			continue
		}
		remaining -= len(state.content)
		out.Files = append(out.Files, MutationSnapshot{
			Path: candidate.path,
			Before: FileState{
				Existed: state.existed,
				Mode:    state.mode,
				Content: append([]byte(nil), state.content...),
			},
			After: FileFingerprint{
				Existed: state.after.existed,
				Mode:    state.after.mode,
				Digest:  state.after.digest,
			},
			turn:  turn,
			state: state,
		})
	}
	out.Partial = len(out.Skipped) > 0 || omitted > 0
	return out, omitted, nil
}

func snapshotTurnLocked(t *Turn, open bool) TurnSnapshot {
	out := TurnSnapshot{Label: t.label, Open: open}
	for path, st := range t.files {
		if !st.committed {
			continue
		}
		out.Files = append(out.Files, MutationSnapshot{
			Path: path,
			Before: FileState{
				Existed: st.existed,
				Mode:    st.mode,
				Content: append([]byte(nil), st.content...),
			},
			After: FileFingerprint{
				Existed: st.after.existed,
				Mode:    st.after.mode,
				Digest:  st.after.digest,
			},
			turn:  t,
			state: st,
		})
	}
	for path, st := range t.skipped {
		if st.committed {
			out.Skipped = append(out.Skipped, path)
		}
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Path < out.Files[j].Path })
	sort.Strings(out.Skipped)
	out.Partial = len(out.Skipped) > 0
	return out
}

// ReadSnapshotCurrent returns the exact current post-image for snapshot after
// proving that the snapshot still names live recorder evidence, the captured
// parent directory has not changed identity, and existence, mode, and complete
// content digest still match the committed After fingerprint. It never follows
// a target symlink. Callers must not fall back to reading snapshot.Path when
// this method refuses: doing so would turn stale or redirected bytes into a
// review of the recorded mutation.
//
// File I/O runs without the recorder mutex and is capped at maxFileBytes+1;
// the live token is revalidated after the read. If the expected file exceeds
// that bound, the method returns its stable existence and mode with
// ErrSnapshotTooLarge, omits Content, and does not claim its digest was checked.
func (r *Recorder) ReadSnapshotCurrent(snapshot MutationSnapshot) (FileState, error) {
	return r.readSnapshotCurrentBounded(snapshot, maxFileBytes)
}

// ReadSnapshotCurrentBounded is ReadSnapshotCurrent with a caller-supplied
// content ceiling. A matching file above the ceiling returns
// ErrSnapshotTooLarge without reading or returning its bytes.
func (r *Recorder) ReadSnapshotCurrentBounded(snapshot MutationSnapshot, maxBytes int) (FileState, error) {
	if maxBytes < 0 {
		maxBytes = 0
	}
	if maxBytes > maxFileBytes {
		maxBytes = maxFileBytes
	}
	return r.readSnapshotCurrentBounded(snapshot, maxBytes)
}

func (r *Recorder) readSnapshotCurrentBounded(snapshot MutationSnapshot, maxBytes int) (FileState, error) {
	r.mu.Lock()
	r.waitForRestoresLocked()

	st, ok := r.snapshotStateLocked(snapshot)
	if !ok {
		r.mu.Unlock()
		return FileState{}, fmt.Errorf("%w: review snapshot is no longer current", ErrStale)
	}
	if st.activeKind != captureIdle || st.active != 0 {
		r.mu.Unlock()
		return FileState{}, fmt.Errorf("%w: %s has another mutation in progress", ErrStale, snapshot.Path)
	}
	expected := st.after
	readState := &fileState{
		parent:    st.parent,
		parents:   append([]ancestorIdentity(nil), st.parents...),
		parentSet: st.parentSet,
	}
	afterOpen := r.snapshotAfterOpenHook
	r.mu.Unlock()

	current, readErr := readSnapshotCurrent(snapshot.Path, expected, readState, afterOpen, int64(maxBytes))

	r.mu.Lock()
	live, stillCurrent := r.snapshotStateLocked(snapshot)
	valid := stillCurrent && live == st && live.activeKind == captureIdle && live.active == 0 &&
		r.activeRestores == 0 && live.after == expected
	r.mu.Unlock()
	if !valid {
		return FileState{}, fmt.Errorf("%w: review snapshot changed while its post-image was read", ErrStale)
	}
	return current, readErr
}

func (r *Recorder) snapshotStateLocked(snapshot MutationSnapshot) (*fileState, bool) {
	if snapshot.turn == nil || snapshot.state == nil || snapshot.Path == "" {
		return nil, false
	}
	knownTurn := snapshot.turn == r.cur
	if !knownTurn {
		for _, turn := range r.turns {
			if turn == snapshot.turn {
				knownTurn = true
				break
			}
		}
	}
	if !knownTurn {
		return nil, false
	}
	st, ok := snapshot.turn.files[snapshot.Path]
	if !ok || st != snapshot.state || !st.committed {
		return nil, false
	}
	if snapshot.Before.Existed != st.existed ||
		snapshot.Before.Mode != st.mode ||
		!bytes.Equal(snapshot.Before.Content, st.content) ||
		snapshot.After.Existed != st.after.existed ||
		snapshot.After.Mode != st.after.mode ||
		snapshot.After.Digest != st.after.digest {
		return nil, false
	}
	return st, true
}

// StateBefore returns, for every file any turn from index turn onward
// captured, its state just before that turn ran: the oldest pre-image at
// or after it. The index is into Turns(), the still-open scope included.
// Files no turn in that range captured are absent from the map — their
// state before the turn is whatever they hold now, and the caller already
// has that. Paths a partial turn skipped are absent too, which is why a
// reconstruction over a partial turn must be refused, not attempted.
func (r *Recorder) StateBefore(turn int) map[string]FileState {
	r.mu.Lock()
	defer r.mu.Unlock()
	scopes := r.turns
	if r.cur != nil && (len(r.cur.files) > 0 || len(r.cur.skipped) > 0) {
		scopes = append(append([]*Turn(nil), r.turns...), r.cur)
	}
	out := map[string]FileState{}
	for i := len(scopes) - 1; i >= turn && i >= 0; i-- {
		for path, st := range scopes[i].files {
			out[path] = FileState{Existed: st.existed, Mode: st.mode, Content: append([]byte(nil), st.content...)}
		}
	}
	return out
}

// PrepareUndoCurrent binds a future restore to one exact durable opening and
// captures the current post-images needed to roll the workspace forward if a
// later publication fails. It does not change any file or consume checkpoint
// evidence. A partial turn is refused because no transaction can restore what
// was never captured.
func (r *Recorder) PrepareUndoCurrent(identity TurnIdentity) (*PreparedUndo, error) {
	if identity.SessionID == "" || identity.OpeningMessage < 0 {
		return nil, fmt.Errorf("%w: retry requires a durable turn identity", ErrStale)
	}
	r.restoreMu.Lock()
	defer r.restoreMu.Unlock()

	r.mu.Lock()
	r.waitForTransactionsLocked()
	r.waitForRestoresLocked()
	if r.cur == nil || r.cur.identity != identity {
		r.mu.Unlock()
		return nil, fmt.Errorf("%w: the requested checkpoint turn is not current", ErrStale)
	}
	// Legacy Record callers learn their post-image at the same seam ordinary
	// undo uses. First-party two-phase captures are already committed.
	r.finalizeLegacyLocked()
	r.revision++

	plan := &PreparedUndo{
		recorder: r,
		identity: identity,
		revision: r.revision,
		label:    r.cur.label,
	}
	seen := make(map[string]bool)
	collect := func(turn *Turn) error {
		if turn == nil || turn.identity != identity {
			return nil
		}
		for path, skipped := range turn.skipped {
			if skipped.committed {
				return fmt.Errorf("checkpoint turn is partial; %s has no restorable pre-image", path)
			}
		}
		for path, state := range turn.files {
			if !state.committed {
				continue
			}
			if seen[path] {
				return fmt.Errorf("%w: checkpoint turn contains more than one live capture for %s", ErrStale, path)
			}
			seen[path] = true
			// Apply and preparation perform filesystem I/O without Recorder.mu.
			// Snapshot every guard field now: another two-phase edit may advance
			// the live state's post-image after we unlock, and final revision
			// validation must not be the first thing preventing a data race.
			restoreState := &fileState{
				existed:   state.existed,
				mode:      state.mode,
				content:   state.content,
				after:     state.after,
				parent:    state.parent,
				parents:   append([]ancestorIdentity(nil), state.parents...),
				parentSet: state.parentSet,
			}
			plan.files = append(plan.files, preparedUndoFile{
				path: path, turn: turn, state: state, restore: restoreState,
			})
		}
		return nil
	}
	for _, turn := range r.turns {
		if err := collect(turn); err != nil {
			r.mu.Unlock()
			return nil, err
		}
	}
	if err := collect(r.cur); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	sort.Slice(plan.files, func(i, j int) bool { return plan.files[i].path < plan.files[j].path })
	if len(plan.files) > maxPreparedUndoFiles {
		r.mu.Unlock()
		return nil, fmt.Errorf("%w: %d files exceeds the %d-file limit",
			ErrPreparedUndoTooLarge, len(plan.files), maxPreparedUndoFiles)
	}
	var knownPostBytes int64
	for _, entry := range plan.files {
		if entry.restore.after.existed && entry.restore.after.size >= 0 {
			knownPostBytes += entry.restore.after.size
			if knownPostBytes > maxPreparedUndoBytes {
				r.mu.Unlock()
				return nil, fmt.Errorf("%w: current post-images exceed the %d-byte aggregate limit",
					ErrPreparedUndoTooLarge, maxPreparedUndoBytes)
			}
		}
	}
	afterOpen := r.snapshotAfterOpenHook
	r.mu.Unlock()

	// Post-images are bounded by the same cap as pre-images. If a turn grew a
	// small file beyond that cap, ordinary /undo can still be best-effort; a
	// transactional retry must refuse because it could not roll a failed
	// adoption back to the exact post-turn workspace.
	var postBytes int64
	for i := range plan.files {
		entry := &plan.files[i]
		post, err := readSnapshotCurrent(entry.path, entry.restore.after, entry.restore, afterOpen, maxFileBytes)
		if err != nil {
			return nil, fmt.Errorf("preparing retry restore for %s: %w", entry.path, err)
		}
		postBytes += int64(len(post.Content))
		if postBytes > maxPreparedUndoBytes {
			return nil, fmt.Errorf("%w: current post-images exceed the %d-byte aggregate limit",
				ErrPreparedUndoTooLarge, maxPreparedUndoBytes)
		}
		entry.post = post
	}

	r.mu.Lock()
	valid := r.preparedUndoCurrentLocked(plan)
	r.mu.Unlock()
	if !valid {
		return nil, fmt.Errorf("%w: checkpoint turn changed while its retry restore was prepared", ErrStale)
	}
	return plan, nil
}

func (r *Recorder) preparedUndoCurrentLocked(plan *PreparedUndo) bool {
	if plan == nil || plan.recorder != r || r.cur == nil || r.cur.identity != plan.identity ||
		r.revision != plan.revision || r.activeTransactions != 0 || r.activeRestores != 0 {
		return false
	}
	for _, entry := range plan.files {
		known := entry.turn == r.cur
		if !known {
			for _, turn := range r.turns {
				if turn == entry.turn {
					known = true
					break
				}
			}
		}
		if !known || entry.turn.identity != plan.identity || entry.turn.files[entry.path] != entry.state ||
			!entry.state.committed || entry.restore == nil || entry.state.after != entry.restore.after {
			return false
		}
	}
	for _, turn := range append(append([]*Turn(nil), r.turns...), r.cur) {
		if turn.identity != plan.identity {
			continue
		}
		for _, skipped := range turn.skipped {
			if skipped.committed {
				return false
			}
		}
	}
	return true
}

// Apply publishes a prepared turn restore without a separate commit action.
func (p *PreparedUndo) Apply() (PreparedUndoResult, error) {
	return p.ApplyAndCommit(nil)
}

// ApplyAndCommit publishes every prepared pre-image and then invokes commit
// while the recorder still excludes other captures and restores. Evidence is
// consumed only if both phases succeed. A restore or commit failure rolls every
// published path forward to its prepared post-image before returning, so the
// caller may leave the source conversation active without splitting it from
// its workspace.
func (p *PreparedUndo) ApplyAndCommit(commit func() error) (PreparedUndoResult, error) {
	var durableCommit func() (DurableCommitOutcome, error)
	if commit != nil {
		durableCommit = func() (DurableCommitOutcome, error) {
			err := commit()
			return DurableCommitOutcome{Published: err == nil, Durable: err == nil}, err
		}
	}
	return p.applyAndCommit(durableCommit)
}

// ApplyAndCommitDurably preserves a visible-but-not-durable publication as a
// committed restore while retaining the recovery journal. This is the only
// safe response once another process could discover the child.
func (p *PreparedUndo) ApplyAndCommitDurably(commit func() (DurableCommitOutcome, error)) (PreparedUndoResult, error) {
	return p.applyAndCommit(commit)
}

func (p *PreparedUndo) applyAndCommit(commit func() (DurableCommitOutcome, error)) (PreparedUndoResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.used || p.recorder == nil {
		return PreparedUndoResult{}, fmt.Errorf("%w: prepared undo was already used", ErrStale)
	}
	if p.journal != nil && commit == nil {
		return PreparedUndoResult{}, errors.New("a durable retry restore requires a publication commit")
	}
	p.used = true
	r := p.recorder

	removeJournal := func() error {
		if p.journal == nil {
			return nil
		}
		err := p.journal.remove()
		if p.journal.file == nil {
			p.journal = nil
		}
		return err
	}
	retainJournal := func() error {
		if p.journal == nil {
			return nil
		}
		err := p.journal.close()
		p.journal = nil
		return err
	}
	failBeforeMutation := func(cause error) (PreparedUndoResult, error) {
		if cleanupErr := removeJournal(); cleanupErr != nil {
			return PreparedUndoResult{}, errors.Join(ErrDurableUndoRecoveryRequired, cause,
				fmt.Errorf("removing unused retry journal: %w", cleanupErr))
		}
		return PreparedUndoResult{}, cause
	}
	failAfterRollback := func(cause, rollbackErr error) (PreparedUndoResult, error) {
		if rollbackErr != nil {
			// The synced journal remains authoritative. Recovery will finish
			// rolling every recorded pre-image forward before another session is
			// adopted, or refuse on any unrecognised third state.
			return PreparedUndoResult{}, errors.Join(ErrDurableUndoRecoveryRequired, cause,
				retainJournal(),
				fmt.Errorf("restoring the post-turn workspace also failed; retry journal retained: %w", rollbackErr))
		}
		if cleanupErr := removeJournal(); cleanupErr != nil {
			return PreparedUndoResult{}, errors.Join(ErrDurableUndoRecoveryRequired,
				fmt.Errorf("%w; the file restore was rolled back", cause),
				fmt.Errorf("post-turn files were restored but removing the retry journal failed: %w", cleanupErr))
		}
		return PreparedUndoResult{}, fmt.Errorf("%w; the file restore was rolled back", cause)
	}

	r.restoreMu.Lock()
	defer r.restoreMu.Unlock()
	r.mu.Lock()
	r.waitForTransactionsLocked()
	if !r.preparedUndoCurrentLocked(p) {
		r.mu.Unlock()
		return failBeforeMutation(fmt.Errorf("%w: checkpoint turn changed before retry adoption", ErrStale))
	}
	r.revision++
	hook := r.startRestoreLocked()
	hooks := restoreHooks{
		beforeRemove:    r.beforeRemoveHook,
		beforeReplace:   r.beforeReplaceHook,
		publicationSeam: r.publicationSeamHook,
		afterRemove:     r.afterRemoveHook,
		afterReplace:    r.afterReplaceHook,
	}
	r.mu.Unlock()
	defer r.finishRestore()
	if hook != nil {
		hook()
	}

	// Validate every compare-and-swap guard before touching the first path. A
	// stale editor write therefore leaves the whole workspace in its post-turn
	// state instead of restoring an arbitrary prefix of the turn.
	for _, entry := range p.files {
		current, err := readSnapshotCurrent(entry.path, entry.restore.after, entry.restore, nil, maxFileBytes)
		if err != nil {
			return failBeforeMutation(fmt.Errorf("validating retry restore for %s: %w", entry.path, err))
		}
		if !sameFileState(current, entry.post) {
			return failBeforeMutation(fmt.Errorf("%w: %s changed after retry restore preparation", ErrStale, entry.path))
		}
	}

	result := PreparedUndoResult{Label: p.label}
	published := make([]preparedUndoFile, 0, len(p.files))
	if p.journal != nil && r.durableUndoHook != nil {
		r.durableUndoHook(durableUndoBeforeRestore, -1)
	}
	for i, entry := range p.files {
		entryHooks := hooks
		entryHooks.tempName = entry.tempName
		outcome := r.restoreWithLedger(entry.path, entry.restore, entryHooks)
		if outcome.published {
			published = append(published, entry)
			if entry.restore.existed {
				result.Restored = append(result.Restored, entry.path)
			} else {
				result.Removed = append(result.Removed, entry.path)
			}
		}
		if outcome.err != nil {
			rollbackErr := rollbackPreparedUndo(r, published)
			return failAfterRollback(
				fmt.Errorf("retry restore failed for %s: %w", entry.path, outcome.err),
				rollbackErr,
			)
		}
		if p.journal != nil && r.durableUndoHook != nil {
			r.durableUndoHook(durableUndoAfterRestore, i)
		}
	}
	publicationUncertain := false
	if commit != nil {
		if p.journal != nil && r.durableUndoHook != nil {
			r.durableUndoHook(durableUndoBeforeCommit, -1)
		}
		outcome, err := commit()
		if !outcome.Published {
			if err == nil {
				err = errors.New("retry publication returned neither an error nor a published outcome")
			}
			rollbackErr := rollbackPreparedUndo(r, published)
			return failAfterRollback(
				fmt.Errorf("retry adoption failed after file restore: %w", err),
				rollbackErr,
			)
		}
		if !outcome.Durable {
			publicationUncertain = true
			if err == nil {
				err = errors.New("publication became visible before its durability was proven")
			}
			result.RecoveryRequired = errors.Join(ErrDurableUndoRecoveryRequired, err)
			if closeErr := retainJournal(); closeErr != nil {
				result.RecoveryRequired = errors.Join(result.RecoveryRequired,
					fmt.Errorf("releasing retained retry journal: %w", closeErr))
			}
		} else if err != nil {
			result.CleanupWarning = fmt.Errorf("retry publication completed with a warning: %w", err)
		}
		if p.journal != nil && r.durableUndoHook != nil {
			r.durableUndoHook(durableUndoAfterCommit, -1)
		}
	}
	// Publication is the commit record. A cleanup failure cannot turn a
	// successful publication back into an error: doing so would make the caller
	// re-adopt the source while the child is already discoverable. Startup
	// recovery will validate the child and remove a retained journal.
	if !publicationUncertain {
		if cleanupErr := removeJournal(); cleanupErr != nil {
			result.CleanupWarning = errors.Join(result.CleanupWarning,
				fmt.Errorf("retry committed but its recovery journal could not be removed: %w", cleanupErr))
		}
	}

	// File publication and the caller's adoption commit succeeded everywhere.
	// Only now consume the inverse evidence; a failed or rolled-back operation
	// leaves it available for /undo or a new retry.
	r.mu.Lock()
	r.revision++
	for _, entry := range p.files {
		delete(entry.turn.files, entry.path)
	}
	kept := r.turns[:0]
	for _, turn := range r.turns {
		if len(turn.files) == 0 && len(turn.skipped) == 0 {
			continue
		}
		kept = append(kept, turn)
	}
	r.turns = kept
	r.mu.Unlock()
	return result, nil
}

func sameFileState(a, b FileState) bool {
	return a.Existed == b.Existed && (!a.Existed || a.Mode == b.Mode && bytes.Equal(a.Content, b.Content))
}

func rollbackPreparedUndo(recorder *Recorder, published []preparedUndoFile) error {
	var rollbackErr error
	for i := len(published) - 1; i >= 0; i-- {
		entry := published[i]
		expected := fingerprintBytes(entry.restore.existed, entry.restore.mode, entry.restore.content)
		target := fingerprintBytes(entry.post.Existed, entry.post.Mode, entry.post.Content)
		// A matching post-image reached through a rebound parent is not this
		// transaction's file. Check the captured namespace identity before the
		// fast-path comparison so rollback can never mistake an outside twin for
		// an already-restored target.
		if err := validateParentIdentity(entry.path, entry.restore); err != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("%s: %w", entry.path, err))
			continue
		}
		current, err := fingerprintPath(entry.path)
		if err != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("%s: %w", entry.path, err))
			continue
		}
		if sameFingerprint(current, target) {
			continue
		}
		if !sameFingerprint(current, expected) {
			rollbackErr = errors.Join(rollbackErr,
				fmt.Errorf("%w: %s changed again before retry rollback", ErrStale, entry.path))
			continue
		}
		state := &fileState{
			existed:   entry.post.Existed,
			mode:      entry.post.Mode,
			content:   append([]byte(nil), entry.post.Content...),
			after:     expected,
			parent:    entry.restore.parent,
			parents:   append([]ancestorIdentity(nil), entry.restore.parents...),
			parentSet: entry.restore.parentSet,
		}
		outcome := recorder.restoreWithLedger(entry.path, state, restoreHooks{tempName: entry.tempName})
		if outcome.err != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("%s: %w", entry.path, outcome.err))
		}
	}
	return rollbackErr
}

// UndoFile restores one file to what it was before the newest turn that
// captured it, and consumes that capture, so a later whole-turn /undo does
// not restore it twice. The turn's other files stay on the stack: taking
// back one file is not taking back the turn. A turn left with nothing is
// dropped, the same rule Begin applies to a scope that captured nothing.
// Outcome.Published can be true alongside a non-nil error: remove/replace
// succeeded, but a later durability or final-state check did not. Such a
// capture is consumed because retrying it would compare against stale evidence.
func (r *Recorder) UndoFile(abs string) (outcome UndoFileOutcome, label string, err error) {
	r.restoreMu.Lock()
	defer r.restoreMu.Unlock()
	r.mu.Lock()
	r.waitForTransactionsLocked()
	r.revision++
	resumeIdentity, resumeLabel, hadOpenScope := TurnIdentity{}, "", r.cur != nil
	if hadOpenScope {
		resumeIdentity = r.cur.identity
		resumeLabel = r.cur.label
	}
	r.commitLocked()
	var turn *Turn
	for i := len(r.turns) - 1; i >= 0; i-- {
		if _, ok := r.turns[i].files[abs]; ok {
			turn = r.turns[i]
			break
		}
	}
	if turn == nil {
		if hadOpenScope {
			r.cur = newTurn(resumeIdentity, resumeLabel)
		}
		r.mu.Unlock()
		return UndoFileOutcome{}, "", fmt.Errorf("no turn captured %s, as far as write and edit saw", abs)
	}
	st := turn.files[abs]
	if !hadOpenScope {
		resumeIdentity = turn.identity
		resumeLabel = turn.label
	}
	// Keep an empty capture scope available before waking any RecordState
	// waiter. A mutation that was blocked behind this restore must never wake
	// to nil and then publish without checkpoint evidence.
	r.cur = newTurn(resumeIdentity, resumeLabel)
	hook := r.startRestoreLocked()
	hooks := restoreHooks{
		beforeRemove:    r.beforeRemoveHook,
		beforeReplace:   r.beforeReplaceHook,
		publicationSeam: r.publicationSeamHook,
		afterRemove:     r.afterRemoveHook,
		afterReplace:    r.afterReplaceHook,
	}
	label = turn.label
	r.mu.Unlock()
	defer r.finishRestore()
	if hook != nil {
		hook()
	}

	// A pre-publication failure leaves the one copy of the old content available
	// for inspection or retry. Once remove/replace succeeds, consume the capture
	// even if a later durability or verification check reports an error.
	restored := r.restoreWithLedger(abs, st, hooks)
	if !restored.published {
		return UndoFileOutcome{}, label, restored.err
	}
	outcome = UndoFileOutcome{Published: true, Removed: !st.existed}

	r.mu.Lock()
	delete(turn.files, abs)
	if len(turn.files) == 0 && len(turn.skipped) == 0 {
		for i, t := range r.turns {
			if t == turn {
				r.turns = append(r.turns[:i], r.turns[i+1:]...)
				break
			}
		}
	}
	r.mu.Unlock()
	return outcome, label, restored.err
}

// Undo restores the most recent turn that changed files and reports the
// restored and removed paths, sorted, plus anything the cap kept it from
// covering. Restore-or-report is per file: one unwritable path does not
// abandon the rest, it gets named. A path whose remove/replace was published
// before a later durability or verification error appears in both its changed
// list and failed; callers must invalidate every path in the changed lists.
func (r *Recorder) Undo() (restored, removed, skipped, failed []string, label string, err error) {
	return r.undo(nil)
}

// UndoCurrent restores only the exact open conversation turn named by
// identity. It never falls back to an older turn that happened to share the
// same display label. A missing, replaced, or empty current scope is refused
// or returned as a no-op without consuming historical checkpoint evidence.
func (r *Recorder) UndoCurrent(identity TurnIdentity) (restored, removed, skipped, failed []string, label string, err error) {
	return r.undo(&identity)
}

func (r *Recorder) undo(expected *TurnIdentity) (restored, removed, skipped, failed []string, label string, err error) {
	r.restoreMu.Lock()
	defer r.restoreMu.Unlock()
	r.mu.Lock()
	r.waitForTransactionsLocked()
	if expected != nil {
		if r.cur == nil || r.cur.identity != *expected {
			r.mu.Unlock()
			return nil, nil, nil, nil, "", fmt.Errorf("the requested checkpoint turn is not the current turn")
		}
		hasCaptures := len(r.cur.files) > 0 || len(r.cur.skipped) > 0
		if !hasCaptures {
			for _, turn := range r.turns {
				if turn.identity == *expected && (len(turn.files) > 0 || len(turn.skipped) > 0) {
					hasCaptures = true
					break
				}
			}
		}
		if !hasCaptures {
			r.mu.Unlock()
			return nil, nil, nil, nil, r.cur.label, nil
		}
	}
	r.revision++
	resumeIdentity, resumeLabel, hadOpenScope := TurnIdentity{}, "", r.cur != nil
	if hadOpenScope {
		resumeIdentity = r.cur.identity
		resumeLabel = r.cur.label
	}
	r.commitLocked()
	if len(r.turns) == 0 {
		if hadOpenScope {
			r.cur = newTurn(resumeIdentity, resumeLabel)
		}
		r.mu.Unlock()
		return nil, nil, nil, nil, "", fmt.Errorf("nothing to undo: no turn has changed files")
	}
	turn := r.turns[len(r.turns)-1]
	if expected != nil {
		turn = nil
		for i := len(r.turns) - 1; i >= 0; i-- {
			if r.turns[i].identity == *expected {
				turn = r.turns[i]
				break
			}
		}
		if turn == nil {
			// Refuse before touching the workspace if recorder invariants ever
			// change between the identity check and scope commit above.
			r.cur = newTurn(resumeIdentity, resumeLabel)
			r.mu.Unlock()
			return nil, nil, nil, nil, "", fmt.Errorf("the requested checkpoint turn is no longer current")
		}
	}
	if !hadOpenScope {
		resumeIdentity = turn.identity
		resumeLabel = turn.label
	}
	r.cur = newTurn(resumeIdentity, resumeLabel)
	label = turn.label
	for p := range turn.skipped {
		skipped = append(skipped, p)
	}
	// Oversize captures were never restorable. Report and consume those
	// markers now; failed file restores stay on this turn and make a later
	// /undo a retry rather than silently advancing to an older turn.
	turn.skipped = map[string]*skippedState{}
	hook := r.startRestoreLocked()
	hooks := restoreHooks{
		beforeRemove:    r.beforeRemoveHook,
		beforeReplace:   r.beforeReplaceHook,
		publicationSeam: r.publicationSeamHook,
		afterRemove:     r.afterRemoveHook,
		afterReplace:    r.afterReplaceHook,
	}
	r.mu.Unlock()
	defer r.finishRestore()
	if hook != nil {
		hook()
	}

	paths := make([]string, 0, len(turn.files))
	for p := range turn.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, p := range paths {
		st := turn.files[p]
		outcome := r.restoreWithLedger(p, st, hooks)
		if outcome.err != nil {
			failed = append(failed, p+": "+outcome.err.Error())
		}
		if !outcome.published {
			continue
		}
		if st.existed {
			restored = append(restored, p)
		} else {
			removed = append(removed, p)
		}
		r.mu.Lock()
		delete(turn.files, p)
		r.mu.Unlock()
	}

	sort.Strings(skipped)
	r.mu.Lock()
	if len(turn.files) == 0 && len(turn.skipped) == 0 {
		for i, candidate := range r.turns {
			if candidate == turn {
				r.turns = append(r.turns[:i], r.turns[i+1:]...)
				break
			}
		}
	}
	r.mu.Unlock()
	return restored, removed, skipped, failed, label, nil
}

func fingerprintBytes(existed bool, mode fs.FileMode, content []byte) fingerprint {
	fp := fingerprint{existed: existed}
	if !existed {
		return fp
	}
	fp.mode = restorableMode(mode)
	fp.size = int64(len(content))
	fp.digest = sha256.Sum256(content)
	return fp
}

func committedSize(path string, existed bool) int64 {
	if !existed {
		return 0
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return -1
	}
	return info.Size()
}

func readSnapshotCurrent(path string, expected fingerprint, st *fileState, afterOpen func(), maxBytes int64) (FileState, error) {
	return readSnapshotCurrentWithHooks(path, expected, st, nil, afterOpen, maxBytes)
}

func readSnapshotCurrentWithHooks(path string, expected fingerprint, st *fileState, beforeOpen, afterOpen func(), maxBytes int64) (FileState, error) {
	if err := validateParentIdentity(path, st); err != nil {
		return FileState{}, err
	}

	linfo, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) && !expected.existed {
			if err := validateParentIdentity(path, st); err != nil {
				return FileState{}, err
			}
			return FileState{}, nil
		}
		if os.IsNotExist(err) {
			return FileState{}, fmt.Errorf("%w: %s no longer exists", ErrStale, path)
		}
		return FileState{}, err
	}
	if !expected.existed {
		return FileState{}, fmt.Errorf("%w: %s exists after a recorded deletion", ErrStale, path)
	}
	if !linfo.Mode().IsRegular() {
		return FileState{}, fmt.Errorf("%w: %s is not a regular file", ErrStale, path)
	}
	if restorableMode(linfo.Mode()) != expected.mode || (expected.size >= 0 && linfo.Size() != expected.size) {
		return FileState{}, fmt.Errorf("%w: %s size or mode changed after the recorded mutation", ErrStale, path)
	}

	if beforeOpen != nil {
		beforeOpen()
	}
	f, err := openCheckpointPathRead(path)
	if err != nil {
		return FileState{}, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return FileState{}, err
	}
	if !os.SameFile(linfo, opened) {
		return FileState{}, fmt.Errorf("%w: %s changed identity while it was opened", ErrStale, path)
	}
	if afterOpen != nil {
		afterOpen()
	}
	if restorableMode(opened.Mode()) != expected.mode || (expected.size >= 0 && opened.Size() != expected.size) {
		return FileState{}, fmt.Errorf("%w: %s size or mode changed while it was opened", ErrStale, path)
	}

	if opened.Size() > maxBytes {
		if err := validateSnapshotFileObservation(path, f, opened); err != nil {
			return FileState{}, err
		}
		if err := validateParentIdentity(path, st); err != nil {
			return FileState{}, err
		}
		return FileState{Existed: true, Mode: restorableMode(opened.Mode())},
			fmt.Errorf("%w: %s is larger than %d bytes; digest was not reverified", ErrSnapshotTooLarge, path, maxBytes)
	}

	h := sha256.New()
	var content bytes.Buffer
	if opened.Size() > 0 {
		content.Grow(int(opened.Size()))
	}
	n, err := io.Copy(io.MultiWriter(h, &content), io.LimitReader(f, maxBytes+1))
	if err != nil {
		return FileState{}, err
	}
	if n != opened.Size() || n > maxBytes {
		return FileState{}, fmt.Errorf("%w: %s changed size while it was read", ErrStale, path)
	}
	if err := validateSnapshotFileObservation(path, f, opened); err != nil {
		return FileState{}, err
	}
	actual := fingerprint{existed: true, mode: restorableMode(opened.Mode()), size: n}
	copy(actual.digest[:], h.Sum(nil))
	if !sameFingerprint(actual, expected) {
		return FileState{}, fmt.Errorf("%w: %s changed after the recorded mutation", ErrStale, path)
	}
	if err := validateParentIdentity(path, st); err != nil {
		return FileState{}, err
	}

	current := FileState{Existed: true, Mode: actual.mode}
	current.Content = append([]byte(nil), content.Bytes()...)
	return current, nil
}

func validateSnapshotFileObservation(path string, f *os.File, opened fs.FileInfo) error {
	finished, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(opened, finished) || opened.Size() != finished.Size() ||
		!opened.ModTime().Equal(finished.ModTime()) || restorableMode(opened.Mode()) != restorableMode(finished.Mode()) {
		return fmt.Errorf("%w: %s changed while it was read", ErrStale, path)
	}
	linked, err := os.Lstat(path)
	if err != nil || !linked.Mode().IsRegular() || !os.SameFile(finished, linked) ||
		linked.Size() != finished.Size() || !linked.ModTime().Equal(finished.ModTime()) ||
		restorableMode(linked.Mode()) != restorableMode(finished.Mode()) {
		return fmt.Errorf("%w: %s changed identity while it was read", ErrStale, path)
	}
	return nil
}

func fingerprintPath(path string) (fingerprint, error) {
	return fingerprintPathBoundedWithHook(path, -1, nil)
}

func fingerprintPathWithHook(path string, afterHash func()) (fingerprint, error) {
	return fingerprintPathBoundedWithHook(path, -1, afterHash)
}

func fingerprintPathBounded(path string, maxBytes int64) (fingerprint, error) {
	return fingerprintPathBoundedWithHook(path, maxBytes, nil)
}

func fingerprintPathBoundedWithHook(path string, maxBytes int64, afterHash func()) (fingerprint, error) {
	return fingerprintPathBoundedWithHooks(path, maxBytes, nil, afterHash)
}

func fingerprintPathBoundedWithHooks(path string, maxBytes int64, beforeOpen, afterHash func()) (fingerprint, error) {
	linfo, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fingerprint{}, nil
		}
		return fingerprint{}, err
	}
	if !linfo.Mode().IsRegular() {
		return fingerprint{}, fmt.Errorf("%s is not a regular file", path)
	}
	if maxBytes >= 0 && linfo.Size() > maxBytes {
		return fingerprint{}, fmt.Errorf("%w: %s exceeds the %d-byte fingerprint bound", ErrSnapshotTooLarge, path, maxBytes)
	}
	if beforeOpen != nil {
		beforeOpen()
	}
	f, err := openCheckpointPathRead(path)
	if err != nil {
		return fingerprint{}, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return fingerprint{}, err
	}
	if !os.SameFile(linfo, opened) {
		return fingerprint{}, fmt.Errorf("%s changed identity while it was opened", path)
	}
	h := sha256.New()
	reader := io.Reader(f)
	if maxBytes >= 0 {
		reader = io.LimitReader(f, maxBytes+1)
	}
	n, err := io.Copy(h, reader)
	if err != nil {
		return fingerprint{}, err
	}
	if maxBytes >= 0 && n > maxBytes {
		return fingerprint{}, fmt.Errorf("%w: %s grew beyond the %d-byte fingerprint bound", ErrSnapshotTooLarge, path, maxBytes)
	}
	finished, err := f.Stat()
	if err != nil {
		return fingerprint{}, err
	}
	if !os.SameFile(opened, finished) || opened.Size() != finished.Size() || finished.Size() != n ||
		!opened.ModTime().Equal(finished.ModTime()) || restorableMode(opened.Mode()) != restorableMode(finished.Mode()) {
		return fingerprint{}, fmt.Errorf("%s changed while it was fingerprinted", path)
	}
	if afterHash != nil {
		afterHash()
	}
	linked, err := os.Lstat(path)
	if err != nil || !linked.Mode().IsRegular() || !os.SameFile(finished, linked) ||
		linked.Size() != finished.Size() || !linked.ModTime().Equal(finished.ModTime()) ||
		restorableMode(linked.Mode()) != restorableMode(finished.Mode()) {
		return fingerprint{}, fmt.Errorf("%w: %s changed identity while it was fingerprinted", ErrStale, path)
	}
	fp := fingerprint{existed: true, mode: restorableMode(finished.Mode()), size: finished.Size()}
	copy(fp.digest[:], h.Sum(nil))
	return fp, nil
}

func sameFingerprint(a, b fingerprint) bool {
	if a.existed != b.existed {
		return false
	}
	if !a.existed {
		return true
	}
	if a.mode != b.mode || a.digest != b.digest {
		return false
	}
	return a.size < 0 || b.size < 0 || a.size == b.size
}

func parentIdentity(path string) (fs.FileInfo, []ancestorIdentity, bool) {
	if !filepath.IsAbs(path) {
		return nil, nil, false
	}
	parent := filepath.Clean(filepath.Dir(path))
	var reverse []string
	for current := parent; ; current = filepath.Dir(current) {
		reverse = append(reverse, current)
		next := filepath.Dir(current)
		if next == current {
			break
		}
	}
	ancestors := make([]ancestorIdentity, 0, len(reverse))
	for i := len(reverse) - 1; i >= 0; i-- {
		info, err := os.Lstat(reverse[i])
		if err != nil || (!info.IsDir() && info.Mode()&fs.ModeSymlink == 0) {
			return nil, nil, false
		}
		ancestors = append(ancestors, ancestorIdentity{path: reverse[i], info: info})
	}
	if len(ancestors) == 0 {
		return nil, nil, false
	}
	immediate := ancestors[len(ancestors)-1].info
	if !immediate.IsDir() || immediate.Mode()&fs.ModeSymlink != 0 {
		return nil, nil, false
	}
	return immediate, ancestors, true
}

func validateParentIdentity(path string, st *fileState) error {
	if !st.parentSet || st.parent == nil || len(st.parents) == 0 {
		return fmt.Errorf("%w: no trustworthy parent identity was captured for %s", ErrStale, path)
	}
	for i, captured := range st.parents {
		role := "ancestor"
		if i == len(st.parents)-1 {
			role = "parent"
		}
		current, err := os.Lstat(captured.path)
		if err != nil {
			return fmt.Errorf("%w: cannot verify %s %s of %s: %v", ErrStale, role, captured.path, path, err)
		}
		capturedSymlink := captured.info.Mode()&fs.ModeSymlink != 0
		currentSymlink := current.Mode()&fs.ModeSymlink != 0
		if capturedSymlink != currentSymlink || captured.info.IsDir() != current.IsDir() ||
			!os.SameFile(captured.info, current) {
			return fmt.Errorf("%w: %s %s of %s changed identity", ErrStale, role, captured.path, path)
		}
		if i == len(st.parents)-1 && (!current.IsDir() || currentSymlink) {
			return fmt.Errorf("%w: parent of %s is no longer a real directory", ErrStale, path)
		}
	}
	return nil
}

type restoreHooks struct {
	prepareTemp      func(*os.File) error
	fingerprintLimit int64
	displayPath      string
	beforeRemove     func() error
	beforeReplace    func() error
	publicationSeam  func() error
	afterRemove      func() error
	afterReplace     func() error
	tempName         string
	bindTemp         func(*os.File, bool) error
	bindDisplaced    func(*os.File) error
	retirementRoot   *os.Root
}

type restoreOutcome struct {
	published      bool
	cleanupPending bool
	err            error
}

func unpublishedRestore(err error) restoreOutcome {
	return restoreOutcome{err: err}
}

func publishedRestoreError(path string, err error) restoreOutcome {
	return restoreOutcome{
		published:      true,
		cleanupPending: true,
		err:            fmt.Errorf("restore was published for %s, but durability or final-state verification failed: %w", path, err),
	}
}

func restore(path string, st *fileState, hooks restoreHooks) restoreOutcome {
	return restoreInScope(nil, path, st, hooks)
}

func restoreInScope(scope *restoreScope, path string, st *fileState, hooks restoreHooks) restoreOutcome {
	displayPath := path
	if hooks.displayPath != "" {
		displayPath = hooks.displayPath
	}
	// The post-image comparison remains cooperative with an external writer,
	// but the directory capability is held from that comparison through the
	// irreversible operation. A parent rename or symlink swap can therefore
	// make the restore stale, but can never redirect it to another directory.
	// Read-only turn review never calls this function.
	parent, err := openBoundRestoreParent(scope, path, st)
	if err != nil {
		return unpublishedRestore(err)
	}
	defer parent.close()
	namespace, err := openBoundRestoreNamespace(scope, parent, path)
	if err != nil {
		return unpublishedRestore(err)
	}
	current, err := parent.fingerprint(restoreFingerprintLimit(hooks))
	if err != nil {
		return unpublishedRestore(err)
	}
	if !sameFingerprint(current, st.after) {
		return unpublishedRestore(fmt.Errorf("%w: %s changed after the recorded mutation; refusing to overwrite it", ErrStale, path))
	}
	if err := ensureRetirementCompatible(namespace.root, hooks.retirementRoot); err != nil {
		return unpublishedRestore(fmt.Errorf("binding checkpoint retirement storage: %w", err))
	}
	if !st.existed {
		if hooks.beforeRemove != nil {
			if err := hooks.beforeRemove(); err != nil {
				return unpublishedRestore(err)
			}
		}
		seam := hooks.publicationSeam
		published, removeErr := removeBoundTarget(namespace, parent, hooks.retirementRoot, hooks.tempName, hooks.bindTemp, st.after, seam)
		if removeErr != nil {
			if published {
				return publishedRestoreError(displayPath, removeErr)
			}
			return unpublishedRestore(removeErr)
		}
		if hooks.afterRemove != nil {
			if err := hooks.afterRemove(); err != nil {
				return publishedRestoreError(displayPath, err)
			}
		}
		if err := syncBoundDirectory(parent.root); err != nil {
			return publishedRestoreError(displayPath, fmt.Errorf("syncing parent directory: %w", err))
		}
		check, err := parent.fingerprint(restoreFingerprintLimit(hooks))
		if err != nil {
			return publishedRestoreError(displayPath, fmt.Errorf("verifying removal: %w", err))
		}
		if check.existed {
			return publishedRestoreError(displayPath, errors.New("file still exists after removal"))
		}
		if len(st.parents) > 0 {
			if err := validateParentIdentity(path, st); err != nil {
				return publishedRestoreError(displayPath, err)
			}
		} else if scope != nil {
			if err := scope.validateLinked(); err != nil {
				return publishedRestoreError(displayPath, err)
			}
		}
		return restoreOutcome{published: true}
	}
	return atomicRestoreBound(scope, namespace, parent, path, st.content, st.mode, st.after, st, hooks)
}

func restoreFingerprintLimit(hooks restoreHooks) int64 {
	if hooks.fingerprintLimit > 0 {
		return hooks.fingerprintLimit
	}
	return -1
}

func atomicRestore(path string, content []byte, mode fs.FileMode, expected fingerprint, st *fileState, hooks restoreHooks) restoreOutcome {
	parent, err := openBoundRestoreParent(nil, path, st)
	if err != nil {
		return unpublishedRestore(err)
	}
	defer parent.close()
	namespace, err := openBoundRestoreNamespace(nil, parent, path)
	if err != nil {
		return unpublishedRestore(err)
	}
	return atomicRestoreBound(nil, namespace, parent, path, content, mode, expected, st, hooks)
}

func atomicRestoreBound(scope *restoreScope, namespace *boundRestoreNamespace, parent *boundRestoreParent, path string, content []byte, mode fs.FileMode, expected fingerprint, st *fileState, hooks restoreHooks) (outcome restoreOutcome) {
	displayPath := path
	if hooks.displayPath != "" {
		displayPath = hooks.displayPath
	}
	if namespace == nil || namespace.root == nil {
		return unpublishedRestore(errors.New("restore namespace is closed"))
	}
	if err := ensureRetirementCompatible(namespace.root, hooks.retirementRoot); err != nil {
		return unpublishedRestore(fmt.Errorf("binding checkpoint retirement storage: %w", err))
	}
	tmp, _, tmpRel, err := namespace.createTemp(hooks.tempName)
	if err != nil {
		return unpublishedRestore(err)
	}
	publishedTemp := false
	retainTemp := false
	outcome.cleanupPending = true
	defer func() {
		if !publishedTemp && !retainTemp {
			cleanupErr := retireBoundOpenFileTo(namespace.root, hooks.retirementRoot, tmpRel, tmp, true, nil, nil)
			outcome.cleanupPending = cleanupErr != nil
			if cleanupErr != nil {
				if outcome.published {
					outcome = publishedRestoreError(displayPath, errors.Join(outcome.err,
						fmt.Errorf("retiring unpublished checkpoint temporary: %w", cleanupErr)))
				} else {
					outcome.err = errors.Join(outcome.err,
						fmt.Errorf("retiring unpublished checkpoint temporary: %w", cleanupErr))
				}
			}
		}
		if closeErr := tmp.Close(); closeErr != nil {
			outcome.err = errors.Join(outcome.err, closeErr)
		}
	}()
	if hooks.prepareTemp != nil {
		if err := hooks.prepareTemp(tmp); err != nil {
			return unpublishedRestore(fmt.Errorf("preparing checkpoint temporary: %w", err))
		}
	}
	// Bind and durably record the inode before writing any pre-image bytes.
	// A crash between O_EXCL creation and this record can leave only an empty
	// 0600 file; recovery never deletes an exact-name file without this positive
	// inode identity.
	if hooks.bindTemp != nil {
		if err := hooks.bindTemp(tmp, true); err != nil {
			return unpublishedRestore(err)
		}
	}
	if err := writeAll(tmp, content); err != nil {
		return unpublishedRestore(err)
	}
	if err := tmp.Chmod(mode); err != nil {
		return unpublishedRestore(err)
	}
	if err := tmp.Sync(); err != nil {
		return unpublishedRestore(err)
	}
	tmpInfo, err := tmp.Stat()
	if err != nil {
		return unpublishedRestore(err)
	}
	currentTmp, err := namespace.root.Lstat(tmpRel)
	if err != nil || !currentTmp.Mode().IsRegular() || !os.SameFile(tmpInfo, currentTmp) {
		return unpublishedRestore(errors.Join(err, fmt.Errorf("%w: undo temporary file changed identity", ErrStale)))
	}
	current, err := parent.fingerprint(restoreFingerprintLimit(hooks))
	if err != nil {
		return unpublishedRestore(err)
	}
	if !sameFingerprint(current, expected) {
		return unpublishedRestore(fmt.Errorf("%w: %s changed while undo was being prepared", ErrStale, path))
	}
	currentTmp, err = namespace.root.Lstat(tmpRel)
	if err != nil || !currentTmp.Mode().IsRegular() || !os.SameFile(tmpInfo, currentTmp) {
		return unpublishedRestore(errors.Join(err, fmt.Errorf("%w: undo temporary file changed before publication", ErrStale)))
	}
	if hooks.beforeReplace != nil {
		if err := hooks.beforeReplace(); err != nil {
			return unpublishedRestore(err)
		}
	}
	// Recheck after the last caller-controlled seam. POSIX renameat still names
	// its source rather than accepting an fd, so this closes the deterministic
	// swap window and the post-rename SameFile check below ensures a later race
	// can never be reported as successful or durable.
	currentTmp, err = namespace.root.Lstat(tmpRel)
	if err != nil || !currentTmp.Mode().IsRegular() || !os.SameFile(tmpInfo, currentTmp) {
		return unpublishedRestore(errors.Join(err,
			fmt.Errorf("%w: undo temporary file changed at the publication seam", ErrStale)))
	}
	var currentTarget *os.File
	var targetInfo fs.FileInfo
	if expected.existed {
		currentTarget, err = openCheckpointRootRead(namespace.root, namespace.target)
		if err != nil {
			return unpublishedRestore(err)
		}
		defer currentTarget.Close()
		if err := acquireReplacementTargetLivenessLock(currentTarget); err != nil {
			return unpublishedRestore(fmt.Errorf("locking replacement target: %w", err))
		}
		targetInfo, err = currentTarget.Stat()
		if err != nil {
			return unpublishedRestore(err)
		}
		targetFP, err := namespace.fingerprintTarget(restoreFingerprintLimit(hooks), targetInfo)
		if err != nil || !sameFingerprint(targetFP, expected) {
			return unpublishedRestore(errors.Join(err,
				fmt.Errorf("%w: restore target changed before publication", ErrStale)))
		}
		if hooks.bindDisplaced != nil {
			if err := hooks.bindDisplaced(currentTarget); err != nil {
				return unpublishedRestore(err)
			}
		}
	} else if _, err := namespace.root.Lstat(namespace.target); !errors.Is(err, fs.ErrNotExist) {
		return unpublishedRestore(errors.Join(err,
			fmt.Errorf("%w: restore target appeared before publication", ErrStale)))
	}
	if hooks.publicationSeam != nil {
		if err := hooks.publicationSeam(); err != nil {
			return unpublishedRestore(err)
		}
	}
	currentTmp, err = namespace.root.Lstat(tmpRel)
	if err != nil || !currentTmp.Mode().IsRegular() || !os.SameFile(tmpInfo, currentTmp) {
		return unpublishedRestore(errors.Join(err,
			fmt.Errorf("%w: undo temporary file changed at the final publication seam", ErrStale)))
	}
	if expected.existed {
		linkedTarget, err := namespace.root.Lstat(namespace.target)
		if err != nil || !linkedTarget.Mode().IsRegular() || !os.SameFile(targetInfo, linkedTarget) {
			return unpublishedRestore(errors.Join(err,
				fmt.Errorf("%w: restore target changed at the final publication seam", ErrStale)))
		}
		targetFP, err := namespace.fingerprintTarget(restoreFingerprintLimit(hooks), targetInfo)
		if err != nil || !sameFingerprint(targetFP, expected) {
			return unpublishedRestore(errors.Join(err,
				fmt.Errorf("%w: restore target contents changed at the final publication seam", ErrStale)))
		}
	} else if _, err := namespace.root.Lstat(namespace.target); !errors.Is(err, fs.ErrNotExist) {
		return unpublishedRestore(errors.Join(err,
			fmt.Errorf("%w: restore target appeared at the final publication seam", ErrStale)))
	}
	renameResult, err := renameBoundRestoreFile(namespace.root, tmp, currentTarget, tmpRel, namespace.target, expected.existed)
	if err != nil && expected.existed && boundRenameWasRolledBack(err) {
		if rollbackVerifyErr := verifyBoundReplacementRollback(namespace, tmpRel, tmpInfo, targetInfo, expected); rollbackVerifyErr == nil {
			// The Windows exchange crossed an intermediate name but its own
			// rollback durably restored both exact descriptor-bound files. It
			// is therefore an unpublished refusal, and the deferred cleanup may
			// retire the selected temporary before its ledger is removed.
			publishedTemp = false
			return unpublishedRestore(err)
		} else {
			err = errors.Join(err, rollbackVerifyErr)
		}
	}
	if renameResult.published {
		linked, linkErr := namespace.root.Lstat(namespace.target)
		publishedTemp = linkErr == nil && linked.Mode().IsRegular() && os.SameFile(tmpInfo, linked)
	}
	if err != nil {
		if renameResult.published {
			return publishedRestoreError(displayPath, err)
		}
		return unpublishedRestore(err)
	}
	desired := fingerprintBytes(true, mode, content)
	publishedInfo, err := namespace.root.Lstat(namespace.target)
	if err != nil || !publishedInfo.Mode().IsRegular() || !os.SameFile(tmpInfo, publishedInfo) {
		cause := errors.Join(err,
			fmt.Errorf("%w: restored target is not the temporary inode selected for publication", ErrStale))
		if expected.existed {
			rolledBack, rollbackErr := rollbackBoundReplacement(namespace.root, tmp, currentTarget, tmpRel, namespace.target)
			if rolledBack && rollbackErr == nil {
				publishedTemp = false
				return unpublishedRestore(cause)
			}
			cause = errors.Join(cause, fmt.Errorf("rolling back mismatched checkpoint exchange: %w", rollbackErr))
		}
		return publishedRestoreError(displayPath, cause)
	}
	if expected.existed {
		rollbackPublication := func(cause error) restoreOutcome {
			rolledBack, rollbackErr := false, error(nil)
			if boundRollbackTestHook != nil {
				rollbackErr = boundRollbackTestHook()
			} else {
				rolledBack, rollbackErr = rollbackBoundReplacement(namespace.root, tmp, currentTarget, tmpRel, namespace.target)
			}
			if rolledBack && rollbackErr == nil {
				rollbackErr = verifyBoundPublicationWithdrawn(namespace, tmpRel, tmpInfo)
				if rollbackErr == nil {
					publishedTemp = false
					return unpublishedRestore(cause)
				}
			}
			if rollbackErr == nil {
				rollbackErr = errors.New("checkpoint exchange rollback was not confirmed")
			}
			// Preserve every ledger-bound name after an ambiguous rollback.
			// Recovery, not best-effort deferred cleanup, owns the next step.
			retainTemp = true
			return publishedRestoreError(displayPath, errors.Join(
				fmt.Errorf("rolling back stale checkpoint publication: %w", rollbackErr),
				cause,
			))
		}
		displaced, displacedErr := namespace.root.Lstat(tmpRel)
		if displacedErr != nil || !displaced.Mode().IsRegular() || !os.SameFile(targetInfo, displaced) {
			cause := errors.Join(displacedErr,
				fmt.Errorf("%w: displaced restore target is not the inode selected before publication", ErrStale))
			return rollbackPublication(cause)
		}
		if boundAfterNamespaceTestHook != nil {
			if hookErr := boundAfterNamespaceTestHook(); hookErr != nil {
				return rollbackPublication(hookErr)
			}
		}
		displacedFP, displacedFPErr := fingerprintInRoot(
			namespace.root, tmpRel, path+" displaced pre-image",
			restoreFingerprintLimit(hooks), nil, targetInfo,
		)
		if displacedFPErr != nil || !sameFingerprint(displacedFP, expected) {
			return rollbackPublication(errors.Join(displacedFPErr,
				fmt.Errorf("%w: displaced restore target contents changed before commit", ErrStale)))
		}
		if linkErr := validateRestoreNamespaceLink(scope, path, st); linkErr != nil {
			return rollbackPublication(linkErr)
		}
		if retireErr := retireBoundOpenFileTo(namespace.root, hooks.retirementRoot, tmpRel, currentTarget, false, nil, nil); retireErr != nil {
			return publishedRestoreError(displayPath, fmt.Errorf("retiring displaced restore target: %w", retireErr))
		}
	} else {
		rollbackCreation := func(cause error) restoreOutcome {
			removed, rollbackErr := false, error(nil)
			if boundRollbackTestHook != nil {
				rollbackErr = boundRollbackTestHook()
			} else {
				removed, rollbackErr = rollbackBoundCreation(namespace, hooks.retirementRoot, tmpRel, tmp, desired)
			}
			if removed && rollbackErr == nil {
				// removeBoundTarget retired the exact selected inode. Keep
				// publishedTemp true only to suppress the deferred second cleanup.
				publishedTemp = true
				return unpublishedRestore(cause)
			}
			if rollbackErr == nil {
				rollbackErr = errors.New("created-file rollback was not confirmed")
			}
			retainTemp = true
			return publishedRestoreError(displayPath, errors.Join(
				fmt.Errorf("rolling back stale created-file publication: %w", rollbackErr),
				cause,
			))
		}
		if boundAfterNamespaceTestHook != nil {
			if hookErr := boundAfterNamespaceTestHook(); hookErr != nil {
				return rollbackCreation(hookErr)
			}
		}
		if linkErr := validateRestoreNamespaceLink(scope, path, st); linkErr != nil {
			return rollbackCreation(linkErr)
		}
		if renameResult.sourceRetained {
			// Retire only an alias the namespace primitive explicitly reports. A
			// no-replace move (the normal absent-target path) retains no source.
			if retireErr := retireBoundOpenFileTo(namespace.root, hooks.retirementRoot, tmpRel, tmp, false, nil, nil); retireErr != nil {
				return publishedRestoreError(displayPath, fmt.Errorf("retiring published restore alias: %w", retireErr))
			}
		}
	}
	if hooks.afterReplace != nil {
		if err := hooks.afterReplace(); err != nil {
			return publishedRestoreError(displayPath, err)
		}
	}
	if err := syncBoundReplacement(tmp); err != nil {
		return publishedRestoreError(displayPath, fmt.Errorf("syncing restored file: %w", err))
	}
	if err := syncBoundDirectory(parent.root); err != nil {
		return publishedRestoreError(displayPath, fmt.Errorf("syncing parent directory: %w", err))
	}
	got, err := namespace.fingerprintTarget(restoreFingerprintLimit(hooks), tmpInfo)
	if err != nil {
		return publishedRestoreError(displayPath, fmt.Errorf("verifying restored file: %w", err))
	}
	if !sameFingerprint(got, desired) {
		return publishedRestoreError(displayPath, errors.New("restored file post-image mismatch"))
	}
	return restoreOutcome{published: true}
}
