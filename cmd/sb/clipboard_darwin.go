package main

import (
	"os/exec"
	"strings"

	"github.com/switchboard-code/switchboard/internal/execution"
)

const pbcopyToolPath = "/usr/bin/pbcopy"

// systemClipboardCommand is absolute so a repository cannot receive selected
// transcript text by shadowing pbcopy on PATH. The fixed-child environment
// withholds credentials and loader injection as a second egress boundary.
func systemClipboardCommand() *exec.Cmd {
	cmd := exec.Command(pbcopyToolPath)
	cmd.Env = execution.ScrubbedChildEnv()
	return cmd
}

func nativeClipboardWrite(text string) (bool, error) {
	cmd := systemClipboardCommand()
	cmd.Stdin = strings.NewReader(text)
	return true, cmd.Run()
}
