package execution

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestAccountHomeIgnoresForgedAmbientHome(t *testing.T) {
	account, err := accountHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	forged := t.TempDir()
	t.Setenv("HOME", forged)

	gotAccount, err := accountHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if gotAccount != account {
		t.Fatalf("account home followed HOME: got %q, want %q", gotAccount, account)
	}
	ambient, err := ambientHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	resolvedForged, err := filepath.EvalSymlinks(forged)
	if err != nil {
		t.Fatal(err)
	}
	if ambient != resolvedForged {
		t.Fatalf("ambient home = %q, want %q", ambient, resolvedForged)
	}
	homes, err := protectedHomeDirectories()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{account, resolvedForged} {
		if !slices.Contains(homes, want) {
			t.Fatalf("protected homes %q omit %q", homes, want)
		}
	}
}

func TestMinimalHomeCoversHandlesNestedHomes(t *testing.T) {
	sep := string(filepath.Separator)
	outer := filepath.Clean(sep + filepath.Join("accounts"))
	inner := filepath.Join(outer, "user")
	peer := filepath.Clean(sep + filepath.Join("functional", "home"))

	for _, homes := range [][]string{{inner, outer, peer}, {outer, inner, peer}} {
		got := minimalHomeCovers(homes)
		if !slices.Equal(got, []string{outer, peer}) {
			t.Fatalf("minimal covers for %q = %q, want %q", homes, got, []string{outer, peer})
		}
	}
}

func TestSandboxCheckCacheUsesAccountHome(t *testing.T) {
	account, err := accountHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	path, err := checkCachePath()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(account, ".switchboard", "sandbox-check.json")
	if path != want {
		t.Fatalf("sandbox check path followed HOME: got %q, want %q", path, want)
	}
}

func TestForgedHomeDoesNotChangeGoHomeProtection(t *testing.T) {
	account, err := accountHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	forged := t.TempDir()
	t.Setenv("HOME", forged)
	ambient, err := ambientHomeDir()
	if err != nil {
		t.Fatal(err)
	}

	accountToolchain := filepath.Join(account, ".toolchains", "go")
	if !rootNeedsHomeReopen(accountToolchain) {
		t.Fatalf("account toolchain %q was not recognized beneath the protected account home", accountToolchain)
	}
	ambientToolchain := filepath.Join(ambient, ".toolchains", "go")
	if !rootNeedsHomeReopen(ambientToolchain) {
		t.Fatalf("ambient toolchain %q was not recognized beneath the protected ambient home", ambientToolchain)
	}
	for _, root := range []string{
		filepath.Join(account, ".ssh", "go"),
		filepath.Join(account, ".switchboard", "go"),
		filepath.Join(ambient, ".aws", "go"),
	} {
		if !overlapsProtectedCredentialPath(root) {
			t.Fatalf("credential-overlapping Go root %q was considered safe to reopen", root)
		}
	}
}

func TestReadableHomePathClosesParentAroundSymlinkedCredential(t *testing.T) {
	home := t.TempDir()
	cargo := filepath.Join(home, ".cargo")
	if err := os.Mkdir(cargo, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	credential := filepath.Join(cargo, "credentials")
	if err := os.Symlink(outside, credential); err != nil {
		t.Fatal(err)
	}
	if paths := readableHomePaths(home); slices.Contains(paths, cargo) {
		t.Fatalf("readable paths %q reopened .cargo around a credential symlink", paths)
	}
	if safeWritableHomeCache(home, ".cargo") {
		t.Fatal("writable cache accepted .cargo around a credential symlink")
	}
	targets, err := resolvedSymlinkedProtectedTargets(home)
	if err != nil {
		t.Fatal(err)
	}
	resolvedOutside, err := filepath.EvalSymlinks(outside)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(targets, resolvedOutside) {
		t.Fatalf("resolved credential targets %q omit %q", targets, resolvedOutside)
	}

	if err := os.Remove(credential); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credential, []byte("ordinary credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	if paths := readableHomePaths(home); !slices.Contains(paths, cargo) {
		t.Fatalf("ordinary masked credential unnecessarily removed .cargo from readable paths: %q", paths)
	}
	if paths := secretHomePaths(home); !slices.Contains(paths, credential) {
		t.Fatalf("ordinary credential path was not emitted for masking: %q", paths)
	}
}
