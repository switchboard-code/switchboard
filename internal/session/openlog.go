package session

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// openSessionLog opens one existing log without following a stable symlink and
// proves that the descriptor names the same regular file observed at the path.
// The platform opener also makes a FIFO replacement non-blocking on Unix, so a
// corrupt store entry cannot hang inventory before this type check runs.
func openSessionLog(path string, writable bool) (*os.File, error) {
	parent := filepath.Dir(path)
	parentBefore, err := os.Lstat(parent)
	if err != nil {
		return nil, err
	}
	if !parentBefore.IsDir() {
		return nil, fmt.Errorf("session log parent %s is not a directory", parent)
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("session log %s is not a regular file", path)
	}

	f, err := openSessionLogDescriptor(path, writable)
	if err != nil {
		return nil, err
	}
	if err := verifySessionLogPath(f, path, before, parentBefore); err != nil {
		_ = f.Close()
		return nil, err
	}
	if writable {
		if err := securePrivateSessionFile(f); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("securing session log %s: %w", path, err)
		}
		if err := verifySessionLogPath(f, path, before, parentBefore); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	return f, nil
}

// verifySessionLogPath is repeated after the writable append lock is acquired.
// Candidate selection may race a pathname replacement, but no migration,
// repair, or append is allowed until the locked descriptor still owns its path.
func verifySessionLogPath(f *os.File, path string, expected, expectedParent os.FileInfo) error {
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(expected, opened) {
		return fmt.Errorf("session log %s changed while it was opened", path)
	}
	if err := verifySessionLogSingleLink(f, path); err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("checking session log %s after open: %w", path, err)
	}
	if !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		return fmt.Errorf("session log %s no longer names its opened regular file", path)
	}
	currentParent, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("checking parent for session log %s after open: %w", path, err)
	}
	if !currentParent.IsDir() || !os.SameFile(expectedParent, currentParent) {
		return fmt.Errorf("session log parent %s changed while the log was opened", filepath.Dir(path))
	}
	return nil
}

// verifyCurrentSessionLogPath checks only the descriptor and current path. It
// is used after a lock is acquired, when the pre-open FileInfo is no longer the
// interesting identity boundary.
func verifyCurrentSessionLogPath(f *os.File, path string) error {
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("checking locked session log %s: %w", path, err)
	}
	if !opened.Mode().IsRegular() || !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		return fmt.Errorf("locked session log %s no longer owns its path", path)
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("checking parent for locked session log %s: %w", path, err)
	}
	if !parent.IsDir() {
		return fmt.Errorf("locked session log parent %s is not a directory", filepath.Dir(path))
	}
	if err := verifySessionLogSingleLink(f, path); err != nil {
		return err
	}
	return nil
}

func verifySessionLogSingleLink(f *os.File, path string) error {
	links, err := sessionLogLinkCount(f)
	if err != nil {
		return fmt.Errorf("checking link count for session log %s: %w", path, err)
	}
	if links != 1 {
		return fmt.Errorf("session log %s has %d hard links; refusing an aliased log", path, links)
	}
	return nil
}

// openPublishedLog performs the publication gate and payload read on one
// descriptor. Returning to the pathname after the gate would let a replacement
// file feed hidden payloads into export, accounting, blame, or statistics.
func openPublishedLog(path string) (*os.File, error) {
	f, err := openSessionLog(path, false)
	if err != nil {
		return nil, err
	}
	start, err := readFirstSessionStart(f, path)
	if err == nil {
		err = validateSessionStartPathIdentity(path, start)
	}
	if err == nil {
		err = validatePublishedMarker(path, start)
	}
	if err == nil {
		err = verifyCurrentSessionLogPath(f, path)
	}
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}
