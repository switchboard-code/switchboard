// Package session stores conversations as append-only event logs on disk.
//
// A session is a file, not a database row. Replay reconstructs the whole state,
// which keeps the canonical log the source of truth even when a provider offers
// a server-side continuation handle (§5.2, §12).
package session

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/switchboard-code/switchboard/internal/continuity"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

// ErrSessionLocked reports that another process is appending to this session.
// Two writers would interleave frames and corrupt the log.
var ErrSessionLocked = errors.New("session is open in another process")

// ErrSessionPoisoned means a durable append had an ambiguous or partial
// failure. No later record may be appended behind that point: replay stops at
// the first torn frame, so a later WAL record could otherwise appear synced
// while being unreachable after restart.
var ErrSessionPoisoned = errors.New("session log is poisoned by a failed append")

var ErrRaceBranchPending = errors.New("race branch is not resumable until its origin ledger is reconciled")

var ErrNoSessions = errors.New("no sessions recorded for this workspace")

var ErrSessionInventoryTooLarge = errors.New("session inventory exceeds its directory-entry limit")

const (
	maxSessionWorkspaceDirectories = 4096
	maxSessionDirectoryEntries     = 16384
	sessionDirectoryReadBatch      = 256
)

// ErrSessionUnpublished means a durable child log exists but has not crossed
// its adoption commit point. Discovery and resume treat it as nonexistent.
var ErrSessionUnpublished = errors.New("session is staged and not published")

// ErrPublicationOwnership means Publish was attempted through a replayed or
// otherwise foreign handle. Only the handle that created a staged log holds
// the in-memory capability that can make it discoverable.
var ErrPublicationOwnership = errors.New("session publication is not owned by this handle")

type Store struct {
	root        string
	maintenance StagedMaintenanceReport
	replayLimit replayLimits // zero selects the production limits; tests may narrow it

	createDirectorySync            func(string) error               // test-only durability fault seam
	maintenanceValidate            func(string, SessionStart) error // test-only fault seam
	maintenanceBeforeOpen          func(string)                     // test-only entry swap seam
	maintenanceBeforeOwned         func(string)                     // test-only race seam
	maintenanceBeforeRemove        func(string)                     // test-only log swap seam
	maintenanceBeforeMarkerRemove  func(string)                     // test-only marker swap seam
	latestAfterList                func()                           // test-only inventory interleaving seam
	openPublicationMarkerSync      func(*os.File) error             // test-only resume durability seam
	openPublicationDirectorySync   func(*os.File) error             // test-only resume durability seam
	openPublicationBeforeDirectory func(string)                     // test-only parent swap seam
	openPublicationAfterMarker     func(string)                     // test-only marker swap seam
}

// DefaultStore places sessions under the user's config directory rather than in
// the workspace, so a session never lands in a repository or a build artifact.
func DefaultStore() (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return NewStore(filepath.Join(home, ".switchboard", "sessions"))
}

func NewStore(root string) (*Store, error) {
	if err := preparePrivateSessionStore(root); err != nil {
		return nil, fmt.Errorf("creating session store: %w", err)
	}
	store := &Store{root: root}
	store.maintenance = store.cleanupExpiredStaged(time.Now().Add(-stagedRetention), stagedCleanupLimit)
	return store, nil
}

// State is the result of replaying a log.
type State struct {
	ID        string
	Workspace string
	Target    string
	CreatedAt time.Time

	// WorkspaceBinding is the canonical workspace identity durably proven for
	// a legacy session whose immutable SessionStart.Workspace used a symlink
	// spelling. Empty means the start path remains authoritative.
	WorkspaceBinding string

	// RetryIntent is non-nil only while a published retry child still has a
	// durable replay handoff. Pending is safe to start once from its exact source
	// coordinate; started is deliberately ambiguous and never auto-replayed.
	RetryIntent *RetryIntent

	// RuntimeBinding is the latest committed tier/target/pin state. Its zero
	// value identifies a legacy log that only has SessionStart.Target.
	RuntimeBinding RuntimeBinding

	// Messages includes assistant messages marked Incomplete. They are kept for
	// diagnosis and display; the provider replay projection excludes them from
	// estimation, cache planning, and wire rendering so an interrupted turn is
	// never replayed as a finished one (§10.3).
	Messages []provider.Message

	Usage provider.Usage
	Calls int

	// UsageTargets is replay-derived from the existing per-call Usage records.
	// It contains each non-empty target identity once, in first-seen order, so
	// cost surfaces can distinguish mixed metering without another record type.
	UsageTargets []string

	// CostMicroUSD totals what the catalog priced this session at. It is an
	// estimate and a reconciliation aid, never a substitute for the provider's
	// invoice (§15).
	CostMicroUSD int64

	// RetryReserveMicroUSD totals conservative allowances for failed provider
	// attempts that may still be billed despite returning no usage, plus
	// write-ahead attempts whose settlement was never made durable. Keeping it
	// separate preserves the distinction between observed usage and a
	// pessimistic hard-ceiling reserve.
	RetryReserveMicroUSD int64

	// ExternalCostMicroUSD is actual priced work admitted by this session but
	// whose provider Usage lives in another log, such as a delegate or losing
	// race arm. It participates in the hard ceiling without fabricating tokens
	// or calls in this session's provider telemetry.
	ExternalCostMicroUSD int64

	// Pins are the named points /fork can cut back to, in the order set,
	// with a re-used name moving its pin rather than stacking a second.
	Pins []Pin

	// Continuity is the latest bounded task-state record, including a cleared
	// tombstone. ContinuityRef is the newest capsule an appended message says
	// was made model-visible; comparing their IDs prevents reinjection after a
	// process resume without parsing prompt prose.
	Continuity    *continuity.Capsule
	ContinuityRef string

	// CatalogRevision is the revision the session started against.
	CatalogRevision string

	pendingBudgetAttempts  map[string]int64
	appliedBudgetTransfers map[string]bool
	providerCallIDs        map[string]bool
	raceBranchOrigin       string
	raceBranchPending      bool
	raceBranchFinalized    bool
	raceBranchContinuation bool
	publicationPending     bool
	publicationID          string
	retryIntentSeen        bool
}

// AccountedCostMicroUSD is the observed dollar cost attributable to this
// continuing session. RetryReserveMicroUSD is deliberately excluded: callers
// display or add that pessimistic allowance separately.
func (s State) AccountedCostMicroUSD() int64 {
	total, err := checkedMicroUSDAdd(s.CostMicroUSD, s.ExternalCostMicroUSD)
	if err != nil {
		return math.MaxInt64
	}
	return total
}

func checkedMicroUSDAdd(current, delta int64) (int64, error) {
	if current < 0 || delta < 0 || delta > math.MaxInt64-current {
		return 0, fmt.Errorf("micro-USD accounting overflow")
	}
	return current + delta, nil
}

func (s State) checkedObservedCost(localDelta, externalDelta int64) (local, external int64, err error) {
	local, err = checkedMicroUSDAdd(s.CostMicroUSD, localDelta)
	if err != nil {
		return 0, 0, err
	}
	external, err = checkedMicroUSDAdd(s.ExternalCostMicroUSD, externalDelta)
	if err != nil {
		return 0, 0, err
	}
	if _, err = checkedMicroUSDAdd(local, external); err != nil {
		return 0, 0, err
	}
	return local, external, nil
}

func (s State) checkedUsage(u Usage) (provider.Usage, error) {
	if err := u.Usage.Validate(); err != nil {
		return provider.Usage{}, fmt.Errorf("invalid provider usage: %w", err)
	}
	if u.CostMicroUSD < 0 {
		return provider.Usage{}, fmt.Errorf("usage cost cannot be negative")
	}
	if u.Attempts < 0 {
		return provider.Usage{}, fmt.Errorf("usage attempts cannot be negative")
	}
	if u.CallID != "" && s.providerCallIDs[u.CallID] {
		return provider.Usage{}, fmt.Errorf("provider call %q is already recorded", u.CallID)
	}
	total, err := s.Usage.CheckedAdd(u.Usage)
	if err != nil {
		return provider.Usage{}, fmt.Errorf("provider usage accounting: %w", err)
	}
	if s.Calls == math.MaxInt {
		return provider.Usage{}, fmt.Errorf("provider call accounting overflow")
	}
	return total, nil
}

func (s *State) recordProviderCallID(id string) {
	if id == "" {
		return
	}
	if s.providerCallIDs == nil {
		s.providerCallIDs = make(map[string]bool)
	}
	s.providerCallIDs[id] = true
}

func (s *State) recordUsageTarget(target string) {
	if target == "" {
		return
	}
	for _, recorded := range s.UsageTargets {
		if recorded == target {
			return
		}
	}
	s.UsageTargets = append(s.UsageTargets, target)
}

type Session struct {
	mu       sync.Mutex
	f        *os.File
	path     string
	seq      int
	poisoned error
	closed   bool

	state State

	// publicationOwner is an in-memory capability held only by the handle that
	// created a staged log. It is deliberately not reconstructed by replay.
	// publicationCommitted makes a second Publish by that same handle
	// idempotent without accepting a pre-existing sidecar as proof of ownership.
	// publicationDurable remains false when the exact marker became visible but
	// a later file or directory sync failed; retry journals must survive that
	// distinction. publicationMarkerInfo remembers the marker inode created by
	// this live handle. It is the authority for continuing an interrupted strict
	// prefix without deleting or accepting a pre-existing partial sidecar.
	publicationOwner      string
	publicationCommitted  bool
	publicationDurable    bool
	publicationMarkerInfo os.FileInfo
	publicationLogStamp   publicationMutationStamp
	publicationLogStamped bool
	publicationFault      func(publicationStep) error // test-only fault seam
	publicationFailCheck  func(string)                // test-only failure-commit seam
	discardBeforeInspect  func(string)                // test-only marker-commit seam
	discardBeforeClose    func(string)                // test-only pathname-swap seam

	// assistantDrafts indexes logical incomplete messages assembled from
	// append-only streaming checkpoints. A final ordinary Message carrying the
	// same DraftID replaces the indexed message and removes this transient fold
	// state; the map itself is fully replay-derived and never serialized.
	assistantDrafts map[string]*assistantDraftState

	// liveUsages only holds calls appended since this Session handle was
	// opened. A UsageCursor can therefore name an exact live interval without
	// rereading the whole append-only log on every turn; a reopened handle
	// starts its first cursor after every replayed record.
	liveUsages []sequencedUsage
	// lastRouteUsageCursor prevents an accidentally reused window from
	// attributing the same durable provider call to two route records.
	lastRouteUsageCursor int

	// truncated counts bytes discarded by replay because the tail of the log was
	// unreadable. Non-zero means the user lost recorded work and must be told.
	truncated int64

	// replayLimit is inherited from the Store that opens this handle. It is not
	// durable policy; zero selects the production limits.
	replayLimit replayLimits
}

type sequencedUsage struct {
	seq   int
	usage Usage
}

// UsageCursor is an opaque boundary in one live session handle. Route
// accounting uses it to correlate only provider calls durably appended after
// the turn began. Its fields stay private so callers cannot fabricate a
// sequence or reuse a cursor against another session.
type UsageCursor struct {
	sessionID string
	seq       int
}

// Create starts a session. catalogRevision pins the price and capability data
// in force, so a cost recorded in this log stays checkable against the data
// that produced it rather than whatever is current when it is read back.
func (s *Store) Create(workspace string, target provider.RouteTargetID, catalogRevision string) (*Session, error) {
	return s.create(workspace, target, catalogRevision, false)
}

// CreateStaged starts a durable session that is invisible to List, Latest,
// Open, and read-only replay until its creating handle calls PublishDurably. It
// is the construction primitive for clear, compact, fork, retry, and race adoption:
// a failed or crashed operation may leave bytes for diagnosis, but can never
// hijack --continue.
func (s *Store) CreateStaged(workspace string, target provider.RouteTargetID, catalogRevision string) (*Session, error) {
	return s.create(workspace, target, catalogRevision, true)
}

func (s *Store) create(workspace string, target provider.RouteTargetID, catalogRevision string, staged bool) (*Session, error) {
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(s.root, workspaceKey(workspace))
	if err := ensurePrivateSessionDirectory(dir); err != nil {
		return nil, err
	}

	id, err := newID()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, id+".log")
	publicationID := ""
	if staged {
		publicationID, err = newPublicationID()
		if err != nil {
			return nil, err
		}
	}

	f, err := createPrivateSessionFile(path)
	if err != nil {
		return nil, fmt.Errorf("creating session file: %w", err)
	}
	if err := acquireLock(f); err != nil {
		return nil, errors.Join(err, discardCreatedSessionFile(f, path, false))
	}
	if _, err := fmt.Fprintf(f, "%s %d\n", magic, SchemaVersion); err != nil {
		return nil, errors.Join(err, discardCreatedSessionFile(f, path, true))
	}

	sess := &Session{
		f:                f,
		path:             path,
		publicationOwner: publicationID,
		state: State{
			ID:                 id,
			Workspace:          workspace,
			Target:             string(target),
			CatalogRevision:    catalogRevision,
			CreatedAt:          time.Now().UTC(),
			publicationPending: staged,
			publicationID:      publicationID,
		},
	}
	err = sess.append(RecordSessionStart, SessionStart{
		ID:              id,
		Workspace:       workspace,
		Target:          string(target),
		Binary:          binaryVersion(),
		CatalogRevision: catalogRevision,
		Staged:          staged,
		PublicationID:   publicationID,
	})
	if err != nil {
		sess.closed = true
		return nil, errors.Join(err, discardCreatedSessionFile(f, path, true))
	}
	// File.Sync above makes the session_start bytes durable. The containing
	// directory is a separate crash-consistency boundary: without syncing it,
	// Create could return a session whose directory entry disappears on power
	// loss. Sync the workspace directory first, then the store root that names a
	// newly-created workspace directory.
	if err := s.syncCreatedDirectory(dir); err != nil {
		sess.closed = true
		return nil, errors.Join(fmt.Errorf("syncing new session directory: %w", err), discardCreatedSessionFile(f, path, true))
	}
	if err := s.syncCreatedDirectory(s.root); err != nil {
		sess.closed = true
		return nil, errors.Join(fmt.Errorf("syncing session store directory: %w", err), discardCreatedSessionFile(f, path, true))
	}
	return sess, nil
}

func (s *Store) syncCreatedDirectory(path string) error {
	if s.createDirectorySync != nil {
		return s.createDirectorySync(path)
	}
	return syncSessionDirectory(path)
}

// discardCreatedSessionFile is only used before a new Session escapes its
// creator. Identity-checked removal ensures even this error path cannot delete
// a pathname replacement installed between creation and cleanup.
func discardCreatedSessionFile(f *os.File, path string, locked bool) error {
	info, statErr := f.Stat()
	var unlockErr error
	if locked {
		unlockErr = releaseLock(f)
	}
	closeErr := f.Close()
	if statErr != nil {
		return errors.Join(statErr, unlockErr, closeErr)
	}
	return errors.Join(unlockErr, closeErr, removePathIfSame(path, info))
}

// WorkspaceDir is the per-workspace directory the store keeps logs in,
// created if absent. Per-workspace state that is not a session log — the
// schedule ledger — lives beside the logs under the same key, so it follows
// the same machine-local placement rule DefaultStore states.
func (s *Store) WorkspaceDir(workspace string) (string, error) {
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(s.root, workspaceKey(workspace))
	if err := ensurePrivateSessionDirectory(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// Open replays a session by ID and reopens it for appending. A caller adopting
// the transcript into an existing tool runtime must use OpenInWorkspace.
func (s *Store) Open(id string) (*Session, error) {
	candidate, err := s.resolveCandidate(id)
	if err != nil {
		return nil, err
	}
	return s.openPath(candidate.path, candidateExpectation{
		id:        id,
		workspace: effectiveWorkspace(candidate.start.Workspace, candidate.state.WorkspaceBinding),
	})
}

// OpenInWorkspace is the interactive-resume boundary. A session's transcript
// and the process's tools must name the same workspace: adopting a log from a
// different workspace would let its next model request drive tools, trust,
// hooks, MCP, and language services rooted in the wrong tree. Open remains for
// callers that deliberately rebuild their entire runtime from the returned
// session's recorded workspace.
func (s *Store) OpenInWorkspace(id, workspace string) (*Session, error) {
	workspace, err := cleanAbsoluteWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	candidate, err := s.resolveCandidate(id)
	if err != nil {
		return nil, err
	}
	if !workspaceIdentityMatches(effectiveWorkspace(candidate.start.Workspace, candidate.state.WorkspaceBinding), workspace) {
		return nil, fmt.Errorf(
			"session %s belongs to workspace %q, not %q; restart switchboard with -workspace %q to resume it",
			id, effectiveWorkspace(candidate.start.Workspace, candidate.state.WorkspaceBinding), workspace,
			effectiveWorkspace(candidate.start.Workspace, candidate.state.WorkspaceBinding))
	}
	return s.openPathInWorkspace(candidate.path, id, workspace)
}

// Latest opens the most recently modified session for a workspace, which is
// what `sb --continue` resumes.
func (s *Store) Latest(workspace string) (*Session, error) {
	infos, err := s.List(workspace)
	if err != nil {
		return nil, err
	}
	if s.latestAfterList != nil {
		s.latestAfterList()
	}
	if len(infos) == 0 {
		return nil, ErrNoSessions
	}
	// A published retry child with an unresolved execution handoff owns the
	// workspace even if the source's post-commit outcome note gave that source a
	// later mtime. Choosing the source beside the child's pre-turn files would
	// split conversation from workspace. Ambiguous children fail closed rather
	// than picking one by mtime.
	if id, _, found, retryErr := unresolvedRetryFromInfos(infos); retryErr != nil {
		return nil, retryErr
	} else if found {
		return s.OpenInWorkspace(id, workspace)
	}
	// A completed race keeps both fully accounted answers explicitly
	// resumable, but --continue must select the branch the user chose rather
	// than a later-touched alternative. If all records predate this marker,
	// fall back to the ordinary mtime ordering for compatibility.
	var fallback *Info
	for i := range infos {
		info := &infos[i]
		checked, stateErr := s.validateCandidate(info.Path, candidateExpectation{id: info.ID, workspace: workspace})
		if stateErr != nil {
			continue
		}
		if fallback == nil {
			fallback = info
		}
		if !checked.state.raceAlternative() {
			return s.openPathInWorkspace(info.Path, info.ID, workspace)
		}
	}
	if fallback != nil {
		return s.openPathInWorkspace(fallback.Path, fallback.ID, workspace)
	}
	// If every listed session is integrity-blocked, surface the newest one's
	// exact refusal rather than pretending the workspace has no history.
	return s.openPathInWorkspace(infos[0].Path, infos[0].ID, workspace)
}

// UnresolvedRetry returns the published child whose execution handoff still
// governs the workspace. It is read-only: default startup uses it before
// deciding whether to create a fresh session, while an explicit -resume keeps
// the user's chosen conversation.
func (s *Store) UnresolvedRetry(workspace string) (string, RetryIntentStatus, bool, error) {
	infos, err := s.List(workspace)
	if err != nil {
		return "", "", false, err
	}
	return unresolvedRetryFromInfos(infos)
}

// unresolvedRetryFromInfos deliberately consumes the same descriptor-stable
// inventory snapshot that supplied Latest's modification ordering. Re-listing
// here can combine an old order with a newer completed retry status and resume
// the source beside the child's already-published workspace state.
func unresolvedRetryFromInfos(infos []Info) (string, RetryIntentStatus, bool, error) {
	type unresolved struct {
		id     string
		status RetryIntentStatus
	}
	var found []unresolved
	for _, info := range infos {
		if info.Health.ReplayLimit {
			return "", "", false, fmt.Errorf("session %s exceeds the cumulative replay limit; automatic continue cannot prove workspace retry state: %w", info.ID, ErrSessionReplayTooLarge)
		}
		if info.Health.RetryIntent == "" {
			continue
		}
		if info.Health.CorruptRecord {
			return "", "", false, fmt.Errorf("unresolved retry child %s is corrupt or unreadable: %w", info.ID, ErrCorruptRecord)
		}
		found = append(found, unresolved{id: info.ID, status: info.Health.RetryIntent})
	}
	switch len(found) {
	case 0:
		return "", "", false, nil
	case 1:
		return found[0].id, found[0].status, true, nil
	default:
		ids := make([]string, len(found))
		for i := range found {
			ids[i] = found[i].id
		}
		return "", "", false, fmt.Errorf("workspace has %d unresolved retry children (%s); refusing to guess", len(found), strings.Join(ids, ", "))
	}
}

type Info struct {
	ID       string
	Path     string
	Modified time.Time
	Size     int64
	Health   ResumeHealth
}

// ListAll returns every workspace's sessions, keyed by the workspace path
// each log's own header records. The store's directories are content
// hashes, so the answer comes from the logs rather than from names that
// never held it. A log whose complete start binds a valid identity remains
// listed when later corruption blocks resume; a header or start too damaged to
// establish ownership is skipped, the same posture List takes per file.
func (s *Store) ListAll() (map[string][]Info, error) {
	dirs, err := readSessionDirectory(s.root, maxSessionWorkspaceDirectories)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := map[string][]Info{}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		entries, err := readSessionDirectory(filepath.Join(s.root, d.Name()), maxSessionDirectoryEntries)
		if err != nil {
			if errors.Is(err, ErrSessionInventoryTooLarge) {
				return nil, err
			}
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
				continue
			}
			path := filepath.Join(s.root, d.Name(), e.Name())
			id := strings.TrimSuffix(e.Name(), ".log")
			checked, err := s.validateCandidate(path, candidateExpectation{id: id})
			if err != nil && !checked.blockedForInventory() {
				continue
			}
			start, state := checked.start, checked.state
			if state.RaceBranchPending() || state.ID != start.ID || state.Workspace != start.Workspace {
				continue
			}
			fi := checked.fileInfo
			if fi == nil {
				continue
			}
			health := ResumeHealthForState(state, checked.recoveredCorruptTail)
			health.CorruptRecord = checked.blockedByCorruption
			health.ReplayLimit = checked.blockedByReplayLimit
			workspace := effectiveWorkspace(start.Workspace, state.WorkspaceBinding)
			out[workspace] = append(out[workspace], Info{
				ID:       start.ID,
				Path:     path,
				Modified: fi.ModTime(),
				Size:     fi.Size(),
				Health:   health,
			})
		}
	}
	return out, nil
}

// List returns a workspace's published session inventory, most recent first.
// A log with a validated identity and later complete corruption remains in the
// inventory with Health.CorruptRecord set; opening or fully replaying it still
// fails closed.
func (s *Store) List(workspace string) ([]Info, error) {
	workspace, err := cleanAbsoluteWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	dirs, err := readSessionDirectory(s.root, maxSessionWorkspaceDirectories)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var infos []Info
	seen := make(map[string]string)
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		entries, readErr := readSessionDirectory(filepath.Join(s.root, dir.Name()), maxSessionDirectoryEntries)
		if readErr != nil {
			if errors.Is(readErr, ErrSessionInventoryTooLarge) {
				return nil, readErr
			}
			// Preserve the exact directory's historical error behavior. An
			// unrelated or obsolete alias directory cannot make this workspace's
			// inventory unavailable merely because it is unreadable.
			if dir.Name() == workspaceKey(workspace) {
				return nil, readErr
			}
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
				continue
			}
			// A candidate needs its complete first session_start, not merely the
			// magic header. Validation also binds the physical log to its immutable
			// start directory, then proves either an exact workspace binding or a
			// live same-directory legacy alias.
			path := filepath.Join(s.root, dir.Name(), e.Name())
			id := strings.TrimSuffix(e.Name(), ".log")
			checked, candidateErr := s.validateCandidate(path, candidateExpectation{id: id, workspace: workspace})
			if candidateErr != nil && !checked.blockedForInventory() {
				continue
			}
			start, state := checked.start, checked.state
			if state.RaceBranchPending() || state.ID != start.ID || state.Workspace != start.Workspace {
				continue
			}
			fi := checked.fileInfo
			if fi == nil {
				continue
			}
			if previous, duplicate := seen[start.ID]; duplicate {
				return nil, fmt.Errorf("session %s is ambiguous between %s and %s", start.ID, previous, path)
			}
			seen[start.ID] = path
			health := ResumeHealthForState(state, checked.recoveredCorruptTail)
			health.CorruptRecord = checked.blockedByCorruption
			health.ReplayLimit = checked.blockedByReplayLimit
			infos = append(infos, Info{
				ID:       start.ID,
				Path:     path,
				Modified: fi.ModTime(),
				Size:     fi.Size(),
				Health:   health,
			})
		}
	}
	// Modification time first, then the id, because a filesystem can stamp two
	// files in the same tick and mtime alone would then leave `--continue`
	// resuming whichever one the directory happened to be read in. The id
	// tiebreak makes the answer stable; it does not make it right, because the
	// id only carries seconds, so two sessions started inside one second are
	// ordered by the random suffix rather than by which came first.
	sort.Slice(infos, func(i, j int) bool {
		if !infos[i].Modified.Equal(infos[j].Modified) {
			return infos[i].Modified.After(infos[j].Modified)
		}
		return infos[i].ID > infos[j].ID
	})
	return infos, nil
}

func readSessionDirectory(path string, limit int) ([]os.DirEntry, error) {
	if limit < 1 {
		return nil, fmt.Errorf("session inventory limit must be positive")
	}
	root, err := rootedfs.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	entries := make([]os.DirEntry, 0, min(limit, sessionDirectoryReadBatch))
	for {
		batch, readErr := directory.ReadDir(sessionDirectoryReadBatch)
		entries = append(entries, batch...)
		if len(entries) > limit {
			return nil, fmt.Errorf("%w: %s contains more than %d entries; archive stale sessions or move unrelated files", ErrSessionInventoryTooLarge, path, limit)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}

// openPathInWorkspace is the writable compatibility boundary for a session
// discovered through an old symlink spelling. The append lock and a second
// same-directory proof come first; only then is the canonical binding made
// durable. A later run can use that record even if the legacy alias is gone.
func (s *Store) openPathInWorkspace(path, id, workspace string) (*Session, error) {
	sess, err := s.openPath(path, candidateExpectation{id: id, workspace: workspace})
	if err != nil {
		return nil, err
	}
	state := sess.State()
	if state.WorkspaceBinding == workspace || state.Workspace == workspace {
		return sess, nil
	}
	if err := sess.AppendWorkspaceBinding(workspace); err != nil {
		return nil, errors.Join(fmt.Errorf("recording canonical workspace binding: %w", err), sess.Close())
	}
	return sess, nil
}

func (s *Store) openPath(path string, expect candidateExpectation) (*Session, error) {
	f, err := openSessionLog(path, true)
	if err != nil {
		return nil, err
	}
	if err := acquireLock(f); err != nil {
		f.Close()
		return nil, err
	}
	if err := verifyCurrentSessionLogPath(f, path); err != nil {
		releaseLock(f)
		f.Close()
		return nil, err
	}
	logStamp, err := captureStablePublicationLogStamp(f, path)
	if err != nil {
		releaseLock(f)
		f.Close()
		return nil, err
	}
	// Recheck identity under the append lock. List/Open discovery is read-only
	// and may race a path replacement; no migration or incomplete-tail repair
	// is allowed until the locked file proves it is the chosen session.
	checked, err := s.validateCandidateFile(f, path, expect)
	if err != nil {
		releaseLock(f)
		f.Close()
		return nil, err
	}
	if err := verifyPublicationLogStamp(f, path, logStamp); err != nil {
		releaseLock(f)
		f.Close()
		return nil, err
	}
	// A complete marker is the visibility commit, but bytes observed after a
	// crash are not necessarily durable. Before migration, torn-tail repair, or
	// any reconciliation append can mutate the log, flush the exact marker and
	// its bound parent directory. This is the one writable adoption boundary
	// shared by Open, OpenInWorkspace, Latest, and every staged-child workflow.
	if err := s.ensurePublishedSessionDurableForOpen(f, path, checked.start, logStamp); err != nil {
		releaseLock(f)
		f.Close()
		return nil, err
	}
	f, err = migrateForAppend(f, path)
	if err != nil {
		releaseLock(f)
		f.Close()
		return nil, err
	}

	sess := &Session{f: f, path: path, replayLimit: s.replayLimit}
	if err := sess.replay(); err != nil {
		releaseLock(f)
		f.Close()
		return nil, err
	}
	// validateCandidateFile proved the staged sidecar under the append lock.
	// Replay folds immutable start provenance, so clear only the live pending
	// bit without granting this reopened handle publication ownership.
	sess.state.publicationPending = false
	if sess.state.raceBranchPending {
		releaseLock(f)
		f.Close()
		return nil, fmt.Errorf("%w: origin session %s", ErrRaceBranchPending, sess.state.raceBranchOrigin)
	}
	// Open is the writable resume boundary. Repair only after replay has
	// truncated a torn frame and after a pending race branch has been refused;
	// read-only List/ReadState paths never reach this append.
	if _, err := sess.ReconcileInterruptedToolCalls(); err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("recovering interrupted tool calls: %w", err)
	}
	return sess, nil
}

// migrateForAppend upgrades old readable logs before this process is allowed
// to append records whose execution semantics older binaries cannot preserve.
// Schemas 1 through 5 have same-width headers, so the version byte can be
// replaced in place while the file is exclusively locked. This works on
// Windows too, where replacing the path of an open, non-share-delete file is
// not permitted. Sync completes the upgrade
// before any schema-5 record may be appended.
func migrateForAppend(old *os.File, path string) (*os.File, error) {
	if _, err := old.Seek(0, io.SeekStart); err != nil {
		return old, err
	}
	r := bufio.NewReader(old)
	header, version, err := readSessionHeader(r, path)
	if err != nil {
		return old, err
	}
	if version == SchemaVersion {
		return old, nil
	}
	if version != 1 && version != 2 && version != 3 && version != 4 {
		return old, fmt.Errorf("cannot migrate session schema %d to %d", version, SchemaVersion)
	}
	oldHeader := []byte(header)
	newHeader := []byte(fmt.Sprintf("%s %d\n", magic, SchemaVersion))
	if len(oldHeader) != len(newHeader) {
		return old, fmt.Errorf("cannot migrate session schema header in place (%d bytes to %d)", len(oldHeader), len(newHeader))
	}
	n, err := old.WriteAt(newHeader, 0)
	if err == nil && n != len(newHeader) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return old, fmt.Errorf("writing session schema migration: %w", err)
	}
	if err := old.Sync(); err != nil {
		return old, fmt.Errorf("syncing session schema migration: %w", err)
	}
	if _, err := old.Seek(0, io.SeekStart); err != nil {
		return old, err
	}
	return old, nil
}

// replay folds the log into state. It truncates only a provably incomplete
// final frame; a complete corrupt frame is preserved and refuses the resume.
func (s *Session) replay() error {
	if _, err := s.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReader(s.f)

	header, _, err := readSessionHeader(r, s.path)
	if err != nil {
		return err
	}

	offset := int64(len(header))
	lastSeq := 0
	budget := newReplayBudget(s.replayLimit, len(header))
	for {
		rec, consumed, err := budget.decode(r, &lastSeq)
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.Is(err, errTornFinalRecord) {
			size, statErr := s.f.Seek(0, io.SeekEnd)
			if statErr != nil {
				return statErr
			}
			s.truncated = size - offset
			if err := s.f.Truncate(offset); err != nil {
				return fmt.Errorf("truncating incomplete final frame at %d: %w", offset, err)
			}
			if err := s.f.Sync(); err != nil {
				return fmt.Errorf("syncing incomplete-frame repair at %d: %w", offset, err)
			}
			break
		}
		if errors.Is(err, ErrCorruptRecord) {
			return fmt.Errorf("%s contains a complete corrupt record at byte %d; original log preserved: %w", s.path, offset, err)
		}
		if err != nil {
			return err
		}
		offset += int64(consumed)
		if err := s.apply(rec); err != nil {
			return err
		}
	}

	if _, err := s.f.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	return nil
}

func (s *Session) apply(rec Record) error {
	s.seq = rec.Seq
	switch rec.Type {
	case RecordSessionStart:
		var p SessionStart
		if err := json.Unmarshal(rec.Payload, &p); err != nil {
			return err
		}
		s.state.ID = p.ID
		s.state.Workspace = p.Workspace
		s.state.Target = p.Target
		s.state.CatalogRevision = p.CatalogRevision
		s.state.CreatedAt = rec.At
		s.state.publicationPending = p.Staged
		s.state.publicationID = p.PublicationID
	case RecordMessage:
		var m provider.Message
		if err := json.Unmarshal(rec.Payload, &m); err != nil {
			return err
		}
		m = provider.CloneMessage(m)
		if m.ContinuityRef != "" {
			if err := validateContinuityDelivery(s.state, m); err != nil {
				return fmt.Errorf("message in record %d: %w", rec.Seq, err)
			}
			s.state.ContinuityRef = m.ContinuityRef
		}
		if err := s.applyMessageState(m); err != nil {
			return fmt.Errorf("message in record %d: %w", rec.Seq, err)
		}
	case RecordAssistantDraft:
		checkpoint, err := decodeAssistantDraft(rec.Payload)
		if err != nil {
			return fmt.Errorf("assistant draft in record %d: %w", rec.Seq, err)
		}
		if err := s.applyAssistantDraft(checkpoint); err != nil {
			return fmt.Errorf("assistant draft in record %d: %w", rec.Seq, err)
		}
	case RecordMessageContinuity:
		m, capsule, err := decodeMessageContinuity(rec.Payload)
		if err != nil {
			return fmt.Errorf("message-continuity in record %d: %w", rec.Seq, err)
		}
		if m.Role != provider.RoleTool || !hasSuccessfulTodoResult(m) {
			return fmt.Errorf("message-continuity in record %d does not hold a successful todo result", rec.Seq)
		}
		if m.ContinuityRef != "" {
			if err := validateContinuityDelivery(s.state, m); err != nil {
				return fmt.Errorf("message in record %d: %w", rec.Seq, err)
			}
		}
		if capsule.Source != continuity.SourceTodo || capsule.Cleared {
			return fmt.Errorf("message-continuity in record %d does not hold live todo state", rec.Seq)
		}
		if capsule.BasisMessages != len(s.state.Messages)+1 {
			return fmt.Errorf("continuity in record %d is based on %d messages, but the atomic result makes %d", rec.Seq, capsule.BasisMessages, len(s.state.Messages)+1)
		}
		// All validation precedes either state change: a malformed compound
		// frame cannot publish its message without its matching capsule.
		s.state.Messages = append(s.state.Messages, m)
		if m.ContinuityRef != "" {
			s.state.ContinuityRef = m.ContinuityRef
		}
		cloned := continuity.Clone(capsule)
		s.state.Continuity = &cloned
	case RecordUsage:
		var p Usage
		if err := json.Unmarshal(rec.Payload, &p); err != nil {
			return err
		}
		usage, err := s.state.checkedUsage(p)
		if err != nil {
			return fmt.Errorf("usage in record %d: %w", rec.Seq, err)
		}
		local, external, err := s.state.checkedObservedCost(p.CostMicroUSD, 0)
		if err != nil {
			return fmt.Errorf("usage cost in record %d: %w", rec.Seq, err)
		}
		s.state.Usage = usage
		s.state.CostMicroUSD, s.state.ExternalCostMicroUSD = local, external
		s.state.Calls++
		s.state.recordProviderCallID(p.CallID)
		s.state.recordUsageTarget(p.Target)
	case RecordRetryReserve:
		var p RetryReserve
		if err := json.Unmarshal(rec.Payload, &p); err != nil {
			return err
		}
		reserve, err := checkedMicroUSDAdd(s.state.RetryReserveMicroUSD, p.CostMicroUSD)
		if err != nil {
			return fmt.Errorf("retry reserve in record %d: %w", rec.Seq, err)
		}
		s.state.RetryReserveMicroUSD = reserve
	case RecordBudgetAttempt:
		var p BudgetAttempt
		if err := json.Unmarshal(rec.Payload, &p); err != nil {
			return err
		}
		if err := s.state.applyBudgetAttempt(p); err != nil {
			return fmt.Errorf("budget attempt in record %d: %w", rec.Seq, err)
		}
	case RecordBudgetSettle:
		var p BudgetSettlement
		if err := json.Unmarshal(rec.Payload, &p); err != nil {
			return err
		}
		if err := s.state.applyBudgetSettlement(p); err != nil {
			return fmt.Errorf("budget settlement in record %d: %w", rec.Seq, err)
		}
	case RecordBudgetTransfer:
		var p BudgetTransfer
		if err := json.Unmarshal(rec.Payload, &p); err != nil {
			return err
		}
		if err := s.state.applyBudgetTransfer(p); err != nil {
			return fmt.Errorf("budget transfer in record %d: %w", rec.Seq, err)
		}
	case RecordRaceBranch:
		var p RaceBranch
		if err := json.Unmarshal(rec.Payload, &p); err != nil {
			return err
		}
		if p.OriginID == "" {
			return fmt.Errorf("race branch in record %d has no origin", rec.Seq)
		}
		if s.state.raceBranchOrigin != "" && s.state.raceBranchOrigin != p.OriginID {
			return fmt.Errorf("race branch in record %d changes origin", rec.Seq)
		}
		s.state.raceBranchOrigin = p.OriginID
		s.state.raceBranchPending = !p.Finalized
		s.state.raceBranchFinalized = p.Finalized
		s.state.raceBranchContinuation = p.Finalized && p.Continuation
	case RecordRuntimeBinding:
		var p RuntimeBinding
		if err := json.Unmarshal(rec.Payload, &p); err != nil {
			return err
		}
		if p.Tier == "" || p.Target == "" {
			return fmt.Errorf("runtime binding in record %d has an empty tier or target", rec.Seq)
		}
		// The optional note belongs to this record's audit timeline. Keeping it
		// out of State preserves the equality and resume semantics of a binding.
		p.Note = nil
		s.state.RuntimeBinding = p
	case RecordWorkspaceBinding:
		var p WorkspaceBinding
		if err := json.Unmarshal(rec.Payload, &p); err != nil {
			return err
		}
		workspace, err := cleanAbsoluteWorkspace(p.Workspace)
		if err != nil || workspace != p.Workspace {
			return fmt.Errorf("workspace binding in record %d has an invalid workspace %q", rec.Seq, p.Workspace)
		}
		if s.state.WorkspaceBinding != "" && s.state.WorkspaceBinding != p.Workspace {
			return fmt.Errorf("workspace binding in record %d changes canonical workspace from %q to %q", rec.Seq, s.state.WorkspaceBinding, p.Workspace)
		}
		s.state.WorkspaceBinding = p.Workspace
	case RecordContinuity:
		p, err := continuity.DecodeStored(rec.Payload)
		if err != nil {
			return fmt.Errorf("continuity in record %d: %w", rec.Seq, err)
		}
		if p.BasisMessages != len(s.state.Messages) {
			return fmt.Errorf("continuity in record %d is based on %d messages, but the log held %d", rec.Seq, p.BasisMessages, len(s.state.Messages))
		}
		cloned := continuity.Clone(p)
		s.state.Continuity = &cloned
	case RecordRetryIntent:
		var p RetryIntent
		if err := json.Unmarshal(rec.Payload, &p); err != nil {
			return fmt.Errorf("retry intent in record %d: %w", rec.Seq, err)
		}
		if err := s.state.applyRetryIntent(p); err != nil {
			return fmt.Errorf("retry intent in record %d: %w", rec.Seq, err)
		}
	case RecordPin:
		var p Pin
		if err := json.Unmarshal(rec.Payload, &p); err != nil {
			return err
		}
		s.state.setPin(p)
	case RecordPermission, RecordNote, RecordRoute, RecordRace:
		// Recorded for audit and for §8.4's training signal; none of them carry
		// conversation state, so replay skips them without losing anything.
	default:
		// An unknown type from a same-schema log is forward-compatible padding,
		// not corruption. A newer schema is refused before replay reaches here.
	}
	return nil
}

func (s *Session) append(t RecordType, payload any) error {
	if s.poisoned != nil {
		return fmt.Errorf("%w: %v", ErrSessionPoisoned, s.poisoned)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	nextSeq, err := s.nextRecordSequence()
	if err != nil {
		return err
	}
	frame, err := encodeRecord(Record{
		Seq:     nextSeq,
		At:      time.Now().UTC(),
		Type:    t,
		Payload: raw,
	})
	if err != nil {
		return err
	}
	if err := s.writeFrame(frame, nextSeq); err != nil {
		return err
	}
	// Records are few per turn, so paying for durability here is cheap and it is
	// what makes resume-after-interruption a guarantee rather than a hope.
	if err := s.f.Sync(); err != nil {
		s.poisoned = fmt.Errorf("syncing record %d: %w", nextSeq, err)
		return fmt.Errorf("%w: %v", ErrSessionPoisoned, s.poisoned)
	}
	// Encoding is fallible before a byte is written. Commit the in-memory
	// sequence only after the complete frame is durable, otherwise a rejected
	// oversized record leaves a gap that makes the next valid append impossible
	// to replay.
	s.seq = nextSeq
	return nil
}

func (s *Session) nextRecordSequence() (int, error) {
	if s.seq < 0 || s.seq == math.MaxInt {
		s.poisoned = fmt.Errorf("record sequence cannot advance after %d", s.seq)
		return 0, fmt.Errorf("%w: %v", ErrSessionPoisoned, s.poisoned)
	}
	return s.seq + 1, nil
}

func (s *Session) writeFrame(frame []byte, seq int) error {
	n, err := s.f.Write(frame)
	if err == nil && n != len(frame) {
		err = io.ErrShortWrite
	}
	if err != nil {
		s.poisoned = fmt.Errorf("writing record %d (%d of %d bytes): %w", seq, n, len(frame), err)
		return fmt.Errorf("%w: %v", ErrSessionPoisoned, s.poisoned)
	}
	return nil
}

func (s *Session) AppendMessage(m provider.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m = provider.CloneMessage(m)
	if err := validateRetryOpeningAppend(s.state, m); err != nil {
		return err
	}
	if m.ContinuityRef != "" {
		if err := validateContinuityDelivery(s.state, m); err != nil {
			return err
		}
	}
	if err := s.validateDraftFinal(m); err != nil {
		return err
	}
	if err := s.append(RecordMessage, m); err != nil {
		return err
	}
	if err := s.applyMessageState(m); err != nil {
		s.poisoned = fmt.Errorf("applying durable message record %d: %w", s.seq, err)
		return fmt.Errorf("%w: %v", ErrSessionPoisoned, s.poisoned)
	}
	if m.ContinuityRef != "" {
		s.state.ContinuityRef = m.ContinuityRef
	}
	return nil
}

// AppendToolResultsWithTasks commits one successful tool-result message and
// the exact todo continuity it produced in one checksummed, synced WAL frame.
// A torn frame replays neither half; a complete frame replays both.
func (s *Session) AppendToolResultsWithTasks(m provider.Message, tasks []continuity.Task) (continuity.Capsule, error) {
	return s.AppendToolResultsWithWorking(m, tasks, continuity.Working{})
}

// AppendToolResultsWithWorking is the same commit, carrying what the model said
// about the job alongside its list. The two are one WAL frame for the reason
// the tasks alone were: a crash between them would leave replay holding a
// successful tool result and an older belief about the work.
func (s *Session) AppendToolResultsWithWorking(m provider.Message, tasks []continuity.Task, working continuity.Working) (continuity.Capsule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m = provider.CloneMessage(m)
	if m.Role != provider.RoleTool || !hasSuccessfulTodoResult(m) {
		return continuity.Capsule{}, fmt.Errorf("atomic todo result requires a tool message with a successful todo result")
	}
	if m.ContinuityRef != "" {
		return continuity.Capsule{}, fmt.Errorf("tool-result message cannot carry a continuity reference")
	}
	var current *continuity.Capsule
	if s.state.Continuity != nil {
		cloned := continuity.Clone(*s.state.Continuity)
		current = &cloned
	}
	next := continuity.WithWorking(current, tasks, working)
	next.BasisMessages = len(s.state.Messages) + 1
	prepared, err := continuity.Prepare(next)
	if err != nil {
		return continuity.Capsule{}, err
	}
	payload := messageContinuity{Message: m, Continuity: prepared}
	if err := s.append(RecordMessageContinuity, payload); err != nil {
		return continuity.Capsule{}, err
	}
	s.state.Messages = append(s.state.Messages, m)
	cloned := continuity.Clone(prepared)
	s.state.Continuity = &cloned
	return continuity.Clone(prepared), nil
}

func hasSuccessfulTodoResult(message provider.Message) bool {
	for _, block := range message.Content {
		result, ok := block.(provider.ToolResult)
		if ok && result.Name == "todo" && !result.IsError {
			return true
		}
	}
	return false
}

// StampContinuityOpening folds the one pending capsule into a complete user
// opening before routing or token estimation. The dedicated first text block
// and reference are an atomic delivery stamp: AppendMessage and replay accept
// the reference only while that exact capsule is current, undelivered,
// non-stale, and rendered byte-for-byte in this message.
func (s *Session) StampContinuityOpening(opening provider.Message) (provider.Message, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	opening = provider.CloneMessage(opening)
	if !continuityOpening(opening) {
		return provider.Message{}, false, fmt.Errorf("continuity can only be stamped on a complete, non-injected user opening")
	}
	if opening.ContinuityRef != "" {
		if err := validateContinuityDelivery(s.state, opening); err != nil {
			return provider.Message{}, false, err
		}
		return opening, true, nil
	}
	current := s.state.Continuity
	if current == nil || current.Cleared || current.ID == s.state.ContinuityRef || continuityStaleForResume(s.state) {
		return opening, false, nil
	}
	rendered, err := continuityDeliveryText(*current)
	if err != nil {
		return provider.Message{}, false, fmt.Errorf("render continuity opening: %w", err)
	}
	stamped := opening
	stamped.Content = make([]provider.Block, 0, len(opening.Content)+1)
	stamped.Content = append(stamped.Content, provider.Text{Text: rendered})
	stamped.Content = append(stamped.Content, opening.Content...)
	stamped.ContinuityRef = current.ID
	return stamped, true, nil
}

func validateContinuityDelivery(state State, message provider.Message) error {
	if !continuity.ValidID(message.ContinuityRef) {
		return fmt.Errorf("message has an invalid continuity reference")
	}
	if !continuityOpening(message) {
		return fmt.Errorf("continuity reference requires a complete, non-injected user opening")
	}
	if state.Continuity == nil || state.Continuity.Cleared || state.Continuity.ID != message.ContinuityRef {
		return fmt.Errorf("message refers to a continuity capsule that is not current")
	}
	if state.ContinuityRef == message.ContinuityRef {
		return fmt.Errorf("continuity capsule %s was already delivered", message.ContinuityRef)
	}
	if continuityStaleForResume(state) {
		return fmt.Errorf("message refers to continuity made stale by later user input")
	}
	rendered, err := continuityDeliveryText(*state.Continuity)
	if err != nil {
		return fmt.Errorf("render continuity delivery: %w", err)
	}
	if len(message.Content) == 0 {
		return fmt.Errorf("continuity reference has no rendered capsule block")
	}
	first, ok := message.Content[0].(provider.Text)
	if !ok || first.Text != rendered {
		return fmt.Errorf("continuity reference does not match the first rendered capsule block")
	}
	for _, block := range message.Content[1:] {
		if text, ok := block.(provider.Text); ok && text.Text == rendered {
			return fmt.Errorf("continuity reference duplicates the rendered capsule block")
		}
	}
	return nil
}

func continuityDeliveryText(c continuity.Capsule) (string, error) {
	rendered, err := continuity.Render(c)
	if err != nil {
		return "", err
	}
	// Ollama and chat-completions adapters flatten adjacent text blocks with
	// no delimiter. Carry the boundary inside the stamped block so both those
	// wires and block-preserving APIs keep capsule and prompt separated.
	return rendered + "\n\n", nil
}

func continuityOpening(message provider.Message) bool {
	return !message.Incomplete && OpensTurn(message)
}

// AppendContinuity redacts, bounds, identities, and durably appends the latest
// working-state capsule at the exact current conversation boundary. The caller
// receives the canonical stored value; it must not retain its pre-redaction
// input as though that were what replay will recover.
func (s *Session) AppendContinuity(c continuity.Capsule) (continuity.Capsule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c.BasisMessages = len(s.state.Messages)
	return s.appendContinuityLocked(c)
}

// AppendTasksContinuity atomically derives the todo-owned fields from the
// latest capsule and appends the result. Keeping the read/derive/write under
// one session lock prevents a concurrent clear or compaction capsule from
// being silently overwritten by a stale snapshot.
func (s *Session) AppendTasksContinuity(tasks []continuity.Task) (continuity.Capsule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var current *continuity.Capsule
	if s.state.Continuity != nil {
		cloned := continuity.Clone(*s.state.Continuity)
		current = &cloned
	}
	next := continuity.WithTasks(current, tasks)
	next.BasisMessages = len(s.state.Messages)
	return s.appendContinuityLocked(next)
}

func (s *Session) appendContinuityLocked(c continuity.Capsule) (continuity.Capsule, error) {
	prepared, err := continuity.Prepare(c)
	if err != nil {
		return continuity.Capsule{}, err
	}
	if err := s.append(RecordContinuity, prepared); err != nil {
		return continuity.Capsule{}, err
	}
	cloned := continuity.Clone(prepared)
	s.state.Continuity = &cloned
	return continuity.Clone(prepared), nil
}

// ClearContinuity appends a tombstone instead of deleting history. A fork
// before the tombstone can still recover the state that was current there;
// replay at or after it cannot accidentally revive that older capsule.
func (s *Session) ClearContinuity(source continuity.Source) (continuity.Capsule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var current *continuity.Capsule
	if s.state.Continuity != nil {
		copy := continuity.Clone(*s.state.Continuity)
		current = &copy
	}
	c := continuity.Tombstone(current, source)
	c.BasisMessages = len(s.state.Messages)
	if current != nil {
		c.ParentSession = s.state.ID
		c.ParentMessages = len(s.state.Messages)
		c.ParentCapsule = current.ID
	}
	return s.appendContinuityLocked(c)
}

func (s *Session) AppendUsage(u Usage) error {
	_, err := s.AppendUsageRecord(u)
	return err
}

// AppendUsageRecord appends one provider receipt and returns the exact stored
// value, including the Session-assigned durable CallID. Observers must receive
// this returned record rather than their pre-append copy or later telemetry
// cannot correlate the callback with the durable log.
func (s *Session) AppendUsageRecord(u Usage) (Usage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u.CallID == "" {
		nextSeq, err := s.nextRecordSequence()
		if err != nil {
			return Usage{}, err
		}
		u.CallID = fmt.Sprintf("call:%s:%d", s.state.ID, nextSeq)
	}
	usage, err := s.state.checkedUsage(u)
	if err != nil {
		return Usage{}, err
	}
	local, external, err := s.state.checkedObservedCost(u.CostMicroUSD, 0)
	if err != nil {
		return Usage{}, err
	}
	if err := s.append(RecordUsage, u); err != nil {
		return Usage{}, err
	}
	s.state.Usage = usage
	s.state.CostMicroUSD, s.state.ExternalCostMicroUSD = local, external
	s.state.Calls++
	s.state.recordProviderCallID(u.CallID)
	s.state.recordUsageTarget(u.Target)
	s.liveUsages = append(s.liveUsages, sequencedUsage{seq: s.seq, usage: u})
	return u, nil
}

// AppendRetryReserve durably accounts for a failed provider attempt without
// pretending it returned successful usage.
func (s *Session) AppendRetryReserve(costMicroUSD int64) error {
	if costMicroUSD == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	reserve, err := checkedMicroUSDAdd(s.state.RetryReserveMicroUSD, costMicroUSD)
	if err != nil {
		return err
	}
	if err := s.append(RecordRetryReserve, RetryReserve{CostMicroUSD: costMicroUSD}); err != nil {
		return err
	}
	s.state.RetryReserveMicroUSD = reserve
	return nil
}

// BeginBudgetAttempt writes and syncs the conservative bound before a provider
// request may be issued. The returned ID is the only token that can settle the
// attempt; losing it is safe because replay keeps the attempt pending.
func (s *Session) BeginBudgetAttempt(costMicroUSD int64) (string, error) {
	if costMicroUSD <= 0 {
		return "", fmt.Errorf("budget attempt cost must be positive")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	nextSeq, err := s.nextRecordSequence()
	if err != nil {
		return "", err
	}
	id := fmt.Sprintf("%s:%d", s.state.ID, nextSeq)
	p := BudgetAttempt{ID: id, CostMicroUSD: costMicroUSD}
	if err := s.state.validateBudgetAttempt(p); err != nil {
		return "", err
	}
	if err := s.append(RecordBudgetAttempt, p); err != nil {
		return "", err
	}
	if err := s.state.applyBudgetAttempt(p); err != nil {
		return "", err
	}
	return id, nil
}

// SettleBudgetAttempt records the known outcome of a write-ahead attempt.
// externalCostMicroUSD is zero for a call whose Usage is in this log and the
// actual priced cost for a delegate/race call whose Usage lives elsewhere.
func (s *Session) SettleBudgetAttempt(attemptID, outcome string, externalCostMicroUSD int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := BudgetSettlement{AttemptID: attemptID, Outcome: outcome, ExternalCostMicroUSD: externalCostMicroUSD}
	if err := s.state.validateBudgetSettlement(p); err != nil {
		return err
	}
	if err := s.append(RecordBudgetSettle, p); err != nil {
		return err
	}
	return s.state.applyBudgetSettlement(p)
}

// AppendBudgetTransfer atomically attributes work from another authoritative
// ledger to this continuing session. Source must be stable for that transfer;
// duplicates are refused so a race verdict cannot silently double charge.
func (s *Session) AppendBudgetTransfer(source string, externalCostMicroUSD, retryReserveMicroUSD int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := BudgetTransfer{Source: source, ExternalCostMicroUSD: externalCostMicroUSD, RetryReserveMicroUSD: retryReserveMicroUSD}
	if err := s.state.validateBudgetTransfer(p); err != nil {
		return err
	}
	if err := s.append(RecordBudgetTransfer, p); err != nil {
		return err
	}
	return s.state.applyBudgetTransfer(p)
}

func (s *Session) MarkRaceBranchPending(originID string) error {
	if originID == "" {
		return fmt.Errorf("race branch origin is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.raceBranchOrigin != "" {
		return fmt.Errorf("session is already a race branch")
	}
	if err := s.append(RecordRaceBranch, RaceBranch{OriginID: originID}); err != nil {
		return err
	}
	s.state.raceBranchOrigin = originID
	s.state.raceBranchPending = true
	return nil
}

func (s *Session) FinalizeRaceBranch() error {
	return s.finalizeRaceBranch(true)
}

// FinalizeRaceBranchAlternative makes a fully reconciled branch explicitly
// resumable without letting --continue choose it over the user's verdict.
func (s *Session) FinalizeRaceBranchAlternative() error {
	return s.finalizeRaceBranch(false)
}

func (s *Session) finalizeRaceBranch(continuation bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.raceBranchOrigin == "" || !s.state.raceBranchPending {
		return fmt.Errorf("session is not a pending race branch")
	}
	if err := s.append(RecordRaceBranch, RaceBranch{OriginID: s.state.raceBranchOrigin, Finalized: true, Continuation: continuation}); err != nil {
		return err
	}
	s.state.raceBranchPending = false
	s.state.raceBranchFinalized = true
	s.state.raceBranchContinuation = continuation
	return nil
}

func (s State) RaceBranchPending() bool { return s.raceBranchPending }

func (s State) raceAlternative() bool {
	return s.raceBranchOrigin != "" && s.raceBranchFinalized && !s.raceBranchContinuation
}

func (s *State) validateBudgetAttempt(p BudgetAttempt) error {
	if p.ID == "" || p.CostMicroUSD <= 0 {
		return fmt.Errorf("invalid pending attempt")
	}
	if _, exists := s.pendingBudgetAttempts[p.ID]; exists {
		return fmt.Errorf("attempt %q is already pending", p.ID)
	}
	if _, err := checkedMicroUSDAdd(s.RetryReserveMicroUSD, p.CostMicroUSD); err != nil {
		return err
	}
	return nil
}

func (s *State) applyBudgetAttempt(p BudgetAttempt) error {
	if err := s.validateBudgetAttempt(p); err != nil {
		return err
	}
	if s.pendingBudgetAttempts == nil {
		s.pendingBudgetAttempts = make(map[string]int64)
	}
	s.pendingBudgetAttempts[p.ID] = p.CostMicroUSD
	reserve, _ := checkedMicroUSDAdd(s.RetryReserveMicroUSD, p.CostMicroUSD)
	s.RetryReserveMicroUSD = reserve
	return nil
}

func (s *State) validateBudgetSettlement(p BudgetSettlement) error {
	bound, exists := s.pendingBudgetAttempts[p.AttemptID]
	if p.AttemptID == "" || !exists || bound <= 0 {
		return fmt.Errorf("attempt %q is not pending", p.AttemptID)
	}
	if p.ExternalCostMicroUSD < 0 {
		return fmt.Errorf("external cost cannot be negative")
	}
	if _, _, err := s.checkedObservedCost(0, p.ExternalCostMicroUSD); err != nil {
		return err
	}
	switch p.Outcome {
	case BudgetOutcomeSucceeded:
		if s.RetryReserveMicroUSD < bound {
			return fmt.Errorf("pending attempt %q exceeds total retry reserve", p.AttemptID)
		}
		return nil
	case BudgetOutcomeFailed:
		if p.ExternalCostMicroUSD != 0 {
			return fmt.Errorf("a failed attempt cannot carry observed external cost")
		}
		return nil
	default:
		return fmt.Errorf("unknown budget outcome %q", p.Outcome)
	}
}

func (s *State) applyBudgetSettlement(p BudgetSettlement) error {
	if err := s.validateBudgetSettlement(p); err != nil {
		return err
	}
	bound := s.pendingBudgetAttempts[p.AttemptID]
	delete(s.pendingBudgetAttempts, p.AttemptID)
	if p.Outcome == BudgetOutcomeSucceeded {
		s.RetryReserveMicroUSD -= bound
	}
	local, external, _ := s.checkedObservedCost(0, p.ExternalCostMicroUSD)
	s.CostMicroUSD, s.ExternalCostMicroUSD = local, external
	return nil
}

func (s *State) validateBudgetTransfer(p BudgetTransfer) error {
	if p.Source == "" {
		return fmt.Errorf("budget transfer source is required")
	}
	if p.ExternalCostMicroUSD < 0 || p.RetryReserveMicroUSD < 0 {
		return fmt.Errorf("budget transfer amounts cannot be negative")
	}
	if _, _, err := s.checkedObservedCost(0, p.ExternalCostMicroUSD); err != nil {
		return err
	}
	if _, err := checkedMicroUSDAdd(s.RetryReserveMicroUSD, p.RetryReserveMicroUSD); err != nil {
		return err
	}
	if s.appliedBudgetTransfers[p.Source] {
		return fmt.Errorf("budget transfer %q was already applied", p.Source)
	}
	return nil
}

func (s *State) applyBudgetTransfer(p BudgetTransfer) error {
	if err := s.validateBudgetTransfer(p); err != nil {
		return err
	}
	if s.appliedBudgetTransfers == nil {
		s.appliedBudgetTransfers = make(map[string]bool)
	}
	s.appliedBudgetTransfers[p.Source] = true
	local, external, _ := s.checkedObservedCost(0, p.ExternalCostMicroUSD)
	reserve, _ := checkedMicroUSDAdd(s.RetryReserveMicroUSD, p.RetryReserveMicroUSD)
	s.CostMicroUSD, s.ExternalCostMicroUSD = local, external
	s.RetryReserveMicroUSD = reserve
	return nil
}

// BeginUsageWindow snapshots the durable sequence immediately before a routed
// turn. The returned cursor is bound to this session and can only be consumed
// by AppendRouteWithUsage.
func (s *Session) BeginUsageWindow() UsageCursor {
	s.mu.Lock()
	defer s.mu.Unlock()
	return UsageCursor{sessionID: s.state.ID, seq: s.seq}
}

// AppendRouteWithUsage fills a route's accounting from the exact durable
// purpose=turn calls appended since cursor, then appends the route atomically
// with respect to every other session writer. This avoids ledger subtraction:
// concurrent background usage and retry-reserve settlement cannot enter the
// route, and a decreasing or saturated aggregate can never yield a negative
// delta.
func (s *Session) AppendRouteWithUsage(cursor UsageCursor, r Route) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cursor.sessionID == "" || cursor.sessionID != s.state.ID || cursor.seq < 0 || cursor.seq > s.seq {
		return errors.New("route usage cursor does not belong to this session boundary")
	}
	if cursor.seq <= s.lastRouteUsageCursor {
		return errors.New("route usage cursor was already consumed")
	}
	var usage provider.Usage
	var cost int64
	var callIDs []string
	seen := make(map[string]bool)
	for _, record := range s.liveUsages {
		if record.seq <= cursor.seq || record.usage.EffectivePurpose() != UsagePurposeTurn {
			continue
		}
		if record.usage.CallID == "" || seen[record.usage.CallID] {
			return errors.New("route usage has no unique durable call identity")
		}
		seen[record.usage.CallID] = true
		var err error
		usage, err = usage.CheckedAdd(record.usage.Usage)
		if err != nil {
			return fmt.Errorf("route usage accounting: %w", err)
		}
		cost, err = checkedMicroUSDAdd(cost, record.usage.CostMicroUSD)
		if err != nil {
			return fmt.Errorf("route cost accounting: %w", err)
		}
		callIDs = append(callIDs, record.usage.CallID)
	}
	r.Usage = usage
	r.CostMicroUSD = cost
	r.UsageCallIDs = callIDs
	if err := s.appendRouteLocked(r); err != nil {
		return err
	}
	s.lastRouteUsageCursor = cursor.seq
	return nil
}

// AppendRoute records §8.4's training signal for one turn. Callers that own a
// live model turn should use AppendRouteWithUsage so its accounting is bound
// to durable provider-call identities.
func (s *Session) AppendRoute(r Route) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendRouteLocked(r)
}

func (s *Session) appendRouteLocked(r Route) error {
	if r.VerificationStatus == "" {
		switch {
		case r.Verified:
			// Older callers only had Verified; a verified result necessarily ran.
			r.VerificationRan = true
			r.VerificationStatus = RouteVerificationPassed
		case r.VerificationRan:
			r.VerificationStatus = RouteVerificationFailed
		default:
			r.VerificationStatus = RouteVerificationUnavailable
		}
	}
	if r.FailureKind != "" && !validRouteFailureKind(r.FailureKind) {
		return fmt.Errorf("unknown route failure kind %q", r.FailureKind)
	}
	return s.append(RecordRoute, r)
}

func validRouteFailureKind(kind string) bool {
	switch kind {
	case RouteFailureProvider, RouteFailureBudget, RouteFailureContext, RouteFailureRoundLimit,
		RouteFailureVerification, RouteFailureCancelled, RouteFailureInternal:
		return true
	default:
		return false
	}
}

// AppendRace records a paired trial's verdict. It lands on the session that
// continues — the picked branch, or the pre-race session when the race was
// abandoned — so the record travels with the history it judged.
func (s *Session) AppendRace(r Race) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.append(RecordRace, r)
}

func (s *Session) AppendPermission(p Permission) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.append(RecordPermission, p)
}

func (s *Session) AppendNote(level, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.append(RecordNote, Note{Level: level, Text: text})
}

// AppendRuntimeBinding durably commits the tier, exact parameterized target,
// and pin posture that a reopened process must reconstruct. Callers append it
// only for permanent manual actions or committed automatic moves; temporary
// one-turn and process-only inference-parameter overrides deliberately do not.
func (s *Session) AppendRuntimeBinding(tier string, target provider.RouteTargetID, pinned bool) error {
	return s.appendRuntimeBinding(tier, target, pinned, nil)
}

// AppendRuntimeBindingNote commits a moving binding and the audit sentence
// that explains it in one framed append. A fallback substitution must not
// leave either a false note with no binding or a binding whose substitution
// disappeared from the durable timeline.
func (s *Session) AppendRuntimeBindingNote(tier string, target provider.RouteTargetID, pinned bool, level, text string) error {
	if level == "" || text == "" {
		return fmt.Errorf("runtime binding note requires a level and text")
	}
	return s.appendRuntimeBinding(tier, target, pinned, &Note{Level: level, Text: text})
}

func (s *Session) appendRuntimeBinding(tier string, target provider.RouteTargetID, pinned bool, note *Note) error {
	if tier == "" || target == "" {
		return fmt.Errorf("runtime binding requires a tier and target")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := RuntimeBinding{Tier: tier, Target: target, Pinned: pinned, Note: note}
	stateBinding := p
	stateBinding.Note = nil
	if note == nil && stateBinding == s.state.RuntimeBinding {
		return nil
	}
	if err := s.append(RecordRuntimeBinding, p); err != nil {
		return err
	}
	s.state.RuntimeBinding = stateBinding
	return nil
}

// AppendWorkspaceBinding durably records the canonical identity of a legacy
// session opened through a symlink spelling. The immutable start path must
// still name the same live directory at this append-locked boundary; after the
// record is synced, that one canonical binding is authoritative even if the
// obsolete alias is later removed.
func (s *Session) AppendWorkspaceBinding(workspace string) error {
	workspace, err := cleanAbsoluteWorkspace(workspace)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.WorkspaceBinding != "" {
		if s.state.WorkspaceBinding == workspace {
			return nil
		}
		return fmt.Errorf("session is already bound to canonical workspace %q", s.state.WorkspaceBinding)
	}
	if s.state.Workspace == workspace {
		return nil
	}
	if !workspaceIdentityMatches(s.state.Workspace, workspace) {
		return fmt.Errorf("legacy workspace %q is not the same live directory as %q", s.state.Workspace, workspace)
	}
	p := WorkspaceBinding{Workspace: workspace}
	if err := s.append(RecordWorkspaceBinding, p); err != nil {
		return err
	}
	s.state.WorkspaceBinding = workspace
	return nil
}

// AppendPin marks the current point in the conversation under a name. The
// count is taken here, under the lock, so the pin means "everything the log
// held when the user said so" whatever arrives next.
func (s *Session) AppendPin(name string) (Pin, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := Pin{Name: name, Messages: len(s.state.Messages)}
	if err := s.append(RecordPin, p); err != nil {
		return Pin{}, err
	}
	s.state.setPin(p)
	return p, nil
}

// setPin keeps one pin per name: setting a name again moves it, because two
// cut points under one name would make /fork's answer depend on which the
// reader found first.
func (st *State) setPin(p Pin) {
	for i, have := range st.Pins {
		if have.Name == p.Name {
			st.Pins[i] = p
			return
		}
	}
	st.Pins = append(st.Pins, p)
}

// Pin resolves a name to its recorded point.
func (st State) Pin(name string) (Pin, bool) {
	for _, p := range st.Pins {
		if p.Name == name {
			return p, true
		}
	}
	return Pin{}, false
}

func (s *Session) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.state
	out.Messages = provider.CloneMessages(s.state.Messages)
	// Active assistant drafts keep immutable text chunks off the canonical
	// message until a reader asks for state. Materialize each one exactly once
	// for this snapshot; checkpoint frequency therefore does not repeatedly
	// copy the complete growing prefix.
	for _, draft := range s.assistantDrafts {
		if draft != nil && draft.messageIndex >= 0 && draft.messageIndex < len(out.Messages) {
			out.Messages[draft.messageIndex] = s.materializeAssistantDraft(draft)
		}
	}
	out.Pins = append([]Pin(nil), s.state.Pins...)
	out.UsageTargets = append([]string(nil), s.state.UsageTargets...)
	if s.state.Continuity != nil {
		copy := continuity.Clone(*s.state.Continuity)
		out.Continuity = &copy
	}
	if s.state.RetryIntent != nil {
		copy := *s.state.RetryIntent
		out.RetryIntent = &copy
	}
	if s.state.pendingBudgetAttempts != nil {
		out.pendingBudgetAttempts = make(map[string]int64, len(s.state.pendingBudgetAttempts))
		for id, amount := range s.state.pendingBudgetAttempts {
			out.pendingBudgetAttempts[id] = amount
		}
	}
	if s.state.appliedBudgetTransfers != nil {
		out.appliedBudgetTransfers = make(map[string]bool, len(s.state.appliedBudgetTransfers))
		for source, applied := range s.state.appliedBudgetTransfers {
			out.appliedBudgetTransfers[source] = applied
		}
	}
	if s.state.providerCallIDs != nil {
		out.providerCallIDs = make(map[string]bool, len(s.state.providerCallIDs))
		for id, recorded := range s.state.providerCallIDs {
			out.providerCallIDs[id] = recorded
		}
	}
	return out
}

// CurrentContinuity returns an isolated snapshot of the latest capsule without
// copying the entire conversation. Audit, lineage, and post-tool persistence
// use this narrow projection on potentially long-running sessions; generic
// binding uses ResumableContinuity so later user authority wins.
func (s *Session) CurrentContinuity() *continuity.Capsule {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Continuity == nil {
		return nil
	}
	copy := continuity.Clone(*s.state.Continuity)
	return &copy
}

// ResumableContinuity returns the latest capsule only while no later
// authoritative user input has made it stale. The capsule remains available
// through CurrentContinuity for audit, lineage, and explicit surfaces; this
// projection is solely the fail-closed state a generic session bind may
// restore into the live todo registry.
func (s *Session) ResumableContinuity() *continuity.Capsule {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Continuity == nil || s.state.Continuity.Cleared || continuityStaleForResume(s.state) {
		return nil
	}
	copy := continuity.Clone(*s.state.Continuity)
	return &copy
}

func (s *Session) ID() string   { return s.state.ID }
func (s *Session) Path() string { return s.path }

// TruncatedBytes is how much of the log replay could not read. Non-zero means
// recorded work was lost and the user is owed that fact.
func (s *Session) TruncatedBytes() int64 { return s.truncated }

func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeLocked()
}

func (s *Session) closeLocked() error {
	if s.closed || s.f == nil {
		return nil
	}
	s.closed = true
	unlockErr := releaseLock(s.f)
	closeErr := s.f.Close()
	return errors.Join(unlockErr, closeErr)
}

func workspaceKey(abs string) string {
	sum := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(sum[:8])
}

// newID sorts lexically by creation time, which makes a directory listing
// chronological without reading any file.
func newID() (string, error) {
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	// Microseconds, not seconds. The id is what orders a directory of sessions
	// when the filesystem stamps two of them in the same tick, and a
	// second-resolution id carries no ordering information at exactly the
	// moment ordering is needed. The random suffix keeps two sessions started
	// in the same microsecond from colliding; it is not what orders them.
	return time.Now().UTC().Format("20060102T150405.000000") + "-" + hex.EncodeToString(suffix[:]), nil
}

// validSessionID accepts only IDs generated by Switchboard. The seconds-only
// shape is retained for pre-microsecond logs; both shapes require the exact
// lowercase random suffix. Callers must apply this before any Glob or path
// join, not merely after opening a candidate.
func validSessionID(id string) bool {
	dash := strings.LastIndexByte(id, '-')
	var layout string
	switch dash {
	case len("20060102T150405"):
		layout = "20060102T150405"
	case len("20060102T150405.000000"):
		layout = "20060102T150405.000000"
	default:
		return false
	}
	if len(id) != dash+1+8 {
		return false
	}
	prefix, suffix := id[:dash], id[dash+1:]
	stamp, err := time.Parse(layout, prefix)
	if err != nil || stamp.Format(layout) != prefix {
		return false
	}
	for _, ch := range suffix {
		if !('0' <= ch && ch <= '9') && !('a' <= ch && ch <= 'f') {
			return false
		}
	}
	return true
}

// binaryVersion records what produced a session so a historical decision can be
// reconstructed against the code that made it.
func binaryVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	var revision, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	switch {
	case revision == "":
		return info.Main.Version
	case modified == "true":
		return revision[:min(12, len(revision))] + "-dirty"
	default:
		return revision[:min(12, len(revision))]
	}
}
