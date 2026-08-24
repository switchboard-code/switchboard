//go:build windows

package execution

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// canonicalAmbientHomeDirectory binds an explicit HOME beneath a local drive
// with no-reparse semantics before inspecting it. A lexical UNC check alone is
// insufficient: C:\repo\home may be a directory symlink to \\host\share, and
// EvalSymlinks would contact that host before confinement and approval exist.
// os.Root's Windows open rejects every reparse point in the relative path, so
// the returned spelling comes from an exact local directory handle rather than
// from a second pathname traversal.
func canonicalAmbientHomeDirectory(path string) (result string, err error) {
	clean := filepath.Clean(path)
	if strings.HasPrefix(clean, `\\`) || strings.HasPrefix(clean, `//`) {
		return "", errors.New("ambient HOME must be on a local Windows drive")
	}
	abs, err := filepath.Abs(clean)
	if err != nil {
		return "", fmt.Errorf("making ambient HOME absolute: %w", err)
	}
	volume := filepath.VolumeName(abs)
	if len(volume) != 2 || volume[1] != ':' {
		return "", errors.New("ambient HOME must be on a local Windows drive")
	}
	drivePath := volume + `\`
	driveName, err := windows.UTF16PtrFromString(drivePath)
	if err != nil {
		return "", fmt.Errorf("checking ambient HOME drive: %w", err)
	}
	switch driveType := windows.GetDriveType(driveName); driveType {
	case windows.DRIVE_FIXED, windows.DRIVE_REMOVABLE, windows.DRIVE_RAMDISK:
		// These are local media. Unknown, missing, optical, and remote drives
		// cannot be a writable local HOME authority.
	case windows.DRIVE_REMOTE:
		return "", errors.New("ambient HOME must not use a mapped remote Windows drive")
	default:
		return "", fmt.Errorf("ambient HOME drive is not writable local storage (type %d)", driveType)
	}

	rel, err := filepath.Rel(drivePath, abs)
	if err != nil || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, `..\`) {
		return "", errors.Join(err, errors.New("ambient HOME is outside its local Windows drive"))
	}
	drive, err := os.OpenRoot(drivePath)
	if err != nil {
		return "", fmt.Errorf("binding ambient HOME drive: %w", err)
	}
	defer func() { err = errors.Join(err, drive.Close()) }()
	home, err := drive.OpenRoot(rel)
	if err != nil {
		return "", fmt.Errorf("opening ambient HOME without reparse traversal: %w", err)
	}
	defer func() { err = errors.Join(err, home.Close()) }()
	directory, err := home.Open(".")
	if err != nil {
		return "", fmt.Errorf("opening bound ambient HOME: %w", err)
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	info, err := directory.Stat()
	if err != nil {
		return "", fmt.Errorf("inspecting bound ambient HOME: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("ambient HOME is not a directory")
	}
	result, err = finalWindowsDirectoryPath(windows.Handle(directory.Fd()))
	if err != nil {
		return "", fmt.Errorf("identifying bound ambient HOME: %w", err)
	}
	return result, nil
}

func finalWindowsDirectoryPath(handle windows.Handle) (string, error) {
	const maxPathUTF16 = 1 << 20
	buffer := make([]uint16, 256)
	for {
		n, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
		if err != nil {
			return "", err
		}
		if n < uint32(len(buffer)) {
			path := windows.UTF16ToString(buffer[:n])
			if strings.HasPrefix(path, `\\?\UNC\`) || !strings.HasPrefix(path, `\\?\`) {
				return "", errors.New("bound ambient HOME did not resolve to a local DOS path")
			}
			path = filepath.Clean(strings.TrimPrefix(path, `\\?\`))
			volume := filepath.VolumeName(path)
			if len(volume) != 2 || volume[1] != ':' {
				return "", errors.New("bound ambient HOME did not resolve to a local Windows drive")
			}
			return path, nil
		}
		if n > maxPathUTF16 {
			return "", errors.New("bound ambient HOME path is too long")
		}
		buffer = make([]uint16, int(n)+1)
	}
}
