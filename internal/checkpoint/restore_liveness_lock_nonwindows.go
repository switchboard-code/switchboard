//go:build !windows

package checkpoint

import "os"

// Restore liveness is advisory on non-Windows platforms, so the journal lock
// primitive can also guard an in-flight temporary or removal target without
// preventing this process from fingerprinting it through another descriptor.
func acquireRestoreLivenessLock(f *os.File) error {
	return acquireJournalLock(f)
}

// Unix publication is one atomic namespace operation. The replacement target
// does not need Windows' extra exclusion around a multi-step exchange.
func acquireReplacementTargetLivenessLock(*os.File) error { return nil }
