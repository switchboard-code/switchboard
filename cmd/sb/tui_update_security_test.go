package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/config"
)

func TestStartupUpdateRequiresExplicitAutomaticInstallOptIn(t *testing.T) {
	cfg, err := config.LoadFile(filepath.Join(t.TempDir(), config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UpdateAuto {
		t.Fatal("missing configuration enabled automatic installation")
	}

	applied := 0
	rt := startupUpdateRuntime{
		current: func() string { return "v1.21.0" },
		fetch: func(context.Context, string) (*ghRelease, error) {
			return &ghRelease{TagName: "v1.22.0"}, nil
		},
		apply: func(context.Context, *ghRelease) error {
			applied++
			return nil
		},
	}
	msg := startupUpdateWith(cfg, rt)(context.Background())
	if got, ok := msg.(updateCheckMsg); !ok || got.latest != "v1.22.0" {
		t.Fatalf("default startup result = %#v, want an update notice", msg)
	}
	if applied != 0 {
		t.Fatalf("default startup performed %d executable replacements", applied)
	}

	cfg.UpdateAuto = true
	msg = startupUpdateWith(cfg, rt)(context.Background())
	if got, ok := msg.(updateAppliedMsg); !ok || got.version != "v1.22.0" {
		t.Fatalf("opted-in startup result = %#v, want applied update", msg)
	}
	if applied != 1 {
		t.Fatalf("explicit opt-in performed %d executable replacements, want 1", applied)
	}
}

func TestUpdateSettingsAreAtomicAcrossSaveFailureAndSuccess(t *testing.T) {
	m := testModel(t)
	m.app.config.UpdateChannel = "stable"
	m.app.config.UpdateAuto = false
	m.app.config.Path = t.TempDir() // a directory cannot be replaced by the config file

	for _, args := range []string{"channel beta", "auto on"} {
		msg, ok := updateSettings(m, args)().(noticeMsg)
		if !ok || msg.level != "error" || !strings.Contains(msg.text, "nothing changed") {
			t.Fatalf("%q failure notice = %#v", args, msg)
		}
		if m.app.config.UpdateChannel != "stable" || m.app.config.UpdateAuto {
			t.Fatalf("%q changed live update settings: channel=%q auto=%v", args,
				m.app.config.UpdateChannel, m.app.config.UpdateAuto)
		}
	}

	path := filepath.Join(t.TempDir(), config.FileName)
	m.app.config.Path = path
	for _, args := range []string{"channel beta", "auto on"} {
		msg, ok := updateSettings(m, args)().(noticeMsg)
		if !ok || msg.level == "error" {
			t.Fatalf("%q success notice = %#v", args, msg)
		}
	}
	if m.app.config.UpdateChannel != "beta" || !m.app.config.UpdateAuto {
		t.Fatalf("successful settings not adopted: channel=%q auto=%v",
			m.app.config.UpdateChannel, m.app.config.UpdateAuto)
	}
	saved, err := config.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.UpdateChannel != "beta" || !saved.UpdateAuto {
		t.Fatalf("successful settings not persisted: channel=%q auto=%v", saved.UpdateChannel, saved.UpdateAuto)
	}
}

func TestUpdateAssetNameRequiresCanonicalSemver(t *testing.T) {
	valid := []string{
		"v0.0.0",
		"v1.21.0",
		"v1.21.0-beta.1",
		"v1.21.0-rc-one+build.5",
	}
	for _, tag := range valid {
		name, err := updateAssetName(tag, "darwin", "arm64")
		if err != nil {
			t.Errorf("valid tag %q: %v", tag, err)
			continue
		}
		if want := "sb_" + strings.TrimPrefix(tag, "v") + "_darwin_arm64.tar.gz"; name != want {
			t.Errorf("asset name for %q = %q, want %q", tag, name, want)
		}
	}

	invalid := []string{
		"", "v", "1.21.0", " v1.21.0", "v1.21.0 ",
		"v1", "v1.2", "v1.2.3.4", "v01.2.3", "v1.02.3", "v1.2.03",
		"v1.2.3-", "v1.2.3-01", "v1.2.3-beta..1", "v1.2.3+",
		"v1.2.3+build..1", "v1.2.3/../../escape", "v1.2.3-β",
		"v18446744073709551616.0.0",
		"v1.2.3+" + strings.Repeat("a", 129),
	}
	for _, tag := range invalid {
		if name, err := updateAssetName(tag, "linux", "amd64"); err == nil {
			t.Errorf("invalid tag %q produced %q", tag, name)
		}
	}
	for _, platform := range [][2]string{{"linux/../../", "amd64"}, {"linux", "amd64 x"}, {"", "amd64"}} {
		if _, err := updateAssetName("v1.21.0", platform[0], platform[1]); err == nil {
			t.Errorf("unsafe platform %q/%q was accepted", platform[0], platform[1])
		}
	}
}

func TestNewerVersionUsesTheCanonicalReleaseContract(t *testing.T) {
	tests := []struct {
		candidate string
		current   string
		want      bool
	}{
		{candidate: "v1.21.1+linux.1", current: "v1.21.0", want: true},
		{candidate: "v1.22.0", current: "v1.21.0+darwin.1", want: true},
		// Build metadata does not participate in SemVer precedence.
		{candidate: "v1.21.0+build.2", current: "v1.21.0+build.1", want: false},
		{candidate: "v1.21.0-rc.2+build.9", current: "v1.21.0-rc.1+build.10", want: true},
		{candidate: "v18446744073709551615.0.0", current: "v18446744073709551614.99.99", want: true},
		// Numeric prerelease identifiers are unbounded by SemVer and must not
		// overflow into the alphanumeric comparison branch.
		{candidate: "v1.21.0-1000000000000000000000", current: "v1.21.0-999999999999999999999", want: true},
		{candidate: "v1.21.0-999999999999999999999", current: "v1.21.0-1000000000000000000000", want: false},
		{candidate: "v1.21.0-alpha", current: "v1.21.0-999999999999999999999", want: true},
		// Forms the release workflow refuses are not silently normalized by the
		// update chooser either.
		{candidate: "1.22.0", current: "v1.21.0", want: false},
		{candidate: "v1.22", current: "v1.21.0", want: false},
		{candidate: "v01.22.0", current: "v1.21.0", want: false},
		{candidate: "v1.22.0-01", current: "v1.21.0", want: false},
		{candidate: "v18446744073709551616.0.0", current: "v1.21.0", want: false},
	}
	for _, test := range tests {
		if got := newerVersion(test.candidate, test.current); got != test.want {
			t.Errorf("newerVersion(%q, %q) = %v, want %v", test.candidate, test.current, got, test.want)
		}
	}
}

func TestNewerVersionPrecedenceProperties(t *testing.T) {
	versions := []string{
		"v0.0.0",
		"v1.21.0-0",
		"v1.21.0-beta.2",
		"v1.21.0-rc.1",
		"v1.21.0",
		"v1.22.0",
		"v18446744073709551615.0.0",
	}
	for _, version := range versions {
		if _, ok := canonicalReleaseVersion(version); !ok {
			t.Fatalf("test version %q is not canonical", version)
		}
		if parsed, ok := parseSemver(version); !ok {
			t.Fatalf("canonical version %q did not parse: %+v", version, parsed)
		}
		if newerVersion(version+"+left", version+"+right") {
			t.Errorf("build metadata changed precedence for %q", version)
		}
	}
	for _, left := range versions {
		for _, right := range versions {
			if newerVersion(left, right) && newerVersion(right, left) {
				t.Errorf("precedence is not antisymmetric for %q and %q", left, right)
			}
			if got, want := newerVersion(left+"+candidate", right+"+current"), newerVersion(left, right); got != want {
				t.Errorf("build metadata changed newerVersion(%q, %q) from %v to %v", left, right, want, got)
			}
		}
	}
}

func TestUpdateAssetURLsRequireOneExactPair(t *testing.T) {
	const asset = "sb_1.21.0_linux_amd64.tar.gz"
	good := &ghRelease{TagName: "v1.21.0", Assets: []ghAsset{
		{Name: asset, URL: "https://example.test/archive"},
		{Name: "checksums.txt", URL: "https://example.test/sums"},
		{Name: "notes.txt", URL: "https://example.test/notes"},
	}}
	archiveURL, sumsURL, err := updateAssetURLs(good, asset)
	if err != nil || archiveURL != "https://example.test/archive" || sumsURL != "https://example.test/sums" {
		t.Fatalf("exact assets = %q, %q, %v", archiveURL, sumsURL, err)
	}

	for _, test := range []struct {
		name   string
		assets []ghAsset
	}{
		{name: "missing archive", assets: []ghAsset{{Name: "checksums.txt", URL: "s"}}},
		{name: "missing sums", assets: []ghAsset{{Name: asset, URL: "a"}}},
		{name: "duplicate archive", assets: []ghAsset{{Name: asset, URL: "a"}, {Name: asset, URL: "b"}, {Name: "checksums.txt", URL: "s"}}},
		{name: "duplicate sums", assets: []ghAsset{{Name: asset, URL: "a"}, {Name: "checksums.txt", URL: "s"}, {Name: "checksums.txt", URL: "t"}}},
		{name: "empty archive url", assets: []ghAsset{{Name: asset}, {Name: "checksums.txt", URL: "s"}}},
		{name: "empty sums url", assets: []ghAsset{{Name: asset, URL: "a"}, {Name: "checksums.txt"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := updateAssetURLs(&ghRelease{TagName: "v1.21.0", Assets: test.assets}, asset); err == nil {
				t.Fatal("invalid asset inventory was accepted")
			}
		})
	}
}

func TestDownloadEnforcesTheByteCap(t *testing.T) {
	const limit = 32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/exact":
			_, _ = w.Write(bytes.Repeat([]byte("x"), limit))
		case "/length":
			_, _ = w.Write(bytes.Repeat([]byte("x"), limit+1))
		case "/chunked":
			w.Header().Set("Trailer", "X-Complete")
			w.WriteHeader(http.StatusOK)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			_, _ = w.Write(bytes.Repeat([]byte("x"), limit+1))
		case "/error":
			http.Error(w, "no", http.StatusBadGateway)
		}
	}))
	defer server.Close()

	got, err := download(context.Background(), server.URL+"/exact", limit)
	if err != nil || len(got) != limit {
		t.Fatalf("exact-cap download = %d bytes, %v", len(got), err)
	}
	for _, path := range []string{"/length", "/chunked"} {
		if _, err := download(context.Background(), server.URL+path, limit); err == nil || !strings.Contains(err.Error(), "limit") {
			t.Errorf("over-cap %s = %v", path, err)
		}
	}
	if _, err := download(context.Background(), server.URL+"/error", limit); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("status error = %v", err)
	}
	if _, err := download(context.Background(), server.URL+"/exact", -1); err == nil {
		t.Fatal("negative cap was accepted")
	}
}

func TestChecksumForRequiresExactSHA256SumGrammar(t *testing.T) {
	name := "sb_1.21.0_linux_amd64.tar.gz"
	sum := strings.Repeat("a", 64)
	for _, line := range []string{sum + "  " + name + "\n", strings.ToUpper(sum) + " *" + name + "\r\n"} {
		got, err := checksumFor([]byte(line), name)
		if err != nil || got != sum {
			t.Errorf("valid checksum %q = %q, %v", line, got, err)
		}
	}

	invalid := []string{
		strings.Repeat("a", 63) + "  " + name + "\n",
		strings.Repeat("a", 65) + "  " + name + "\n",
		strings.Repeat("g", 64) + "  " + name + "\n",
		sum + " " + name + "\n",
		sum + "   " + name + "\n",
		sum + "  " + name + " extra\n",
		sum + "  " + name + "\n" + sum + " *" + name + "\n",
		sum + "  another.tar.gz\n",
	}
	for _, input := range invalid {
		if _, err := checksumFor([]byte(input), name); err == nil {
			t.Errorf("invalid checksum input was accepted: %q", input)
		}
	}
	if _, err := checksumFor([]byte(sum+"  "+name+"\n"), "../"+name); err == nil {
		t.Fatal("unsafe checksum lookup name was accepted")
	}
}

func TestExtractUpdateBinaryRequiresOneExactBoundedMember(t *testing.T) {
	valid := makeUpdateArchive(t, []updateTarEntry{{name: "sb", body: []byte("new binary")}}, nil)
	got, err := extractUpdateBinary(valid, "sb", 64)
	if err != nil || string(got) != "new binary" {
		t.Fatalf("valid archive = %q, %v", got, err)
	}

	tests := []struct {
		name    string
		archive func(*testing.T) []byte
		cap     int64
	}{
		{name: "wrong platform member", archive: func(t *testing.T) []byte {
			return makeUpdateArchive(t, []updateTarEntry{{name: "sb.exe", body: []byte("binary")}}, nil)
		}, cap: 64},
		{name: "nested member", archive: func(t *testing.T) []byte {
			return makeUpdateArchive(t, []updateTarEntry{{name: "bin/sb", body: []byte("binary")}}, nil)
		}, cap: 64},
		{name: "duplicate member", archive: func(t *testing.T) []byte {
			return makeUpdateArchive(t, []updateTarEntry{{name: "sb", body: []byte("one")}, {name: "sb", body: []byte("two")}}, nil)
		}, cap: 64},
		{name: "extra member", archive: func(t *testing.T) []byte {
			return makeUpdateArchive(t, []updateTarEntry{{name: "sb", body: []byte("one")}, {name: "README", body: []byte("two")}}, nil)
		}, cap: 64},
		{name: "symlink", archive: func(t *testing.T) []byte {
			return makeUpdateArchive(t, []updateTarEntry{{name: "sb", typeflag: tar.TypeSymlink, linkname: "outside"}}, nil)
		}, cap: 64},
		{name: "directory", archive: func(t *testing.T) []byte {
			return makeUpdateArchive(t, []updateTarEntry{{name: "sb", typeflag: tar.TypeDir}}, nil)
		}, cap: 64},
		{name: "empty", archive: func(t *testing.T) []byte {
			return makeUpdateArchive(t, []updateTarEntry{{name: "sb"}}, nil)
		}, cap: 64},
		{name: "decompressed cap", archive: func(t *testing.T) []byte {
			return makeUpdateArchive(t, []updateTarEntry{{name: "sb", body: bytes.Repeat([]byte("x"), 65)}}, nil)
		}, cap: 64},
		{name: "nonzero tar tail", archive: func(t *testing.T) []byte {
			return makeUpdateArchive(t, []updateTarEntry{{name: "sb", body: []byte("binary")}}, []byte("hidden"))
		}, cap: 64},
		{name: "second gzip stream", archive: func(t *testing.T) []byte {
			first := makeUpdateArchive(t, []updateTarEntry{{name: "sb", body: []byte("binary")}}, nil)
			second := makeUpdateArchive(t, []updateTarEntry{{name: "sb", body: []byte("other")}}, nil)
			return append(first, second...)
		}, cap: 64},
		{name: "raw trailing bytes", archive: func(t *testing.T) []byte {
			return append(makeUpdateArchive(t, []updateTarEntry{{name: "sb", body: []byte("binary")}}, nil), []byte("tail")...)
		}, cap: 64},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := extractUpdateBinary(test.archive(t), "sb", test.cap); err == nil {
				t.Fatal("invalid archive was accepted")
			}
		})
	}
}

func TestSelfUpdateExecutableDownloadsVerifiesAndPublishes(t *testing.T) {
	member := "sb"
	if runtime.GOOS == "windows" {
		member = "sb.exe"
	}
	archive := makeUpdateArchive(t, []updateTarEntry{{name: member, body: []byte("new binary")}}, nil)
	assetName, err := updateAssetName("v1.21.0", runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(archive)
	checksums := hex.EncodeToString(sum[:]) + "  " + assetName + "\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/checksums.txt":
			_, _ = w.Write([]byte(checksums))
		case "/archive":
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	exe := filepath.Join(dir, member)
	if err := os.WriteFile(exe, []byte("old binary"), 0o701); err != nil {
		t.Fatal(err)
	}
	rel := &ghRelease{TagName: "v1.21.0", Assets: []ghAsset{
		{Name: assetName, URL: server.URL + "/archive"},
		{Name: "checksums.txt", URL: server.URL + "/checksums.txt"},
	}}
	err = selfUpdateExecutable(context.Background(), rel, exe, updateRuntime{
		goos: runtime.GOOS, goarch: runtime.GOARCH, fetch: download, replace: installUpdateBinary,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(exe)
	if err != nil || string(got) != "new binary" {
		t.Fatalf("installed binary = %q, %v", got, err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(exe)
		if err != nil || info.Mode().Perm() != 0o701 {
			t.Fatalf("installed mode = %v, %v", info.Mode().Perm(), err)
		}
	} else if old, err := os.ReadFile(exe + ".old"); err != nil || string(old) != "old binary" {
		t.Fatalf("Windows rollback copy = %q, %v", old, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".sb-update-") {
			t.Fatalf("staged update was retained: %s", entry.Name())
		}
	}
}

func TestSelfUpdateExecutableRefusesBeforePublication(t *testing.T) {
	fetches, replacements := 0, 0
	rt := updateRuntime{
		goos: "linux", goarch: "amd64",
		fetch: func(context.Context, string, int64) ([]byte, error) {
			fetches++
			return nil, errors.New("must not fetch")
		},
		replace: func(string, []byte) error {
			replacements++
			return nil
		},
	}
	if err := selfUpdateExecutable(context.Background(), &ghRelease{TagName: "v1.21.0"}, "/usr/bin/sb", rt); err == nil || !strings.Contains(err.Error(), "package manager") {
		t.Fatalf("package-manager refusal = %v", err)
	}
	if fetches != 0 || replacements != 0 {
		t.Fatalf("refusal crossed boundary: fetches=%d replacements=%d", fetches, replacements)
	}

	archive := makeUpdateArchive(t, []updateTarEntry{{name: "sb", body: []byte("new")}}, nil)
	asset, _ := updateAssetName("v1.21.0", "linux", "amd64")
	calls := 0
	rt.fetch = func(context.Context, string, int64) ([]byte, error) {
		calls++
		if calls == 1 {
			return []byte(strings.Repeat("0", 64) + "  " + asset + "\n"), nil
		}
		return archive, nil
	}
	exe := filepath.Join(t.TempDir(), "sb")
	if err := os.WriteFile(exe, []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}
	rel := &ghRelease{TagName: "v1.21.0", Assets: []ghAsset{{Name: asset, URL: "asset"}, {Name: "checksums.txt", URL: "sums"}}}
	if err := selfUpdateExecutable(context.Background(), rel, exe, rt); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("checksum refusal = %v", err)
	}
	if replacements != 0 {
		t.Fatal("checksum mismatch reached replacement")
	}
	if got, _ := os.ReadFile(exe); string(got) != "old" {
		t.Fatalf("checksum mismatch changed executable to %q", got)
	}
}

func TestReplaceExecutableWithBackupRollsBack(t *testing.T) {
	t.Run("publication failure restores current executable", func(t *testing.T) {
		dir := t.TempDir()
		exe, staged := filepath.Join(dir, "sb.exe"), filepath.Join(dir, "staged.exe")
		mustWriteUpdateTestFile(t, exe, "old")
		mustWriteUpdateTestFile(t, staged, "new")
		calls := 0
		injected := errors.New("injected publication failure")
		err := replaceExecutableWithBackup(exe, staged, func(from, to string) error {
			calls++
			if calls == 2 {
				return injected
			}
			return os.Rename(from, to)
		})
		if !errors.Is(err, injected) || calls != 3 {
			t.Fatalf("rollback = calls %d, err %v", calls, err)
		}
		if got, _ := os.ReadFile(exe); string(got) != "old" {
			t.Fatalf("restored executable = %q", got)
		}
		if got, _ := os.ReadFile(staged); string(got) != "new" {
			t.Fatalf("staged update = %q", got)
		}
		if _, err := os.Stat(exe + ".old"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("backup remains after rollback: %v", err)
		}
	})

	t.Run("rollback failure retains backup evidence", func(t *testing.T) {
		dir := t.TempDir()
		exe, staged := filepath.Join(dir, "sb.exe"), filepath.Join(dir, "staged.exe")
		mustWriteUpdateTestFile(t, exe, "old")
		mustWriteUpdateTestFile(t, staged, "new")
		calls := 0
		publishErr := errors.New("publish failed")
		rollbackErr := errors.New("rollback failed")
		err := replaceExecutableWithBackup(exe, staged, func(from, to string) error {
			calls++
			switch calls {
			case 1:
				return os.Rename(from, to)
			case 2:
				return publishErr
			default:
				return rollbackErr
			}
		})
		if !errors.Is(err, publishErr) || !errors.Is(err, rollbackErr) {
			t.Fatalf("joined rollback error = %v", err)
		}
		if got, _ := os.ReadFile(exe + ".old"); string(got) != "old" {
			t.Fatalf("retained backup = %q", got)
		}
	})

	t.Run("existing backup fails before mutation", func(t *testing.T) {
		dir := t.TempDir()
		exe, staged := filepath.Join(dir, "sb.exe"), filepath.Join(dir, "staged.exe")
		mustWriteUpdateTestFile(t, exe, "old")
		mustWriteUpdateTestFile(t, staged, "new")
		mustWriteUpdateTestFile(t, exe+".old", "foreign")
		calls := 0
		if err := replaceExecutableWithBackup(exe, staged, func(string, string) error { calls++; return nil }); err == nil {
			t.Fatal("existing backup was overwritten")
		}
		if calls != 0 {
			t.Fatalf("existing backup made %d moves", calls)
		}
	})
}

func TestInstallScriptConsumesOneExactMemberOffline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh supports macOS and Linux; Windows uses release downloads")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}

	for _, tag := range []string{"v1.21.0", "v1.21.0-rc-one+build.01", "v1.21.0-01-beta", ""} {
		name := tag
		if name == "" {
			name = "latest metadata"
		}
		t.Run("valid "+name, func(t *testing.T) {
			result := runInstallScriptFixture(t, script, tag, []updateTarEntry{{name: "sb", body: []byte("installed")}}, nil)
			if result.err != nil {
				t.Fatalf("installer failed: %v\n%s", result.err, result.output)
			}
			got, err := os.ReadFile(filepath.Join(result.installDir, "sb"))
			if err != nil || string(got) != "installed" {
				t.Fatalf("installed bytes = %q, %v", got, err)
			}
			entries, err := os.ReadDir(result.installDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != "sb" {
				t.Fatalf("install directory = %v", entries)
			}
		})
	}

	for _, test := range []struct {
		name    string
		tag     string
		entries []updateTarEntry
		sums    []byte
	}{
		{name: "traversal", tag: "v1.21.0", entries: []updateTarEntry{{name: "../sb", body: []byte("escape")}}},
		{name: "duplicate", tag: "v1.21.0", entries: []updateTarEntry{{name: "sb", body: []byte("one")}, {name: "sb", body: []byte("two")}}},
		{name: "extra", tag: "v1.21.0", entries: []updateTarEntry{{name: "sb", body: []byte("one")}, {name: "README", body: []byte("two")}}},
		{name: "symlink", tag: "v1.21.0", entries: []updateTarEntry{{name: "sb", typeflag: tar.TypeSymlink, linkname: "outside"}}},
		{name: "hardlink", tag: "v1.21.0", entries: []updateTarEntry{{name: "sb", typeflag: tar.TypeLink, linkname: "outside"}}},
		{name: "directory", tag: "v1.21.0", entries: []updateTarEntry{{name: "sb", typeflag: tar.TypeDir}}},
		{name: "empty binary", tag: "v1.21.0", entries: []updateTarEntry{{name: "sb"}}},
		{name: "invalid tag", tag: "v1.21/../../escape", entries: []updateTarEntry{{name: "sb", body: []byte("one")}}},
		{name: "leading-zero core", tag: "v01.21.0", entries: []updateTarEntry{{name: "sb", body: []byte("one")}}},
		{name: "leading-zero numeric prerelease", tag: "v1.21.0-01", entries: []updateTarEntry{{name: "sb", body: []byte("one")}}},
		{name: "overflowing core", tag: "v18446744073709551616.0.0", entries: []updateTarEntry{{name: "sb", body: []byte("one")}}},
		{name: "malformed checksum", tag: "v1.21.0", entries: []updateTarEntry{{name: "sb", body: []byte("one")}}, sums: []byte("not a checksum\n")},
		{name: "single-space checksum", tag: "v1.21.0", entries: []updateTarEntry{{name: "sb", body: []byte("one")}}, sums: []byte(strings.Repeat("0", 64) + " sb_1.21.0_ignored.tar.gz\n")},
		{name: "tab checksum", tag: "v1.21.0", entries: []updateTarEntry{{name: "sb", body: []byte("one")}}, sums: []byte(strings.Repeat("0", 64) + "\tsb_1.21.0_ignored.tar.gz\n")},
		{name: "oversized checksums", tag: "v1.21.0", entries: []updateTarEntry{{name: "sb", body: []byte("one")}}, sums: bytes.Repeat([]byte("x"), 65537)},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := runInstallScriptFixture(t, script, test.tag, test.entries, test.sums)
			if result.err == nil {
				t.Fatalf("unsafe fixture installed successfully:\n%s", result.output)
			}
			if _, err := os.Stat(filepath.Join(result.installDir, "sb")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refused install published sb: %v", err)
			}
		})
	}

	t.Run("bounded archive download", func(t *testing.T) {
		boundedScript := installScriptWithLimit(t, script, "MAX_ARCHIVE_BYTES=134217728", "MAX_ARCHIVE_BYTES=64")
		result := runInstallScriptFixture(t, boundedScript, "v1.21.0", []updateTarEntry{{name: "sb", body: bytes.Repeat([]byte("x"), 2048)}}, nil)
		if result.err == nil || !bytes.Contains(result.output, []byte("exceeds the 64-byte limit")) {
			t.Fatalf("oversized archive result = %v\n%s", result.err, result.output)
		}
	})

	t.Run("bounded extracted binary", func(t *testing.T) {
		boundedScript := installScriptWithLimit(t, script, "MAX_BINARY_BYTES=268435456", "MAX_BINARY_BYTES=1024")
		result := runInstallScriptFixture(t, boundedScript, "v1.21.0", []updateTarEntry{{name: "sb", body: bytes.Repeat([]byte("x"), 1025)}}, nil)
		if result.err == nil || !bytes.Contains(result.output, []byte("could not extract a bounded sb")) {
			t.Fatalf("oversized binary was not stopped by the pre-write bound: %v\n%s", result.err, result.output)
		}
		if _, err := os.Stat(filepath.Join(result.installDir, "sb")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("oversized binary was published: %v", err)
		}
	})
}

func installScriptWithLimit(t *testing.T, script, old, replacement string) string {
	t.Helper()
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	updated := bytes.Replace(body, []byte(old), []byte(replacement), 1)
	if bytes.Equal(updated, body) {
		t.Fatalf("installer limit %q was not found", old)
	}
	path := filepath.Join(t.TempDir(), "install.sh")
	if err := os.WriteFile(path, updated, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

type updateTarEntry struct {
	name     string
	body     []byte
	typeflag byte
	linkname string
}

func makeUpdateArchive(t *testing.T, entries []updateTarEntry, uncompressedTail []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		header := &tar.Header{
			Name: entry.name, Mode: 0o755, Size: int64(len(entry.body)),
			Typeflag: typeflag, Linkname: entry.linkname,
		}
		if typeflag != tar.TypeReg {
			header.Size = 0
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err := tw.Write(entry.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if len(uncompressedTail) > 0 {
		if _, err := gz.Write(uncompressedTail); err != nil {
			t.Fatal(err)
		}
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func mustWriteUpdateTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
}

type installScriptResult struct {
	installDir string
	output     []byte
	err        error
}

func runInstallScriptFixture(t *testing.T, script, tag string, entries []updateTarEntry, sumsOverride []byte) installScriptResult {
	t.Helper()
	root := t.TempDir()
	effectiveTag := tag
	if effectiveTag == "" {
		effectiveTag = "v1.21.0"
	}
	archivePath := filepath.Join(root, "archive.tar.gz")
	archive := makeUpdateArchive(t, entries, nil)
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	osName, arch := runtime.GOOS, runtime.GOARCH
	asset := "sb_" + strings.TrimPrefix(effectiveTag, "v") + "_" + osName + "_" + arch + ".tar.gz"
	sum := sha256.Sum256(archive)
	sums := []byte(hex.EncodeToString(sum[:]) + "  " + asset + "\n")
	if sumsOverride != nil {
		sums = sumsOverride
	}
	sumsPath := filepath.Join(root, "checksums.txt")
	if err := os.WriteFile(sumsPath, sums, 0o600); err != nil {
		t.Fatal(err)
	}
	releasePath := filepath.Join(root, "release.json")
	if err := os.WriteFile(releasePath, []byte(`{"tag_name":"`+effectiveTag+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	fakeBin := filepath.Join(root, "bin")
	if err := os.Mkdir(fakeBin, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeCurl := filepath.Join(fakeBin, "curl")
	const curlScript = `#!/bin/sh
set -eu
output=""
url=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) output=$2; shift 2 ;;
    --max-filesize) shift 2 ;;
    -*) shift ;;
    *) url=$1; shift ;;
  esac
done
case "$url" in
  */releases/latest) cp "$SB_TEST_RELEASE" "$output" ;;
  */checksums.txt) cp "$SB_TEST_SUMS" "$output" ;;
  *.tar.gz) cp "$SB_TEST_ARCHIVE" "$output" ;;
  *) exit 22 ;;
esac
`
	if err := os.WriteFile(fakeCurl, []byte(curlScript), 0o700); err != nil {
		t.Fatal(err)
	}
	installDir := filepath.Join(root, "install")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// The public install command pipes the script to Bash. Running the same
	// interpreter here pins Bash's 1024-byte `ulimit -f` unit: the regression
	// above fails if the script accidentally calculates in POSIX 512-byte units.
	cmd := exec.CommandContext(ctx, "bash", script)
	cmd.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SB_VERSION="+tag,
		"SB_INSTALL_DIR="+installDir,
		"SB_TEST_ARCHIVE="+archivePath,
		"SB_TEST_SUMS="+sumsPath,
		"SB_TEST_RELEASE="+releasePath,
	)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("installer did not terminate: %v\n%s", ctx.Err(), output)
	}
	return installScriptResult{installDir: installDir, output: output, err: err}
}

func TestPackageManagerForPlatformNormalizesWindowsPaths(t *testing.T) {
	tests := []struct {
		path, goos, want string
	}{
		{`C:\Users\alice\scoop\apps\switchboard\current\sb.exe`, "windows", "Scoop"},
		{"/opt/homebrew/Cellar/switchboard/1/bin/sb", "darwin", "Homebrew"},
		{"/usr/local/bin/sb", "linux", "the system package manager"},
	}
	for _, test := range tests {
		got, ok := packageManagerForPlatform(test.path, test.goos)
		if !ok || got != test.want {
			t.Errorf("packageManagerForPlatform(%q, %q) = %q, %v", test.path, test.goos, got, ok)
		}
	}
	if got, ok := packageManagerForPlatform("/work/scoop/project/sb", "linux"); ok {
		t.Fatalf("ordinary Linux path was classified as %s", got)
	}
}

func TestFetchJSONRejectsOverCapAndTrailingValues(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/large":
			_, _ = w.Write(bytes.Repeat([]byte(" "), (1<<20)+1))
		case "/trailing":
			_, _ = fmt.Fprint(w, `{"tag_name":"v1.21.0"} {"tag_name":"v1.22.0"}`)
		}
	}))
	defer server.Close()
	if _, err := fetchJSON[ghRelease](context.Background(), server.URL+"/large"); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("large JSON = %v", err)
	}
	if _, err := fetchJSON[ghRelease](context.Background(), server.URL+"/trailing"); err == nil || !strings.Contains(err.Error(), "more than one") {
		t.Fatalf("trailing JSON = %v", err)
	}
}
