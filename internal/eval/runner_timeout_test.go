package eval

import (
	"context"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/provider"
)

func TestRunnerTimeoutIncludesVerifier(t *testing.T) {
	entered := make(chan struct{})
	task := Task{
		ID:         "verifier-timeout",
		Provenance: HandWritten,
		Prompt:     "finish",
		Setup:      func(string) error { return nil },
		Verify: func(ctx context.Context, _ string) (bool, string, error) {
			close(entered)
			<-ctx.Done()
			return false, "", ctx.Err()
		},
	}
	arm := Arm{
		Name:     "fixed",
		Target:   provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-haiku-4-5"},
		Provider: &recordingProvider{probe: liveProbe(), turns: [][]provider.Event{completedTurn()}},
	}

	started := time.Now()
	got := (Runner{Catalog: evalCatalog(t), Timeout: 2 * time.Second}).Run(context.Background(), task, arm, 0)
	select {
	case <-entered:
	default:
		t.Fatal("attempt deadline expired before the verifier started; test did not exercise verifier timeout")
	}
	if got.Failure != FailureTimeout || got.Solved {
		t.Fatalf("verifier timeout run = %#v", got)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("verifier timeout returned after %s", elapsed)
	}
}

func TestRunnerCancellationStopsVerifier(t *testing.T) {
	entered := make(chan struct{})
	task := Task{
		ID:         "verifier-cancel",
		Provenance: HandWritten,
		Prompt:     "finish",
		Setup:      func(string) error { return nil },
		Verify: func(ctx context.Context, _ string) (bool, string, error) {
			close(entered)
			<-ctx.Done()
			return false, "", ctx.Err()
		},
	}
	arm := Arm{
		Name:     "fixed",
		Target:   provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-haiku-4-5"},
		Provider: &recordingProvider{probe: liveProbe(), turns: [][]provider.Event{completedTurn()}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan Run, 1)
	cat := evalCatalog(t)
	go func() {
		result <- (Runner{Catalog: cat, Timeout: time.Minute}).Run(ctx, task, arm, 0)
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("verifier did not start")
	}
	cancel()
	select {
	case got := <-result:
		if got.Failure != FailureCancelled || got.Solved {
			t.Fatalf("cancelled verifier run = %#v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("verifier did not return after owner cancellation")
	}
}
