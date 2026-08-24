package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/advisor"
	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/tools"
)

func TestSessionSwapDropsPendingAdviceFromThePriorSession(t *testing.T) {
	m := raceModel(t)
	shown := make(chan string, 1)
	client := &racedProvider{turns: []racedTurn{racedText("old-session-only advice")}}
	adv := advisor.New(agent.NopObserver{}, client, m.app.tier.Target, func(text string) { shown <- text },
		advisor.WithBounds(1, time.Nanosecond))
	adv.StartTurn("old session task")
	req := permission.Request{Tool: "exec", Argv: []string{"go", "test", "./..."}}
	for i := 0; i < 4; i++ {
		call := provider.ToolUse{
			ID:    fmt.Sprintf("call-%d", i),
			Name:  "exec",
			Input: json.RawMessage(`{"argv":["go","test","./..."]}`),
		}
		adv.ToolStart(call, req)
		adv.ToolEnd(call, req, tools.Result{Content: "FAIL: TestX", IsError: true}, time.Second)
	}
	select {
	case <-shown:
	case <-time.After(5 * time.Second):
		t.Fatal("advisor did not produce pending advice")
	}
	if err := adv.PauseAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.app.setAdvisor(adv)

	fresh, err := m.app.store.Create(m.app.workspace, m.app.tier.Target.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fresh.Close() })
	cmd := m.onSessionSwap(sessionSwapMsg{
		sess: fresh, tier: m.app.tier, client: m.app.loop.Binding().Provider, fresh: true,
		release: adv.Resume,
	})
	if cmd != nil {
		t.Fatal("ordinary committed session swap returned unexpected continuation")
	}
	if got := m.app.loop.Session.ID(); got != fresh.ID() {
		t.Fatalf("active session = %s, want %s", got, fresh.ID())
	}
	if got := m.adviceContext("new session task"); got != "new session task" {
		t.Fatalf("old-session advisor evidence crossed the swap: %q", got)
	}
}
