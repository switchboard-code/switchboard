package execution

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
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
	path, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bubblewrap is not installed")
	}
	bwrap, err := resolveBubblewrapExecutable(path, string(filepath.Separator), 0, true)
	if err != nil {
		t.Skipf("bubblewrap is not a trusted system executable: %v", err)
	}

	// Unprivileged user namespaces are a kernel setting some distributions turn
	// off. Without them bubblewrap cannot build a namespace at all, which is a
	// property of the host rather than a defect in the construction.
	probe := exec.Command(bwrap.path, "--ro-bind", "/", "/", "--unshare-user", "--", "/bin/true")
	if err := probe.Run(); err != nil {
		t.Skipf("bubblewrap cannot create a namespace here: %v", err)
	}
	return &Confinement{mechanism: MechanismBubblewrap, wrap: bwrap.wrap}
}

func runConfined(t *testing.T, ws string, network NetworkAccess, argv []string, shell bool) Result {
	t.Helper()
	res, err := Run(context.Background(), Command{
		Argv:    argv,
		Shell:   shell,
		Dir:     ws,
		Timeout: 60 * time.Second,
		Confine: confined(t),
		Policy:  Policy{Workspace: ws, Network: network},
	})
	if err != nil {
		t.Fatalf("running %v: %v", argv, err)
	}
	return res
}

func TestSelfTestPassesOnThisHost(t *testing.T) {
	confinement := confined(t)
	ok, detail := linuxSelfTest(confinement.wrap)
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

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	// Not t.TempDir(): that lives under $TMPDIR, which is granted on purpose
	// because build tools are unusable without it.
	for _, escape := range []string{
		filepath.Join(home, ".switchboard-write-escape-probe"),
		"/etc/switchboard-write-escape-probe",
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

// The counterpart of the macOS keychain finding: hiding credential files is
// pointless if the daemon handing out those credentials is still reachable.
func TestConfinedCommandCannotReadCredentials(t *testing.T) {
	ws := workspaceFor(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}

	canary := filepath.Join(home, ".switchboard", "cred-canary")
	if err := os.MkdirAll(filepath.Dir(canary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canary, []byte(canaryToken), 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(canary)

	res := runConfined(t, ws, NetworkLoopback, []string{"/bin/cat", canary}, false)
	if strings.Contains(res.Output, canaryToken) {
		t.Error("a confined command read Switchboard's session state, which holds other projects' prompts and code")
	}

	fake := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(fake, 0o700); err == nil {
		key := filepath.Join(fake, "switchboard-test-key")
		if err := os.WriteFile(key, []byte(canaryToken), 0o600); err == nil {
			defer os.Remove(key)
			res := runConfined(t, ws, NetworkLoopback, []string{"/bin/cat", key}, false)
			if strings.Contains(res.Output, canaryToken) {
				t.Error("a confined command read a key out of ~/.ssh")
			}
		}
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

func TestExactGoRootCannotReopenResolvedCredentialTarget(t *testing.T) {
	account, err := accountHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	toolchainDir, err := os.MkdirTemp(account, ".switchboard-goroot-secret-")
	if err != nil {
		t.Skipf("cannot stage an account-home Go root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(toolchainDir) })
	root := fakeGoToolchain(t, toolchainDir)
	secret := filepath.Join(root, "credential")
	if err := os.WriteFile(secret, []byte(canaryToken), 0o600); err != nil {
		t.Fatal(err)
	}
	goPath := filepath.Join(root, "bin", "go")
	if err := os.WriteFile(goPath, []byte("#!/bin/sh\ncat \"$GOROOT/credential\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cargo := filepath.Join(account, ".cargo")
	createdCargo := false
	if info, err := os.Lstat(cargo); os.IsNotExist(err) {
		if err := os.Mkdir(cargo, 0o700); err != nil {
			t.Skipf("cannot stage .cargo: %v", err)
		}
		createdCargo = true
	} else if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Skipf("account .cargo is not a real directory: %v", err)
	}
	if createdCargo {
		t.Cleanup(func() { _ = os.Remove(cargo) })
	}
	var credential string
	for _, name := range []string{"credentials", "credentials.toml"} {
		candidate := filepath.Join(cargo, name)
		if _, err := os.Lstat(candidate); os.IsNotExist(err) {
			credential = candidate
			break
		}
	}
	if credential == "" {
		t.Skip("both cargo credential paths already exist; refusing to replace user state")
	}
	if err := os.Symlink(secret, credential); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(credential) })

	t.Setenv("HOME", account)
	workspace := workspaceFor(t)
	res, err := Run(context.Background(), Command{
		Argv:    []string{goPath},
		Dir:     workspace,
		Timeout: 30 * time.Second,
		Confine: confined(t),
		Policy:  Policy{Workspace: workspace, Network: NetworkLoopback},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Output, canaryToken) {
		t.Fatalf("exact Go-root reopen exposed resolved credential target: %q", res.Output)
	}
}

// The policy mapping is checked without touching the network, so it holds on a
// host with no egress and no HTTP client. An assertion that needs the internet
// to detect a missing flag is an assertion that quietly stops running.
func TestNetworkNamespaceFlags(t *testing.T) {
	ws := workspaceFor(t)
	bwrap := testBubblewrapExecutable(t)

	loopback, err := bwrap.wrap(Policy{Workspace: ws, Network: NetworkLoopback}, []string{"/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(loopback, "--unshare-net") {
		t.Error("the default policy must take a private network namespace")
	}

	full, err := bwrap.wrap(Policy{Workspace: ws, Network: NetworkFull}, []string{"/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(full, "--unshare-net") {
		t.Error("a granted-network command must keep the host's network namespace")
	}
}

// Model argv starts only after bubblewrap's end-of-options marker. Without the
// marker this request is parsed as an extra --bind rule followed by /bin/true,
// so it exits successfully instead of trying (and failing) to execute a binary
// literally named --bind.
func TestOptionLookingExecutableCannotInjectBubblewrapRules(t *testing.T) {
	ws := workspaceFor(t)
	res := runConfined(t, ws, NetworkLoopback,
		[]string{"--bind", "/", "/", "/bin/true"}, false)
	if res.ExitCode == 0 {
		t.Fatal("option-looking model argv was parsed as bubblewrap policy")
	}
}

func TestNetworkPolicy(t *testing.T) {
	ws := workspaceFor(t)

	probe := egressProbeArgv()
	if probe == nil {
		t.Skip("no curl, wget, or nc to attempt a connection with")
	}

	// The granted case runs first: without it, a denial proves nothing, because
	// a host with no internet refuses either way.
	if granted := runConfined(t, ws, NetworkFull, probe, false); granted.ExitCode != 0 {
		t.Skipf("this host has no egress even when granted (exit %d); nothing to compare against", granted.ExitCode)
	}
	if denied := runConfined(t, ws, NetworkLoopback, probe, false); denied.ExitCode == 0 {
		t.Error("the default policy allowed egress off the machine")
	}
}

// A private network namespace comes with a working loopback interface, so
// fixture servers bind while egress stays unreachable. That is the property the
// whole loopback policy rests on.
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

// bubblewrap puts the command in a new PID namespace where it is init, so
// killing the wrapper has to tear the namespace down with it. If that stopped
// working, a timed-out build would leave its compiler running.
func TestProcessGroupKillSurvivesTheWrap(t *testing.T) {
	ws := workspaceFor(t)
	marker := filepath.Join(ws, "still-running")

	res, err := Run(context.Background(), Command{
		Argv:    []string{"(while true; do touch " + marker + "; sleep 0.1; done) & wait"},
		Shell:   true,
		Dir:     ws,
		Timeout: 500 * time.Millisecond,
		Confine: confined(t),
		Policy:  Policy{Workspace: ws, Network: NetworkLoopback},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut {
		t.Fatalf("expected a timeout, got %+v", res)
	}

	// A pid from inside a PID namespace means nothing out here, so liveness is
	// measured by whether the descendant keeps touching a file after the
	// wrapper is gone.
	os.Remove(marker)
	time.Sleep(600 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("a descendant survived the timeout and is still writing")
	}
}

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

// A user who has never run a build has no ~/.cache. Binding it with --bind-try
// silently skips it, and the tool inside cannot create it either, because the
// home directory is read-only by then. The failure lands on the very first
// confined command, which is the worst possible time for it.
func TestFirstRunWithNoCacheDirectory(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain")
	}

	// Not t.TempDir(): that lives under $TMPDIR, which the confinement binds
	// writable, so a cache directory there would be creatable and the test
	// would pass whether or not the bug was fixed. The fresh home has to sit
	// somewhere the sandbox actually makes read-only.
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := os.MkdirTemp(realHome, ".switchboard-freshhome-")
	if err != nil {
		t.Skipf("cannot create a scratch home under %s: %v", realHome, err)
	}
	defer os.RemoveAll(fresh)

	t.Setenv("HOME", fresh)
	// Pinned rather than left to default resolution so the test does not
	// inherit a GOCACHE from the environment running it. The path is the one
	// the default would produce under this HOME.
	t.Setenv("GOCACHE", filepath.Join(fresh, ".cache", "go-build"))

	if _, err := os.Stat(filepath.Join(fresh, ".cache")); err == nil {
		t.Fatal("the fresh home already has a cache directory; the test proves nothing")
	}

	ws := workspaceFor(t)
	os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module m\n\ngo 1.26\n"), 0o644)
	os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\nfunc main(){}\n"), 0o644)

	res := runConfined(t, ws, NetworkLoopback, []string{"go", "build", "-o", "out", "."}, false)
	if res.ExitCode != 0 {
		t.Fatalf("first confined build in a fresh home failed: %s", res.Output)
	}
}

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
				t.Errorf("%s does not work confined: %s", tc.name, res.Output)
			}
			ran++
		})
	}
	if ran == 0 {
		t.Skip("no toolchains installed to check")
	}
}
