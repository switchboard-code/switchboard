package main

// /advisor: a second model watching the session, wired per §9.2 — advice,
// never edits, bounded per turn. Advice renders in the transcript the moment
// it arrives and reaches the model at the next safe seam: mid-turn through
// the loop's injection point, or folded into the next prompt if the turn
// already ended.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/advisor"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

type adviceMsg struct{ text string }

type advisorReadyMsg struct {
	adv             *advisor.Advisor
	tier            config.Tier
	client          provider.Provider
	providerRetries int
	action          string
	err             error
	operation       uint64
	sourceID        string
}

const advisorUsage = "usage: /advisor [on|off|status]"

func cmdAdvisor(m *tuiModel, args string) tea.Cmd {
	switch strings.TrimSpace(args) {
	case "", "status":
		if m.operationActive && strings.HasPrefix(m.operationName, "advisor ") {
			return noticeCmd("", m.operationName+" is in progress")
		}
		advisor := m.app.currentAdvisor()
		if advisor == nil {
			return noticeCmd("", "advisor is off; /advisor on binds "+describeAdvisorChoice(m.app)+"")
		}
		return noticeCmd("", "advisor is on, using "+advisor.Target().Display())
	case "on":
		if m.operationActive && m.operationName == "advisor on" {
			return noticeCmd("", "advisor is already starting")
		}
		if advisor := m.app.currentAdvisor(); advisor != nil {
			return noticeCmd("", "advisor is already on, using "+advisor.Target().Display())
		}
		ctx, generation, sourceID, err := m.startOperation("advisor on")
		if err != nil {
			return noticeCmd("warn", err.Error())
		}
		return m.ownOperationCmd(generation, startAdvisor(ctx, m.app, generation, sourceID))
	case "off":
		if m.operationActive && m.operationName == "advisor on" {
			if !m.operationCancelling && m.turnCancel != nil {
				m.operationCancelling = true
				m.turnCancel()
				m.addNotice("", "cancelling advisor on")
			}
			return nil
		}
		adv := m.app.currentAdvisor()
		if adv == nil {
			return noticeCmd("", "advisor is already off")
		}
		ctx, generation, sourceID, err := m.startOperation("advisor off")
		if err != nil {
			return noticeCmd("warn", err.Error())
		}
		return m.ownOperationCmd(generation, func() tea.Msg {
			// Detaching without draining would make a later fork unable to see
			// this advisor even though its old-session provider call was still
			// running. Keep it discoverable until its WAL is settled.
			result := advisorReadyMsg{adv: adv, action: "off", operation: generation, sourceID: sourceID}
			if err := adv.PauseAndWait(ctx); err != nil {
				adv.Resume()
				result.err = err
			}
			return result
		})
	default:
		return noticeCmd("error", advisorUsage)
	}
}

// advisorTier resolves which model advises. The slots table wins, because
// "the advisor is a slot" is the configuration model this whole tool is
// built on; absent a binding, §9.2's default applies: one rung above the
// primary, or the top rung when the primary is already there.
func advisorTier(app *tuiApp) (config.Tier, error) {
	if ref, ok := app.config.Slots["advisor"]; ok {
		resolved, found := app.config.Tier(ref)
		if !found {
			target, err := config.ParseTarget(ref, "", "")
			if err != nil {
				return config.Tier{}, err
			}
			resolved = config.Tier{ID: "-advisor", Label: "advisor", Target: target}
		}
		if err := destinationAllowed(app.config, resolved.Target); err != nil {
			return config.Tier{}, fmt.Errorf("the advisor slot cannot run: %w", err)
		}
		return resolved, nil
	}

	// The default picks a rung off the ladder directly rather than through
	// the router, so the destination policy has to be applied here too.
	tiers := app.config.Tiers
	rank := app.rankOf(app.tier)
	chosen := app.tier
	switch {
	case rank < 0 || len(tiers) == 0:
	case rank+1 < len(tiers):
		chosen = tiers[rank+1]
	default:
		chosen = tiers[rank]
	}
	if err := destinationAllowed(app.config, chosen.Target); err != nil {
		return config.Tier{}, fmt.Errorf("the advisor has no approved rung: %w", err)
	}
	return chosen, nil
}

func describeAdvisorChoice(app *tuiApp) string {
	t, err := advisorTier(app)
	if err != nil {
		return "the [slots] advisor entry, which does not parse: " + err.Error()
	}
	return t.Target.Display()
}

func startAdvisor(ctx context.Context, app *tuiApp, operation uint64, sourceID string) tea.Cmd {
	return startAdvisorAttempt(ctx, app, operation, sourceID, 0)
}

func startAdvisorAttempt(ctx context.Context, app *tuiApp, operation uint64, sourceID string, retries int) tea.Cmd {
	tier, err := advisorTier(app)
	if err != nil {
		return func() tea.Msg {
			return advisorReadyMsg{action: "on", err: err, operation: operation, sourceID: sourceID}
		}
	}
	sess := app.loop.Session
	return func() tea.Msg {
		probed, client, err := app.providers.probeTier(ctx, tier)
		if err != nil {
			return advisorReadyMsg{action: "on", err: err, operation: operation, sourceID: sourceID}
		}
		adv := advisor.New(app.watcher, client, probed.Target, func(text string) {
			app.p.Send(adviceMsg{text: text})
		}, advisor.WithMeter(advisorMeterFor(app, sess, probed.Target)))
		return advisorReadyMsg{adv: adv, tier: probed, client: client, providerRetries: retries,
			action: "on", operation: operation, sourceID: sourceID}
	}
}

func advisorMeterFor(app *tuiApp, sess *session.Session, target provider.RouteTarget) advisor.Meter {
	return func(req provider.Request) (advisor.AttemptFinish, error) {
		finish, err := beginMeteredCall(app.budget, app.catalog, sess, target, req, session.UsagePurposeAdvisor)
		if err != nil {
			return nil, err
		}
		return advisor.AttemptFinish(finish), nil
	}
}

// pauseAdvisorLedger is the session-transition barrier for the advisor's
// asynchronous provider call. It both prevents a new call and waits until an
// admitted call has durably settled before a fork takes its ledger snapshot.
// The returned release is idempotent so every error path can safely defer it.
func pauseAdvisorLedger(ctx context.Context, app *tuiApp) (func(), error) {
	adv := app.currentAdvisor()
	if adv == nil {
		return func() {}, nil
	}
	if err := adv.PauseAndWait(ctx); err != nil {
		adv.Resume()
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(adv.Resume) }, nil
}

func (m *tuiModel) onAdvisorReady(msg advisorReadyMsg) tea.Cmd {
	if !m.operationMatches(msg.operation, msg.sourceID) {
		if msg.action == "off" && msg.adv != nil {
			msg.adv.Resume()
		}
		return nil
	}
	cancelled := m.operationCancelling || errors.Is(msg.err, context.Canceled)
	if cancelled {
		if msg.action == "off" && msg.adv != nil {
			msg.adv.Resume()
		}
		m.finishOperation(msg.operation, false)
		m.addNotice("", "advisor "+msg.action+" cancelled")
		return m.nextQueuedTurn()
	}
	if msg.err != nil {
		m.finishOperation(msg.operation, false)
		m.addNotice("error", "advisor could not "+map[string]string{"on": "start", "off": "stop cleanly"}[msg.action]+": "+msg.err.Error())
		return m.nextQueuedTurn()
	}
	if msg.action == "off" {
		if m.app.currentAdvisor() == msg.adv {
			m.app.setAdvisor(nil)
			m.app.loop.SetObserver(m.app.watcher)
		} else if msg.adv != nil {
			msg.adv.Resume()
		}
		m.finishOperation(msg.operation, false)
		m.addNotice("", "advisor is off")
		return m.nextQueuedTurn()
	}
	if m.app.providers != nil && !m.app.providers.preparedClientCurrent(msg.client) {
		if msg.providerRetries >= maxProviderReplans {
			m.finishOperation(msg.operation, false)
			m.addNotice("error", "provider settings kept changing while starting the advisor; advisor remains off")
			return m.nextQueuedTurn()
		}
		return m.ownOperationCmd(msg.operation,
			startAdvisorAttempt(m.turnCtx, m.app, msg.operation, msg.sourceID, msg.providerRetries+1))
	}
	// The probe may have completed after a session/tier bind rebuilt the
	// watcher. Always wrap the graph current at installation time, not the one
	// that happened to exist when the async probe started.
	msg.adv.SetInner(m.app.watcher)
	msg.adv.SetMeter(advisorMeterFor(m.app, m.app.loop.Session, msg.adv.Target()))
	m.app.setAdvisor(msg.adv)
	m.app.loop.SetObserver(msg.adv)
	m.addNotice("advisor", "watching this session with "+msg.adv.Target().Display()+
		"; advice appears here and is passed to the model")
	m.finishOperation(msg.operation, false)
	return m.nextQueuedTurn()
}

// adviceContext folds advice that arrived after a turn ended into the next
// prompt, the same seam the ! output uses and for the same reason: one user
// message per turn.
func (m *tuiModel) adviceContext(prompt string) string {
	advisor := m.app.currentAdvisor()
	if advisor == nil {
		return prompt
	}
	msgs := advisor.Drain()
	if len(msgs) == 0 {
		return prompt
	}
	var b strings.Builder
	for _, msg := range msgs {
		for _, block := range msg.Content {
			if t, ok := block.(provider.Text); ok {
				b.WriteString(t.Text + "\n\n")
			}
		}
	}
	b.WriteString(prompt)
	return b.String()
}
