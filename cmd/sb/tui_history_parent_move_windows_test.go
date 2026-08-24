//go:build windows

package main

import (
	"errors"

	"golang.org/x/sys/windows"
)

func historyParentMoveBlockedByRetainedHandle(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED)
}

func historySubstitutionRefusesBeforePublication() bool { return true }
