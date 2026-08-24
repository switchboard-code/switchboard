//go:build unix

package checkpoint

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

var moveBoundNameToTestHook func(*os.Root, *os.Root, string, string) error
var retirementCompatibilityTestHook func() error
var boundNamespaceMutationTestHook func()

func ensureRetirementCompatible(source, sink *os.Root) error {
	if sink == nil {
		return nil
	}
	if retirementCompatibilityTestHook != nil {
		return retirementCompatibilityTestHook()
	}
	left, err := source.Open(".")
	if err != nil {
		return err
	}
	defer left.Close()
	right, err := sink.Open(".")
	if err != nil {
		return err
	}
	defer right.Close()
	leftInfo, err := left.Stat()
	if err != nil {
		return err
	}
	rightInfo, err := right.Stat()
	if err != nil {
		return err
	}
	leftStat, leftOK := leftInfo.Sys().(*syscall.Stat_t)
	rightStat, rightOK := rightInfo.Sys().(*syscall.Stat_t)
	if !leftOK || !rightOK || leftStat == nil || rightStat == nil {
		return errors.New("checkpoint retirement roots have no stable device identity")
	}
	if leftStat.Dev != rightStat.Dev {
		return fmt.Errorf("checkpoint workspace and cleanup directory are on different filesystems")
	}
	return nil
}

func validateBoundRestorePlatform() error { return nil }

func syncBoundDirectory(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func syncBoundReplacement(*os.File) error { return nil }

func renameBoundOpenFile(parent *os.Root, file, _ *os.File, from, to string, replace bool) (bool, error) {
	if replace {
		if boundNamespaceMutationTestHook != nil {
			boundNamespaceMutationTestHook()
		}
		if err := exchangeBoundNames(parent, from, to); err != nil {
			return false, err
		}
		return true, nil
	}
	// A restore into an absent name must never clobber a file that appears at
	// the last instant. The platform no-replace rename also leaves only one
	// link: recovery can therefore distinguish an unpublished source from the
	// exact inode published at the target without ever scrubbing a target alias.
	if boundNamespaceMutationTestHook != nil {
		boundNamespaceMutationTestHook()
	}
	if err := moveNoReplaceBoundNames(parent, from, to); err != nil {
		return false, err
	}
	opened, err := file.Stat()
	if err != nil {
		return true, err
	}
	linked, err := parent.Lstat(to)
	if err != nil || !linked.Mode().IsRegular() || !os.SameFile(opened, linked) {
		return true, errors.Join(err,
			fmt.Errorf("%w: no-replace publication moved a foreign inode", ErrStale))
	}
	if _, err := parent.Lstat(from); !errors.Is(err, os.ErrNotExist) {
		return true, errors.Join(err,
			fmt.Errorf("%w: no-replace publication retained its source name", ErrStale))
	}
	if err := syncBoundDirectory(parent); err != nil {
		return true, err
	}
	return true, nil
}

func rollbackBoundReplacement(root *os.Root, _, _ *os.File, sourceName, targetName string) (bool, error) {
	// Bind both names as they exist after the first exchange. A hostile writer
	// may have substituted either name after the caller's final observation;
	// the only safe rollback claim is that this second atomic exchange restored
	// those exact two inodes to their pre-exchange names. In particular, do not
	// require the original restore temporary to be at sourceName: a source-name
	// substitution leaves that owned inode reachable only through its already
	// open handle, and deferred cleanup must scrub that handle rather than touch
	// the foreign inode now occupying sourceName.
	rollbackSource, err := openCheckpointRootRead(root, sourceName)
	if err != nil {
		return false, err
	}
	defer rollbackSource.Close()
	rollbackTarget, err := openCheckpointRootRead(root, targetName)
	if err != nil {
		return false, err
	}
	defer rollbackTarget.Close()
	sourceInfo, err := rollbackSource.Stat()
	if err != nil {
		return false, err
	}
	targetInfo, err := rollbackTarget.Stat()
	if err != nil {
		return false, err
	}
	linkedSource, err := root.Lstat(sourceName)
	if err != nil || !linkedSource.Mode().IsRegular() || !os.SameFile(sourceInfo, linkedSource) {
		return false, errors.Join(err,
			fmt.Errorf("%w: checkpoint rollback source changed identity", ErrStale))
	}
	linkedTarget, err := root.Lstat(targetName)
	if err != nil || !linkedTarget.Mode().IsRegular() || !os.SameFile(targetInfo, linkedTarget) {
		return false, errors.Join(err,
			fmt.Errorf("%w: checkpoint rollback target changed identity", ErrStale))
	}
	if err := exchangeBoundNames(root, sourceName, targetName); err != nil {
		return false, err
	}
	if err := syncBoundDirectory(root); err != nil {
		return true, err
	}
	linkedSource, sourceErr := root.Lstat(sourceName)
	linkedTarget, targetErr := root.Lstat(targetName)
	if sourceErr != nil || targetErr != nil || !linkedSource.Mode().IsRegular() || !linkedTarget.Mode().IsRegular() ||
		!os.SameFile(targetInfo, linkedSource) || !os.SameFile(sourceInfo, linkedTarget) {
		return true, errors.Join(sourceErr, targetErr,
			fmt.Errorf("%w: checkpoint exchange rollback did not restore both selected inodes", ErrStale))
	}
	return true, nil
}

// POSIX has no portable unlink-by-descriptor primitive. Move the currently
// linked name to an unguessable quarantine, verify what moved, scrub the exact
// opened inode, and retain the quarantine. If a writer substitutes the source
// at the final seam, its inode may be quarantined but is never unlinked or
// scrubbed; the mismatch is surfaced as stale/recovery-required by the caller.
func retireBoundOpenFile(root *os.Root, name string, file *os.File, owned bool, before func(), after func(string)) error {
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	linked, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: checkpoint cleanup name disappeared before retirement", ErrStale)
	}
	if err != nil || !linked.Mode().IsRegular() || !os.SameFile(linked, opened) {
		return errors.Join(err,
			fmt.Errorf("%w: checkpoint cleanup name changed identity", ErrStale))
	}
	if before != nil {
		before()
	}
	linked, err = root.Lstat(name)
	if err != nil || !linked.Mode().IsRegular() || !os.SameFile(linked, opened) {
		return errors.Join(err,
			fmt.Errorf("%w: checkpoint cleanup name changed at the retirement seam", ErrStale))
	}
	quarantine, err := unusedQuarantineSibling(root, name)
	if err != nil {
		return err
	}
	if err := root.Rename(name, quarantine); err != nil {
		return err
	}
	if after != nil {
		after(quarantine)
	}
	if err := syncBoundDirectory(root); err != nil {
		return err
	}
	moved, err := root.Lstat(quarantine)
	if err != nil || !moved.Mode().IsRegular() || !os.SameFile(moved, opened) {
		return errors.Join(err,
			fmt.Errorf("%w: checkpoint cleanup quarantined a replacement; it was retained as %s", ErrStale, quarantine))
	}
	if owned {
		return scrubBoundOpenFileWithLinks(file, 1)
	}
	return nil
}

func retireBoundOpenFileTo(root, sink *os.Root, name string, file *os.File, owned bool, before func(), after func(string)) error {
	if sink == nil {
		return retireBoundOpenFile(root, name, file, owned, before, after)
	}
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	linked, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: workspace cleanup name disappeared before retirement", ErrStale)
	}
	if err != nil || !linked.Mode().IsRegular() || !os.SameFile(linked, opened) {
		return errors.Join(err, fmt.Errorf("%w: workspace cleanup name changed identity", ErrStale))
	}
	if before != nil {
		before()
	}
	linked, err = root.Lstat(name)
	if err != nil || !linked.Mode().IsRegular() || !os.SameFile(linked, opened) {
		return errors.Join(err, fmt.Errorf("%w: workspace cleanup name changed at the retirement seam", ErrStale))
	}
	retired := retiredSinkName(name)
	if _, err := sink.Lstat(retired); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(err, fmt.Errorf("%w: trusted checkpoint retirement name is occupied", ErrStale))
	}
	move := moveBoundNameTo
	if moveBoundNameToTestHook != nil {
		move = moveBoundNameToTestHook
	}
	if err := move(root, sink, name, retired); err != nil {
		return err
	}
	if after != nil {
		after(retired)
	}
	if err := errors.Join(syncBoundDirectory(root), syncBoundDirectory(sink)); err != nil {
		return err
	}
	moved, err := sink.Lstat(retired)
	if err != nil || !moved.Mode().IsRegular() || !os.SameFile(moved, opened) {
		return errors.Join(err,
			fmt.Errorf("%w: trusted checkpoint retirement contains a replacement", ErrStale))
	}
	return removeTrustedRetiredFile(sink, retired, file, owned)
}

func removeTrustedRetiredFile(root *os.Root, name string, file *os.File, owned bool) error {
	// An owned checkpoint temporary may have been hardlinked out of the
	// workspace after O_EXCL creation. Keep the exact trusted-sink name and its
	// ledger evidence when that happened: unlinking first would make recovery
	// unable to name the inode, while truncating it would corrupt the outside
	// alias. Workspace targets are deliberately not subject to this check;
	// their content is never scrubbed and unlinking the trusted retirement link
	// cannot mutate another hardlink.
	if owned {
		if err := requireBoundOpenFileLinks(file, 1); err != nil {
			return err
		}
	}
	if err := root.Remove(name); err != nil {
		return err
	}
	if err := syncBoundDirectory(root); err != nil {
		return err
	}
	if owned {
		return scrubBoundOpenFileWithLinks(file, 0)
	}
	return nil
}

func removeLocalRetiredFile(_ *os.Root, _ string, file *os.File, owned bool) error {
	if owned {
		return scrubBoundOpenFileWithLinks(file, 1)
	}
	return nil
}

func scrubBoundOpenFileWithLinks(file *os.File, want uint64) error {
	if err := requireBoundOpenFileLinks(file, want); err != nil {
		return err
	}
	return scrubBoundOpenFile(file)
}

func requireBoundOpenFileLinks(file *os.File, want uint64) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return errors.New("checkpoint inode has no stable link count")
	}
	if uint64(stat.Nlink) != want {
		return fmt.Errorf("%w: checkpoint inode has %d links; refusing to scrub external aliases", ErrStale, uint64(stat.Nlink))
	}
	return nil
}

func removeBoundTarget(namespace *boundRestoreNamespace, parent *boundRestoreParent, retirementRoot *os.Root, requested string, bind func(*os.File, bool) error, expected fingerprint, before func() error) (published bool, err error) {
	file, err := namespace.root.OpenFile(namespace.target, os.O_RDWR|os.O_SYNC, 0)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	if err := acquireRestoreLivenessLock(file); err != nil {
		return false, fmt.Errorf("locking removal target: %w", err)
	}
	if bind != nil {
		if err := bind(file, false); err != nil {
			return false, err
		}
	}
	opened, err := file.Stat()
	if err != nil {
		return false, err
	}
	current, err := namespace.fingerprintTarget(-1, opened)
	if err != nil || !sameFingerprint(current, expected) {
		return false, errors.Join(err, fmt.Errorf("%w: removal target changed while it was bound", ErrStale))
	}
	tombstone, err := parent.unusedRestoreName(requested)
	if err != nil {
		return false, err
	}
	tombstoneRel, err := namespace.sibling(tombstone)
	if err != nil {
		return false, err
	}
	if before != nil {
		if err := before(); err != nil {
			return false, err
		}
	}
	linked, err := namespace.root.Lstat(namespace.target)
	if err != nil || !linked.Mode().IsRegular() || !os.SameFile(linked, opened) {
		return false, errors.Join(err, fmt.Errorf("%w: removal target changed at the publication seam", ErrStale))
	}
	result, err := renameBoundRestoreFile(namespace.root, file, nil, namespace.target, tombstoneRel, false)
	if err != nil {
		return result.published, err
	}
	published = result.published
	renamed, err := namespace.root.Lstat(tombstoneRel)
	if err != nil || !renamed.Mode().IsRegular() || !os.SameFile(opened, renamed) {
		return true, errors.Join(err,
			fmt.Errorf("%w: removal tombstone is not the inode selected for publication", ErrStale))
	}
	if err := syncBoundDirectory(parent.root); err != nil {
		return true, err
	}
	if err := retireBoundOpenFileTo(namespace.root, retirementRoot, tombstoneRel, file, false, nil, nil); err != nil {
		return true, err
	}
	return true, nil
}

func boundRootIdentity(root *os.Root) (string, error) {
	dir, err := root.Open(".")
	if err != nil {
		return "", err
	}
	defer dir.Close()
	return boundOpenFileIdentity(dir)
}

func boundOpenFileIdentity(file *os.File) (string, error) {
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return "", errors.New("directory stat has no stable device/inode identity")
	}
	return fmt.Sprintf("unix:%d:%d", uint64(stat.Dev), uint64(stat.Ino)), nil
}
