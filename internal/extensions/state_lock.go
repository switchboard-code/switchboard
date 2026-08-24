package extensions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/switchboard-code/switchboard/internal/fileprivacy"
)

const pluginStateLockTimeout = 2 * time.Second

var pluginStateLockBeforeOpenTestHook func()
var pluginStateLockAfterOpenTestHook func()

type pluginStateLock struct {
	file      *os.File
	directory *pluginStateDirectory
}

func acquirePluginStateLock(ctx context.Context, statePath string) (*pluginStateLock, error) {
	directory, err := openPluginStateDirectory(statePath)
	if err != nil {
		return nil, err
	}
	lock, err := acquirePluginStateLockInDirectory(ctx, directory)
	if err != nil {
		return nil, errors.Join(err, directory.close())
	}
	lock.directory = directory
	return lock, nil
}

func acquirePluginStateLockInDirectory(ctx context.Context, directory *pluginStateDirectory) (*pluginStateLock, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := directory.validateLinked(); err != nil {
		return nil, err
	}
	name := directory.name + ".lock"
	before, err := directory.root.Lstat(name)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspecting plugin state lock: %w", err)
	}
	if err == nil && (before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular()) {
		return nil, errors.New("plugin state lock is not a regular file")
	}
	if pluginStateLockBeforeOpenTestHook != nil {
		pluginStateLockBeforeOpenTestHook()
	}
	file, _, err := fileprivacy.OpenReadWriteOrCreateInRoot(directory.root, name)
	if err != nil {
		return nil, fmt.Errorf("opening plugin state lock: %w", err)
	}
	closeWith := func(err error) (*pluginStateLock, error) {
		_ = file.Close()
		return nil, err
	}
	after, err := file.Stat()
	if err != nil {
		return closeWith(fmt.Errorf("inspecting opened plugin state lock: %w", err))
	}
	current, err := directory.root.Lstat(name)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() || !os.SameFile(current, after) {
		return closeWith(errors.Join(errors.New("plugin state lock changed while it was opened"), err))
	}
	if before != nil && !os.SameFile(before, after) {
		return closeWith(errors.New("plugin state lock changed while it was opened"))
	}
	ownerOnly, err := fileprivacy.IsOwnerOnly(file)
	if err != nil || !ownerOnly {
		return closeWith(errors.Join(errors.New("plugin state lock is not owner-only"), err))
	}
	if err := directory.validateLinked(); err != nil {
		return closeWith(err)
	}
	if pluginStateLockAfterOpenTestHook != nil {
		pluginStateLockAfterOpenTestHook()
	}
	deadline := time.Now().Add(pluginStateLockTimeout)
	for {
		if err := ctx.Err(); err != nil {
			return closeWith(err)
		}
		acquired, lockErr := tryPluginStateFileLock(file)
		if lockErr != nil {
			return closeWith(fmt.Errorf("locking plugin state: %w", lockErr))
		}
		if acquired {
			if err := ctx.Err(); err != nil {
				_ = unlockPluginStateFile(file)
				return closeWith(err)
			}
			locked, statErr := file.Stat()
			linked, linkErr := directory.root.Lstat(name)
			ownerOnly, ownerErr := fileprivacy.IsOwnerOnly(file)
			if statErr != nil || linkErr != nil || ownerErr != nil || !ownerOnly ||
				linked.Mode()&os.ModeSymlink != 0 || !linked.Mode().IsRegular() || !os.SameFile(locked, linked) {
				_ = unlockPluginStateFile(file)
				return closeWith(errors.Join(
					errors.New("plugin state lock changed identity or permissions while it was acquired"),
					statErr, linkErr, ownerErr,
				))
			}
			if err := directory.validateLinked(); err != nil {
				_ = unlockPluginStateFile(file)
				return closeWith(err)
			}
			return &pluginStateLock{file: file}, nil
		}
		if time.Now().After(deadline) {
			return closeWith(errors.New("plugin state is busy; timed out waiting for its lock"))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (lock *pluginStateLock) Close() error {
	if lock == nil {
		return nil
	}
	var err error
	if lock.file != nil {
		err = errors.Join(unlockPluginStateFile(lock.file), lock.file.Close())
		lock.file = nil
	}
	if lock.directory != nil {
		err = errors.Join(err, lock.directory.close())
		lock.directory = nil
	}
	return err
}
