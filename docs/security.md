# Security

Switchboard separates permission, containment, workspace trust, and credential
handling. A permission answer does not claim that a process is sandboxed.

## Permission modes

Tools declare an effect: read, write, execute, or external. The permission
engine applies rules and the active mode to that effect.

| Mode | Reads | File writes | Commands | External tools |
| --- | --- | --- | --- | --- |
| `plan` | Allowed | Denied | Denied | Denied |
| `default` | Allowed | Ask | Ask | Ask |
| `acceptEdits` | Allowed | Allowed | Ask | Ask |
| `auto` | Allowed | Allowed | Under active verified confinement, eligible direct commands receive bounded model review; host-direct, opaque interpreter, sensitive, and shared-loopback commands ask | Ask |
| `yolo` | Allowed | Allowed | Allowed with full, unconfined host reach | Allowed |
| `bypass` | Allowed | Allowed | Allowed only when verified confinement isolates host network and IPC; current production profiles ask | Ask |

External tools act outside the workspace and outside Switchboard's command
sandbox. MCP and computer-use calls therefore require an explicit rule or user
approval in every bounded mode, bypass included. Yolo alone covers them: it is
the everything-grant, and a grant that exempted the riskiest effect would not
be what it says. A remembered approval lasts for the
current session and is scoped to the permission identity, not to arbitrary new
tools that happen to sanitize to the same display name.

Auto mode uses a model reviewer only while verified command confinement is
active. With the sandbox off, or when an `auto` sandbox selection cannot apply a
verified profile, every execute effect asks the user because a workspace build
can run code. Eligible metadata goes to the `[slots] approver` when set;
otherwise Switchboard tries reachable ladder tiers from the bottom. The packet
contains the tool, path, argv, network request, effective reach, and retained
host IPC or loopback authority. It excludes file contents, environment values,
and external tool arguments. The
reviewer can allow, deny, or escalate. Missing reviewers, errors, invalid
decisions, and explicit escalation go to the user. A context with no listener
denies an unresolved command. The review result is recorded with the
permission event, and its model call is budgeted and tagged as approval work.
Shell form and inline interpreter code such as `sh -c`, `python -c`, or
`node -e` skip model review, as do sensitive commands and commands with shared
host loopback. An explicit full-network request under verified confinement can
remain reviewable, with retained host IPC authority disclosed. Eligible Linux
direct argv is reviewable only while verified bubblewrap confinement is active.
Windows execution remains human-gated because no verified profile exists and
descendant cleanup is not guaranteed. Outside yolo, external tools always ask
unless an explicit rule or remembered human answer covers them.

Human approval views escape terminal controls. A very large command is visibly
shortened while retaining its executable and early flags, its tail, and an
omitted-character count; the shortening is display-only. All modal prompts use
one FIFO broker, approvals start on No, and cancelling the terminal interaction
denies or declines every queued waiter. Narrow-terminal rendering keeps the
warning and safe choice visible rather than clipping the decision.

`yolo` is deliberately unconfined and exempts nothing: writes, commands,
sensitive commands, and external tools all run without asking. It does not
override explicit deny rules or the outbound-secret gate. The current sandbox
selection is retained while `yolo` forces full host reach, so leaving `yolo` can
restore the selected posture.

## Command containment

The sandbox is off by default. Approved commands then have the user's normal
filesystem and network reach. `sb -sandbox` requires verified confinement at
startup. `/sandbox on|off|auto|status` changes or reports the posture for the
running process. `on` fails if the host cannot apply its verified profile;
`auto` uses the profile when available and otherwise stays visibly off. A
successful interactive change is also saved as the next launch default.

Switchboard uses Seatbelt on macOS and a provenance-checked system bubblewrap
on Linux. A confined command can write inside the workspace and approved build
caches, cannot read
credential stores or their service sockets, and cannot open direct
non-loopback connections unless its permission decision authorizes network
access. On macOS, an existing host-loopback service can act with its own
authority. Both production profiles expose some host-local IPC; see the
platform details before treating confinement as isolation from local services.

Windows has no verified containment profile. `/sandbox on` fails there and
`auto` remains off. Permission mode still decides whether a host-direct command
asks, is denied, or receives the explicit yolo grant. The full filesystem and
network contract is in [Confining commands](sandbox.md). In yolo mode,
descendant processes may survive cancellation; the approval reason states this
limit.

## Workspace trust

A checkout can declare MCP servers and hooks under `.switchboard/`, its build
graph can control a language server, and Git status can invoke repository
filters or hooks while constructing `/diff`. None of those processes start
until the workspace is trusted. `/diff` therefore refuses before its first Git
command unless the checkout has a standing grant; `/review` remains the
Git-free recorded-turn view.

`/trust` previews the exact declared MCP servers, hook commands and tool
filters, and language-server candidate without starting them. `/trust grant`
records permission for the resolved checkout, including Git's repository
execution behavior. `/trust revoke` removes it.

User files under `~/.switchboard` are treated as the user's own configuration.
Native Codex and Claude project MCP entries still require Switchboard trust;
another client's remembered trust does not cross the boundary. Plugin
executable trust is separate and digest-bound. See
[Native extension compatibility](extensions.md).

## Credential lookup

Local targets need no credential. Other providers resolve credentials in this
order:

1. `SB_<PROVIDER>_API_KEY`, or the vendor's standard environment variable;
2. a configured credential helper;
3. an OAuth login;
4. the OS credential service, using Keychain on macOS or Secret Service on
   Linux. Windows builds currently have no native credential-store backend.

`/login`, `sb auth`, and first-run setup read secrets from a masked prompt or
stdin. Secrets are not accepted on the command line and are not written to
configuration, session logs, or errors. Status output uses placeholders.

There is no encrypted-file fallback. An owner-only file (0600 on Unix; a
protected current-user DACL on Windows) controls access but does not encrypt
its contents. On Windows, or any machine without a credential service, use an
environment variable or helper:

```toml
[auth.anthropic]
helper = ["op", "read", "op://vault/anthropic/credential"]
```

The helper is provider-wide. A helper that returns a Codex CLI token follows
Codex's expiration schedule; running `codex` refreshes that login. `/setup` can
configure this helper when it finds an existing Codex login.

## Outbound secret checks

Before a prompt leaves the machine, Switchboard scans it for known credential
prefixes. The scan includes file attachments and output injected by `!cmd`.
It uses known issuer formats rather than entropy guesses so a match has a clear
meaning.

TUI `@path` attachments are opened at submit time through a capability bound to
the canonical workspace. A path that escapes that root, a replaced
root, parent, or file, a non-regular file, or a file that exceeds its bound is
not attached; the prompt receives an explicit omission note instead.

In the TUI, a match offers three choices: redact it, send the original text, or
drop the prompt. Redaction tells the model where a credential was removed. A
headless `sb -p` run cannot ask, so it refuses the send unless
`-allow-secrets` is present. Diagnostics name the credential type and prefix
without repeating the value.

Web queries and URLs are scanned before egress. Outside yolo, the first
request to a host asks for approval, and that approval covers only that host
for the session. A
redirect to a different host is refused. No permission mode skips this check.

Computer-use text is scanned before it is typed into another application, and
text read from applications is redacted before it enters the session. See
[Computer use](computer.md).

Prompt history is an owner-private, per-workspace JSONL record: mode 0600 on
Unix and a protected DACL with exactly one current-user allow entry on Windows.
The program verifies that protection through the open file handle before every
write and after atomic publication; a filesystem that cannot enforce it loses
persistent history rather than receiving a weaker copy. Legacy plaintext
history is accepted once and rewritten in the bounded format. Credential-shaped
text is redacted before persistence, so choosing “send as typed” for one
outbound turn does not silently create another durable copy in history.

The cross-agent boundary is scanned separately. Tasks, steering, workflow
carry, failures, and child results are redacted before they cross; every child
receives a final runtime contract after any repository-defined agent prompt.
Child reports return inside an explicit untrusted-evidence frame. A worker can
specialize its assignment, but its report cannot widen the parent's task,
permissions, or trust decisions.

## Configured processes

Agent-requested exec commands, language-server and provider probe processes,
and editor launches use the central scrubbed child environment. Explicit user
`!` shell commands and custom commands intentionally retain the ambient
environment: they are the user's own commands, not agent-requested execution.

Legacy Switchboard MCP child processes inherit the ordinary environment after
SSH-agent sockets and names associated with secrets, tokens, keys, passwords,
credentials, authentication, sessions, cookies, service URLs, and DSNs are
removed case-insensitively. Restricted and native declarations start from a
small baseline, add only named forwarded variables, then apply explicit
environment values.

Hooks are the user's standing commands. They run without a per-call prompt and
outside the command sandbox, which is why repository hooks require workspace
trust. An armed `/watch` verifier is also unconfined, but only the user can arm
one; repositories cannot declare it.

## OpenAI subscription OAuth

`sb auth oauth login openai/subscription` uses the OAuth client registered by
OpenAI's Codex CLI. Switchboard is not affiliated with or endorsed by OpenAI,
and OpenAI does not publish this flow for third-party clients. Accounts have
been actioned for using it. OpenAI's terms govern the account.

A client you register under `[auth.openai.oauth]` takes precedence over the
bundled client.

## Update integrity

Startup release checks are notice-only by default. Manual `/update` and
`sb update`, or an explicit `/update auto on`, download one platform archive
and verify its exact SHA-256 digest against `checksums.txt` from the same
GitHub release before replacing an executable. This detects corruption and
mismatched assets, but it is not independent publisher authentication: a
release-account compromise could replace both the binary and its checksum.
Switchboard does not silently opt users into that trust model, and it does not
replace package-manager-owned installations.
