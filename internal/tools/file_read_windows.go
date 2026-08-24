//go:build windows

package tools

import "os"

func openWorkspaceRead(root *os.Root, relative string) (*os.File, error) { return root.Open(relative) }
