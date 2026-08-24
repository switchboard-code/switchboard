package checkpoint

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// boundAfterNamespaceTestHook is a deterministic seam after a desired inode
// crossed the leaf namespace but before its displaced source is retired. A
// failure here must roll the publication back while both exact files remain
// descriptor-bound. Production leaves it nil.
var boundAfterNamespaceTestHook func() error
var boundRollbackTestHook func() error

// boundRenameRolledBackError is internal positive evidence that a namespace
// primitive crossed an intermediate publication boundary but restored and
// flushed its exact pre-operation names before returning the wrapped cause.
// Higher layers still verify both descriptor-bound names before treating the
// operation as unpublished.
type boundRenameRolledBackError struct {
	cause error
}

func (e *boundRenameRolledBackError) Error() string { return e.cause.Error() }
func (e *boundRenameRolledBackError) Unwrap() error { return e.cause }

func finishBoundRenameRollback(cause, rollbackErr error) error {
	if rollbackErr != nil {
		return errors.Join(cause, rollbackErr)
	}
	return &boundRenameRolledBackError{cause: cause}
}

func boundRenameWasRolledBack(err error) bool {
	var rolledBack *boundRenameRolledBackError
	return errors.As(err, &rolledBack)
}

func verifyBoundReplacementRollback(namespace *boundRestoreNamespace, tmpName string, tmpInfo, targetInfo fs.FileInfo, expected fingerprint) error {
	if err := verifyBoundReplacementNamespaceRollback(namespace, tmpName, tmpInfo, targetInfo); err != nil {
		return err
	}
	targetFP, fingerprintErr := namespace.fingerprintTarget(-1, targetInfo)
	if fingerprintErr == nil && sameFingerprint(targetFP, expected) {
		return nil
	}
	return errors.Join(fingerprintErr,
		fmt.Errorf("%w: namespace rollback restored a changed pre-image", ErrStale))
}

// verifyBoundReplacementNamespaceRollback proves that a failed publication
// put the two exact descriptor-bound inodes back at their original names. A
// concurrent writer may have changed the old target's bytes while it was
// displaced; that is still an unpublished CAS refusal once the desired inode
// is no longer at the target, even though the caller must receive ErrStale.
func verifyBoundReplacementNamespaceRollback(namespace *boundRestoreNamespace, tmpName string, tmpInfo, targetInfo fs.FileInfo) error {
	linkedTarget, targetErr := namespace.root.Lstat(namespace.target)
	linkedTemp, tempErr := namespace.root.Lstat(tmpName)
	_, stagingErr := namespace.root.Lstat(restoreExchangeStagingName(tmpName, namespace.target))
	if targetErr == nil && tempErr == nil && errors.Is(stagingErr, fs.ErrNotExist) &&
		linkedTarget.Mode().IsRegular() && linkedTemp.Mode().IsRegular() &&
		os.SameFile(targetInfo, linkedTarget) && os.SameFile(tmpInfo, linkedTemp) {
		return nil
	}
	return errors.Join(targetErr, tempErr, stagingErr,
		fmt.Errorf("%w: namespace rollback did not restore both exact descriptor-bound names", ErrStale))
}

// verifyBoundPublicationWithdrawn is the weaker, caller-facing rollback fact:
// the desired inode is back at its private temporary name and is no longer the
// target. An external writer is allowed to have moved or replaced the old
// target during the exchange; preserving that writer's result is an
// unpublished stale refusal, not an ambiguous publication.
func verifyBoundPublicationWithdrawn(namespace *boundRestoreNamespace, tmpName string, tmpInfo fs.FileInfo) error {
	linkedTemp, tempErr := namespace.root.Lstat(tmpName)
	linkedTarget, targetErr := namespace.root.Lstat(namespace.target)
	_, stagingErr := namespace.root.Lstat(restoreExchangeStagingName(tmpName, namespace.target))
	targetAbsent := errors.Is(targetErr, fs.ErrNotExist)
	targetExcludesTemp := targetAbsent || (targetErr == nil && !os.SameFile(tmpInfo, linkedTarget))
	if tempErr == nil && linkedTemp.Mode().IsRegular() && os.SameFile(tmpInfo, linkedTemp) &&
		targetExcludesTemp && errors.Is(stagingErr, fs.ErrNotExist) {
		return nil
	}
	return errors.Join(tempErr, targetErr, stagingErr,
		fmt.Errorf("%w: namespace rollback did not withdraw the exact published inode", ErrStale))
}

// rollbackBoundCreation moves the exact descriptor-bound inode published into
// a formerly missing target back to its ledger name and retires it. It uses the
// already-held temporary handle rather than reopening and relocking the target;
// the cleanup ledger intentionally holds that handle's liveness lock.
func rollbackBoundCreation(namespace *boundRestoreNamespace, retirementRoot *os.Root, tmpName string, tmp *os.File, desired fingerprint) (bool, error) {
	tmpInfo, err := tmp.Stat()
	if err != nil {
		return false, err
	}
	current, err := namespace.fingerprintTarget(-1, tmpInfo)
	if err != nil || !sameFingerprint(current, desired) {
		return false, errors.Join(err,
			fmt.Errorf("%w: created target changed before rollback", ErrStale))
	}
	if _, err := namespace.root.Lstat(tmpName); !errors.Is(err, fs.ErrNotExist) {
		return false, errors.Join(err,
			fmt.Errorf("%w: created-file rollback name is occupied", ErrStale))
	}
	result, err := renameBoundRestoreFile(namespace.root, tmp, nil, namespace.target, tmpName, false)
	if err != nil {
		return result.published, err
	}
	linked, linkErr := namespace.root.Lstat(tmpName)
	_, targetErr := namespace.root.Lstat(namespace.target)
	if linkErr != nil || !linked.Mode().IsRegular() || !os.SameFile(tmpInfo, linked) ||
		!errors.Is(targetErr, fs.ErrNotExist) {
		return true, errors.Join(linkErr, targetErr,
			fmt.Errorf("%w: created-file rollback did not restore the exact absent target", ErrStale))
	}
	if err := retireBoundOpenFileTo(namespace.root, retirementRoot, tmpName, tmp, true, nil, nil); err != nil {
		return true, fmt.Errorf("retiring rolled-back created file: %w", err)
	}
	return true, nil
}

func validateRestoreNamespaceLink(scope *restoreScope, path string, state *fileState) error {
	if len(state.parents) > 0 {
		return validateParentIdentity(path, state)
	}
	if scope != nil {
		return scope.validateLinked()
	}
	return nil
}
