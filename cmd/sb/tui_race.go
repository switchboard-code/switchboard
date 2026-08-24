package main

// /race: one prompt, two rungs, both answers on screen, the user picks
// which branch the session continues on. The mechanics live in race.go;
// what belongs here is the surface — parsing, the rails, the pick dialog,
// and the swap onto the winner. The escalation policy and the advisor sit
// this turn out: an arm that moved rungs mid-race would no longer be the
// rung under trial, so both arms run pinned by construction, with no
// watcher wired to move them.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/terminaltext"
	"github.com/switchboard-code/switchboard/internal/tools"
)

type raceProbeMsg struct {
	operation       uint64
	sourceID        string
	prompt          string // as typed; expansion happens when the race starts
	requestedA      config.Tier
	requestedB      config.Tier
	providerRetries int
	a, b            config.Tier
	ca, cb          provider.Provider
	na, nb          string // fallback substitution notes, rendered before content is sent
	err             error
}

// raceSetupMsg completes the ledger barrier and branch assembly off the UI
// goroutine. operation/sourceID reject a result that outlives its launching
// session, exactly like an asynchronous session swap.
type raceSetupMsg struct {
	operation       uint64
	sourceID        string
	probe           raceProbeMsg
	prompt          string
	opening         provider.Message
	providerRetries int
	staleProviders  bool
	before          session.State
	arms            [2]*raceArm
	release         func()
	err             error
}

type raceToolMsg struct {
	arm  int
	name string
}
type raceUsageMsg struct {
	arm int
	u   session.Usage
}
type raceNoticeMsg struct {
	arm         int
	level, text string
}
type raceArmDoneMsg struct {
	arm int
	err error
}

// raceRun is a race in flight. send is how the arm goroutines reach the
// program; tests point it at a collector instead of a tea.Program.
type raceRun struct {
	typed  string // what the user wrote, for the record and the transcript
	arms   [2]*raceArm
	before session.State // authoritative origin ledger before either arm ran
	// publishDurably is a deterministic fault seam for publication ordering
	// tests. Runtime races leave it nil and use Session.PublishDurably.
	publishDurably durableSessionPublisher

	cancel    context.CancelFunc
	cancelled bool
	done      [2]bool
	// workers joins the two branch goroutines during abnormal TUI teardown.
	// Their errors are written before each completion message is sent, so a
	// stopped Bubble Tea program can still finalize an abandoned, fully-settled
	// race after those messages are dropped.
	workers sync.WaitGroup
	exitErr [2]error

	// releaseAdvisor ends the ledger barrier acquired before the arm forks.
	// It stays held through verdict accounting and any winner bind.
	releaseAdvisor func()

	rails  [2]*entry
	tools  [2]int
	in     [2]int
	out    [2]int
	labels [2]string

	send func(tea.Msg)
}

// raceObserver forwards one arm's loop events into the program, labelled.
// Text and thinking deltas are deliberately dropped: two branches streaming
// into one transcript would interleave into noise, so the rails carry
// progress and the finished answers render whole, once each.
type raceObserver struct {
	arm  int
	send func(tea.Msg)
}

func (o *raceObserver) ThinkingDelta(string) {}
func (o *raceObserver) TextDelta(string)     {}
func (o *raceObserver) ToolStart(call provider.ToolUse, _ permission.Request) {
	o.send(raceToolMsg{arm: o.arm, name: call.Name})
}
func (o *raceObserver) ToolEnd(provider.ToolUse, permission.Request, tools.Result, time.Duration) {}
func (o *raceObserver) ToolBatchEnd(context.Context)                                              {}
func (o *raceObserver) Notice(level, text string) {
	o.send(raceNoticeMsg{arm: o.arm, level: level, text: text})
}
func (o *raceObserver) TurnUsage(u session.Usage) {
	o.send(raceUsageMsg{arm: o.arm, u: u})
}

// parseRaceArgs reads "/race tA tB prompt" or "/race tB prompt", the second
// form racing the active tier against tB. The prompt keeps its spacing;
// only the tier tokens are cut off the front.
func parseRaceArgs(app *tuiApp, args string) (config.Tier, config.Tier, string, error) {
	usage := errors.New("usage: /race [tier [tier]] <prompt> — one prompt on two rungs at once; the bare form races this rung against the next one up")
	first, rest, _ := strings.Cut(strings.TrimSpace(args), " ")
	if first == "" {
		return config.Tier{}, config.Tier{}, "", usage
	}
	a, ok := app.config.Tier(first)
	if !ok {
		// No tier named: the whole argument is the prompt, and the race is
		// the ladder's own question — this rung against the next one up,
		// which is the comparison every escalation decision is implicitly
		// making. At the top there is no up, and the error says which
		// direction is left.
		rank := app.rankOf(app.tier)
		if rank < 0 || rank+1 >= len(app.config.Tiers) {
			return config.Tier{}, config.Tier{}, "", fmt.Errorf(
				"%s is the top rung, so there is no next rung up to race; name the rungs, e.g. /race t1 <prompt>", app.tier.ID)
		}
		return app.tier, app.config.Tiers[rank+1], strings.TrimSpace(args), nil
	}
	second, tail, _ := strings.Cut(strings.TrimSpace(rest), " ")
	if b, ok := app.config.Tier(second); ok {
		if prompt := strings.TrimSpace(tail); prompt != "" {
			return a, b, prompt, nil
		}
		return config.Tier{}, config.Tier{}, "", usage
	}
	// One tier named: the sitting rung takes the other lane.
	if prompt := strings.TrimSpace(rest); prompt != "" {
		return app.tier, a, prompt, nil
	}
	return config.Tier{}, config.Tier{}, "", usage
}

func cmdRace(m *tuiModel, args string) tea.Cmd {
	a, b, prompt, err := parseRaceArgs(m.app, args)
	if err != nil {
		return noticeCmd("error", err.Error())
	}
	if a.ID == b.ID {
		return noticeCmd("error", "a race needs two different rungs; "+a.ID+" against itself measures nothing")
	}
	ctx, generation, sourceID, err := m.startOperation("race probe")
	if err != nil {
		return noticeCmd("warn", err.Error())
	}
	return m.ownOperationCmd(generation, m.probeRaceTiers(ctx, generation, sourceID, prompt, a, b, 0))
}

func (m *tuiModel) probeRaceTiers(ctx context.Context, generation uint64, sourceID, prompt string,
	a, b config.Tier, retries int,
) tea.Cmd {
	return func() tea.Msg {
		result := raceProbeMsg{operation: generation, sourceID: sourceID, prompt: prompt,
			requestedA: a, requestedB: b, providerRetries: retries}
		probedA, ca, na, err := m.app.providers.probeTierFallback(ctx, a)
		if err != nil {
			result.err = fmt.Errorf("%s cannot race: %w", a.ID, err)
			return result
		}
		probedB, cb, nb, err := m.app.providers.probeTierFallback(ctx, b)
		if err != nil {
			result.err = fmt.Errorf("%s cannot race: %w", b.ID, err)
			return result
		}
		if err := ctx.Err(); err != nil {
			result.err = err
			return result
		}
		result.a, result.b, result.ca, result.cb, result.na, result.nb = probedA, probedB, ca, cb, na, nb
		return result
	}
}

// targetTakesImages is the §4 evidence order for vision: a live probe that
// attested image input, then a catalog entry carrying it from its own
// verification. No evidence means no attach.
func (m *tuiModel) targetTakesImages(target provider.RouteTarget) bool {
	if attested, known := m.app.providers.probedVision(target); known {
		return attested
	}
	info, _, ok := m.app.catalog.Lookup(target)
	return ok && info.Vision
}

// onRaceProbe has both clients answering; assemble the arms and start.
func (m *tuiModel) onRaceProbe(msg raceProbeMsg) tea.Cmd {
	if !m.operationMatches(msg.operation, msg.sourceID) {
		return nil
	}
	abort := func(level, text string) tea.Cmd {
		m.finishOperation(msg.operation, false)
		if text != "" {
			m.addNotice(level, text)
		}
		return m.nextQueuedTurn()
	}
	if m.operationCancelling {
		return abort("", "")
	}
	if msg.err != nil {
		return abort("error", msg.err.Error())
	}
	if m.app.providers != nil && (!m.app.providers.preparedClientCurrent(msg.ca) ||
		!m.app.providers.preparedClientCurrent(msg.cb)) {
		if msg.providerRetries >= maxProviderReplans {
			return abort("error", "provider settings kept changing while preparing the race; neither arm was sent")
		}
		a, b := msg.requestedA, msg.requestedB
		if a.ID == "" {
			a = msg.a
		}
		if b.ID == "" {
			b = msg.b
		}
		return m.ownOperationCmd(msg.operation,
			m.probeRaceTiers(m.turnCtx, msg.operation, msg.sourceID, msg.prompt, a, b, msg.providerRetries+1))
	}
	// Fallback may have substituted either lane's target, so the degenerate
	// check runs against what will actually serve, not what was named.
	if msg.a.Target.ID() == msg.b.Target.ID() {
		return abort("error", "both rungs resolve to "+msg.a.Target.Display()+"; a race against the same target measures nothing")
	}

	// Both arms resolve their rungs from the ladder directly rather than
	// through the router, so the destination policy is applied here or a race
	// is the way around it.
	for _, tier := range []config.Tier{msg.a, msg.b} {
		if err := destinationAllowed(m.app.config, tier.Target); err != nil {
			return abort("error", "this race cannot run: "+err.Error())
		}
	}

	expanded, images := m.expandMentions(msg.prompt)
	prompt := m.adviceContext(m.shellContext(expanded))
	if len(images) > 0 {
		for _, tier := range []config.Tier{msg.a, msg.b} {
			if !m.targetTakesImages(tier.Target) {
				return abort("error", tier.Target.Display()+" has no evidence it takes images, and a race is only fair if both arms see the same prompt; drop the image mention or race rungs that both take one")
			}
		}
	}
	// The same outbound gate as a plain turn, doubled in consequence: a key
	// in a race prompt would land in two branch logs and two providers.
	leaks := credential.ScanPrompt(prompt)
	start := func(p string) tea.Cmd {
		display := msg.prompt
		if len(leaks) > 0 && p != prompt {
			display = credential.Redact(display, leaks)
		}
		return m.startRaceArms(msg, p, images, display)
	}
	if len(leaks) > 0 {
		return m.openSecretGate(leaks, prompt, func(p string) tea.Cmd {
			return start(p)
		}, func() tea.Cmd { return abort("", "") })
	}
	return start(prompt)
}

// startRaceArms is onRaceProbe past the gates that can still stop it. The
// advisor barrier may wait for an inflight provider call, so every setup step
// runs as a cancellable command rather than blocking Bubble Tea's Update.
func (m *tuiModel) startRaceArms(msg raceProbeMsg, prompt string, images []provider.Image, display string) tea.Cmd {
	unstamped := turnOpeningAuthored(prompt, display, images)
	if !m.operationMatches(msg.operation, msg.sourceID) {
		return nil
	}
	if m.operationCancelling {
		m.finishOperation(msg.operation, false)
		return m.nextQueuedTurn()
	}
	// The prompt becomes transcript history only after every outbound gate has
	// resolved. A dropped credential-bearing race must leave no raw user card.
	m.addUser(display)
	ctx, generation, sourceID := m.turnCtx, msg.operation, msg.sourceID
	m.operationName = "race setup"
	app := m.app
	return m.ownOperationCmd(generation, func() tea.Msg {
		opening, err := stampTurnOpening(app.loop.Session, unstamped)
		if err != nil {
			return raceSetupMsg{operation: generation, sourceID: sourceID, probe: msg, prompt: prompt, err: err}
		}
		return m.prepareRaceSetup(ctx, generation, sourceID, msg, prompt, opening, msg.providerRetries)
	})
}

func (m *tuiModel) prepareRaceSetup(ctx context.Context, generation uint64, sourceID string,
	probe raceProbeMsg, prompt string, opening provider.Message, retries int,
) raceSetupMsg {
	app := m.app
	result := raceSetupMsg{operation: generation, sourceID: sourceID, probe: probe, prompt: prompt,
		opening: opening, providerRetries: retries}
	if app.providers != nil && (!app.providers.preparedClientCurrent(probe.ca) ||
		!app.providers.preparedClientCurrent(probe.cb)) {
		result.staleProviders = true
		return result
	}
	releaseAdvisor, err := pauseAdvisorLedger(ctx, app)
	if err != nil {
		result.err = fmt.Errorf("race could not stabilize the session ledger: %w", err)
		return result
	}
	result.release = releaseAdvisor
	result.before = app.loop.Session.State()
	if reason, blocked := racePreflight(app.budget, app.catalog, result.before,
		app.loop.System, app.loop.Tools.Definitions(), opening, probe.a, probe.b,
		app.providers.outputTokenAllowance); blocked {
		result.err = errors.New("no race: " + reason)
		return result
	}

	send := func(v tea.Msg) {
		if app.p != nil {
			app.p.Send(v)
		}
	}
	armA, err := assembleRaceArm(app, probe.a, probe.ca, &raceObserver{arm: 0, send: send})
	if err != nil {
		result.err = fmt.Errorf("race setup failed: %w", err)
		return result
	}
	result.arms[0] = armA
	armB, err := assembleRaceArm(app, probe.b, probe.cb, &raceObserver{arm: 1, send: send})
	if err != nil {
		result.err = fmt.Errorf("race setup failed: %w", err)
		return result
	}
	result.arms[1] = armB
	if err := ctx.Err(); err != nil {
		result.err = err
		return result
	}
	if err := recordRaceFallbackBindings(result.arms, [2]string{probe.na, probe.nb}); err != nil {
		result.err = err
		return result
	}
	raceGates(app.budget, app.catalog, app.loop.Session, result.before, armA, armB)
	return result
}

// recordRaceFallbackBindings is the last write-ahead boundary before either
// branch may call its provider. A failure on one arm aborts the whole race;
// the caller closes both staged logs, leaving any earlier arm record invisible
// for bounded maintenance.
func recordRaceFallbackBindings(arms [2]*raceArm, notes [2]string) error {
	for i, note := range notes {
		if note == "" {
			continue
		}
		arm := arms[i]
		if arm == nil || arm.sess == nil {
			return fmt.Errorf("race setup cannot record arm %d's fallback substitution without a session", i+1)
		}
		if err := arm.sess.AppendRuntimeBindingNote(arm.tier.ID, arm.tier.Target.ID(), false, "warn", note); err != nil {
			return fmt.Errorf("race setup failed to record %s's fallback substitution: %w", arm.tier.ID, err)
		}
	}
	return nil
}

func (m *tuiModel) retryRaceSetup(msg raceSetupMsg) tea.Cmd {
	ctx := m.turnCtx
	app := m.app
	return m.ownOperationCmd(msg.operation, func() tea.Msg {
		msg.arms = [2]*raceArm{}
		msg.release = nil
		msg.err = nil
		msg.staleProviders = false
		msg.providerRetries++
		probe := msg.probe
		a, b := probe.requestedA, probe.requestedB
		if a.ID == "" {
			a = probe.a
		}
		if b.ID == "" {
			b = probe.b
		}
		probedA, ca, na, err := app.providers.probeTierFallback(ctx, a)
		if err != nil {
			msg.probe.ca, msg.probe.cb = nil, nil
			msg.err = fmt.Errorf("%s cannot race after provider settings changed: %w", a.ID, err)
			return msg
		}
		probedB, cb, nb, err := app.providers.probeTierFallback(ctx, b)
		if err != nil {
			msg.probe.ca, msg.probe.cb = nil, nil
			msg.err = fmt.Errorf("%s cannot race after provider settings changed: %w", b.ID, err)
			return msg
		}
		probe.a, probe.b, probe.ca, probe.cb, probe.na, probe.nb = probedA, probedB, ca, cb, na, nb
		probe.providerRetries = msg.providerRetries
		if probedA.Target.ID() == probedB.Target.ID() {
			msg.err = fmt.Errorf("both rungs now resolve to %s after provider settings changed", probedA.Target.Display())
			return msg
		}
		for _, tier := range []config.Tier{probedA, probedB} {
			if err := destinationAllowed(app.config, tier.Target); err != nil {
				msg.err = fmt.Errorf("the refreshed race cannot run: %w", err)
				return msg
			}
			if messagesNeedVision([]provider.Message{msg.opening}) && !m.targetTakesImages(tier.Target) {
				msg.err = fmt.Errorf("%s no longer has evidence it takes images", tier.Target.Display())
				return msg
			}
		}
		return m.prepareRaceSetup(ctx, msg.operation, msg.sourceID, probe, msg.prompt, msg.opening, msg.providerRetries)
	})
}

func (m *tuiModel) onRaceSetup(msg raceSetupMsg) tea.Cmd {
	closeArms := func() {
		for _, arm := range msg.arms {
			if arm != nil && arm.sess != nil {
				_ = arm.sess.CloseDiscardingStaged()
			}
		}
	}
	if !m.operationMatches(msg.operation, msg.sourceID) {
		closeArms()
		if msg.release != nil {
			msg.release()
		}
		return nil
	}
	if m.operationCancelling {
		closeArms()
		if msg.release != nil {
			msg.release()
		}
		m.finishOperation(msg.operation, false)
		return m.nextQueuedTurn()
	}
	staleProviders := msg.staleProviders || (m.app.providers != nil &&
		(!m.app.providers.preparedClientCurrent(msg.probe.ca) || !m.app.providers.preparedClientCurrent(msg.probe.cb)))
	if staleProviders {
		closeArms()
		if msg.release != nil {
			msg.release()
		}
		if msg.providerRetries >= maxProviderReplans {
			m.finishOperation(msg.operation, false)
			m.addNotice("error", "provider settings kept changing while assembling the race; neither arm was sent")
			return m.nextQueuedTurn()
		}
		return m.retryRaceSetup(msg)
	}
	if msg.err != nil || msg.arms[0] == nil || msg.arms[1] == nil {
		closeArms()
		if msg.release != nil {
			msg.release()
		}
		m.finishOperation(msg.operation, false)
		if msg.err != nil && !errors.Is(msg.err, context.Canceled) {
			m.addNotice("error", msg.err.Error())
		}
		return m.nextQueuedTurn()
	}

	// Setup ownership becomes race ownership without opening an idle gap.
	m.finishOperation(msg.operation, true)
	for _, note := range []string{msg.probe.na, msg.probe.nb} {
		if note != "" {
			m.addNotice("warn", note)
		}
	}
	send := func(v tea.Msg) {
		if m.app.p != nil {
			m.app.p.Send(v)
		}
	}
	run := &raceRun{
		typed:          msg.probe.prompt,
		arms:           msg.arms,
		before:         msg.before,
		send:           send,
		releaseAdvisor: msg.release,
	}
	run.labels[0] = "a · " + raceTierLabel(msg.arms[0].tier)
	run.labels[1] = "b · " + raceTierLabel(msg.arms[1].tier)
	m.addNotice("", fmt.Sprintf("racing %s against %s — both branches read-only; you pick which continues",
		raceTierLabel(msg.arms[0].tier), raceTierLabel(msg.arms[1].tier)))
	for i, arm := range run.arms {
		run.rails[i] = m.tr.add(&entry{kind: kindInfo, text: run.railLine(i), rank: m.app.rankOf(arm.tier)})
	}
	m.tr.scrollToBottom()

	m.race = run
	m.busy = true
	m.started = time.Now()
	m.launchRace(run, msg.opening)
	return m.spin.Tick
}

func raceTierLabel(t config.Tier) string {
	if t.Label != "" {
		return t.ID + " " + t.Label
	}
	return t.ID
}

// launchRace starts both arms. Each runs a plain TurnMessage on its own
// goroutine against its own forked session; everything they report arrives
// as messages through run.send, so the model stays the only writer of UI
// state.
func (m *tuiModel) launchRace(run *raceRun, opening provider.Message) {
	ctx, cancel := context.WithCancel(context.Background())
	run.cancel = cancel
	run.workers.Add(len(run.arms))
	for i, arm := range run.arms {
		go func(i int, arm *raceArm) {
			defer run.workers.Done()
			arm.started = time.Now()
			err := arm.loop.TurnMessage(ctx, opening)
			arm.wall = time.Since(arm.started)
			run.exitErr[i] = err
			if run.send != nil {
				run.send(raceArmDoneMsg{arm: i, err: err})
			}
		}(i, arm)
	}
}

func (run *raceRun) railLine(i int) string {
	state := "running"
	if run.done[i] {
		state = run.arms[i].status
		if run.arms[i].wall > 0 {
			state += " " + run.arms[i].wall.Round(time.Second).String()
		}
	}
	line := fmt.Sprintf("%s — %s", run.labels[i], state)
	if run.tools[i] > 0 {
		line += fmt.Sprintf(" · %d tool calls", run.tools[i])
	}
	if run.in[i]+run.out[i] > 0 {
		line += fmt.Sprintf(" · ↓%s ↑%s", compact(run.in[i]), compact(run.out[i]))
	}
	return line
}

func (m *tuiModel) refreshRail(i int) {
	if m.race == nil || m.race.rails[i] == nil {
		return
	}
	m.race.rails[i].text = m.race.railLine(i)
	if idx := m.tr.indexOf(m.race.rails[i]); idx >= 0 {
		m.tr.invalidate(idx)
	}
}

func (m *tuiModel) onRaceTool(msg raceToolMsg) {
	if m.race == nil {
		return
	}
	m.race.tools[msg.arm]++
	m.refreshRail(msg.arm)
}

func (m *tuiModel) onRaceUsage(msg raceUsageMsg) {
	if m.race == nil {
		return
	}
	m.race.in[msg.arm] += msg.u.Usage.InputTokens + msg.u.Usage.CacheWriteTokens
	m.race.out[msg.arm] += msg.u.Usage.OutputTokens
	m.refreshRail(msg.arm)
}

func (m *tuiModel) onRaceNotice(msg raceNoticeMsg) {
	if m.race == nil {
		return
	}
	m.addNotice(msg.level, m.race.labels[msg.arm]+": "+msg.text)
}

func (m *tuiModel) onRaceArmDone(msg raceArmDoneMsg) tea.Cmd {
	run := m.race
	if run == nil {
		return nil
	}
	applyRaceArmOutcome(run, msg.arm, msg.err)
	m.refreshRail(msg.arm)
	if !run.done[0] || !run.done[1] {
		return nil
	}
	return m.onRaceFinished(run)
}

func applyRaceArmOutcome(run *raceRun, armIndex int, err error) {
	if run == nil || armIndex < 0 || armIndex >= len(run.arms) || run.done[armIndex] {
		return
	}
	arm := run.arms[armIndex]
	switch {
	case err == nil:
		arm.status = "completed"
	case errors.Is(err, context.Canceled):
		arm.status = "cancelled"
	case errors.Is(err, agent.ErrRoundLimit):
		arm.status = "round_limit"
	default:
		arm.status = "error"
		arm.failure = err.Error()
	}
	run.done[armIndex] = true
}

// finishRaceForExit settles an in-flight race after Bubble Tea has stopped
// accepting its completion messages. Both arms are cancelled and joined before
// their sessions are finalized, so no goroutine can append behind teardown and
// the advisor ledger barrier cannot be stranded in an embedding or test host.
func (m *tuiModel) finishRaceForExit(run *raceRun) {
	if run == nil {
		return
	}
	run.cancelled = true
	if run.cancel != nil {
		run.cancel()
	}
	run.workers.Wait()
	// A verdict dialog drained during teardown may already have synchronously
	// finalized this run. In that case there is nothing left to settle.
	if m.race != run {
		return
	}
	for i := range run.arms {
		applyRaceArmOutcome(run, i, run.exitErr[i])
	}
	clear(m.queue)
	m.queue = nil
	_ = m.finishRace(run, "", "abandoned")
}

// onRaceFinished has both arms answered, one way or another. Completed
// answers render whole; then the user judges, unless there is nothing left
// to judge.
func (m *tuiModel) onRaceFinished(run *raceRun) tea.Cmd {
	completed := 0
	for i, arm := range run.arms {
		if arm.status != "completed" {
			why := arm.status
			if arm.failure != "" {
				why += ": " + arm.failure
			}
			m.addNotice("warn", run.labels[i]+" has no answer ("+why+")")
			continue
		}
		completed++
		m.addInfo(run.labels[i])
		if text := lastAssistantText(arm.sess.State().Messages); text != "" {
			e := m.tr.add(&entry{kind: kindAssistant, text: text, rank: m.app.rankOf(arm.tier)})
			m.tr.finalize(e)
		}
	}
	m.tr.scrollToBottom()

	if completed == 0 {
		outcome := "incomparable"
		if run.cancelled {
			outcome = "abandoned"
		}
		return m.finishRace(run, "", outcome)
	}
	m.openDialog(newRaceDialog(m, run))
	return nil
}

// finishRace resolves the verdict: record, notes, closes, and — when a
// branch was kept — the swap onto it. pick names the kept arm ("a", "b"),
// or is empty when nothing continues; outcome is the Race vocabulary.
func (m *tuiModel) finishRace(run *raceRun, pick, outcome string) tea.Cmd {
	if run.releaseAdvisor != nil {
		defer func() {
			run.releaseAdvisor()
			run.releaseAdvisor = nil
		}()
	}
	m.race = nil
	m.busy = false

	var kept *raceArm
	switch pick {
	case "a":
		kept = run.arms[0]
	case "b":
		kept = run.arms[1]
	}

	keptTier := ""
	if kept != nil {
		keptTier = kept.tier.ID
	}
	// The record redacts unconditionally: it is a summary, not the
	// transcript, and a key pasted into the /race prompt must not ride a
	// summary into the log after the gate scrubbed it from what was sent.
	prompt := credential.Redact(run.typed, credential.ScanPrompt(run.typed))
	record := raceRecord(prompt, run.arms[0], run.arms[1], outcome, keptTier)
	if err := reconcileRaceAccounting(m.app.loop.Session, run.before, run.arms[0], run.arms[1]); err != nil {
		return m.abortRaceAccounting(run, record, "the race's cost ledger could not be reconciled: "+err.Error())
	}
	if err := transferRaceAccounting(m.app.loop.Session, run.before, run.arms[0], run.arms[1]); err != nil {
		return m.abortRaceAccounting(run, record, "race branch A's cost ledger could not be transferred: "+err.Error())
	}
	if err := transferRaceAccounting(m.app.loop.Session, run.before, run.arms[1], run.arms[0]); err != nil {
		return m.abortRaceAccounting(run, record, "race branch B's cost ledger could not be transferred: "+err.Error())
	}

	line := fmt.Sprintf("race %s vs %s: %s", run.arms[0].tier.ID, run.arms[1].tier.ID, outcome)
	if kept != nil {
		line += ", kept " + kept.tier.ID
	}
	m.raceLog = append(m.raceLog, line)

	if kept == nil {
		// Nothing continues: finalize both alternatives while hidden, then make
		// the verdict durable on the session that continues. Only after that
		// commit may the branch accounting records become resumable history.
		for _, arm := range run.arms {
			if err := arm.sess.FinalizeRaceBranchAlternative(); err != nil {
				return m.abortRaceAccounting(run, record, "a race branch could not be finalized: "+err.Error())
			}
			_ = arm.sess.AppendNote("info", "race: this branch was not kept ("+outcome+")")
		}
		if err := m.app.loop.Session.AppendRace(record); err != nil {
			return m.abortRaceAccounting(run, record, "the race verdict could not be saved: "+err.Error())
		}
		var durabilityUncertain error
	publicationLoop:
		for i, arm := range run.arms {
			outcome, rawErr := publishDurablyWith(arm.sess, run.publishDurably)
			disposition, err := publicationResult(outcome, rawErr, "race alternative "+arm.sess.ID())
			switch disposition {
			case publicationUnpublished:
				m.addNotice("warn", "a finalized race alternative stayed hidden because publication failed: "+err.Error())
				_ = arm.sess.CloseDiscardingStaged()
			case publicationVisibleUncertain:
				// The alternative is already discoverable, so it cannot be discarded.
				// Close it and the continuing source immediately: no later append or
				// sibling publication may follow a durability-uncertain commit.
				durabilityUncertain = errors.Join(durabilityUncertain, err)
				durabilityUncertain = errors.Join(durabilityUncertain, arm.sess.Close())
				durabilityUncertain = errors.Join(durabilityUncertain, m.app.loop.Session.Close())
				// A sibling whose publication was never attempted is still a hidden,
				// rollbackable stage. Discard it without making it discoverable.
				for _, pending := range run.arms[i+1:] {
					durabilityUncertain = errors.Join(durabilityUncertain, pending.sess.CloseDiscardingStaged())
				}
				break publicationLoop
			case publicationDurable:
				_ = arm.sess.Close()
			}
		}
		if durabilityUncertain != nil {
			m.shutdownErr = errors.Join(m.shutdownErr, durabilityUncertain)
			m.addNotice("error", durabilityUncertain.Error())
			m.quitting = true
			return tea.Quit
		}
		m.addNotice("", "race over, nothing kept; the session continues where it was")
		if len(m.queue) > 0 {
			next := m.queue[0]
			m.queue = m.queue[1:]
			return m.startTurn(next, "")
		}
		return nil
	}

	var other *raceArm
	for _, arm := range run.arms {
		if arm != kept {
			other = arm
		}
	}
	if err := other.sess.FinalizeRaceBranchAlternative(); err != nil {
		return m.abortRaceAccounting(run, record, "the other race branch could not be finalized: "+err.Error())
	}
	_ = other.sess.AppendNote("info", "race: this branch was not kept; the "+kept.tier.ID+" branch continued")
	if err := kept.sess.FinalizeRaceBranch(); err != nil {
		return m.abortRaceAccounting(run, record, "the race winner could not be finalized: "+err.Error())
	}

	// The record rides the branch that continues, appended before the swap
	// so it is durable whatever happens next.
	if err := kept.sess.AppendRace(record); err != nil {
		return m.abortRaceAccounting(run, record, "the race verdict could not be saved: "+err.Error())
	}
	origID := m.app.loop.Session.ID()
	loser := other
	loserID := loser.sess.ID()

	note := fmt.Sprintf("continuing on the %s branch; /resume %s returns to the pre-race session",
		kept.tier.ID, origID)
	// The swap applies here, on the UI goroutine, not through a command: a
	// command leaves a gap where the session is idle but still the old one,
	// and a prompt submitted into that gap would open a turn on a log about
	// to be closed.
	next := m.onSessionSwap(sessionSwapMsg{
		sess: kept.sess, tier: kept.tier, client: kept.client, note: note, keepFold: true,
		publishAfter: loser.sess, publishAfterNote: fmt.Sprintf(", /resume %s to the other answer", loserID),
	})
	if m.app.loop.Session == kept.sess {
		// An ordinary swap drops /why evidence from the log being left. This
		// verdict is different: it was durably appended to the branch just
		// adopted, so keep that one new-log fact without carrying older races
		// from the pre-race process view.
		m.raceLog = []string{line}
	}
	return next
}

// abortRaceAccounting fails closed on the pre-race session. The origin is the
// sole ledger while arms run, so staying there preserves every reservation;
// swapping despite a failed transfer would be the only unsafe choice.
func (m *tuiModel) abortRaceAccounting(run *raceRun, record session.Race, why string) tea.Cmd {
	record.Kept = ""
	if record.Outcome == "a" || record.Outcome == "b" || record.Outcome == "tie" {
		record.Outcome = "incomparable"
	}
	for _, arm := range run.arms {
		_ = arm.sess.AppendNote("warn", "race: accounting transfer failed; pre-race session continued")
		_ = arm.sess.CloseDiscardingStaged()
	}
	if err := m.app.loop.Session.AppendRace(record); err != nil {
		why += "; the race record also could not be saved: " + err.Error()
	}
	m.addNotice("error", why+"; staying on the pre-race session")
	if len(m.queue) > 0 {
		next := m.queue[0]
		m.queue = m.queue[1:]
		return m.startTurn(next, "")
	}
	return nil
}

// raceDialog is the verdict. Its options depend on what survived: two
// completed answers offer the full §8.4 vocabulary — either pick, the tie
// that keeps the cheaper rung, or neither — while a lone survivor can only
// be kept or declined, and the record calls that incomparable rather than
// a preference, because a comparison with one side is not one.
type raceDialog struct {
	m   *tuiModel
	run *raceRun
	ids []string
	lbl []string
	sel int
}

func newRaceDialog(m *tuiModel, run *raceRun) *raceDialog {
	d := &raceDialog{m: m, run: run}
	a, b := run.arms[0], run.arms[1]
	if a.status == "completed" {
		d.add("a", "keep "+run.labels[0]+"  "+raceCostLabel(m, a))
	}
	if b.status == "completed" {
		d.add("b", "keep "+run.labels[1]+"  "+raceCostLabel(m, b))
	}
	if a.status == "completed" && b.status == "completed" {
		cheaper := a
		if raceRank(m, b.tier) < raceRank(m, a.tier) {
			cheaper = b
		}
		d.add("tie", "tie — both suffice, keep the cheaper ("+cheaper.tier.ID+")")
	}
	d.add("drop", "neither; stay where the session was")
	// Race completion is asynchronous. The keystroke that happens to follow
	// the result must never adopt an arm, so Enter starts on the only outcome
	// that leaves the source session untouched. Arrow navigation is the
	// deliberate act that opts into keeping an answer.
	d.sel = len(d.ids) - 1
	return d
}

// raceRank orders a tie: the ladder position is the cost order by
// construction, and a tier off the ladder — a resumed ad-hoc target —
// sorts last rather than first, because rankOf's -1 would otherwise crown
// the one rung whose cost the ladder says nothing about.
func raceRank(m *tuiModel, tier config.Tier) int {
	if r := m.app.rankOf(tier); r >= 0 {
		return r
	}
	return len(m.app.config.Tiers)
}

func (d *raceDialog) add(id, label string) {
	d.ids = append(d.ids, id)
	d.lbl = append(d.lbl, label)
}

// raceCostLabel prices one arm for the pick, three meterings kept apart
// (§4): local consumed nothing scarce, plan consumed quota, and only
// dollars print as dollars.
func raceCostLabel(m *tuiModel, arm *raceArm) string {
	rec := arm.record()
	info, _, ok := m.app.catalog.Lookup(arm.tier.Target)
	switch {
	case !ok:
		return "unpriced"
	case info.Metering == catalog.Local:
		return "local"
	case info.Metering == catalog.Plan:
		return "plan quota"
	default:
		return catalog.Money(rec.CostMicroUSD).String()
	}
}

func (d *raceDialog) update(key tea.KeyMsg, _ *theme) (bool, tea.Cmd) {
	switch key.String() {
	case "esc":
		return true, d.cancel()
	case "up":
		if d.sel > 0 {
			d.sel--
		}
	case "down":
		if d.sel < len(d.ids)-1 {
			d.sel++
		}
	case "enter":
		return true, d.resolve(d.ids[d.sel])
	}
	return false, nil
}

func (d *raceDialog) cancel() tea.Cmd { return d.resolve("drop") }

func (d *raceDialog) resolve(id string) tea.Cmd {
	run := d.run
	a, b := run.arms[0], run.arms[1]
	bothCompleted := a.status == "completed" && b.status == "completed"
	switch id {
	case "a", "b":
		outcome := id
		if !bothCompleted {
			outcome = "incomparable"
		}
		return d.m.finishRace(run, id, outcome)
	case "tie":
		pick := "a"
		if raceRank(d.m, b.tier) < raceRank(d.m, a.tier) {
			pick = "b"
		}
		return d.m.finishRace(run, pick, "tie")
	default:
		outcome := "abandoned"
		if !bothCompleted {
			outcome = "incomparable"
		}
		return d.m.finishRace(run, "", outcome)
	}
}

func (d *raceDialog) view(width int, th *theme) string {
	return d.viewWithin(width, dialogUnlimitedHeight, th)
}

func (d *raceDialog) viewWithin(width, height int, th *theme) string {
	width = max(width, 1)
	if height <= 10 {
		lines := []string{
			fitCells(th.bold.Render(" keep which answer?"), width),
			fitCells(th.warn.Render(" ROUTING EVIDENCE"), width),
		}
		focus := 0
		safeLine := -1
		for i, id := range d.ids {
			label := ""
			switch id {
			case "a":
				label = raceCostLabel(d.m, d.run.arms[0]) + " · keep " + d.run.labels[0]
			case "b":
				label = raceCostLabel(d.m, d.run.arms[1]) + " · keep " + d.run.labels[1]
			case "tie":
				label = "tie · keep cheaper"
			default:
				label = "neither · stay here"
				safeLine = len(lines)
			}
			prefix := "  "
			style := th.dim
			if i == d.sel {
				prefix = th.accent.Render("▌ ")
				style = th.bold
				focus = len(lines)
			}
			lines = append(lines, fitCells(prefix+style.Render(terminaltext.Escape(label)), width))
		}
		hintLine := len(lines)
		lines = append(lines, th.faint.Render(fitCells("↑↓ · enter · esc neither", width)))
		return strings.Join(dialogWindow(lines, height, focus, 0, 1, safeLine, hintLine), "\n")
	}
	var b strings.Builder
	b.WriteString(wrapCells(th.bold.Render(" which answer does the session keep?"), width) + "\n")
	b.WriteString(wrapCells(th.dim.Render(" the pick is recorded as routing evidence; a tie says the cheaper rung sufficed"), width) + "\n\n")
	for i, label := range d.lbl {
		label = terminaltext.Escape(label)
		if i == d.sel {
			b.WriteString(wrapCellsBounded(th.accent.Render(" ▌ ")+th.bold.Render(label), width, 2) + "\n")
		} else {
			b.WriteString(wrapCellsBounded(th.dim.Render("   "+label), width, 2) + "\n")
		}
	}
	b.WriteString(wrapCells(th.faint.Render(" ↑↓ choose · enter keep · esc neither"), width))
	lines := strings.Split(b.String(), "\n")
	focus := dialogLine(lines, "▌", true)
	return strings.Join(dialogWindow(lines, height, focus, 0, len(lines)-1), "\n")
}
