//go:build !unix && !windows

package main

import (
	"errors"
)

func replaceExecutable(string, string) error {
	return errors.New("atomic self-update is unsupported on this platform")
}
