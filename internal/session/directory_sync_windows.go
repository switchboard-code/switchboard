//go:build windows

package session

import "os"

// Windows does not expose directory handles through os.Open in a form that
// File.Sync can flush; calling it can return ERROR_INVALID_FUNCTION and would
// make every otherwise durable session creation/publication fail. Session and
// marker files themselves are flushed before this seam, so this matches the
// platform contract used by checkpoint's atomic publication path.
func syncSessionDirectory(string) error { return nil }

func syncOpenedSessionDirectory(*os.File) error { return nil }
