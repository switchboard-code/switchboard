//go:build windows

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/execution"
	"golang.org/x/sys/windows"
)

func TestWindowsExecScriptUsesCmdDialectAndRuns(t *testing.T) {
	registry, err := NewRegistry(t.TempDir(), execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	tool, ok := registry.Get("exec")
	if !ok {
		t.Fatal("no exec tool")
	}
	if schema := string(tool.Schema()); !strings.Contains(schema, "cmd.exe") || strings.Contains(schema, "/bin/sh") {
		t.Fatalf("Windows exec schema describes the wrong shell: %s", schema)
	}
	plan, err := tool.Plan(json.RawMessage(`{"script":"echo switchboard-windows-shell"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := Describe(plan.Request.Argv, plan.Request.Shell); !strings.HasPrefix(got, "cmd.exe /c ") {
		t.Fatalf("Describe() = %q", got)
	}
	result, err := plan.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || !strings.Contains(result.Content, "switchboard-windows-shell") {
		t.Fatalf("Windows script result = %#v", result)
	}
}

func TestWindowsRegistryDisplayUsesSlashPolicyGrammar(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "nested", "existing.go"), []byte("package nested\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry(root, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		root + `\src\nested\existing.go`:                             "src/nested/existing.go",
		root + `\src\nested\new.go`:                                  "src/nested/new.go",
		filepath.Join(registry.root, "src", "nested", "existing.go"): "src/nested/existing.go",
	} {
		if got := registry.display(path); got != want {
			t.Fatalf("display(%q) = %q, want %q", path, got, want)
		}
	}

	outside := filepath.Join(t.TempDir(), "outside.go")
	want, err := filepath.Rel(registry.root, outside)
	if err != nil {
		t.Fatal(err)
	}
	if got := registry.display(outside); got != filepath.ToSlash(want) {
		t.Fatalf("external display(%q) = %q, want unchanged relative escape %q", outside, got, filepath.ToSlash(want))
	}
}

func TestWindowsResolveKeepsCanonicalIdentityAndSafeDisplayAliases(t *testing.T) {
	root := t.TempDir()
	mixedCase := filepath.Join(root, "MixedCase.go")
	if err := os.WriteFile(mixedCase, []byte("package fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry(root, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("case-only spelling and folded lease", func(t *testing.T) {
		callerName := "mixedcase.go"
		callerPath := filepath.Join(root, callerName)
		callerInfo, err := os.Lstat(callerPath)
		if os.IsNotExist(err) {
			t.Skip("temporary volume is case-sensitive")
		}
		if err != nil {
			t.Fatal(err)
		}
		storedInfo, err := os.Lstat(mixedCase)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(callerInfo, storedInfo) {
			t.Skip("case-only spelling names a distinct file")
		}

		resolved, err := registry.resolve(callerName)
		if err != nil {
			t.Fatal(err)
		}
		canonical := filepath.Join(registry.root, filepath.Base(mixedCase))
		if resolved != canonical {
			t.Fatalf("resolve split case alias identity: got %q, want %q", resolved, canonical)
		}
		if got := registry.display(resolved); got != callerName {
			t.Fatalf("display rewrote caller case: got %q, want %q", got, callerName)
		}
		if mutationLockKey(resolved) != mutationLockKey(canonical) {
			t.Fatalf("case aliases received different mutation leases: %q and %q", resolved, canonical)
		}
	})

	t.Run("symlink target stays canonical", func(t *testing.T) {
		realDir := filepath.Join(root, "real")
		if err := os.Mkdir(realDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(realDir, "inside.go"), []byte("package real\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(root, "alias")
		if err := os.Symlink(realDir, alias); err != nil {
			t.Skipf("directory symlinks are unavailable: %v", err)
		}
		resolved, err := registry.resolve(filepath.Join("alias", "inside.go"))
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(registry.root, "real", "inside.go")
		if resolved != want {
			t.Fatalf("resolve retained symlink spelling: got %q, want %q", resolved, want)
		}

		outsideDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(outsideDir, "outside.go"), []byte("package outside\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		escape := filepath.Join(root, "escape")
		if err := os.Symlink(outsideDir, escape); err != nil {
			t.Fatal(err)
		}
		if _, err := registry.resolve(filepath.Join("escape", "outside.go")); !errors.Is(err, errOutsideWorkspace) {
			t.Fatalf("escaping symlink error = %v, want %v", err, errOutsideWorkspace)
		}
	})

	t.Run("short-name alias stays canonical", func(t *testing.T) {
		shortRoot := windowsShortPath(t, registry.root)
		if strings.EqualFold(filepath.Clean(shortRoot), filepath.Clean(registry.root)) {
			t.Skip("8.3 alias is unavailable for the temporary path")
		}
		resolved, err := registry.resolve(filepath.Join(shortRoot, filepath.Base(mixedCase)))
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(registry.root, filepath.Base(mixedCase))
		if resolved != want {
			t.Fatalf("resolve retained 8.3 spelling: got %q, want %q", resolved, want)
		}
	})

	t.Run("external path stays refused", func(t *testing.T) {
		outside := filepath.Join(t.TempDir(), "outside.go")
		if _, err := registry.resolve(outside); !errors.Is(err, errOutsideWorkspace) {
			t.Fatalf("resolve external path error = %v, want %v", err, errOutsideWorkspace)
		}
	})
}

func windowsShortPath(t *testing.T, path string) string {
	t.Helper()
	longPath, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	size, err := windows.GetShortPathName(longPath, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if size == 0 {
		t.Fatal("GetShortPathName returned an empty path")
	}
	buffer := make([]uint16, size)
	if _, err := windows.GetShortPathName(longPath, &buffer[0], uint32(len(buffer))); err != nil {
		t.Fatal(err)
	}
	return windows.UTF16ToString(buffer)
}
