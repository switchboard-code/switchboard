package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/switchboard-code/switchboard/internal/fileprivacy"
	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

const (
	// A caller-provided deadline owns interactive responsiveness. Background
	// startup and compatibility callers still need a finite backstop, but the
	// fallback must accommodate a legitimate queue of durable serialized
	// publications on a loaded or slow filesystem.
	nativeMCPStateLockWait = 30 * time.Second
	nativeMCPStateLockPoll = 10 * time.Millisecond
)

// Test seams for the two cancellation boundaries around a non-blocking lock
// attempt. Production leaves both nil.
var (
	nativeMCPStateLockAfterPoll    func()
	nativeMCPStateLockAfterAcquire func()
)

type nativeMCPStateFileLock struct {
	path          string
	directoryPath string
	stateLeaf     string
	lockLeaf      string
	root          *os.Root
	directory     *os.File
	directoryInfo os.FileInfo
	recoveryPath  string
	recoveryLeaf  string
	recoveryRoot  *os.Root
	recoveryDir   *os.File
	recoveryInfo  os.FileInfo
	file          *os.File
}

// acquireNativeMCPStateFileLock serializes read-modify-write state updates
// across Switchboard processes. The sidecar is permanent: unlinking a held
// lock would let a second process lock a different inode under the same name.
func acquireNativeMCPStateFileLock(ctx context.Context, statePath string) (*nativeMCPStateFileLock, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	statePath = filepath.Clean(statePath)
	if !filepath.IsAbs(statePath) || filepath.Base(statePath) == "." || !filepath.IsLocal(filepath.Base(statePath)) {
		return nil, fmt.Errorf("native MCP state path %q is not an absolute local file", statePath)
	}
	directoryPath := filepath.Dir(statePath)
	if err := fileprivacy.EnsurePrivateDir(directoryPath); err != nil {
		return nil, fmt.Errorf("creating %s: %w", directoryPath, err)
	}
	root, directory, directoryInfo, err := bindNativeMCPStateDirectory(directoryPath)
	if err != nil {
		return nil, err
	}
	closeDirectory := func(cause error) (*nativeMCPStateFileLock, error) {
		return nil, errors.Join(cause, directory.Close(), root.Close())
	}
	recoveryRoot, err := fileprivacy.EnsurePrivateDirInRoot(root, nativeMCPStateRecoveryDirName)
	if err != nil {
		return closeDirectory(fmt.Errorf("binding native MCP state recovery directory: %w", err))
	}
	recoveryDirectory, err := recoveryRoot.Open(".")
	if err != nil {
		return nil, errors.Join(fmt.Errorf("opening native MCP state recovery directory: %w", err),
			recoveryRoot.Close(), directory.Close(), root.Close())
	}
	recoveryInfo, err := recoveryDirectory.Stat()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("identifying native MCP state recovery directory: %w", err),
			recoveryDirectory.Close(), recoveryRoot.Close(), directory.Close(), root.Close())
	}
	closeRecovery := func(cause error) (*nativeMCPStateFileLock, error) {
		return nil, errors.Join(cause, recoveryDirectory.Close(), recoveryRoot.Close(), directory.Close(), root.Close())
	}
	stateLeaf := filepath.Base(statePath)
	lockLeaf := stateLeaf + ".lock"
	path := filepath.Join(directoryPath, lockLeaf)
	file, _, err := fileprivacy.OpenReadWriteOrCreateInRoot(root, lockLeaf)
	if err != nil {
		return closeRecovery(fmt.Errorf("opening native MCP state lock %s: %w", path, err))
	}
	lock := &nativeMCPStateFileLock{
		path: path, directoryPath: directoryPath, stateLeaf: stateLeaf, lockLeaf: lockLeaf,
		root: root, directory: directory, directoryInfo: directoryInfo, file: file,
		recoveryPath: filepath.Join(directoryPath, nativeMCPStateRecoveryDirName), recoveryLeaf: nativeMCPStateRecoveryDirName,
		recoveryRoot: recoveryRoot, recoveryDir: recoveryDirectory, recoveryInfo: recoveryInfo,
	}
	if err := validateNativeMCPStateLockFile(file, path); err != nil {
		return nil, errors.Join(err, lock.closeAuthority())
	}
	if err := lock.verifyAuthority(); err != nil {
		return nil, errors.Join(err, lock.closeAuthority())
	}

	waitCtx, cancel := nativeMCPStateLockContext(ctx)
	defer cancel()
	for {
		if waitErr := waitCtx.Err(); waitErr != nil {
			return nil, errors.Join(
				fmt.Errorf("locking native MCP state %s: %w", statePath, waitErr),
				lock.closeAuthority(),
			)
		}
		acquired, lockErr := tryNativeMCPStateFileLock(file)
		if lockErr != nil {
			return nil, errors.Join(
				fmt.Errorf("locking native MCP state %s: %w", statePath, lockErr),
				lock.closeAuthority(),
			)
		}
		if acquired {
			if nativeMCPStateLockAfterAcquire != nil {
				nativeMCPStateLockAfterAcquire()
			}
			if waitErr := waitCtx.Err(); waitErr != nil {
				return nil, errors.Join(
					fmt.Errorf("locking native MCP state %s: %w", statePath, waitErr),
					unlockNativeMCPStateFileLock(file), lock.closeAuthority(),
				)
			}
			if err := lock.verifyAuthority(); err != nil {
				return nil, errors.Join(err, unlockNativeMCPStateFileLock(file), lock.closeAuthority())
			}
			if waitErr := waitCtx.Err(); waitErr != nil {
				return nil, errors.Join(
					fmt.Errorf("locking native MCP state %s: %w", statePath, waitErr),
					unlockNativeMCPStateFileLock(file), lock.closeAuthority(),
				)
			}
			return lock, nil
		}
		timer := time.NewTimer(nativeMCPStateLockPoll)
		select {
		case <-waitCtx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, errors.Join(
				fmt.Errorf("locking native MCP state %s: %w", statePath, waitCtx.Err()),
				lock.closeAuthority(),
			)
		case <-timer.C:
			if nativeMCPStateLockAfterPoll != nil {
				nativeMCPStateLockAfterPoll()
			}
		}
	}
}

// nativeMCPStateLockContext preserves an explicit caller deadline exactly.
// Only a context with no deadline receives the finite production fallback;
// cancellation still propagates immediately in both cases.
func nativeMCPStateLockContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, hasDeadline := ctx.Deadline(); hasDeadline {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, nativeMCPStateLockWait)
}

func bindNativeMCPStateDirectory(path string) (*os.Root, *os.File, os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("inspecting native MCP state directory %s: %w", path, err)
	}
	if !before.IsDir() || before.Mode()&fs.ModeSymlink != 0 {
		return nil, nil, nil, fmt.Errorf("native MCP state directory %s is not a real directory", path)
	}
	root, err := rootedfs.OpenRoot(path)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("binding native MCP state directory %s: %w", path, err)
	}
	failRoot := func(cause error) (*os.Root, *os.File, os.FileInfo, error) {
		return nil, nil, nil, errors.Join(cause, root.Close())
	}
	directory, err := root.Open(".")
	if err != nil {
		return failRoot(fmt.Errorf("opening native MCP state directory %s: %w", path, err))
	}
	fail := func(cause error) (*os.Root, *os.File, os.FileInfo, error) {
		return nil, nil, nil, errors.Join(cause, directory.Close(), root.Close())
	}
	opened, err := directory.Stat()
	if err != nil {
		return fail(fmt.Errorf("identifying native MCP state directory %s: %w", path, err))
	}
	linked, linkErr := os.Lstat(path)
	if linkErr != nil || !linked.IsDir() || linked.Mode()&fs.ModeSymlink != 0 ||
		!os.SameFile(before, opened) || !os.SameFile(opened, linked) {
		return fail(errors.Join(linkErr,
			fmt.Errorf("native MCP state directory %s changed while it was opened", path)))
	}
	ownerOnly, err := fileprivacy.DirectoryIsOwnerOnly(directory)
	if err != nil {
		return fail(fmt.Errorf("checking native MCP state directory %s permissions: %w", path, err))
	}
	if !ownerOnly {
		return fail(fmt.Errorf("native MCP state directory %s is not owner-only", path))
	}
	return root, directory, opened, nil
}

// verifyAuthority proves that both permanent names still identify the exact
// directory and lock descriptors retained by this process. All state reads and
// publication stay root-relative after this point.
func (l *nativeMCPStateFileLock) verifyAuthority() error {
	if l == nil || l.root == nil || l.directory == nil || l.directoryInfo == nil ||
		l.recoveryRoot == nil || l.recoveryDir == nil || l.recoveryInfo == nil || l.file == nil {
		return errors.New("native MCP state lock authority is closed")
	}
	opened, err := l.directory.Stat()
	if err != nil || !opened.IsDir() || !os.SameFile(l.directoryInfo, opened) {
		return errors.Join(err, errors.New("native MCP state directory descriptor changed identity"))
	}
	rootInfo, err := l.root.Stat(".")
	if err != nil || !rootInfo.IsDir() || !os.SameFile(l.directoryInfo, rootInfo) {
		return errors.Join(err, errors.New("native MCP state root capability changed identity"))
	}
	ownerOnly, err := fileprivacy.DirectoryIsOwnerOnly(l.directory)
	if err != nil || !ownerOnly {
		if err == nil {
			err = errors.New("directory is not owner-only")
		}
		return fmt.Errorf("checking native MCP state directory authority: %w", err)
	}
	linked, err := os.Lstat(l.directoryPath)
	if err != nil || !linked.IsDir() || linked.Mode()&fs.ModeSymlink != 0 || !os.SameFile(l.directoryInfo, linked) {
		return errors.Join(err,
			fmt.Errorf("native MCP state directory %s no longer names the retained directory", l.directoryPath))
	}
	recoveryOpened, err := l.recoveryDir.Stat()
	if err != nil || !recoveryOpened.IsDir() || !os.SameFile(l.recoveryInfo, recoveryOpened) {
		return errors.Join(err, errors.New("native MCP state recovery directory descriptor changed identity"))
	}
	recoveryRootInfo, err := l.recoveryRoot.Stat(".")
	if err != nil || !recoveryRootInfo.IsDir() || !os.SameFile(l.recoveryInfo, recoveryRootInfo) {
		return errors.Join(err, errors.New("native MCP state recovery root capability changed identity"))
	}
	recoveryPrivate, err := fileprivacy.DirectoryIsOwnerOnly(l.recoveryDir)
	if err != nil || !recoveryPrivate {
		if err == nil {
			err = errors.New("recovery directory is not owner-only")
		}
		return fmt.Errorf("checking native MCP state recovery directory authority: %w", err)
	}
	recoveryLinked, err := l.root.Lstat(l.recoveryLeaf)
	if err != nil || !recoveryLinked.IsDir() || recoveryLinked.Mode()&fs.ModeSymlink != 0 ||
		!os.SameFile(l.recoveryInfo, recoveryLinked) {
		return errors.Join(err, errors.New("native MCP state recovery name no longer identifies the retained directory"))
	}
	recoveryPathLinked, err := os.Lstat(l.recoveryPath)
	if err != nil || !recoveryPathLinked.IsDir() || recoveryPathLinked.Mode()&fs.ModeSymlink != 0 ||
		!os.SameFile(l.recoveryInfo, recoveryPathLinked) {
		return errors.Join(err,
			fmt.Errorf("native MCP state recovery path %s no longer names the retained directory", l.recoveryPath))
	}
	lockInfo, err := l.file.Stat()
	if err != nil || !lockInfo.Mode().IsRegular() {
		return errors.Join(err, errors.New("native MCP state lock descriptor is not a regular file"))
	}
	lockLinked, err := l.root.Lstat(l.lockLeaf)
	if err != nil || !lockLinked.Mode().IsRegular() || !os.SameFile(lockInfo, lockLinked) {
		return errors.Join(err, errors.New("native MCP state lock name no longer identifies the retained lock"))
	}
	return validateNativeMCPStateLockFile(l.file, l.path)
}

func validateNativeMCPStateLockFile(file *os.File, path string) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("reading native MCP state lock %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("reading native MCP state lock %s: lock is not a regular file", path)
	}
	ownerOnly, err := fileprivacy.IsOwnerOnly(file)
	if err != nil {
		return fmt.Errorf("reading native MCP state lock %s permissions: %w", path, err)
	}
	if !ownerOnly {
		return fmt.Errorf("reading native MCP state lock %s: permissions are not owner-only", path)
	}
	return nil
}

func (l *nativeMCPStateFileLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := unlockNativeMCPStateFileLock(l.file)
	return errors.Join(unlockErr, l.closeAuthority())
}

func (l *nativeMCPStateFileLock) closeAuthority() error {
	if l == nil {
		return nil
	}
	var result error
	if l.file != nil {
		result = errors.Join(result, l.file.Close())
		l.file = nil
	}
	if l.recoveryDir != nil {
		result = errors.Join(result, l.recoveryDir.Close())
		l.recoveryDir = nil
	}
	if l.recoveryRoot != nil {
		result = errors.Join(result, l.recoveryRoot.Close())
		l.recoveryRoot = nil
	}
	if l.directory != nil {
		result = errors.Join(result, l.directory.Close())
		l.directory = nil
	}
	if l.root != nil {
		result = errors.Join(result, l.root.Close())
		l.root = nil
	}
	return result
}
