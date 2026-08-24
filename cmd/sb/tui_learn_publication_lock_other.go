//go:build !unix && !windows

package main

import (
	"errors"
	"os"
)

func tryLearnPublicationLock(*os.File) (bool, error) {
	return false, errors.New("cross-process learn publication locking is unsupported on this platform")
}

func unlockLearnPublication(*os.File) error { return nil }
