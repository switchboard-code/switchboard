package execution

import (
	"os"
	"path/filepath"
)

type egressProbeCandidate struct {
	paths []string
	args  []string
}

var systemEgressProbeCandidates = []egressProbeCandidate{
	{
		paths: []string{"/usr/bin/curl", "/bin/curl"},
		args:  []string{"-s", "-m", "3", "http://1.1.1.1", "-o", "/dev/null"},
	},
	{
		paths: []string{"/usr/bin/wget", "/bin/wget"},
		args:  []string{"-q", "-T", "3", "-O", "/dev/null", "http://1.1.1.1"},
	},
	{
		paths: []string{"/usr/bin/nc", "/bin/nc"},
		args:  []string{"-z", "-w", "3", "1.1.1.1", "80"},
	},
}

// linuxSelfTest proves bubblewrap confines a command on this machine, right
// now. Unprivileged user namespaces are a kernel configuration that some
// distributions and hardening profiles disable, so bubblewrap being installed
// says nothing about whether it can actually build a namespace here.
func linuxSelfTest(wrap wrapFunc) (bool, string) {
	env, cleanup, err := newSelfTestEnv()
	if err != nil {
		return false, err.Error()
	}
	defer cleanup()

	policy := Policy{Workspace: env.Workspace, Network: NetworkLoopback}

	cases := []selfTestCase{
		{
			name:    "write inside the workspace",
			mustRun: true,
			argv:    []string{"/bin/sh", "-c", "echo ok > " + shellQuote(filepath.Join(env.Workspace, "probe"))},
		},
		{
			name:    "read inside the workspace",
			mustRun: true,
			argv:    []string{"/bin/cat", filepath.Join(env.Workspace, "readable")},
		},
		{
			name: "write into the home directory",
			argv: []string{"/bin/sh", "-c", "echo escaped > " + shellQuote(env.Escape)},
		},
		{
			name: "write into /etc",
			argv: []string{"/bin/sh", "-c", "echo escaped > /etc/switchboard-sandbox-escape-probe"},
		},
		{
			// Reads the file directly rather than listing the directory, so an
			// empty listing cannot be mistaken for a working deny.
			name:          "read Switchboard's own session state",
			argv:          []string{"/bin/cat", env.Canary},
			mustNotOutput: canaryToken,
		},
		{
			// Nothing names this file. Under a deny-list policy it is readable,
			// which is exactly the posture this replaces.
			name:          "read an unlisted file in the home directory",
			argv:          []string{"/bin/cat", env.UnlistedCanary},
			mustNotOutput: canaryToken,
		},
	}

	// Egress needs a real client. /dev/tcp is a bash builtin and /bin/sh is
	// dash on Debian and Ubuntu, so a shell-based probe fails identically
	// whether or not the network is reachable, which would make this assertion
	// pass while measuring nothing.
	if probe := egressProbeArgv(); probe != nil {
		cases = append(cases, selfTestCase{
			name: "reach the network off this machine",
			argv: probe,
		})
	}

	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if _, err := os.Stat(sock); err == nil {
			if _, lookErr := os.Stat("/usr/bin/ssh-add"); lookErr == nil {
				cases = append(cases, selfTestCase{
					name: "use the ssh agent",
					argv: []string{"/usr/bin/ssh-add", "-l"},
				})
			}
		}
	}

	ok, detail := runSelfTestCases(policy, wrap, cases)
	if ok {
		if _, err := os.Stat(env.Escape); err == nil {
			return false, "sandbox self-test failed: a denied write still reached the home directory"
		}
		if egressProbeArgv() == nil {
			// Say what was not measured. The network namespace is still created
			// by the kernel and TestNetworkNamespaceFlags covers the policy
			// mapping, but this host produced no live evidence of it.
			detail += " (no trusted system curl, wget, or nc available, so egress was not exercised)"
		}
	}
	return ok, detail
}

// egressProbeArgv picks a client that can attempt a connection to a literal
// address with no name resolution, so a refusal means the network was blocked
// rather than DNS being unavailable.
func egressProbeArgv() []string {
	return selectEgressProbe(systemEgressProbeCandidates)
}

func selectEgressProbe(candidates []egressProbeCandidate) []string {
	for _, candidate := range candidates {
		for _, path := range candidate.paths {
			// The sandbox self-test runs before the profile has earned execution
			// trust. Its probe must therefore be a fixed system executable, never a
			// checkout-provided PATH shadow.
			if !filepath.IsAbs(path) {
				continue
			}
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
				continue
			}
			return append([]string{path}, candidate.args...)
		}
	}
	return nil
}
