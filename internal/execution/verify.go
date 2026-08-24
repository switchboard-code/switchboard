package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

// wrapFunc turns a command into the same command under confinement.
type wrapFunc func(Policy, []string) ([]string, error)

// selfTestCase is one assertion about what confinement does. mustRun cases
// prove the profile has not simply broken everything; the rest prove the
// boundary holds, which is the direction that matters.
type selfTestCase struct {
	name    string
	mustRun bool
	argv    []string

	// mustNotOutput fails the case when it appears in the command's output,
	// whatever the exit status. Exit status alone is not enough: a hidden file
	// can surface as an empty successful read rather than an error, and the
	// question being asked is whether the bytes escaped.
	mustNotOutput string
}

// canaryToken is written into a directory the profile is supposed to hide, so
// the self-test proves the deny works rather than proving a directory happened
// to be empty.
const canaryToken = "switchboard-sandbox-canary-must-not-be-readable"

// selfTestEnv is the throwaway setting a verification runs in.
type selfTestEnv struct {
	Workspace string
	Home      string

	// Canary is a real file inside a directory the profile explicitly hides.
	Canary string

	// UnlistedCanary sits at the top of the home directory under a name that
	// appears on no allow list and no deny list. It is the check that the home
	// directory is closed by default rather than by enumeration: under a
	// deny-list policy this file is readable, and that is the whole failure
	// mode being designed out.
	UnlistedCanary string

	// Escape is a path outside every writable root. Nothing should be able to
	// create it, and the self-test checks the file system as well as the exit
	// code, because a shell can fail for reasons unrelated to the sandbox.
	Escape string
}

func newSelfTestEnv() (*selfTestEnv, func(), error) {
	workspace, err := os.MkdirTemp("", "switchboard-sandbox-check")
	if err != nil {
		return nil, nil, fmt.Errorf("creating a directory to verify the sandbox: %w", err)
	}
	cleanup := func() { os.RemoveAll(workspace) }

	resolved, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("resolving the verification directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(resolved, "readable"), []byte("ok"), 0o600); err != nil {
		cleanup()
		return nil, nil, err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		cleanup()
		return nil, nil, err
	}

	// The canary lives in Switchboard's own state directory, which every
	// profile hides because it holds other workspaces' transcripts.
	canary := filepath.Join(home, ".switchboard", "sandbox-selftest-canary")
	if err := os.MkdirAll(filepath.Dir(canary), 0o700); err != nil {
		cleanup()
		return nil, nil, err
	}
	if err := os.WriteFile(canary, []byte(canaryToken), 0o600); err != nil {
		cleanup()
		return nil, nil, err
	}

	unlisted := filepath.Join(home, ".switchboard-selftest-unlisted-canary")
	if err := os.WriteFile(unlisted, []byte(canaryToken), 0o600); err != nil {
		cleanup()
		return nil, nil, err
	}

	env := &selfTestEnv{
		Workspace:      resolved,
		Home:           home,
		Canary:         canary,
		UnlistedCanary: unlisted,
		Escape:         filepath.Join(home, ".switchboard-sandbox-escape-probe"),
	}
	return env, func() {
		os.Remove(canary)
		os.Remove(unlisted)
		os.Remove(env.Escape)
		cleanup()
	}, nil
}

// runSelfTestCases reports whether every assertion held.
//
// A single failure refuses the whole profile rather than trusting the part that
// still works. A rule that stopped matching after an OS update looks exactly
// like a rule that works until something is asked of it, and a partial boundary
// presented as a whole one is what design principle 4 exists to prevent.
func runSelfTestCases(policy Policy, wrap wrapFunc, cases []selfTestCase) (bool, string) {
	var failures []string
	for _, c := range cases {
		ran, output, err := probeUnderWrap(policy, wrap, c.argv)
		if err != nil {
			return false, fmt.Sprintf("could not run the %q check: %v", c.name, err)
		}
		switch {
		case c.mustRun && !ran:
			failures = append(failures, "blocked something it must permit: "+c.name)
		case !c.mustRun && ran:
			failures = append(failures, "permitted something it must deny: "+c.name)
		}
		if c.mustNotOutput != "" && strings.Contains(output, c.mustNotOutput) {
			failures = append(failures, "leaked content it must hide: "+c.name)
		}
	}
	if len(failures) > 0 {
		return false, "sandbox self-test failed: " + strings.Join(failures, "; ")
	}
	return true, fmt.Sprintf("confinement verified against %d checks on this host", len(cases))
}

// probeUnderWrap reports whether the command succeeded under confinement. A
// non-zero exit counts as refused, which is what a denied syscall produces
// through the base-OS binaries these checks use.
func probeUnderWrap(policy Policy, wrap wrapFunc, argv []string) (bool, string, error) {
	wrapped, err := wrap(policy, argv)
	if err != nil {
		return false, "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, wrapped[0], wrapped[1:]...)
	cmd.Env = childEnv()

	out, err := cmd.CombinedOutput()
	if err == nil {
		return true, string(out), nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return false, string(out), nil
	}
	return false, string(out), err
}

// checkRecord caches a verification verdict. It is keyed by the profile and the
// host, because editing the profile or updating the OS can change what the
// kernel enforces and neither should inherit an old pass.
type checkRecord struct {
	ProfileKey string    `json:"profile_key"`
	HostKey    string    `json:"host_key"`
	Verified   bool      `json:"verified"`
	Detail     string    `json:"detail"`
	CheckedAt  time.Time `json:"checked_at"`
}

const maxCheckCacheBytes = 64 << 10

func cachedVerification(profileKey, hostKey string, run func() (bool, string)) (bool, string) {
	path, err := checkCachePath()
	if err == nil {
		if rec, readErr := readCheck(path); readErr == nil &&
			rec.ProfileKey == profileKey && rec.HostKey == hostKey {
			return rec.Verified, rec.Detail
		}
	}

	verified, detail := run()
	if path != "" {
		writeCheck(path, checkRecord{
			ProfileKey: profileKey,
			HostKey:    hostKey,
			Verified:   verified,
			Detail:     detail,
			CheckedAt:  time.Now().UTC(),
		})
	}
	return verified, detail
}

func checkCachePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".switchboard")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "sandbox-check.json"), nil
}

func readCheck(path string) (checkRecord, error) {
	var rec checkRecord
	data, err := rootedfs.ReadFile(filepath.Dir(path), filepath.Base(path), maxCheckCacheBytes)
	if err != nil {
		return rec, err
	}
	return rec, json.Unmarshal(data, &rec)
}

func writeCheck(path string, rec checkRecord) {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(path, data, 0o600)
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// commandOutput runs a small informational command and returns its trimmed
// output, or "unknown". It is used for host identity, never for policy.
func commandOutput(name string, args ...string) string {
	cmd := exec.Command(name, args...)
	cmd.Env = childEnv()
	out, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// shellQuote wraps a path for /bin/sh inside a self-test. The paths are ones
// this process just created, so this is hygiene rather than a boundary.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
