package provider

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

type fixedOutputAllower struct {
	Provider
	allowance int
	observed  *outputAllowanceCall
}

type outputAllowanceCall struct {
	target     RouteTarget
	catalogMax int
}

func (f fixedOutputAllower) OutputTokenAllowance(target RouteTarget, catalogMax int) int {
	if f.observed != nil {
		*f.observed = outputAllowanceCall{target: target, catalogMax: catalogMax}
	}
	return f.allowance
}

type fixedOutputResolver struct {
	fixedOutputAllower
	resolved int
	err      error
}

func (f fixedOutputResolver) ResolveOutputTokenAllowance(RouteTarget, int) (int, error) {
	return f.resolved, f.err
}

func TestRouteTargetIDSeparatesExplicitInferenceParameters(t *testing.T) {
	base := RouteTarget{Provider: "openai", Surface: "api", ModelID: "gpt-test"}
	if got, want := base.ID(), RouteTargetID("openai/api/gpt-test"); got != want {
		t.Fatalf("default target ID = %q, want legacy ID %q", got, want)
	}

	withMax := base
	withMax.Params.MaxOutputTokens = 2_048
	withOtherMax := base
	withOtherMax.Params.MaxOutputTokens = 4_096
	if withMax.ID() == base.ID() || withMax.ID() == withOtherMax.ID() {
		t.Fatalf("explicit output limits collided: default=%q 2048=%q 4096=%q",
			base.ID(), withMax.ID(), withOtherMax.ID())
	}
	if got, want := withMax.ID(), RouteTargetID("rt2:p=b3BlbmFp&s=YXBp&m=Z3B0LXRlc3Q&max=2048&temp=n&think=n&effort="); got != want {
		t.Fatalf("explicit output ID = %q, want canonical ID %q", got, want)
	}

	zero := 0.0
	half := 0.5
	withZeroTemperature := base
	withZeroTemperature.Params.Temperature = &zero
	withHalfTemperature := base
	withHalfTemperature.Params.Temperature = &half
	if withZeroTemperature.ID() == base.ID() || withZeroTemperature.ID() == withHalfTemperature.ID() {
		t.Fatalf("explicit temperatures collided: default=%q zero=%q half=%q",
			base.ID(), withZeroTemperature.ID(), withHalfTemperature.ID())
	}
	if !strings.HasPrefix(string(withZeroTemperature.ID()), "rt2:") {
		t.Fatalf("explicit zero-temperature ID = %q, want versioned identity", withZeroTemperature.ID())
	}

	combined := withMax
	combined.Params.Temperature = &half
	if first, second := combined.ID(), combined.ID(); first != second {
		t.Fatalf("combined parameter ID is unstable: %q then %q", first, second)
	}
}

func TestRouteTargetIDEscapesSuffixLookingModelNames(t *testing.T) {
	base := RouteTarget{Provider: "openai", Surface: "api", ModelID: "gpt-test"}
	temperature := 0.5
	parameterized := []RouteTarget{base, base, base}
	parameterized[0].Params.Reasoning = &Reasoning{Enabled: true, Effort: "high"}
	parameterized[1].Params.MaxOutputTokens = 2_048
	parameterized[2].Params.Temperature = &temperature
	literals := []RouteTarget{base, base, base}
	literals[0].ModelID += "+think:high"
	literals[1].ModelID += "+max:2048"
	literals[2].ModelID += "+temp:0.5"
	for index := range parameterized {
		if parameterized[index].ID() == literals[index].ID() {
			t.Fatalf("parameterized target %q collided with literal model %q", parameterized[index].ID(), literals[index].ModelID)
		}
		parsed, err := ParseRouteTargetID(literals[index].ID())
		if err != nil || !reflect.DeepEqual(parsed, literals[index]) {
			t.Fatalf("literal suffix-looking model %q did not round trip: parsed=%#v err=%v", literals[index].ModelID, parsed, err)
		}
	}
	if got, want := literals[1].ID(), RouteTargetID("openai/api/gpt-test+max:2048"); got != want {
		t.Fatalf("default literal model ID = %q, want legacy-compatible %q", got, want)
	}
	percent := base
	percent.ModelID = "gpt%2Bpreview"
	plus := base
	plus.ModelID = "gpt+preview"
	if percent.ID() == plus.ID() {
		t.Fatalf("literal percent sequence %q collided with plus %q", percent.ID(), plus.ID())
	}
}

func TestRouteTargetIDRoundTripsEveryExplicitParameter(t *testing.T) {
	zero := 0.0
	target := RouteTarget{
		Provider: "custom/provider%+",
		Surface:  "surface/name%+",
		ModelID:  "vendor/model+preview%2B",
		Params: Params{
			MaxOutputTokens: -2_048,
			Temperature:     &zero,
			Reasoning:       &Reasoning{Enabled: false, Effort: "off+max:7%"},
		},
	}
	parsed, err := ParseRouteTargetID(target.ID())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed, target) {
		t.Fatalf("route target round trip\n got: %#v\nwant: %#v\n  id: %s", parsed, target, target.ID())
	}
	if target.ID() == (RouteTarget{Provider: target.Provider, Surface: target.Surface, ModelID: target.ModelID}).ID() {
		t.Fatal("explicit disabled reasoning, max output, and zero temperature collapsed into defaults")
	}
}

func TestRouteTargetDisplayIsReadableAndUnambiguous(t *testing.T) {
	temperature := 0.5
	base := RouteTarget{Provider: "openai", Surface: "api", ModelID: "gpt-test"}
	parameterized := base
	parameterized.Params = Params{
		MaxOutputTokens: 2_048,
		Temperature:     &temperature,
		Reasoning:       &Reasoning{Enabled: true, Effort: "high"},
	}
	display := parameterized.Display()
	for _, readable := range []string{"openai/api/gpt-test", "think:high", "max:2048", "temp:0.5"} {
		if !strings.Contains(display, readable) {
			t.Errorf("Display() = %q, missing %q", display, readable)
		}
	}
	for _, opaque := range []string{"rt2:", "b3BlbmFp", "Z3B0LXRlc3Q"} {
		if strings.Contains(display, opaque) {
			t.Errorf("Display() = %q, leaked machine identity %q", display, opaque)
		}
	}
	if got := DisplayRouteTargetID(parameterized.ID()); got != display {
		t.Fatalf("DisplayRouteTargetID() = %q, want %q", got, display)
	}

	literalThink := base
	literalThink.ModelID += "+think:high"
	withThink := base
	withThink.Params.Reasoning = &Reasoning{Enabled: true, Effort: "high"}
	literalMax := base
	literalMax.ModelID += "+max:2048"
	withMax := base
	withMax.Params.MaxOutputTokens = 2_048
	literalTemp := base
	literalTemp.ModelID += "+temp:0.5"
	withTemp := base
	withTemp.Params.Temperature = &temperature
	for _, pair := range [][2]RouteTarget{
		{literalThink, withThink},
		{literalMax, withMax},
		{literalTemp, withTemp},
	} {
		if pair[0].Display() == pair[1].Display() {
			t.Errorf("distinct targets share display %q: %#v and %#v", pair[0].Display(), pair[0], pair[1])
		}
	}

	enabledFalse := base
	enabledFalse.Params.Reasoning = &Reasoning{Enabled: true, Effort: "false"}
	disabled := base
	disabled.Params.Reasoning = &Reasoning{Enabled: false}
	if enabledFalse.Display() == disabled.Display() || !strings.Contains(disabled.Display(), "think-off") {
		t.Fatalf("enabled effort=false and explicit disabled reasoning are ambiguous: %q / %q",
			enabledFalse.Display(), disabled.Display())
	}
	effortWithDelimiter := base
	effortWithDelimiter.Params.Reasoning = &Reasoning{Enabled: true, Effort: "high+max:2048"}
	effortAndMax := base
	effortAndMax.Params.Reasoning = &Reasoning{Enabled: true, Effort: "high"}
	effortAndMax.Params.MaxOutputTokens = 2_048
	if effortWithDelimiter.Display() == effortAndMax.Display() || !strings.Contains(effortWithDelimiter.Display(), "%2Bmax%3A2048") {
		t.Fatalf("reasoning effort delimiter is ambiguous: %q / %q",
			effortWithDelimiter.Display(), effortAndMax.Display())
	}
}

func TestEffectiveOutputTokenAllowanceUsesExplicitThenCatalogLimit(t *testing.T) {
	target := RouteTarget{Provider: "generic"}
	if got := EffectiveOutputTokenAllowance(nil, target, 64_000); got != 64_000 {
		t.Fatalf("catalog allowance = %d, want 64000", got)
	}
	target.Params.MaxOutputTokens = 1_234
	if got := EffectiveOutputTokenAllowance(nil, target, 64_000); got != 1_234 {
		t.Fatalf("explicit reserve = %d, want 1234", got)
	}
}

func TestEffectiveOutputTokenAllowanceFailsClosedWithoutADefault(t *testing.T) {
	target := RouteTarget{Provider: "openaicompat"}
	if got := EffectiveOutputTokenAllowance(nil, target, 0); got != math.MaxInt {
		t.Fatalf("unknown server default reserve = %d, want MaxInt", got)
	}
	target.Params.MaxOutputTokens = 2_048
	if got := EffectiveOutputTokenAllowance(nil, target, 0); got != 2_048 {
		t.Fatalf("explicit compatible reserve = %d, want 2048", got)
	}
}

func TestEffectiveOutputTokenAllowanceGivesBoundAdapterFirstSay(t *testing.T) {
	target := RouteTarget{Provider: "wire-specific"}
	target.Params.MaxOutputTokens = 1_234
	var observed outputAllowanceCall
	bound := fixedOutputAllower{allowance: 7_777, observed: &observed}
	if got := EffectiveOutputTokenAllowance(bound, target, 64_000); got != 7_777 {
		t.Fatalf("bound adapter allowance = %d, want 7777 ahead of explicit and catalog values", got)
	}
	if !reflect.DeepEqual(observed.target, target) || observed.catalogMax != 64_000 {
		t.Fatalf("adapter saw target=%+v catalogMax=%d, want target=%+v catalogMax=64000",
			observed.target, observed.catalogMax, target)
	}
	target.Params.MaxOutputTokens = 0
	if got := EffectiveOutputTokenAllowance(bound, target, 0); got != 7_777 {
		t.Fatalf("bound adapter allowance = %d, want 7777 when generic evidence is unknown", got)
	}
}

func TestOutputAllowanceResolverPreservesTypedErrorAtPreSendBoundary(t *testing.T) {
	wantErr := errors.New("invalid wire parameters")
	bound := fixedOutputResolver{
		fixedOutputAllower: fixedOutputAllower{allowance: 7_777},
		resolved:           8_888,
		err:                wantErr,
	}
	target := RouteTarget{Provider: "wire-specific", Params: Params{MaxOutputTokens: 1_234}}
	if got := EffectiveOutputTokenAllowance(bound, target, 64_000); got != math.MaxInt {
		t.Fatalf("ranking allowance = %d, want unknown sentinel on typed conflict", got)
	}
	if _, err := ResolveOutputTokenAllowance(bound, target, 64_000); !errors.Is(err, wantErr) {
		t.Fatalf("pre-send resolver error = %v, want %v", err, wantErr)
	}

	bound.err = nil
	if got, err := ResolveOutputTokenAllowance(bound, target, 64_000); err != nil || got != 8_888 {
		t.Fatalf("resolved allowance = %d, %v, want 8888 with resolver ahead of scalar allower", got, err)
	}
}
