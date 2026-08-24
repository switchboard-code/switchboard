package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/checkpoint"
	"github.com/switchboard-code/switchboard/internal/session"
)

func TestInterruptedRetryRecoveryNoticeStatesCommitDecision(t *testing.T) {
	if got := interruptedRetryRecoveryNotice(checkpoint.DurableUndoRecovery{}); got != "" {
		t.Fatalf("empty recovery notice = %q", got)
	}
	published := interruptedRetryRecoveryNotice(checkpoint.DurableUndoRecovery{Found: true, Published: true})
	if !strings.Contains(published, "published") || !strings.Contains(published, "committed pre-turn") {
		t.Fatalf("published recovery notice = %q", published)
	}
	warning := interruptedRetryRecoveryNotice(checkpoint.DurableUndoRecovery{
		Found: true, Published: true, CleanupWarning: errors.New("injected cleanup failure"),
	})
	if !strings.Contains(warning, "could not be cleared") || !strings.Contains(warning, "injected cleanup failure") {
		t.Fatalf("cleanup warning notice = %q", warning)
	}
	rolled := interruptedRetryRecoveryNotice(checkpoint.DurableUndoRecovery{
		Found: true, RolledForward: 2, AlreadyPost: 1,
	})
	for _, want := range []string{"before publication", "source session's post-turn", "3", "2 repaired", "1 already correct"} {
		if !strings.Contains(rolled, want) {
			t.Fatalf("unpublished recovery notice %q does not contain %q", rolled, want)
		}
	}
}

func TestUnresolvedRetryStartupCannotResumePastItsGoverningChild(t *testing.T) {
	const child = "20260823T120000-abcdef12"
	tests := []struct {
		name        string
		opts        options
		interactive bool
		wantResume  string
		wantErr     string
		wantNote    bool
	}{
		{name: "default adopts child", interactive: true, wantResume: child, wantNote: true},
		{name: "continue uses latest child", opts: options{cont: true}, interactive: true},
		{name: "explicit child", opts: options{resume: child}, interactive: true, wantResume: child},
		{name: "explicit source refused", opts: options{resume: "20260823T115900-12345678"}, interactive: true, wantErr: "resume that child first"},
		{name: "headless refused", opts: options{prompt: "work"}, wantErr: "interactive TUI"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := tt.opts
			note, err := constrainUnresolvedRetryStartup(&opts, child, session.RetryIntentStarted, tt.interactive)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("startup error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if opts.resume != tt.wantResume {
				t.Fatalf("resume = %q, want %q", opts.resume, tt.wantResume)
			}
			if (note != "") != tt.wantNote {
				t.Fatalf("startup note = %q, want present=%v", note, tt.wantNote)
			}
		})
	}
}
