//go:build !unix && !windows

package main

import (
	"errors"
	"os"
	"runtime"
)

func openHistoryDataDescriptor(string, bool, bool) (*os.File, error) {
	return nil, errors.New("secure prompt history is unsupported on " + runtime.GOOS)
}

func openHistoryLockDescriptor(string, bool) (*os.File, error) {
	return nil, errors.New("secure prompt history is unsupported on " + runtime.GOOS)
}

func createHistoryBoundPrivateFile(*os.Root, string) (*os.File, error) {
	return nil, errors.New("secure prompt history is unsupported on " + runtime.GOOS)
}

func tryHistoryFileLock(*os.File) (bool, error) { return false, errors.New("unsupported") }
func unlockHistoryFileLock(*os.File) error      { return nil }
func historyFileLinkCount(*os.File) (uint64, error) {
	return 0, errors.New("unsupported")
}
func secureHistoryFile(*os.File) error {
	return errors.New("secure prompt history is unsupported on " + runtime.GOOS)
}
func historyFileIsOwnerOnly(*os.File) (bool, error) {
	return false, errors.New("secure prompt history is unsupported on " + runtime.GOOS)
}
func historyFileIdentity(*os.File) (string, error) {
	return "", errors.New("secure prompt history is unsupported on " + runtime.GOOS)
}
func syncHistoryDirectory(string) error { return nil }
func syncHistoryBoundDirectory(*os.Root) error {
	return errors.New("secure prompt history is unsupported on " + runtime.GOOS)
}
func scrubHistoryRetiredFile(*os.File) error {
	return errors.New("secure prompt history is unsupported on " + runtime.GOOS)
}
