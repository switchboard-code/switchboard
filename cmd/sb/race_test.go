package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/advisor"
	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/tools"
)

// racedProvider scripts model calls the way internal/agent's tests do: each
// turn is a canned event list, and running out of turns is an error rather
// than an invented answer.
type racedProvider struct {
	turns []racedTurn
	calls int
}

type racedTurn struct{ events []provider.Event }

type blockingRaceAdvisorProvider struct {
	entered chan struct{}
	release chan struct{}
}

func (*blockingRaceAdvisorProvider) Name() string { return "blocking-advisor" }
func (p *blockingRaceAdvisorProvider) Stream(context.Context, provider.RouteTarget, provider.Request) (provider.EventStream, error) {
	close(p.entered)
	<-p.release
	return &racedStream{events: racedText("advice").events}, nil
}
func (*blockingRaceAdvisorProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}
func (*blockingRaceAdvisorProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{Reachable: true, ModelPresent: true}, nil
}

func racedText(text string) racedTurn {
	return racedTurn{events: []provider.Event{
		{Type: provider.EventTextDelta, Index: 0, Text: text},
		{Type: provider.EventDone, StopReason: provider.StopEndTurn, Usage: provider.Usage{InputTokens: 10, OutputTokens: 5}},
	}}
}

func racedToolCall(name, input string) racedTurn {
	return racedTurn{events: []provider.Event{
		{Type: provider.EventToolUse, Index: 0, ToolUse: &provider.ToolUse{ID: "call_1", Name: name, Input: json.RawMessage(input)}},
		{Type: provider.EventDone, StopReason: provider.StopToolUse, Usage: provider.Usage{InputTokens: 10, OutputTokens: 5}},
	}}
}

func (p *racedProvider) Name() string { return "scripted" }
func (p *racedProvider) Stream(context.Context, provider.RouteTarget, provider.Request) (provider.EventStream, error) {
	if p.calls >= len(p.turns) {
		return nil, errors.New("scripted provider ran out of turns")
	}
	turn := p.turns[p.calls]
	p.calls++
	return &racedStream{events: turn.events}, nil
}
func (p *racedProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}
func (p *racedProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{Reachable: true, ModelPresent: true}, nil
}

type racedStream struct {
	events []provider.Event
	i      int
}

func (s *racedStream) Next() (provider.Event, error) {
	if s.i < len(s.events) {
		ev := s.events[s.i]
		s.i++
		return ev, nil
	}
	return provider.Event{}, io.EOF
}
func (s *racedStream) Close() error { return nil }

// raceModel is testModel with what a race additionally needs: a session
// store the arms can fork in, a second tier, and a seeded conversation so
// the fork has a prefix.
func raceModel(t *testing.T) *tuiModel {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	targetA := provider.RouteTarget{Provider: "scripted", Surface: "local", ModelID: "small"}
	targetB := provider.RouteTarget{Provider: "scripted", Surface: "local", ModelID: "large"}
	sess, err := store.Create(workspace, targetA.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	if err := sess.AppendMessage(provider.UserText("earlier question")); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{Role: provider.RoleAssistant,
		Content: []provider.Block{provider.Text{Text: "earlier answer"}}}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Tiers: []config.Tier{
		{ID: "t1", Label: "light", Target: targetA},
		{ID: "t2", Label: "deep", Target: targetB},
	}}
	registry, err := tools.NewRegistry(workspace, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	loop := &agent.Loop{
		Session: sess,
		Target:  targetA,
		Tools:   registry,
		Perms:   permission.NewEngine(permission.ModeDefault, execution.Capability{}),
		System:  []provider.Block{provider.Text{Text: "system under test"}},
	}
	app := &tuiApp{
		loop:      loop,
		store:     store,
		config:    cfg,
		catalog:   &catalog.Catalog{Revision: "test"},
		tier:      cfg.Tiers[0],
		workspace: workspace,
	}
	m := newTUIModel(app, darkTheme(), newMarkdown(80, true), newTextarea())
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return m
}

func TestParseRaceArgs(t *testing.T) {
	m := raceModel(t)
	cases := []struct {
		args       string
		a, b       string
		prompt     string
		wantsError bool
	}{
		{args: "t2 fix the flaky test", a: "t1", b: "t2", prompt: "fix the flaky test"},
		{args: "t1 t2 fix the flaky test", a: "t1", b: "t2", prompt: "fix the flaky test"},
		{args: "t2 t1 which is better", a: "t2", b: "t1", prompt: "which is better"},
		{args: "t2", wantsError: true},
		{args: "t1 t2", wantsError: true},
		{args: "", wantsError: true},
		// No tier named: the whole argument is the prompt and the race is
		// this rung against the next one up. A word that merely looks like
		// a tier id is prose under the same rule.
		{args: "do a thing", a: "t1", b: "t2", prompt: "do a thing"},
		{args: "t9 do a thing", a: "t1", b: "t2", prompt: "t9 do a thing"},
	}
	for _, c := range cases {
		a, b, prompt, err := parseRaceArgs(m.app, c.args)
		if c.wantsError {
			if err == nil {
				t.Errorf("parse %q: expected an error, got %s vs %s %q", c.args, a.ID, b.ID, prompt)
			}
			continue
		}
		if err != nil {
			t.Errorf("parse %q: %v", c.args, err)
			continue
		}
		if a.ID != c.a || b.ID != c.b || prompt != c.prompt {
			t.Errorf("parse %q: got %s vs %s %q, want %s vs %s %q", c.args, a.ID, b.ID, prompt, c.a, c.b, c.prompt)
		}
	}

	// The bare form needs an up to race toward: at the top rung it refuses
	// with the direction that is left, rather than inventing a downward
	// race nobody asked for.
	m.app.tier = m.app.config.Tiers[len(m.app.config.Tiers)-1]
	if _, _, _, err := parseRaceArgs(m.app, "do a thing"); err == nil || !strings.Contains(err.Error(), "top rung") {
		t.Errorf("the bare form at the top rung should refuse with the reason, got %v", err)
	}
}

func TestRaceProbeDoesNotBlockUIOnInflightAdvisor(t *testing.T) {
	m := raceModel(t)
	blocking := &blockingRaceAdvisorProvider{entered: make(chan struct{}), release: make(chan struct{})}
	adv := advisor.New(agent.NopObserver{}, blocking, m.app.config.Tiers[1].Target, nil,
		advisor.WithBounds(1, time.Nanosecond))
	adv.StartTurn("task")
	req := permission.Request{Tool: "exec", Argv: []string{"go", "test", "./..."}}
	call := provider.ToolUse{ID: "call", Name: "exec", Input: json.RawMessage(`{"argv":["go","test","./..."]}`)}
	for range 12 {
		adv.ToolStart(call, req)
		adv.ToolEnd(call, req, tools.Result{Content: "FAIL: TestX", IsError: true}, time.Second)
	}
	select {
	case <-blocking.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("advisor consult did not enter its provider")
	}
	m.app.setAdvisor(adv)
	_, generation, sourceID, err := m.startOperation("race probe")
	if err != nil {
		t.Fatal(err)
	}

	probe := raceProbeMsg{
		operation: generation,
		sourceID:  sourceID,
		prompt:    "compare these answers",
		a:         m.app.config.Tiers[0],
		b:         m.app.config.Tiers[1],
		ca:        &racedProvider{},
		cb:        &racedProvider{},
	}
	updated := make(chan tea.Cmd, 1)
	go func() {
		_, cmd := m.Update(probe)
		updated <- cmd
	}()
	var setup tea.Cmd
	select {
	case setup = <-updated:
		if setup == nil {
			t.Fatal("race probe returned no asynchronous setup command")
		}
	case <-time.After(250 * time.Millisecond):
		close(blocking.release)
		t.Fatal("Update blocked while waiting for the advisor ledger")
	}
	if !m.operationActive || !m.busy {
		t.Fatal("race setup did not claim exclusive ownership before waiting")
	}

	setupResult := make(chan tea.Msg, 1)
	go func() { setupResult <- setup() }()
	select {
	case got := <-setupResult:
		t.Fatalf("race setup crossed the inflight advisor barrier: %#v", got)
	case <-time.After(30 * time.Millisecond):
	}
	close(blocking.release)
	var result raceSetupMsg
	select {
	case got := <-setupResult:
		var ok bool
		result, ok = got.(raceSetupMsg)
		if !ok {
			t.Fatalf("setup result = %#v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("race setup did not resume after advisor settlement")
	}

	// Invalidate before delivery so the assembled arms are closed without
	// launching provider calls; this test is about the UI/barrier boundary.
	m.interrupt()
	m.onRaceSetup(result)
}

func TestRaceProbeOwnsItsSourceBeforeAsyncProbe(t *testing.T) {
	m := raceModel(t)
	source := m.app.loop.Session
	probe := cmdRace(m, "t1 t2 compare these answers")
	if probe == nil || !m.operationActive || !m.busy || m.operationSourceID != source.ID() {
		t.Fatalf("race probe did not claim its source: cmd=%v active=%v busy=%v source=%q",
			probe != nil, m.operationActive, m.busy, m.operationSourceID)
	}
	generation, sourceID := m.operationGeneration, m.operationSourceID
	if clear := cmdClear(m, ""); clear == nil {
		t.Fatal("clear returned nothing during race probe")
	} else if notice, ok := clear().(noticeMsg); !ok || !strings.Contains(notice.text, "already running") {
		t.Fatalf("clear crossed the race-probe ownership barrier: %#v", notice)
	}
	if m.app.loop.Session != source {
		t.Fatal("race-probe overlap replaced the source session")
	}

	m.interrupt()
	if !m.operationCancelling || !m.busy {
		t.Fatal("probe cancellation released ownership before probe completion")
	}
	m.onRaceProbe(raceProbeMsg{operation: generation, sourceID: sourceID, err: context.Canceled})
	if m.operationActive || m.busy {
		t.Fatal("cancelled probe did not release ownership at completion")
	}
}

// The read-only rule for arms is enforced by a plan-mode engine, not by the
// asker or the mode the session runs in: a write the model asks for is
// denied with the reason, whatever bytes the shared system prompt claims
// about the session's own mode.
func TestRaceArmForksThePrefixAndRefusesMutation(t *testing.T) {
	m := raceModel(t)
	before := m.app.loop.Session.State()

	client := &racedProvider{turns: []racedTurn{
		racedToolCall("write", `{"path":"landed.txt","content":"from the race"}`),
		racedText("could not write, answering anyway"),
	}}
	arm, err := assembleRaceArm(m.app, m.app.config.Tiers[1], client, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	defer arm.sess.Close()

	if got := arm.sess.State().Target; got != string(m.app.config.Tiers[1].Target.ID()) {
		t.Errorf("arm session recorded target %q, want the rung under trial", got)
	}
	if got := len(arm.sess.State().Messages); got != len(before.Messages) {
		t.Fatalf("arm holds %d messages, want the full prefix of %d", got, len(before.Messages))
	}

	if err := arm.loop.TurnMessage(context.Background(), provider.UserText("try to write a file")); err != nil {
		t.Fatalf("arm turn failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.app.workspace, "landed.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a race arm wrote to the workspace")
	}
	var denial string
	for _, msg := range arm.sess.State().Messages {
		for _, b := range msg.Content {
			if res, ok := b.(provider.ToolResult); ok {
				denial = res.Content
			}
		}
	}
	if !strings.Contains(denial, "read-only") {
		t.Errorf("the model was not told why the write was refused: %q", denial)
	}
	if got := len(m.app.loop.Session.State().Messages); got != len(before.Messages) {
		t.Errorf("the race arm's turn reached the primary session: %d messages, was %d", got, len(before.Messages))
	}
}

// The concurrent path, end to end: two arms actually racing on their own
// goroutines, events arriving as messages, both rails resolving, and the
// verdict dialog opening with the full vocabulary. The channel is buffered
// and the test body is its only reader; the deadline turns a wiring
// mistake into seconds, not a hung suite.
func TestRaceRunsBothArmsToTheVerdict(t *testing.T) {
	m := raceModel(t)
	msgs := make(chan tea.Msg, 64)
	send := func(v tea.Msg) { msgs <- v }

	armA, err := assembleRaceArm(m.app, m.app.config.Tiers[0],
		&racedProvider{turns: []racedTurn{racedText("answer from the light rung")}},
		&raceObserver{arm: 0, send: send})
	if err != nil {
		t.Fatal(err)
	}
	armB, err := assembleRaceArm(m.app, m.app.config.Tiers[1],
		&racedProvider{turns: []racedTurn{racedText("answer from the deep rung")}},
		&raceObserver{arm: 1, send: send})
	if err != nil {
		t.Fatal(err)
	}
	run := &raceRun{typed: "which is better", arms: [2]*raceArm{armA, armB}, send: send}
	run.labels = [2]string{"a · t1 light", "b · t2 deep"}
	run.rails[0] = m.tr.add(&entry{kind: kindInfo, text: run.railLine(0)})
	run.rails[1] = m.tr.add(&entry{kind: kindInfo, text: run.railLine(1)})
	m.race = run
	m.busy = true

	m.launchRace(run, provider.UserText("which is better"))
	deadline := time.After(10 * time.Second)
	for m.dlg == nil {
		select {
		case v := <-msgs:
			m.Update(v)
		case <-deadline:
			t.Fatal("the race did not reach a verdict in time")
		}
	}

	d, ok := m.dlg.(*raceDialog)
	if !ok {
		t.Fatalf("the verdict is a %T, want the race dialog", m.dlg)
	}
	if len(d.ids) != 4 {
		t.Errorf("two completed arms offer %d options, want keep-a, keep-b, tie, and neither", len(d.ids))
	}
	for _, rail := range run.rails {
		if strings.Contains(rail.text, "running") {
			t.Errorf("a finished arm's rail still says running: %q", rail.text)
		}
	}
	var transcript strings.Builder
	for _, e := range m.tr.entries {
		transcript.WriteString(e.text + "\n")
	}
	for _, want := range []string{"answer from the light rung", "answer from the deep rung"} {
		if !strings.Contains(transcript.String(), want) {
			t.Errorf("a finished answer never rendered: %q", want)
		}
	}

	// Resolve through the dialog itself, the way a keypress would.
	m.dlg = nil
	d.resolve("a")
	if m.app.tier.ID != "t1" {
		t.Errorf("keeping a landed on %s, want t1", m.app.tier.ID)
	}
	if m.busy || m.race != nil {
		t.Error("the verdict did not release the session")
	}
}

// The verdict is the product: the picked branch becomes the session, the
// record rides it, and the road not taken stays on disk, labelled.
func TestFinishRaceSwapsOntoTheWinnerAndRecords(t *testing.T) {
	m := raceModel(t)
	origPath := m.app.loop.Session.Path()

	armA, err := assembleRaceArm(m.app, m.app.config.Tiers[0], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	armB, err := assembleRaceArm(m.app, m.app.config.Tiers[1], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	armA.status, armB.status = "completed", "completed"
	run := &raceRun{typed: "the raced prompt", arms: [2]*raceArm{armA, armB}}
	m.race = run
	m.busy = true

	winnerPath := armB.sess.Path()
	loserPath := armA.sess.Path()
	// The swap applies inside finishRace, synchronously: the model must
	// already be on the winner when this returns, gap-free.
	m.finishRace(run, "b", "b")

	if m.busy || m.race != nil {
		t.Error("the race did not release the session")
	}
	if got := m.app.loop.Session.Path(); got != winnerPath {
		t.Errorf("session is %s, want the winning branch %s", got, winnerPath)
	}
	if armB.sess.PublicationPending() || armA.sess.PublicationPending() {
		t.Fatal("finalized race left the winner or accounting alternative unpublished")
	}
	infos, err := m.app.store.List(m.app.workspace)
	if err != nil {
		t.Fatal(err)
	}
	seenWinner, seenLoser := false, false
	for _, info := range infos {
		seenWinner = seenWinner || info.Path == winnerPath
		seenLoser = seenLoser || info.Path == loserPath
	}
	if !seenWinner || !seenLoser {
		t.Fatalf("published race inventory missing winner=%v loser=%v: %#v", seenWinner, seenLoser, infos)
	}
	if m.app.tier.ID != "t2" {
		t.Errorf("active tier is %s, want the winning rung t2", m.app.tier.ID)
	}
	winnerLog, err := os.ReadFile(winnerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(winnerLog), `"type":"race"`) {
		t.Error("the winning branch's log holds no race record")
	}
	if !strings.Contains(string(winnerLog), `"outcome":"b"`) {
		t.Error("the race record does not carry the verdict")
	}
	loserLog, err := os.ReadFile(loserPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(loserLog), "not kept") {
		t.Error("the losing branch's log does not say it lost")
	}
	if _, err := os.ReadFile(origPath); err != nil {
		t.Errorf("the pre-race session left the disk: %v", err)
	}
	if len(m.raceLog) == 0 || !strings.Contains(m.raceLog[0], "kept t2") {
		t.Errorf("/why has no race line: %v", m.raceLog)
	}
}

func TestFinishRacePublicationCollisionKeepsOriginAndHidesBothBranches(t *testing.T) {
	m := raceModel(t)
	origin := m.app.loop.Session

	armA, err := assembleRaceArm(m.app, m.app.config.Tiers[0], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	armB, err := assembleRaceArm(m.app, m.app.config.Tiers[1], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	armA.status, armB.status = "completed", "completed"
	winnerPath, loserPath := armB.sess.Path(), armA.sess.Path()
	winnerID, loserID := armB.sess.ID(), armA.sess.ID()
	// A marker this creator does not own makes the winner's publication commit
	// fail. The losing answer must still be staged at this point so adoption can
	// close both branches and leave them invisible for bounded maintenance.
	if err := os.WriteFile(winnerPath+".published", []byte("foreign publication marker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := &raceRun{typed: "the raced prompt", arms: [2]*raceArm{armA, armB}}
	m.race = run
	m.busy = true

	m.finishRace(run, "b", "b")

	if m.app.loop.Session != origin {
		t.Fatalf("publication failure adopted %s, want origin %s", m.app.loop.Session.ID(), origin.ID())
	}
	for _, branch := range []struct{ id, path string }{
		{id: winnerID, path: winnerPath},
		{id: loserID, path: loserPath},
	} {
		assertRetainedUnpublishedStage(t, m.app.store, m.app.workspace, branch.id, branch.path)
	}
	infos, err := m.app.store.List(m.app.workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range infos {
		if info.Path == winnerPath || info.Path == loserPath {
			t.Fatalf("failed race branch became discoverable: %+v", info)
		}
	}
}

// A tie is a preference for the cheaper rung: both sufficed, so the ladder
// order — cheapest first — decides which branch carries on.
func TestRaceTieKeepsTheCheaperRung(t *testing.T) {
	m := raceModel(t)

	armA, err := assembleRaceArm(m.app, m.app.config.Tiers[1], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	armB, err := assembleRaceArm(m.app, m.app.config.Tiers[0], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	armA.status, armB.status = "completed", "completed"
	run := &raceRun{typed: "tie prompt", arms: [2]*raceArm{armA, armB}}
	m.race = run
	m.busy = true

	d := newRaceDialog(m, run)
	d.resolve("tie")
	// Arm A raced t2, arm B raced t1; the tie keeps t1, the cheaper rung.
	if m.app.tier.ID != "t1" {
		t.Errorf("tie kept %s, want the cheaper t1", m.app.tier.ID)
	}
	winnerLog, err := os.ReadFile(armB.sess.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(winnerLog), `"outcome":"tie"`) {
		t.Error("the kept branch's record does not say the race was a tie")
	}
}

// §15 applied twice over: the arms run at once, so both upper bounds have
// to fit under the ceiling at once.
func TestRacePreflightCoversBothArms(t *testing.T) {
	cat, target := pricedTarget(t)
	tierA := config.Tier{ID: "t1", Target: target}
	tierB := config.Tier{ID: "t2", Target: target}
	before := session.State{}
	opening := provider.UserText("a race under a ceiling")

	bs := &budgetState{}
	if reason, blocked := racePreflight(bs, cat, before, nil, nil, opening, tierA, tierB); blocked {
		t.Fatalf("no ceiling set, but the race was refused: %s", reason)
	}

	bs.set(1 * catalog.MicroUSD)
	reason, blocked := racePreflight(bs, cat, before, nil, nil, opening, tierA, tierB)
	if !blocked {
		t.Fatal("a one-micro-dollar ceiling let a two-arm race through")
	}
	if !strings.Contains(reason, "/budget") {
		t.Errorf("refusal %q does not say how to raise the ceiling", reason)
	}

	// A later race shares the session's pessimistic reserve for provider
	// attempts that failed without returning billable usage.
	before.ID = "session-with-debt"
	bs.set(1_000 * catalog.USD)
	if reason, blocked := racePreflight(bs, cat, before, nil, nil, opening, tierA, tierB); blocked {
		t.Fatalf("generous ceiling refused debt-free race: %s", reason)
	}
	before.RetryReserveMicroUSD = int64(1_000 * catalog.USD)
	if reason, blocked := racePreflight(bs, cat, before, nil, nil, opening, tierA, tierB); !blocked || !strings.Contains(reason, "reserved for failed attempts") {
		t.Fatalf("race ignored retry debt: blocked=%v reason=%q", blocked, reason)
	}
}

func TestRacePreflightRefusesKnownPaidArmWithoutConservativePrice(t *testing.T) {
	cat := catalogWithLocalModels(t,
		localModelSpec{name: "pricing-gap", contextWindow: 10_000, inputPerMTok: "1", outputPerMTok: "1", priceMaxInput: 500},
		localModelSpec{name: "covered", contextWindow: 10_000, inputPerMTok: "1", outputPerMTok: "1"},
		localModelSpec{name: "explicit-free", contextWindow: 10_000, inputPerMTok: "0", outputPerMTok: "0"},
	)
	gap := ollamaTier("t1", "pricing-gap")
	covered := ollamaTier("t2", "covered")
	opening := provider.UserText(strings.Repeat("x", 600))
	if reason, blocked := racePreflight(nil, cat, session.State{}, nil, nil, opening, gap, covered); !blocked ||
		!strings.Contains(reason, "no positive conservative cost bound") {
		t.Fatalf("unpriceable paid race arm was admitted without a ceiling: blocked=%v reason=%q", blocked, reason)
	}
	free := ollamaTier("t3", "explicit-free")
	if reason, blocked := racePreflight(nil, cat, session.State{}, nil, nil, opening, free, covered); blocked {
		t.Fatalf("explicit all-zero per-token race arm was refused: %s", reason)
	}
}

func TestRaceRetryReserveUsesTheOriginAsItsAuthoritativeLedger(t *testing.T) {
	cat, target := pricedTarget(t)
	info, _, _ := cat.Lookup(target)
	bound := preflightBoundForTarget(info, target, 10_000)
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	newSession := func() *session.Session {
		sess, err := store.Create(t.TempDir(), target.ID(), cat.Revision)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { sess.Close() })
		return sess
	}
	origin := newSession()
	aSess, bSess := newSession(), newSession()
	a := &raceArm{sess: aSess, loop: &agent.Loop{Session: aSess, Target: target}}
	b := &raceArm{sess: bSess, loop: &agent.Loop{Session: bSess, Target: target}}
	bs := &budgetState{}
	raceGates(bs, cat, origin, origin.State(), a, b)
	if err := a.loop.Budget(10_000, 1); err != nil {
		t.Fatal(err)
	}
	if err := a.loop.BudgetResult(10_000, 1, session.Usage{}, errors.New("arm failed")); err != nil {
		t.Fatal(err)
	}
	if got := origin.State().RetryReserveMicroUSD; got != int64(bound) {
		t.Fatalf("origin retry reserve = %d, want %d", got, bound)
	}
	for name, sess := range map[string]*session.Session{"a": aSess, "b": bSess} {
		if got := sess.State().RetryReserveMicroUSD; got != 0 {
			t.Fatalf("%s copied a live origin reserve: %d", name, got)
		}
	}
}

func TestRaceAccountingTransfersTotalCostAndDebtToEveryResumableArm(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	newSession := func(target string) *session.Session {
		sess, err := store.Create(workspace, provider.RouteTargetID(target), "rev")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sess.Close() })
		return sess
	}
	origin := newSession("scripted/local/origin")
	aSess, bSess := newSession("scripted/local/a"), newSession("scripted/local/b")
	before := origin.State()
	a := &raceArm{sess: aSess, baseCost: 0}
	b := &raceArm{sess: bSess, baseCost: 0}
	if err := aSess.AppendUsage(session.Usage{CostMicroUSD: 40_000}); err != nil {
		t.Fatal(err)
	}
	if err := bSess.AppendUsage(session.Usage{CostMicroUSD: 60_000}); err != nil {
		t.Fatal(err)
	}
	// Represents other paid work on the origin after the arm snapshot (for
	// example, an advisor consult that was already admitted). It must not be
	// lost merely because it is neither arm's local Usage.
	if err := origin.AppendUsage(session.Usage{CostMicroUSD: 10_000}); err != nil {
		t.Fatal(err)
	}
	for _, cost := range []int64{40_000, 60_000} {
		id, err := origin.BeginBudgetAttempt(100_000)
		if err != nil {
			t.Fatal(err)
		}
		if err := origin.SettleBudgetAttempt(id, session.BudgetOutcomeSucceeded, cost); err != nil {
			t.Fatal(err)
		}
	}
	failed, err := origin.BeginBudgetAttempt(70_000)
	if err != nil {
		t.Fatal(err)
	}
	if err := origin.SettleBudgetAttempt(failed, session.BudgetOutcomeFailed, 0); err != nil {
		t.Fatal(err)
	}
	if err := reconcileRaceAccounting(origin, before, a, b); err != nil {
		t.Fatal(err)
	}
	if err := transferRaceAccounting(origin, before, a, b); err != nil {
		t.Fatal(err)
	}
	if err := transferRaceAccounting(origin, before, b, a); err != nil {
		t.Fatal(err)
	}
	for name, sess := range map[string]*session.Session{"a": aSess, "b": bSess} {
		state := sess.State()
		if state.AccountedCostMicroUSD() != 110_000 {
			t.Fatalf("%s branch can recover headroom after resume: %+v", name, state)
		}
		if state.RetryReserveMicroUSD != 70_000 {
			t.Fatalf("%s branch retry debt = %d, want 70000", name, state.RetryReserveMicroUSD)
		}
		if state.Calls != 1 || state.Usage != (provider.Usage{}) {
			// AppendUsage with an empty Usage intentionally records one real local
			// call; transfers must not add another or invent tokens.
			t.Fatalf("%s branch provider telemetry was fabricated: %+v", name, state)
		}
	}
}

func TestEmptyRaceArmInheritsAccountingLineage(t *testing.T) {
	m := raceModel(t)
	origin, err := m.app.store.Create(m.app.workspace, m.app.tier.Target.ID(), m.app.catalog.Revision)
	if err != nil {
		t.Fatal(err)
	}
	original := m.app.loop.Session
	m.app.loop.Session = origin
	defer func() {
		m.app.loop.Session = original
		_ = origin.Close()
	}()
	if err := origin.AppendBudgetTransfer("pre-race", 12_345, 54_321); err != nil {
		t.Fatal(err)
	}
	if got := len(origin.State().Messages); got != 0 {
		t.Fatalf("fixture has %d messages, want an empty first-turn lineage", got)
	}
	arm, err := assembleRaceArm(m.app, m.app.config.Tiers[0], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	defer arm.sess.Close()
	state := arm.sess.State()
	if state.AccountedCostMicroUSD() != 12_345 || state.RetryReserveMicroUSD != 54_321 {
		t.Fatalf("empty race arm dropped inherited accounting: %+v", state)
	}
}

func TestFinalizedRaceWinnerCanRaceAgain(t *testing.T) {
	m := raceModel(t)
	first, err := assembleRaceArm(m.app, m.app.config.Tiers[0], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.sess.FinalizeRaceBranch(); err != nil {
		t.Fatal(err)
	}
	// A winner becomes a valid source only after the real adoption seam's
	// publication commit. This test bypasses finishRace/onSessionSwap, so mirror
	// that final step explicitly before starting another race from it.
	if err := first.sess.Publish(); err != nil {
		t.Fatal(err)
	}
	origin := m.app.loop.Session
	m.app.loop.Session = first.sess
	defer func() {
		_ = first.sess.Close()
		_ = origin.Close()
	}()

	second, err := assembleRaceArm(m.app, m.app.config.Tiers[1], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatalf("winner's finalized lifecycle marker blocked a second race: %v", err)
	}
	defer second.sess.Close()
}

func TestRaceReconcileRetainsActualCostBesideUnsettledBound(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	origin, err := store.Create(workspace, "scripted/local/origin", "rev")
	if err != nil {
		t.Fatal(err)
	}
	aSess, err := store.Create(workspace, "scripted/local/a", "rev")
	if err != nil {
		t.Fatal(err)
	}
	bSess, err := store.Create(workspace, "scripted/local/b", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer origin.Close()
	defer aSess.Close()
	defer bSess.Close()
	before := origin.State()
	a := &raceArm{sess: aSess}
	b := &raceArm{sess: bSess}
	if _, err := origin.BeginBudgetAttempt(100_000); err != nil {
		t.Fatal(err)
	}
	if err := aSess.AppendUsage(session.Usage{CostMicroUSD: 25_000}); err != nil {
		t.Fatal(err)
	}
	if err := reconcileRaceAccounting(origin, before, a, b); err != nil {
		t.Fatal(err)
	}
	state := origin.State()
	if state.ExternalCostMicroUSD != 25_000 || state.RetryReserveMicroUSD != 100_000 {
		t.Fatalf("unsettled race accounting = %+v", state)
	}
}

// Nothing kept: the record lands on the session that continues — the one
// the race started from — and both branches close labelled.
func TestFinishRaceAbandonedRecordsOnTheOriginal(t *testing.T) {
	m := raceModel(t)

	armA, err := assembleRaceArm(m.app, m.app.config.Tiers[0], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	armB, err := assembleRaceArm(m.app, m.app.config.Tiers[1], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	armA.status, armB.status = "completed", "completed"
	run := &raceRun{typed: "abandoned prompt", arms: [2]*raceArm{armA, armB}}
	m.race = run
	m.busy = true

	origPath := m.app.loop.Session.Path()
	m.finishRace(run, "", "abandoned")
	if got := m.app.loop.Session.Path(); got != origPath {
		t.Fatal("abandoning a race swapped sessions")
	}
	log, err := os.ReadFile(m.app.loop.Session.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), `"outcome":"abandoned"`) {
		t.Error("the pre-race session's log holds no abandoned race record")
	}
	if m.busy || m.race != nil {
		t.Error("an abandoned race did not release the session")
	}
}
