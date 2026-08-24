package execution

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

// accountHomeDir returns the home directory attached to the process's account
// identity, without consulting HOME. HOME is ordinary process input: a command
// launched from an untrusted checkout can inherit a value that points back into
// that checkout. It must never decide which directory the sandbox protects or
// where Switchboard stores evidence that the sandbox passed its self-test.
func accountHomeDir() (string, error) {
	account, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("looking up the current account: %w", err)
	}
	if account == nil || account.HomeDir == "" {
		return "", errors.New("the current account has no home directory")
	}
	return canonicalHomeDirectory(account.HomeDir, "account home")
}

// ambientHomeDir is the functional home presented to child processes. Unlike
// accountHomeDir it deliberately honors HOME, because build tools and their
// caches are expected to follow the user's environment. The sandbox protects
// this directory too when it differs from the account home, but it never uses
// it as a substitute for the account-home security boundary.
func ambientHomeDir() (string, error) {
	// os.UserHomeDir consults HOME on Unix, but USERPROFILE (or
	// HOMEDRIVE/HOMEPATH) on Windows. Switchboard's functional child
	// environment deliberately treats an explicit HOME consistently across
	// platforms: Unix-oriented build tools honor it on Windows too, and the
	// confinement boundary must cover the same directory those tools see.
	home := os.Getenv("HOME")
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolving ambient home: %w", err)
		}
	}
	return canonicalAmbientHomeDirectory(home)
}

func canonicalHomeDirectory(path, kind string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("%s is empty", kind)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("making %s absolute: %w", kind, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", kind, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("inspecting %s: %w", kind, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s %q is not a directory", kind, resolved)
	}
	return resolved, nil
}

// protectedHomeDirectories returns every home-shaped authority that must be
// closed by confinement. The account home is always first. A distinct ambient
// home is included for defense in depth and for predictable tool behavior.
func protectedHomeDirectories() ([]string, error) {
	account, err := accountHomeDir()
	if err != nil {
		return nil, err
	}
	ambient, err := ambientHomeDir()
	if err != nil {
		return nil, err
	}
	homes := []string{account}
	if ambient != account {
		homes = append(homes, ambient)
	}
	return homes, nil
}

// minimalHomeCovers removes a home that is already covered by an ancestor.
// Bubblewrap builds its mount tree in order; mounting a tmpfs on both an
// ancestor and its child is unnecessary and can make the child's mountpoint
// disappear. Reopens are still calculated for every logical home.
func minimalHomeCovers(homes []string) []string {
	var covers []string
	for _, candidate := range homes {
		covered := false
		for _, root := range covers {
			if directoryContains(root, candidate) {
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		kept := covers[:0]
		for _, root := range covers {
			if !directoryContains(candidate, root) {
				kept = append(kept, root)
			}
		}
		covers = append(kept, candidate)
	}
	return covers
}

func directoryContains(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !filepath.IsAbs(rel) &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
