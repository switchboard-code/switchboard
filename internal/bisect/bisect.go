// Package bisect finds the recorded turn that turned a verifier red, by
// binary-searching the per-turn states the checkpoint recorder holds.
//
// The search mutates the workspace in place, but every mutation is an exact
// expected-to-desired transition beneath one retained workspace capability.
// A private cleanup journal makes each transition recoverable and a permanent
// sidecar lock prevents two Switchboard processes from reconstructing the same
// workspace concurrently.
package bisect

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/switchboard-code/switchboard/internal/checkpoint"
	"github.com/switchboard-code/switchboard/internal/fileprivacy"
	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

const (
	maxBisectFileBytes = int64(4 << 20)

	// A turn recorder is already bounded, but Runner is also an internal API.
	// These independent limits prevent a hostile or stale caller from using a
	// large collection of individually valid states as a second allocation sink.
	maxBisectFiles        = 1024
	maxBisectCurrentBytes = int64(32 << 20)
	maxBisectPlanBytes    = int64(256 << 20)

	bisectJournalDirectory = ".switchboard-bisect"
	bisectLockFile         = "lock"
)

// ErrLocked reports that another process owns this workspace's bisect
// transaction. The permanent lock name is never removed; kernel ownership is
// released with the descriptor, including after a process crash.
var ErrLocked = errors.New("another process is already bisecting this workspace")

type Verdict struct {
	Passed    bool
	FirstFail string
	Err       error
}

type Outcome int

const (
	Found Outcome = iota
	AlreadyGreen
	RedBeforeRecord
)

type Result struct {
	Outcome Outcome
	Culprit int
	Fail    Verdict
	Probes  int
}

// Runner bisects one canonical workspace in place.
type Runner struct {
	// Workspace must be the canonical absolute workspace selected by startup.
	// Every checkpoint path must be a canonical absolute descendant of it.
	Workspace string

	// JournalDir is an owner-private durable state directory, normally the
	// session store's per-workspace directory. Runner creates a dedicated child
	// beneath it for checkpoint cleanup ledgers and the cross-process lock.
	JournalDir string

	States  []map[string]checkpoint.FileState
	Verify  func(context.Context) Verdict
	OnProbe func(turn, probes int)

	// Deterministic security-test seam after the parent capability is retained.
	beforeRestore func(string)
}

type RestoreError struct{ Err error }

func (e *RestoreError) Error() string {
	return "the workspace was not fully restored: " + e.Err.Error()
}

func (e *RestoreError) Unwrap() error { return e.Err }

type workspaceAuthority struct {
	workspace     string
	workspaceRoot *os.Root
	workspaceInfo fs.FileInfo

	journal     string
	journalRoot *os.Root
	journalInfo fs.FileInfo
	lock        *os.File
	lockInfo    fs.FileInfo

	beforeRestore func(string)
}

// Run restores the exact current state before every return. A failed restore
// is always surfaced as a RestoreError beside the original cause.
func (r *Runner) Run(ctx context.Context) (result Result, err error) {
	if ctx == nil {
		return Result{}, errors.New("bisect has no context")
	}
	if len(r.States) == 0 {
		return Result{}, errors.New("no recorded turns to bisect")
	}
	if r.Verify == nil {
		return Result{}, errors.New("bisect has no verifier")
	}

	authority, err := bindWorkspaceAuthority(r.Workspace)
	if err != nil {
		return Result{}, err
	}
	defer func() { err = errors.Join(err, authority.close()) }()

	journalBase, err := canonicalDirectory(r.JournalDir)
	if err != nil {
		return Result{}, fmt.Errorf("resolving bisect cleanup directory: %w", err)
	}
	journalPath := filepath.Join(journalBase, bisectJournalDirectory)
	states, paths, err := prepareStates(authority.workspace, journalPath, r.States)
	if err != nil {
		return Result{}, err
	}
	if err := authority.bindJournal(ctx, journalBase); err != nil {
		return Result{}, err
	}
	authority.beforeRestore = r.beforeRestore

	current := make(map[string]checkpoint.FileState, len(paths))
	var currentBytes int64
	for _, path := range paths {
		state, captureErr := authority.capture(path, nil)
		if captureErr != nil {
			return Result{}, fmt.Errorf("cannot capture %s before probing: %w", path, captureErr)
		}
		currentBytes += int64(len(state.Content))
		if currentBytes > maxBisectCurrentBytes {
			return Result{}, fmt.Errorf("current bisect images exceed the %d-byte aggregate limit", maxBisectCurrentBytes)
		}
		current[path] = state
	}

	// live advances after every published CAS, including a publication that
	// later reports a durability error. It remains the expected side of rollback
	// after a partially completed multi-file probe.
	live := cloneStateMap(current)
	defer func() {
		if restoreErr := authority.restoreAll(context.Background(), live, current, paths); restoreErr != nil {
			err = errors.Join(err, &RestoreError{Err: restoreErr})
		}
	}()

	probes := 0
	verifyAt := func(state int) (Verdict, error) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Verdict{}, ctxErr
		}
		if r.OnProbe != nil {
			r.OnProbe(state, probes)
		}
		desired := current
		if state < len(states) {
			desired = overlay(current, states[state])
		}
		if applyErr := authority.restoreAll(ctx, live, desired, paths); applyErr != nil {
			return Verdict{}, applyErr
		}
		probes++
		v := r.Verify(ctx)
		if v.Err != nil {
			return Verdict{}, fmt.Errorf("the verifier could not run: %w", v.Err)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Verdict{}, ctxErr
		}
		return v, nil
	}

	now, err := verifyAt(len(states))
	if err != nil {
		return Result{}, err
	}
	if now.Passed {
		return Result{Outcome: AlreadyGreen, Probes: probes}, nil
	}
	oldest, err := verifyAt(0)
	if err != nil {
		return Result{}, err
	}
	if !oldest.Passed {
		return Result{Outcome: RedBeforeRecord, Fail: oldest, Probes: probes}, nil
	}

	lo, hi, fail := 0, len(states), now
	for hi-lo > 1 {
		mid := (lo + hi) / 2
		v, verifyErr := verifyAt(mid)
		if verifyErr != nil {
			return Result{}, verifyErr
		}
		if v.Passed {
			lo = mid
		} else {
			hi, fail = mid, v
		}
	}
	return Result{Outcome: Found, Culprit: lo, Fail: fail, Probes: probes}, nil
}

func bindWorkspaceAuthority(workspace string) (*workspaceAuthority, error) {
	if strings.TrimSpace(workspace) == "" || !filepath.IsAbs(workspace) || filepath.Clean(workspace) != workspace {
		return nil, fmt.Errorf("bisect workspace %q is not a canonical absolute path", workspace)
	}
	resolved, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolving bisect workspace: %w", err)
	}
	resolved = filepath.Clean(resolved)
	if resolved != workspace {
		return nil, fmt.Errorf("bisect workspace %q is not canonical (resolved to %q)", workspace, resolved)
	}
	before, err := os.Lstat(workspace)
	if err != nil || !before.IsDir() || before.Mode()&fs.ModeSymlink != 0 {
		return nil, errors.Join(err, fmt.Errorf("bisect workspace %s is not a physical directory", workspace))
	}
	root, err := rootedfs.OpenRoot(workspace)
	if err != nil {
		return nil, fmt.Errorf("binding bisect workspace: %w", err)
	}
	fail := func(cause error) (*workspaceAuthority, error) {
		return nil, errors.Join(cause, root.Close())
	}
	opened, err := root.Stat(".")
	if err != nil {
		return fail(err)
	}
	linked, linkErr := os.Lstat(workspace)
	if linkErr != nil || !linked.IsDir() || linked.Mode()&fs.ModeSymlink != 0 ||
		!os.SameFile(before, opened) || !os.SameFile(opened, linked) {
		return fail(errors.Join(linkErr, errors.New("bisect workspace changed while its capability was retained")))
	}
	return &workspaceAuthority{workspace: workspace, workspaceRoot: root, workspaceInfo: opened}, nil
}

func (a *workspaceAuthority) bindJournal(ctx context.Context, base string) error {
	if a == nil || a.workspaceRoot == nil {
		return errors.New("bisect workspace authority is closed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	base, err := canonicalDirectory(base)
	if err != nil {
		return fmt.Errorf("binding bisect cleanup directory: %w", err)
	}
	baseRoot, err := rootedfs.OpenRoot(base)
	if err != nil {
		return fmt.Errorf("binding bisect cleanup directory: %w", err)
	}
	baseDir, err := baseRoot.Open(".")
	if err != nil {
		return errors.Join(err, baseRoot.Close())
	}
	openedBase, statBaseErr := baseDir.Stat()
	linkedBase, linkBaseErr := os.Lstat(base)
	ownerOnly, privateErr := fileprivacy.DirectoryIsOwnerOnly(baseDir)
	closeDirErr := baseDir.Close()
	if statBaseErr != nil || linkBaseErr != nil || privateErr != nil || closeDirErr != nil || !ownerOnly ||
		!openedBase.IsDir() || !linkedBase.IsDir() || linkedBase.Mode()&fs.ModeSymlink != 0 ||
		!os.SameFile(openedBase, linkedBase) {
		return errors.Join(statBaseErr, linkBaseErr, privateErr, closeDirErr, baseRoot.Close(),
			fmt.Errorf("bisect cleanup directory %s is not owner-private", base))
	}
	journalRoot, err := fileprivacy.EnsurePrivateDirInRoot(baseRoot, bisectJournalDirectory)
	closeBaseErr := baseRoot.Close()
	if err != nil || closeBaseErr != nil {
		if journalRoot != nil {
			_ = journalRoot.Close()
		}
		return errors.Join(err, closeBaseErr)
	}
	journal := filepath.Join(base, bisectJournalDirectory)
	journalInfo, statJournalErr := journalRoot.Stat(".")
	linkedJournal, linkJournalErr := os.Lstat(journal)
	if statJournalErr != nil || linkJournalErr != nil || !journalInfo.IsDir() || !linkedJournal.IsDir() ||
		linkedJournal.Mode()&fs.ModeSymlink != 0 || !os.SameFile(journalInfo, linkedJournal) {
		return errors.Join(statJournalErr, linkJournalErr, journalRoot.Close(),
			errors.New("bisect cleanup journal changed while it was retained"))
	}
	lock, _, err := fileprivacy.OpenReadWriteOrCreateInRoot(journalRoot, bisectLockFile)
	if err != nil {
		return errors.Join(fmt.Errorf("opening bisect process lock: %w", err), journalRoot.Close())
	}
	lockInfo, statErr := lock.Stat()
	linkedLock, linkErr := journalRoot.Lstat(bisectLockFile)
	if statErr != nil || linkErr != nil || !lockInfo.Mode().IsRegular() || !linkedLock.Mode().IsRegular() ||
		!os.SameFile(lockInfo, linkedLock) {
		return errors.Join(statErr, linkErr, lock.Close(), journalRoot.Close(),
			errors.New("bisect process lock changed while it was retained"))
	}
	if err := acquireBisectLock(lock); err != nil {
		return errors.Join(err, lock.Close(), journalRoot.Close())
	}
	lockedName, lockedNameErr := journalRoot.Lstat(bisectLockFile)
	if lockedNameErr != nil || !lockedName.Mode().IsRegular() || !os.SameFile(lockInfo, lockedName) {
		return errors.Join(lockedNameErr, releaseBisectLock(lock), lock.Close(), journalRoot.Close(),
			errors.New("bisect process lock changed while it was acquired"))
	}
	a.journal, a.journalRoot, a.journalInfo = journal, journalRoot, journalInfo
	a.lock, a.lockInfo = lock, lockInfo
	if err := checkpoint.RecoverFilePublicationCleanupBound(journal, a.workspace, journalRoot, a.workspaceRoot); err != nil {
		return errors.Join(fmt.Errorf("recovering interrupted bisect publication: %w", err), a.closeJournal())
	}
	return nil
}

func canonicalDirectory(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("directory path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	resolved = filepath.Clean(resolved)
	info, err := os.Lstat(resolved)
	if err != nil || !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return "", errors.Join(err, fmt.Errorf("%s is not a physical directory", resolved))
	}
	return resolved, nil
}

func (a *workspaceAuthority) validateWorkspaceLinked() error {
	if a == nil || a.workspaceRoot == nil || a.workspaceInfo == nil {
		return errors.New("bisect workspace authority is closed")
	}
	bound, err := a.workspaceRoot.Stat(".")
	linked, linkErr := os.Lstat(a.workspace)
	if err != nil || linkErr != nil || !bound.IsDir() || !linked.IsDir() || linked.Mode()&fs.ModeSymlink != 0 ||
		!os.SameFile(a.workspaceInfo, bound) || !os.SameFile(bound, linked) {
		return errors.Join(err, linkErr, errors.New("bisect workspace changed identity while the run was active"))
	}
	return nil
}

func prepareStates(workspace, journal string, states []map[string]checkpoint.FileState) ([]map[string]checkpoint.FileState, []string, error) {
	out := make([]map[string]checkpoint.FileState, len(states))
	paths := make(map[string]struct{})
	var planBytes int64
	for i, stateMap := range states {
		out[i] = make(map[string]checkpoint.FileState, len(stateMap))
		for path, state := range stateMap {
			if _, err := workspaceRelative(workspace, path); err != nil {
				return nil, nil, fmt.Errorf("turn state %d: %w", i, err)
			}
			if pathInsideOrEqual(path, journal) {
				return nil, nil, fmt.Errorf("turn state %d: bisect path %s overlaps its cleanup journal", i, path)
			}
			state, err := normalizeState(state)
			if err != nil {
				return nil, nil, fmt.Errorf("turn state %d path %s: %w", i, path, err)
			}
			planBytes += int64(len(state.Content))
			if planBytes > maxBisectPlanBytes {
				return nil, nil, fmt.Errorf("bisect plan exceeds the %d-byte aggregate limit", maxBisectPlanBytes)
			}
			state.Content = append([]byte(nil), state.Content...)
			out[i][path] = state
			paths[path] = struct{}{}
		}
	}
	if len(paths) > maxBisectFiles {
		return nil, nil, fmt.Errorf("bisect plan contains %d files, over the %d-file limit", len(paths), maxBisectFiles)
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	return out, ordered, nil
}

func pathInsideOrEqual(path, directory string) bool {
	rel, err := filepath.Rel(directory, path)
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}

func normalizeState(state checkpoint.FileState) (checkpoint.FileState, error) {
	if !state.Existed {
		if state.Mode != 0 || len(state.Content) != 0 {
			return checkpoint.FileState{}, errors.New("absent file state carries mode or content")
		}
		return checkpoint.FileState{}, nil
	}
	if int64(len(state.Content)) > maxBisectFileBytes {
		return checkpoint.FileState{}, fmt.Errorf("file state exceeds the %d-byte checkpoint limit", maxBisectFileBytes)
	}
	// Recorder snapshots are already normalized, but Runner is also an
	// internal API and durable snapshots from before Windows normalization can
	// legitimately carry a Unix permission mask such as 0644. Accept only the
	// portable permission/special-bit vocabulary, then reduce it to the exact
	// mode this platform can restore. File-type bits still fail closed.
	if state.Mode&^(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0 {
		return checkpoint.FileState{}, errors.New("file state carries unsupported mode bits")
	}
	state.Mode = bisectRestorableMode(state.Mode)
	return state, nil
}

func workspaceRelative(workspace, path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", fmt.Errorf("bisect path %q is not a canonical absolute path", path)
	}
	rel, err := filepath.Rel(workspace, path)
	if err != nil || rel == "." || filepath.IsAbs(rel) || !filepath.IsLocal(rel) {
		return "", errors.Join(err, fmt.Errorf("bisect path %s is outside workspace %s", path, workspace))
	}
	return rel, nil
}

func (a *workspaceAuthority) capture(path string, beforeOpen func()) (checkpoint.FileState, error) {
	if err := a.validateWorkspaceLinked(); err != nil {
		return checkpoint.FileState{}, err
	}
	rel, err := workspaceRelative(a.workspace, path)
	if err != nil {
		return checkpoint.FileState{}, err
	}
	info, err := a.workspaceRoot.Lstat(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return checkpoint.FileState{}, nil
		}
		return checkpoint.FileState{}, err
	}
	if !info.Mode().IsRegular() {
		return checkpoint.FileState{}, fmt.Errorf("%s is no longer a regular file", path)
	}
	if info.Size() > maxBisectFileBytes {
		return checkpoint.FileState{}, fmt.Errorf("%s exceeds the %d-byte checkpoint file limit", path, maxBisectFileBytes)
	}
	if beforeOpen != nil {
		beforeOpen()
	}
	file, err := openBisectRead(a.workspaceRoot, rel)
	if err != nil {
		return checkpoint.FileState{}, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return checkpoint.FileState{}, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return checkpoint.FileState{}, fmt.Errorf("%s changed identity while its current state was captured", path)
	}
	content, err := io.ReadAll(io.LimitReader(file, maxBisectFileBytes+1))
	if err != nil {
		return checkpoint.FileState{}, err
	}
	if int64(len(content)) > maxBisectFileBytes {
		return checkpoint.FileState{}, fmt.Errorf("%s grew beyond the %d-byte checkpoint file limit", path, maxBisectFileBytes)
	}
	finished, err := file.Stat()
	if err != nil {
		return checkpoint.FileState{}, err
	}
	linked, linkErr := a.workspaceRoot.Lstat(rel)
	if linkErr != nil || !linked.Mode().IsRegular() || !os.SameFile(opened, finished) ||
		!os.SameFile(finished, linked) || opened.Size() != finished.Size() ||
		finished.Size() != int64(len(content)) || !opened.ModTime().Equal(finished.ModTime()) {
		return checkpoint.FileState{}, errors.Join(linkErr, fmt.Errorf("%s changed while its current state was captured", path))
	}
	if err := a.validateWorkspaceLinked(); err != nil {
		return checkpoint.FileState{}, err
	}
	return checkpoint.FileState{Existed: true, Mode: bisectRestorableMode(info.Mode()), Content: content}, nil
}

func (a *workspaceAuthority) recover() error {
	if a == nil || a.journalRoot == nil || a.workspaceRoot == nil {
		return errors.New("bisect cleanup authority is closed")
	}
	if err := a.validateJournalAuthority(); err != nil {
		return err
	}
	return checkpoint.RecoverFilePublicationCleanupBound(a.journal, a.workspace, a.journalRoot, a.workspaceRoot)
}

func (a *workspaceAuthority) validateJournalAuthority() error {
	if a == nil || a.journalRoot == nil || a.journalInfo == nil || a.lock == nil || a.lockInfo == nil {
		return errors.New("bisect cleanup authority is closed")
	}
	bound, boundErr := a.journalRoot.Stat(".")
	linked, linkErr := os.Lstat(a.journal)
	openedLock, lockStatErr := a.lock.Stat()
	linkedLock, lockLinkErr := a.journalRoot.Lstat(bisectLockFile)
	if boundErr != nil || linkErr != nil || lockStatErr != nil || lockLinkErr != nil ||
		!bound.IsDir() || !linked.IsDir() || linked.Mode()&fs.ModeSymlink != 0 ||
		!openedLock.Mode().IsRegular() || !linkedLock.Mode().IsRegular() ||
		!os.SameFile(a.journalInfo, bound) || !os.SameFile(bound, linked) ||
		!os.SameFile(a.lockInfo, openedLock) || !os.SameFile(openedLock, linkedLock) {
		return errors.Join(boundErr, linkErr, lockStatErr, lockLinkErr,
			errors.New("bisect cleanup journal or process lock changed identity"))
	}
	return nil
}

func (a *workspaceAuthority) restoreAll(ctx context.Context, live, desired map[string]checkpoint.FileState, paths []string) error {
	if err := a.recover(); err != nil {
		return err
	}
	var failures []error
	for _, path := range paths {
		expected, expectedOK := live[path]
		want, desiredOK := desired[path]
		if !expectedOK || !desiredOK {
			failures = append(failures, fmt.Errorf("%s has no complete bisect state", path))
			continue
		}
		if stateEqual(expected, want) {
			actual, captureErr := a.capture(path, nil)
			if captureErr != nil || !stateEqual(actual, expected) {
				failures = append(failures, errors.Join(captureErr,
					fmt.Errorf("%w: %s changed outside the bisect transaction", checkpoint.ErrStale, path)))
			}
			continue
		}
		published, restoreErr := a.restoreOne(ctx, path, expected, want)
		if published {
			live[path] = want
		}
		if restoreErr != nil {
			failures = append(failures, fmt.Errorf("%s: %w", path, restoreErr))
		}
	}
	if len(failures) == 0 {
		return nil
	}
	recoveryErr := a.recover()
	return errors.Join(fmt.Errorf("restore left %d files wrong", len(failures)), errors.Join(failures...), recoveryErr)
}

func (a *workspaceAuthority) restoreOne(ctx context.Context, path string, expected, desired checkpoint.FileState) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := a.validateWorkspaceLinked(); err != nil {
		return false, err
	}
	rel, err := workspaceRelative(a.workspace, path)
	if err != nil {
		return false, err
	}
	parent, err := rootedfs.OpenRootAt(a.workspaceRoot, filepath.Dir(rel))
	if err != nil {
		return false, err
	}
	defer parent.Close()
	if a.beforeRestore != nil {
		a.beforeRestore(path)
	}
	return checkpoint.RestoreStandaloneFileCASBound(
		ctx, a.journal, a.workspace, a.journalRoot, a.workspaceRoot,
		path, parent, filepath.Base(rel), expected, desired,
		maxBisectFileBytes, nil, nil,
	)
}

func (a *workspaceAuthority) closeJournal() error {
	if a == nil {
		return nil
	}
	var err error
	if a.lock != nil {
		err = errors.Join(err, releaseBisectLock(a.lock), a.lock.Close())
		a.lock = nil
	}
	if a.journalRoot != nil {
		err = errors.Join(err, a.journalRoot.Close())
		a.journalRoot = nil
		a.journalInfo = nil
	}
	a.lockInfo = nil
	return err
}

func (a *workspaceAuthority) close() error {
	if a == nil {
		return nil
	}
	err := a.closeJournal()
	if a.workspaceRoot != nil {
		err = errors.Join(err, a.workspaceRoot.Close())
		a.workspaceRoot = nil
	}
	return err
}

func overlay(base, states map[string]checkpoint.FileState) map[string]checkpoint.FileState {
	out := make(map[string]checkpoint.FileState, len(base))
	for path, state := range base {
		if past, ok := states[path]; ok {
			state = past
		}
		out[path] = state
	}
	return out
}

func cloneStateMap(states map[string]checkpoint.FileState) map[string]checkpoint.FileState {
	out := make(map[string]checkpoint.FileState, len(states))
	for path, state := range states {
		state.Content = append([]byte(nil), state.Content...)
		out[path] = state
	}
	return out
}

func stateEqual(left, right checkpoint.FileState) bool {
	return left.Existed == right.Existed && left.Mode == right.Mode && bytes.Equal(left.Content, right.Content)
}
