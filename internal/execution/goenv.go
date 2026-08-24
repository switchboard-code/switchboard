package execution

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/switchboard-code/switchboard/internal/safeexec"
)

// bindGoToolchain replaces ambient GOROOT with one derived from the exact Go
// executable a direct argv command selected. Recent distribution binaries may
// be trimmed and unable to discover GOROOT when a sandbox hides the executable's
// parent (notably actions/setup-go under macOS runner home directories).
//
// Runtime.GOROOT is deliberately not used: it names the machine that built sb,
// not necessarily this machine's selected Go installation. Nor is `go env`
// executed as a preflight, because that would run workspace-selected code
// outside the command's approved confinement.
func bindGoToolchain(c Command) (Policy, []string) {
	policy := c.Policy
	if c.Shell || len(c.Argv) == 0 || !isGoExecutableName(c.Argv[0]) {
		return policy, nil
	}

	authorityInputs := make([]string, 0, 2)
	if strings.TrimSpace(c.Policy.Workspace) != "" {
		authorityInputs = append(authorityInputs, c.Policy.Workspace)
	}
	if strings.TrimSpace(c.Dir) != "" && c.Dir != c.Policy.Workspace {
		authorityInputs = append(authorityInputs, c.Dir)
	}
	authorityRoots, err := safeexec.WorkspaceAndCurrentAuthorityRoots(authorityInputs...)
	if err != nil {
		return policy, nil
	}

	var executable safeexec.Executable
	if filepath.IsAbs(c.Argv[0]) {
		executable, err = safeexec.ResolvePathOutside(c.Argv[0], authorityRoots...)
	} else if filepath.Base(c.Argv[0]) == c.Argv[0] {
		executable, err = safeexec.ResolveOutside(c.Argv[0], authorityRoots...)
	} else {
		return policy, nil
	}
	if err != nil {
		return policy, nil
	}

	root, err := validatedGoRoot(executable, authorityRoots)
	if err != nil {
		return policy, nil
	}
	if rootNeedsHomeReopen(root) && !overlapsProtectedCredentialPath(root) {
		policy.readOnlyRoots = append(policy.readOnlyRoots, root)
	}
	return policy, []string{"GOROOT=" + root}
}

func isGoExecutableName(path string) bool {
	name := filepath.Base(path)
	return strings.EqualFold(name, "go") || strings.EqualFold(name, "go.exe")
}

func validatedGoRoot(executable safeexec.Executable, authorityRoots []string) (string, error) {
	bin := filepath.Dir(executable.Path())
	if !strings.EqualFold(filepath.Base(bin), "bin") {
		return "", errors.New("Go executable is not beneath a bin directory")
	}
	root, err := safeexec.ResolveDirectoryOutside(filepath.Dir(bin), authorityRoots...)
	if err != nil {
		return "", err
	}
	if _, err := safeexec.ResolveDirectoryOutside(filepath.Join(root, "src", "runtime"), authorityRoots...); err != nil {
		return "", err
	}
	compiler := filepath.Join(root, "pkg", "tool", runtime.GOOS+"_"+runtime.GOARCH, "compile")
	if runtime.GOOS == "windows" {
		compiler += ".exe"
	}
	if _, err := safeexec.ResolvePathOutside(compiler, authorityRoots...); err != nil {
		return "", err
	}
	return root, nil
}

func rootNeedsHomeReopen(root string) bool {
	homes, err := protectedHomeDirectories()
	if err != nil {
		return false
	}
	for _, home := range homes {
		if goPathWithin(home, root) {
			return true
		}
	}
	return false
}

func overlapsProtectedCredentialPath(root string) bool {
	homes, err := protectedHomeDirectories()
	if err != nil {
		return true
	}
	for _, home := range homes {
		for _, path := range protectedHomePaths(home) {
			if goPathWithin(root, path) || goPathWithin(path, root) {
				return true
			}
		}
	}
	return false
}

func goPathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !filepath.IsAbs(rel) &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
