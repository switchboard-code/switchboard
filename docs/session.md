# Sessions and command reference

Switchboard's default interactive surface is the full terminal workbench. The
line-oriented REPL remains available with `-repl` for constrained terminals and
script-oriented use, and `-p` runs one headless turn. The REPL is deliberately
smaller: its `/help` lists only the commands it implements. Fullscreen file,
change-review, task, and language-server views are TUI-only.

## TUI state

The prompt frame shows the permission mode when it differs from `default`. The
cursor and tier labels use the active tier's color. The status area shows the
active ladder position, route moves, streaming token rate, cost in its native
metering, context occupancy, session age, and current verifier state. Less
important decoration disappears first when the terminal becomes narrow.

Tool rails and route records expand with Ctrl+O, or with a mouse click once
`/mouse on` has handed the mouse to the session. Ctrl+F searches the transcript
from newest match to oldest and marks every match in the margin. Ctrl+P opens a
searchable palette over the TUI command registry.

## Input

| Input | Behavior |
| --- | --- |
| `@path` | Completes a workspace path and attaches its contents |
| Image path or screenshot mention | Attaches an image only when the selected target has live or catalog-verified vision support |
| `!cmd` | Runs a command immediately as the user and carries its output into the next turn |
| Trailing `\` | Continues the prompt on another line |
| Ctrl+G | Opens the current prompt in `$VISUAL`, falling back to `$EDITOR` |
| Ctrl+R | Searches prompt history for the workspace |
| Ctrl+F | Searches the transcript |
| Ctrl+P | Opens the searchable command palette |

If the target cannot accept an image, Switchboard refuses the attachment and
states the missing capability. It does not send an image to a target that may
ignore it.

Prompts entered while a turn is running join a queue. `/queue` shows them and
`/queue clear` removes them. They run after the active turn completes.

To redirect the running turn instead, type the correction and press `ctrl+s`
(or use `/steer <text>`): the words reach the model at the turn's next round
boundary, marked `[steer]` in the log. What the turn ends before reading is
not dropped — it leads the queue and starts a turn of its own. A session swap
drops what was never delivered, and `/tasks steer <id> <text>` addresses a
delegate task rather than the primary.

## Questions and approvals

The `ask` tool can present up to several choices plus a free-text answer. Arrow
keys and Enter choose one item, digits select by number, and Space selects
multiple items when allowed. Escape declines the question; the model receives
that decline as an answer and continues.

An approval or question can ring the terminal bell. In a headless run,
delegated task, or race branch, no listener exists. `ask` then tells the model
to choose and state an assumption instead of waiting. Free-text answers are
scanned for credentials before they enter the log.

Tool approvals from parallel delegates take one serialized lane and name the
task asking, so prompts do not overlap or lose their owner.

## Common commands

| Command | Purpose |
| --- | --- |
| `/models` | Browse available models and bind tiers |
| `/tiers` | Show the ladder and active profile |
| `/t3` | Pin the session to tier 3 |
| `/tier auto` | Resume automatic per-turn routing |
| `/routing on|off` | Let the policy move the rung, or hold the current one |
| `/why` | Explain routing decisions and reprice the session on other tiers |
| `/think high` | Change reasoning effort for the active target |
| `/context` | Show estimated system, tool, and conversation use separately from provider-reported usage |
| `/compact [preview]` | Preview or perform context compaction |
| `/budget 2.50` | Set the persistent dollar ceiling |
| `/destinations` | Show or set the providers this workspace's turns may reach |
| `/permissions` | Show the standing rules that answer without asking |
| `/audit` | Check the finished turn's claims against its record on a second rung |
| `/estimate <prompt>` | Estimate the next assembled request on every tier |
| `/cache` | Show the cache belief used by routing |
| `/doctor` | Run startup, credential, sandbox, tool, and MCP checks |
| `/doctor extensions` | Inspect every retained startup extension diagnostic in discovery order |
| `/tasks [cancel <id>]` | Inspect current-session delegate work or cancel one queued or running task |
| `/setup` | Reopen provider setup |
| `/mode <plan|default|acceptEdits|auto|yolo|bypass>` | Change the permission policy, including mid-turn |
| `/sandbox on|off|auto|status` | Change or inspect command confinement for this process |
| `/theme <dark|light|auto>` | Set the persistent TUI theme |
| `/notify [on|off]` | Control completion and approval notifications |
| `/mouse [on|off]` | Give the wheel and click-to-expand to sb, at the cost of the terminal's own text selection |

Routing, budget, cache, and cost semantics are detailed in
[Routing and the model ladder](routing.md).

Startup extension notices use a bounded risk-first summary. In both the TUI and
REPL, `/doctor extensions` opens the retained, sanitized record with duplicates
intact. It is a startup snapshot, not a live health dashboard. Buffer overflow
is never silent: the summary reports the exact dropped count and says that the
dropped text is unavailable. See [Startup diagnostics](extensions.md#startup-diagnostics).

`/tasks` is a busy-safe TUI command, not a direct task launcher. It reports the
ID, name, status, tier, live call count and observed cost, and parent and
delegate session IDs for this primary session. The process-wide history is
capped at 100, and its
IDs live only in the current process, although delegate session logs remain
durable. Targeted cancel does not cancel sibling tasks. The full batch rules are
in [Delegation and named agents](extensions.md#delegation-and-named-agents).

## Scheduled prompts

`/every <interval> <prompt>` arms a recurring prompt, `/at <HH:MM> <prompt>`
a one-shot at a 24-hour local clock time, and `/schedule` lists what is armed
with each entry's next fire, both relative and on the wall clock.
`/schedule cancel <id>` removes one. A fired entry opens an ordinary user
turn with a `[scheduled sN]` lead, so the transcript and the model can tell
it from typed text; a turn already in flight delays the fire into the queue
rather than dropping it.

Entries persist per workspace, in `schedule.json` beside the session logs
under `~/.switchboard/sessions/`, and resume when sb next runs in that
workspace. There is no daemon: nothing fires while sb is not running. An
entry whose moment passed while the process was down fires once at startup,
and a recurring entry reschedules from then — it never catches up the ticks
it missed. The TUI checks what is due every few seconds; the REPL checks
before each prompt is read, so an entry that comes due while a line reader
sits idle waits for the next line.

Intervals start at one minute and a workspace holds at most 32 entries. A new
entry's prompt passes the same credential scan a typed prompt does, and
because the ledger keeps it on disk the answers are redact or drop — the
TUI's gate offers no as-typed arm and the REPL refuses outright, naming the
finding kinds. Firing replays what was armed; the schedule adds no gate of
its own beyond the outbound scan every send passes.

One running sb owns a workspace's ledger; a second sb opened in the same
workspace reports the schedule as held and fires nothing. Headless `-p` runs
never load it. Ids are the lowest free number and are reused after a cancel,
so an old transcript's `[scheduled s2]` can name a different prompt than a
new entry's.

## Workspace workbench

The TUI keeps basic code navigation close to the running session:

| Command | Behavior |
| --- | --- |
| `/files [query]` | Quick-open workspace files from a revision-aware index, with source preview, filtering, copy, and `$VISUAL` or `$EDITOR` handoff |
| `/search <literal>` | Search bounded workspace text and inspect exact file-and-line matches |
| `/diff` | Show the working tree against `HEAD`, including staged, unstaged, and untracked files, without changing the Git index |
| `/review [turn]` | Review the exact recorded write/edit mutations for one agent turn without changing files or checkpoint state |
| `/lsp` | Report configured language-server state and advertised capabilities without starting a process |
| `/outline <path>` | Browse semantic declarations in one source file |
| `/symbols <query>` | Search semantic declarations across the workspace |
| `/problems [path]` | Browse published diagnostics with freshness and coverage labels |
| `/definition ...` and `/references ...` | Browse semantic locations and open workspace results in an editor |

File and text results carry a content revision. If the file changes between
the result and the editor handoff, Switchboard refuses the stale action and
asks you to reopen the view. External language-server locations are copy-only;
the workbench does not open a file outside the workspace.

Workspace search labels partial evidence and counts truncated, skipped, or
oversized files. No matches in the files it could search is not presented as no
matches in the whole workspace. If `/diff` reaches its patch cap, it follows
the truncation marker with a bounded, sorted inventory of paths not fully shown
and the remaining count.

`/problems` consumes diagnostics the server has pushed for documents it knows
about. Rows are labeled fresh, stale, unversioned, or pending; the panel labels
its push coverage partial. An empty Problems view does not prove the workspace
is clean. Use the repository's verifier for that claim.

## Advisor and compaction

`/advisor` reports the current state; `/advisor on` assigns a second model to
observe loop events. It reacts to the same stuck-agent signals as the router
and injects advice at the next safe seam. It cannot edit files and has a
per-turn limit. Set `[slots] advisor = "t2"` to enable it by default.

`/compact` replaces older conversation with a summary in a fresh context. It
runs automatically after the last provider request crosses 85 percent of the
context window. `/compact preview` reports the messages and estimated tokens
that would be replaced, the content that remains fixed, and the model that
will summarize. `[slots] summarizer` assigns a dedicated tier to this work.

### What crosses a context boundary

`todo` carries three fields beside the task list: `objective`, `next_action`,
and `stop_condition`. They are what the capsule takes across a compaction with
the list, and they were specified, validated, redacted, bounded, and rendered
long before anything wrote them, so a continuing model used to inherit
checkboxes whose point had been left behind.

`objective` and `stop_condition` are kept until changed, because the list moves
far more often than the reason for it. `next_action` is cleared by every call
that does not set it: it names the very next step, and the call that changed
the list is the moment it stopped being true.

When the window passes 70 percent and automatic compaction is on, the model is
told once that the boundary is coming, what will cross it, and to set those
fields now while it still has the whole picture. Once per session, not once per
round: a warning repeated at every boundary is one that stops being read.

## Session history

| Command | Purpose |
| --- | --- |
| `/export` or `sb export [id]` | Write a Markdown timeline with route decisions, race verdicts, warnings, and messages |
| `/recap` or `sb recap [id]` | Summarize the opening prompt, turns, cost, route movement, touched files, race verdicts, and next resume/blame actions |
| `/find <text>` or `sb find <text>` | Search recorded prompts and responses case-insensitively |
| `/find all <text>` or `sb find all <text>` | Search every workspace and group matches by the workspace name stored in each log |
| `/fork [turns|pin]` | Continue from an earlier prefix in a new session log |
| `/pin [name]` | List pins or name the current point for a later fork |
| `/retry [tier]` | Revert the last captured turn and replay its recorded opening, optionally on another tier |
| `/resume` | Open a recorded session |

Inside a running session, bare `/recap` reads the previous log because the
current session is not where the user left off. `sb recap <id>` reads a
specific session.

Forking does not rewrite the original log. The copied message prefix remains
byte-identical, so a provider that still holds it may serve the branch from
cache. Files are not rewound by a fork.

`/retry` uses a fork at the last turn's opening and replays the exact recorded
message, including image and reference metadata. The discarded answer remains
resumable with a `user_corrected` label. File changes recorded by write and edit
are restored first. A partial or failed restore refuses the replay and remains
retryable until it is explicitly handled. Shell side effects remain. A tier
argument runs the replay there and then returns.

### Continuity capsules

The session log can carry a bounded continuity capsule beside the append-only
conversation. The production loop records a successful todo state and derives
its next action. Capsule content is normalized, size-limited, and scanned for
credentials before it is written.

After a restart or session swap, the newest undelivered valid capsule is stamped
into the next user opening before routing, estimation, and provider send, then
consumed. Pending and delivered state survives the swap, so an already-delivered
capsule is not injected again. It stays hidden from the visible transcript; the
prompt, history label, and retry label remain the text the user wrote. The swap
also restores active todos and revokes old file-read authority. Fork and retry
preserve a capsule only when its recorded message boundary belongs to the branch.

Compaction carries the live recorded todo state and derived next action when
present. It records immediate parent lineage and stamps the capsule once into
the compact seed without duplicating it in the generated summary. The next real
user opening is therefore clean.

A capsule is advisory. It tells the next model what the previous context
believed, but it does not grant file-read authority, relax permissions, or
replace verification. The model is told to check the workspace before writing.

## File history and verification

`/changes` lists files captured by write and edit, grouped by the turn that
changed them. It does not claim to see shell-command side effects.

`/review [turn]` is the TUI's read-only view of that checkpoint evidence. Bare
`/review` means the currently open user turn only; a no-op reports that it has
no recorded mutations and never falls back to an older turn. A positive decimal
selects a retained mutation turn, one-based and oldest first. The view covers
agent `write` and `edit` calls only, not shell commands, hooks, MCP tools, or
manual changes. `/diff` remains the repository-wide view against `HEAD`.

`/audit` puts the turn that just finished in front of a second model: the
closing message is the claim, the recorded tool calls with their results and
the recorder's captures are the evidence, and a finding is a place the two
disagree. It reads the record and not the code, so it does not review the work
or suggest changes. It takes no turn number: the message log and the
checkpoint recorder number turns separately, and the turn that just finished is
the one pair certainly the same in both. A turn that called no tools and
changed no files is reported as having nothing to check. The report states its
scope every time: how many calls and captures it read, that a shell command's
side effects are outside the recorder and so make a claim unchecked rather than
wrong, any paths that exceeded the capture bound, and any credential-shaped
strings redacted from the evidence before it was sent. `[slots] auditor`
assigns the rung that reads it; with none bound the audit runs on the rung that
made the claims and says so, because a model checking itself is the weakest
reading of its own work. Nothing it produces is appended to the session or
injected into the conversation.

Before current bytes appear, Switchboard rechecks existence, mode, size,
digest, the target identity, and the captured parent and ancestor identities.
A stale, unsafe, or redirected path is refused without disclosing its current
bytes. Created, deleted, truncated, empty, mode-only, and binary states are
named explicitly. Oversized pre-images are unavailable; a post-image beyond
the load budget is marked unverified instead of getting a content or digest
claim.

One load covers at most 256 selected-turn paths and 256 KiB of aggregate
content, with an exact omitted-path count. Rendering stops at 1,200 lines or
256 KiB and drops whole file sections rather than cutting a diff hunk. The
fullscreen view is physically bounded at 80x24, terminal-sanitized, and bound
to the launching session, workspace generation, checkpoint revision, selected
turn, and command invocation, so a late result is discarded. It is unavailable
while an agent runs and has no rollback, apply, editor, per-file, or per-hunk
action. It does not touch the checkpoint, Git index, or worktree.

`/undo <path>` restores one file to its state before the newest turn that
captured it. The checkpoint is consumed only after a successful restore.
`/undo` restores every captured file from the newest turn. Conversation
messages remain unchanged to preserve the sent prefix and its cache identity.
A restored file must be read again before a later edit.

Write and edit compare the expected file state immediately before publishing a
complete replacement atomically, and preserve the file's mode. Undo makes the
same pre-publication check against the current bytes, mode, and parent directory
identity captured for the mutation. A mismatch already present makes the
checkpoint stale; undo reports the path and leaves both the current file and
checkpoint intact. The comparison and final rename are not one atomic pathname
CAS, so a simultaneous external replacement at that seam cannot be excluded.
Oversized files are named as skipped rather than presented as recoverable.

`/blame <path>` and `sb blame <path>` replay recorded write and edit operations
against the current file. Each explained line includes the turn, tier, target,
and prompt. Lines created by a shell, typed by hand, or predating the logs are
reported as unknown rather than guessed. The replay spans recorded workspace
sessions, including delegate sessions.

Bare `/blame` summarizes surviving lines by target and metering. A location such
as `/blame cache.go:42` reports the line's writing turn, prompt, other files,
final response, and resume command.

`/mistakes` and `sb mistakes` group repeated test-shaped command failures by a
digit-normalized signature. Each result lists the sessions that encountered
it. A copied fork prefix counts as one observation. Failures printed outside
the exec tool are outside this record.

### Watch

Files the model read are watched for change by anything other than its own
write and edit calls: a formatter, a shell command, a branch switch, the user's
own editor. At the next round boundary the model is told which files moved, so
it re-reads before composing an edit rather than learning at the refusal. The
sweep stats what it tracks and hashes only a file whose size or timestamp
moved, covers at most 128 files, reports a change once until it changes again,
and names a file too large to re-hash as touched rather than claiming it
differs. Like advisor advice and watch reports, it reaches the model through
the TUI's round-boundary seam, so the REPL does not carry it. The write and edit stale check is unchanged: a notice is not evidence
the model read it, so the guarantee stays where it was.

`/watch <command>` arms a user-selected verifier. It runs after edit rounds and
again at turn end. Only a changed result is reported: a new failure or a
transition from red to green. The current status remains visible even when the
result is unchanged.

A new mid-turn failure can trigger escalation. A turn-end result informs the
user and the next prompt because the completed turn can no longer move.
`/watch off` disables the verifier. The setting survives `/clear`, `/fork`, and
`/resume` within the process.

The verifier runs unconfined as the user. Repositories cannot declare it. Bare
`/watch` may suggest a command from a real Makefile test target, Go module, or
npm test script, but the user must arm it.

### Bisect

`/bisect` finds the turn that changed an armed verifier from green to red. It
binary-searches per-turn checkpoints, reconstructs each candidate state, and
runs the verifier. `/bisect <command>` supplies a verifier for that run.

The original tree is restored on success, failure, or cancellation. The search
covers only changes captured by write and edit; current shell and manual
changes remain in every reconstructed state. Prompts queue while the bisect is
busy, and Escape cancels it. The result is attached to the next prompt so a
follow-up such as `fix it` carries the failing turn and first error.

## Accounting and routing records

| Command | Purpose |
| --- | --- |
| `/cost` | Current session cost |
| `/cost turns` | Cost by user turn, plus labeled compaction, learning, advisor, and command-approval work |
| `/cost rungs` | Cold counterfactual cost on every tier |
| `/stats` or `sb stats` | Workspace lifetime accounting |
| `sb stats all` | Accounting across all workspaces |
| `/ladder` or `sb ladder` | Opening and ending tier distribution |
| `/races` or `sb races` | Paired race verdicts |

The outputs preserve local, plan, and dollar units. See
[Routing and the model ladder](routing.md) for the accounting rules.

## Clipboard, selection, and notifications

`/copy` copies the last response. `/copy code` copies its newest fenced block,
and `/copy code 2` selects the preceding block, counting newest first across
session responses.

The mouse is on by default: the wheel scrolls the transcript, a click
expands a route or tool entry, and a drag selects whole lines and copies
them on release — the copy goes by OSC 52 where the terminal takes it and
by `pbcopy` on macOS, where Terminal.app takes no escape. The terminal's own
selection still works through its modifier (shift, option, or fn).
`/mouse off` hands the mouse back entirely. The
setting persists as `[ui] mouse`.

Nothing is lost while the mouse is off. `pgup` and `pgdn` scroll a page,
`shift+↑` and `shift+↓` scroll a few lines, `ctrl+u` and `ctrl+d` scroll half a
page, `home` and `end` reach the ends of the
transcript, and `ctrl+o` expands the last route or tool entry, which is what a
click on one would have done.

`/notify` controls the terminal bell for completed turns and waiting approvals.
The terminal title also marks active work. Notifications are enabled by
default, and `/notify off` persists the setting.

## Tool surface

The core registry includes read, write, edit, exec, glob, grep, todo, ask,
websearch, and webfetch. A second read of an unchanged file returns a short
marker because the bytes already exist in the model context.

Web tools ask before contacting a new host, reject cross-host redirects, and
scan outbound URLs and queries for known credential forms. See
[Security](security.md).

If `ast-grep` is installed, session assembly adds `astgrep` for syntax-tree
search. Install it on macOS with `brew install ast-grep`. It is always an
execution effect because a PATH-resolved binary can write, even when its query
looks read-only. Verified confinement can limit that process, but does not
reclassify it; the active permission mode still applies. Switchboard's own
bounded workspace index supplies file names and literal text, not semantic
inference. Language-server requests supply semantic results; an external hosted
index is a service destination and belongs behind MCP.

Language-server tools and TUI views join when the project type, installed
server, and workspace trust agree. Supported mappings are `gopls` for Go, the
TypeScript 7 compiler's native server for TypeScript, `pyright` for Python, and
`clangd` when `compile_commands.json` supplies flags. `definition` and
`references` accept a file, line, and symbol and return exact file-and-line
results. Outline, symbol, definition, and reference requests start the server
lazily; `/lsp` status and `/problems` do not.

On macOS, the optional `computer` tool controls applications through the
Accessibility API. Its permission model and tested limits are in
[Computer use](computer.md).

MCP, hooks, skills, plugins, custom commands, and delegation are documented in
[Native extension compatibility](extensions.md).

### Background commands

`exec` with `background: true` starts a command and returns a handle instead of
waiting, which is the only way to run a dev server, a watch build, or a
migration that outlasts a turn. The `proc` tool then lists what this session
started, reads what one has printed without consuming it, and stops one along
with everything it started.

The bounds are the point. A background command is killed at a one-hour ceiling
and when the session ends, because a process Switchboard started and forgot is
Switchboard's fault. At most eight run at once. Output goes to the same bounded
capture a foreground command uses, and a read returns the tail. Confinement is
applied by the same code that applies it to a foreground command and fails
closed the same way. `proc` carries the execute effect even though it starts
nothing, because a stop signals a process group and a read returns what a
running process wrote. A restricted agent granted `exec` without `proc` is
refused `background` rather than handed a process it has no verb for.

## Scripting

`sb -p "prompt"` runs one turn and exits. Piped stdin becomes an attachment:

```sh
git diff | sb -p "review this"
```

Because stdin supplied content, it cannot answer an approval prompt. A tool
that needs approval is refused and the reason is returned to the model. Bypass
is prompt-free only when verified confinement isolates both host network and
host IPC. The current macOS and Linux profiles retain host IPC, so command
approvals still fail closed in a headless bypass run. With `-mode yolo` a
headless run asks nothing at all: external tools and credential-shaped
commands run too, with no one present to ask.

`-output json` writes exactly one JSON object on stdout while the transcript
goes to stderr. The object contains the result, outcome, final tier and target,
tokens, and a cost object with separate local, plan, and dollar forms.

`-output stream-json` writes one JSON object per line as the run happens, so a
script can watch progress instead of waiting for the end and then parsing
English. Every line is complete and carries a `type`: `init` names the session,
rung, target, and permission mode before anything runs; `text` and `thinking`
carry the model's output as it streams; `tool_start` and `tool_end` name the
tool, its display detail, its effect, whether it failed, and how long it took;
`notice` carries what the run needed to say; `usage` carries what a call
consumed. The last line is always `type: "result"` holding exactly the object
`-output json` prints, so a consumer that only wants the outcome reads the last
line and one written against `-output json` needs no rewriting. Nothing is
emitted that the loop did not report.

The exit status is the same either way: 0 for a completed run, 1 for an error,
2 for a usage mistake, and 130 for an interruption.

`sb -sessions` lists recorded sessions. `-resume` and `-continue` reopen one
after a script exits or a process crashes.

Repository instructions in `AGENTS.md` or `CLAUDE.md` enter the system prompt.
`/init` creates an instruction file when a repository has none. Custom command
files are covered in [Native extension compatibility](extensions.md).
