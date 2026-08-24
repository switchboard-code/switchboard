package main

import (
	"context"
	"os/exec"
)

func newUserShellCommand(command string) *exec.Cmd {
	name, args := userShellInvocation(command)
	return exec.Command(name, args...)
}

func newUserShellCommandContext(ctx context.Context, command string) *exec.Cmd {
	name, args := userShellInvocation(command)
	return exec.CommandContext(ctx, name, args...)
}
