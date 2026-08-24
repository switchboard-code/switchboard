//go:build !unix && !windows

package session

import (
	"errors"
	"fmt"
	"os"
)

func preparePrivateSessionStore(root string) error {
	return ensurePrivateSessionDirectory(root)
}

func ensurePrivateSessionDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("session directory %s is not owner-only", path)
	}
	return nil
}

func createPrivateSessionFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if err := securePrivateSessionFile(f); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

func createPrivateSessionFileInRoot(root *os.Root, name string) (*os.File, error) {
	f, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if err := securePrivateSessionFile(f); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

func securePrivateSessionFile(f *os.File) error {
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	ownerOnly, err := privateSessionFileIsOwnerOnly(f)
	if err != nil {
		return err
	}
	if !ownerOnly {
		return errors.New("session file is not mode 0600 after securing it")
	}
	return nil
}

func privateSessionFileIsOwnerOnly(f *os.File) (bool, error) {
	info, err := f.Stat()
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm() == 0o600, err
}

func createPrivateSessionTempDir(parent, pattern string) (string, error) {
	path, err := os.MkdirTemp(parent, pattern)
	if err != nil {
		return "", err
	}
	if err := ensurePrivateSessionDirectory(path); err != nil {
		return "", errors.Join(err, os.Remove(path))
	}
	return path, nil
}
