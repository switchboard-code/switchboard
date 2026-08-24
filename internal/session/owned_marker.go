package session

import (
	"bytes"
	"errors"
	"io"
	"os"
)

// inspectOwnedPublicationMarker binds marker contents to the identity of the
// regular file that supplied them. Callers carry that FileInfo into
// removePathIfSame, so a later pathname swap is refused rather than deleted.
// When allowComplete is false, a valid publication commit is never considered
// cleanup-owned.
func inspectOwnedPublicationMarker(logPath string, start SessionStart, allowComplete bool) (os.FileInfo, bool, bool, error) {
	path := publicationMarkerPath(logPath)
	f, err := openSessionLog(path, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, true, nil
	}
	if err != nil {
		return nil, false, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() > maxPublicationMarker {
		return nil, true, false, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxPublicationMarker+1))
	if err != nil || len(data) > maxPublicationMarker {
		return nil, true, false, err
	}
	if err := verifyCurrentSessionLogPath(f, path); err != nil {
		return nil, true, false, err
	}
	expected := []byte(publicationMarker(start.ID, start.PublicationID))
	owned := bytes.HasPrefix(expected, data) && (allowComplete || len(data) < len(expected))
	return info, true, owned, nil
}
