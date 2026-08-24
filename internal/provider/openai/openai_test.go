package openai

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/provider/openaicompat"
)

func testClient(t *testing.T) *openaicompat.Client {
	t.Helper()
	client, err := New(FirstParty)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// OpenAI is its own provider, not a profile of the compatible adapter. Sharing
// the decoder is an implementation detail; sharing the identity would file
// OpenAI's price sheet and OpenAI's credential under a name that means "some
// server speaking this format".
func TestIdentityIsNotTheCompatibleAdapter(t *testing.T) {
	target := Target("gpt-5-mini")

	if target.Provider != "openai" || target.Surface != "first-party" {
		t.Errorf("target = %+v", target)
	}
	if strings.Contains(string(target.ID()), openaicompat.Name) {
		t.Errorf("target id %s files OpenAI under the compatible adapter", target.ID())
	}

	// The credential is looked up by provider and surface, so the identity
	// above is what decides which key pays for the request.
	if got := testClient(t).Name(); got != Name {
		t.Errorf("client reports provider %q, so its errors and its credential would be attributed to the wrong vendor", got)
	}
}

// Nothing in this package has been run against the live API. Until it has,
// asking for reasoning has to be a capability error the caller sees rather than
// a parameter quietly dropped (§5.2), because a silently ignored reasoning
// request produces a cheaper, worse answer that looks like a correct one.
func TestUntestedCapabilitiesAreRefusedNotDropped(t *testing.T) {
	target := Target("gpt-5-mini")
	target.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: "high"}

	_, err := testClient(t).Stream(context.Background(), target, provider.Request{
		Messages: []provider.Message{provider.UserText("hello")},
	})

	var capErr *provider.CapabilityError
	if !errors.As(err, &capErr) {
		t.Fatalf("err = %v, want a CapabilityError while the profile is unverified", err)
	}
}

// A cache plan cannot be rendered into this format at all, so a non-nil plan is
// an error rather than a request sent without the markers the caller asked for.
func TestCachePlanIsRefused(t *testing.T) {
	_, err := testClient(t).Stream(context.Background(), Target("gpt-5-mini"), provider.Request{
		Messages:  []provider.Message{provider.UserText("hello")},
		CachePlan: &provider.CachePlan{Breakpoints: []provider.Breakpoint{{}}},
	})

	var capErr *provider.CapabilityError
	if !errors.As(err, &capErr) {
		t.Fatalf("err = %v, want a CapabilityError", err)
	}
}

func TestUnknownSurfaceIsRefused(t *testing.T) {
	if client, err := New("firstparty"); err == nil || client != nil {
		t.Fatalf("unknown surface constructed client=%v err=%v", client, err)
	}
	if got := DefaultBaseURL("firstparty"); got != "" {
		t.Fatalf("unknown surface inherited base URL %q", got)
	}
	if client, err := New(Subscription); err == nil || client != nil {
		t.Fatalf("subscription constructed the chat-completions client=%v err=%v", client, err)
	}
}

// The developer API and the subscription backend are different endpoints, with
// different credentials and different billing. Collapsing them would attach one
// surface's price sheet and one surface's token to the other's traffic.
func TestTheTwoSurfacesAreDifferentTargets(t *testing.T) {
	api := Target("gpt-5")
	sub := SubscriptionTarget("gpt-5")

	if api.ID() == sub.ID() {
		t.Fatal("the same model on two surfaces produced one target id")
	}
	if DefaultBaseURL(FirstParty) == DefaultBaseURL(Subscription) {
		t.Error("both surfaces resolved to the same endpoint")
	}
}

// The subscription surface ships a login client so it works without
// configuration; the developer API takes a key and has no flow to offer.
func TestBundledOAuthOnlyAppliesToTheSubscriptionSurface(t *testing.T) {
	if got := DefaultOAuth(Subscription); got.ClientID == "" {
		t.Error("the subscription surface has no bundled client, so a login would need configuration")
	} else if got.AuthorizeURL == "" || got.TokenURL == "" {
		t.Errorf("the bundled client is incomplete: %+v", got)
	}
	if got := DefaultOAuth(FirstParty); got.ClientID != "" {
		t.Error("the developer API offered a login flow; it takes an API key")
	}
}
