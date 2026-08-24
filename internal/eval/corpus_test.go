package eval

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	fakeGoHelperEnv = "SWITCHBOARD_EVAL_TEST_FAKE_GO"
	fakeGoMarkerEnv = "SWITCHBOARD_EVAL_TEST_FAKE_GO_MARKER"
)

func TestMain(m *testing.M) {
	if os.Getenv(fakeGoHelperEnv) == "1" {
		marker := os.Getenv(fakeGoMarkerEnv)
		if marker == "" || os.WriteFile(marker, []byte("executed"), 0o600) != nil {
			os.Exit(98)
		}
		os.Exit(99)
	}
	os.Exit(m.Run())
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// The corpus is pinned to this repository's source. A spec whose text no longer
// matches is a task that would be handed out already solved, and a harness that
// reports a solve rate over already-solved tasks is worse than no harness.
func TestEverySpecStillMatchesTheSource(t *testing.T) {
	root := repoRoot(t)

	for _, s := range specs {
		t.Run(s.id, func(t *testing.T) {
			for _, b := range s.breaks {
				body, err := os.ReadFile(filepath.Join(root, b.file))
				if err != nil {
					t.Fatalf("%s: %v", b.file, err)
				}
				_, matches := applyBreakage(string(body), b)
				if matches == 0 {
					t.Errorf("%s no longer contains the text this task breaks:\n%q", b.file, b.old)
				}
				if matches > 1 {
					// Replace(…, 1) would break an arbitrary one of them, so the
					// task would not be the one it claims to be.
					t.Errorf("%s contains the broken text %d times, so the edit is ambiguous",
						b.file, matches)
				}
			}
			for path, want := range s.mustContain {
				body, err := os.ReadFile(filepath.Join(root, path))
				if err != nil {
					t.Fatalf("%s: %v", path, err)
				}
				if !strings.Contains(string(body), want) {
					t.Errorf("%s does not contain the property this task re-checks: %q", path, want)
				}
			}
		})
	}
}

func TestApplyBreakagePreservesMaterializedLineEndingsAndExactness(t *testing.T) {
	mutation := breakage{
		old: "if ready {\n\treturn true\n}",
		new: "if false {\n\treturn true\n}",
	}
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "lf",
			body: "before\nif ready {\n\treturn true\n}\nafter\n",
			want: "before\nif false {\n\treturn true\n}\nafter\n",
		},
		{
			name: "crlf",
			body: "before\r\nif ready {\r\n\treturn true\r\n}\r\nafter\r\n",
			want: "before\r\nif false {\r\n\treturn true\r\n}\r\nafter\r\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, matches := applyBreakage(tc.body, mutation)
			if matches != 1 || got != tc.want {
				t.Fatalf("applyBreakage() = matches %d, %q; want 1, %q", matches, got, tc.want)
			}
		})
	}

	mixed := "if ready {\n\treturn true\n}\r\nif ready {\r\n\treturn true\r\n}\r\n"
	if got, matches := applyBreakage(mixed, mutation); matches != 2 || got != mixed {
		t.Fatalf("ambiguous mixed-ending mutation = matches %d, %q", matches, got)
	}

	singleLine := breakage{old: "return true", new: "if ready {\n\treturn true\n}"}
	crlfBody := "before\r\nreturn true\r\nafter\r\n"
	crlfWant := "before\r\nif ready {\r\n\treturn true\r\n}\r\nafter\r\n"
	if got, matches := applyBreakage(crlfBody, singleLine); matches != 1 || got != crlfWant {
		t.Fatalf("single-line CRLF mutation = matches %d, %q; want 1, %q", matches, got, crlfWant)
	}
}

func TestTaskSetupMutatesCRLFCheckoutWithoutNormalizingTheFile(t *testing.T) {
	source := t.TempDir()
	body := "package fixture\r\n\r\nfunc ready() bool {\r\n\treturn true\r\n}\r\n"
	if err := os.WriteFile(filepath.Join(source, "fixture.go"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	task := taskFor(source, spec{
		id: "crlf-checkout",
		breaks: []breakage{{
			file: "fixture.go",
			old:  "func ready() bool {\n\treturn true\n}",
			new:  "func ready() bool {\n\treturn false\n}",
		}},
	})
	if err := task.Setup(destination); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(destination, "fixture.go"))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(body, "\treturn true", "\treturn false", 1)
	if string(got) != want {
		t.Fatalf("mutated CRLF checkout = %q, want %q", got, want)
	}
}

// §8.6 sets the floor at twenty to thirty hand-written tasks, and the gate
// refuses below it.
func TestTheCorpusMeetsTheFloor(t *testing.T) {
	tasks := Tier1(repoRoot(t))
	if len(tasks) < MinimumTier1Tasks {
		t.Errorf("the corpus has %d tasks and the gate needs %d", len(tasks), MinimumTier1Tasks)
	}
	seen := map[string]bool{}
	for _, task := range tasks {
		if task.Provenance != HandWritten {
			t.Errorf("%s is not tier 1", task.ID)
		}
		if seen[task.ID] {
			t.Errorf("duplicate task id %q", task.ID)
		}
		seen[task.ID] = true
	}
}

func TestTaskVerifierIgnoresCopiedRepositoryGoOnPATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the PATH-shadow executable fixture is a POSIX script")
	}
	want, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	want, err = filepath.EvalSymlinks(want)
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	for name, body := range map[string]string{
		"go.mod":          "module example.com/switchboard/pathshadow\n\ngo 1.20\n",
		"fixture.go":      "package fixture\n",
		"fixture_test.go": "package fixture\n\nimport \"testing\"\n\nfunc TestFixture(t *testing.T) {}\n",
		"go":              "#!/bin/sh\n: > \"$FAKE_GO_MARKER\"\nexit 99\n",
	} {
		mode := os.FileMode(0o644)
		if name == "go" {
			mode = 0o755
		}
		if err := os.WriteFile(filepath.Join(source, name), []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}

	copy := t.TempDir()
	task := taskFor(source, spec{id: "path-shadow", pkg: "./"})
	if err := task.Setup(copy); err != nil {
		t.Fatalf("copying verifier fixture: %v", err)
	}
	marker := filepath.Join(t.TempDir(), "fake-go-ran")
	t.Setenv("FAKE_GO_MARKER", marker)
	t.Setenv("PATH", copy+string(os.PathListSeparator)+os.Getenv("PATH"))

	solved, detail, err := task.Verify(context.Background(), copy)
	if err != nil {
		t.Fatalf("verifying copied repository: %v", err)
	}
	if !solved {
		t.Fatalf("real Go verifier did not pass: %s", detail)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("copied repository's PATH-shadowed go executed; marker stat error = %v", err)
	}
	environ, err := verifierGoEnv(source, copy)
	if err != nil {
		t.Fatal(err)
	}
	got, err := verifierGoExecutable(environ, source, copy)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path() != want || !filepath.IsAbs(got.Path()) {
		t.Fatalf("running Go executable = %q, want absolute %q", got.Path(), want)
	}
}

func TestTaskVerifierRejectsLaunchCheckoutGoForDifferentWorkspace(t *testing.T) {
	launchWorkspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(launchWorkspace, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	launchBin := filepath.Join(launchWorkspace, "bin")
	if err := os.Mkdir(launchBin, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeName := "go"
	if runtime.GOOS == "windows" {
		fakeName += ".exe"
	}
	fakeGo := filepath.Join(launchBin, fakeName)
	testExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	sourceExecutable, err := os.Open(testExecutable)
	if err != nil {
		t.Fatal(err)
	}
	defer sourceExecutable.Close()
	fakeExecutable, err := os.OpenFile(fakeGo, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(fakeExecutable, sourceExecutable); err != nil {
		_ = fakeExecutable.Close()
		t.Fatal(err)
	}
	if err := fakeExecutable.Close(); err != nil {
		t.Fatal(err)
	}

	source := t.TempDir()
	for name, body := range map[string]string{
		"go.mod":          "module example.com/switchboard/crossworkspace\n\ngo 1.20\n",
		"fixture.go":      "package fixture\n",
		"fixture_test.go": "package fixture\n\nimport \"testing\"\n\nfunc TestFixture(t *testing.T) {}\n",
	} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	copyDir := t.TempDir()
	task := taskFor(source, spec{id: "cross-workspace-path-shadow", pkg: "./"})
	if err := task.Setup(copyDir); err != nil {
		t.Fatal(err)
	}
	// Capture the resolver's trusted baseline through the same sanitized path
	// policy the verifier uses. On Windows, setup-go installs the active
	// toolchain behind a cross-volume junction that filepath.EvalSymlinks may
	// conservatively omit, so an unsanitized exec.LookPath result is not a
	// portable oracle for which remaining trusted toolchain will be selected.
	baselineEnv, err := verifierGoEnv(source, copyDir)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := verifierGoExecutable(baselineEnv, source, copyDir)
	if err != nil {
		t.Fatal(err)
	}
	baselineInfo, err := os.Stat(baseline.Path())
	if err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(t.TempDir(), "fake-go-ran")
	t.Setenv(fakeGoHelperEnv, "1")
	t.Setenv(fakeGoMarkerEnv, marker)
	t.Setenv("PATH", launchBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Chdir(launchWorkspace)

	environ, err := verifierGoEnv(source, copyDir)
	if err != nil {
		t.Fatal(err)
	}
	var filteredPath string
	for _, entry := range environ {
		key, value, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(key, "PATH") {
			filteredPath = value
		}
	}
	for _, directory := range filepath.SplitList(filteredPath) {
		if directory == launchBin {
			t.Fatalf("verifier PATH retained launch-checkout directory %q", launchBin)
		}
	}
	resolved, err := verifierGoExecutable(environ, source, copyDir)
	if err != nil {
		t.Fatal(err)
	}
	resolvedInfo, err := os.Stat(resolved.Path())
	if err != nil {
		t.Fatal(err)
	}
	fakeInfo, err := os.Stat(fakeGo)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(resolved.Path()) || !os.SameFile(baselineInfo, resolvedInfo) || os.SameFile(fakeInfo, resolvedInfo) {
		t.Fatalf("verifier Go = %q, want trusted baseline %q and not launch-checkout shadow %q",
			resolved.Path(), baseline.Path(), fakeGo)
	}

	solved, detail, err := task.Verify(context.Background(), copyDir)
	if err != nil || !solved || detail != "" {
		t.Fatalf("cross-workspace verifier = solved=%v detail=%q err=%v", solved, detail, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("launch-checkout Go executed: %v", err)
	}
}

func TestTaskVerifierIgnoresHostileAmbientGOROOT(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the hostile Go executable fixture is a POSIX script")
	}
	const child = "SB_EVAL_HOSTILE_GOROOT_CHILD"
	marker := os.Getenv("SB_EVAL_HOSTILE_GOROOT_MARKER")
	if os.Getenv(child) == "1" {
		source := t.TempDir()
		for name, body := range map[string]string{
			"go.mod":          "module example.com/switchboard/hostilegoroot\n\ngo 1.20\n",
			"fixture.go":      "package fixture\n",
			"fixture_test.go": "package fixture\n\nimport \"testing\"\n\nfunc TestFixture(t *testing.T) {}\n",
		} {
			if err := os.WriteFile(filepath.Join(source, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		copy := t.TempDir()
		task := taskFor(source, spec{id: "hostile-goroot", pkg: "./"})
		if err := task.Setup(copy); err != nil {
			t.Fatal(err)
		}
		solved, detail, err := task.Verify(context.Background(), copy)
		if err != nil || !solved || detail != "" {
			t.Fatalf("hostile GOROOT verifier = solved=%v detail=%q err=%v", solved, detail, err)
		}
		if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
			t.Fatalf("hostile GOROOT Go executable ran: %v", statErr)
		}
		return
	}

	fakeRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(fakeRoot, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	marker = filepath.Join(t.TempDir(), "hostile-go-ran")
	fakeGo := "#!/bin/sh\n: > '" + strings.ReplaceAll(marker, "'", "'\"'\"'") + "'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(fakeRoot, "bin", "go"), []byte(fakeGo), 0o700); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestTaskVerifierIgnoresHostileAmbientGOROOT$")
	env := make([]string, 0, len(os.Environ())+3)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if name != "GOROOT" && name != child && name != "SB_EVAL_HOSTILE_GOROOT_MARKER" {
			env = append(env, entry)
		}
	}
	cmd.Env = append(env,
		"GOROOT="+fakeRoot,
		child+"=1",
		"SB_EVAL_HOSTILE_GOROOT_MARKER="+marker,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hostile-GOROOT child: %v\n%s", err, output)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("hostile GOROOT Go executable ran: %v", err)
	}
}

func TestVerifierGoEnvPinsOfflinePolicyAndDropsAmbientToolControls(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	t.Setenv("PATH", workspace+string(os.PathListSeparator)+external)
	t.Setenv("GOFLAGS", "-toolexec="+filepath.Join(workspace, "evil"))
	t.Setenv("GOTOOLCHAIN", filepath.Join(workspace, "toolchain"))
	t.Setenv("CC", filepath.Join(workspace, "cc"))
	t.Setenv("NODE_OPTIONS", "--require="+filepath.Join(workspace, "evil.js"))

	environ, err := verifierGoEnv(workspace)
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string][]string)
	for _, entry := range environ {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			values[strings.ToUpper(name)] = append(values[strings.ToUpper(name)], value)
		}
	}
	for name, want := range map[string]string{
		"GOENV": "off", "GOFLAGS": "-count=1", "GOTOOLCHAIN": "local",
		"GOWORK": "off", "GOPROXY": "off", "GOSUMDB": "off",
		"GOVCS": "*:off", "CGO_ENABLED": "0",
	} {
		if got := values[name]; len(got) != 1 || got[0] != want {
			t.Errorf("%s = %q, want exactly %q", name, got, want)
		}
	}
	for _, name := range []string{"CC", "NODE_OPTIONS"} {
		if got := values[name]; len(got) != 0 {
			t.Errorf("verifier environment retained %s=%q", name, got)
		}
	}
	wantPath, err := filepath.EvalSymlinks(external)
	if err != nil {
		t.Fatal(err)
	}
	if got := values["PATH"]; len(got) != 1 || got[0] != wantPath {
		t.Fatalf("verifier PATH = %q, want exactly %q", got, wantPath)
	}
}

func TestTaskVerifierDeadlineStopsHangingTestProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group descendant proof is Unix-specific")
	}
	source := t.TempDir()
	for name, body := range map[string]string{
		"go.mod":          "module example.com/switchboard/hangingverifier\n\ngo 1.20\n",
		"fixture.go":      "package fixture\n",
		"fixture_test.go": "package fixture\n\nimport \"testing\"\n\nfunc TestHang(t *testing.T) { select {} }\n",
	} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	task := taskFor(source, spec{id: "hanging-verifier", pkg: "./"})
	if err := task.Setup(dir); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	solved, detail, err := task.Verify(ctx, dir)
	if !errors.Is(err, context.DeadlineExceeded) || solved || detail != "" {
		t.Fatalf("hanging verifier = solved=%v detail=%q err=%v", solved, detail, err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("hanging verifier returned after %s", elapsed)
	}
}

func TestVerifierOutputCapExactAndOneOver(t *testing.T) {
	out := &verifierOutput{}
	exact := strings.Repeat("x", maxVerifierOutputBytes)
	if n, err := out.Write([]byte(exact)); err != nil || n != len(exact) {
		t.Fatalf("exact write = %d, %v", n, err)
	}
	if got, truncated := out.String(); truncated || got != exact {
		t.Fatalf("exact output = len %d truncated %v", len(got), truncated)
	}
	if n, err := out.Write([]byte("y")); err != nil || n != 1 {
		t.Fatalf("one-over write = %d, %v", n, err)
	}
	got, truncated := out.String()
	if !truncated || len(got) != maxVerifierOutputBytes {
		t.Fatalf("one-over output = len %d truncated %v", len(got), truncated)
	}
	if got[0] != 'x' || got[len(got)-1] != 'y' {
		t.Fatalf("one-over output head/tail = %q/%q", got[:1], got[len(got)-1:])
	}
}

// Every task must break its package and be restored by the obvious fix. A task
// that passes before it is attempted measures nothing, and one that cannot be
// fixed measures nothing either.
func TestTasksBreakWhatTheyClaimAndPassWhenRestored(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles and runs the suite for each task")
	}
	root := repoRoot(t)

	for _, s := range specs {
		t.Run(s.id, func(t *testing.T) {
			if s.goos != "" && s.goos != runtime.GOOS {
				// A breakage to a file this platform never compiles cannot fail
				// here, so the task would pass its verifier untouched.
				t.Skipf("task targets %s-only code", s.goos)
			}
			t.Parallel()
			dir := t.TempDir()

			task := taskFor(root, s)
			if err := task.Setup(dir); err != nil {
				t.Fatalf("setup: %v", err)
			}

			solved, detail, err := task.Verify(context.Background(), dir)
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			if solved {
				t.Fatalf("the task passes its own verifier before anything is done to it, so it measures nothing")
			}
			if detail == "" {
				t.Error("a failing verifier reported no detail, so a model would be told nothing")
			}
		})
	}
}
