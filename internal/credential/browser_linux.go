package credential

import (
	"os/exec"
)

func browserCommand(target string) (*exec.Cmd, error) {
	return linuxBrowserCommand(target, "xdg-open",
		"/usr/bin/xdg-open",
		"/usr/local/bin/xdg-open",
		"/bin/xdg-open",
	)
}

func linuxBrowserCommand(target, name string, preferred ...string) (*exec.Cmd, error) {
	helper, err := resolveLinuxHelper(name, preferred...)
	if err != nil {
		return nil, err
	}
	cmd, err := helper.Command(target)
	if err != nil {
		return nil, err
	}
	env, err := linuxHelperEnvironment(false)
	if err != nil {
		return nil, err
	}
	cmd.Env = env
	return cmd, nil
}

func openBrowser(target string) {
	cmd, err := browserCommand(target)
	if err != nil || cmd.Start() != nil {
		return
	}
	go func() { _ = cmd.Wait() }()
}
