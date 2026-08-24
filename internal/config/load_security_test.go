package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/fileprivacy"
)

const configSecurityFixture = "[tiers.t1]\nmodel = \"ollama/private\"\n"

func assertConfigOwnerOnly(t *testing.T, path string) {
	t.Helper()
	file, err := fileprivacy.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ownerOnly, ownerErr := fileprivacy.IsOwnerOnly(file)
	closeErr := file.Close()
	if ownerErr != nil || closeErr != nil || !ownerOnly {
		t.Fatalf("config owner-only=%v ownerErr=%v closeErr=%v", ownerOnly, ownerErr, closeErr)
	}
}

func TestLoadFileMissingParentRemainsFirstRunAndCreatesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-created", FileName)
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Path != path || len(cfg.Tiers) != 0 {
		t.Fatalf("missing-parent config = %+v", cfg)
	}
	if _, err := os.Lstat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("LoadFile created its missing parent: %v", err)
	}
}

func TestLoadFileMigratesCurrentUserLegacyPermissionsBeforeParsing(t *testing.T) {
	path := write(t, configSecurityFixture)
	if err := makeLegacyBroadConfigForTest(path); err != nil {
		t.Skipf("cannot create a legacy permission fixture: %v", err)
	}

	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if tier, ok := cfg.Tier("t1"); !ok || tier.Target.ModelID != "private" {
		t.Fatalf("repaired config did not load: %+v", cfg.Tiers)
	}
	assertConfigOwnerOnly(t, path)
}

func TestLoadFileSecuresLegacyFileEvenWhenTOMLIsInvalid(t *testing.T) {
	path := write(t, "[auth.openai\nhelper = [\"must-not-run\"]\n")
	if err := makeLegacyBroadConfigForTest(path); err != nil {
		t.Skipf("cannot create a legacy permission fixture: %v", err)
	}
	if _, err := LoadFile(path); err == nil || !strings.Contains(err.Error(), "parsing") {
		t.Fatalf("invalid config load error = %v", err)
	}
	assertConfigOwnerOnly(t, path)
}

func TestLoadFileRejectsOversizeConfigBeforeParsing(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, bytes.Repeat([]byte{' '}, int(maxConfigBytes)+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(path); err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("oversize config error = %v", err)
	}
}

func TestLoadFileRejectsHardlinkAndSymlink(t *testing.T) {
	t.Run("hardlink", func(t *testing.T) {
		path := write(t, configSecurityFixture)
		if err := os.Link(path, filepath.Join(filepath.Dir(path), "alias.toml")); err != nil {
			t.Skipf("hard links unavailable: %v", err)
		}
		if _, err := LoadFile(path); err == nil || !strings.Contains(strings.ToLower(err.Error()), "hard link") {
			t.Fatalf("hard-linked config error = %v", err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target.toml")
		if err := os.WriteFile(target, []byte(configSecurityFixture), 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, FileName)
		if err := os.Symlink(target, path); err != nil {
			t.Skipf("symlinks unavailable without platform privilege: %v", err)
		}
		if _, err := LoadFile(path); err == nil || !strings.Contains(strings.ToLower(err.Error()), "regular file") {
			t.Fatalf("symlinked config error = %v", err)
		}
	})
}

func TestLoadFileRejectsSpecialObject(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(path); err == nil || !strings.Contains(strings.ToLower(err.Error()), "regular file") {
		t.Fatalf("directory config error = %v", err)
	}
}

func TestLoadFileRejectsSymlinkedConfigParent(t *testing.T) {
	root := t.TempDir()
	realParent := filepath.Join(root, "real")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realParent, FileName), []byte(configSecurityFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(root, "linked")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Skipf("directory symlinks unavailable without platform privilege: %v", err)
	}
	if _, err := LoadFile(filepath.Join(linkedParent, FileName)); err == nil || !strings.Contains(err.Error(), "not a real directory") {
		t.Fatalf("symlinked config parent error = %v", err)
	}
}

func TestLoadFileRejectsPathReplacementBeforeParsing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	replacement := filepath.Join(dir, "replacement.toml")
	displaced := filepath.Join(dir, "displaced.toml")
	if err := os.WriteFile(path, []byte("[auth.openai]\nhelper = [\"original-helper\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacement, []byte("[auth.openai]\nhelper = [\"replacement-helper\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loadFileBeforeFinalIdentityCheck = func() {
		loadFileBeforeFinalIdentityCheck = nil
		if err := os.Rename(path, displaced); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(replacement, path); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { loadFileBeforeFinalIdentityCheck = nil })

	if cfg, err := LoadFile(path); err == nil || cfg != nil || !strings.Contains(err.Error(), "no longer names") {
		t.Fatalf("path-replaced config cfg=%+v err=%v", cfg, err)
	}
}

func TestLoadFileRejectsSameFileMutationDuringParsing(t *testing.T) {
	path := write(t, "[auth.openai]\nhelper = [\"original-helper\"]\n")
	loadFileBeforeFinalIdentityCheck = func() {
		loadFileBeforeFinalIdentityCheck = nil
		if err := os.WriteFile(path, []byte("[auth.openai]\nhelper = [\"changed-helper\"]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { loadFileBeforeFinalIdentityCheck = nil })

	if cfg, err := LoadFile(path); err == nil || cfg != nil || !strings.Contains(err.Error(), "changed while it was parsed") {
		t.Fatalf("mutated config cfg=%+v err=%v", cfg, err)
	}
}
