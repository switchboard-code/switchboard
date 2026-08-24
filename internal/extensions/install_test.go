package extensions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAcquireInstallLockHonorsCancellation(t *testing.T) {
	rootPath := t.TempDir()
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.Mkdir("held.lock", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := securePrivateInstallDirectory(root, "held.lock"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	err = acquireInstallLock(ctx, root, "held.lock")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("lock cancellation = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cancelled lock wait took %s", elapsed)
	}
}

func TestDefaultInstallRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
		t.Setenv("HOMEDRIVE", filepath.VolumeName(home))
		t.Setenv("HOMEPATH", strings.TrimPrefix(home, filepath.VolumeName(home)))
	}
	got, err := DefaultInstallRoot()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".switchboard", "plugin-cache")
	if got != want {
		t.Fatalf("DefaultInstallRoot() = %q, want %q", got, want)
	}
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Fatalf("DefaultInstallRoot created the cache: %v", err)
	}
}

func TestInstallDeterministicIdempotentAndNonExecuting(t *testing.T) {
	sourceRoot := makePlugin(t, DialectCodex, `{"name":"safe-install"}`)
	mustWriteInstallFile(t, filepath.Join(sourceRoot, "regular.txt"), []byte("regular"), 0o666)
	mustWriteInstallFile(t, filepath.Join(sourceRoot, "install.sh"), []byte("#!/bin/sh\ntouch should-not-exist\n"), 0o777)
	mustWriteInstallFile(t, filepath.Join(sourceRoot, ".git", "config"), []byte("ignored metadata"), 0o600)
	plugin := discoverInstallPlugin(t, sourceRoot, ScopeUser, DialectCodex)
	cacheRoot := t.TempDir()

	first, err := Install(plugin, cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	firstInfo, err := os.Stat(first.RealPath)
	if err != nil {
		t.Fatal(err)
	}
	firstModTime := firstInfo.ModTime()
	if first.ID != plugin.ID || first.Digest != plugin.Digest || first.RealPath == plugin.RealPath {
		t.Fatalf("unexpected installed identity: %#v", first)
	}
	wantRoot := filepath.Join(canonicalInstallPath(t, cacheRoot), filepath.FromSlash(installDestination(plugin)), installPluginLeaf(plugin))
	if first.RealPath != wantRoot {
		t.Fatalf("installed root = %q, want %q", first.RealPath, wantRoot)
	}
	if _, err := os.Lstat(filepath.Join(first.RealPath, ".git")); !os.IsNotExist(err) {
		t.Fatalf(".git metadata was copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sourceRoot, "should-not-exist")); !os.IsNotExist(err) {
		t.Fatalf("plugin lifecycle script ran: %v", err)
	}

	// Excluded VCS metadata does not change the content address or cause a new
	// object to be published.
	mustWriteInstallFile(t, filepath.Join(sourceRoot, ".git", "config"), []byte("changed metadata"), 0o600)
	second, err := Install(plugin, cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	secondInfo, err := os.Stat(second.RealPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(firstInfo, secondInfo) {
		t.Fatal("idempotent install replaced the cached object")
	}
	if !secondInfo.ModTime().Equal(firstModTime) {
		t.Fatalf("idempotent install changed object timestamp: %s -> %s", firstModTime, secondInfo.ModTime())
	}
	assertNoInstallArtifacts(t, cacheRoot)
}

func TestInstallCanonicalizesPermissions(t *testing.T) {
	sourceRoot := makePlugin(t, DialectCodex, `{"name":"permissions"}`)
	mustWriteInstallFile(t, filepath.Join(sourceRoot, "nested", "regular"), []byte("regular"), 0o666)
	mustWriteInstallFile(t, filepath.Join(sourceRoot, "nested", "executable"), []byte("executable"), 0o777)
	plugin := discoverInstallPlugin(t, sourceRoot, ScopeLocal, DialectCodex)
	installed, err := Install(plugin, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	assertInstallMode(t, installed.RealPath, 0o700)
	assertInstallMode(t, filepath.Join(installed.RealPath, "nested"), 0o700)
	assertInstallMode(t, filepath.Join(installed.RealPath, "nested", "regular"), 0o600)
	assertInstallMode(t, filepath.Join(installed.RealPath, "nested", "executable"), 0o700)
	assertInstallMode(t, filepath.Join(installed.RealPath, ".codex-plugin", "plugin.json"), 0o600)
}

func TestInstallPreservesManifestlessClaudeIdentity(t *testing.T) {
	parent := t.TempDir()
	firstRoot := filepath.Join(parent, "first-skill")
	secondRoot := filepath.Join(parent, "second-skill")
	for _, root := range []string{firstRoot, secondRoot} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		mustWriteInstallFile(t, filepath.Join(root, "SKILL.md"), []byte("# Same content\n"), 0o600)
	}
	firstPlugin := discoverInstallPlugin(t, firstRoot, ScopeUser, DialectClaude)
	secondPlugin := discoverInstallPlugin(t, secondRoot, ScopeUser, DialectClaude)
	if firstPlugin.Digest != secondPlugin.Digest {
		t.Fatal("test requires identical content digests")
	}
	cacheRoot := t.TempDir()
	first, err := Install(firstPlugin, cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Install(secondPlugin, cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID || first.RealPath == second.RealPath {
		t.Fatalf("manifestless identities collided: %#v %#v", first, second)
	}
	if filepath.Base(first.RealPath) != first.Namespace || filepath.Base(second.RealPath) != second.Namespace {
		t.Fatalf("manifestless root names were not preserved: %q %q", first.RealPath, second.RealPath)
	}
}

func TestInstallActivationIsIndependentOfNativeEnablement(t *testing.T) {
	root := makePlugin(t, DialectCodex, `{"name":"independent"}`)
	available := discoverInstallPlugin(t, root, ScopeUser, DialectCodex)
	candidate, err := InstallActivation(available, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	installed := candidate.Plugin()
	if installed.RealPath == available.RealPath || installed.ID != available.ID || installed.Digest != available.Digest {
		t.Fatalf("installed activation identity = %#v", installed)
	}
	state, err := OpenStateFile(filepath.Join(t.TempDir(), StateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Enable(candidate, ""); err != nil {
		t.Fatal(err)
	}
	if got := state.Status(installed, ""); !got.Enabled {
		t.Fatalf("installed candidate was not enabled: %+v", got)
	}
}

func TestInstallActivationRecoversExactCachedCapabilityIdempotently(t *testing.T) {
	root := makePlugin(t, DialectCodex, `{"name":"recoverable"}`)
	source := discoverInstallPlugin(t, root, ScopeUser, DialectCodex)
	cacheRoot := t.TempDir()
	first, err := InstallActivation(source, cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	installed := first.Plugin()
	second, err := InstallActivation(installed, cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	recovered := second.Plugin()
	if recovered.ID != installed.ID || recovered.Digest != installed.Digest || recovered.RealPath != installed.RealPath {
		t.Fatalf("recovered capability changed cached identity:\nfirst:  %#v\nsecond: %#v", installed, recovered)
	}
}

func TestInstallRejectsSourceMutationAndBounds(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
		want   string
	}{
		{
			name: "content mutation",
			mutate: func(t *testing.T, root string) {
				mustWriteInstallFile(t, filepath.Join(root, "changed"), []byte("changed"), 0o600)
			},
			want: "digest changed",
		},
		{
			name: "manifest identity mutation",
			mutate: func(t *testing.T, root string) {
				mustWriteInstallFile(t, filepath.Join(root, ".codex-plugin", "plugin.json"), []byte(`{"name":"other"}`), 0o600)
			},
			want: "ID changed",
		},
		{
			name: "symlink",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				if err := os.Symlink(filepath.Join(root, ".codex-plugin", "plugin.json"), filepath.Join(root, "linked")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			},
			want: "symlink",
		},
		{
			name: "control path",
			mutate: func(t *testing.T, root string) {
				if runtime.GOOS == "windows" {
					t.Skip("Win32 filenames cannot contain the control-character fixture")
				}
				mustWriteInstallFile(t, filepath.Join(root, "bad\nname"), []byte("bad"), 0o600)
			},
			want: "control",
		},
		{
			name: "byte limit",
			mutate: func(t *testing.T, root string) {
				file, err := os.Create(filepath.Join(root, "oversized"))
				if err != nil {
					t.Fatal(err)
				}
				if err := file.Truncate(maxDigestBytes + 1); err != nil {
					file.Close()
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			},
			want: "digest limit",
		},
		{
			name: "depth limit",
			mutate: func(t *testing.T, root string) {
				current := root
				for range maxDigestDepth + 1 {
					current = filepath.Join(current, "d")
					if err := os.Mkdir(current, 0o700); err != nil {
						t.Fatal(err)
					}
				}
			},
			want: "depth limit",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sourceRoot := makePlugin(t, DialectCodex, `{"name":"mutating"}`)
			plugin := discoverInstallPlugin(t, sourceRoot, ScopeUser, DialectCodex)
			test.mutate(t, sourceRoot)
			cacheRoot := t.TempDir()
			_, err := Install(plugin, cacheRoot)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Install() error = %v, want %q", err, test.want)
			}
			objectPath := filepath.Join(canonicalInstallPath(t, cacheRoot), filepath.FromSlash(installDestination(plugin)))
			if _, statErr := os.Lstat(objectPath); !os.IsNotExist(statErr) {
				t.Fatalf("failed install published an object: %v", statErr)
			}
			assertNoInstallArtifacts(t, cacheRoot)
		})
	}
}

func TestInstallRejectsForgedTraversalAndControlIdentity(t *testing.T) {
	root := makePlugin(t, DialectCodex, `{"name":"identity"}`)
	plugin := discoverInstallPlugin(t, root, ScopeUser, DialectCodex)
	for _, mutate := range []func(*Plugin){
		func(plugin *Plugin) {
			plugin.Namespace = "../escape"
			plugin.ID = "codex:../escape"
		},
		func(plugin *Plugin) { plugin.ID += "\n" },
		func(plugin *Plugin) { plugin.Root += "\n" },
	} {
		forged := plugin
		mutate(&forged)
		if _, err := Install(forged, t.TempDir()); err == nil {
			t.Fatalf("Install accepted forged plugin: %#v", forged)
		}
	}
	badCache := filepath.Join(t.TempDir(), "bad\ncache")
	if _, err := Install(plugin, badCache); err == nil {
		t.Fatal("Install accepted a control character in cache root")
	}
	if _, err := os.Stat(badCache); runtime.GOOS != "windows" && !os.IsNotExist(err) {
		t.Fatalf("invalid cache root was created: %v", err)
	}
}

func TestInstallNeverOverwritesMismatchedDestination(t *testing.T) {
	setups := []struct {
		name  string
		setup func(*testing.T, string) func(*testing.T)
	}{
		{
			name: "empty directory",
			setup: func(t *testing.T, destination string) func(*testing.T) {
				if err := os.MkdirAll(destination, 0o700); err != nil {
					t.Fatal(err)
				}
				return func(t *testing.T) {
					entries, err := os.ReadDir(destination)
					if err != nil || len(entries) != 0 {
						t.Fatalf("empty destination changed: %v %#v", err, entries)
					}
				}
			},
		},
		{
			name: "file",
			setup: func(t *testing.T, destination string) func(*testing.T) {
				if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
					t.Fatal(err)
				}
				mustWriteInstallFile(t, destination, []byte("sentinel"), 0o600)
				return func(t *testing.T) {
					raw, err := os.ReadFile(destination)
					if err != nil || string(raw) != "sentinel" {
						t.Fatalf("destination file changed: %v %q", err, raw)
					}
				}
			},
		},
		{
			name: "nonempty directory",
			setup: func(t *testing.T, destination string) func(*testing.T) {
				mustWriteInstallFile(t, filepath.Join(destination, "sentinel"), []byte("sentinel"), 0o600)
				return func(t *testing.T) {
					raw, err := os.ReadFile(filepath.Join(destination, "sentinel"))
					if err != nil || string(raw) != "sentinel" {
						t.Fatalf("destination directory changed: %v %q", err, raw)
					}
				}
			},
		},
		{
			name: "symlink",
			setup: func(t *testing.T, destination string) func(*testing.T) {
				if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
					t.Fatal(err)
				}
				outside := t.TempDir()
				mustWriteInstallFile(t, filepath.Join(outside, "sentinel"), []byte("outside"), 0o600)
				if err := os.Symlink(outside, destination); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				return func(t *testing.T) {
					info, err := os.Lstat(destination)
					if err != nil || info.Mode()&os.ModeSymlink == 0 {
						t.Fatalf("destination symlink changed: %v %#v", err, info)
					}
					raw, err := os.ReadFile(filepath.Join(outside, "sentinel"))
					if err != nil || string(raw) != "outside" {
						t.Fatalf("outside target changed: %v %q", err, raw)
					}
				}
			},
		},
	}

	for _, setup := range setups {
		t.Run(setup.name, func(t *testing.T) {
			sourceRoot := makePlugin(t, DialectCodex, `{"name":"conflict"}`)
			plugin := discoverInstallPlugin(t, sourceRoot, ScopeUser, DialectCodex)
			cacheRoot := t.TempDir()
			destination := filepath.Join(canonicalInstallPath(t, cacheRoot), filepath.FromSlash(installDestination(plugin)))
			assertUnchanged := setup.setup(t, destination)
			if _, err := Install(plugin, cacheRoot); err == nil {
				t.Fatal("Install overwrote or accepted a mismatched destination")
			}
			assertUnchanged(t)
		})
	}
}

func TestInstallRejectsCorruptedCachedTree(t *testing.T) {
	root := makePlugin(t, DialectCodex, `{"name":"corrupt"}`)
	mustWriteInstallFile(t, filepath.Join(root, "data"), []byte("original"), 0o600)
	plugin := discoverInstallPlugin(t, root, ScopeUser, DialectCodex)
	cacheRoot := t.TempDir()
	installed, err := Install(plugin, cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	corruptPath := filepath.Join(installed.RealPath, "data")
	mustWriteInstallFile(t, corruptPath, []byte("corrupted"), 0o600)
	if _, err := Install(plugin, cacheRoot); err == nil || !strings.Contains(err.Error(), "digest changed") {
		t.Fatalf("Install() error = %v, want cached digest conflict", err)
	}
	raw, err := os.ReadFile(corruptPath)
	if err != nil || string(raw) != "corrupted" {
		t.Fatalf("corrupted cache was repaired or replaced: %v %q", err, raw)
	}
	if runtime.GOOS == "windows" {
		// Windows has no portable Unix mode-bit permission class. Its DACL
		// tampering case is covered in install_security_windows_test.go.
		return
	}

	// Even permission-only tampering is a conflict although executable-vs-
	// regular is the only permission class covered by the content digest.
	mustWriteInstallFile(t, corruptPath, []byte("original"), 0o600)
	if err := os.Chmod(corruptPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(plugin, cacheRoot); err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("Install() error = %v, want cached mode conflict", err)
	}
	assertInstallMode(t, corruptPath, 0o644)
}

func TestInstallConcurrentSamePlugin(t *testing.T) {
	root := makePlugin(t, DialectCodex, `{"name":"concurrent"}`)
	for index := 0; index < 64; index++ {
		mustWriteInstallFile(t, filepath.Join(root, "files", string(rune('a'+index%26)), string(rune('A'+index/26))), []byte("payload"), 0o600)
	}
	plugin := discoverInstallPlugin(t, root, ScopeUser, DialectCodex)
	cacheRoot := t.TempDir()
	const workers = 24
	start := make(chan struct{})
	results := make(chan Plugin, workers)
	errorsChannel := make(chan error, workers)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			installed, err := Install(plugin, cacheRoot)
			if err != nil {
				errorsChannel <- err
				return
			}
			results <- installed
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	close(errorsChannel)
	for err := range errorsChannel {
		t.Errorf("concurrent Install: %v", err)
	}
	var first Plugin
	for installed := range results {
		if first.ID == "" {
			first = installed
			continue
		}
		if installed.RealPath != first.RealPath || installed.Digest != first.Digest {
			t.Errorf("concurrent installs diverged: %#v vs %#v", first, installed)
		}
	}
	if first.ID == "" {
		t.Fatal("no concurrent install succeeded")
	}
	assertNoInstallArtifacts(t, cacheRoot)
}

func TestInstallCopyUsesEntryAndDepthBudgets(t *testing.T) {
	sourcePath := t.TempDir()
	destinationPath := t.TempDir()
	mustWriteInstallFile(t, filepath.Join(sourcePath, "file"), []byte("x"), 0o600)
	source, err := os.OpenRoot(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	destination, err := os.OpenRoot(destinationPath)
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	if err := copyInstallDirectory(source, destination, "", 0, &digestBudget{entries: maxDigestEntries}); err == nil || !strings.Contains(err.Error(), "entries") {
		t.Fatalf("entry budget error = %v", err)
	}
	if err := copyInstallDirectory(source, destination, "", maxDigestDepth, &digestBudget{}); err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("depth budget error = %v", err)
	}
}

func TestInstallRejectsWritableCacheRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows permission bits do not model Unix group/world writability")
	}
	root := makePlugin(t, DialectCodex, `{"name":"cache-mode"}`)
	plugin := discoverInstallPlugin(t, root, ScopeUser, DialectCodex)
	cacheRoot := t.TempDir()
	if err := os.Chmod(cacheRoot, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(plugin, cacheRoot); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("Install() error = %v, want writable-cache rejection", err)
	}
}

func TestInstallRejectsUnsafeCacheNamespace(t *testing.T) {
	root := makePlugin(t, DialectCodex, `{"name":"cache-namespace"}`)
	plugin := discoverInstallPlugin(t, root, ScopeUser, DialectCodex)
	t.Run("loose permissions", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows permission bits do not model Unix group/world writability")
		}
		cache := t.TempDir()
		namespace := filepath.Join(cache, installCacheVersion)
		if err := os.Mkdir(namespace, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(namespace, 0o777); err != nil {
			t.Fatal(err)
		}
		if _, err := Install(plugin, cache); err == nil || !strings.Contains(err.Error(), "mode") {
			t.Fatalf("Install() error = %v, want loose-namespace rejection", err)
		}
		assertInstallMode(t, namespace, 0o777)
	})
	t.Run("symlink namespace", func(t *testing.T) {
		cache := t.TempDir()
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(cache, installCacheVersion)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := Install(plugin, cache); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("Install() error = %v, want symlink-namespace rejection", err)
		}
		entries, err := os.ReadDir(outside)
		if err != nil || len(entries) != 0 {
			t.Fatalf("symlink target was modified: %v %#v", err, entries)
		}
	})
	t.Run("symlink cache root", func(t *testing.T) {
		outside := t.TempDir()
		alias := filepath.Join(t.TempDir(), "cache")
		if err := os.Symlink(outside, alias); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := Install(plugin, alias); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Fatalf("Install() error = %v, want symlink-cache rejection", err)
		}
	})
}

func TestInstallRejectsReplacedSourceRoot(t *testing.T) {
	parent := t.TempDir()
	sourceRoot := filepath.Join(parent, "source")
	mustWriteInstallFile(t, filepath.Join(sourceRoot, ".codex-plugin", "plugin.json"), []byte(`{"name":"replaced"}`), 0o600)
	mustWriteInstallFile(t, filepath.Join(sourceRoot, "data"), []byte("original"), 0o600)
	plugin := discoverInstallPlugin(t, sourceRoot, ScopeUser, DialectCodex)
	oldRoot := filepath.Join(parent, "old-source")
	if err := os.Rename(sourceRoot, oldRoot); err != nil {
		t.Fatal(err)
	}
	mustWriteInstallFile(t, filepath.Join(sourceRoot, ".codex-plugin", "plugin.json"), []byte(`{"name":"replaced"}`), 0o600)
	mustWriteInstallFile(t, filepath.Join(sourceRoot, "data"), []byte("replacement"), 0o600)
	if _, err := Install(plugin, t.TempDir()); err == nil || !strings.Contains(err.Error(), "digest changed") {
		t.Fatalf("Install() error = %v, want replaced-root rejection", err)
	}
}

func TestPublishInstallDoesNotReplaceExistingDirectory(t *testing.T) {
	cachePath := t.TempDir()
	for _, name := range []string{"source", "destination"} {
		if err := os.Mkdir(filepath.Join(cachePath, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	directory, err := os.Open(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if err := publishInstall(directory, root, "source", "destination"); err == nil {
		t.Fatal("no-replace publication overwrote an existing empty directory")
	}
	for _, name := range []string{"source", "destination"} {
		info, err := os.Stat(filepath.Join(cachePath, name))
		if err != nil || !info.IsDir() {
			t.Fatalf("%s changed after rejected publication: %v %#v", name, err, info)
		}
	}
}

func discoverInstallPlugin(t *testing.T, root string, scope Scope, dialect Dialect) Plugin {
	t.Helper()
	result := Discover([]Candidate{{Root: root, Scope: scope, Dialect: dialect}})
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.Severity == SeverityError {
			t.Fatalf("discovering install source: %#v", result)
		}
	}
	if len(result.Plugins) != 1 {
		t.Fatalf("discovering install source: %#v", result)
	}
	return result.Plugins[0]
}

func mustWriteInstallFile(t *testing.T, name string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(name, mode); err != nil {
		t.Fatal(err)
	}
}

func canonicalInstallPath(t *testing.T, root string) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(path)
}

func assertInstallMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	assertInstallPlatformProtection(t, path, want)
}

func assertNoInstallArtifacts(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".install-") || strings.HasSuffix(name, ".lock") {
			return errors.New("temporary install artifact remains at " + path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestInstallIdempotencyDoesNotTouchObjectTime(t *testing.T) {
	// Keep this assertion separate from filesystem operations above so a coarse
	// timestamp filesystem cannot hide a replacement.
	root := makePlugin(t, DialectCodex, `{"name":"timestamp"}`)
	plugin := discoverInstallPlugin(t, root, ScopeUser, DialectCodex)
	cache := t.TempDir()
	first, err := Install(plugin, cache)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(first.RealPath)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(first.RealPath, want, want); err != nil {
		t.Fatal(err)
	}
	second, err := Install(plugin, cache)
	if err != nil {
		t.Fatal(err)
	}
	secondInfo, err := os.Stat(second.RealPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(info, secondInfo) || !secondInfo.ModTime().Equal(want) {
		t.Fatalf("idempotent install touched cached root: inode=%v modtime=%s", os.SameFile(info, secondInfo), secondInfo.ModTime())
	}
}
