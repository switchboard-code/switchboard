//go:build windows

package checkpoint

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

func validateBoundRestorePlatform() error { return nil }

// Windows has no portable directory fsync. The replacement file remains open
// across its handle-relative rename, so flushing that handle after publication
// supplies the write-through barrier without resolving the parent pathname.
func syncBoundDirectory(*os.Root) error { return nil }

func syncBoundReplacement(file *os.File) error { return file.Sync() }

func rollbackBoundReplacement(root *os.Root, source, displaced *os.File, sourceName, targetName string) (bool, error) {
	return renameBoundOpenFile(root, source, displaced, targetName, sourceName, true)
}

// Windows does not expose a portable directory-fsync operation. Preserve a
// writable, write-through handle to the target, rename it to a private
// tombstone relative to the already-bound parent, and flush that same file
// after the rename. Microsoft documents FlushFileBuffers on the file as the
// barrier for cached file metadata. Losing the later tombstone unlink can
// leave garbage, but cannot resurrect the original target name.
func removeBoundTarget(namespace *boundRestoreNamespace, parent *boundRestoreParent, retirementRoot *os.Root, requested string, bind func(*os.File, bool) error, expected fingerprint, before func() error) (published bool, err error) {
	file, err := namespace.root.OpenFile(namespace.target, os.O_RDWR|os.O_SYNC, 0)
	if err != nil {
		return false, fmt.Errorf("opening removal target with write-through access: %w", err)
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
	if err != nil || !linked.Mode().IsRegular() || !os.SameFile(opened, linked) {
		return false, errors.Join(err, fmt.Errorf("%w: removal target changed at the publication seam", ErrStale))
	}
	result, err := renameBoundRestoreFile(namespace.root, file, nil, namespace.target, tombstoneRel, false)
	published = result.published
	if err != nil {
		return published, err
	}
	renamed, err := namespace.root.Lstat(tombstoneRel)
	if err != nil || !renamed.Mode().IsRegular() || !os.SameFile(opened, renamed) {
		return true, errors.Join(err,
			fmt.Errorf("%w: removal tombstone is not the inode selected for publication", ErrStale))
	}
	if err := retireBoundOpenFileTo(namespace.root, retirementRoot, tombstoneRel, file, false, nil, nil); err != nil {
		return true, fmt.Errorf("retiring durable undo tombstone: %w", err)
	}
	return true, nil
}

func boundRootIdentity(root *os.Root) (string, error) {
	dir, err := root.Open(".")
	if err != nil {
		return "", err
	}
	defer dir.Close()
	if err := requireStableWindowsFilesystem(dir); err != nil {
		return "", err
	}
	return boundOpenFileIdentity(dir)
}

func boundOpenFileIdentity(file *os.File) (string, error) {
	info, err := stableWindowsFileID(windows.Handle(file.Fd()))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("windows:%016x:%x", info.VolumeSerialNumber, info.FileID), nil
}

func requireStableWindowsFilesystem(file *os.File) error {
	name := make([]uint16, 32)
	if err := windows.GetVolumeInformationByHandle(
		windows.Handle(file.Fd()), nil, 0, nil, nil, nil, &name[0], uint32(len(name)),
	); err != nil {
		return fmt.Errorf("identifying checkpoint filesystem: %w", err)
	}
	filesystem := strings.ToUpper(windows.UTF16ToString(name))
	if filesystem != "NTFS" {
		return fmt.Errorf("secure checkpoint restore currently requires NTFS; %s does not provide the identity guarantees this build verifies", filesystem)
	}
	return nil
}
