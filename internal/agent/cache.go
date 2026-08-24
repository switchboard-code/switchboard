package agent

import (
	"time"

	"github.com/switchboard-code/switchboard/internal/breakpoint"
	"github.com/switchboard-code/switchboard/internal/cachestate"
	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/prefix"
	"github.com/switchboard-code/switchboard/internal/provider"
)

// Cache connects §6 to the loop.
//
// Everything under §6 was built and none of it was reachable: the loop assembled
// a request straight from the session, so the zones, the breakpoint manager, and
// the tracker never saw a real turn. That is the same failure the escalation
// policy had, and it has the same shape: a component with tests and no caller
// does nothing at all.
//
// It is optional. A nil Cache is a cache-unaware loop, which is exactly the
// control arm §7.1 asks for when it wants the interval against an otherwise
// identical cache-unaware router to exclude zero.
type Cache struct {
	Manager *breakpoint.Manager
	Tracker *cachestate.Tracker
	Policy  catalog.CachePolicy
	Target  provider.RouteTargetID

	// Observer receives what was decided and what came back, so a terminal can
	// show it. §6.6 wants this per turn rather than at the end.
	Observer func(CacheEvent)

	lastHash     string
	lastEligible bool
}

// CacheEvent is one turn's cache activity, in the terms §6.6 asks to report.
type CacheEvent struct {
	Placed     int
	Mode       catalog.CacheMode
	RoutingKey string
	Declined   []string

	Usage    provider.Usage
	Expected float64
	Alarm    string
}

// plan lays out the request and decides where markers go.
//
// The zone split is the loop's own shape: everything before the last message is
// settled, and the last message is the turn being asked about. That is the
// volatile tail §6.1 describes, and keeping it out of the marked prefix is the
// whole reason a marker survives to the next turn.
func (c *Cache) plan(system []provider.Block, tools []provider.ToolDefinition, messages []provider.Message) *provider.CachePlan {
	if c == nil || c.Manager == nil {
		return nil
	}
	messages = provider.ReplayRequest(provider.Request{Messages: messages}).Messages

	layout := cacheLayout(system, tools, messages)

	decision := c.Manager.Plan(layout)
	c.lastHash = layout.PrefixHash()
	c.lastEligible = decision.Placed() > 0 || decision.RoutingKey != ""

	if c.Observer != nil {
		c.Observer(CacheEvent{
			Placed:     decision.Placed(),
			Mode:       decision.Mode,
			RoutingKey: decision.RoutingKey,
			Declined:   decision.Declined,
		})
	}
	if decision.Plan == nil && decision.RoutingKey == "" {
		return nil
	}
	plan := &provider.CachePlan{RoutingKey: decision.RoutingKey}
	if decision.Plan != nil {
		plan.Breakpoints = append(plan.Breakpoints, decision.Plan.Breakpoints...)
	}
	return plan
}

func cacheLayout(system []provider.Block, tools []provider.ToolDefinition, messages []provider.Message) *prefix.Layout {
	layout := prefix.New(system, tools, 0)
	if len(messages) > 0 {
		layout.AppendHistory(messages[:len(messages)-1]...)
		layout.SetTail(messages[len(messages)-1].Content...)
	}
	return layout
}

// Predict reports the tracker's belief for a prospective request without
// mutating the cache controller. The router calls it for every target before
// selection; only the target that actually serves the call may update state.
func (c *Cache) Predict(system []provider.Block, tools []provider.ToolDefinition, messages []provider.Message, now time.Time) (cachestate.Expectation, bool) {
	if c == nil || c.Tracker == nil || c.Manager == nil {
		return cachestate.Expectation{}, false
	}
	messages = provider.ReplayRequest(provider.Request{Messages: messages}).Messages
	hash := cacheLayout(system, tools, messages).PrefixHash()
	if hash == "" {
		return cachestate.Expectation{}, false
	}
	return c.Tracker.Expect(c.Target, hash, now), true
}

// observe records what the provider reported.
//
// §6.3's rule is that entries are updated from response usage rather than from
// how the request was built, which is why this takes the usage and not the plan.
func (c *Cache) observe(usage provider.Usage, now time.Time) {
	if c == nil || c.Tracker == nil || c.lastHash == "" {
		return
	}

	c.Tracker.Observe(cachestate.Observation{
		Target:     c.Target,
		PrefixHash: c.lastHash,
		Usage:      usage,
		At:         now,
		Accounting: c.Policy.UsageAccounting,
		Eligible:   c.lastEligible,
		MinimumTTL: shortestTTL(c.Policy),
	})

	if c.Observer == nil {
		return
	}
	event := CacheEvent{Usage: usage}
	if health := c.Tracker.Health(c.Target); health.Alarm {
		event.Alarm = health.Detail
	}
	c.Observer(event)
}

// Expectation reports the tracker's belief about the prefix the last request
// sent, for a surface that wants to say it. ok is false before any turn has
// been planned or when the loop runs cache-unaware. Callers read this
// between turns: lastHash is the loop goroutine's to write during one.
func (c *Cache) Expectation(now time.Time) (cachestate.Expectation, bool) {
	if c == nil || c.Tracker == nil || c.lastHash == "" {
		return cachestate.Expectation{}, false
	}
	return c.Tracker.Expect(c.Target, c.lastHash, now), true
}

// SessionHealth reports the tracker's per-target summary, and Snapshot its
// entries, newest first. Both are nil-safe the way the loop's own calls are.
func (c *Cache) SessionHealth() (cachestate.Health, bool) {
	if c == nil || c.Tracker == nil {
		return cachestate.Health{}, false
	}
	return c.Tracker.Health(c.Target), true
}

func (c *Cache) Snapshot() []cachestate.Entry {
	if c == nil || c.Tracker == nil {
		return nil
	}
	return c.Tracker.Entries()
}

// shortestTTL is the retention the manager asks for, which is what the tracker
// measures expiry against. They have to agree, or the tracker believes in a
// window the request never bought.
func shortestTTL(policy catalog.CachePolicy) time.Duration {
	shortest := time.Duration(0)
	for _, raw := range policy.TTLs {
		d, err := time.ParseDuration(raw)
		if err != nil {
			continue
		}
		if shortest == 0 || d < shortest {
			shortest = d
		}
	}
	return shortest
}
