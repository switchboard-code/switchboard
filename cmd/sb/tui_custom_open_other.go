//go:build !unix && !windows

package main

import (
	"errors"
	"os"
)

func openCustomCommandFile(_ *os.Root, _ string) (*os.File, error) {
	return nil, errors.New("secure custom command reads are unsupported on this platform")
}
