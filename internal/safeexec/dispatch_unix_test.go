//go:build unix

package safeexec

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFilteredPathKeepsLaunchCheckoutOutOfEnvShebangDispatch(t *testing.T) {
	launch := t.TempDir()
	if err := os.Mkdir(filepath.Join(launch, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	launchBin := filepath.Join(launch, "bin")
	trustedBin := t.TempDir()
	if err := os.Mkdir(launchBin, 0o755); err != nil {
		t.Fatal(err)
	}
	maliciousMarker := filepath.Join(t.TempDir(), "malicious-ran")
	trustedMarker := filepath.Join(t.TempDir(), "trusted-ran")
	const interpreter = "switchboard-safeexec-interpreter"
	if err := os.WriteFile(filepath.Join(launchBin, interpreter), []byte(
		"#!/bin/sh\nprintf malicious > '"+maliciousMarker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trustedBin, interpreter), []byte(
		"#!/bin/sh\nprintf trusted > '"+trustedMarker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	dispatcher := filepath.Join(trustedBin, "switchboard-safeexec-dispatcher")
	if err := os.WriteFile(dispatcher, []byte("#!/usr/bin/env "+interpreter+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(launch)
	roots, err := WorkspaceAndCurrentAuthorityRoots(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executable, err := ResolvePathOutside(dispatcher, roots...)
	if err != nil {
		t.Fatal(err)
	}
	environ, err := FilterEnvironmentPath([]string{
		"PATH=" + launchBin + string(os.PathListSeparator) + trustedBin,
	}, roots...)
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := executable.Command()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = environ
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(maliciousMarker); !os.IsNotExist(err) {
		t.Fatalf("launch-checkout interpreter executed: %v", err)
	}
	if _, err := os.Stat(trustedMarker); err != nil {
		t.Fatalf("trusted interpreter did not execute: %v", err)
	}
}
