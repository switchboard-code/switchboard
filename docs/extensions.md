# Native extension compatibility

Switchboard reads native formats as data; it does not inherit another client's
authority. An enabled plugin, trusted workspace, remembered MCP approval, or
manual-only skill in Codex or Claude remains evidence about that client, not a
grant to Switchboard. Where a native control cannot be preserved, the affected
definition stays inspectable and does not run.

This page distinguishes three different claims:

- **Discovered** means Switchboard can identify and inspect the definition.
- **Activated** means Switchboard recorded its own decision for that exact MCP
  definition or installed plugin identity.
- **Assembled** means the component is actually part of a new session.

Those states are intentionally not synonyms.

## Startup diagnostics

Extension discovery can produce more useful detail than a small terminal can
show before the first prompt. Switchboard therefore renders a risk-first
summary of at most three 79-column ASCII lines. Routine problems are
deduplicated into at most five noncritical highlights; every retained
`fatal`, `critical`, `high`, or `required` failure remains visible. Category
counts say unique/total, and mandatory duplicates remain separate highlights.

`/doctor extensions` opens the static startup record in both the TUI and REPL.
It shows every retained diagnostic, terminal-sanitized, in discovery order,
with duplicates intact. The pre-surface buffer holds 200 entries. If discovery
produces more, a mandatory high-severity notice gives the exact dropped count
and says the missing text cannot appear in the drill-down. The report is not a
live health dashboard: later extension notices still reach the running TUI,
while the REPL drill-down remains the startup snapshot.

## Hooks

User hooks are declared in `~/.switchboard/hooks.toml`:

```toml
[[hooks.pre_tool]]
tools = ["exec"]
run = "./scripts/audit.sh"

[[hooks.post_tool]]
tools = ["write", "edit"]
run = "gofmt -w \"$SB_HOOK_PATH\""
```

A pre-tool hook can block a call. A nonzero exit or timeout is a denial, and
its output becomes the reason shown to the model. A post-tool hook runs after
the call and adds its output to the tool result. JSON on stdin carries the full
hook payload. Environment variables expose only the event, tool, path, and
workspace.

Hooks are standing user commands. They run unconfined and without a per-call
prompt. Repository hooks under `.switchboard/hooks.toml` therefore stay off
until `/trust grant` approves the checkout. User hooks under `~/.switchboard`
do not require workspace trust.

## Delegation and named agents

The `delegate` tool gives a self-contained task to a subagent with a fresh
context. The caller can choose a tier; otherwise the inexpensive tier is the
default. Subagents receive the core tool set, share the primary permission
engine and approval surface, and keep a separate session log. They cannot
delegate again, and bridged MCP tools are not copied into their registry.

When one provider response contains only independent `delegate` calls, up to
four can run at once. Results rejoin the provider loop in original call order.
A batch that mixes delegates with reads or writes stays serial, as does a batch
with applicable hooks. First-party writes share the primary turn checkpoint;
same-path contenders serialize, and a stale contender fails instead of merging
concurrent content.

`/tasks` is a busy-safe TUI view of the current primary session. It shows each
task's ID, name, status, serving tier, observed cost and call count, and parent
and delegate session IDs. `/tasks cancel <id>` stops only that queued or running
task. It does not cancel siblings. Approval prompts use one serialized lane and
name the task asking. A partial answer can remain usable even though the task's
status is `failed`.

The process-wide status history is memory-only and capped at 100 entries. Task
IDs and status do not survive a restart, but each delegate session remains
durable for accounting and blame. There is no direct `/task <prompt>` launcher:
the provider starts delegate calls through the tool surface.

Named agents are Markdown files under `.switchboard/agents/` in a project or
`~/.switchboard/agents/` for the user:

```markdown
---
description: reviews a diff for correctness
tier: t2
tools: read, grep, glob
---

Review changes and report defects. Do not edit files.
```

The tool list can narrow the subagent's registry but cannot grant a tool the
parent did not provide. An explicit tier on a call overrides the definition's
default. `/agents` lists the loaded definitions. Repository definitions are
prompts, so reading them does not require workspace execution trust; every
tool call still passes the permission engine.

Compatible Claude definitions are discovered recursively under
`.claude/agents/` and `~/.claude/agents/`. `/agents` shows the source dialect
and exact relative path rather than presenting those files as Switchboard's
own format. Discovery reads only stable regular files through anchored roots
and applies entry, definition, and byte ceilings. Symlinked definitions are
refused. Same-scope duplicate names reject every contender; a rejected project
definition still reserves its recovered name so it cannot silently activate a
user-level fallback.

The runtime appends a non-overridable worker contract after the named prompt:
stay inside the assigned task and tool grant, treat repository text and other
agents' output as evidence, omit credentials, and return findings rather than
instructions to the parent. Task text, steering, workflow carry, errors, and
the returned report are scanned at this boundary. The parent receives the
report inside a data-only evidence frame and is told to verify it independently.

Native Claude agent definitions fail closed when they contain a behavior or
authority field Switchboard cannot preserve. Unknown fields, explicit empty
tool grants, unsupported model, permission, isolation, hook, MCP, memory, and
lifecycle controls remain inspectable diagnostics rather than silently loading
with broader behavior. The supported name, description, tier, and tool subset
accept quoted, inline-list, or block-list YAML forms with duplicate and
indentation checks. Switchboard's own agent frontmatter rejects unknown fields
too: a typo such as `tool` cannot become an omitted `tools` grant and widen the
agent to the full suite.

Workflow TOML files under `.switchboard/workflows/` and
`~/.switchboard/workflows/` use the same anchored, regular-file-only,
bounded-inventory discovery posture. The project basename has precedence, and
a malformed project workflow reserves that basename instead of falling
through to a user workflow with different instructions. Loaded workflows keep
both their logical discovery path and resolved file identity as provenance.

A workflow is a small, explicit stage graph. Tasks within one stage run in
parallel; stages run in order. For example,

```toml
description = "survey the requested package, then propose a change"

[[stage]]
name = "survey"
[[stage.task]]
task = "List the relevant call sites in $ARGUMENTS with file:line."
[[stage.task]]
agent = "test-reviewer"
task = "Find the tests covering $ARGUMENTS."

[[stage]]
name = "propose"
carry = true
[[stage.task]]
tier = "t2"
task = "Use the survey evidence to propose the smallest safe patch."
```

`task` is required. `tier` and `agent` are optional; an explicit tier wins
over the named agent's default. `$ARGUMENTS` and `$1` through `$9` expand from
the words after the workflow name. `carry = true` gives each task the previous
stage's bounded, redacted answers in an untrusted-data frame. Definitions are
limited to four stages, four tasks in a stage, and eight tasks total. Workflow
filenames are their command names and therefore cannot contain whitespace.

Use `/workflow list`, `/workflow show <name>`, and
`/workflow run <name> [arguments]` in the TUI. For automation,
`sb -workflow <name> [arguments]` runs the same graph and exits. The flag is an
unattended surface even when stdin is a terminal: it never installs a question
or permission-prompt relay, so an undecided approval fails closed instead of
waiting for input. Standing permission rules or an explicitly wider `-mode`
still apply normally. Every expanded task, agent, and tier is resolved before
the primary startup session becomes resumable, so a typo cannot replace the
previous `-continue` candidate with an empty log.

## Custom commands and repository instructions

Custom commands are Markdown files in `.switchboard/commands/` or
`~/.switchboard/commands/`. `$ARGUMENTS` and `$1` through `$9` substitute user
arguments. `@file` attaches a file. An inline form such as `` !`git status` ``
expands by running a shell command, but only for commands from the user's home
directory. Repository command files cannot run inline shell during expansion.

Repository instructions use the first nonempty root file in this order:
`AGENTS.md`, then `CLAUDE.md`. `/init` asks the agent to create or revise
`AGENTS.md`; an existing `CLAUDE.md` does not change that target.

## Language servers

Language-server support is a built-in project integration, not an imported
plugin component. A server becomes available only when the project mapping,
installed executable, and Switchboard workspace trust agree. The current
mappings are `gopls` for Go, the TypeScript 7 compiler's native server for
TypeScript, `pyright` for Python, and `clangd` for projects with a
`compile_commands.json`.

The provider receives a frozen set of semantic model tools at session assembly.
The TUI exposes the same server through `/outline`, `/symbols`, `/problems`,
`/definition`, and `/references`. `/lsp` reports configuration and advertised
capabilities without starting the process. Outline, symbol, definition, and
reference queries start it lazily; `/problems` reads published diagnostics
without starting it.

Diagnostics are an honest partial view of what the server has published. The
Problems panel labels freshness and coverage, and an empty view does not claim
that the whole workspace is clean. Results outside the workspace remain
copy-only. Native plugin LSP declarations are still inventory-only, as shown in
the plugin table below; recognizing their manifest field does not attach them
to this built-in integration.

## Skills

| Format | Repository or workspace | User | Managed | Session behavior |
| --- | --- | --- | --- | --- |
| Switchboard | `.switchboard/skills/<name>.md` or `<name>/SKILL.md` | `~/.switchboard/skills/` | None | Loaded directly |
| Codex Agent Skills | `.agents/skills/<name>/SKILL.md` from the working directory through the first Git root | `~/.agents/skills/` | `/etc/codex/skills/` on Unix | Loaded directly |
| Claude skills | `.claude/skills/<name>/SKILL.md` from the working directory through the first Git root | `~/.claude/skills/` | None | Loaded directly |
| Claude legacy commands | Recursive `.claude/commands/**/*.md` from the working directory through the first Git root | Recursive `~/.claude/commands/**/*.md` | None | Manual-only skill inventory; explicit `/skill` invocation only |
| Enabled plugin skills | Manifest-declared or conventional plugin skill roots | Same | Same | Loaded only from the exact Switchboard-enabled plugin |

Discovery happens once during session assembly because the descriptions are
part of the frozen tool schema. A file added by `/learn` therefore appears in
the next Switchboard run, not halfway through the current process. `/learn <name>` writes
the standard `.agents/skills/<name>/SKILL.md` layout.

Equal display names do not shadow one another across products. `/skills` shows
the complete inventory with selectors such as
`codex:repo:.agents/skills/review` and
`claude:user:review`; `/skill <canonical-selector> [args]` requires that exact
selector. A manual-only or blocked skill stays visible there even though the
model cannot see it in the tool schema.

Legacy Claude command files use canonical path selectors such as
`claude:repo:.claude/commands/deploy.md`,
`claude:repo:apps/api/.claude/commands/ops/review.md`, and
`claude:user:team/review.md`. They are always excluded from the model-visible
skill schema and can be invoked only through explicit
`/skill <selector> [args]`. A
same-scope `.claude/skills/<name>/SKILL.md` wins over every command whose
basename is `<name>`, with an omission diagnostic. A project skill and a user
command (or the reverse) coexist under their exact selectors. That deliberate
cross-scope behavior avoids silently importing Claude's personal-over-project
authority decision.

The portable `name`, `description`, and `when_to_use` metadata is honored.
Switchboard also preserves these invocation controls:

- Codex `agents/openai.yaml` `allow_implicit_invocation`;
- Claude `disable-model-invocation` and `user-invocable`;
- Claude `argument-hint`, named `arguments`, `$ARGUMENTS`, indexed arguments,
  and named placeholders for explicit `/skill` invocation;
- Claude `paths` as a block on automatic model exposure while leaving an
  otherwise safe skill available to explicit invocation.

Legacy commands honor their description and argument hint, derive a
description from the body when needed, and use the same safe static argument
substitution. Their filename is the native identity, so frontmatter cannot
rename them. Dynamic shell/context forms, implicit attachments, or unsupported
behavior controls block invocation; reading a command never executes it.

A skill is blocked when honoring it would require behavior Switchboard has not
implemented. That includes native tool allow/deny lists, forced model or
effort, alternate context or agent selection, background execution, hooks or
shell controls, Codex tool dependencies, Claude shell interpolation, Claude
dynamic-context substitutions, and Claude `@` attachments. Malformed safety
metadata also fails closed. Codex UI-only `interface.default_prompt` is not an
instruction and is ignored; the `SKILL.md` body remains the prompt.

Skill resources are read only beneath the resolved skill directory. Symlinks
cannot retarget the definition or supporting-file root after discovery, and a
skill never executes merely because it was read.

### Learning a skill

`/learn <name>` distills the current session into
`.agents/skills/<name>/SKILL.md`. The distillation runs outside the agent loop,
uses the configured summarizer tier when present, has no tools, and does not
append its drafting exchange to the session. A credential scan runs before the
file reaches disk.

The new skill is offered on the next Switchboard run because discovery and the
model-visible schema are fixed for the process. Each generated pack records the
source session, date, and writing model so maintainers can compare the method
with its evidence and remove it when it becomes stale.

## Plugins

Switchboard recognizes Codex `.codex-plugin/plugin.json` bundles and Claude
`.claude-plugin/plugin.json` bundles, including the bounded conventional Claude
layout when no manifest exists. Native inventory comes from exact local
sources:

- Codex user and workspace `.agents/plugins/marketplace.json` indexes and
  configured local marketplace paths;
- Claude `~/.claude/plugins/installed_plugins.json` (or the corresponding
  `CLAUDE_CONFIG_DIR` path), joined with applicable local, workspace, and user
  `enabledPlugins` settings and the same authoritative platform policy loader
  used for MCP: managed-settings.json, managed drop-ins, server-managed remote
  settings, and OS-managed/registry surfaces in their native precedence. A
  managed false remains an authoritative deny after the native source is
  updated or removed because the exact marketplace identity is persisted with
  Switchboard's cached activation. A detected authoritative source that cannot
  be decoded quarantines all Claude plugin behavior for that run.

Codex marketplace records are availability inventory only, even when the
native plugin entry is enabled. A Claude installed record becomes eligible for
activation only after a unique installed-root and manifest-identity join. That
eligibility does not depend on the native enabled bit, but an applicable
managed false or ambiguous equal-precedence managed setting denies it. Neither
inventory state grants Switchboard authority. Native discovery does not crawl
caches or the network; it is capped at 64 catalogs, 256 entries per catalog, 32
Claude settings layers, and 64 installed records.

Catalog availability, installation, native enablement, Switchboard enablement,
and executable trust remain separate columns in `sb plugins list`. `/plugins`
exposes the same inventory inside the TUI. The actions are:

| Action | Effect |
| --- | --- |
| `inspect <selector>` | Shows normalized components, digest, provenance, warnings, and both native and Switchboard state |
| `install <selector>` | Copies an available exact local source into `~/.switchboard/plugin-cache/` and enables that cached copy |
| `enable <selector>` | Enables one freshly rediscovered installed identity for the next Switchboard run |
| `disable <selector>` | Removes enablement and executable trust for that exact identity, including a stale `saved:` recovery selector |
| `trust <selector>` | Grants executable trust to the current plugin-tree digest; the plugin must already be enabled |
| `untrust <selector>` | Revokes only executable trust, including for a stale `saved:` recovery selector |

Switchboard's decisions live in the per-user `~/.switchboard/plugins.json`
ledger. Workspace-scoped entries are also bound to the resolved workspace.
The ledger stores a private random recovery key; list output derives opaque,
stable `saved:` selectors from it when activated bytes can no longer be
rediscovered. These selectors permit exact disable or untrust without
printing the full ambiguous identity.
Live activations expose the same selector, so records that share a plugin ID
and cache path across user/project scopes remain individually removable. State
mutations take a bounded kernel-backed sidecar lock, reload the latest validated
ledger under that lock, and honor cancellation before publishing the atomic
replacement; a stale CLI process cannot resurrect a revoked entry.
Another client's enabled or trusted bit is displayed as provenance but cannot
write this ledger. An applicable managed native denial may still forbid
activation; a restriction can cross the boundary even though permission
cannot. If plugin bytes change, the old executable-trust digest is reported as
changed and no longer authorizes them.

Installation is intentionally an offline local copy operation. The source must
already be present and pass exact discovery again before and after copying. The
destination is content-addressed by plugin ID and digest and is published
without replacing an existing object. Installation does not clone Git, fetch
an archive, contact a marketplace or package registry, resolve npm, run an
install hook, or execute a lifecycle script.

Component support is deliberately narrower than discovery:

| Plugin component | Discovered and included in digest | Assembled today |
| --- | --- | --- |
| Skills | Yes | Yes, after Switchboard enablement; executable trust is not needed |
| MCP declarations | Yes | Yes, for one exact Switchboard-enabled root after digest-bound executable trust, managed policy, and runtime-feature gates pass |
| Hooks | Yes | No; detection and executable trust do not register them as Switchboard hooks |
| Agents and commands | Reported as unsupported | No |
| Apps, workflows, monitors, themes, settings, output styles | Reported as unsupported | No |
| LSP declarations | Reported as unsupported executable capability | No |

This distinction prevents a recognized manifest field from being mistaken for
a working integration. Plugin MCP uses the same typed stdio/HTTP adapter as
direct native MCP; it is revalidated before and after its declarations are
read, and changed bytes lose executable authority. Adding any remaining
inventory-only component requires a typed adapter and its own permission
boundary; it cannot be enabled by relabeling it as an existing Switchboard
feature.

## MCP runtime

The assembled MCP surface today combines `~/.switchboard/mcp.toml`, a trusted
workspace `.switchboard/mcp.toml`, activated compatible native Codex/Claude
definitions, and compatible MCP components from exact enabled and
digest-trusted plugins. It supports stdio and Streamable HTTP, static or
explicitly inherited stdio environment, a working directory, static HTTP
headers, header values read from named environment variables, bearer-token
environment variables, startup and tool timeouts, enabled/disabled tool
filters, and the typed `required` marker. Legacy Switchboard declarations also
have the existing per-tool `allow` list; native approval modes are not imported
as an equivalent. A required server that is shadowed by an earlier declaration,
cannot pass materialization, or fails to connect aborts session assembly and
closes the peers that did connect.

The client first probes stateless MCP 2026-07-28. It also implements the
initialization-based 2025-06-18 and 2025-03-26 revisions. An explicit supported
legacy version, the documented stdio probe boundary, or an unrecognized HTTP
400 can establish an older server. Authentication failures, rate limits,
server failures, cancellation, and recognized modern protocol errors do not.
Tool listing follows pagination. Cancellation reaches the active transport.
Where the transport permits a client response, server-initiated `ping` is
answered, and `elicitation/create` is answered on a surface that has a user.
The elicitation capability is declared at initialize only when one does, so a
headless run, a delegate subagent, and a race branch each leave the method
unserved rather than accepting a question nobody can hear. The dialog is the
`ask` tool's, and names the server that wrote the question. One request asks at
most four properties, in the order the schema wrote them, of type `string`
(free text, or a closed set of at most twelve `enum` values), `boolean`,
`number`, or `integer`; a schema outside that is refused as invalid params
rather than as an unserved method, because the method is served and this
request is not. A typed answer passes the credential scan and redacts before it
leaves. Declining and cancelling are reported as themselves. Sampling, roots,
and other ungranted client roles remain refused. Modern HTTP response POSTs are rejected explicitly. A mid-session
tool-list change is reported and takes effect only on the next Switchboard run.

An image block in a tool result reaches the model. The client decodes it and
the bridge queues it, and it is delivered at the next round boundary as an
injected user-role message rather than inside the tool result: every adapter
already maps an image inside a message and none has a captured mapping for one
inside a tool result, so this adds no adapter code and touches no wire format.
Delivery is gated on the bound target's recorded vision support, read live
because a move can change it, and a target the catalog does not price counts as
cannot. When an image is not delivered the result says how many and why rather
than omitting it silently. At most four images and four MiB ride out of one
call, and the result names the cap that bit. A block that claims to be an image
and does not decode is reported as such rather than handed over as bytes.
Audio and every other non-text block is still named and omitted.

MCP tools remain external effects. A server process is not inside the command
sandbox, so `bypass` never auto-approves its tools and `yolo` always does,
because that grant exempts nothing. An `allow`
entry is joined to the raw server and tool identity and becomes a permission
rule only after that exact bridged tool wins name-collision resolution and
registers.

Legacy Switchboard stdio declarations inherit the ordinary process environment
after SSH-agent sockets and secret-, token-, key-, password-, credential-,
auth-, session-, cookie-, database-URL-, and DSN-like variable names are removed
case-insensitively. `restricted_env = true` instead starts from a small
process baseline, adds only names in `inherit_env`, then applies the server's
explicit `env` values. A native-to-runtime adapter must use the restricted form
so importing a declaration cannot widen its environment by accident.

## Native MCP configuration

`internal/mcpnative` discovers and normalizes these native sources without
dialing a URL, expanding an environment variable, or reading a native
credential store:

| Client | Sources |
| --- | --- |
| Codex | Raw user `[mcp_servers.*]` and workspace-to-current `.codex/config.toml` are inventory only; executable definitions come from installed Codex app-server `config/read` with `includeLayers=true` for the canonical cwd |
| Claude | User and matching project-local entries in `~/.claude.json`; `.mcp.json` layers from the selected workspace root through the current directory; an explicitly resolved system `managed-mcp.json`, which is exclusive when present |

Codex app-server layers preserve native recursive merge and their exact
contributors across package, system, managed, cloud, user/profile, project,
and session sources. Claude server entries use whole-record native precedence.
Cross-client names remain dialect-qualified instead of receiving an invented
Codex-versus-Claude winner. Project and local entries require Switchboard's
workspace trust. Managed and user entries do not inherit another client's
workspace decision. Sensitive commands, arguments, URLs, environment values,
and headers redact in every ordinary rendering. If app-server is unavailable,
returns an incomplete stack, or reports a non-null requirements bundle whose
MCP semantics are not implemented, Codex execution is quarantined; raw TOML
inventory is never promoted as a fallback.

The normalizer preserves stdio, HTTP, SSE, and WebSocket transports; working
directories; static and forwarded environment; static, environment-backed, and
bearer headers; timeout forms; required servers; tool filters; approval modes;
OAuth and ChatGPT authentication requests; remote execution; header helpers;
tool-exposure controls; parallel-tool declarations; and `alwaysLoad`. Unknown
or invalid fields make the entry unsupported, and every non-baseline semantic
must be explicitly claimed by the eventual runtime before materialization.
Unreadable higher-precedence configuration quarantines lower entries instead
of failing open.

The materialization gate is already defined: it requires an explicit
Switchboard activation bound by a keyed digest to the whole winning definition,
the exact canonical config path, and its trust root; project/local trust; any
authoritative managed-policy decision; and a runtime feature claim for every
preserved semantic. Changing any bound input invalidates the decision.

`sb mcp list`, `inspect <id>`, `enable <id>`, and `disable <id>` expose that
native inventory and the per-user `~/.switchboard/native-mcp.json` activation
ledger; `/mcp` accepts the same actions inside the TUI, while bare `/mcp`
continues to show live connected servers. Native enabled state is reported
separately and never creates a Switchboard activation. A project or local entry
still needs `/trust grant` after activation.

Native materialization is wired into `cmd/sb` session assembly. An activated
entry starts on the next Switchboard run only if native enablement, managed
policy, project/local workspace trust, and every required runtime feature all
pass. The direct adapter supports stdio and HTTP; CWD and restricted local
environment forwarding; static, environment-backed, and bearer headers;
startup/tool timeout forms; tool filters; `required`; controlled Claude
`${VAR}` and `${VAR:-fallback}` expansion; and eager `alwaysLoad` assembly.
An optional activated entry that fails one of these gates stays off with a
diagnostic. A required activated entry aborts assembly.

The Codex app-server subprocess is on demand: it runs only when an existing
Codex MCP activation or an enabled, digest-trusted Codex plugin requires the
authoritative snapshot, plus explicit `sb mcp` inspection. Requests and output
are bounded, and a `codex` executable resolved inside the workspace is refused.
`configRequirements/read` must explicitly return null before Switchboard treats
the cloud/managed requirements check as empty; non-null requirements fail
closed until their MCP projection is supported.

Plugin MCP uses this same baseline after plugin enablement and current-digest
executable trust; its plugin root, source, definition, and trust root remain
bound to the materialized identity. It does not need a second `sb mcp enable`
record. Codex plugin MCP under managed plugin rules also needs one unique,
marketplace-qualified native plugin identity; the manifest name alone is never
used as that policy identity. OAuth and ChatGPT auth, native approval modes, SSE, WebSocket, remote
execution, header helpers, tool-exposure controls, parallel-tool declarations,
and unsupported plugin context expansion remain feature-gated and fail closed.
