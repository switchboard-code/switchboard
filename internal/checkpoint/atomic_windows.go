//go:build windows

package checkpoint

// Bound checkpoint replacement flushes the still-open destination handle.
// Other durable ledgers flush their files before publication, and Windows has
// no portable directory fsync equivalent.
func syncDirectory(string) error { return nil }
