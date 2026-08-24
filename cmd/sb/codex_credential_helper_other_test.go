//go:build !unix && !windows

package main

import "errors"

func makeCodexAuthNonPrivateForTest(string) error {
	return errors.New("privacy mutation is unavailable")
}
