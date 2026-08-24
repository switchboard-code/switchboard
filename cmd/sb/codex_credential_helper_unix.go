//go:build unix

package main

import (
	"errors"
	"os"

	"github.com/switchboard-code/switchboard/internal/fileprivacy"
	"golang.org/x/sys/unix"
)

// openCodexAuthFile walks two literal components from a stable home directory
// descriptor. O_NOFOLLOW rejects a symlink at every component and O_NONBLOCK
// makes a FIFO replacement inspectable rather than a credential-helper hang.
func openCodexAuthFile(home string) (*os.File, error) {
	homeFD, err := unix.Open(home,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(homeFD)

	codexFD, err := unix.Openat(homeFD, ".codex",
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(codexFD)

	authFD, err := unix.Openat(codexFD, "auth.json",
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(authFD), "auth.json")
	if file == nil {
		_ = unix.Close(authFD)
		return nil, errors.New("converting the Codex login descriptor")
	}
	return file, nil
}

// Codex creates auth.json with mode 0600 on Unix. Accept an owner-readable
// variant such as 0400, but never group/world bits, another uid, hard links,
// or a Darwin ACL that grants access beyond those mode bits.
func codexAuthFileIsAcceptable(file *os.File) (bool, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return false, err
	}
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	mode := info.Mode().Perm()
	if stat.Uid != uint32(unix.Geteuid()) || stat.Nlink != 1 || mode&0o077 != 0 || mode&0o400 == 0 {
		return false, nil
	}
	return fileprivacy.IsOwnerOnlyMode(file, mode)
}
