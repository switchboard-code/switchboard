//go:build unix

package scm

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/safeexec"
)

func TestDiscoverNeverExecutesRepositoryPATHShadow(t *testing.T) {
	root := initTestRepo(t)
	workspace := filepath.Join(root, "nested")
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "shadow-ran")
	script := "#!/bin/sh\n/usr/bin/touch '" + marker + "'\nexit 99\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := Discover(context.Background(), workspace, testExecutionAuthority(true))
	if !errors.Is(err, safeexec.ErrUntrustedPath) {
		t.Fatalf("error = %v, want safeexec.ErrUntrustedPath", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("repository PATH shadow executed: %v", err)
	}
}

func TestDiscoverRejectsLaunchRepositoryPATHShadowForDifferentTarget(t *testing.T) {
	launchRoot := initTestRepo(t)
	targetRoot := initTestRepo(t)
	bin := filepath.Join(launchRoot, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(launchRoot, "cross-workspace-shadow-ran")
	script := "#!/bin/sh\n/usr/bin/touch '" + marker + "'\nexit 99\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Chdir(launchRoot)

	_, err := Discover(context.Background(), targetRoot, testExecutionAuthority(true))
	if !errors.Is(err, safeexec.ErrUntrustedPath) {
		t.Fatalf("error = %v, want safeexec.ErrUntrustedPath", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("launch-repository PATH shadow executed for a different target: %v", err)
	}
}

func TestDiscoverNeverExecutesRepositorySymlinkPATHShadow(t *testing.T) {
	root := initTestRepo(t)
	workspace := filepath.Join(root, "nested")
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "symlink-shadow-ran")
	external := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\n/usr/bin/touch '" + marker + "'\nexit 99\n"
	if err := os.WriteFile(external, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(bin, "git")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := Discover(context.Background(), workspace, testExecutionAuthority(true))
	if !errors.Is(err, safeexec.ErrUntrustedPath) {
		t.Fatalf("error = %v, want safeexec.ErrUntrustedPath", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("repository symlink PATH shadow executed: %v", err)
	}
}

func TestDiscoverEnvPythonHelperCannotImportWorkspaceUserCustomize(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "nested")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	userBase := filepath.Join(workspace, "python-user-base")
	t.Setenv("PYTHONUSERBASE", userBase)
	t.Setenv("PYTHONNOUSERSITE", "")

	query := exec.Command(python, "-s", "-c", "import site; print(site.getusersitepackages())")
	siteOutput, err := query.Output()
	if err != nil {
		t.Fatalf("locating the Python user site: %v", err)
	}
	userSite := strings.TrimSpace(string(siteOutput))
	rel, err := filepath.Rel(workspace, userSite)
	if err != nil || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("Python user site %q is not beneath workspace %q", userSite, workspace)
	}
	if err := os.MkdirAll(userSite, 0o755); err != nil {
		t.Fatal(err)
	}
	customize := `import os
with open(os.environ["SB_SCM_USERCUSTOMIZE_MARKER"], "w", encoding="utf-8") as marker:
    marker.write("workspace code ran")
`
	if err := os.WriteFile(filepath.Join(userSite, "usercustomize.py"), []byte(customize), 0o644); err != nil {
		t.Fatal(err)
	}

	trustedBin := t.TempDir()
	gitPath := filepath.Join(trustedBin, "git")
	gitScript := `#!/usr/bin/env python3
import os
import sys
if "--is-inside-work-tree" in sys.argv:
    print("true")
elif "--show-toplevel" in sys.argv:
    print(os.environ["SB_SCM_TEST_ROOT"])
else:
    raise SystemExit(2)
`
	if err := os.WriteFile(gitPath, []byte(gitScript), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(workspace, "usercustomize-ran")
	t.Setenv("SB_SCM_USERCUSTOMIZE_MARKER", marker)
	t.Setenv("SB_SCM_TEST_ROOT", root)
	t.Setenv("PATH", strings.Join([]string{trustedBin, filepath.Dir(python), os.Getenv("PATH")}, string(os.PathListSeparator)))

	repo, err := Discover(context.Background(), workspace, testExecutionAuthority(true))
	if err != nil {
		t.Fatalf("discovering with a trusted env-Python helper: %v", err)
	}
	wantRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if repo.Root != wantRoot {
		t.Fatalf("repository root = %q, want %q", repo.Root, wantRoot)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("workspace usercustomize executed through trusted Git helper: %v", err)
	}
}
