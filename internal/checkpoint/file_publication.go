package checkpoint

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

var standaloneFilePublicationAfterConfigTestHook func()

// PublishFileCAS durably publishes desiredContent at path only while the
// target still has the exact expected state. parent is the directory
// capability the caller used to read that state; it proves the captured parent
// identity, while the atomic namespace operation itself uses the configured
// retained workspace root and the target's full relative path. Published is
// true only when the desired inode crossed the namespace boundary and was not
// positively rolled back. It may therefore be true with an error after a
// durability or final identity check failed.
//
// The caller must first prepare this exact path through RecordState, and must
// finish that transaction with Commit or Abort according to Published. Crash
// recovery must be configured with ConfigureRestoreCleanup before recording
// starts. This restriction keeps the multi-step Windows exchange and every
// platform's displaced-file retirement covered by the same durable ledger as
// checkpoint restore.
func (r *Recorder) PublishFileCAS(
	ctx context.Context,
	path string,
	parent *os.Root,
	leaf string,
	expectedExisted bool,
	expectedMode fs.FileMode,
	expectedContent []byte,
	desiredMode fs.FileMode,
	desiredContent []byte,
	beforePublication func(),
) (published bool, err error) {
	return r.PublishFileCASPrepared(
		ctx, path, parent, leaf,
		expectedExisted, expectedMode, expectedContent,
		desiredMode, desiredContent,
		nil, beforePublication,
	)
}

// PublishFileCASPrepared is PublishFileCAS with one additional preparation
// step on the exact exclusively-created replacement file. prepareTemp runs
// before cleanup-ledger inode binding and before desiredContent is written, so
// authority-bearing callers can install descriptor-bound ACLs without a
// pathname reopen or a window in which sensitive bytes have weaker access.
// A preparation failure is an unpublished outcome and retires both the empty
// temporary and its cleanup-ledger reservation.
func (r *Recorder) PublishFileCASPrepared(
	ctx context.Context,
	path string,
	parent *os.Root,
	leaf string,
	expectedExisted bool,
	expectedMode fs.FileMode,
	expectedContent []byte,
	desiredMode fs.FileMode,
	desiredContent []byte,
	prepareTemp func(*os.File) error,
	beforePublication func(),
) (published bool, err error) {
	return r.publishFileCASPrepared(
		ctx, path, parent, leaf,
		expectedExisted, expectedMode, expectedContent,
		desiredMode, desiredContent,
		-1, prepareTemp, beforePublication,
	)
}

func (r *Recorder) publishFileCASPrepared(
	ctx context.Context,
	path string,
	parent *os.Root,
	leaf string,
	expectedExisted bool,
	expectedMode fs.FileMode,
	expectedContent []byte,
	desiredMode fs.FileMode,
	desiredContent []byte,
	fingerprintLimit int64,
	prepareTemp func(*os.File) error,
	beforePublication func(),
) (published bool, err error) {
	return r.restoreFileCASPrepared(
		ctx, path, parent, leaf,
		FileState{Existed: expectedExisted, Mode: expectedMode, Content: expectedContent},
		FileState{Existed: true, Mode: desiredMode, Content: desiredContent},
		fingerprintLimit, prepareTemp, beforePublication,
	)
}

func (r *Recorder) restoreFileCASPrepared(
	ctx context.Context,
	path string,
	parent *os.Root,
	leaf string,
	expected FileState,
	desired FileState,
	fingerprintLimit int64,
	prepareTemp func(*os.File) error,
	beforePublication func(),
) (published bool, err error) {
	if r == nil {
		return false, errors.New("atomic file publication has no checkpoint recorder")
	}
	if ctx == nil {
		return false, errors.New("atomic file publication has no context")
	}
	if parent == nil {
		return false, errors.New("atomic file publication has no parent capability")
	}
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) || leaf == "" || leaf != filepath.Base(path) || !filepath.IsLocal(leaf) {
		return false, fmt.Errorf("invalid atomic file publication target %q", path)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}

	boundParent, err := parent.Stat(".")
	if err != nil {
		return false, fmt.Errorf("identifying atomic file publication parent: %w", err)
	}
	if !boundParent.IsDir() || boundParent.Mode()&fs.ModeSymlink != 0 {
		return false, errors.New("atomic file publication parent is not a real directory")
	}

	// RecordState owns the lifecycle exclusion: undo waits for this prepared
	// mutation to Commit or Abort. Requiring its live state also gives this
	// publisher the parent identity captured at the read/publish boundary.
	r.mu.Lock()
	if r.restoreCleanup == nil {
		r.mu.Unlock()
		return false, errors.New("atomic file publication requires configured crash recovery")
	}
	if r.cur == nil {
		r.mu.Unlock()
		return false, errors.New("atomic file publication has no open checkpoint turn")
	}
	captured, ok := r.cur.files[path]
	if !ok || captured.activeKind != captureTwoPhase || captured.active <= 0 {
		r.mu.Unlock()
		return false, errors.New("atomic file publication was not prepared with RecordState")
	}
	parentInfo := captured.parent
	parents := append([]ancestorIdentity(nil), captured.parents...)
	parentSet := captured.parentSet
	beforeRemove := r.beforeRemoveHook
	beforeReplace := r.beforeReplaceHook
	publicationSeam := r.publicationSeamHook
	afterReplace := r.afterReplaceHook
	r.mu.Unlock()

	// A missing parent can be created between RecordState and publication. Bind
	// it now, and store that identity in the still-prepared checkpoint so a
	// later undo uses the same directory this publication selected.
	if !parentSet {
		parentInfo, parents, parentSet = parentIdentity(path)
		if !parentSet || parentInfo == nil || !os.SameFile(boundParent, parentInfo) {
			return false, fmt.Errorf("%w: atomic file publication parent changed before it was bound", ErrStale)
		}
		r.mu.Lock()
		if r.cur == nil || r.cur.files[path] != captured || captured.activeKind != captureTwoPhase || captured.active <= 0 {
			r.mu.Unlock()
			return false, fmt.Errorf("%w: atomic file publication checkpoint changed before commit", ErrStale)
		}
		if !captured.parentSet {
			captured.parent = parentInfo
			captured.parents = append([]ancestorIdentity(nil), parents...)
			captured.parentSet = true
		}
		r.mu.Unlock()
	} else if parentInfo == nil || !os.SameFile(boundParent, parentInfo) {
		return false, fmt.Errorf("%w: atomic file publication parent no longer matches its prepared checkpoint", ErrStale)
	}

	state := &fileState{
		existed:   desired.Existed,
		mode:      restorableMode(desired.Mode),
		content:   append([]byte(nil), desired.Content...),
		after:     fingerprintBytes(expected.Existed, expected.Mode, expected.Content),
		parent:    parentInfo,
		parents:   parents,
		parentSet: parentSet,
	}
	beforeMutation := func(internal func() error) func() error {
		return func() error {
			if internal != nil {
				if err := internal(); err != nil {
					return err
				}
			}
			if beforePublication != nil {
				beforePublication()
			}
			return ctx.Err()
		}
	}
	hooks := restoreHooks{
		prepareTemp:      prepareTemp,
		fingerprintLimit: fingerprintLimit,
		beforeRemove:     beforeMutation(beforeRemove),
		beforeReplace:    beforeMutation(beforeReplace),
		publicationSeam:  publicationSeam,
		afterReplace:     afterReplace,
	}
	outcome := r.restoreWithLedger(path, state, hooks)
	return outcome.published, outcome.err
}

// PublishStandaloneFileCAS performs one descriptor-bound, crash-recoverable
// file replacement without adding the source to user-visible undo history.
// maxBytes is a caller-selected complete-file bound capped at 8 MiB; source
// and desired content are rejected before recorder, ledger, or temporary-file
// creation when they exceed it. prepareTemp has the same descriptor-only
// contract as PublishFileCASPrepared.
func PublishStandaloneFileCAS(
	ctx context.Context,
	journalDir string,
	workspace string,
	path string,
	parent *os.Root,
	leaf string,
	expectedExisted bool,
	expectedMode fs.FileMode,
	expectedContent []byte,
	desiredMode fs.FileMode,
	desiredContent []byte,
	maxBytes int64,
	prepareTemp func(*os.File) error,
	beforePublication func(),
) (published bool, err error) {
	if err := validateStandaloneFilePublication(ctx, expectedExisted, expectedContent, desiredContent, maxBytes); err != nil {
		return false, err
	}
	config, err := resolveRestoreCleanup(journalDir, workspace)
	if err != nil {
		return false, err
	}
	return publishStandaloneFileCAS(
		ctx, config, path, parent, leaf,
		expectedExisted, expectedMode, expectedContent,
		desiredMode, desiredContent, maxBytes, prepareTemp, beforePublication,
	)
}

// PublishStandaloneFileCASBound is PublishStandaloneFileCAS with the cleanup
// journal and workspace paths pinned to retained directory capabilities. It is
// intended for authority ledgers whose process lock and state read already use
// those exact roots.
func PublishStandaloneFileCASBound(
	ctx context.Context,
	journalDir string,
	workspace string,
	journalRoot *os.Root,
	workspaceRoot *os.Root,
	path string,
	parent *os.Root,
	leaf string,
	expectedExisted bool,
	expectedMode fs.FileMode,
	expectedContent []byte,
	desiredMode fs.FileMode,
	desiredContent []byte,
	maxBytes int64,
	prepareTemp func(*os.File) error,
	beforePublication func(),
) (published bool, err error) {
	if err := validateStandaloneFilePublication(ctx, expectedExisted, expectedContent, desiredContent, maxBytes); err != nil {
		return false, err
	}
	config, err := resolveRestoreCleanupBound(journalDir, workspace, journalRoot, workspaceRoot)
	if err != nil {
		return false, err
	}
	if standaloneFilePublicationAfterConfigTestHook != nil {
		standaloneFilePublicationAfterConfigTestHook()
	}
	return publishStandaloneFileCAS(
		ctx, config, path, parent, leaf,
		expectedExisted, expectedMode, expectedContent,
		desiredMode, desiredContent, maxBytes, prepareTemp, beforePublication,
	)
}

func validateStandaloneFilePublication(ctx context.Context, expectedExisted bool, expectedContent, desiredContent []byte, maxBytes int64) error {
	if maxBytes <= 0 || maxBytes > maxStandaloneFilePublicationBytes {
		return fmt.Errorf("standalone file publication bound must be between 1 and %d bytes", maxStandaloneFilePublicationBytes)
	}
	if int64(len(expectedContent)) > maxBytes || int64(len(desiredContent)) > maxBytes {
		return fmt.Errorf("standalone file publication exceeds %d bytes", maxBytes)
	}
	if !expectedExisted && len(expectedContent) != 0 {
		return errors.New("absent standalone file publication source has content")
	}
	if ctx == nil {
		return errors.New("standalone file publication has no context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func publishStandaloneFileCAS(
	ctx context.Context,
	config *restoreCleanupConfig,
	path string,
	parent *os.Root,
	leaf string,
	expectedExisted bool,
	expectedMode fs.FileMode,
	expectedContent []byte,
	desiredMode fs.FileMode,
	desiredContent []byte,
	maxBytes int64,
	prepareTemp func(*os.File) error,
	beforePublication func(),
) (published bool, err error) {
	return restoreStandaloneFileCAS(
		ctx, config, path, parent, leaf,
		FileState{Existed: expectedExisted, Mode: expectedMode, Content: expectedContent},
		FileState{Existed: true, Mode: desiredMode, Content: desiredContent},
		maxBytes, prepareTemp, beforePublication,
	)
}

// RestoreStandaloneFileCASBound applies an arbitrary bounded file state under
// retained journal and workspace capabilities. It is the deletion-capable
// counterpart to PublishStandaloneFileCASBound: Expected is compared at the
// final namespace seam, Desired may be present or absent, and every published
// or ambiguous outcome remains covered by the cleanup ledger.
func RestoreStandaloneFileCASBound(
	ctx context.Context,
	journalDir string,
	workspace string,
	journalRoot *os.Root,
	workspaceRoot *os.Root,
	path string,
	parent *os.Root,
	leaf string,
	expected FileState,
	desired FileState,
	maxBytes int64,
	prepareTemp func(*os.File) error,
	beforePublication func(),
) (published bool, err error) {
	if err := validateStandaloneRestore(ctx, expected, desired, maxBytes); err != nil {
		return false, err
	}
	config, err := resolveRestoreCleanupBound(journalDir, workspace, journalRoot, workspaceRoot)
	if err != nil {
		return false, err
	}
	if standaloneFilePublicationAfterConfigTestHook != nil {
		standaloneFilePublicationAfterConfigTestHook()
	}
	return restoreStandaloneFileCAS(
		ctx, config, path, parent, leaf, expected, desired,
		maxBytes, prepareTemp, beforePublication,
	)
}

func restoreStandaloneFileCAS(
	ctx context.Context,
	config *restoreCleanupConfig,
	path string,
	parent *os.Root,
	leaf string,
	expected FileState,
	desired FileState,
	maxBytes int64,
	prepareTemp func(*os.File) error,
	beforePublication func(),
) (published bool, err error) {
	recorder := NewRecorder()
	recorder.restoreCleanup = config
	recorder.Begin("standalone file publication")
	if err := recorder.recordStateForFilePublication(path, expected.Existed, expected.Mode, expected.Content, maxBytes); err != nil {
		return false, err
	}
	published, err = recorder.restoreFileCASPrepared(
		ctx, path, parent, leaf, expected, desired,
		maxBytes, prepareTemp, beforePublication,
	)
	if published {
		recorder.Commit(path, desired.Existed, desired.Mode, sha256.Sum256(desired.Content))
	} else {
		recorder.Abort(path)
	}
	if err == nil && !published {
		err = errors.New("standalone file publication returned without committing")
	}
	return published, err
}

func validateStandaloneRestore(ctx context.Context, expected, desired FileState, maxBytes int64) error {
	if maxBytes <= 0 || maxBytes > maxStandaloneFilePublicationBytes {
		return fmt.Errorf("standalone file publication bound must be between 1 and %d bytes", maxStandaloneFilePublicationBytes)
	}
	for _, state := range []struct {
		name  string
		value FileState
	}{{"expected", expected}, {"desired", desired}} {
		if !state.value.Existed && (state.value.Mode != 0 || len(state.value.Content) != 0) {
			return fmt.Errorf("absent standalone %s state carries file data", state.name)
		}
		if int64(len(state.value.Content)) > maxBytes {
			return fmt.Errorf("standalone file publication exceeds %d bytes", maxBytes)
		}
	}
	if ctx == nil {
		return errors.New("standalone file publication has no context")
	}
	return ctx.Err()
}
