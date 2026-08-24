# Confining commands

How Switchboard confines commands, how the user selects that confinement, and
what it does not protect against. The sandbox is off by default.

| Platform | Mechanism | Status |
|---|---|---|
| macOS | Seatbelt via `sandbox-exec` | Available only after a successful host self-test; opt-in |
| Linux | bubblewrap | Canonical root-owned binary and parent chain, no group/other or invoking-user write access, plus successful host self-test; opt-in |
| Windows | none | `on` unavailable; `auto` remains off |

On Windows, `auto` permission mode keeps command execution with the human
because descendant cleanup is not guaranteed. `yolo` grants full host reach
but warns that descendant processes may survive cancellation.

Linux accepts only a canonical root-owned `bwrap` whose parent directories are
also root-owned. The executable and chain must have no group/other write bits
and must not be writable by the invoking user through effective permissions
such as ACLs. Resolution follows symlinks first, so a PATH or profile entry is
accepted only when its resolved regular executable and every resolved parent
back to `/` meet those rules. A user-owned or user-writable install is
unavailable. Install bubblewrap through a trusted system package. Switchboard
revalidates the exact executable identity before each confined command.

## Selecting a posture

Start `sb -sandbox` to require verified confinement for the process. Startup
fails if the host has no verified profile. During a session:

| Command | Result |
| --- | --- |
| `/sandbox on` | Require the verified profile; leave the previous selection unchanged if unavailable |
| `/sandbox off` | Run approved commands directly on the host |
| `/sandbox auto` | Use verified confinement when available; otherwise remain visibly host-direct |
| `/sandbox status` | Report the requested mode, effective confinement, and host reach |

The change is immediate and shared with registries already created for the
session, including delegated work. `[execution] sandbox = "off|on|auto"` sets
the startup default. A bare `-sandbox` selects `on` for one process;
`-sandbox=off|on|auto` supplies another process override. A successful
interactive change updates the user config. If saving fails, the controller
keeps the live change and reports that the next launch default was not updated.
Startup rejects `-mode yolo` when the requested sandbox setting is `on` or
`auto`; select `-sandbox=off` explicitly when the config requests either.

Permission mode is a separate control. `bypass` suppresses command prompts only
when verified confinement isolates both host network and host IPC. Both current
production profiles retain some host IPC, and Seatbelt also shares host
loopback, so bypass asks on both platforms today. `yolo` forces host-direct
filesystem and network access while retaining the requested sandbox selection,
which becomes effective again after leaving `yolo`. Default, acceptEdits, and
auto modes remain separate from confinement. Default and acceptEdits can run a
host-direct command after human approval. Permission `auto` also asks the human
whenever verified confinement is not active; a cheap reviewer never authorizes
host-direct execution. While `yolo` is active, `/sandbox on` and `/sandbox auto`
are refused; leave `yolo` first.

Every rule on both platforms was arrived at by running it. Where one looks
redundant it is usually load-bearing in a way that is invisible until it is
removed, and the sections below say which.

## The contract

Both platforms implement the same promise. A confined command may:

- read the system: `/usr`, `/opt`, `/etc`, and the rest of the filesystem
  outside the account and ambient home directories;
- read the workspace, the build caches, and per-user toolchain installs;
- write inside the workspace, the temp directory, and those build caches;
- execute other programs, allocate terminals, and fork;
- bind and connect to loopback addresses, subject to the platform distinction
  below.

It may not:

- write anywhere else, including either home directory;
- **read anything else under the account or ambient home directory**, whether or not anyone
  thought to name it;
- reach the daemon that hands out credentials, or use the ssh agent;
- open a direct non-loopback connection unless egress was granted for that
  command.

### Reads follow the risk

The read policy is deliberately asymmetric, and this is the one decision here
most worth understanding.

Outside the home directory, reads are broad. System directories hold no user
secrets, and an allowlist over them would break every compiler for nothing.

Inside home directories, reads are closed by default and opened only where a
build needs them. Home is where credentials actually live, and enumerating what
leaks there is a race nobody wins. A survey of one ordinary developer machine
found 51 top-level entries in home. A hand-written deny list covered six of
them. Still readable were an npm registry auth token in `.npmrc`, shell history
with whatever had been pasted into it, `Library/Application Support` for every
installed application, `Documents`, and the credential directories of five
separate CLI tools. Adding those six names would not have fixed it; the next
tool installed would reopen the hole.

The security boundary comes from the current account database, not `$HOME`,
which is ordinary process input and may point into a checkout. The canonical
account home and any distinct canonical ambient `$HOME` are denied wholesale.
They are reopened narrowly for build caches,
per-user toolchain installs, and `.gitconfig`. Version managers keep the actual
compiler under home, so denying `.rustup` or `.nvm` removes the tool rather than
protecting anything. A few paths are then denied again because they sit inside
something reopened: cargo keeps registry tokens beside its package cache, and
the XDG data directory holds the Linux keyring beside legitimately shared files.

Every reopen rejects symlink ancestry. Known credential symlink targets from
the account home are denied at their resolved locations after all reopens; an
ambient `$HOME` can never inject host paths into the policy. The lists are
`homeReadable` and `homeSecrets` in
`internal/execution/homepolicy.go`.

The cost is real: a tool that reads config from an unlisted place under home
will not find it, and the fix is to add the path rather than to widen the
policy. That is the trade being made on purpose.

## Verification

Whether confinement is available is not a constant, build tag, or user
selection. `Capability` carries a `*Confinement`, which is produced only by a
self-test that passes on this machine. The same value wraps the command. The
two cannot disagree. `Run` fails closed when it has a confinement it cannot
apply.

The self-test checks the security-critical direction, that things which must be
refused are refused, plus enough of the allowed direction to catch a profile
that simply breaks everything. Reads of a hidden file are checked by content
rather than exit status, using a canary written into Switchboard's own state
directory, because a hidden file can surface as an empty successful read rather
than an error.

Results cache in the canonical account home at
`.switchboard/sandbox-check.json`, never beneath ambient `$HOME`, keyed by a
hash of the profile and by the host's OS build or kernel. Editing the confinement or
updating the system invalidates the cache, because neither should inherit an old
pass. Any failed assertion refuses the whole profile rather than trusting the
part that still works.

Whether real toolchains still function is checked by the platform-tagged tests
rather than at startup, since it needs compilers the user may not have.

## Network

Filesystem access and network egress use separate permission decisions.

When verified confinement is active, `NetworkLoopback` is the default so
fixture servers on ephemeral ports still work. Linux runs the command in a
private network namespace: only that namespace's loopback is available and it
has no route off the machine. macOS Seatbelt instead permits the host's
loopback services. Switchboard strips proxy environment variables before
launch, but an explicitly used local proxy or forwarder can still reach other
destinations with that service's authority. Treat host-loopback services on
macOS as a separate trust boundary, not as proof of off-machine isolation.

`NetworkFull` is requested per command through the `network` field on the `exec`
tool. The permission decision includes that request. Default, acceptEdits, and
bypass modes surface it to the user. Under active verified confinement, auto
mode can include an explicit full-network request in bounded model review. Plan
mode denies it. The sandbox governs what a command reads and writes; it cannot
judge whether sending this workspace to the internet is what the user meant.

Under macOS Seatbelt, an ordinary loopback-only command skips auto review and
asks the user because it can address an existing host service. An explicit
full-network request remains eligible for auto review because that broader
reach is stated in the review packet. Linux direct argv remains eligible only
inside verified bubblewrap confinement. Both review packets disclose retained
host IPC authority. Shell form, inline interpreter code, sensitive commands,
and external tools always skip model review.

## Host services and IPC

Network namespaces and socket rules do not isolate every host-local IPC path.
The current Seatbelt and bubblewrap profiles expose some Unix-domain services
needed by supported toolchains or the host. Tested credential services and SSH
agent sockets remain blocked, but another reachable daemon acts with its own
authority outside the filesystem policy.

The execution posture therefore marks host IPC as shared on both platforms.
Auto review receives that fact in its bounded packet. Bypass cannot suppress a
human prompt until a verified profile isolates both host-local networking and
IPC.

With the sandbox off, or while `yolo` is active, a command has the host's normal
network reach regardless of the `network` hint. The permission decision marks
that full reach explicitly because loopback-only networking cannot be enforced
without containment.

Neither platform can filter by hostname, so there is no middle ground between
loopback and open egress. Do not add a per-domain option; it would not be
enforceable.

## macOS

The policy is `internal/execution/seatbelt.sb`, rendered with parameters passed
as separate argv elements so a workspace path containing a quote cannot rewrite
it.

**Paths must be pre-resolved.** Seatbelt fully resolves a path before matching,
so a rule naming `/tmp`, `/var`, or `/etc` never fires. `$TMPDIR` is the trap:
on macOS it is `/var/folders/...`, and a profile granting `/tmp` fails every Go
build with `operation not permitted`. A rule written against a symlink is worse
than a missing rule, because the profile still reads as strict.

**Later rules win, so denies go last.** Moving a deny above the grant it is
meant to override silently disables it. The home-directory rules are generated
in Go and appended after the embedded profile, because their number depends on
which toolchain directories exist on the machine. Paths there go into the policy
text rather than into a `-D` parameter, so they are escaped: a workspace named
with a quote would otherwise close the string literal and have the rest of its
name read as policy.

**`mach-lookup` is granted nowhere.** `securityd` answers keychain queries over
mach IPC, so denying the keychain files accomplishes nothing on its own:
`security list-keychains` returns real output through a broad `(allow
mach-lookup)` while the profile looks strict. Denying it entirely closes that,
and no toolchain tested needs it. If one ever does, grant the specific service
and re-run the keychain assertion.

**Binding a port needs `network-inbound` as well as `network-bind`.** With
`network-bind` alone `net.Listen` fails as though the kernel refused.

`sandbox-exec` has carried a deprecation warning in the headers since 10.8. It
emits nothing at runtime and remains what Apple's own software and Chromium use.
If Apple removes it, the self-test leaves capability unavailable: `on` fails,
`auto` remains host-direct, and the permission engine still governs commands.

## Linux

Confinement is a namespace construction rather than a policy language.
bubblewrap builds a filesystem view: the whole tree read-only, writable binds on
top, and empty mounts over what must not be readable.

**Order is the policy.** Mounts apply in sequence, so writable binds must come
after the read-only root and the deny mounts must come after those. A deny
placed before a bind covering the same path silently does nothing.

**A tmpfs is writable, so closing home takes two steps.** A `--tmpfs` over each
minimal account/ambient home cover hides everything under it, and then accepts writes into a filesystem that evaporates:
the real home is untouched, but the command sees success and a later one finds
nothing. `--remount-ro` turns that into the refusal it should have been, and it
has to come after every mount placed inside home, or bubblewrap cannot create
their mountpoints.

**A deny mount needs an existing mountpoint.** `--tmpfs` on a path that does not
exist fails the entire invocation, because bubblewrap cannot create the
directory under a parent that is already read-only. Absent paths are filtered
out, which is safe: there is nothing there to hide.

**A private network namespace comes with working loopback.** Unlike macOS, no
extra grant is needed; `--unshare-net` gives fixture servers a usable `lo` and
no route off the machine.

**The session bus is the keychain equivalent.** gnome-keyring, kwallet, and
anything else implementing the Secret Service API hand out credentials over
D-Bus, so hiding `~/.ssh` while leaving `$DBUS_SESSION_BUS_ADDRESS` reachable
repeats the mistake macOS made with securityd. The bus socket and
`$XDG_RUNTIME_DIR/keyring` are covered.

**`/bin/sh` is dash on Debian and Ubuntu.** The egress check cannot use
bash's `/dev/tcp`, because on those systems it fails identically whether or not
the network is reachable, which would make the assertion pass while measuring
nothing. The self-test uses `curl`, `wget`, or `nc`, and says so in its detail
string when none is available. The policy mapping itself is covered by a
deterministic test that needs no network at all.

**Unprivileged user namespaces are a kernel setting.** Some distributions and
hardening profiles disable them, and then bubblewrap cannot build a namespace at
all. That is a host property, not a defect, and it correctly results in
per-action approval.

**Killing the wrapper tears down the PID namespace,** so a timed-out build does
not leave its compiler running. Verified by liveness rather than by pid, since a
pid inside the namespace means nothing outside it.

## What this does not protect against

**Reads outside home are broad.** A secret stored outside the home directory,
in `/etc` or a shared directory, is readable. The asymmetry is the point, but it
is an asymmetry: this protects the place credentials normally live, not every
place they could.

**A reopened toolchain directory is trusted wholesale.** `.rustup` and `.nvm`
are readable so their compilers work. A credential stashed inside one, beyond
those in `homeSecrets`, is readable with it.

**Cache directories are a persistence vector.** Granting writes to account-home
`.cargo`, `.npm`, and the rest is what makes a second build fast. It also lets a command
plant a config or a compiled artifact that a later, separately approved command
executes. Confinement is per command; it is not a durable boundary between
commands.

The granted list holds what has actually been exercised under confinement, so
Java and Gradle are not covered yet. On Linux account-home caches are created
component-by-component through a rooted, no-symlink directory capability.
Caches under a distinct ambient `$HOME` are ephemeral tmpfs mounts instead:
Switchboard never follows or mutates checkout-controlled cache paths before
confinement starts.

**The temp directory is writable,** because build tools are unusable without it.
A confined command can write anywhere in it, including files another process may
later read.

**Nothing here confines the model.** The sandbox constrains commands. It does
not constrain what the agent reads into context and sends to a provider. The
outbound policy and credential gate cover that separate boundary; see
[Security](security.md#outbound-secret-checks).

## Adding a toolchain

When a tool fails confined:

1. Run it under the confinement directly to see the refusal.
2. If it needs to read something under home, add it to `homeReadable` in
   `internal/execution/homepolicy.go`. If the directory also holds credentials,
   add those to `homeSecrets` in the same file.
3. If it needs to write outside the workspace, add its cache with a comment
   naming the tool, and weigh the persistence cost above.
4. If it needs a service the confinement blocks, grant that specific one, then
   re-run the credential assertions. A broad grant reopens the credential store
   on both platforms.
5. Add it to `TestInstalledToolchainsWorkConfined` so the next change does not
   silently break it.

Editing either confinement changes its key, which invalidates cached
verifications automatically.

## Testing Linux from macOS

The Linux path was developed and verified in a container:

```
docker build -f Dockerfile.linuxdev -t sb-linuxdev .
docker run --rm --privileged -v "$PWD:/src" -w /src sb-linuxdev go test ./...
```

`--privileged` is required because Docker Desktop's kernel does not allow
unprivileged user namespaces inside a container, which bubblewrap needs. Real
Linux desktops generally do allow them. A privileged container is a good proxy
for the construction but not proof about a specific host, which is why the
self-test still has to pass where the binary actually runs.
