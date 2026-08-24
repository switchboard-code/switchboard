package main

import "github.com/atotto/clipboard"

// Windows clipboard access is an in-process Win32 call in this dependency;
// unlike its Unix implementations it cannot be redirected through PATH.
func nativeClipboardWrite(text string) (bool, error) {
	return true, clipboard.WriteAll(text)
}
