package execution

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func workspaceFor(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func confined(t *testing.T) *Confinement {
	t.Helper()
	if _, err := os.Stat(sandboxExec); err != nil {
		t.Skip("sandbox-exec is not present")
	}
	return &Confinement{mechanism: MechanismSeatbelt, wrap: wrapSeatbelt}
}

func runConfined(t *testing.T, ws string, network NetworkAccess, argv []string, shell bool) Result {
	t.Helper()
	res, err := Run(context.Background(), Command{
		Argv:    argv,
		Shell:   shell,
		Dir:     ws,
		Timeout: 30 * time.Second,
		Confine: confined(t),
		Policy:  Policy{Workspace: ws, Network: network},
	})
	if err != nil {
		t.Fatalf("running %v: %v", argv, err)
	}
	return res
}

// The self-test is what earns the right to run commands without asking, so it
// has to actually pass on a machine where the sandbox works.
func TestSelfTestPassesOnThisHost(t *testing.T) {
	if _, err := os.Stat(sandboxExec); err != nil {
		t.Skip("sandbox-exec is not present")
	}
	ok, detail := darwinSelfTest()
	if !ok {
		t.Fatalf("self-test failed on this host: %s", detail)
	}
	if !strings.Contains(detail, "verified") {
		t.Errorf("detail = %q, want it to say what was verified", detail)
	}
}

func TestConfinedWritesStayInTheWorkspace(t *testing.T) {
	ws := workspaceFor(t)

	res := runConfined(t, ws, NetworkLoopback,
		[]string{"echo confined > " + filepath.Join(ws, "inside.txt")}, true)
	if res.ExitCode != 0 {
		t.Fatalf("a write inside the workspace must succeed: %s", res.Output)
	}
	if data, err := os.ReadFile(filepath.Join(ws, "inside.txt")); err != nil || !strings.Contains(string(data), "confined") {
		t.Errorf("file = %q, err = %v", data, err)
	}

	// Not t.TempDir(): that lives under $TMPDIR, which the profile grants on
	// purpose because build tools are unusable without it. The boundary being
	// tested is the home directory and the rest of the filesystem.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, escape := range []string{
		filepath.Join(home, ".switchboard-write-escape-probe"),
		"/private/tmp/switchboard-write-escape-probe",
	} {
		os.Remove(escape)
		res = runConfined(t, ws, NetworkLoopback, []string{"echo out > " + escape}, true)
		if res.ExitCode == 0 {
			t.Errorf("a write to %s succeeded", escape)
		}
		if _, err := os.Stat(escape); err == nil {
			os.Remove(escape)
			t.Errorf("the write to %s actually landed", escape)
		}
	}
}

func TestConfinedCommandCannotReadCredentials(t *testing.T) {
	ws := workspaceFor(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}

	// securityd answers keychain queries over mach IPC, so a profile that only
	// denies the keychain files still leaks unless mach-lookup is denied too.
	// This is the check that caught it.
	if res := runConfined(t, ws, NetworkLoopback, []string{"/usr/bin/security", "list-keychains"}, false); res.ExitCode == 0 {
		t.Errorf("the keychain API answered inside the sandbox: %s", res.Output)
	}

	if res := runConfined(t, ws, NetworkLoopback, []string{"/bin/ls", filepath.Join(home, ".switchboard")}, false); res.ExitCode == 0 {
		t.Error("a confined command listed Switchboard's session logs, which hold other projects' prompts and code")
	}

	if os.Getenv("SSH_AUTH_SOCK") != "" {
		if res := runConfined(t, ws, NetworkLoopback, []string{"/usr/bin/ssh-add", "-l"}, false); res.ExitCode == 0 {
			t.Error("a confined command reached the ssh agent")
		}
	}
}

// Egress must be refused by the rule, not as a side effect of DNS failing, so
// this dials a raw address with no name lookup involved.
func TestNetworkPolicy(t *testing.T) {
	ws := workspaceFor(t)

	denied := runConfined(t, ws, NetworkLoopback,
		[]string{"/usr/bin/curl", "-s", "-m", "5", "http://1.1.1.1", "-o", "/dev/null"}, false)
	if denied.ExitCode == 0 {
		t.Error("loopback policy allowed egress off the machine")
	}

	granted := runConfined(t, ws, NetworkFull,
		[]string{"/usr/bin/curl", "-s", "-m", "10", "http://1.1.1.1", "-o", "/dev/null"}, false)
	if granted.ExitCode != 0 {
		t.Skipf("network policy granted but the host has no egress (exit %d); nothing to compare against", granted.ExitCode)
	}
}

func TestSeatbeltSeparatesPolicyOptionsFromTargetArgv(t *testing.T) {
	ws := workspaceFor(t)
	target := []string{"-D", "WORKSPACE=/", "/bin/sh", "-c", "echo escaped"}
	wrapped, err := wrapSeatbelt(Policy{Workspace: ws, Network: NetworkLoopback}, target)
	if err != nil {
		t.Fatal(err)
	}
	for i := range wrapped {
		if wrapped[i] != "--" {
			continue
		}
		if got := wrapped[i+1:]; !slices.Equal(got, target) {
			t.Fatalf("target after separator = %#v, want %#v", got, target)
		}
		return
	}
	t.Fatalf("sandbox-exec argv has no end-of-options separator: %#v", wrapped)
}

func TestSeatbeltReopensOnlyBoundToolchainRoot(t *testing.T) {
	ws := workspaceFor(t)
	root := filepath.Join(ws, "outside-home-fixture")
	profile, err := profileText(Policy{
		Workspace:     ws,
		Network:       NetworkLoopback,
		readOnlyRoots: []string{root},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "(allow file-read* (subpath " + seatbeltString(root) + "))"
	if !strings.Contains(profile, want) {
		t.Fatalf("profile omitted bound toolchain root %q", root)
	}
}

func TestSeatbeltProtectsAccountHomeWhenHOMEIsWorkspace(t *testing.T) {
	account, err := accountHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	workspace := workspaceFor(t)
	t.Setenv("HOME", workspace)

	profile, err := profileText(Policy{Workspace: workspace, Network: NetworkLoopback})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"(deny file-read* (subpath " + seatbeltString(account) + "))",
		"(deny file-read* (subpath " + seatbeltString(workspace) + "))",
		"(allow file-read* (subpath " + seatbeltString(workspace) + "))",
	} {
		if !strings.Contains(profile, want) {
			t.Fatalf("profile omitted %q:\n%s", want, profile)
		}
	}
	params, err := profileParams(Policy{Workspace: workspace, Network: NetworkLoopback})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := params["DENY_SWITCHBOARD"], filepath.Join(account, ".switchboard"); got != want {
		t.Fatalf("static credential deny followed HOME: got %q, want %q", got, want)
	}
	if got := params["CACHE_XDG"]; got != workspace {
		t.Fatalf("untrusted ambient cache widened the profile: got %q, want inert workspace root %q", got, workspace)
	}
}

func TestForgedHOMECannotExposeAccountHome(t *testing.T) {
	account, err := accountHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	canary, err := os.CreateTemp(account, ".switchboard-account-home-canary-")
	if err != nil {
		t.Skipf("cannot stage an account-home canary: %v", err)
	}
	canaryPath := canary.Name()
	defer os.Remove(canaryPath)
	if _, err := canary.WriteString(canaryToken); err != nil {
		canary.Close()
		t.Fatal(err)
	}
	if err := canary.Close(); err != nil {
		t.Fatal(err)
	}

	workspace := workspaceFor(t)
	alias := filepath.Join(workspace, ".asdf")
	if err := os.Symlink(account, alias); err != nil {
		t.Fatal(err)
	}
	writeAlias := filepath.Join(workspace, "go")
	if err := os.Symlink(account, writeAlias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", workspace)
	res := runConfined(t, workspace, NetworkLoopback, []string{"/bin/cat", filepath.Join(alias, filepath.Base(canaryPath))}, false)
	if res.ExitCode == 0 || strings.Contains(res.Output, canaryToken) {
		t.Fatalf("forged HOME exposed account-home canary: exit=%d output=%q", res.ExitCode, res.Output)
	}
	marker := filepath.Join(account, "switchboard-cache-alias-write")
	_ = os.Remove(marker)
	res = runConfined(t, workspace, NetworkLoopback, []string{"/bin/sh", "-c", "echo escaped > " + shellQuote(filepath.Join(writeAlias, filepath.Base(marker)))}, false)
	if res.ExitCode == 0 {
		t.Fatalf("forged HOME cache alias reported a successful account-home write: %q", res.Output)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		_ = os.Remove(marker)
		t.Fatalf("forged HOME cache alias mutated account home: %v", err)
	}
}

// A fixture server on an ephemeral loopback port is the most common thing a
// test suite does. Binding one needs network-inbound as well as network-bind,
// which is not obvious: network-bind alone fails as though the kernel refused.
func TestLoopbackServersStillWork(t *testing.T) {
	ws := workspaceFor(t)

	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain to build the probe with")
	}
	writeProbe(t, ws)

	build := runConfined(t, ws, NetworkLoopback, []string{"go", "build", "-o", "probe", "."}, false)
	if build.ExitCode != 0 {
		t.Fatalf("building the probe under the sandbox failed: %s", build.Output)
	}

	res := runConfined(t, ws, NetworkLoopback, []string{filepath.Join(ws, "probe")}, false)
	if res.ExitCode != 0 {
		t.Fatalf("loopback probe failed under the sandbox: %s", res.Output)
	}
	if !strings.Contains(res.Output, "listen ok") || !strings.Contains(res.Output, "dial ok") {
		t.Errorf("probe output = %q, want both a bind and a connect to succeed", res.Output)
	}
}

func writeProbe(t *testing.T, dir string) {
	t.Helper()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module probe\n\ngo 1.26\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
)

func main() {
	for _, addr := range []string{"127.0.0.1:0", "[::1]:0"} {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			fmt.Printf("listen failed %s: %v\n", addr, err)
			return
		}
		l.Close()
	}
	fmt.Println("listen ok")

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer s.Close()
	if _, err := http.Get(s.URL); err != nil {
		fmt.Printf("dial failed: %v\n", err)
		return
	}
	fmt.Println("dial ok")
}
`), 0o644)
}

// sandbox-exec replaces itself with the target, so the process group the runner
// created still governs everything the command spawns. If that stopped being
// true, a timed-out build would leave its compiler running.
func TestProcessGroupKillSurvivesTheWrap(t *testing.T) {
	ws := workspaceFor(t)

	res, err := Run(context.Background(), Command{
		Argv:    []string{"sleep 60 & echo CHILD:$!; wait"},
		Shell:   true,
		Dir:     ws,
		Timeout: 400 * time.Millisecond,
		Confine: confined(t),
		Policy:  Policy{Workspace: ws, Network: NetworkLoopback},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut {
		t.Fatalf("expected a timeout, got %+v", res)
	}

	_, rest, ok := strings.Cut(res.Output, "CHILD:")
	if !ok {
		t.Fatalf("probe did not report a grandchild pid: %q", res.Output)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(strings.SplitN(rest, "\n", 2)[0]))
	if err != nil {
		t.Fatalf("parsing pid from %q: %v", rest, err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	syscall.Kill(pid, syscall.SIGKILL)
	t.Errorf("grandchild %d survived: the wrap broke process-group signalling", pid)
}

// Failing closed is the point. A sandbox that quietly runs the command anyway
// is worse than none, because the interface goes on reporting containment.
func TestUnapplicableConfinementRefusesToRun(t *testing.T) {
	res, err := Run(context.Background(), Command{
		Argv:    []string{"/bin/echo", "should not run"},
		Timeout: 5 * time.Second,
		Confine: confined(t),
		Policy:  Policy{Workspace: "/nonexistent/workspace/path", Network: NetworkLoopback},
	})
	if err == nil {
		t.Fatalf("expected a refusal, got %+v", res)
	}
	if !strings.Contains(err.Error(), "refusing to run unconfined") {
		t.Errorf("err = %v, want it to say the command was not run", err)
	}
}

// The toolchains present on this machine must still work confined. This
// reports only what is installed here; it is not a claim about ecosystems that
// were never run.
func TestInstalledToolchainsWorkConfined(t *testing.T) {
	if testing.Short() {
		t.Skip("toolchain matrix is slow")
	}
	ws := workspaceFor(t)

	cases := []struct {
		name  string
		bin   string
		setup func(dir string)
		argv  []string
	}{
		{"go", "go", func(d string) {
			os.WriteFile(filepath.Join(d, "go.mod"), []byte("module m\n\ngo 1.26\n"), 0o644)
			os.WriteFile(filepath.Join(d, "main.go"), []byte("package main\nfunc main(){}\n"), 0o644)
		}, []string{"go", "build", "-o", "out", "."}},

		{"node", "node", func(d string) {
			os.WriteFile(filepath.Join(d, "i.js"), []byte("console.log('ok')\n"), 0o644)
		}, []string{"node", "i.js"}},

		{"python3", "python3", func(d string) {
			os.WriteFile(filepath.Join(d, "m.py"), []byte("print('ok')\n"), 0o644)
		}, []string{"python3", "m.py"}},

		{"clang", "clang", func(d string) {
			os.WriteFile(filepath.Join(d, "m.c"), []byte("int main(){return 0;}\n"), 0o644)
		}, []string{"clang", "m.c", "-o", "m"}},

		{"git", "git", func(string) {}, []string{"git", "init", "-q", "."}},

		{"cargo", "cargo", func(d string) {
			os.MkdirAll(filepath.Join(d, "src"), 0o755)
			os.WriteFile(filepath.Join(d, "Cargo.toml"),
				[]byte("[package]\nname=\"m\"\nversion=\"0.1.0\"\nedition=\"2021\"\n"), 0o644)
			os.WriteFile(filepath.Join(d, "src", "main.rs"), []byte("fn main(){}\n"), 0o644)
		}, []string{"cargo", "build", "--offline"}},
	}

	ran := 0
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := exec.LookPath(tc.bin); err != nil {
				t.Skipf("%s is not installed", tc.bin)
			}
			dir := filepath.Join(ws, tc.name)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			tc.setup(dir)

			res, err := Run(context.Background(), Command{
				Argv:    tc.argv,
				Dir:     dir,
				Timeout: 3 * time.Minute,
				Confine: confined(t),
				Policy:  Policy{Workspace: ws, Network: NetworkLoopback},
			})
			if err != nil {
				t.Fatal(err)
			}
			if res.ExitCode != 0 {
				t.Errorf("%s does not work under the profile: %s", tc.name, res.Output)
			}
			ran++
		})
	}
	if ran == 0 {
		t.Skip("no toolchains installed to check")
	}
}
