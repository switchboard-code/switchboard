package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/config"
)

// Update machinery (§18). The check names nothing but the running version and
// is separate from telemetry; [updates] check = false or SB_NO_UPDATE_CHECK=1
// turns it off.
//
// Releases are expected to publish, per tag, one archive per platform named
// sb_<version>_<goos>_<goarch>.tar.gz containing the sb binary, plus a
// checksums.txt of sha256 sums. The checksum is verified before anything is
// replaced. §18 additionally calls for signed update metadata: that needs a
// signing key in the release pipeline, which does not exist yet, so this
// verifies integrity only. A checksum served beside a compromised binary is
// not authenticity, and this comment is not a substitute for fixing that.
var (
	// version is set at release time: -ldflags "-X main.version=v0.3.0".
	version = "dev"

	// updateRepo is the GitHub owner/repo releases are fetched from.
	updateRepo = "switchboard-code/switchboard"
)

// currentVersion is the release version, or "" for a dev build, which has
// nothing meaningful to compare against.
func currentVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return ""
}

var updateHTTP = &http.Client{Timeout: 8 * time.Second}

type ghRelease struct {
	TagName    string    `json:"tag_name"`
	Prerelease bool      `json:"prerelease"`
	Assets     []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// fetchLatest resolves the newest release the channel accepts. Stable asks
// GitHub's /latest, which already excludes prereleases; beta has to list and
// choose, because "latest including prereleases" is not an endpoint.
func fetchLatest(ctx context.Context, channel string) (*ghRelease, error) {
	if channel != "beta" {
		return fetchJSON[ghRelease](ctx, "https://api.github.com/repos/"+updateRepo+"/releases/latest")
	}
	releases, err := fetchJSON[[]ghRelease](ctx, "https://api.github.com/repos/"+updateRepo+"/releases?per_page=20")
	if err != nil {
		return nil, err
	}
	var best *ghRelease
	for i := range *releases {
		rel := &(*releases)[i]
		if best == nil || newerVersion(rel.TagName, best.TagName) {
			best = rel
		}
	}
	if best == nil {
		return nil, errNoRelease
	}
	return best, nil
}

func fetchJSON[T any](ctx context.Context, url string) (*T, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "switchboard/"+version)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := updateHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errNoRelease
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release check answered %s", resp.Status)
	}
	body, err := readBounded(resp.Body, 1<<20, "release response")
	if err != nil {
		return nil, err
	}
	var out T
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&out); err != nil {
		return nil, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	return &out, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("release response contains more than one JSON value")
		}
		return fmt.Errorf("reading release response tail: %w", err)
	}
	return nil
}

var errNoRelease = errors.New("no releases published yet")

// newerVersion reports whether candidate outranks current under semver
// precedence. Prerelease ordering matters because the beta channel moves
// v0.4.0-beta.1 → v0.4.0-beta.2 → v0.4.0, and a comparison that strips the
// suffix would refuse the second step and repeat the third forever.
func newerVersion(candidate, current string) bool {
	c, ok1 := parseSemver(candidate)
	u, ok2 := parseSemver(current)
	if !ok1 || !ok2 {
		return false
	}
	for i := range c.core {
		if c.core[i] != u.core[i] {
			return c.core[i] > u.core[i]
		}
	}
	// Equal cores: a release outranks any prerelease of it.
	if c.pre == "" || u.pre == "" {
		return c.pre == "" && u.pre != ""
	}
	return comparePrerelease(c.pre, u.pre) > 0
}

type semver struct {
	core [3]uint64
	pre  string
}

func parseSemver(v string) (semver, bool) {
	var out semver
	version, ok := canonicalReleaseVersion(v)
	if !ok {
		return out, false
	}
	// Build metadata identifies release artifacts but has no SemVer
	// precedence. The release pipeline and asset resolver accept it, so the
	// update chooser must ignore it rather than making that valid release
	// unreachable from (or after) an ordinary build.
	precedence, _, _ := strings.Cut(version, "+")
	core, pre, hasPrerelease := strings.Cut(precedence, "-")
	if hasPrerelease {
		out.pre = pre
	}
	parts := strings.Split(core, ".")
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return out, false
		}
		out.core[i] = n
	}
	return out, true
}

// comparePrerelease is semver §11.4: dot-separated identifiers, numeric ones
// compared numerically and ranking below alphanumeric ones, fewer identifiers
// ranking lower when all shared ones are equal.
func comparePrerelease(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		aNumeric := asciiDigits(as[i])
		bNumeric := asciiDigits(bs[i])
		switch {
		case aNumeric && bNumeric:
			// SemVer numeric identifiers have no size bound. Canonical tags have
			// no leading zero, so digit count followed by lexical order is exact
			// without overflowing a machine integer.
			if len(as[i]) != len(bs[i]) {
				if len(as[i]) > len(bs[i]) {
					return 1
				}
				return -1
			}
			if c := strings.Compare(as[i], bs[i]); c != 0 {
				return c
			}
		case aNumeric:
			return -1
		case bNumeric:
			return 1
		default:
			if c := strings.Compare(as[i], bs[i]); c != 0 {
				return c
			}
		}
	}
	switch {
	case len(as) > len(bs):
		return 1
	case len(as) < len(bs):
		return -1
	}
	return 0
}

// startupUpdate runs once at TUI startup. The default notice-only posture does
// not replace the executable. An explicit auto opt-in goes all the way:
// download, verify, replace, and say so; the running process is untouched and
// the next start runs the new binary. Failure is silent beyond falling back
// to the notice, because a tool that nags about its own update check failing
// is worse than one that skips it.
func startupUpdate(cfg *config.Config) func(context.Context) tea.Msg {
	return startupUpdateWith(cfg, startupUpdateRuntime{
		current: currentVersion,
		fetch:   fetchLatest,
		apply:   selfUpdate,
	})
}

type startupUpdateRuntime struct {
	current func() string
	fetch   func(context.Context, string) (*ghRelease, error)
	apply   func(context.Context, *ghRelease) error
}

func startupUpdateWith(cfg *config.Config, rt startupUpdateRuntime) func(context.Context) tea.Msg {
	channel, auto := cfg.UpdateChannel, cfg.UpdateAuto
	return func(lifetime context.Context) tea.Msg {
		current := rt.current()
		if current == "" {
			return updateCheckMsg{}
		}
		ctx, cancel := context.WithTimeout(lifetime, 3*time.Minute)
		defer cancel()
		rel, err := rt.fetch(ctx, channel)
		if err != nil || !newerVersion(rel.TagName, current) {
			return updateCheckMsg{}
		}
		if !auto {
			return updateCheckMsg{latest: rel.TagName}
		}
		if err := rt.apply(ctx, rel); err != nil {
			// Including installs a package manager owns: those fall back to
			// the notice, which /update explains rather than fights.
			return updateCheckMsg{latest: rel.TagName}
		}
		return updateAppliedMsg{version: rel.TagName}
	}
}

const updateUsage = "usage: /update, /update channel [stable|beta], or /update auto [on|off]"

// runUpdateCLI is `sb update`: the same fetch-verify-replace as /update, on
// stdout, so scripts and CI can move a binary forward without a terminal.
func runUpdateCLI(ctx context.Context, cfg *config.Config) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	rel, err := fetchLatest(ctx, cfg.UpdateChannel)
	if errors.Is(err, errNoRelease) {
		fmt.Println("no releases published yet; nothing to update to")
		return nil
	}
	if err != nil {
		return fmt.Errorf("update check failed: %w", err)
	}
	if current := currentVersion(); current != "" && !newerVersion(rel.TagName, current) {
		fmt.Println("already on the latest (" + cliText(current) + ")")
		return nil
	}
	if err := selfUpdate(ctx, rel); err != nil {
		return fmt.Errorf("update failed: %w", err)
	}
	fmt.Println("updated to " + cliText(rel.TagName))
	return nil
}

func cmdUpdate(m *tuiModel, args string) tea.Cmd {
	if args != "" {
		return updateSettings(m, args)
	}
	channel := m.app.config.UpdateChannel
	return m.ownUpdateCmd(func(lifetime context.Context) tea.Msg {
		ctx, cancel := context.WithTimeout(lifetime, 3*time.Minute)
		defer cancel()

		rel, err := fetchLatest(ctx, channel)
		if errors.Is(err, errNoRelease) {
			return noticeMsg{text: "no releases published yet; nothing to update to"}
		}
		if err != nil {
			return noticeMsg{level: "error", text: "update check failed: " + err.Error()}
		}
		if current := currentVersion(); current != "" && !newerVersion(rel.TagName, current) {
			return noticeMsg{text: "already on the latest (" + current + ")"}
		}
		if err := selfUpdate(ctx, rel); err != nil {
			return noticeMsg{level: "error", text: "update failed: " + err.Error()}
		}
		return noticeMsg{text: "updated to " + rel.TagName + "; restart sb to run it"}
	})
}

// updateSettings is /update channel and /update auto: the update posture is
// configuration, and configuration is set from inside the TUI.
func updateSettings(m *tuiModel, args string) tea.Cmd {
	cfg := m.app.config
	what, value, _ := strings.Cut(strings.TrimSpace(args), " ")
	value = strings.TrimSpace(value)
	switch what {
	case "channel":
		switch value {
		case "":
			ch := cfg.UpdateChannel
			if ch == "" {
				ch = "stable"
			}
			return noticeCmd("", "update channel is "+ch)
		case "stable", "beta":
			if err := cfg.SetUpdateChannelAndSave(value); err != nil {
				return noticeCmd("error", "saving the channel failed, nothing changed: "+err.Error())
			}
			return noticeCmd("", "update channel is now "+value)
		default:
			return noticeCmd("error", updateUsage)
		}
	case "auto":
		switch value {
		case "":
			state := "on"
			if !cfg.UpdateAuto {
				state = "off"
			}
			return noticeCmd("", "auto-update is "+state)
		case "on", "off":
			if err := cfg.SetUpdateAutoAndSave(value == "on"); err != nil {
				return noticeCmd("error", "saving the setting failed, nothing changed: "+err.Error())
			}
			return noticeCmd("", "auto-update is now "+value)
		default:
			return noticeCmd("error", updateUsage)
		}
	default:
		return noticeCmd("error", updateUsage)
	}
}

// selfUpdate downloads the archive for this platform, verifies it against the
// release's checksums, and atomically replaces the running binary.
func selfUpdate(ctx context.Context, rel *ghRelease) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	return selfUpdateExecutable(ctx, rel, exe, updateRuntime{
		goos:    runtime.GOOS,
		goarch:  runtime.GOARCH,
		fetch:   download,
		replace: installUpdateBinary,
	})
}

const (
	maxUpdateChecksums = int64(1 << 16)
	maxUpdateArchive   = int64(128 << 20)
	maxUpdateBinary    = int64(256 << 20)
	maxUpdateTarTail   = int64(1 << 20)
)

type updateRuntime struct {
	goos    string
	goarch  string
	fetch   func(context.Context, string, int64) ([]byte, error)
	replace func(string, []byte) error
}

func selfUpdateExecutable(ctx context.Context, rel *ghRelease, exe string, rt updateRuntime) error {
	if rel == nil {
		return errors.New("release metadata is missing")
	}
	if rt.fetch == nil || rt.replace == nil {
		return errors.New("update runtime is incomplete")
	}
	if rt.goos == "" || rt.goarch == "" {
		return errors.New("update platform is missing")
	}

	// §18: an install that came from a package manager defers to it rather
	// than fighting it.
	if managedBy, ok := packageManagerForPlatform(exe, rt.goos); ok {
		return fmt.Errorf("this install is managed by %s; update through it", managedBy)
	}

	assetName, err := updateAssetName(rel.TagName, rt.goos, rt.goarch)
	if err != nil {
		return err
	}
	assetURL, sumsURL, err := updateAssetURLs(rel, assetName)
	if err != nil {
		return err
	}

	sums, err := rt.fetch(ctx, sumsURL, maxUpdateChecksums)
	if err != nil {
		return fmt.Errorf("downloading checksums.txt: %w", err)
	}
	want, err := checksumFor(sums, assetName)
	if err != nil {
		return err
	}

	archive, err := rt.fetch(ctx, assetURL, maxUpdateArchive)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", assetName, err)
	}
	sum := sha256.Sum256(archive)
	if hex.EncodeToString(sum[:]) != want {
		return errors.New("checksum mismatch; nothing was installed")
	}

	member := "sb"
	if rt.goos == "windows" {
		member = "sb.exe"
	}
	binary, err := extractUpdateBinary(archive, member, maxUpdateBinary)
	if err != nil {
		return err
	}
	// Cancellation before publication leaves the currently installed binary
	// untouched. Once replacement starts it is deliberately not interruptible;
	// the TUI lifetime owner joins the complete namespace transaction instead.
	if err := ctx.Err(); err != nil {
		return err
	}
	return rt.replace(exe, binary)
}

func updateAssetName(tag, goos, goarch string) (string, error) {
	version, ok := canonicalReleaseVersion(tag)
	if !ok {
		return "", fmt.Errorf("release tag %q is not a canonical vX.Y.Z version", tag)
	}
	for _, value := range []string{version, goos, goarch} {
		if value == "" || strings.ContainsAny(value, "/\\\x00 \t\r\n") {
			return "", errors.New("release identity contains an unsafe asset-name component")
		}
	}
	return fmt.Sprintf("sb_%s_%s_%s.tar.gz", version, goos, goarch), nil
}

func canonicalReleaseVersion(tag string) (string, bool) {
	if strings.TrimSpace(tag) != tag || !strings.HasPrefix(tag, "v") || len(tag) == 1 || len(tag) > 128 {
		return "", false
	}
	version := tag[1:]
	main, build, hasBuild := strings.Cut(version, "+")
	if hasBuild && !validSemverIdentifiers(build, false) {
		return "", false
	}
	core, prerelease, hasPrerelease := strings.Cut(main, "-")
	if hasPrerelease && !validSemverIdentifiers(prerelease, true) {
		return "", false
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return "", false
	}
	for _, part := range parts {
		if !asciiDigits(part) || len(part) > 1 && part[0] == '0' {
			return "", false
		}
		if _, err := strconv.ParseUint(part, 10, 64); err != nil {
			return "", false
		}
	}
	return version, true
}

func validSemverIdentifiers(value string, rejectNumericLeadingZero bool) bool {
	if value == "" {
		return false
	}
	for _, identifier := range strings.Split(value, ".") {
		if identifier == "" {
			return false
		}
		numeric := true
		for _, r := range identifier {
			if r < '0' || r > '9' {
				numeric = false
			}
			if r < '0' || r > '9' {
				if r < 'A' || r > 'Z' {
					if r < 'a' || r > 'z' {
						if r != '-' {
							return false
						}
					}
				}
			}
		}
		if rejectNumericLeadingZero && numeric && len(identifier) > 1 && identifier[0] == '0' {
			return false
		}
	}
	return true
}

func asciiDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func updateAssetURLs(rel *ghRelease, assetName string) (string, string, error) {
	var assetURL, sumsURL string
	assetCount, sumsCount := 0, 0
	for _, asset := range rel.Assets {
		switch asset.Name {
		case assetName:
			assetCount++
			assetURL = asset.URL
		case "checksums.txt":
			sumsCount++
			sumsURL = asset.URL
		}
	}
	if assetCount == 0 {
		return "", "", fmt.Errorf("release %s has no build named %s", rel.TagName, assetName)
	}
	if assetCount != 1 {
		return "", "", fmt.Errorf("release %s has %d assets named %s", rel.TagName, assetCount, assetName)
	}
	if sumsCount == 0 {
		return "", "", errors.New("release has no checksums.txt; refusing to install unverified bits")
	}
	if sumsCount != 1 {
		return "", "", fmt.Errorf("release %s has %d assets named checksums.txt", rel.TagName, sumsCount)
	}
	if assetURL == "" || sumsURL == "" {
		return "", "", errors.New("release asset has an empty download URL")
	}
	return assetURL, sumsURL, nil
}

// sweepOldBinary removes the .old a Windows self-update leaves behind. Called
// at startup; every error is ignorable because the file either is not there,
// is still running, or will be swept next time.
func sweepOldBinary() {
	if runtime.GOOS != "windows" {
		return
	}
	if exe, err := os.Executable(); err == nil {
		os.Remove(exe + ".old")
	}
}

// packageManagerFor recognizes install layouts that belong to a package
// manager by where the binary lives.
func packageManagerFor(exe string) (string, bool) {
	return packageManagerForPlatform(exe, runtime.GOOS)
}

func packageManagerForPlatform(exe, goos string) (string, bool) {
	normalized := strings.ReplaceAll(filepath.ToSlash(exe), `\`, "/")
	lower := strings.ToLower(normalized)
	switch {
	case strings.Contains(normalized, "/Cellar/"), strings.Contains(lower, "/homebrew/"), strings.Contains(lower, "/linuxbrew/"):
		return "Homebrew", true
	case goos == "windows" && strings.Contains(lower, "/scoop/"):
		return "Scoop", true
	case strings.HasPrefix(normalized, "/usr/local/bin/") && goos == "linux",
		strings.HasPrefix(normalized, "/usr/bin/"):
		return "the system package manager", true
	}
	return "", false
}

func download(ctx context.Context, url string, cap int64) ([]byte, error) {
	if cap < 0 {
		return nil, errors.New("download byte cap is invalid")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "switchboard/"+version)
	resp, err := updateHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s: %s", url, resp.Status)
	}
	if resp.ContentLength > cap {
		return nil, fmt.Errorf("download exceeds the %d-byte limit", cap)
	}
	return readBounded(resp.Body, cap, "download")
}

func readBounded(r io.Reader, cap int64, label string) ([]byte, error) {
	if cap < 0 || cap == int64(^uint64(0)>>1) {
		return nil, fmt.Errorf("%s byte cap is invalid", label)
	}
	data, err := io.ReadAll(io.LimitReader(r, cap+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > cap {
		return nil, fmt.Errorf("%s exceeds the %d-byte limit", label, cap)
	}
	return data, nil
}

// checksumFor reads a sha256sum-format checksums file.
func checksumFor(sums []byte, name string) (string, error) {
	if filepath.Base(name) != name || name == "" || strings.ContainsAny(name, "\x00\r\n") {
		return "", errors.New("checksum lookup has an unsafe asset name")
	}
	found := ""
	for line := range strings.Lines(string(sums)) {
		line = strings.TrimSuffix(line, "\n")
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		if len(line) < 67 || line[64] != ' ' || line[65] != ' ' && line[65] != '*' {
			return "", errors.New("checksums.txt is not in sha256sum format")
		}
		sum, file := line[:64], line[66:]
		decoded, err := hex.DecodeString(sum)
		if err != nil || len(decoded) != sha256.Size || file == "" {
			return "", errors.New("checksums.txt contains an invalid sha256 entry")
		}
		if file == name {
			if found != "" {
				return "", fmt.Errorf("checksums.txt has more than one entry for %s", name)
			}
			found = strings.ToLower(sum)
		}
	}
	if found == "" {
		return "", fmt.Errorf("checksums.txt has no entry for %s", name)
	}
	return found, nil
}

// extractSB retains the historical test seam while applying the current
// platform's exact archive-member contract.
func extractSB(archive []byte) ([]byte, error) {
	member := "sb"
	if runtime.GOOS == "windows" {
		member = "sb.exe"
	}
	return extractUpdateBinary(archive, member, maxUpdateBinary)
}

func extractUpdateBinary(archive []byte, member string, cap int64) ([]byte, error) {
	if member != "sb" && member != "sb.exe" {
		return nil, errors.New("release archive member is invalid")
	}
	compressed := bytes.NewReader(archive)
	gz, err := gzip.NewReader(compressed)
	if err != nil {
		return nil, fmt.Errorf("opening release gzip: %w", err)
	}
	gz.Multistream(false)
	defer gz.Close()
	tr := tar.NewReader(gz)
	var binary []byte
	entries := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading release tar: %w", err)
		}
		entries++
		if entries != 1 || hdr.Name != member || hdr.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("release archive must contain exactly one regular file named %s", member)
		}
		if hdr.Size <= 0 || hdr.Size > cap {
			return nil, fmt.Errorf("release binary size %d is outside the 1..%d byte bound", hdr.Size, cap)
		}
		binary, err = readBounded(tr, cap, "release binary")
		if err != nil {
			return nil, err
		}
		if int64(len(binary)) != hdr.Size {
			return nil, errors.New("release binary is truncated")
		}
	}
	if entries != 1 || len(binary) == 0 {
		return nil, fmt.Errorf("release archive must contain exactly one regular file named %s", member)
	}
	tail, err := readBounded(gz, maxUpdateTarTail, "release tar padding")
	if err != nil {
		return nil, err
	}
	for _, b := range tail {
		if b != 0 {
			return nil, errors.New("release tar has non-zero data after its end marker")
		}
	}
	if compressed.Len() != 0 {
		return nil, errors.New("release archive contains trailing or concatenated gzip data")
	}
	return binary, nil
}
