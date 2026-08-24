//go:build windows

package main

import "os"

func openWorkspaceBoundedRead(root *os.Root, path string) (*os.File, error) { return root.Open(path) }
