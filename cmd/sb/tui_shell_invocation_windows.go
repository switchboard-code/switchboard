//go:build windows

package main

import "os"

func userShellInvocation(command string) (string, []string) {
	shell := os.Getenv("COMSPEC")
	if shell == "" {
		shell = "cmd.exe"
	}
	return shell, []string{"/d", "/s", "/c", command}
}
