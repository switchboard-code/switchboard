//go:build windows

package workspace

import (
	"os"
)

// Windows' os.Root uses handle-relative NtCreateFile calls with
// O_NOFOLLOW_ANY and resolves only symlinks proven to remain beneath the root
// handle. Reserved device names are rejected by the standard library.
func openWorkspaceReadFile(rootPath string, identity os.FileInfo, relative string) (*os.File, error) {
	rooted, err := openVerifiedWorkspaceRoot(rootPath, identity)
	if err != nil {
		return nil, err
	}
	file, openErr := rooted.Open(relative)
	closeErr := rooted.Close()
	if openErr != nil {
		return nil, openErr
	}
	if closeErr != nil {
		_ = file.Close()
		return nil, closeErr
	}
	return file, nil
}
