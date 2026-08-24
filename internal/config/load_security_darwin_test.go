//go:build darwin

package config

import (
	"os/exec"
	"testing"
)

func TestLoadFileMigratesDarwinExtendedACLBeforeParsing(t *testing.T) {
	path := write(t, configSecurityFixture)
	if output, err := exec.Command("chmod", "+a", "everyone allow read", path).CombinedOutput(); err != nil {
		t.Fatalf("adding Darwin ACL: %v: %s", err, output)
	}

	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Tier("t1"); !ok {
		t.Fatal("ACL-migrated config did not parse")
	}
	assertConfigOwnerOnly(t, path)
}
