package agent

import (
	"reflect"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/breakpoint"
	"github.com/switchboard-code/switchboard/internal/cachestate"
	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/provider"
)

func explicitPolicy() catalog.CachePolicy {
	return catalog.CachePolicy{
		DefaultMode: catalog.CacheExplicit, MinTokens: 0, MaxBreakpoints: 4,
		TTLs: []string{"5m"}, UsageAccounting: catalog.AccountingSeparate,
	}
}

func longMessages(n int) []provider.Message {
	out := make([]provider.Message, 0, n)
	for range n {
		out = append(out, provider.UserText("a settled turn with some content in it"))
	}
	return append(out, provider.UserText("the question being asked now"))
}

// A nil Cache is the cache-unaware control arm §7.1 compares against, and it
// must not panic or place anything.
func TestANilCacheIsTheControlArm(t *testing.T) {
	var c *Cache
	if plan := c.plan(nil, nil, longMessages(2)); plan != nil {
		t.Errorf("a nil cache produced a plan: %+v", plan)
	}
	c.observe(provider.Usage{CacheReadTokens: 10}, time.Now())
}

// The last message is the turn being asked about, and keeping it out of the
// marked prefix is the whole reason a marker survives to the next turn.
func TestTheCurrentMessageIsNotMarked(t *testing.T) {
	c := &Cache{
		Manager: &breakpoint.Manager{Policy: explicitPolicy(), Target: "t/s/m"},
		Policy:  explicitPolicy(),
		Target:  "t/s/m",
	}
	messages := longMessages(3)
	plan := c.plan([]provider.Block{provider.Text{Text: "system"}}, nil, messages)

	if plan == nil || len(plan.Breakpoints) == 0 {
		t.Fatal("no markers were placed on an explicit target")
	}
	last := len(messages) - 1
	for _, bp := range plan.Breakpoints {
		if bp.Position.MessageIndex == last {
			t.Error("a marker landed on the message being asked about, which is rewritten every turn")
		}
	}
}

// §6.3: the tracker is updated from the response, not from the request. Having
// placed a marker is not evidence anything was cached.
func TestPlacingAMarkerIsNotAnObservation(t *testing.T) {
	tracker := cachestate.New()
	c := &Cache{
		Manager: &breakpoint.Manager{Policy: explicitPolicy(), Target: "t/s/m"},
		Tracker: tracker,
		Policy:  explicitPolicy(),
		Target:  "t/s/m",
	}

	c.plan([]provider.Block{provider.Text{Text: "system"}}, nil, longMessages(3))
	if got := tracker.Health("t/s/m"); got.EligibleRequests != 0 {
		t.Errorf("planning alone recorded %d requests", got.EligibleRequests)
	}

	c.observe(provider.Usage{CacheWriteTokens: 5000}, time.Now())
	c.observe(provider.Usage{CacheReadTokens: 5000}, time.Now())

	got := tracker.Health("t/s/m")
	if got.EligibleRequests != 2 || got.Hits != 1 {
		t.Errorf("health = %+v, want two eligible requests and one hit", got)
	}
}

// The same prefix has to hash the same across turns, or every request looks new
// and the tracker never expects a hit.
func TestTheSamePrefixIsRecognisedAcrossTurns(t *testing.T) {
	tracker := cachestate.New()
	c := &Cache{
		Manager: &breakpoint.Manager{Policy: explicitPolicy(), Target: "t/s/m"},
		Tracker: tracker,
		Policy:  explicitPolicy(),
		Target:  "t/s/m",
	}
	system := []provider.Block{provider.Text{Text: "system"}}
	history := longMessages(3)

	c.plan(system, nil, history)
	first := c.lastHash
	c.observe(provider.Usage{CacheWriteTokens: 5000}, time.Now())

	// The same settled history with a different current message: the prefix is
	// unchanged, so the hash must be too.
	changedTail := append(append([]provider.Message(nil), history[:len(history)-1]...),
		provider.UserText("a different question"))
	c.plan(system, nil, changedTail)

	if c.lastHash != first {
		t.Error("changing only the current message changed the prefix hash, so no turn would ever hit")
	}
	if got := tracker.Expect("t/s/m", c.lastHash, time.Now()); got.HitProbability == 0 {
		t.Errorf("the tracker expects nothing from a prefix it just watched being written: %+v", got)
	}
}

func TestIncompleteAssistantDoesNotEnterTheCachePrefix(t *testing.T) {
	system := []provider.Block{provider.Text{Text: "system"}}
	baseline := []provider.Message{
		provider.UserText("settled request"),
		provider.UserText("current request"),
	}
	withPartial := []provider.Message{
		baseline[0],
		{
			Role:       provider.RoleAssistant,
			Incomplete: true,
			Content:    []provider.Block{provider.Text{Text: "partial must not be cached"}},
		},
		baseline[1],
	}

	newCache := func() *Cache {
		return &Cache{
			Manager: &breakpoint.Manager{Policy: explicitPolicy(), Target: "t/s/m"},
			Policy:  explicitPolicy(),
			Target:  "t/s/m",
		}
	}
	baseCache := newCache()
	basePlan := baseCache.plan(system, nil, baseline)
	partialCache := newCache()
	partialPlan := partialCache.plan(system, nil, withPartial)

	if baseCache.lastHash != partialCache.lastHash {
		t.Fatalf("incomplete assistant changed cache identity: %q != %q", baseCache.lastHash, partialCache.lastHash)
	}
	if !reflect.DeepEqual(basePlan, partialPlan) {
		t.Fatalf("incomplete assistant shifted cache markers:\nbase: %+v\nwith partial: %+v", basePlan, partialPlan)
	}
	if len(withPartial) != 3 || !withPartial[1].Incomplete {
		t.Fatal("cache planning mutated the durable history")
	}
}

// A target that does not cache gets no plan, and the reason is reported rather
// than left as silence.
func TestATargetThatDoesNotCacheGetsNoPlan(t *testing.T) {
	var events []CacheEvent
	c := &Cache{
		Manager: &breakpoint.Manager{
			Policy: catalog.CachePolicy{DefaultMode: catalog.CacheNone}, Target: "t/s/m"},
		Policy:   catalog.CachePolicy{DefaultMode: catalog.CacheNone},
		Target:   "t/s/m",
		Observer: func(e CacheEvent) { events = append(events, e) },
	}

	if plan := c.plan(nil, nil, longMessages(2)); plan != nil {
		t.Errorf("a plan was produced for a target with no cache: %+v", plan)
	}
	if len(events) == 0 || len(events[0].Declined) == 0 {
		t.Error("nothing was reported, so a permanent miss would look like a bug")
	}
}

func TestAutomaticCacheRoutingKeyReachesTheProviderPlan(t *testing.T) {
	policy := catalog.CachePolicy{DefaultMode: catalog.CacheAutomatic, RoutingKeySupport: true}
	c := &Cache{
		Manager: &breakpoint.Manager{Policy: policy, Target: "openai/subscription/model"},
		Policy:  policy, Target: "openai/subscription/model",
	}
	plan := c.plan([]provider.Block{provider.Text{Text: "system"}}, nil, longMessages(2))
	if plan == nil || plan.RoutingKey == "" {
		t.Fatalf("automatic cache plan = %+v, want a routing key", plan)
	}
	if len(plan.Breakpoints) != 0 {
		t.Fatalf("automatic cache placed explicit markers: %+v", plan.Breakpoints)
	}
}
