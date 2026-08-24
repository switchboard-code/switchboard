# Product comparison

This comparison is dated 2026-08-23. It separates repository-backed
Switchboard behavior, measured results, and external product reports.
Competitor behavior changes quickly, so external claims should be rechecked
before use in a release announcement.

## Scope

Claude Code, Codex CLI, OpenCode, and Switchboard all provide the basic coding
agent loop: file operations, shell commands, repository instructions, session
resume, and extensibility through MCP or an equivalent mechanism. The useful
differences are in routing, cost controls, state recovery, verification, and
the authority granted to extensions.

The table summarizes Switchboard's current position. “No comparable surface”
means the public material reviewed for this dated comparison did not describe
one. It does not prove that another product lacks an internal mechanism.

| Area | Switchboard | External baseline in this review |
| --- | --- | --- |
| Model selection | Ordered multi-provider target ladder with deterministic per-turn routing and evidence-based moves between completed rounds | User or configuration selects the model before the work |
| Route explanation | `/why` records feasible and rejected targets, moves, and counterfactual cost | No comparable ladder explanation surface found |
| Metering | Local execution, plan quota, and dollar billing remain separate | Usage is usually reported after calls |
| Hard budget | Retry-inclusive dollar ceiling checked before routes, moves, and provider calls | No comparable model-selection budget gate found |
| Cache state | Per-target modeled warmth with observed provider accounting | Cache discounts may be documented without a live routing belief |
| Session branching | Append-only logs with fork, named pins, retry, recap, and line provenance | Resume and checkpoint features vary by product |
| Resume integrity | Read-only health shows the exact surface, incomplete output, pending tool repair, continuity, recoverable tail, and blocking corruption; adoption is workspace-bound | [Claude Code sessions](https://code.claude.com/docs/en/sessions) describe continuous local saves, resume, branch, and compact; public guarantees differ at crash and corruption boundaries |
| Stream crash durability | Assistant deltas are synced before display and collapse into one message on success; interrupted output stays visible but is withheld from later model requests | No comparable public visible-before-durable contract was found in the reviewed session guides |
| Terminal workbench | Searchable command palette, revision-aware file and literal search, bounded Git diff with an omission inventory, and built-in semantic LSP views | Terminal and IDE surfaces divide this work differently; integration breadth varies |
| Verification | User-armed watch, turn bisect, paired races, a second-rung audit of a turn's claims against its record, and a router evaluation gate | Hooks and test commands are common; no equivalent combined surface found |
| Command safety | Sandbox off by default; opt-in verified confinement; explicit yolo mode for unconfined host access | Products expose sandbox or approval modes with different guarantees |
| Standing permissions | Rules in the user's own file, refused when they reach as wide as a mode, yielding to a credential-bearing request, and answerable offline with `sb permissions -- <command>` under a stated scope | Persisted allowlists are common; no comparable dry-run with a stated coverage boundary was found |
| Interaction safety | One FIFO modal lane, denial-first approvals, exact cancellation ownership, and 20-column bounded dialogs | Approval and question surfaces vary; this row makes no claim about another product's default selection |
| CLI discovery | Static help before config or extension discovery; generated completion follows the dispatcher's closed grammar | Help and completion depth vary by product and release |
| Scripting | `-output json` for one result object, `-output stream-json` for typed events as they happen, with a stable last line and exit codes a script can branch on | Streaming JSON output is common; the coverage and stability of the event vocabulary vary |
| Extensions | Compatible native skills, local plugins, direct and trusted plugin MCP with server-initiated elicitation, hooks, and one subagent level with up to four independent calls in an all-delegate batch | The reviewed guides list skill, plugin, provider, and LSP surfaces that Switchboard does not yet match; this review did not rank ecosystems |
| Delegation boundary | Child prompts end with a runtime contract; cross-agent text is credential-scanned and returned as untrusted evidence | [Claude Code's parallel-agent guide](https://code.claude.com/docs/en/agents) exposes broader orchestration choices; authority and evidence boundaries are product-specific |
| Computer control | macOS Accessibility tool under the normal permission engine | Hosted or API computer-use surfaces exist; terminal integration varies |

## Routing, cost, and cache

Switchboard routes the assembled request immediately before each user turn.
Capability, context, availability, and hard-budget checks exclude infeasible
targets. A user pin must pass the same checks. During a turn, repeated tool
calls, error spikes, new failure signatures, and hedging can propose a move.
The provider binding changes only after the current model round and its tool
work finish. See [Routing and the model ladder](routing.md).

Cost keeps three units: local execution, plan quota, and dollars. `/estimate`
prices a prospective request, `/cost` reports the recorded session, `/stats`
aggregates workspace history, and `/cost rungs` reprices the session cold on
each tier. `/budget` sets a persistent dollar ceiling. The conservative gate
is implemented in `cmd/sb/budget.go`; accounting and counterfactual commands
read recorded calls rather than reconstructing them.

Cache state belongs to a target, not a model name. `/cache` reports the
eligible prefix, modeled hit probability, reason, observed hits, and repeated
misses. A target that does not report cache accounting remains unknown. The
token estimator's measured error and the bound used by the cost model are in
[Token estimator error](estimator.md).

The learned router is absent. A model can ship only after a clean evaluation
produces at least two useful tiers and the candidate beats the deterministic
policy after runtime and distribution costs. The current evidence and the
failed historical matrix-integrity check are in
[Routing evaluation](eval.md).

## Session evidence and verification

The session record is append-only. `/fork` branches from an earlier message
prefix without rewriting the source log. `/pin` names a point, and `/retry`
replays the recorded opening bytes on the same or another feasible tier.
`/undo` restores captured write and edit changes without changing the sent
conversation. Shell and manual side effects remain outside that checkpoint.
Writes publish atomically. Edit and undo compare the expected file state
immediately before publication and refuse a mismatch already present. The
comparison and rename are not one atomic pathname CAS.

`/review [turn]` reads the same checkpoint evidence without consuming or
restoring it. It shows one retained write/edit mutation turn and refuses stale
or redirected current bytes; bare `/review` means the open turn and never an
older fallback. The bounded TUI panel has no apply, rollback, editor, Git-index,
or worktree action. `/diff` remains the separate repository view against
`HEAD`, including untracked files.

A bounded, redacted continuity capsule can accompany the append-only history.
It preserves recorded todo state and the derived next action across a restart,
fork, retry, or compaction boundary without changing the visible user prompt.
An undelivered valid capsule is injected once into the appropriate opening or
compact seed, stays bound to its message boundary, and never grants file-read
or execution authority.

Compaction has one shared TUI/REPL implementation. Its prompt treats transcript,
repository, tool, and user-emphasis text as untrusted source data and requests
exact sections for the active objective and execution frontier. The resulting
handoff is credential-redacted, byte-bounded, and structurally validated before
the new session can be published. This follows the high-signal context posture
described in [Anthropic's context-engineering guidance](https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents)
and the outcome/constraint/frontier guidance in
[OpenAI's model guidance](https://developers.openai.com/api/docs/guides/latest-model),
while keeping the runtime checks independent of either provider.

Streamed assistant output is write-ahead checkpointed before it is observable.
A crash therefore may lose provider bytes that were never shown, but not text
the user already saw. On resume, incomplete output is evidence rather than a
completed turn, and dangling tool calls are closed once with an explicit
unknown-outcome error. Fresh child logs for clear, compact, fork, retry, and
race stay undiscoverable until adoption publishes them, preventing an aborted
operation from becoming `--continue`'s newest session.

`/blame <path>` replays recorded write and edit operations against the current
file. A surviving line can therefore be attributed to a session, turn, tier,
target, and prompt. Lines created by a shell, formatter, hand edit, or work
that predates the logs remain unknown. `/recap`, `/find`, `/changes`, and
`/mistakes` expose other parts of the same record.

Files the model read are watched for change by anything other than its own
write and edit calls, and the drift is reported at the next round boundary
rather than at the refusal a later edit would hit. The sweep uses the hashes
the stale check already keeps, stats before it hashes, is twice bounded, and
never refreshes what the model was shown, so the refusal it anticipates still
stands.

`/audit` reads the turn that just finished on a second rung: the closing
message is the claim, the recorded tool calls with their results and the
checkpoint captures are the evidence, and a finding is where the two disagree.
It reads the record and not the code, takes no turn number because the message
log and the recorder number turns separately, and states its scope every time,
including that a shell command's side effects are outside the recorder and so
make a claim unchecked rather than wrong. With no `[slots] auditor` bound it
runs on the rung that made the claims and says so.

`/watch <command>` runs a user-selected verifier after edit rounds and reports
only changed results. A new mid-turn failure can contribute routing evidence.
`/bisect` searches captured per-turn checkpoints for the green-to-red
transition and restores the original tree on every exit path. `/race` runs a
read-only prompt on two tiers and records the user's verdict without training
the production router. See [Sessions and command reference](session.md) for
the operational limits of each command.

## Safety and extension authority

Switchboard treats permission, containment, workspace trust, and extension
activation as separate decisions.

- The sandbox starts off. `on` requires Seatbelt on macOS or a provenance-checked
  system bubblewrap on Linux to pass a live self-test. `auto` uses a verified
  profile when present.
- `bypass` suppresses prompts only when verified confinement isolates host
  network and IPC. Both current production profiles retain host IPC, so bypass
  asks today. `yolo` is a separate explicit grant of full host reach.
- MCP and computer-use tools act outside the command sandbox. `bypass` never
  auto-approves them; `yolo` does, because the everything-grant exempts
  nothing.
- Repository hooks, project MCP, and language servers require Switchboard
  workspace trust. Another client's remembered trust does not transfer.
- Native MCP definitions need explicit Switchboard activation, applicable
  policy, trust where required, and a supported runtime feature set.
- Plugin executable components need independent enablement and trust bound to
  the current plugin-tree digest.
- Prompts, attachments, command output, web requests, and computer-use text
  are checked for known credential forms before egress.

See [Security](security.md), [Confining commands](sandbox.md), and
[Native extension compatibility](extensions.md) for the exact boundaries.

Native compatibility is deliberately narrower than format discovery. Safe
Codex and Claude skill subsets assemble. Claude legacy command files are
manual-only. Enabled plugin skills and compatible trusted plugin MCP assemble.
Plugin hooks, agents, commands, apps, workflows, and LSP declarations remain
inventory-only or unsupported. Native OAuth, SSE, WebSocket, helper,
remote-execution, and approval semantics that the runtime cannot preserve fail
closed.

The optional macOS `computer` tool drives application controls through the
Accessibility API. It requires per-application approval, scans text before
typing, redacts text read back, and does not claim screenshot support. See
[Computer use](computer.md).

## Measured results

The historical routing evaluation cannot support a release verdict. Its
journal contains duplicate cells and predates the identity fields needed to
bind rows to an exact commit, catalog, prompt, ladder, and model snapshot. The
current evaluator refuses such a journal. Diagnostic projections also show
only one useful tier, so there is no learned-routing decision to fit.

The dated head-to-head run used eleven seeded Go defects and the same verifier
for Switchboard, Claude Code, and Codex CLI. All three solved 11 of 11 tasks.
Switchboard completed four entirely on local tiers, used plan quota for the
rest, and billed zero API dollars. Claude Code reported $22.35 over ten
reporting runs; one solved task reached the watchdog limit. Codex CLI reported
641,576 plan-metered tokens. These numbers establish behavior on that corpus,
machine, and day. They do not rank general coding quality. See
[Head-to-head results](head-to-head-2026-08-16.md).

The token estimator measurement covered eighteen calls on one model through
two adapters. It undercounted by as much as 24 percent. The cost model widens
its upper bound from that measurement; it does not treat the result as an exact
provider invoice.

## What this fixes

Public issue trackers contain useful failure reports, but an issue is not a
prevalence estimate and may be fixed after this date. Reddit links below are
individual community anecdotes or discussions, not verified incident rates.
The first column names failure modes Switchboard treats as product requirements;
it does not claim that every named product or user encounters them. Rows marked
as design requirements are engineering hazards, not competitor incident claims.

| Reported failure mode | External examples | Switchboard response |
| --- | --- | --- |
| Compaction or restart loses active task state | [Codex #27555](https://github.com/openai/codex/issues/27555), [Codex #32169](https://github.com/openai/codex/issues/32169), [Claude Code #32407](https://github.com/anthropics/claude-code/issues/32407), [Claude Code #34872](https://github.com/anthropics/claude-code/issues/34872) | The source session stays append-only. A bounded continuity capsule carries recorded todo state and the derived next action once across the boundary; compaction still has a preview. Switchboard does not claim that a capsule can preserve unsupported native workflow controls. |
| Usage is hard to explain or grows unexpectedly | [“300M tokens for a day?”](https://www.reddit.com/r/ClaudeCode/comments/1sgh2dc/300m_tokens_for_a_day/), [“Saying 'hey' cost me 22%”](https://www.reddit.com/r/ClaudeAI/comments/1s3hh29/saying_hey_cost_me_22_of_my_usage_limits/) | `/estimate` gives a pre-send range, `/budget` enforces a hard dollar ceiling, and the durable accounting ledger tags model work by purpose so turns, compaction, learning, advising, and command approval remain distinguishable. |
| Terminal work hangs or cancellation does not settle cleanly | [Cursor terminal-action thread](https://www.reddit.com/r/cursor/comments/1msdwto/i_really_wish_cursor_would_fix_the_agent_choking/) | Cancellation is bounded and reaches active transports. macOS and Linux terminate the process group or tree; Windows terminates the direct child and warns that descendants may survive. Prompts entered during work stay visible in `/queue`; recovery paths drop them when the workspace cannot be restored safely. |
| MCP approval has no usable unattended or parent-visible path | [Codex #18268](https://github.com/openai/codex/issues/18268), [Codex #24135](https://github.com/openai/codex/issues/24135), [Claude Code #61315](https://github.com/anthropics/claude-code/issues/61315) | External calls remain explicit permission effects. Headless and race contexts fail closed instead of waiting. Delegated agents do not inherit bridged MCP tools. |
| Tool and plugin schemas consume context, collide, or load twice | [Claude plugin duplication thread](https://www.reddit.com/r/ClaudeAI/comments/1rij9tr/psa_your_claude_code_plugins_are_probably_loading/), [Cline tool-injection discussion](https://github.com/cline/cline/discussions/8578) | Plugins need explicit Switchboard enablement; executable components also need digest trust. Exact plugin identities and bridged tool-name collisions are resolved deterministically, MCP filters are enforced, and list changes apply on the next run. |
| File-edit retries or false success leave the working tree hard to trust | [Individual Windsurf editing thread](https://www.reddit.com/r/Codeium/comments/1j9eott/anyone_else_having_issues_with_windsurf_editing/) | First-party writes publish atomically and reject an already-stale read token immediately before publication; `/diff` shows tracked and untracked results. This does not prove an edit is semantically correct or capture shell-written changes in undo. |
| Session history is hard to search, export, or recover | [Windsurf feature request #127](https://github.com/Exafunction/codeium/issues/127) | Append-only local logs support `/find`, `/export`, `/recap`, resume, and bounded continuity. Provider-side state is not treated as the only copy. |
| Repository automation or attached output exposes secrets | General risk, not a prevalence claim | Repository hooks require trust, MCP child environments are scrubbed or restricted, and outbound text passes the credential gate. |
| Agent confidence or instruction following outruns verification | [LocalLLaMA software-engineering thread](https://www.reddit.com/r/LocalLLaMA/comments/1vavh2h/software_engineers_do_you_honestly_get_anything/) | Watch, bisect, race, and the evaluation gate keep test evidence separate from model claims. |
| An editor, formatter, shell, or overlapping turn changes a file before an agent edit or undo publishes | Design requirement; no competitor prevalence claim | Per-path transactions compare the expected state immediately before atomic publication and refuse an observed mismatch. This is not an atomic pathname CAS against a simultaneous external replacement. |
| A change review calls the tree clean while untracked work exists, or attributes later bytes to an agent turn | Design requirement; no competitor prevalence claim | `/diff` reads staged, unstaged, and untracked state without changing the index. `/review` separately revalidates exact recorded write/edit mutations and refuses stale current bytes. |
| A diagnostics panel looks authoritative despite seeing only published documents | Design requirement; no competitor prevalence claim | `/problems` labels freshness and partial push coverage. An empty view explicitly does not claim that the repository passes its verifier. |
| Extension startup noise either hides the prompt or a mandatory failure | Design requirement; no competitor prevalence claim | A bounded risk-first summary preserves retained mandatory severity, `/doctor extensions` shows every retained ordered detail, and buffer overflow reports its exact loss instead of claiming completeness. |
| Parallel delegate work hides status, cost, or approval ownership | Design requirement; no competitor prevalence claim | `/tasks` names current-session work and targeted cancellation; approvals serialize with task identity and results rejoin in call order. Task IDs and status are process-local, while delegate session logs remain durable. |
| Help is unavailable because config, inventory, or provider setup is broken | CLI operability requirement; no competitor prevalence claim | Root, subcommand, and nested action help run before update checks or runtime state. Parse errors, ordinary failures, and cancellation keep distinct shell exit statuses. |

Switchboard still has open limits here. Compaction quality depends on the
configured summarizer. Shell side effects are not captured by undo or bisect.
Unconfined user hooks and armed watch commands run with the user's authority.
Fail-closed MCP behavior can make a native definition unavailable until its
semantics are implemented.

## Where Switchboard is narrower

The August 2026 source review found capabilities that Switchboard does not yet
match: Claude Code skill and plugin surfaces, agent-team workflows, IDE
integrations, and MCP OAuth; Codex CLI profiles, sandbox postures, IDE and cloud
surfaces, and extension distribution; and OpenCode provider and language-server
catalogs. This is a dated feature inventory, not a normalized measure of
ecosystem size, adoption, or quality.

Switchboard currently supports four language-server families, one subagent
level, and computer control only on macOS. Its plugin installer copies exact
local sources and does not fetch from a marketplace. Advanced native MCP
authentication, transport, and approval features remain disabled unless the
runtime can enforce them. Of the client roles an MCP server can invoke,
elicitation is answered on a surface that has a user; sampling and roots stay
refused. An image in an MCP tool result reaches a
target that reads images, delivered as a round-boundary message rather than
inside the tool result, so no adapter mapping or wire format is involved; audio
and other non-text blocks are still named and omitted. The router remains deterministic until the
evaluation gate passes.

## Reproduce it

The repository provides a deterministic one-task-per-package cut of the eval
corpus. Materialize one lane:

```sh
SB_BENCH_MATERIALIZE=/tmp/bench/sb \
  go test ./internal/eval/ -run TestMaterializeBench
```

For each task directory and prompt in `manifest.jsonl`, run the agent under
test. The 2026-08-16 run used these non-interactive forms:

```sh
sb -p "<prompt>" -mode bypass -output json
claude -p "<prompt>" --permission-mode acceptEdits \
  --allowedTools "Bash(go test:*)" "Bash(go build:*)" --output-format json
codex exec --sandbox workspace-write --skip-git-repo-check "<prompt>"
```

That Switchboard build enabled verified Seatbelt confinement automatically.
Current builds start with confinement off, and both production profiles retain
host IPC authority that keeps bypass approvals with the human. The historical
bypass run therefore has no equivalent prompt-free headless posture under the
current boundary.

Judge the materialized lane with the same verifier:

```sh
SB_BENCH_VERIFY=/tmp/bench/sb \
  go test ./internal/eval/ -run TestVerifyBench -v
```

`SB_BENCH_CUT=all` widens both materialization and verification to every task.
Verdicts go to `verdicts.jsonl`. The completed run and its caveats are in
[Head-to-head results](head-to-head-2026-08-16.md), with the raw JSONL beside
it.

## External references

Competitor feature-inventory observations were checked against public guides
available in August 2026. They are not ecosystem rankings. The issue links in
[What this fixes](#what-this-fixes) are primary user reports. They document
individual failures or requests, not product-wide rates.

- [Claude Code feature reference](https://toolsbase.dev/en/reference/claude-code-features)
- [Claude Code settings reference](https://hidekazu-konishi.com/entry/claude_code_features_settings_reference_2026.html)
- [Codex CLI profiles and sandbox guide](https://www.digitalapplied.com/blog/codex-cli-deep-dive-config-profiles-sandbox-2026)
- [Codex CLI guide](https://blakecrosley.com/guides/codex)
- [OpenCode overview](https://www.explainx.ai/blog/opencode-open-source-ai-coding-agent-guide-2026)
- [OpenCode feature summary](https://vibecodinghub.org/tools/opencode)
