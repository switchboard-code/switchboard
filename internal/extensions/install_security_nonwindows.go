//go:build !windows

package extensions

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/switchboard-code/switchboard/internal/fileprivacy"
)

func prepareInstallCacheDirectory(path string) error {
	info, err := os.Lstat(path)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("plugin cache root must not be a symbolic link")
		}
		if !info.IsDir() {
			return errors.New("plugin cache root is not a physical directory")
		}
		if info.Mode().Perm()&0o022 != 0 {
			return errors.New("plugin cache root must not be group- or world-writable")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fileprivacy.EnsurePrivateDir(path)
}

func validateInstallCacheDirectory(path string, info os.FileInfo) error {
	return validateInstallCacheDirectoryPath(path, info)
}

func validateInstallCacheDirectoryPath(path string, info os.FileInfo) error {
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New("plugin cache root must not be group- or world-writable")
	}
	root, err := openInstallDirectory(path)
	if err != nil {
		return fmt.Errorf("opening plugin cache root capability: %w", err)
	}
	defer root.Close()
	f, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("opening plugin cache root directory: %w", err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.IsDir() || !os.SameFile(info, opened) {
		return errors.Join(errors.New("plugin cache root changed while it was validated"), err)
	}
	ownerOnly, err := fileprivacy.DirectoryIsOwnerOnly(f)
	if err != nil {
		return err
	}
	if !ownerOnly {
		return errors.New("plugin cache root has an extended ACL or is not mode 0700")
	}
	linked, err := os.Lstat(path)
	if err != nil || linked.Mode()&os.ModeSymlink != 0 || !linked.IsDir() || !os.SameFile(opened, linked) {
		return errors.Join(errors.New("plugin cache root changed while it was validated"), err)
	}
	return nil
}

func securePrivateInstallDirectory(root *os.Root, rel string) error {
	info, err := safeInfo(root, rel)
	if err != nil {
		return err
	}
	if info == nil {
		return os.ErrNotExist
	}
	if !info.IsDir() {
		return errors.New("not a directory")
	}
	f, err := openPrivateInstallObject(root, rel, info, true)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := fileprivacy.SecureDirectory(f); err != nil {
		return err
	}
	anchored, err := safeInfo(root, rel)
	if err != nil || anchored == nil || !os.SameFile(info, anchored) {
		return errors.Join(errors.New("plugin directory changed while it was secured"), err)
	}
	return nil
}

func validatePrivateInstallDirectory(root *os.Root, rel string, info os.FileInfo) error {
	if !info.IsDir() {
		return errors.New("not a directory")
	}
	f, err := openPrivateInstallObject(root, rel, info, true)
	if err != nil {
		return err
	}
	defer f.Close()
	ownerOnly, err := fileprivacy.DirectoryIsOwnerOnly(f)
	if err != nil {
		return err
	}
	if !ownerOnly {
		return fmt.Errorf("mode is %04o or an extended ACL is present; want owner-only 0700", info.Mode().Perm())
	}
	return nil
}

func securePrivateInstallFile(root *os.Root, rel string, file *os.File, executable bool) error {
	want := os.FileMode(0o600)
	if executable {
		want = 0o700
	}
	if err := fileprivacy.SecureMode(file, want); err != nil {
		return err
	}
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	anchored, err := safeInfo(root, rel)
	if err != nil {
		return err
	}
	if anchored == nil || !os.SameFile(opened, anchored) {
		return errors.New("plugin file changed while it was secured")
	}
	return validatePrivateInstallFile(root, rel, opened, executable)
}

func validatePrivateInstallFile(root *os.Root, rel string, info os.FileInfo, executable bool) error {
	if !info.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	want := os.FileMode(0o600)
	if executable {
		want = 0o700
	}
	f, err := openPrivateInstallObject(root, rel, info, false)
	if err != nil {
		return err
	}
	defer f.Close()
	ownerOnly, err := fileprivacy.IsOwnerOnlyMode(f, want)
	if err != nil {
		return err
	}
	if !ownerOnly {
		return fmt.Errorf("mode is %04o or an extended ACL is present; want owner-only %04o", info.Mode().Perm(), want)
	}
	return nil
}

func openPrivateInstallObject(root *os.Root, rel string, expected os.FileInfo, directory bool) (*os.File, error) {
	f, err := root.Open(filepath.FromSlash(rel))
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	if err != nil || opened.IsDir() != directory || !os.SameFile(expected, opened) {
		_ = f.Close()
		return nil, errors.Join(errors.New("plugin cache object changed while it was opened"), err)
	}
	return f, nil
}

func installSourceExecutable(info os.FileInfo) bool {
	return info.Mode().Perm()&0o111 != 0
}
