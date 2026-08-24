//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenDiffRefusesRepositoryExecutionUntilTrustGrant(t *testing.T) {
	root := initTUIDiffRepo(t)
	writeTUIDiffFile(t, root, ".gitattributes", []byte("*.txt filter=switchboard-security-test\n"))
	writeTUIDiffFile(t, root, "tracked.txt", []byte("base\n"))
	commitTUIDiffFiles(t, root)

	filterMarker := filepath.Join(root, "filter-ran")
	hookMarker := filepath.Join(root, "hook-ran")
	runTUIDiffGit(t, root, "config", "filter.switchboard-security-test.clean",
		"/usr/bin/touch '"+filterMarker+"'; /bin/cat")
	runTUIDiffGit(t, root, "config", "filter.switchboard-security-test.required", "true")
	hook := filepath.Join(root, ".git", "hooks", "post-index-change")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n/usr/bin/touch '"+hookMarker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTUIDiffFile(t, root, "tracked.txt", []byte("changed\n"))

	untrusted := openDiff(root, false, nil)().(diffLoadedMsg)
	if untrusted.err == nil || !strings.Contains(untrusted.err.Error(), "/trust grant") {
		t.Fatalf("untrusted /diff error = %v, want trust instruction", untrusted.err)
	}
	for _, marker := range []string{filterMarker, hookMarker} {
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("untrusted /diff executed repository code via %s: %v", marker, err)
		}
	}

	trusted := openDiff(root, false, grantTUIDiffTrust(t, root))().(diffLoadedMsg)
	if trusted.err != nil {
		t.Fatal(trusted.err)
	}
	if _, err := os.Stat(filterMarker); err != nil {
		t.Fatalf("trusted /diff did not preserve repository Git filter behavior: %v", err)
	}
}
