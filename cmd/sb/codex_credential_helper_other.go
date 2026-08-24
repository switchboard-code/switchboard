//go:build !unix && !windows

package main

import (
	"errors"
	"os"

	"github.com/switchboard-code/switchboard/internal/fileprivacy"
)

// Unsupported kernels have no verified descriptor-rooted, no-follow opener.
// Failing closed keeps a credential path from silently acquiring weaker
// semantics on a new port.
func openCodexAuthFile(string) (*os.File, error) {
	return nil, errors.New("secure Codex credential reads are unavailable on this platform")
}

func codexAuthFileIsAcceptable(file *os.File) (bool, error) {
	return fileprivacy.IsOwnerOnly(file)
}
