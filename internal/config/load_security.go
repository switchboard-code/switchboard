package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/switchboard-code/switchboard/internal/fileprivacy"
)

const maxConfigBytes = int64(1 << 20)

// loadFileBeforeFinalIdentityCheck is a narrow deterministic race-test seam.
// Production leaves it nil. The bytes remain unparsed until the check after
// this hook proves the pathname still names the opened descriptor.
var loadFileBeforeFinalIdentityCheck func()

// loadFileAfterInitialInspection exercises the lstat-to-open boundary. The
// platform opener must stay nonblocking and no-follow even when that boundary
// is raced; production leaves the hook nil.
var loadFileAfterInitialInspection func()

type secureConfigRead struct {
	data           []byte
	file           *os.File
	path           string
	expected       os.FileInfo
	expectedParent os.FileInfo
}

func (r *secureConfigRead) Close() error {
	if r == nil || r.file == nil {
		return nil
	}
	return r.file.Close()
}

// Verify runs after TOML decoding and validation but before the resulting
// Config is returned. The descriptor stays open across parsing, and a second
// bounded read proves both the bytes and the pathname still represent the
// authority that was validated.
func (r *secureConfigRead) Verify() error {
	if loadFileBeforeFinalIdentityCheck != nil {
		loadFileBeforeFinalIdentityCheck()
	}
	if err := verifyConfigIdentity(r.file, r.path, r.expected, r.expectedParent); err != nil {
		return err
	}
	if _, err := r.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewinding configuration %s for final verification: %w", r.path, err)
	}
	current, err := io.ReadAll(io.LimitReader(r.file, maxConfigBytes+1))
	if err != nil {
		return fmt.Errorf("re-reading configuration %s for final verification: %w", r.path, err)
	}
	if !bytes.Equal(current, r.data) {
		return fmt.Errorf("configuration %s changed while it was parsed", r.path)
	}
	if err := verifyConfigIdentity(r.file, r.path, r.expected, r.expectedParent); err != nil {
		return err
	}
	owned, err := fileprivacy.IsCurrentUserOwner(r.file)
	if err != nil || !owned {
		if err != nil {
			return fmt.Errorf("rechecking configuration owner %s: %w", r.path, err)
		}
		return fmt.Errorf("configuration %s changed owner while it was parsed", r.path)
	}
	private, err := fileprivacy.IsOwnerOnly(r.file)
	if err != nil || !private {
		if err != nil {
			return fmt.Errorf("rechecking configuration privacy %s: %w", r.path, err)
		}
		return fmt.Errorf("configuration %s became accessible beyond the current user while it was parsed", r.path)
	}
	return nil
}

func readConfigBytes(path string) (*secureConfigRead, bool, error) {
	before, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("inspecting %s: %w", path, err)
	}
	if !before.Mode().IsRegular() {
		return nil, false, fmt.Errorf("configuration %s is not a regular file", path)
	}
	parentPath := filepath.Dir(path)
	parentBefore, err := os.Lstat(parentPath)
	if err != nil {
		return nil, false, fmt.Errorf("inspecting configuration parent %s: %w", parentPath, err)
	}
	if !parentBefore.IsDir() {
		return nil, false, fmt.Errorf("configuration parent %s is not a real directory", parentPath)
	}
	if loadFileAfterInitialInspection != nil {
		loadFileAfterInitialInspection()
	}

	opened, err := fileprivacy.Open(path)
	if err != nil {
		return nil, false, fmt.Errorf("opening configuration %s safely: %w", path, err)
	}
	keepOpened := false
	defer func() {
		if !keepOpened {
			_ = opened.Close()
		}
	}()
	if err := verifyConfigIdentity(opened, path, before, parentBefore); err != nil {
		return nil, false, err
	}
	owned, err := fileprivacy.IsOwnedByCurrentTokenAuthority(opened)
	if err != nil {
		return nil, false, fmt.Errorf("checking configuration owner %s: %w", path, err)
	}
	if !owned {
		return nil, false, fmt.Errorf("configuration %s is not owned by the current user", path)
	}

	active := opened
	var writable *os.File
	keepWritable := false
	defer func() {
		if !keepWritable && writable != nil {
			_ = writable.Close()
		}
	}()
	private, err := fileprivacy.IsOwnerOnly(opened)
	if err != nil {
		return nil, false, fmt.Errorf("checking configuration privacy %s: %w", path, err)
	}
	if !private {
		// Older releases and ordinary Windows creation could leave this
		// current-user-owned authority file inheriting broader permissions.
		// Bind a writable handle to the same inode and narrow it before any
		// TOML bytes are read or parsed.
		writable, err = fileprivacy.OpenWritable(path)
		if err != nil {
			return nil, false, fmt.Errorf("configuration %s is not owner-private and could not be opened for repair: %w", path, err)
		}
		if err := verifySameConfigFile(opened, writable, path); err != nil {
			return nil, false, err
		}
		if err := verifyConfigIdentity(writable, path, before, parentBefore); err != nil {
			return nil, false, err
		}
		owned, err := fileprivacy.IsOwnedByCurrentTokenAuthority(writable)
		if err != nil || !owned {
			if err != nil {
				return nil, false, fmt.Errorf("checking writable configuration owner %s: %w", path, err)
			}
			return nil, false, fmt.Errorf("configuration %s changed owner before repair", path)
		}
		if err := fileprivacy.Secure(writable); err != nil {
			return nil, false, fmt.Errorf("securing legacy configuration %s: %w", path, err)
		}
		if err := verifyConfigIdentity(writable, path, before, parentBefore); err != nil {
			return nil, false, err
		}
		private, err := fileprivacy.IsOwnerOnly(writable)
		if err != nil || !private {
			if err != nil {
				return nil, false, fmt.Errorf("verifying repaired configuration %s: %w", path, err)
			}
			return nil, false, fmt.Errorf("configuration %s remained accessible beyond the current user after repair", path)
		}
		active = writable
	}

	readStart, err := active.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("inspecting opened configuration %s: %w", path, err)
	}
	if readStart.Size() < 0 || readStart.Size() > maxConfigBytes {
		return nil, false, fmt.Errorf("configuration %s exceeds the %d-byte limit", path, maxConfigBytes)
	}
	data, err := io.ReadAll(io.LimitReader(active, maxConfigBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("reading configuration %s: %w", path, err)
	}
	if int64(len(data)) > maxConfigBytes {
		return nil, false, fmt.Errorf("configuration %s exceeds the %d-byte limit", path, maxConfigBytes)
	}
	readEnd, err := active.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("rechecking opened configuration %s: %w", path, err)
	}
	if readStart.Size() != readEnd.Size() || int64(len(data)) != readEnd.Size() ||
		!readStart.ModTime().Equal(readEnd.ModTime()) {
		return nil, false, fmt.Errorf("configuration %s changed while it was read", path)
	}
	if err := verifyConfigIdentity(active, path, before, parentBefore); err != nil {
		return nil, false, err
	}
	owned, err = fileprivacy.IsCurrentUserOwner(active)
	if err != nil || !owned {
		if err != nil {
			return nil, false, fmt.Errorf("rechecking configuration owner %s: %w", path, err)
		}
		return nil, false, fmt.Errorf("configuration %s changed owner while it was read", path)
	}
	private, err = fileprivacy.IsOwnerOnly(active)
	if err != nil || !private {
		if err != nil {
			return nil, false, fmt.Errorf("rechecking configuration privacy %s: %w", path, err)
		}
		return nil, false, fmt.Errorf("configuration %s became accessible beyond the current user while it was read", path)
	}
	if active != opened {
		closeErr := opened.Close()
		keepOpened = true
		if closeErr != nil {
			return nil, false, fmt.Errorf("closing superseded configuration descriptor %s: %w", path, closeErr)
		}
		keepWritable = true
	} else {
		keepOpened = true
	}
	return &secureConfigRead{
		data: data, file: active, path: path,
		expected: before, expectedParent: parentBefore,
	}, true, nil
}

func verifyConfigIdentity(opened *os.File, path string, expected, expectedParent os.FileInfo) error {
	info, err := opened.Stat()
	if err != nil {
		return fmt.Errorf("inspecting opened configuration %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || !os.SameFile(expected, info) {
		return fmt.Errorf("configuration %s changed while it was opened", path)
	}
	current, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("rechecking configuration path %s: %w", path, err)
	}
	if !current.Mode().IsRegular() || !os.SameFile(info, current) {
		return fmt.Errorf("configuration %s no longer names its opened file", path)
	}
	parentPath := filepath.Dir(path)
	currentParent, err := os.Lstat(parentPath)
	if err != nil {
		return fmt.Errorf("rechecking configuration parent %s: %w", parentPath, err)
	}
	if !currentParent.IsDir() || !os.SameFile(expectedParent, currentParent) {
		return fmt.Errorf("configuration parent %s changed while the file was opened", parentPath)
	}
	return nil
}

func verifySameConfigFile(first, second *os.File, path string) error {
	firstInfo, err := first.Stat()
	if err != nil {
		return err
	}
	secondInfo, err := second.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(firstInfo, secondInfo) {
		return fmt.Errorf("configuration %s changed before its permissions could be repaired", path)
	}
	return nil
}
