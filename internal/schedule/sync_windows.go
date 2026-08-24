//go:build windows

package schedule

import "os"

// Windows does not expose directory handles through os.Open in a form that
// File.Sync can flush. Ledger files themselves are flushed before migration
// publication, matching the platform durability boundary used by sessions and
// checkpoint publication.
func syncScheduleDirectory(string) error { return nil }

func syncScheduleRoot(*os.Root) error { return nil }
