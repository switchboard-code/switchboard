//go:build !darwin && !linux && !windows

package main

func nativeClipboardWrite(string) (bool, error) { return false, nil }
