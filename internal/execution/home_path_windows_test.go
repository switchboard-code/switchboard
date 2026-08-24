//go:build windows

package execution

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAmbientHomeRejectsUNCBeforeCanonicalization(t *testing.T) {
	for _, path := range []string{`\\server\share\home`, `\\?\UNC\server\share\home`, `//server/share/home`} {
		if _, err := canonicalAmbientHomeDirectory(path); err == nil || !strings.Contains(err.Error(), "local Windows drive") {
			t.Fatalf("canonicalAmbientHomeDirectory(%q) = %v, want local-drive refusal", path, err)
		}
	}
}

func TestAmbientHomeRejectsEveryReparsePoint(t *testing.T) {
	target := t.TempDir()
	if err := os.Mkdir(filepath.Join(target, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "home-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("creating a Windows directory symlink requires developer mode or privilege: %v", err)
	}
	for _, candidate := range []string{link, filepath.Join(link, "nested")} {
		if _, err := canonicalAmbientHomeDirectory(candidate); err == nil || !strings.Contains(err.Error(), "without reparse traversal") {
			t.Fatalf("canonicalAmbientHomeDirectory(%q) = %v, want no-reparse refusal", candidate, err)
		}
	}
}

func TestAmbientHomeBindsLocalDirectory(t *testing.T) {
	home := t.TempDir()
	got, err := canonicalAmbientHomeDirectory(home)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs(home)
	if err != nil {
		t.Fatal(err)
	}
	gotInfo, gotErr := os.Stat(got)
	wantInfo, wantErr := os.Stat(want)
	if gotErr != nil || wantErr != nil || !os.SameFile(gotInfo, wantInfo) {
		t.Fatalf("bound ambient HOME = %q, want %q: got=%v want=%v", got, want, gotErr, wantErr)
	}
}
