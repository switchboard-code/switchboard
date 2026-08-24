# Switchboard

Terminal coding agent whose model is a configurable slot rather than a fixed
property of the tool. The design of record is the maintainers' design
document, kept outside the public tree; the § references in code comments
point into it, and this file restates the constraints that bind the code.

## Where things are

    cmd/sb/              CLI entry point, the phase-0 REPL, and the phase-3 TUI
    internal/provider/   canonical message types, Provider interface, adapters
    internal/session/    append-only event log, replay, resume
    internal/router/     deterministic per-turn selection, hard feasibility,
                         sticky mid-turn evidence and transactional moves
    internal/execution/  process runner and sandbox capability reporting
    internal/permission/ modes and rules
    internal/tools/      the core tool suite
    internal/agent/      the loop
    internal/advisor/    §9.2 run continuously: a second model that watches
                         the loop's observer stream and injects advice at
                         round boundaries; advice, never edits
    internal/mcp/        MCP client over stdio and Streamable HTTP, and the
                         bridge that puts each discovered tool in the registry
                         as mcp__server__tool
    internal/mcpnative/  bounded Codex and Claude MCP-config discovery;
                         sensitive values stay sealed until activation, trust,
                         policy, and runtime-feature gates all pass
    internal/extensions/ bounded Codex and Claude plugin discovery, offline
                         local installation, and Switchboard's independent
                         enablement/executable-trust ledger
    internal/hooks/      user commands at the seams of a tool call; a pre_tool
                         hook blocks on non-zero exit and on timeout
    internal/delegate/   the delegate tool: one level of subagent on a chosen
                         ladder rung, sharing the permission engine; named
                         agent definitions load from .switchboard/agents/
    internal/skills/     instruction packs the model pulls in by tool call:
                         descriptions ride the tool's schema, bodies stay on
                         disk until asked for; native .agents and .claude
                         invocation controls fail closed; legacy Claude
                         commands are explicit-only prompt entries; prompts
                         load without a trust grant because nothing executes
                         at read time
    internal/trust/      per-workspace grants that gate repository-declared
                         MCP servers (Switchboard or native), hooks, and the
                         language server
    internal/watch/      the /watch verifier: the user's own command, run at
                         a turn's seams when the checkpoint recorder says
                         files changed, reporting only the delta
    internal/lsp/        a deliberately bounded LSP client: initialize,
                         document sync, cancellation, document and workspace
                         symbols, definition, references, pushed diagnostics
    internal/checkpoint/ per-turn file snapshots behind /undo and the bounded,
                         read-only /review; restores never rewrite messages
    internal/blame/      line-level provenance behind /blame: replays the
                         write and edit calls the session logs carry and
                         aligns them against the file on disk; a line the
                         replay cannot explain is outside the record,
                         never guessed
    internal/bisect/     /bisect: binary search over the checkpoint
                         recorder's per-turn states for the turn that
                         turned the declared verifier red; mutates the
                         tree in place and restores it on every exit
                         path, cancellation included
    internal/schedule/   the per-workspace ledger behind /every, /at, and
                         /schedule: prompts that fire as ordinary user turns
                         while sb runs; no daemon, and an overdue entry
                         fires once rather than catching up missed ticks
    internal/config/     the ladder and settings; the TUI owns the file and
                         Save regenerates it, so nothing may depend on
                         comments in config.toml surviving

## Constraints that are not negotiable

**The core knows nothing about terminals.** Nothing under `internal/` may import
a TUI library, write to stdout, or assume a tty. The TUI and the REPL in `cmd/sb`
are consumers of the library: the TUI talks to the loop through `agent.Observer`
and `permission.Asker`, and every turn event crosses as a Bubble Tea message, so
the loop's goroutine never touches UI state directly. Retrofitting this is
expensive, which is why it holds from the first commit (design principle 1).

**Adapters never silently drop requested semantics.** When a provider cannot do
what the request asked for, the adapter returns a typed error. Emulating the
missing capability is a decision for a visible policy layer, not something an
adapter does quietly (§5.2).

**An adapter that emits tool-use blocks must report `StopToolUse`.** The loop
executes tool calls only on that stop reason, so an adapter reporting
`end_turn` or `max_tokens` alongside tool-use blocks leaves those calls in the
session with no results, and every later request built from it is malformed.
The Ollama adapter derives the stop reason from whether calls were emitted
rather than trusting the server's `done_reason`, precisely because the server
reports `"stop"` on a turn that ended in a call. The OpenAI-compatible adapter
does the same, for the same reason. Check this first when adding a provider.

**A serving surface is part of target identity, not a label on it.** The same
model reached through a different endpoint is a different target: different
adapter, different capability evidence, different catalog entry, different
price. `openaicompat/ollama/qwen3.5:9b-mlx` and `ollama/local/qwen3.5:9b-mlx`
are the same weights and are not interchangeable to the router.

For the OpenAI-compatible adapter the profile name *is* the surface, because a
profile is a claim about one server's behavior. There is no default: `New` on
an unknown profile is an error rather than a fall back to the generic floor,
since a typo would otherwise quietly disable the capabilities the user asked
for. A profile nobody has run against a real server does not belong in the map.

**The router is rules until evidence earns a learned one.** §8.2 defines every
classifier dimension by a measurement procedure against the §8.6 eval corpus,
and §19.2 gates a learned router on beating the heuristic after runtime and
distribution costs. The current historical journal fails the strengthened
matrix-integrity gate, and its diagnostic projection has only one rung on the
capability front. There is no routing decision to learn from that evidence.
Do not fit a model, call a dirty journal training data, or ship weights until a
clean harder corpus produces at least two useful rungs and the candidate passes
the gate. Running the deterministic policy is what produces the evidence to
settle this.

**Opening routing is per user turn, over the prospective request.** An
interactive session bootstraps on the bottom rung only because no user request
exists yet. Immediately before each user turn, `cmd/sb/turn_route.go` measures
the request that would actually be sent: frozen system and tool zones, replayed
messages plus the opening message, attachments and vision need, live capability
evidence, context fit, cache state, and the remaining hard budget including
retry reserve. Do not route on the prompt string alone, and do not record the
empty startup bootstrap as a decision. A user pin still passes every hard
feasibility check; `/tier auto` removes the pin and resumes this per-turn path.
`/routing off` (`RouteAuto`) holds the current rung through the same pin path —
the opening route hard-checks it and chooses nothing, the watcher stays
paused, and relief is refused — so the rung changes only when the user changes
it.

**A route move is a prepared transaction at a completed model-round seam.**
Planning a user turn and assessing a sticky escalation are pure proposals.
Probe the destination and recheck capability, context, and budget against the
request it would receive before applying the prepared provider binding and
sticky rank together. A failed probe, stale proposal, or refused hard check
must leave both unchanged. Mid-turn signals can arrive from tool calls, but a
move is assessed only after the model round and its tool work complete; never
swap the provider under an in-flight round.

**A trigger that needs state the loop does not keep is absent, not guessed.**
`internal/router` detects repeated tool calls, tool error spikes, new test
failure signatures, and hedging, because the observer already carries what each
needs. §8.3 also lists an edit reverted twice and a diff crossing a threshold;
neither is emitted, because the loop keeps no edit history or running diff and
approximating them would escalate on evidence that does not exist.

A failure signature is the first line that looks like a failure, with digits
stripped. Comparing whole outputs would make every retry look new, because
timings and counts differ between two runs of the same broken thing.

**Provenance is replay of the record, never inference about it.** `/blame`
(`internal/blame`) attributes a line only when replaying the recorded write
and edit calls reproduces that line: the write's bytes and the edit's
replacement come from the session log, an edit re-applies under the same
exactly-once-or-replace_all rule the tool enforced, and one that no longer
applies against the reconstruction is counted as unplaced and said, not
forced into place. Everything else — hands, shell commands, formatters, the
file before the log began — reads as outside the record. Attribution is
evidence about which rungs earn their keep, and one guessed line poisons
every claim built on it, so do not widen the replay with fuzzy matching or
nearest-edit heuristics.

**A recap claims only what a ladder-less reader can carry.** /recap
(`cmd/sb/recap.go`) tells one recorded session's story — opening, route,
files, races, bill — and its route line is deliberately built from
opening tiers and move counts alone: a moved turn's destination is a
target id in the record, and naming it as a rung would need a ladder
this reader does not take. Bare /recap from inside a session skips the
log it is typed into, because "where you left off" is the previous
session, not the running one. The recorder's boundary is stated in the
output, and an unbilled session says "nothing billed", never $0.00.

**A recurrence is two sessions, not two lines of output.** /mistakes
(`cmd/sb/mistakes.go`) sums failure signatures across the workspace's
recorded sessions, and every choice defers to machinery that already
exists. What counts as a failure is the escalation detector's own gate,
exported for the purpose (`router.LooksLikeTests`,
`router.ExtractFailures`): a failing run of a test-shaped command, the
first failing line's signature, one run one observation — a ledger that
decided "test run" differently would disagree with the routing record
about what a failure was. A fork's copied record deduplicates on the
record's own timestamp, the Usage.At mechanism, and gathering runs oldest
log first so a copy can never claim the meeting its source made. The bar
is a second session, because recurrence across contexts is the claim: the
same afternoon failing five times is debugging, not a standing problem.
The scope is stated in the output — a failure the recorder cannot see is
absent, not guessed — and the closer names /learn, the way a red watch
names /bisect.

**A bisect leaves the tree the way it found it, or that is the error.**
`internal/bisect` probes past states in place, and its contract has three
legs that must survive any change. The restore runs on every exit path —
verdict, verifier error, unwritable file, cancellation — and a restore
failure outranks the answer, because a bisect that leaves the workspace in
the past has done damage no verdict repays. The verifier is declared, the
/watch posture: the armed command or one typed into /bisect, never
inferred from the workspace. And a turn whose capture passed the snapshot
cap refuses the whole bisect, because reconstructing over it would restore
half a turn — the same refusal /undo makes, applied before any probe runs.
While one runs the session is busy the way a turn is; do not add a path
that lets a turn start against a reconstructed tree.

**Feasibility is not economics.** A target that cannot hold the context, lacks a
capability, or is not an approved destination is infeasible, not expensive. The
filter checks those before budget so that a target excluded by policy is never
reported as one that was out-priced, and a ceiling is checked against the upper
bound rather than the expectation: a turn affordable on average is not a turn
under a ceiling.

The /budget ceiling is enforced in three places and all three price the same
way, through the §6.4 estimator's upper bound (`cmd/sb/budget.go`): the router
excludes rungs it does not fit, `moveTo` refuses an escalation onto one, and
`Loop.Budget` gates each call before it goes out (§15). The loop's hook takes
a token count and returns an error; the ceiling itself lives with the surface,
because the surface knows what the session has spent and what a dollar is. A
ceiling governs dollars only — a local or plan rung passes the gate, because
the three meterings are never collapsed — and an unpriced target passes too,
with /budget saying so, since a ceiling cannot govern what has no price.

**An output cap belongs to the rung, including its outage path.** A positive
`[tiers.<id>] max_output` is copied into the concrete identity of the primary
and every ordered fallback, sent on each successful provider request, and used
unchanged by routing, context, and hard-budget admission. Save refuses an
in-memory fallback whose cap differs from its primary rather than silently
rewriting either target. An omitted allowance with no adapter or catalog bound
stays unknown; a finite context window therefore refuses it instead of guessing
a server default. Adapter-required defaults may be derived only while the cap
is omitted. In particular, Anthropic's token-budget dialect may raise its own
default above `budget_tokens`, but an explicit cap that does not exceed the
reasoning budget is a typed capability conflict and is never raised behind the
user's back.

**A profile is a ladder, not a mode.** `[profiles.<name>]`
(`internal/config`) holds tiers and nothing else — slots, auth, and
settings stay global — and the undecoded-keys check refuses anything
more, which is the honest answer until a workload proves it needs more
than a different ladder. The swap is launch time only (`-profile`,
`Config.ApplyProfile`): the ladder feeds session assembly and the frozen
zone, and a mid-session swap would repoint records that name tiers by
id. While a profile is active, `Tiers` holds its ladder and `mainTiers`
keeps the main one for the file; the save contract has two legs that
must both survive any change to the writer — a save under a profile
writes the main ladder into `[tiers.*]` untouched, and a rung bound with
/models under a profile lands in the profile it was bound in.
`TestSaveUnderAProfileKeepsTheMainLadder` is that contract run, not
described. /tiers says when a profile is active, because "which ladder
am I on" is the first question a surprising route decision raises.

**Fallback is availability, never routing.** A tier's `fallback` list
(§5.4, `probeTierFallback`) is consulted only when the primary cannot be
probed, the rung's identity does not change, and each candidate passes the
same probe a primary does — a fallback that cannot call tools is refused
the same way. Every entry was written into the user's own config, which is
what makes it an approved destination; what the design still demands is
that the substitution renders before content is sent and is recorded on
the session, so every call site of `probeTierFallback` must surface the
note it returns rather than dropping it. Entries resolve the provider's
default serving surface; a non-default-surface fallback is not expressible
and that is a stated limit, not an oversight. `max_output` is the exception to
otherwise target-specific fallback parameters because it is rung policy: the
same value constrains the primary and every fallback.

**An outcome is worth less as evidence than it looks.** §8.4's labelling rules
are in `internal/router` because each prevents a specific failure. A clean
completion is weak evidence of sufficiency and none of necessity, which is the
main way a naive router learns to over-provision. An escalation is not a
negative label: provider failure, a phase change, and a bad rule produce the
same event. Abandonment is censored rather than negative.

**One component owns cache-marker placement.** `internal/breakpoint` decides
where markers go and whether to place any at all, because the four reachable
surfaces want four different things: explicit markers with a limit and a
minimum, a routing key, nothing at all, and no cache whatsoever. Spread that
across call sites and each one grows its own wrong assumption.

A declined marker is recorded rather than dropped. A marker below a target's
minimum is accepted by the server and silently does nothing, with no error
either way, so a logged reason is the only thing separating an expected miss
from a bug.

**Cache state comes from what the provider reported, never from what was
sent.** `internal/cachestate` records observations and nothing else. Sending a
marker is not evidence anything was cached: a marker below the minimum is
accepted and does nothing, an entry can be evicted early, and a target may
report nothing at all. All three look identical from the request side.

A write observation and a read observation are different facts, and retention
is modelled rather than known: providers describe a TTL as a floor, so the
tracker reports a probability that decays instead of asserting an expiry it
cannot see. A target with no cache accounting stays Unknown forever, because
silence is not evidence of a miss and recording one would leave the alarm on
permanently.

/cache is that tracker's surface and adds no claim of its own: the modeled
probability says modeled, an unsent prefix's correct expectation is a miss,
and a no-accounting surface stays unknown. The command is deliberately not
busy-safe, because the expectation reads the hash of the last planned
request and that field is the loop goroutine's to write during a turn.

**A capability claim gets tested against the target, not against its docs.**
What the Anthropic adapter asserts was confirmed with a live request first:
that `claude-haiku-4-5` rejects `adaptive` thinking and takes a token budget,
that the one-hour cache TTL needs no beta header and bills to its own bucket,
that replaying a thinking block without its signature is refused while dropping
the block is accepted, and that a tool result is a user message because there is
no tool role. Each of those contradicted a reasonable guess.

**A claim earned against one model is a claim about that model.** The sentence
above used to say "this model" and the adapter applied it to every Anthropic
target, which is how one live result became a rule for models that invert it:
the current Opus and Sonnet models refuse `budget_tokens` with a 400 and take
the effort word on `output_config` instead. The catalog had said so all along —
its entries name the dialect and offer `xhigh`, an effort the budget shape has
no number for — so the two disagreed and the adapter won, silently, on the
targets nobody had run. `adaptiveThinking` in `internal/provider/anthropic` is
now the list, a model absent from it keeps the budget shape because that is the
survivable direction for a wrong guess, and the two dialects are pinned by
offline tests over the bytes the adapter builds. Adding a model to that map is
adding a capability claim: it needs the live case beside it, not a docs page.

**Wire formats get captured before they get mapped.** Both adapters were
written against a recorded response from a running server, checked into
`testdata/`. Both captures contradicted the documentation: Ollama reports
`done_reason: "stop"` on tool-call turns, and the compatibility endpoint sends
its usage chunk *after* `finish_reason`, so a terminal event emitted at
`finish_reason` reports zero tokens for the turn.

**A credential has no rendering that shows it.** `credential.Secret` keeps its
value unexported and redacts in `String`, `GoString`, `MarshalJSON`, and
`MarshalText`, so a secret that reaches a log line, a formatted error, or the
JSON session record prints as a placeholder. Reading it takes `Expose()`, which
is deliberately easy to grep for. Do not add a field, an accessor, or a struct
tag that would let one out by accident.

Secrets go to the platform store over a pipe, never in argv, because every
process's command line is readable by every user on the machine. Both backends
have a test that fails if the value appears in a recorded argv; that test is the
guarantee, not the comment above the code.

The posture faces outward too. `credential.ScanPrompt` (scan.go) checks
outbound prompts for key-shaped strings: known issuer prefixes with length
floors only, precision over recall, because a gate that cries wolf trains
the user to wave everything through. A `Leak` keeps its match unexported
with masked `String`/`GoString` renderings and no accessor — redaction
happens inside the package — so a finding cannot commit the leak it
reports; the test that renders one every way and greps for the token is
the guarantee. The private-key pattern matches the whole block, END line
included and to end-of-text when the END was lost in the paste, because a
redaction that replaced only the header would send the body it was asked
to hold back. The TUI holds the send behind redact/send/drop through one
chokepoint (`tui_secretgate.go`) that covers plain turns, /tN overrides,
steers, and races, with esc meaning drop; the storage form
(`openSecretGateForStorage`) guards what will sit at rest — a scheduled
prompt — and offers only redact or drop, because every durable artifact this
program writes redacts unconditionally; the interactive REPL asks the same
three answers in line; headless refuses outright and names
`-allow-secrets` as the stated widening, because a surface with no one to
ask fails closed. A race's verdict record redacts its stored prompt
unconditionally — the record is a summary, not the transcript, and must
not carry what the gate scrubbed from the sends. Do not add a pattern
without a length floor, and do not give a finding a rendering that shows
the match.

**The watch verifier is declared, never inferred, and speaks only in deltas.**
/watch (`internal/watch`, `cmd/sb/tui_watch.go`) is §8.4's task-specific
verifier given a declaration point: the user types the command into their own
session, which is the hook posture — the user's standing policy, run
unconfined and unprompted — and there is deliberately no repository-declared
form, because a checkout must not get a command executed by the act of being
opened. What decides a run is due is the checkpoint recorder's capture count,
the same evidence /undo restores from; mutations the recorder cannot see (a
shell command's side effects) do not trip it, which is the absent-not-guessed
trigger rule applied here. Reports reach the model through the loop's
round-boundary injection seam and the turn-end fold, never by rewriting
history, and only a change travels: new failure signatures, or red turning
green. A failing run feeds the escalation detector through
`Detector.VerifierFailures` at the weight of one signal per run — the
declaration removes the test-command regex gate, not the parity with a test
run the model made itself — and skips the error-spike count, because the
verifier failing means the task is broken, not that a tool call went wrong.
The injected text passes `credential.ScanPrompt` and redacts unconditionally,
the race record's posture, because a round boundary has no one to ask. A
turn-end run feeds no escalation — the detector and the sticky policy reset
per turn, and the turn it would have moved is over — its verdict goes to the
user and folds behind the next typed prompt. The declaration lives on the
app, not the session, so it survives /clear, /fork, and /resume until
/watch off; injected messages are marked `Injected` in the log so a reader
(/retry's `lastTurnOpening` above all) can tell an opening from what rode
in mid-turn.

**A permission prompt is not a sandbox.** Where OS isolation is unavailable or
unverified, automatic execution is disabled rather than approximated by
prompting (design principle 4, §11).

Permission `auto` may delegate an execute decision to the approver only while
the command will run inside active verified confinement. Host-direct execution
asks the human because a workspace build can run arbitrary code. A confined
explicit `NetworkFull` request can remain reviewable; shared host loopback,
opaque shell or interpreter code, sensitive commands, and external effects
stay human-gated. `yolo` remains the explicit unconfined grant.

Agent exec, LSP and provider probes, and editor launches use the central
scrubbed child environment. Explicit user `!` shell and custom-command launches
retain the ambient environment on purpose. Do not broaden the first claim to
every child process or silently narrow the second; those are different trust
postures.

The same rule prices a first-party subprocess tool. `astgrep` wraps the
user's own ast-grep binary and is always `EffectExecute`, including inside
verified confinement: a PATH-resolved binary can write within the sandbox.
Confinement limits the process when configured, but never reclassifies it as a
read effect; the active permission mode still decides whether it may run.
The binary is looked up once at session assembly (`cmd/sb/astgrep.go`), so
the frozen zone never changes shape mid-session, and the tool is absent
rather than broken on a machine without it. `CoreNames()` deliberately
excludes it: a named agent's tool grant must not depend on another
machine's binaries, so a restricted agent loses astgrep with everything
else it did not name, while unrestricted subagents get it. The JSON it
parses was captured against ast-grep 0.45.1, exit 1 is the no-match
convention rather than a failure, and the runner's combined output means
the binary's warnings ride beside the JSON line — the parser separates
them and hands the warnings to the model on a miss, because "your pattern
did not parse cleanly" is the difference between tightening a pattern and
abandoning the tool.

**A question is interaction, not an effect.** The `ask` tool
(`internal/tools/ask.go`) lets the model put one question to the user:
two to eight options, one pick or several, or an answer in the user's own
words, or a decline. It carries the read effect for the reason todo does —
the answer channel is the user, who can refuse in person — so it is
allowed in every mode, plan included, because planning is exactly when a
question earns its place. What keeps an unattended surface from asking is
the absent `Questioner`, never the permission engine: headless runs,
delegate subagents, and race branches leave it unset, and the tool
refuses with an instruction to decide and state the assumption, because a
question with no one listening fails closed rather than hanging or
inventing an answer. A race arm keeps the schema (frozen zone) and
refuses at Plan through the branch's refuse map; `Branch` never copies
the questioner, which is the backstop if a call site forgets. A decline
is an answer, not an error — the model must hear it and work around it,
so it travels as a result. A typed answer redacts unconditionally through
`credential.ScanPrompt` — the injected-report posture, because the
question dialog is not the secret gate and must not grow into one — and
the test that plants a key and greps the result is the guarantee. Picks
return in offered order, the shape the question was asked in.

**Egress from this process is external too.** `websearch` and `webfetch`
(`internal/tools/web.go`) carry `permission.EffectExternal` even though no
other process is involved, because a fetch is the classic exfiltration
channel: a URL the model composed can carry the workspace anywhere. Their
requests put the host in `Path`, so the remembered answer covers a host
for the session rather than one byte-exact URL, and the outbound URL and
query pass `credential.ScanPrompt` before anything leaves — the test that
greps the refusal for the token is the guarantee, the same pattern as
every other rendering of a secret. A redirect is held to the approval's
own grain: one that stays on the hostname is the server's routing and
follows, one that leaves it is refused before anything is dialed, because
a grant naming host X must not read from host Y — an internal service
included — on X's say-so; the refusal names the destination so a fetch of
it goes through its own approval. The search backend's HTML is parsed
against the captured response in `internal/tools/testdata/ddg.html` (wire
formats get captured before they get mapped); result links arrive as
redirect URLs and the parser unwraps `uddg`, taking a direct href as it
stands so a format change degrades to worse links rather than no results.
Fetches cap what they read and what they hand the context, because a page
has no contract about its size and the context is the scarce thing.

**An external tool is never inside the sandbox.** An MCP server is a process
this program started un-confined, acting wherever it acts, so a bridged call
carries `permission.EffectExternal`: no bounded mode auto-allows it, bypass
included, because bypass suppresses prompts inside a granted sandbox and an
external tool was never inside one. Yolo alone covers it — the
everything-grant that exempted the riskiest effect would not be what it says.
Short of yolo, only an explicit rule (the server's `allow` list)
or a remembered answer lets one run without asking, and the remembered answer
covers the tool, not one byte-exact invocation — that is what the display-only
`Request.Detail` field exists for. A legacy Switchboard stdio declaration
inherits the ordinary environment only after SSH-agent sockets and secret-,
token-, key-, password-, credential-, auth-, session-, cookie-, database-URL-,
and DSN-like names are removed case-insensitively. A restricted/native
declaration starts from the process baseline, admits only named `inherit_env`
values, then applies its explicit `env`; the tests that fail on ambient or
explicit-value leakage are in `internal/mcp/stdio_test.go`.

**Protocol downgrade requires protocol evidence.** The client probes stateless
MCP 2026-07-28, and can negotiate the initialization-based 2025-06-18 and
2025-03-26 revisions. An explicit supported-version answer, the bounded stdio
probe behavior, or an unrecognized HTTP 400 can establish an older server.
Authentication failures, rate limits, 5xx responses, owner cancellation, and
recognized modern protocol errors cannot. Do not turn an operational failure
into a legacy retry. Modern request metadata, HTTP headers, result types,
cancellation, pagination, and secret redaction are protocol semantics, not
cosmetic compatibility.

**A repository's configuration may speak; only a trusted checkout executes.**
`.switchboard/mcp.toml` and `.switchboard/hooks.toml` in a repository are read
only after the user grants trust to that resolved path (`/trust grant`,
`internal/trust`). The same files under ~/.switchboard always run, because
that is the user speaking. Do not add a repository-provided input that starts
a process without routing it through this gate.

The language server sits behind the same gate even though the binary is
the user's own, because the code it chews is the repository's: building
the module graph runs what the workspace directs (toolchain directives,
plugins), unconfined — confinement would deny the caches and network a
server needs. Opening a repository is not permission to run what its
module implies. The client (`internal/lsp`) is deliberately bounded:
initialize, open/change/save/close document synchronization, cancellation,
document and workspace symbols, definition, references, and pushed
diagnostics. It
answers configuration with per-item null defaults, reports the single
workspace folder, acknowledges work-progress creation, and rejects unsupported
server requests. The candidate table in `cmd/sb/lsp.go` holds only servers
verified live on a real workspace
(gopls, TypeScript 7's native `tsc --lsp`, pyright, and clangd when a
compile_commands.json supplies flags), which is the §5.2 profile rule applied
to language servers. The definition and reference tools take
{path, line, symbol} because models copy file:line reliably and invent column
numbers freely; outline, workspace-symbol, and Problems surfaces use the same
client without pretending pushed diagnostics cover the whole workspace.
Server start is lazy for outline, symbol, definition, and reference queries;
status and Problems do not start it. Tool presence is decided at assembly,
which is what the frozen zone requires.

**MCP discovery is once, at session assembly.** Tool definitions sit in the
frozen zone (§6.1), so a server that changes its tool list mid-session is
noted and deliberately not followed; the next Switchboard run lists again. Bridged
names are sorted before registration so the frozen-zone ordering never
depends on which server answered first. Keep each raw server/tool identity
beside its sanitized bridge through collision resolution: an `allow` entry
becomes a permission rule only after that exact bridge registered, never for a
different raw name that sanitized the same way.

`required` is an assembly invariant, not display metadata. A required
declaration shadowed by an earlier equal server name is an error, and a required
connection failure closes every peer that did connect and aborts assembly.
Optional failures remain diagnostics. Keep this check before tool registration
so a failed required set cannot leave a partial frozen schema behind.

**Native MCP execution starts at one fail-closed materialization seam.**
`internal/mcpnative` treats partial Codex user/project TOML as inventory only.
Executable Codex definitions must come from the installed app-server's bounded
`config/read(includeLayers=true)` result for the same canonical cwd, which
contains the effective package, system, managed, cloud, user/profile, project,
and session stack. The app-server is launched only for an existing activation,
a trusted Codex plugin that needs policy, or an explicit `sb mcp` command; a
workspace-local `codex` binary is rejected. Claude user, local,
workspace-to-cwd project, and optional exclusive managed configuration remains
an in-process bounded read. The normalizer seals sensitive values and preserves
every non-baseline semantic as a typed feature requirement. `cmd/sb` may turn
one winner into an `mcp.Spec` only after exact keyed activation, authoritative
managed policy, workspace trust where required, and an explicit runtime-feature
claim all pass.

The current adapter claims stdio and HTTP, working directories, restricted
forwarded environment, static/environment-backed/bearer headers, startup and
tool timeout forms, `required`, tool filters, controlled Claude environment
expansion, and eager `alwaysLoad` assembly. Its feature list and mapper tests
are the compatibility contract. OAuth and ChatGPT auth, native approval modes,
SSE, WebSocket, remote execution, header helpers, tool-exposure behavior, and
parallel-tool declarations must fail closed rather than be discarded or
relabeled as baseline HTTP/stdio. `configRequirements/read` is also
authoritative: null means no managed requirements; a non-null bundle remains
quarantined until its MCP projection is implemented exactly. Never treat raw
Codex files, a missing auth file, or a manifest namespace as proof that the
effective stack or plugin policy is unrestricted. `docs/extensions.md` is the
public matrix.

**Project instructions compose, under one budget, general to specific.**
`internal/agent/instructions.go` reads the user's own file from three roots,
then every directory from the repository root down to the workspace, one file
per directory with an override sibling after it. The old reader took the
workspace root's first hit and sliced it at a byte, which could hand the model
half a character; truncation now cuts on a line and the budget is spent
specific-first, because dropping a package's own rules to keep a user's
defaults is exactly backwards. Whatever did not fit is named.

A whole-line `@path` imports, bounded to two hops with cycles named. A mention
inside a sentence is prose — a reader that spliced on every `@` would import an
email address. A repository's import may not resolve outside the workspace, and
there is no command substitution, which is the /watch refusal again: opening a
checkout must not execute anything.

`.switchboard/rules/*.md` (`cmd/sb/rules.go`) is the conditional half. A rule
names paths and is injected at a round boundary the first time the session
touches one, once each and capped per session. What counts as touching is the
registry's read set and the recorder's captured mutations, which is the
absent-not-guessed rule applied here rather than an inference about what the
turn is for. They are messages and never system blocks, so the frozen zone
stays byte-identical. The limit is stated rather than glossed: a rule fires
after the read or write, so it cannot prevent one — that is what hooks and the
permission engine are for.

**A picture from an external tool rides a message, never a tool result.**
`internal/tools/toolimages.go` queues what the MCP bridge decoded and
`cmd/sb/tui_toolimages.go` delivers it at the round boundary as an injected
user-role message. The delivery shape is the constraint, not a convenience:
every adapter already maps `provider.Image` inside a message and none has a
captured mapping for an image inside a `tool_result`, so carrying it in the
result would mean mapping a wire format nobody has run. This way adds no
adapter code and no capture.

The gate reads the live binding rather than a launch-time value, because an
escalation or a relief substitution changes which rung is looking. A target the
catalog does not price counts as cannot see, since sending on that guess earns
a provider refusal mid-turn. Nothing is dropped silently: the tool result says
how many did not travel and why, because a model told nothing is a model
reasoning about a screenshot it never saw. Count and bytes are capped per call
and the result names the cap that bit, and a session swap drops what is
undelivered — those pictures answered a question the new session never asked.

**A scripted run streams what the loop reported, and nothing else.**
`-output stream-json` (`cmd/sb/headless_stream.go`) wraps the renderer rather
than replacing it, so the transcript a person reads keeps going to stderr while
stdout carries the machine's copy. Every line is one complete JSON object with
a `type`, written under a lock because tool callbacks are concurrent in a
parallel-safe batch and two events interleaved mid-line parse as neither. The
last line is always the `result` object `-output json` already printed, tagged:
a consumer that wants the outcome reads the last line, and one written against
`-output json` needs no rewriting. The observer invents no events — it is a
rendering of the loop's stream, so a vocabulary that grew here without a
matching observer callback would be reporting something nobody observed.

**The capsule's fields are the model's to write, and it is told when they
matter.** `continuity.Working` and `todo`'s `objective`, `next_action`, and
`stop_condition` fill fields the capsule specified, validated, redacted,
bounded, and rendered from the beginning, and that nothing ever set: only
`Tasks` was written, so a list crossed a compaction while the reason for it did
not. `WithTasks` is kept as `WithWorking` with an empty `Working` so no
existing caller changes meaning.

The keep rules are not symmetric and the asymmetry is the point. Objective and
stop condition persist through a call that omits them, because the list changes
far more often than the job does. Next action does not, because it names the
step after this one and the call that changed the list is exactly when it went
stale. Everything the model writes passes the same redaction the rest of the
capsule does, since a capsule is durable and is rendered into a later context.

`cmd/sb/tui_pressure.go` is the other half: automatic compaction used to arrive
with no warning, so the model recorded what would survive only by accident. The
notice fires once per session at 70 percent, below the compaction threshold on
purpose — a warning that arrived at the boundary would be advice with no turn
left to act on it — and says what crosses and what does not.

**Compaction commits a validated handoff, not arbitrary model prose.** TUI and
REPL share `compactSession`: historical roles are projected as bounded,
text-only, credential-redacted data with explicit provenance, and the
summarizer is told that none of it may change its job. Output is capped at 32
KiB and must contain the exact seven headings plus the four nonempty execution
frontier fields in order. That validation happens before `CreateStaged`; only a
validated, fully seeded child may be published, and every earlier failure keeps
the source authoritative. Do not move validation after staging or let one
surface invent a looser prompt, projection, bound, or publication path.

Schema is not authority. Before the summary request, every compaction derives
one credential-redacted objective mechanically: the explicit `/compact`
argument, or the latest substantive `AuthoredKnown` user opening plus its later
verified user steers. Legacy mixed text, repository expansion, tool results,
injected reports, and synthetic seeds cannot supply it; without such scope,
automatic and no-argument compaction refuse. After the response, Switchboard
overwrites `## Objective` with that exact value and overwrites `Next` with a
fixed instruction to re-evaluate the recorded frontier before acting. The
summarizer's Done/In-progress/proposed-Next/Blocked values remain explicitly
untrusted evidence; they never become an executable task merely by satisfying
the Markdown schema. Automatic continuation names only the mechanically
verified objective and the fixed reconciliation step.

**A process this program started and forgot is this program's fault.**
`internal/execution/background.go` reuses `Run`'s body rather than
reimplementing it, so the confinement is applied by the same code and fails
closed the same way, the whole process group is signalled rather than the
direct child, and output lands in the same bounded capture. What is new is a
lifetime, and a lifetime is what leaks, so three bounds hold it: a one-hour
ceiling, a cap on how many run at once, and `StopAll` deferred at session
assembly, which is the last moment the program can still be sure the group is
its own to signal.

`proc` is a separate tool because "what has it printed, is it still running,
stop it" are questions about a process rather than requests to run one. It
carries `EffectExecute` although it starts nothing: a stop signals a group and
a read returns what a running process wrote, and pricing that as a read would
have the engine describing a posture the user does not have. A registry holding
`exec` without `proc` refuses `background` outright, because starting what you
cannot stop is a leak with extra steps, and `Branch` shares the set rather than
copying it — a process started under a branch is still this program's to stop.

**A refusal the ladder can answer is offered to the surface, once, between
rounds.** `Loop.Relief` (`internal/agent/agent.go`, `cmd/sb/tui_relief.go`) is
consulted for exactly two errors: a `ContextWindowError`, which is a fact about
the request that a roomier rung would not state, and an `AvailabilityError`,
which is retryable-to-the-last-attempt and therefore a fact about one target at
one moment. Everything else is the request being wrong, which no rung fixes,
and is not offered.

The hook is the surface's because the checks that make a destination legitimate
live where the ladder does; the loop only knows a round refused. It applies the
binding it is handed rather than returning it, so no window exists in which the
loop and the surface disagree about what is bound, and it is capped at
`maxReliefsPerTurn` because a ladder that cannot answer would otherwise be
walked at a probe and a budget check per rung.

Three constraints are load-bearing. A round that emitted content is never
relieved: half a streamed message finished by a second target is a turn nobody
can attribute. A pin — or routing off — refuses relief, because the user has
said which rung and answering a refusal by leaving it would overrule that
quietly. And the
two reasons keep different records — an overflow rebind is a move and is
written as one, an availability substitution is a runtime binding only, since a
route record would tell every per-rung aggregate the router made a decision it
never made.

**Read drift is reported, and the refusal it anticipates still stands.**
`internal/tools/drift.go` sweeps the hashes the registry already keeps for
write and edit's stale check and says, at the round boundary /watch and the
advisor use, which read files moved without a write or edit call of the
model's own. The evidence is entirely what was already there; what is new is
saying it a round before the refusal does.

Three things keep it honest. It never mutates `seen`: that map is what the
model was shown and is the stale check's evidence, so the reporter keeps its
own `reported` map and a refreshed one would disarm the guarantee at the point
it matters most. It is stat-first and twice bounded, because it runs on the
loop's goroutine between rounds — a file whose size and mtime match is never
opened, one over the hash cap is reported as touched rather than as differing,
and the sweep is capped and sorted so a capped run covers the same files each
time. And a change is reported once until it moves again, because the same
sentence every round is noise a model learns to skip.

Where it does not reach is decided rather than accidental. Only the TUI sets
`Loop.Inject` (`cmd/sb/tui.go`), so the REPL has no round-boundary injection at
all and the drift notice reaches it no more than the advisor or a /watch report
does. Race arms and delegate subagents branch a fresh registry, whose version
map starts empty, and neither is given an injection seam: an arm must stay
byte-identical upstream, and a notice injected into one would be the first
thing to break that. Both facts are load-bearing for the next person wiring an
inject.

**A standing rule is narrower than a mode, and weaker than a seen answer.**
`[[permissions]]` (`internal/config/permissions.go`) fills the rule list the
engine has always taken and only MCP allow lists ever supplied. It is the
user's own file, so it carries the hooks posture: standing policy, granting
what a typed yes grants, unconfined where nothing is configured. Two shapes are
refused at load because they are a mode written as data — an allow naming
nothing, and an allow whose only constraint is `effect` for write, execute, or
external. Those exist as modes, which are typed deliberately and visible while
they hold; a line in a file is neither.

The engine's own order is the thing to keep straight, because it is not "first
match wins": a matching deny answers first and from anywhere, and only then
does the first matching non-deny rule answer. So position decides among allows
and asks and never lets a deny be shadowed. Assembly puts the config's rules
ahead of `mcpRules` on that strength — a rule the user wrote to tighten a
server's tool has to outrank the allow list that server declared for itself.

A rule-matched allow yields to a credential-bearing request (`Check`, the
`SensitiveRequest` branch) in every mode short of yolo. A rule matches calls
the user never saw, which is what separates it from a remembered answer to one
exact request, so no standing rule approves a sensitive command unseen. Yolo
lifts that gate too: the everything-grant exempts nothing, and a carve-out
under it would be the mode lying about what it is.

There is no repository-provided permissions file and no command that mints a
rule mid-session. The first is the /watch refusal applied here: opening a
checkout must not pre-approve a command. The second is a stated limit rather
than an oversight — a one-keystroke "always allow" widens by argv prefix, and
the widening has to be visible before it takes effect, which the file already
makes it.

**A destination policy is a requirement, not a preference.** `[routing]
destinations` and /destinations (`cmd/sb/tui_destinations.go`) fill
`Requirements.ApprovedProviders`, which the filter has always checked before
economics and which nothing outside the tests ever set. It has to be filled at
every seam or it is a rule with a hole: the opening route, a mid-turn move, a
/tN pin, a retry, and resume all pass the same list, which is why the three
`check*Feasible` functions take it explicitly rather than reading it from
somewhere convenient. The router filters candidates, and a candidate is what a
user turn is routed among; every other path to a provider resolves a rung
directly and gets the same check through `destinationAllowed` — the four slots,
both race arms, and the rung a delegate call names. That last is the one that
decides whether this is a policy at all, because the rung there is the model's
choice, and a rule enforced only on turns is a rule a tool call walks around.
A directly resolved rung outside the policy is refused and names itself rather
than being substituted, since each of those callers named a rung on purpose. A preference here would be a policy the escalation
detector could talk its way past on a bad turn.

The unit is a provider name. "Local only" is a conclusion about where a server
runs, and a target identity does not state that — an OpenAI-compatible endpoint
is a laptop or a data centre and the name says neither — so the honest form is
naming the providers. A policy that leaves no configured rung reachable is
refused when it is typed, because the router would correctly exclude every rung
on the next turn and the session would read as broken rather than as governed.

**An audit reads the record, never the code.** /audit (`cmd/sb/tui_audit.go`)
is the system prompt's oldest unenforced rule given a check: say what you did,
and do not describe a change you have not made. The closing message is the
claim; the recorded tool calls with their results and the checkpoint recorder's
captures are the evidence; a finding is where the two disagree. It does not
review the work, because a reviewer that also reviewed the code would bury the
one thing this is for, and it does not read the workspace as it stands, because
the workspace has moved on and the turn has not.

It takes no turn number, and that is the constraint the file exists to hold.
The message log and the recorder are two ledgers with two numberings that
/fork, /undo, and /clear move independently; the recorder's open scope and the
last message that opened a turn are the one pair certainly the same turn.
Lining up any other pair by index would be a guess.

The auditor is a slot, the summarizer's mechanism as a fourth named role,
because a model checking its own claims is the weakest reading of them. With
none bound the audit runs on the rung that made the claims and says so rather
than reporting agreement it has no standing to report. The record's edges ride
with the evidence in the same words the user gets: the recorder does not see
what a shell command wrote, captures over the memory bound are named rather
than half-covered, and each makes a claim unchecked rather than false. The
packet is machine-composed and leaves for a target the turn may never have
reached, so it redacts unconditionally. Nothing it produces is appended to the
session or injected: the finding is the user's, and the prefix stays
append-only.

**A question is a granted role, not an assumed one.** `elicitation/create`
(`internal/mcp/elicit.go`) is answered because a question is interaction rather
than an effect — `ask`'s own framing — and the answer channel is a person who
can refuse in person. Sampling and roots stay declined for the reason they
always were: each hands the server something nobody offered, and a sampling
request spends the user's model budget. What grants the role is the surface
supplying the questioner, and that same presence is what declares the
capability at initialize, because a capability is a promise to answer and an
unattended session cannot keep it. The relay in `cmd/sb/questionrelay.go`
exists only because the two moments differ: MCP declares during assembly and
the TUI's dialog needs a running program. A relay nobody filled refuses.

Two things cross from a server trusted with neither. The question is text on
the user's screen, so the dialog names the server and caps the prose; a
question that looked like Switchboard's own would be the whole attack. And the
answer travels to an unconfined process, so it passes `credential.ScanPrompt`
and redacts unconditionally — the same posture as an injected report, applied
where the consequence is larger. A schema this client will not answer is
invalid params, never method-not-found: the method is served and this request
is not, and a server told otherwise stops asking.

**A hook that hangs has answered.** A pre_tool hook blocks the call on
non-zero exit and on timeout both, because a gate that fails open the moment
it hangs is not a gate. Hooks run unconfined and unprompted — they are the
user's standing policy — which is exactly why the repository's hooks file
sits behind the trust grant.

**Delegate depth is one.** A subagent's registry has no delegate tool; an
agent that can recurse is an agent whose cost has no ceiling. Subagents share
the primary's permission engine and asker, their rails render through the raw
observer rather than the watcher so a subagent's stumbles never escalate the
primary, and their sessions live in their own store so /resume never offers a
context that was never the user's. §19.2 phase 6 expects delegation evaluated
against sticky single-primary baselines; that eval has not run, and the tool's
own description does not claim it has.

A named agent is a definition, not a capability. The files under
`.switchboard/agents/` (§13) load without a trust grant because nothing
executes at read time: a definition is a prompt, a default rung, and a tool
grant, and the grant can only narrow — `Restrict` errors on a name outside
the suite, and the sub-registry never held delegate or the bridged MCP tools
to begin with. Discovery is once, at session assembly, sorted by name,
because the definitions ride the delegate tool's schema into the frozen
zone. A session with no definitions renders the schema byte-identical to
what it was before the feature existed; the test that guards that is the
cache promise, not the comment. A definition naming a rung the ladder lacks
runs on the default rung with a note, rather than erroring on every call.

**A native plugin record is inventory, not authority.** `internal/extensions`
discovers exact Codex and Claude roots, and `internal/extensions/native` joins
them to bounded marketplace, installed-registry, and native settings records.
Native enablement is provenance. Only Switchboard's own per-user ledger may
enable an exact dialect, scope, physical root, and workspace identity. An
available catalog entry has no activation capability; it must first be copied
by the offline local installer and freshly rediscovered. Do not scan caches,
guess an installed root, prefer an ambiguous duplicate, or let a product's
enabled bit cross this boundary. An applicable managed denial remains a hard
upper bound: permission does not cross from a native client, but policy may
still forbid execution.

Plugin enablement and executable trust are distinct. Enablement may expose
prompt-only skills at the next frozen-zone assembly. MCP and hook declarations
make a plugin executable, and trust is bound to the digest of the bounded
plugin tree; changed bytes invalidate it. The current session adapter loads
enabled plugin skills and MCP declarations from one unambiguous enabled root.
MCP additionally requires the current digest's executable grant, exact cache
rediscovery before and after parsing, managed-policy approval, and the same
typed runtime-feature gate as direct native MCP. Plugin hooks, agents,
commands, apps, LSP, and other recognized components remain inventory-only;
merely detecting or trusting them does not wire them to an existing runtime
surface. The installer copies an already present exact local source into the
content-addressed Switchboard cache; it never fetches, resolves packages, or
runs lifecycle code.

Skills follow the named-agent posture exactly, because a skill is a prompt
the way a definition is (§13): `.switchboard/skills`, native `.agents/skills`
and `.claude/skills`, bounded recursive `.claude/commands`, the Unix Codex
managed root, and enabled plugin skill roots load without executable trust,
nothing executes at read time, and whatever a skill persuades the model to do
passes the permission engine on its own merits. Discovery is once, at session
assembly, sorted by canonical source-qualified selector, because the
descriptions ride the skill tool's schema into the frozen zone. If nothing is
model-visible the tool is not registered at all, and the schemas render
byte-identical to a build without the feature — that absence is the cache
promise, and
`TestNoSkillsLeavesTheSchemasByteIdentical` is what pins it. `/skills` still
shows the full inventory, including manual-only and blocked packs.

Legacy Claude command files are always manual-only skills. Discover
`.claude/commands/**/*.md` at each cwd-to-Git-root project layer and under the
user root, pin the resolved tree and file identities, and require `/skill` with
the exact canonical path selector. Their frontmatter description and argument
hint plus static Claude argument substitution are prompt metadata, never a
request to execute a command. Unsupported host controls, dynamic shell or
context expansion, and implicit attachments block invocation. A native Claude
skill wins a same-scope basename collision and the command is omitted with a
diagnostic. Retain cross-scope definitions under distinct selectors rather
than importing Claude's personal-over-project winner; invisible precedence is
the security bug exact selectors exist to avoid.

Equal display names never resolve through invisible ecosystem precedence.
Model tool calls and `/skill` use the exact canonical selector. Native
invocation metadata is a safety boundary: Codex implicit opt-out, Claude
model/user opt-outs, and Claude argument substitution are implemented;
unsupported tool grants, forced models or contexts, agents, hooks, shell or
dynamic-context expansion, Codex dependencies, and implicit Claude
attachments block the affected invocation rather than being ignored.
Malformed controls fail closed. A Claude `paths` filter blocks automatic model
exposure because the host lacks that activation context, but does not by itself
make explicit invocation unsafe. Codex `interface.default_prompt` is UI-only
metadata and is deliberately not substituted for the `SKILL.md` body.

The tool serves a skill's own directory and nothing else, on a pinned resolved
root so a symlink or replacement cannot carry the read outside it: the
workspace-rooted read tool cannot reach a user pack, and the skill named its
own references, not the filesystem. Do not broaden the YAML reader into
guessing behavior: portable unknown descriptive metadata may be ignored, but
any known behavior-bearing control must be honored or block invocation.

`/learn` writes `.agents/skills/<name>/SKILL.md` and inherits both postures at
once. The distillation is `/compact`'s mechanism reused whole — one request
outside the loop, the summarizer slot when bound, no tools, nothing appended to
the session — and
the pack it writes does not hot-register: discovery stays once-per-assembly
because the descriptions ride the frozen zone, so the command reports
"offered on the next Switchboard run" rather than pretending otherwise. The composed file
passes `credential.ScanPrompt` before anything reaches disk and redacts
unconditionally, the race record's posture, because a skill pack outlives
every chance to ask and may be committed; the test that greps the composed
content for the token is the guarantee. Do not add a path that writes the
distiller's output to disk without that scan, and do not register a freshly
written pack into a running session's registry.

Publication is an absent-target transaction, not `MkdirAll` followed by
`WriteFile`. The workspace and every skill-directory component are retained as
physical, current-user-owned directory capabilities; a symlink, FIFO,
replacement, parent retarget, or existing `SKILL.md` refuses the write. A
private per-workspace lock serializes recovery and publication across
processes, and the checkpoint cleanup ledger covers the atomic no-replace
commit. Recovery state stays in the machine-local session store rather than in
the repository. If the cleanup store and workspace cannot support the same
atomic transaction (notably different Windows volumes), `/learn` fails closed.

The pack ends with a provenance paragraph — the session it was distilled
from, the date, the writing model — because a rule whose rationale is
lost can never be safely deleted, and instruction files rot by exactly
that mechanism. It rides the body, not the frontmatter: the neighboring
tools' parsers ignore unknown frontmatter keys, and the line is written
for the reader deciding whether the pack still earns its place. The
provenance string sits inside the credential scan's reach like the rest
of the file.

There is deliberately no exported boolean for this. `execution.Capability`
carries a `*Confinement`, which is produced only by a self-test that passed on
this machine and is also the thing that wraps the command. Do not add a
`Verified bool` beside it and do not let a caller consult one without applying
the other: "we verified containment" and "we applied containment" have to be the
same fact, or the product reports a sandbox it is not using. `Run` fails closed
when a confinement is set and cannot be applied. See `docs/sandbox.md`.

**The prefix is append-only.** Context layout exists to keep provider caches
warm. Anything that rewrites history is a cache-invalidating event and is
scheduled deliberately (§6.1). This is why /undo restores files and never
messages: `internal/checkpoint` snapshots what write and edit are about to
change, per turn, and a restored file already forces a re-read through the
stale check, while the conversation that produced the change stays exactly
as sent. Do not add an undo path that mutates already-sent messages.

`/review [turn]` is presentation over that evidence, never another restore
path. Bare review binds only the open turn; a positive one-based number binds a
retained mutation turn. Selection and asynchronous display stay conditional on
the session, workspace generation, recorder revision, selected turn, and
invocation. Load at most 256 paths and 256 KiB of aggregate content, then render
at most 1,200 lines and 256 KiB without cutting a file section. Current bytes
appear only after the recorder revalidates the committed fingerprint and target,
parent, and ancestor identities. A refusal never falls back to reading the path.
The view has no rollback, apply, editor, index, worktree, or checkpoint action;
`/diff` remains the repository-against-HEAD surface.

An unchanged file is not read into the context twice. A full, uncapped
read arms a per-file record, and a later full read of byte-identical
content answers with a short marker instead of the bytes, which is §6.7's
own framing: hashing prevents re-injection, never relocation — the content
already sits in the prefix, exactly where the cache wants it. The skip is
armed only by a complete read (a partial read updates the stale check and
proves nothing about what the context holds), a mutation or external
change disarms it by hash inequality, and /undo and every session swap
clear it alongside the read versions, in the same struct so the two cannot
drift. A skipped read still refreshes the stale check, so write-after-read
behaves identically either way.

Going back in the conversation is /fork, for the same reason:
`internal/session/fork.go` copies a log's prefix into a new session and
never writes the source, so the fork's messages are byte-identical to what
was already sent and a warm provider prefix stays warm. The cut has to land
on a turn boundary — the first dropped message must be the user message
that opened its turn — because a conversation cut mid-turn leaves tool
calls without results and every request built from it is malformed (§10.3).
Fork branches the log only: files are /undo's job, and the checkpoint
recorder is process-scoped, so it keeps working across the swap.

A pin is a name for a cut, and nothing more. /pin records the message
count the session held when the user said so (`session.AppendPin`), /fork
resolves the name back to that count, a reused name moves its pin rather
than stacking a second, and a numeric name is refused because /fork
already reads a number as a turn count. A pin past a fork's cut does not
ride the fork — it names a point the new log does not contain — and no
pin promises anything about files, because the log cannot keep that
promise.

/retry is a composition, not a mechanism: the last turn's files are eligible
only when the checkpoint stack's top scope has the exact
`TurnIdentity{SessionID, OpeningMessage}` of the durable opening — its display
label is never identity. The conversation goes back through a staged retry
fork, which first records the replay intent and stays undiscoverable. The
prepared restore installs every pre-image and publishes that child as one
commit; any restore or pre-publication failure rolls every path forward to its
post-image, retains the source session, and consumes no checkpoint evidence.
A visible-but-not-confirmed-durable publication retains recovery evidence and
stops before another workspace mutation. Only after adoption succeeds does the
source answer receive its non-fatal `user_corrected` note. The recorded opening
then replays byte-for-byte — deliberately not re-expanded — and a retry onto
another rung uses the /tN one-shot, probe and restore included. `lastTurnOpening`
exists because injected advice and watch reports are user-role messages that
did not open a turn; a retry that replayed one would replay a fragment.

**A race arm is byte-identical upstream and read-only downstream.** /race
(`cmd/sb/race.go`) runs one prompt on two rungs from two forks of the
session, and every constraint follows from one of two facts. Fact one: the
arms' requests share the session's prefix, so an arm reuses the primary's
system blocks and a `Registry.Branch` of its registry — same schema bytes,
but empty §6.7 file-version and read-skip authority. Read provenance cannot
be transferred safely from the primary or another arm; each arm must establish
its own reads, and a read in one must not arm a skip in the other. Fact two:
two branches ran and one will be discarded, so neither may act — each arm
gets a fresh plan-mode permission engine, which denies every non-read effect
before rules or remembered answers are consulted, in every session mode,
bypass included;
the delegate tool is schema-kept and Plan-refused because its subagents
would run under the primary's engine, not the arm's. The verdict is §8.4's
strongest label class — a paired, human-judged comparison, with a tie
recorded as the cheaper rung sufficing and an abandoned race censored —
appended as a `race` record to the session that continues, and deliberately
never consumed by routing: phase 2b collects the corpus, phase 7 gates
acting on it. /budget preflights the sum of both arms' upper bounds, both
gates charge the shared total, a race whose lanes resolve to one target is
refused as measuring nothing, and the losing branch's log survives,
labelled, for /resume.

## Build phase

Phase 0 of §19.2: minimal loop, streaming, `read`/`write`/`edit`/`exec`,
permission model, sandbox capability report, crash-safe session log, one
provider, minimal REPL. The exit gate is that a small verified task corpus
completes safely and sessions resume after forced interruption.

Phase 3's TUI is built and is the default surface. Its phase-3 obligations from
§14 hold: streaming text renders through a plain fast path and is re-rendered
once per completed block through glamour, completed entries cache per width so
repaints never re-render markdown, and diffs highlight once at load. Keep it
that way.

**A modal is queued interaction with an owner, never replaceable UI state.**
Every permission ask, model question, and asynchronous picker enters through
the one FIFO broker. Permission starts on No, and a delayed picker starts with
no selection, so a stray Enter cannot authorize or choose. Active and queued
dialogs keep cancellation callbacks and asynchronous identity tokens; Esc,
Ctrl-C, context cancellation, exit, and a committed session swap must resolve
or cancel the exact owner and unblock its waiter rather than merely hiding it.

**Prompt history is private convenience data, and the live draft is not
history.** Durable records are bounded and credential-redacted. Unix requires
mode 0600; Windows creates a protected DACL before writing and verifies through
the handle that its only non-inherited allow ACE names the current user.
Failure to establish or re-verify that posture fails the history operation
closed. Up-arrow captures the unsent draft as the temporary end entry; while
traversal is active its arrows outrank popups and multiline editing, and an edit
or committed session swap clears traversal without replacing the visible text.

**Composer character movement and deletion are extended-grapheme operations.**
Left/right and Ctrl-B/F, plus Backspace/Delete and Ctrl-H/D, use the same ANSI
segmenter as rendering so combining marks, emoji modifiers, ZWJ sequences, and
flags stay atomic. At a logical newline boundary they defer to textarea's line
merge/crossing behavior. Do not add a rune-wise alias to a user-facing
character operation.

The TUI's look has its own invariants, pinned by the design tests. The
transcript anchors at the top and scrolling clamps to the content, so a
short session never floats above empty rows and never scrolls past itself.
The composer never paints the bubbles textarea's default cursor-line
background — that slab reads as a broken artifact on any tinted terminal —
and its frame takes the permission mode's color when the mode is anything
but default. The status bar sheds luxuries before facts on a narrow
terminal, in order: sparkline, clock, effort, routing dots; the mode, the
spend, and the context number never leave. The routing-history dots are
fed by every rebind, whoever asked for it, because they must agree with
/why about how much the session moved. The streaming rate is chars over
four and its readout says ~ because it is an estimate, not a count a
provider reported. The turn verdict closes a tool rail with └ only when a
rail is directly above it; after prose the corner would hang from nothing.
Transcript search (ctrl+f) paints its match markers into the page margin
on a copy at view time — the flat line buffer never carries search state,
because that buffer is the render cache and search is a lens, not an
edit. docs/tui.svg is generated, not drawn: `SB_FRAMES=<dir> go test
./cmd/sb/ -run TestCaptureFrames` renders the frames from the real view
code.

Phase 4's extensibility has landed — modern and initialization-era MCP over
stdio and Streamable HTTP, hooks, workspace trust, named subagent definitions,
native skills, direct native MCP activation, and bounded plugin
inventory/activation — along with the `glob`/`grep`/`todo`/`ask` tools and
phase 6's `delegate`, each under the constraints above. “Plugin support” does
not erase the component matrix in `docs/extensions.md`: plugin skills and
digest-trusted baseline MCP assemble, while the other recognized plugin
components remain inventory-only. Native skills include explicit-only legacy
Claude command libraries; this does not make plugin commands executable. The
learned router remains absent because phase 7's gate has no clean multi-rung
training decision, and the phase 8 platform program remains out of scope.

## Working here

    go build ./...
    go vet ./...
    go test ./...

Platform-specific files carry build tags that a host-only build never
exercises, so check the other targets before claiming a change is portable:

    GOOS=windows GOARCH=amd64 go vet ./...
    GOOS=linux GOARCH=amd64 go vet ./...

Tests that drive a POSIX shell or signal a process group are tagged `unix`.

The Linux confinement cannot be exercised from macOS, so changes to it are
verified in a container:

    docker build -f Dockerfile.linuxdev -t sb-linuxdev .
    docker run --rm --privileged -v "$PWD:/src" -w /src sb-linuxdev go test ./...

`--privileged` is needed because Docker's kernel blocks the unprivileged user
namespaces bubblewrap depends on. See `docs/sandbox.md`.

**A pid that exists is not a process that is running.** Signal 0 asks only
whether the kernel still has an entry, and it keeps one for a zombie until
something reaps it. In a container with no init process nothing does, so a
descendant the runner killed correctly answers "still here" forever. Any test
that probes for a surviving process has to read its state, not just its pid;
`processIsRunning` in `runner_test.go` is the one that does, and it is why the
container loop above needs no `--init`.

The same image carries a Secret Service, so the Linux credential store is
verified against a real keyring rather than a description of one:

    docker run --rm -v "$PWD:/src" -w /src sb-linuxdev bash -c '
      eval "$(dbus-launch --sh-syntax)"; export DBUS_SESSION_BUS_ADDRESS
      printf "p\n" | gnome-keyring-daemon --unlock --components=secrets >/dev/null 2>&1 &
      sleep 2; SB_LIVE=1 go test ./internal/credential/'


The phase-1 exit gate lives in `internal/gate` and is run, not described:

    SB_LIVE=1 go test ./internal/gate/ -run TestExitGate -v -timeout 40m

It runs the same corpus on both pinned targets and measures the token estimator
against what each server reported. Its companion,
`TestEstimatorStaysWithinTheDocumentedBound`, defends the numbers in
`docs/estimator.md`; if a change to the system prompt, the tool schemas, or the
estimator moves the ratio, that test fails and the document is what has to be
updated. Do not widen the band to make it pass.

The parsers that eat untrusted bytes carry fuzz targets beside their
tests: the session record decoder (a crash-recovered tail is arbitrary
bytes by definition), the search backend's HTML, and the fence extractor
behind /copy code. In an ordinary test run each executes only its seed
corpus; `go test -fuzz` hunts. A finding is a crash in a stated recovery
path, so it is a bug, never a wontfix.

Tests must pass without network access or an API key. Provider behavior is
tested against recorded fixtures served by `httptest`; tests that need a live
model are guarded by `SB_LIVE=1` and skipped otherwise.

Planning documents, status summaries, and handoff notes do not get committed.
