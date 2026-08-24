# <img src="docs/assets/logo.svg" alt="Switchboard logo" width="28" valign="middle"> Switchboard

Switchboard is a terminal coding agent that routes each user turn across an
ordered ladder of model targets. Deterministic request signals choose the
opening tier; tool and test evidence can move it upward between completed
model rounds. `/why` explains every routing decision.

<img src="https://raw.githubusercontent.com/switchboard-code/switchboard/main/docs/tui.svg" alt="Switchboard TUI with model tiers, tool activity, routing history, and status" width="812">

The router is deterministic. It considers provider availability, tool and
vision support, context fit, cache state, and a hard dollar budget. A learned
router will ship only if it beats this policy on a clean multi-tier evaluation.

## Install

The five-minute path on macOS or Linux is:

```sh
curl -fsSL https://raw.githubusercontent.com/switchboard-code/switchboard/main/install.sh | bash
cd /path/to/project
sb
```

The installer verifies release checksums and writes `sb` to `~/.local/bin`.
Windows users can download the matching `windows_amd64` or `windows_arm64`
archive and `checksums.txt` from the [latest release](https://github.com/switchboard-code/switchboard/releases/latest).
To build from source with Go 1.26:

```sh
go build -o sb ./cmd/sb
```

See [Installation and configuration](docs/configuration.md) for shell
completion, updates, profiles, gateways, and the full config format.

## First run

With no configuration, `sb` opens a setup checklist:

1. Choose a reachable provider or local Ollama server.
2. Add credentials in the masked prompt when required.
3. Choose the model for tier 1.
4. Enter a task.

Setup can use an existing Codex CLI login through a credential helper. Keys
entered on macOS or Linux go to the OS credential service, not `config.toml`.
Windows currently requires an environment variable or credential helper; no
native credential-store backend ships there yet. Run `/setup` to reopen the
checklist, `/models` to add tiers, `/doctor` when a provider or tool is not
working, and `/doctor extensions` to expand retained startup diagnostics.

## The ladder

The user config is `~/.switchboard/config.toml`. A minimal ladder looks like
this:

```toml
[tiers.t1]
label = "local"
model = "ollama/qwen3.5:9b-mlx"

[tiers.t2]
label = "deep"
model = "ollama/qwen3.8:27b-mlx"
effort = "high"

[tiers.t3]
label = "codex"
model = "openai/gpt-5.6-sol"
surface = "subscription"
max_output = 8192
fallback = ["ollama/qwen3.8:27b-mlx"]
```

For a custom or unlisted model, `/models` asks for a finite output cap when no
verified one is available. A saved tier `max_output` applies to its primary
and every fallback, so an outage cannot replace a bounded target with an
unbounded one. It is both the provider limit and the hard reserve used for
context and budget checks. See
[Installation and configuration](docs/configuration.md#config-file).

Before each user turn, Switchboard routes the full request that would be sent,
including replay, tools, attachments, context size, live capabilities, cache
state, and retry-inclusive budget. `/t3` pins a tier. `/tier auto` resumes
automatic routing. `/t3 fix the flaky test` uses a tier for one prompt.

See [Routing and the model ladder](docs/routing.md) for escalation, fallbacks,
cache accounting, budgets, races, and the learned-router gate.

## The workbench

The TUI is the full coding workbench: source navigation, change review,
verification, and the model ladder in one terminal.

| Need | Surface |
| --- | --- |
| Open and search code | Ctrl+P opens the command palette; `/files` and `/search` provide revision-checked results and label partial coverage |
| Navigate semantics | `/outline`, `/symbols`, `/definition`, and `/references` use a trusted, installed language server; `/problems` labels diagnostic freshness and partial coverage |
| Review code changes | After `/trust grant`, `/diff` shows staged, unstaged, and untracked work plus a bounded omitted-path inventory; Git status can execute repository filters and hooks. `/review [turn]` shows one turn's recorded mutations without Git. |
| Edit against known state | Writes publish atomically. Edit and undo compare the expected file state immediately before publication and refuse a mismatch already present |
| Verify and recover | `/watch`, `/bisect`, `/undo`, `/fork`, and transactional `/retry` keep test evidence and recovery attached to the session record |
| Check what a turn claimed | `/audit` reads the finished turn on a second rung and reports where the closing message and the record disagree |
| Stop re-answering the same prompt | `[[permissions]]` holds standing rules; `/permissions` lists them and `sb permissions -- <command>` says what they answer |
| Explain the route | `/why`, `/estimate`, `/budget`, `/race`, and `/blame` connect model choice, cost, and surviving code |
| Resume with evidence | `/resume` and `/session` show the exact serving target, interrupted output, pending tool repair, continuity state, torn-tail recovery, and integrity-blocked logs before adoption |
| Survive interruption | Streamed assistant text is synced before it is shown; a crash keeps visible output as explicitly incomplete evidence and never replays it as a finished answer |
| Continue after a boundary | A validated seven-section handoff plus a bounded, redacted continuity capsule carries the active objective and execution frontier across restart, fork, retry, or compaction |

Owner-private prompt history persists per workspace (mode 0600 on Unix and a
verified current-user-only DACL on Windows). Messages entered during a turn queue
until it completes. Ctrl+G opens the prompt in `$VISUAL` or `$EDITOR`, Ctrl+R
searches prompt history, Ctrl+F searches the transcript, and Ctrl+O expands
tool rails.

The complete command and tool reference is in
[Sessions and command reference](docs/session.md).

## Scripting

The line-oriented `-repl` is deliberately smaller, and `-p` is headless. REPL
`/help` omits TUI-only file, change-review, task, and language-server views.

Run one turn without a TUI:

```sh
sb -p "explain the failing test"
git diff | sb -p "review this"
sb -workflow review internal/agent
```

Piped stdin is attached as content, so it cannot also answer approval prompts.
Bypass is prompt-free only when verified confinement isolates both host network
and host IPC. The current macOS and Linux profiles retain host IPC, so command
approvals still fail closed in a headless bypass run.

`-workflow <name> [arguments]` runs a staged subagent workflow without opening
an interactive prompt. `-output json` writes one JSON object to stdout and
sends the transcript to stderr. `-resume` and `-continue` reopen recorded
sessions. More examples are in
[Sessions and command reference](docs/session.md#scripting).

## Extending

| Surface | Current support |
| --- | --- |
| Skills | Switchboard, Codex `.agents/skills`, Claude `.claude/skills`, Unix Codex managed skills, and enabled plugin skills |
| Claude legacy commands | Recursive `.claude/commands`; manual `/skill` invocation only |
| MCP | Switchboard config plus explicitly activated Codex and Claude config; trusted plugin MCP; stdio and Streamable HTTP |
| Plugins | Local inventory, offline install, independent enablement, digest-bound executable trust; skills and compatible MCP assemble |
| Hooks | User hooks and trusted repository hooks at tool-call seams |
| Delegation | One subagent level, optional named agents, and up to four independent calls in one provider-launched delegate batch; shared permissions and labeled approvals |
| Language servers | Trusted projects can use installed `gopls`, TypeScript 7, `pyright`, or `clangd` through model tools and TUI semantic views |
| Computer use | macOS Accessibility control with per-application approval |

Native state never grants Switchboard authority by itself. MCP definitions need
Switchboard activation, applicable policy, workspace trust where required, and
a supported runtime feature set. Plugin MCP also needs current digest trust.
Unsupported authentication, transports, approval modes, and helper semantics
stay off.

See [Native extension compatibility](docs/extensions.md) for exact sources,
precedence, activation, and unsupported components. Computer control is
documented separately in [Computer use](docs/computer.md).

## Credentials

Credentials can come from environment variables, helpers, OAuth, or the OS
credential service. [Security](docs/security.md) covers storage, outbound secret
checks, workspace trust, external tools, and subscription OAuth.

## The sandbox

Command confinement is off by default. Start with `-sandbox` to require a
verified Seatbelt profile on macOS or trusted system bubblewrap on Linux.
Inside a session, `/sandbox on|off|auto|status` changes or reports the current
posture.
`on` refuses to activate when verification is unavailable; `auto` uses verified
confinement when present and otherwise stays visibly off.

Permission mode `auto` delegates an eligible command decision to the low-cost
reviewer only while verified confinement is active. Host-direct execution asks
the human because a workspace build can run arbitrary code. A confined,
explicit full-network request can still be reviewed; shared loopback, opaque or
interpreter commands, sensitive commands, and external tools stay human-gated.

Permission mode is separate. `bypass` is prompt-free only when verified
confinement isolates both host network and host IPC; neither current production
profile does. `yolo` is an explicit grant of full, unconfined host reach:
edits, commands, and external MCP or computer calls all run without asking;
deny rules and secret checks still apply. Windows
has no verified confinement profile.

See [Confining commands](docs/sandbox.md) for filesystem, network, and platform
details.

## Targets, not models

A target is a provider, serving surface, and model with its own price, cache,
and capability record. The same model served locally and through a gateway is
two routing targets. See
[Installation and configuration](docs/configuration.md#targets-and-provider-gateways).

## Where this stands

Version 1.21 adds crash-durable streamed output, read-only resume health,
validated seven-section compaction, transactional retry recovery, concrete
serving-target output caps, denial-first modal queuing, and staged subagent
workflows. The searchable workbench, deterministic model ladder, accounting,
trust model, extensions, and computer use remain. See the
[v1.21 release](https://github.com/switchboard-code/switchboard/releases/tag/v1.21.0).

The learned router is intentionally absent. The current evaluation does not
contain a clean choice between multiple useful tiers. The gate and evidence
are documented in [Routing evaluation](docs/eval.md).

## Documentation

| Topic | Document |
| --- | --- |
| Install, updates, providers, tiers, profiles | [Installation and configuration](docs/configuration.md) |
| Per-turn routing, escalation, cache, cost, races | [Routing and the model ladder](docs/routing.md) |
| TUI input, commands, history, verification, scripting | [Sessions and command reference](docs/session.md) |
| Permissions, credentials, trust, external effects | [Security](docs/security.md) |
| Skills, plugins, MCP, hooks, agents, custom commands | [Native extension compatibility](docs/extensions.md) |
| macOS application control | [Computer use](docs/computer.md) |
| Command confinement | [Confining commands](docs/sandbox.md) |
| Router measurements | [Routing evaluation](docs/eval.md) and [token estimator error](docs/estimator.md) |
| Comparative results | [Head-to-head results](docs/head-to-head-2026-08-16.md) and [product comparison](docs/comparison.md) |

Detailed behavior and command semantics live in the linked topic pages.

## Contributing

```sh
go build ./...
go vet ./...
go test ./...
```

These checks run offline without provider keys. `SB_LIVE=1` enables tests that
contact real servers. Repository constraints are in [AGENTS.md](AGENTS.md), and
[CONTRIBUTING.md](CONTRIBUTING.md) covers the development workflow. Report
security issues through [SECURITY.md](SECURITY.md).

## License

MIT. See [LICENSE](LICENSE).
