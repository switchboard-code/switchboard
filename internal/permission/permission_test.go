package permission

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/execution"
)

var (
	// A host where the mechanism exists but no profile has been demonstrated.
	noSandbox = execution.Capability{
		Platform:         "darwin",
		Mechanism:        execution.MechanismSeatbelt,
		MechanismPresent: true,
	}
	verifiedSandbox = reviewerCapableSandbox()
)

// reviewerCapableSandbox is a platform-stable fixture for the generic policy
// tests. TestingVerifiedCapability deliberately reports the build host, but a
// real Windows host keeps auto execution with the human until descendant
// process cleanup is enforceable. Tests of that boundary use an explicit
// Windows capability below; the rest need to exercise the reviewer path on
// every CI operating system.
func reviewerCapableSandbox() execution.Capability {
	capability := execution.TestingVerifiedCapability()
	capability.Platform = "linux"
	return capability
}

func read() Request  { return Request{Tool: "read", Effect: EffectRead, Path: "main.go"} }
func write() Request { return Request{Tool: "edit", Effect: EffectWrite, Path: "main.go"} }
func exec() Request {
	return Request{Tool: "exec", Effect: EffectExecute, Argv: []string{"go", "test", "./..."}}
}

func TestModeDefaults(t *testing.T) {
	cases := []struct {
		mode              Mode
		read, write, exec Decision
	}{
		{ModePlan, Allow, Deny, Deny},
		{ModeDefault, Allow, Ask, Ask},
		{ModeAcceptEdits, Allow, Allow, Ask},
		{ModeAuto, Allow, Allow, Ask},
		{ModeYOLO, Allow, Allow, Allow},
		// bypass would allow execution, but there is no sandbox to bypass into.
		{ModeBypass, Allow, Allow, Ask},
	}

	for _, tc := range cases {
		t.Run(string(tc.mode), func(t *testing.T) {
			e := NewEngine(tc.mode, noSandbox)
			if got := e.Check(read()).Decision; got != tc.read {
				t.Errorf("read = %s, want %s", got, tc.read)
			}
			if got := e.Check(write()).Decision; got != tc.write {
				t.Errorf("write = %s, want %s", got, tc.write)
			}
			if got := e.Check(exec()).Decision; got != tc.exec {
				t.Errorf("exec = %s, want %s", got, tc.exec)
			}
		})
	}
}

// The whole point of design principle 4: without verified containment, no mode
// grants automatic execution, and the reason has to say why so the UI cannot
// render the prompt as if it were a sandbox.
func TestBypassDoesNotGrantExecutionWithoutASandbox(t *testing.T) {
	e := NewEngine(ModeBypass, noSandbox)

	out := e.Check(exec())
	if out.Decision != Ask {
		t.Fatalf("decision = %s, want ask", out.Decision)
	}
	if !out.SandboxAbsent {
		t.Error("the outcome must mark that this prompt stands in for missing isolation")
	}
	if out.Reason == "" {
		t.Error("a downgraded decision needs a reason the user can read")
	}

	verified := NewEngine(ModeBypass, verifiedSandbox)
	if got := verified.Check(exec()); got.Decision != Allow {
		t.Errorf("with a verified sandbox, bypass should allow execution, got %s", got.Decision)
	} else if got.SandboxAbsent {
		t.Error("SandboxAbsent must be false when containment is verified")
	}
}

func TestExplicitAllowRuleCanGrantHostDirectExecution(t *testing.T) {
	e := NewEngine(ModeDefault, noSandbox, Rule{
		Decision:   Allow,
		Tool:       "exec",
		ArgvPrefix: []string{"go", "test"},
	})

	out := e.Check(exec())
	if out.Decision != Allow || !out.SandboxAbsent || !out.FullReach {
		t.Errorf("an explicit rule should grant host-direct execution and say so, got %+v", out)
	}

	verified := NewEngine(ModeDefault, verifiedSandbox, Rule{
		Decision:   Allow,
		Tool:       "exec",
		ArgvPrefix: []string{"go", "test"},
	})
	if got := verified.Check(exec()).Decision; got != Allow {
		t.Errorf("with containment the same rule should allow, got %s", got)
	}
}

// A sandbox confines what a command reads and writes. It cannot judge whether
// sending this workspace to the internet is what the user meant, so egress is
// approved separately even on a verified host (§11).
func TestNetworkAccessIsApprovedSeparately(t *testing.T) {
	e := NewEngine(ModeBypass, verifiedSandbox)

	offline := exec()
	if got := e.Check(offline).Decision; got != Allow {
		t.Fatalf("a confined offline command = %s, want allow", got)
	}

	online := offline
	online.Network = true
	out := e.Check(online)
	if out.Decision != Ask {
		t.Errorf("a command asking for egress = %s, want ask", out.Decision)
	}
	if !strings.Contains(out.Reason, "network") {
		t.Errorf("the reason must name what is being granted, got %q", out.Reason)
	}

	// The two forms are different requests, so approving the offline one must
	// not silently approve the networked one.
	e.Remember(offline, true)
	if got := e.Check(online).Decision; got != Ask {
		t.Errorf("an offline approval was reused for a networked command: %s", got)
	}
}

func TestDenyRuleWinsOverEverything(t *testing.T) {
	e := NewEngine(ModeBypass, verifiedSandbox,
		Rule{Decision: Allow, Tool: "exec"},
		Rule{Decision: Deny, Tool: "exec", ArgvPrefix: []string{"rm"}},
	)

	rm := Request{Tool: "exec", Effect: EffectExecute, Argv: []string{"rm", "-rf", "build"}}
	if got := e.Check(rm).Decision; got != Deny {
		t.Errorf("decision = %s, want deny even in bypass with a matching allow rule", got)
	}
	if got := e.Check(exec()).Decision; got != Allow {
		t.Errorf("the deny rule should not have caught an unrelated command, got %s", got)
	}
}

func TestPlanModeCannotBeWidenedByARule(t *testing.T) {
	e := NewEngine(ModePlan, verifiedSandbox,
		Rule{Decision: Allow, Tool: "exec"},
		Rule{Decision: Allow, Tool: "edit"},
	)
	if got := e.Check(exec()).Decision; got != Deny {
		t.Errorf("exec in plan mode = %s, want deny", got)
	}
	if got := e.Check(write()).Decision; got != Deny {
		t.Errorf("write in plan mode = %s, want deny", got)
	}
}

func TestRuleMatching(t *testing.T) {
	shellOnly := true
	e := NewEngine(ModeDefault, verifiedSandbox,
		Rule{Decision: Deny, Effect: EffectWrite, PathGlob: "*.lock"},
		Rule{Decision: Deny, Tool: "exec", Shell: &shellOnly},
	)

	if got := e.Check(Request{Tool: "write", Effect: EffectWrite, Path: "go.lock"}).Decision; got != Deny {
		t.Errorf("path glob did not match: %s", got)
	}
	if got := e.Check(Request{Tool: "write", Effect: EffectWrite, Path: "go.mod"}).Decision; got != Ask {
		t.Errorf("path glob matched too much: %s", got)
	}
	if got := e.Check(Request{Tool: "exec", Effect: EffectExecute, Shell: true, Argv: []string{"ls | wc"}}).Decision; got != Deny {
		t.Errorf("shell-mode rule did not match: %s", got)
	}
	// An argv command falls through to the default mode's answer, which is to
	// ask. What matters is that the shell-only deny rule did not catch it.
	if got := e.Check(exec()).Decision; got != Ask {
		t.Errorf("shell-mode rule wrongly caught an argv command: %s", got)
	}
}

func TestRememberIsExactMatchOnly(t *testing.T) {
	e := NewEngine(ModeDefault, noSandbox)

	approved := exec()
	e.Remember(approved, true)

	out := e.Check(approved)
	if out.Decision != Allow {
		t.Errorf("the remembered command = %s, want allow", out.Decision)
	}
	// The approval stands, but the command still runs uncontained and the
	// outcome must keep saying so.
	if !out.SandboxAbsent {
		t.Error("a remembered approval must not erase the fact that there is no sandbox")
	}

	// A different command must not inherit the approval, or the user has
	// approved something they never saw.
	other := Request{Tool: "exec", Effect: EffectExecute, Argv: []string{"go", "test", "./...", "-run", "TestDelete"}}
	if got := e.Check(other).Decision; got != Ask {
		t.Errorf("a longer argv reused the approval: %s", got)
	}

	shellForm := approved
	shellForm.Shell = true
	if got := e.Check(shellForm).Decision; got != Ask {
		t.Errorf("shell mode reused an argv-mode approval: %s", got)
	}
}

func TestRememberedExecutionApprovalIsScopedToEffectiveReach(t *testing.T) {
	controller, err := execution.NewController(verifiedSandbox, execution.SandboxOn)
	if err != nil {
		t.Fatal(err)
	}
	engine := NewEngineWithExecution(ModeDefault, controller)
	confined := exec()
	confinedPolicy := controller.CommandPolicy(false)
	confined.Execution = &confinedPolicy
	engine.Remember(confined, true)
	if got := engine.Check(confined); got.Decision != Allow {
		t.Fatalf("same confined request = %+v", got)
	}

	if err := controller.SetSandbox(execution.SandboxOff); err != nil {
		t.Fatal(err)
	}
	hostDirect := exec()
	hostPolicy := controller.CommandPolicy(false)
	hostDirect.Execution = &hostPolicy
	if got := engine.Check(hostDirect); got.Decision != Ask || !got.FullReach {
		t.Fatalf("confined approval widened to host reach: %+v", got)
	}
}

func TestRememberedDenialSticks(t *testing.T) {
	e := NewEngine(ModeDefault, noSandbox)
	e.Remember(write(), false)
	if got := e.Check(write()).Decision; got != Deny {
		t.Errorf("decision = %s, want deny after the user declined", got)
	}
}

type stubAsker struct {
	resp  Response
	err   error
	calls int
	last  Outcome
}

func (s *stubAsker) Ask(_ context.Context, _ Request, out Outcome) (Response, error) {
	s.calls++
	s.last = out
	return s.resp, s.err
}

func TestResolveConsultsTheAsker(t *testing.T) {
	e := NewEngine(ModeDefault, noSandbox)
	asker := &stubAsker{resp: Response{Approved: true, Remember: true}}

	ok, _, err := e.Resolve(context.Background(), asker, exec())
	if err != nil || !ok {
		t.Fatalf("ok = %v, err = %v", ok, err)
	}
	if asker.calls != 1 {
		t.Fatalf("asker called %d times, want 1", asker.calls)
	}
	if !asker.last.SandboxAbsent {
		t.Error("the asker must be told the prompt stands in for missing isolation")
	}

	// Remembering means the second identical call does not prompt again.
	ok, _, err = e.Resolve(context.Background(), asker, exec())
	if err != nil || !ok {
		t.Fatalf("second call: ok = %v, err = %v", ok, err)
	}
	if asker.calls != 1 {
		t.Errorf("asker called %d times, want the remembered answer to be reused", asker.calls)
	}
}

func TestResolveWithoutAnAskerDenies(t *testing.T) {
	e := NewEngine(ModeDefault, noSandbox)
	ok, out, err := e.Resolve(context.Background(), nil, exec())
	if err != nil {
		t.Fatal(err)
	}
	if ok || out.Decision != Deny {
		t.Errorf("a request that cannot be asked about must fail closed, got ok=%v %+v", ok, out)
	}
}

func TestResolvePropagatesAskerError(t *testing.T) {
	want := errors.New("terminal closed")
	e := NewEngine(ModeDefault, noSandbox)
	ok, _, err := e.Resolve(context.Background(), &stubAsker{err: want}, exec())
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
	if ok {
		t.Error("a failed prompt must not approve the call")
	}
}

func TestResolveAllowsReadsWithoutPrompting(t *testing.T) {
	asker := &stubAsker{}
	e := NewEngine(ModeDefault, noSandbox)
	ok, _, err := e.Resolve(context.Background(), asker, read())
	if err != nil || !ok {
		t.Fatalf("ok = %v, err = %v", ok, err)
	}
	if asker.calls != 0 {
		t.Error("reads must not prompt")
	}
}

func TestParseMode(t *testing.T) {
	for _, s := range []string{"plan", "default", "acceptEdits", "auto", "yolo", "bypass"} {
		if _, err := ParseMode(s); err != nil {
			t.Errorf("ParseMode(%q): %v", s, err)
		}
	}
	if _, err := ParseMode("certainly-not-a-mode"); err == nil {
		t.Error("an unknown mode must be an error, not a silent default")
	}
}

func external() Request {
	return Request{Tool: "mcp__gh__search", Effect: EffectExternal, Detail: `{"q":"x"} (gh server)`}
}

// An MCP tool acts outside every boundary this engine can reason about, so no
// bounded mode auto-allows it — bypass included, even on a host with a
// verified sandbox, because the server was never inside it. Yolo is the one
// exception: it is the everything-grant, and a grant that exempted the
// riskiest effect would not be what it says.
func TestExternalEffectAsksInEveryBoundedMode(t *testing.T) {
	for _, mode := range []Mode{ModeDefault, ModeAcceptEdits, ModeAuto, ModeBypass} {
		for _, cap := range []execution.Capability{noSandbox, verifiedSandbox} {
			e := NewEngine(mode, cap)
			if got := e.Check(external()).Decision; got != Ask {
				t.Errorf("mode %s: external = %s, want Ask", mode, got)
			}
		}
	}
	e := NewEngine(ModePlan, noSandbox)
	if got := e.Check(external()).Decision; got != Deny {
		t.Errorf("plan mode: external = %s, want Deny", got)
	}
	yolo := NewEngine(ModeYOLO, noSandbox)
	if got := yolo.Check(external()).Decision; got != Allow {
		t.Errorf("yolo mode: external = %s, want Allow from the everything-grant", got)
	}
}

func TestExternalAllowRuleAndRememberArePerTool(t *testing.T) {
	e := NewEngine(ModeDefault, noSandbox,
		Rule{Decision: Allow, Tool: "mcp__gh__search", Effect: EffectExternal})
	if got := e.Check(external()).Decision; got != Allow {
		t.Errorf("allow rule: external = %s, want Allow", got)
	}

	// Detail is display only: two invocations that differ only there are the
	// same request to the remember table, which is what makes "allow this
	// tool for the session" mean what it says.
	e = NewEngine(ModeDefault, noSandbox)
	first := external()
	e.Remember(first, true)
	second := external()
	second.Detail = `{"q":"totally different"} (gh server)`
	if got := e.Check(second).Decision; got != Allow {
		t.Errorf("remembered external tool = %s, want Allow across argument changes", got)
	}
}

// A rule matches requests the user never saw, which is what separates it from a
// remembered answer to one exact request. Short of yolo no standing rule
// approves a credential-bearing command unseen; yolo exempts nothing, so there
// the same command runs.
func TestARuleDoesNotApproveASensitiveCommandUnseen(t *testing.T) {
	engine := NewEngine(ModeDefault, execution.Capability{}, Rule{
		Decision: Allow, Tool: "exec", ArgvPrefix: []string{"curl"},
	})

	ordinary := Request{Tool: "exec", Effect: EffectExecute, Argv: []string{"curl", "https://example.com"}}
	if out := engine.Check(ordinary); out.Decision != Allow {
		t.Fatalf("the rule did not take effect on an ordinary command: %+v", out)
	}

	sensitive := ordinary
	sensitive.Sensitive = true
	out := engine.Check(sensitive)
	if out.Decision != Ask {
		t.Errorf("decision = %s, want ask: outside yolo a standing rule never approves a sensitive command unseen", out.Decision)
	}
	if !strings.Contains(out.Reason, "credential-bearing") {
		t.Errorf("reason = %q, which does not say why the rule yielded", out.Reason)
	}

	yolo := NewEngine(ModeYOLO, execution.Capability{}, Rule{
		Decision: Allow, Tool: "exec", ArgvPrefix: []string{"curl"},
	})
	if out := yolo.Check(sensitive); out.Decision != Allow {
		t.Errorf("yolo decision = %s, want allow: the everything-grant does not stop for a sensitive command", out.Decision)
	}
}

// A deny is not a rule among rules: it wins over any allow wherever it sits,
// which is what lets a user tighten a list they did not write.
func TestADenyOutranksAnEarlierAllow(t *testing.T) {
	engine := NewEngine(ModeDefault, execution.Capability{},
		Rule{Decision: Allow, Tool: "exec"},
		Rule{Decision: Deny, Tool: "exec", ArgvPrefix: []string{"rm"}},
	)

	if out := engine.Check(Request{Tool: "exec", Effect: EffectExecute, Argv: []string{"ls"}}); out.Decision != Allow {
		t.Errorf("the allow did not apply to an unrelated command: %+v", out)
	}
	out := engine.Check(Request{Tool: "exec", Effect: EffectExecute, Argv: []string{"rm", "-rf", "."}})
	if out.Decision != Deny {
		t.Errorf("decision = %s, want deny even though the allow is listed first", out.Decision)
	}
}

// Among non-deny rules the first match wins, so whoever is prepended decides.
// The product puts the user's own file ahead of a server's self-declared allow.
func TestTheFirstNonDenyRuleWins(t *testing.T) {
	userFirst := NewEngine(ModeDefault, execution.Capability{},
		Rule{Decision: Ask, Tool: "mcp__srv__deploy", Effect: EffectExternal},
		Rule{Decision: Allow, Tool: "mcp__srv__deploy", Effect: EffectExternal},
	)
	req := Request{Tool: "mcp__srv__deploy", Effect: EffectExternal}
	if out := userFirst.Check(req); out.Decision != Ask {
		t.Errorf("decision = %s, want ask: the rule listed first has to win", out.Decision)
	}

	serverFirst := NewEngine(ModeDefault, execution.Capability{},
		Rule{Decision: Allow, Tool: "mcp__srv__deploy", Effect: EffectExternal},
		Rule{Decision: Ask, Tool: "mcp__srv__deploy", Effect: EffectExternal},
	)
	if out := serverFirst.Check(req); out.Decision != Allow {
		t.Errorf("decision = %s, want allow: order is the whole mechanism", out.Decision)
	}
}
