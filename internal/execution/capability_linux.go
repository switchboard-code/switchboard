package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Linux confinement is a namespace construction rather than a policy language.
// Instead of allowing and denying operations, bubblewrap builds a filesystem
// view: the whole tree read-only, a handful of writable binds on top, and empty
// mounts over the paths that must not be readable.
//
// Two ordering rules fall out of that and are easy to break:
//
//  1. Writable binds must come after the read-only root, and the deny mounts
//     must come after those. Mounts apply in order, so a deny placed before a
//     bind that covers the same path silently does nothing.
//  2. A deny mount can only be placed over a path that exists. bubblewrap
//     cannot create the mountpoint, because its parent is read-only by then,
//     and the whole invocation fails rather than that one flag being skipped.
//     Absent paths are dropped, which is safe: there is nothing there to hide.
func detectPlatform() Capability {
	c := Capability{Mechanism: MechanismBubblewrap}

	path, err := exec.LookPath("bwrap")
	if err != nil {
		return Capability{
			Mechanism: MechanismNone,
			Detail:    "bubblewrap is not installed; install it to enable automatic execution",
		}
	}
	c.MechanismPresent = true
	bwrap, err := resolveBubblewrapExecutable(path, string(filepath.Separator), 0, true)
	if err != nil {
		c.Detail = fmt.Sprintf("bubblewrap at %q is not a trusted system executable: %v", path, err)
		return c
	}

	profileKey, err := linuxProfileKey(bwrap)
	if err != nil {
		c.Detail = fmt.Sprintf("bubblewrap changed while its sandbox profile was being prepared: %v", err)
		return c
	}
	verified, detail := cachedVerification(profileKey, linuxHostKey(), func() (bool, string) {
		return linuxSelfTest(bwrap.wrap)
	})
	c.Detail = detail
	if verified {
		c.confinement = &Confinement{mechanism: MechanismBubblewrap, wrap: bwrap.wrap}
	}
	return c
}

// executableIdentity pins a verified sandbox profile to the exact executable
// that passed it. Path alone is not identity: package upgrades and hostile
// replacement both leave the same name behind.
type executableIdentity struct {
	Device      uint64
	Inode       uint64
	Size        int64
	Mode        uint32
	UID         uint32
	GID         uint32
	ModTimeNano int64
	Digest      [sha256.Size]byte
}

func (id executableIdentity) key() string {
	return shortHash(fmt.Sprintf("%d:%d:%d:%d:%d:%d:%d:%s",
		id.Device, id.Inode, id.Size, id.Mode, id.UID, id.GID,
		id.ModTimeNano, hex.EncodeToString(id.Digest[:])))
}

// bubblewrapExecutable is immutable provenance captured before the sandbox
// self-test. Its wrapper rechecks that provenance before every command, so a
// cached verification cannot bless a replacement executable.
type bubblewrapExecutable struct {
	path       string
	trustRoot  string
	trustedUID uint32
	// rejectCurrentUserWrite closes POSIX ACLs and other permissions that are
	// not represented by the group/other mode bits. It is true in production;
	// tests turn it off only for fixtures owned by the test process.
	rejectCurrentUserWrite bool
	identity               executableIdentity
}

// resolveBubblewrapExecutable canonicalizes a candidate and accepts it only
// when it and every directory back to trustRoot are owned by trustedUID and
// cannot be modified by group or other users. Production uses / and uid 0;
// the explicit root and uid make the same checks deterministic in unit tests.
func resolveBubblewrapExecutable(candidate, trustRoot string, trustedUID uint32, rejectCurrentUserWrite bool) (bubblewrapExecutable, error) {
	resolvedRoot, err := canonicalAbsolutePath(trustRoot)
	if err != nil {
		return bubblewrapExecutable{}, fmt.Errorf("resolving trust root: %w", err)
	}
	resolved, err := canonicalAbsolutePath(candidate)
	if err != nil {
		return bubblewrapExecutable{}, fmt.Errorf("resolving executable: %w", err)
	}
	if !pathWithin(resolvedRoot, resolved) {
		return bubblewrapExecutable{}, fmt.Errorf("resolved path %q is outside trusted root %q", resolved, resolvedRoot)
	}
	if err := validateTrustedDirectoryChain(filepath.Dir(resolved), resolvedRoot, trustedUID, rejectCurrentUserWrite); err != nil {
		return bubblewrapExecutable{}, err
	}
	identity, err := readTrustedExecutableIdentity(resolved, trustedUID, rejectCurrentUserWrite)
	if err != nil {
		return bubblewrapExecutable{}, err
	}
	return bubblewrapExecutable{
		path:                   resolved,
		trustRoot:              resolvedRoot,
		trustedUID:             trustedUID,
		rejectCurrentUserWrite: rejectCurrentUserWrite,
		identity:               identity,
	}, nil
}

func canonicalAbsolutePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func validateTrustedDirectoryChain(dir, root string, trustedUID uint32, rejectCurrentUserWrite bool) error {
	if !pathWithin(root, dir) {
		return fmt.Errorf("executable directory %q is outside trusted root %q", dir, root)
	}
	for {
		info, err := os.Stat(dir)
		if err != nil {
			return fmt.Errorf("inspecting executable parent %q: %w", dir, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("executable parent %q is not a directory", dir)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("cannot verify ownership of executable parent %q", dir)
		}
		if stat.Uid != trustedUID {
			return fmt.Errorf("executable parent %q is owned by uid %d, want trusted uid %d", dir, stat.Uid, trustedUID)
		}
		if info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("executable parent %q is group- or other-writable (%#o)", dir, info.Mode().Perm())
		}
		if rejectCurrentUserWrite {
			if err := rejectWritableByCurrentUser(dir, "executable parent"); err != nil {
				return err
			}
		}
		if dir == root {
			return nil
		}
		next := filepath.Dir(dir)
		if next == dir {
			return fmt.Errorf("executable parent chain did not reach trusted root %q", root)
		}
		dir = next
	}
}

func readTrustedExecutableIdentity(path string, trustedUID uint32, rejectCurrentUserWrite bool) (executableIdentity, error) {
	f, err := os.Open(path)
	if err != nil {
		return executableIdentity{}, fmt.Errorf("opening bubblewrap executable: %w", err)
	}
	defer f.Close()

	before, err := f.Stat()
	if err != nil {
		return executableIdentity{}, fmt.Errorf("inspecting bubblewrap executable: %w", err)
	}
	identity, err := identityFromFileInfo(before, trustedUID)
	if err != nil {
		return executableIdentity{}, err
	}
	if rejectCurrentUserWrite {
		if err := rejectWritableByCurrentUser(path, "bubblewrap executable"); err != nil {
			return executableIdentity{}, err
		}
	}
	hash := sha256.New()
	header := make([]byte, 4)
	if _, err := io.ReadFull(f, header); err != nil {
		return executableIdentity{}, fmt.Errorf("reading bubblewrap executable header: %w", err)
	}
	// The sandbox launcher runs before there is a sandbox. Accepting a trusted
	// script here would still let an ambient workspace-first PATH select the
	// interpreter for a #!/usr/bin/env shebang and execute workspace code on the
	// host. Linux bubblewrap is a native ELF binary; anything else fails closed.
	if string(header) != "\x7fELF" {
		return executableIdentity{}, errors.New("bubblewrap executable is not a native ELF binary")
	}
	_, _ = hash.Write(header)
	if _, err := io.Copy(hash, f); err != nil {
		return executableIdentity{}, fmt.Errorf("hashing bubblewrap executable: %w", err)
	}
	copy(identity.Digest[:], hash.Sum(nil))

	after, err := f.Stat()
	if err != nil {
		return executableIdentity{}, fmt.Errorf("rechecking bubblewrap executable: %w", err)
	}
	afterIdentity, err := identityFromFileInfo(after, trustedUID)
	if err != nil {
		return executableIdentity{}, err
	}
	// Ignore Digest here: the second snapshot exists to catch a file changing
	// while its digest was being read.
	identityWithoutDigest := identity
	identityWithoutDigest.Digest = [sha256.Size]byte{}
	if identityWithoutDigest != afterIdentity {
		return executableIdentity{}, errors.New("bubblewrap executable changed while its identity was being read")
	}
	return identity, nil
}

func rejectWritableByCurrentUser(path, kind string) error {
	// access(2) asks the kernel, including POSIX ACL evaluation, rather than
	// reconstructing access from mode bits. Check effective access as well so
	// file capabilities cannot make a path writable behind the real-id check.
	// A set-id Switchboard process is outside this trust model and fails closed:
	// old kernels emulate AT_EACCESS in userspace and cannot faithfully account
	// for every ACL in that case.
	if os.Getuid() != os.Geteuid() || os.Getgid() != os.Getegid() {
		return fmt.Errorf("cannot trust %s %q while process credentials differ", kind, path)
	}
	checks := []error{
		unix.Access(path, unix.W_OK),
		unix.Faccessat(unix.AT_FDCWD, path, unix.W_OK, unix.AT_EACCESS),
	}
	for _, err := range checks {
		if err == nil {
			return fmt.Errorf("%s %q is writable by the current user", kind, path)
		}
		// EACCES is the proof being sought. A read-only filesystem can instead
		// report EROFS, which is at least as strong for this replacement threat.
		if !errors.Is(err, unix.EACCES) && !errors.Is(err, unix.EROFS) {
			return fmt.Errorf("cannot verify that the current user cannot write %s %q: %w", kind, path, err)
		}
	}
	return nil
}

func identityFromFileInfo(info os.FileInfo, trustedUID uint32) (executableIdentity, error) {
	if !info.Mode().IsRegular() {
		return executableIdentity{}, errors.New("bubblewrap executable is not a regular file")
	}
	if info.Mode().Perm()&0o111 == 0 {
		return executableIdentity{}, errors.New("bubblewrap executable has no execute bit")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return executableIdentity{}, fmt.Errorf("bubblewrap executable is group- or other-writable (%#o)", info.Mode().Perm())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return executableIdentity{}, errors.New("cannot verify ownership of bubblewrap executable")
	}
	if stat.Uid != trustedUID {
		return executableIdentity{}, fmt.Errorf("bubblewrap executable is owned by uid %d, want trusted uid %d", stat.Uid, trustedUID)
	}
	return executableIdentity{
		Device:      uint64(stat.Dev),
		Inode:       stat.Ino,
		Size:        info.Size(),
		Mode:        stat.Mode,
		UID:         stat.Uid,
		GID:         stat.Gid,
		ModTimeNano: info.ModTime().UnixNano(),
	}, nil
}

func (b bubblewrapExecutable) revalidate() error {
	current, err := resolveBubblewrapExecutable(b.path, b.trustRoot, b.trustedUID, b.rejectCurrentUserWrite)
	if err != nil {
		return fmt.Errorf("bubblewrap executable is no longer trusted: %w", err)
	}
	if current.path != b.path || current.identity != b.identity {
		return errors.New("bubblewrap executable changed after sandbox verification")
	}
	return nil
}

func (b bubblewrapExecutable) wrap(p Policy, argv []string) ([]string, error) {
	if err := b.revalidate(); err != nil {
		return nil, err
	}
	return wrapBubblewrap(b.path, p, argv)
}

// writableCaches are build caches granted so a second build is not cold. The
// list holds what has actually been exercised under confinement; extending it
// is documented in docs/sandbox.md.
//
// Granting them is also a persistence vector: a command can leave a config or a
// compiled artifact here that a later, separately approved command executes.
// Confinement is per command and is not a durable boundary between commands.
var writableCaches = []string{
	// The XDG base, which Go, pip, and uv all build on.
	".cache",
	".npm",
	".cargo",
	filepath.Join("go", "pkg", "mod"),
}

func wrapBubblewrap(executable string, p Policy, argv []string) ([]string, error) {
	workspace, err := filepath.EvalSymlinks(p.Workspace)
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	tmp := os.TempDir()
	if resolved, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = resolved
	}

	out := []string{
		executable,
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
	}

	// An empty mount over the home directory closes it wholesale. Everything
	// below reopens only what a build needs, so a credential file nobody
	// thought to enumerate is denied by default rather than by luck.
	out = append(out, "--tmpfs", home)

	// Readable, but not writable: toolchains a version manager installed.
	caches := map[string]bool{}
	for _, rel := range writableCaches {
		caches[filepath.Join(home, rel)] = true
	}
	for _, path := range readableHomePaths(home) {
		if !caches[path] {
			out = append(out, "--ro-bind", path, path)
		}
	}

	// Build caches, writable. The directory has to exist before it can be
	// bound: --bind-try would skip a missing one, and the tool inside cannot
	// create it either, so a user who has never run a build would meet
	// "mkdir ~/.cache: read-only file system" on their first confined command.
	for _, rel := range writableCaches {
		dir := filepath.Join(home, rel)
		os.MkdirAll(dir, 0o700)
		out = append(out, "--bind-try", dir, dir)
	}

	// The workspace usually sits inside home, so it comes after the tmpfs.
	out = append(out, "--bind", workspace, workspace)
	if tmp != "/" && !strings.HasPrefix(tmp, home+string(filepath.Separator)) {
		out = append(out, "--bind", tmp, tmp)
	}

	// Secrets that live inside a directory just reopened: cargo keeps registry
	// tokens beside its package cache, and the XDG data directory holds the
	// keyring beside legitimately shared files.
	for _, path := range secretHomePaths(home) {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			out = append(out, "--tmpfs", path)
		} else if err == nil {
			out = append(out, "--ro-bind", os.DevNull, path)
		}
	}

	out = append(out, agentSocketFlags()...)
	out = append(out, sessionBusFlags()...)

	// A tmpfs is writable, so without this the home directory would accept
	// writes into a filesystem that evaporates: the real home stays untouched,
	// but the command sees success and a later one finds nothing. Remounting
	// read-only turns that into the refusal it should have been.
	//
	// It has to come after every mount placed inside home. Earlier, and
	// bubblewrap cannot create the mountpoints for them.
	out = append(out, "--remount-ro", home)

	out = append(out,
		"--unshare-user",
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-uts",
		"--unshare-cgroup",
	)
	if p.Network != NetworkFull {
		// A fresh network namespace has a working loopback interface and no
		// route off the machine, which is exactly the contract: fixture servers
		// bind, egress is unreachable.
		out = append(out, "--unshare-net")
	}

	out = append(out,
		// The confined process must not outlive the runner that started it.
		"--die-with-parent",
		// A new session denies TIOCSTI, which would otherwise let a confined
		// process push characters into the parent's terminal.
		"--new-session",
	)

	// End bubblewrap's option parsing before appending model-controlled argv.
	// Without this, an option-looking executable name such as --bind is parsed
	// as another sandbox rule instead of a command, which can alter the mount
	// profile that just passed verification.
	out = append(out, "--")
	return append(out, argv...), nil
}

// agentSocketFlags neutralizes the ssh-agent socket. Binding /dev/null over it
// is more surgical than hiding its directory, which on some systems is shared
// with unrelated sockets.
func agentSocketFlags() []string {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil
	}
	if _, err := os.Stat(sock); err != nil {
		return nil
	}
	return []string{"--ro-bind", os.DevNull, sock}
}

// sessionBusFlags hides the session bus, which is how gnome-keyring, kwallet,
// and anything else implementing the Secret Service API hand out credentials.
// It is the Linux counterpart of denying the Keychain on macOS: the files being
// unreadable accomplishes nothing while the daemon is still reachable.
func sessionBusFlags() []string {
	var flags []string
	seen := map[string]bool{}

	add := func(path string) {
		if path == "" || seen[path] {
			return
		}
		info, err := os.Stat(path)
		if err != nil {
			return
		}
		seen[path] = true
		if info.IsDir() {
			flags = append(flags, "--tmpfs", path)
		} else {
			flags = append(flags, "--ro-bind", os.DevNull, path)
		}
	}

	if addr := os.Getenv("DBUS_SESSION_BUS_ADDRESS"); addr != "" {
		// The address looks like "unix:path=/run/user/1000/bus,guid=...".
		for _, part := range strings.Split(addr, ",") {
			if p, ok := strings.CutPrefix(part, "unix:path="); ok {
				add(p)
			}
			if p, ok := strings.CutPrefix(part, "path="); ok {
				add(p)
			}
		}
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		add(filepath.Join(dir, "bus"))
		add(filepath.Join(dir, "keyring"))
	}
	return flags
}

// linuxProfileKey changes whenever the constructed argument list or executable
// identity changes, so a cached pass cannot survive a profile edit or a
// bubblewrap package replacement.
func linuxProfileKey(bwrap bubblewrapExecutable) (string, error) {
	sample, err := bwrap.wrap(
		Policy{Workspace: os.TempDir(), Network: NetworkLoopback},
		[]string{"/probe"},
	)
	if err != nil {
		return "", err
	}
	return shortHash(strings.Join(sample, "\x00") + "\x00executable-identity=" + bwrap.identity.key()), nil
}

// linuxHostKey pins the verdict to this kernel. Namespace and seccomp behavior
// is kernel-dependent, and a distribution upgrade should re-run the check.
func linuxHostKey() string {
	var name unix.Utsname
	if err := unix.Uname(&name); err != nil {
		return "unknown"
	}
	release := unix.ByteSliceToString(name.Release[:])
	machine := unix.ByteSliceToString(name.Machine[:])
	if release == "" || machine == "" {
		return "unknown"
	}
	return release + " " + machine
}
