//go:build !windows

package main

import "os"

func userShellInvocation(command string) (string, []string) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	return shell, []string{"-c", command}
}
