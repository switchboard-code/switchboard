//go:build !unix && !windows

package main

import (
	"fmt"
	"os"
	"runtime"
)

func openEditorPromptRead(*os.Root, string) (*os.File, error) {
	return nil, fmt.Errorf("secure external-editor reads are unsupported on %s", runtime.GOOS)
}
