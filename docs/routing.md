# Routing and the model ladder

Switchboard routes work across an ordered ladder of model targets. The current
router is deterministic. It chooses a feasible tier before each user turn and
can move upward when the running turn produces evidence that the current tier
is stuck.

Tier configuration, profiles, and provider targets are covered in
[Installation and configuration](configuration.md).

## Opening a turn

An interactive process starts on the first reachable tier because no user
request exists yet. That bootstrap is not recorded as a routing decision.

Immediately before a user turn, Switchboard assembles the request it would
send and routes that request. The inputs include:

- the frozen system prompt and tool schemas;
- replayed messages and the new user message;
- file and image attachments;
- current provider capabilities, including tools and vision;
- context-window fit;
- cache state;
- the remaining dollar budget, including retry reserve.

A tier that fails a capability, context, availability, destination-policy, or
hard-budget check is not eligible. A user pin still passes these checks.

These checks use the concrete serving target, not only a model name. Provider,
surface, model, and parameters remain distinct through probing, caching,
pricing, resume, and display. A live enforced context window outranks a user
declaration; a declaration outranks metadata inferred from a model card; the
catalog is the final fallback. The same resolver is used by routing, direct
pins, resume, compaction, and the loop's last pre-send guard.

Capability evidence is tri-state where silence matters. A server that
explicitly reports text-only vision support overrides a broad catalog default;
an API that says nothing does not invent a negative result. Output headroom is
also the concrete adapter's wire allowance, not the catalog's largest possible
generation. For example, Anthropic's adaptive effort dialect reserves the
`max_tokens` it will actually send, while a token-budget dialect raises that
allowance only when the API requires it. Context and hard-budget calculations
consume that same value.

A tier's positive `max_output` is its highest-precedence hard allowance and
applies identically to the primary and every fallback. Ollama and
OpenAI-compatible adapters put that exact cap on the request. Anthropic's
token-budget dialect refuses an explicit cap that does not exceed
`budget_tokens`; it raises only an omitted default, never a value the user
bounded. Routing, moves, estimates, the budget gate, and the loop's final
pre-send check all use the same concrete wire allowance. Without an override,
a verified adapter allowance wins over the catalog. A custom or unlisted
target with a known context window and no finite allowance is refused: an
omitted server default is not evidence that a request fits.

`/destinations ollama anthropic` restricts every turn in the workspace to those
providers, and `/destinations any` removes the restriction. It is a hard
requirement rather than a preference: the filter checks it before economics, so
an excluded target is reported as policy and never as a price, and the same
check runs on the opening route, on a mid-turn move, on a `/tN` pin, on a
retry, and on resume. It also runs wherever a rung is resolved without the
router: the `summarizer`, `auditor`, `advisor`, and `approver` slots, both arms
of a `/race`, and the rung a `delegate` call names. That last one is the reason
the rest exist — the rung in a delegate call is the model's choice, so a policy
enforced only on user turns would be a policy a tool call walks around. A
directly resolved rung outside the policy is refused and says which slot or
call it was, never quietly substituted, because each of those callers named a
rung on purpose. The setting persists as `[routing] destinations` and is
refused when it would leave no configured rung reachable. The unit is a
provider name rather than a metering class, because where a server runs is not
something a target identity states: an OpenAI-compatible endpoint may be a
laptop or a data centre.

Use `/t3` to pin the session to tier 3. `/tier auto` removes the pin. A command
such as `/t3 fix the flaky test` runs one prompt on that feasible tier, then
returns to the previous routing state.

`/routing off` holds the current rung the way a pin does: the per-turn opening
route hard-checks it and goes no further, mid-turn escalation stays paused,
and relief is refused. A rung that cannot serve a turn is an actionable error
there, never a silent move. Signals are still detected and recorded, so `/why`
answers what the policy would have done. `/routing on` resumes automatic
routing. The setting persists, and the rung still changes when you change it.

## Movement during a turn

The sticky escalation policy watches repeated identical tool calls, tool error
spikes, new test-failure signatures, and hedging. A new failure reported by an
armed `/watch` command can contribute the same signal as a test run by the
model.

Signals may arrive while tools are running, but a move is evaluated only after
the model round and its tool work finish. Switchboard first prepares the new
provider binding and rechecks capabilities, context, and budget for the request
the destination would receive. The provider and sticky tier change together.
A failed probe, stale proposal, or rejected hard check leaves both unchanged.

Every applied move appears in the transcript. `/why` reports the opening
decision, rejected candidates, later moves, and the session cost repriced on
the other tiers.

## When a rung will not take the round

Two refusals end a turn that another rung could finish, and the ladder answers
both between rounds.

A request the bound target cannot hold is refused before anything leaves the
process. Instead of ending there, the session looks for a rung whose window
holds it, widest first, and continues the turn on that rung. An unpriced window
sorts last, because a window this program does not know the size of is not
evidence of room. When no rung fits, the refusal stands and compaction remains
the answer.

A target that spends its retries on rate limits, timeouts, or server faults is
a fact about one target at one moment. The session substitutes another rung and
says so in those words. It is recorded as a runtime binding and never as a
route record: the router chose nothing, and writing one would tell `/why`,
`/ladder`, and every per-rung total that a decision was made.

Both pass every check a primary binding passes, in the same order: probe,
capability, context, destination policy, then budget. A pinned session — or
one with routing off — refuses
relief outright, because the user has said which rung. A round that has
already emitted content is never substituted, since half a streamed message
finished by a second target is a turn nobody can attribute. At most two reliefs
run per turn.

## Fallbacks

A tier can list fallback targets. If the primary server is unavailable or the
model is missing, Switchboard probes the fallbacks in order and records the
substitution before sending content. Context, vision, destination, or budget
infeasibility does not unlock fallback; those are facts about the requested
turn or standing policy rather than server availability.

A fallback is an availability substitution. It does not change the tier's
meaning or count as a router move. Each fallback must pass the same capability,
context, and budget checks as the primary.

## Cache state

The router tracks the prefix a target is likely to hold. `/cache` reports the
active target's eligible prefix, modeled hit probability, reason, observed hit
count, and repeated-miss warnings. Modeled values are labeled as estimates.
Providers that do not report cache accounting remain unknown.

When a tier change abandons a warm target, the transcript reports the observed
warm prefix, modeled hit probability, and estimated value of resending that
prefix cold. It omits a number when the inputs cannot support one.

The token estimator and its measured error bounds are documented in
[Token estimator error](estimator.md).

## Budgets and metering

Switchboard keeps three forms of metering separate:

- local execution, which has no provider bill;
- plan or subscription quota;
- per-token dollar billing.

`/budget 2.50` sets a persistent dollar ceiling. The gate uses a conservative
upper bound. It applies before the opening route, before an escalation, and
before each provider call. Lowering the ceiling during a turn constrains later
rounds in that turn. Local and plan targets are not converted into dollar
values.

The accounting commands use the session log rather than reconstructed guesses:

| Command | Output |
| --- | --- |
| `/estimate <prompt>` | Low, expected, and high cost for the next assembled request on each tier; the active tier includes modeled cache warmth and other tiers are priced cold |
| `/cost` or `sb cost` | Current or recorded session totals, preserving local, plan, and dollar units |
| `/cost turns` | Billed calls, tokens, and dollars grouped by user turn; compaction, learning, advising, and command-approval calls remain in labeled purpose buckets; non-dollar work keeps its real metering |
| `/cost rungs` | The recorded session repriced cold on every tier, with context-infeasible calls reported instead of priced |
| `/stats` or `sb stats` | Workspace lifetime totals as routed and repriced on the current ladder |
| `sb stats all` | Totals across every recorded workspace, grouped by the workspace names stored in log headers |
| `/ladder` or `sb ladder` | Where turns opened, whether they stayed, and where moved turns ended |

Race losers and forks count in cost and stats because they made provider calls.
Subagents use their own session stores. Rung counterfactuals stay scoped to one
workspace and ladder.

## Paired routing evidence

`/race review this diff` forks the current session and runs the prompt on the
current tier and the next tier in parallel. `/race t3 ...` chooses the other
lane, and `/race t2 t3 ...` names both. Both branches are enforced read-only
until the user selects a result. The selected branch becomes the session; the
other remains resumable.

A tie keeps the lower configured tier. The verdict and both routes are
recorded. `/races`, `sb races`, and `/races all` or `sb races all` aggregate
those paired results at session, workspace, or global scope.

The production router does not train on race results automatically. Collection
and evaluation are separate so a partial corpus cannot change live behavior.

## Why there is no learned router

The historical evaluation journal does not currently provide a clean
multi-tier capability frontier. Its diagnostic projection has only one useful
tier, so it contains no routing choice worth fitting.

A learned model can ship only after a clean, harder corpus produces at least
two useful tiers and the candidate beats the deterministic heuristic after
runtime and distribution costs. The current implementation therefore keeps the
rules-based router and records the evidence needed to test a future candidate.

See [Routing evaluation](eval.md), [Head-to-head results](head-to-head-2026-08-16.md),
and [Product comparison](comparison.md) for the measured evidence.
