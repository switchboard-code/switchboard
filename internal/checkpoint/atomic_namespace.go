package checkpoint

import "os"

// AtomicNamespaceOutcome describes whether an exact open-file namespace
// operation crossed its publication boundary. Published can be true alongside
// an error when a later durability step failed; callers must preserve recovery
// evidence for that outcome rather than treating it as an untouched tree.
type AtomicNamespaceOutcome struct {
	Published      bool
	SourceRetained bool
}

// ValidateAtomicNamespaceRoot proves that the platform can provide the exact
// bound, no-replace namespace operations used by the helpers below. On
// Windows this performs (and then caches by directory identity) the NTFS and
// FileRenameInformation/FileDispositionInfoEx capability probes; unsupported
// filesystems fail closed. Unix implementations need no separate preflight.
func ValidateAtomicNamespaceRoot(root *os.Root) error {
	return ensureRetirementCompatible(root, nil)
}

// MoveOpenFileNoReplace moves the exact open source name to an absent
// destination beneath root without replacing an existing destination. from
// and to are canonical local paths relative to root, not paths relative to a
// separately opened parent directory.
//
// This is a namespace primitive, not a transaction. A caller that must recover
// process interruption is responsible for durably recording the selected
// source identity and both names before calling it, and for reconciling that
// record on startup.
func MoveOpenFileNoReplace(root *os.Root, source *os.File, from, to string) (AtomicNamespaceOutcome, error) {
	result, err := renameBoundRestoreFile(root, source, nil, from, to, false)
	return AtomicNamespaceOutcome{
		Published:      result.published,
		SourceRetained: result.sourceRetained,
	}, err
}

// ExchangeOpenFiles exchanges source and destination beneath root, retaining
// the displaced destination at the source name after success. source and
// displaced bind the exact files selected before publication. The two
// canonical root-relative names must share one parent directory.
//
// This operation is a recoverable three-rename sequence on Windows, not one
// crash-atomic syscall. Callers must durably record the expected target,
// source, and displaced identities before entry and reconcile every
// intermediate state on startup. Code without such a ledger must use a higher
// level transaction such as Recorder.PublishFileCAS instead.
func ExchangeOpenFiles(root *os.Root, source, displaced *os.File, from, to string) (AtomicNamespaceOutcome, error) {
	result, err := renameBoundRestoreFile(root, source, displaced, from, to, true)
	return AtomicNamespaceOutcome{
		Published:      result.published,
		SourceRetained: result.sourceRetained,
	}, err
}

// RollbackOpenFileExchange reverses a prior ExchangeOpenFiles operation while
// preserving the exact open files selected at the rollback seam. A false
// result means the caller must treat the namespace as published or ambiguous.
// The durable-ledger requirement on ExchangeOpenFiles applies equally here.
func RollbackOpenFileExchange(root *os.Root, source, displaced *os.File, sourceName, targetName string) (bool, error) {
	return rollbackBoundReplacement(root, source, displaced, sourceName, targetName)
}
