package credential

import (
	"os/exec"

	"github.com/switchboard-code/switchboard/internal/execution"
)

const browserOpenPath = "/usr/bin/open"

func browserCommand(target string) (*exec.Cmd, error) {
	cmd := exec.Command(browserOpenPath, target)
	cmd.Env = execution.ScrubbedChildEnv()
	return cmd, nil
}

func openBrowser(target string) {
	cmd, err := browserCommand(target)
	if err != nil || cmd.Start() != nil {
		return
	}
	go func() { _ = cmd.Wait() }()
}
