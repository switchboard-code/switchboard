Switchboard 1.21 makes long-running coding sessions substantially harder to lose, misroute, or resume incorrectly, while turning the terminal UI into a more capable day-to-day workbench.

## Highlights

- **Crash-durable conversations.** Streamed assistant text is persisted before display, interrupted output stays visible as incomplete evidence, dangling tool calls are reconciled without replay, and session health explains torn tails or blocking corruption before adoption.
- **Transactional recovery.** `/retry` binds file restoration to the exact session opening, stages the replacement session, and either publishes the whole transition or rolls files forward. Recovery journals survive interruption at every commit boundary.
- **Validated compaction.** TUI and REPL share an injection-resistant seven-section handoff, explicit provenance, portable text-only projection, credential redaction, a 32 KiB bound, and structural validation before a compacted session can become resumable. Legacy transcripts without verified authored scope refuse plain/automatic compaction and accept `/compact <current objective>` as an explicit, narrowly framed scope anchor.
- **A safer, more responsive TUI.** One FIFO modal lane prevents overlapping approvals and questions; approvals start on No; dialogs remain usable in very small terminals; resize and scroll preserve semantic position; Unicode editing is grapheme-aware; slow discovery and file search stay off the UI loop.
- **Evidence-based model selection.** A rung now carries its concrete serving surface and output allowance through primary and fallback targets. Live context and capability evidence, provider-generation leases, vision state, budget reserve, and output limits are checked at route and call boundaries.
- **Staged subagent workflows.** Bounded multi-stage workflows can fan out tasks, carry redacted evidence forward, run interactively or headlessly, and use named agents without allowing repository text or worker output to widen authority.

## Security and integrity

- Workspace attachments and custom commands use anchored, bounded, regular-file-only reads and refuse replacement races, external symlinks, FIFOs, devices, invalid UTF-8, and oversized inventories.
- Advisor, auditor, learner, watch, bisect, shell-context, compaction, and delegate boundaries redact credentials before truncation and frame model-produced text as untrusted evidence.
- Checkpoint restoration uses root-bound capabilities, exact file identities, atomic exchange/no-replace publication, crash-safe cleanup ledgers, and conservative evidence retention whenever a path becomes ambiguous.
- Prompt history is private, bounded, per workspace, and credential-redacted before persistence. Unix uses mode 0600; Windows creates and verifies a protected current-user-only DACL and fails the history operation closed when the filesystem cannot retain it.
- Release actions are commit-pinned, test jobs receive read-only credentials, archives are reproducible, and existing release assets cannot be silently overwritten.

## Upgrade notes

- Session schema 5 migrates supported older logs on writable adoption. Read-only resume inventory never repairs or truncates a candidate.
- Custom or unlisted finite-context targets may require `max_output`; `/models` collects it, and the same bound applies to every fallback on that rung.
- Secure checkpoint publication fails closed on filesystems without the required atomic primitives. Windows checkpoint recovery is restricted to validated NTFS semantics; cross-filesystem recovery layouts are refused before mutation.

The release includes binaries for macOS, Linux, and Windows on amd64 and arm64,
plus `checksums.txt`. Windows provider credentials currently use environment
variables or credential helpers; no native credential-store backend ships yet.
