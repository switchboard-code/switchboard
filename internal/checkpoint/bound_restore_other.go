//go:build !unix && !windows

package checkpoint

import (
	"errors"
	"os"
	"runtime"
)

func validateBoundRestorePlatform() error {
	return errors.New("secure checkpoint restore is unavailable on " + runtime.GOOS)
}

func syncBoundDirectory(*os.Root) error { return validateBoundRestorePlatform() }

func syncBoundReplacement(*os.File) error { return validateBoundRestorePlatform() }

func ensureRetirementCompatible(*os.Root, *os.Root) error { return validateBoundRestorePlatform() }

func moveNoReplaceBoundNames(*os.Root, string, string) error { return validateBoundRestorePlatform() }

func rollbackBoundReplacement(*os.Root, *os.File, *os.File, string, string) (bool, error) {
	return false, validateBoundRestorePlatform()
}

func renameBoundOpenFile(*os.Root, *os.File, *os.File, string, string, bool) (bool, error) {
	return false, validateBoundRestorePlatform()
}

func retireBoundOpenFile(*os.Root, string, *os.File, bool, func(), func(string)) error {
	return validateBoundRestorePlatform()
}

func retireBoundOpenFileTo(*os.Root, *os.Root, string, *os.File, bool, func(), func(string)) error {
	return validateBoundRestorePlatform()
}

func removeTrustedRetiredFile(*os.Root, string, *os.File, bool) error {
	return validateBoundRestorePlatform()
}

func removeLocalRetiredFile(*os.Root, string, *os.File, bool) error {
	return validateBoundRestorePlatform()
}

func removeBoundTarget(*boundRestoreNamespace, *boundRestoreParent, *os.Root, string, func(*os.File, bool) error, fingerprint, func() error) (bool, error) {
	return false, validateBoundRestorePlatform()
}

func boundRootIdentity(*os.Root) (string, error) { return "", validateBoundRestorePlatform() }

func boundOpenFileIdentity(*os.File) (string, error) { return "", validateBoundRestorePlatform() }
