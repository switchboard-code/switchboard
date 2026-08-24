package execution

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// seatbeltProfile is the confinement policy. Its comments explain each rule and
// docs/sandbox.md records how the rules were arrived at.
//
//go:embed seatbelt.sb
var seatbeltProfile string

// sandboxExec is the Seatbelt front end. It has carried a deprecation warning
// in the headers since 10.8 and is still what Apple's own software and Chromium
// use, so the posture is to depend on it while keeping per-action approval as
// the always-available path (see docs/sandbox.md).
const sandboxExec = "/usr/bin/sandbox-exec"

func detectPlatform() Capability {
	c := Capability{Mechanism: MechanismSeatbelt}

	if _, err := os.Stat(sandboxExec); err != nil {
		return Capability{
			Mechanism: MechanismNone,
			Detail:    "sandbox-exec is not present on this system",
		}
	}
	c.MechanismPresent = true

	profileKey, err := darwinProfileKey()
	if err != nil {
		c.Detail = fmt.Sprintf("could not construct the Seatbelt profile: %v", err)
		return c
	}
	verified, detail := cachedVerification(profileKey, darwinHostKey(), darwinSelfTest)
	c.Detail = detail
	if verified {
		c.confinement = &Confinement{mechanism: MechanismSeatbelt, wrap: wrapSeatbelt}
	}
	return c
}

// wrapSeatbelt turns a command into the same command under the profile.
//
// Parameters are passed as separate argv elements rather than substituted into
// the profile text, so a workspace path containing a quote or a parenthesis
// cannot rewrite the policy.
func wrapSeatbelt(p Policy, argv []string) ([]string, error) {
	params, err := profileParams(p)
	if err != nil {
		return nil, err
	}
	profile, err := profileText(p)
	if err != nil {
		return nil, err
	}

	out := []string{sandboxExec, "-p", profile}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, "-D", k+"="+params[k])
	}
	// sandbox-exec keeps parsing -D/-p options until an explicit terminator.
	// The target argv is model-controlled, so without this separator an
	// option-looking executable can replace profile parameters or the profile
	// itself while the permission layer still reports confined execution.
	out = append(out, "--")
	return append(out, argv...), nil
}

// profileText appends the rules that cannot be expressed with a fixed set of
// parameters, because their number depends on what exists on this machine.
//
// Everything here is emitted after the static profile, and later rules win, so
// this section closes the home directory rather than opening anything the base
// profile denied.
func profileText(p Policy) (string, error) {
	var b strings.Builder
	b.WriteString(seatbeltProfile)

	homes, err := protectedHomeDirectories()
	if err != nil {
		return "", err
	}
	workspace, err := filepath.EvalSymlinks(p.Workspace)
	if err != nil {
		return "", fmt.Errorf("resolving workspace %s: %w", p.Workspace, err)
	}

	b.WriteString("\n;; Account and ambient homes are denied wholesale, then narrowly reopened.\n")
	for _, home := range homes {
		fmt.Fprintf(&b, "(deny file-read* (subpath %s))\n", seatbeltString(home))
	}

	// The workspace is usually inside home, so it has to come back first.
	fmt.Fprintf(&b, "(allow file-read* (subpath %s))\n", seatbeltString(workspace))
	for _, path := range p.readOnlyRoots {
		fmt.Fprintf(&b, "(allow file-read* (subpath %s))\n", seatbeltString(path))
	}
	for _, home := range homes {
		for _, path := range readableHomePaths(home) {
			fmt.Fprintf(&b, "(allow file-read* (subpath %s))\n", seatbeltString(path))
		}
	}
	// Denied last, because some of them sit inside the paths just opened.
	for _, home := range homes {
		for _, path := range secretHomePaths(home) {
			fmt.Fprintf(&b, "(deny file-read* (subpath %s))\n", seatbeltString(path))
			fmt.Fprintf(&b, "(deny file-write* (subpath %s))\n", seatbeltString(path))
		}
	}
	resolvedTargets, err := resolvedSymlinkedProtectedTargets(homes[0])
	if err != nil {
		return "", err
	}
	for _, path := range resolvedTargets {
		fmt.Fprintf(&b, "(deny file-read* (subpath %s))\n", seatbeltString(path))
		fmt.Fprintf(&b, "(deny file-write* (subpath %s))\n", seatbeltString(path))
	}

	if p.Network == NetworkFull {
		b.WriteString("\n; Egress granted explicitly for this command.\n(allow network*)\n")
	}
	return b.String(), nil
}

// seatbeltString renders a path as a profile string literal.
//
// Parameters passed with -D cannot be used for these rules because their count
// varies by machine, so the path goes into the policy text and has to be
// escaped. A workspace directory named with a quote would otherwise close the
// literal and let the rest of the name be read as policy.
func seatbeltString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func profileParams(p Policy) (map[string]string, error) {
	accountHome, err := accountHomeDir()
	if err != nil {
		return nil, err
	}
	ambientHome, err := ambientHomeDir()
	if err != nil {
		return nil, err
	}

	workspace, err := filepath.EvalSymlinks(p.Workspace)
	if err != nil {
		return nil, fmt.Errorf("resolving workspace %s: %w", p.Workspace, err)
	}
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return nil, fmt.Errorf("resolving temp directory: %w", err)
	}

	underAccount := func(rest ...string) string {
		return filepath.Join(append([]string{accountHome}, rest...)...)
	}
	cachePath := func(rest ...string) string {
		// A distinct HOME is ambient input and may be a checkout containing
		// symlink aliases into the account home. Do not turn those aliases into
		// Seatbelt write grants. The workspace is already writable, so using its
		// canonical root as an inert parameter adds no authority.
		if ambientHome != accountHome {
			return workspace
		}
		rel := filepath.Join(rest...)
		if path, ok := safeHomeDescendant(accountHome, rel, true); ok && safeWritableHomeCache(accountHome, rel) {
			return path
		}
		return workspace
	}

	return map[string]string{
		"WORKSPACE": workspace,
		"TMPDIR":    tmp,

		"CACHE_GO_BUILD": cachePath("Library", "Caches", "go-build"),
		"CACHE_GO_MOD":   cachePath("go", "pkg", "mod"),
		"CACHE_NPM":      cachePath(".npm"),
		"CACHE_CARGO":    cachePath(".cargo"),
		"CACHE_XDG":      cachePath(".cache"),
		"CACHE_LIBRARY":  cachePath("Library", "Caches"),

		"DENY_SSH":           underAccount(".ssh"),
		"DENY_AWS":           underAccount(".aws"),
		"DENY_GCLOUD":        underAccount(".config", "gcloud"),
		"DENY_KUBE":          underAccount(".kube"),
		"DENY_DOCKER":        underAccount(".docker"),
		"DENY_GNUPG":         underAccount(".gnupg"),
		"DENY_KEYCHAINS":     underAccount("Library", "Keychains"),
		"DENY_KEYCHAINS_SYS": "/Library/Keychains",
		"DENY_CONFIG_SSH":    underAccount(".config", "ssh"),
		"DENY_SWITCHBOARD":   underAccount(".switchboard"),
	}, nil
}

// darwinProfileKey covers the effective profile, not just the embedded file.
// The generated section depends on which toolchain directories exist, so a
// cached pass must not survive the user installing one.
func darwinProfileKey() (string, error) {
	profile, err := profileText(Policy{Workspace: os.TempDir(), Network: NetworkLoopback})
	if err != nil {
		return "", err
	}
	return shortHash(profile), nil
}

// darwinHostKey pins the verdict to this OS build. What the kernel enforces for
// a given profile can change across releases.
func darwinHostKey() string {
	return commandOutput("/usr/bin/sw_vers", "-buildVersion")
}
