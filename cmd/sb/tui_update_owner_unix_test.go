//go:build unix

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestAbnormalTUIExitWaitsForUnixUpdatePublicationSync(t *testing.T) {
	m := testModel(t)
	dir := t.TempDir()
	exe := filepath.Join(dir, "sb")
	if err := os.WriteFile(exe, []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}

	renamed := make(chan struct{})
	cancelled := make(chan struct{})
	releaseSync := make(chan struct{})
	workErr := make(chan error, 1)
	cmd := m.ownUpdateCmd(func(ctx context.Context) tea.Msg {
		err := installUpdateBinaryWithReplace(exe, []byte("new"), func(current, staged string) error {
			if err := os.Rename(staged, current); err != nil {
				return err
			}
			close(renamed)
			<-ctx.Done()
			close(cancelled)
			<-releaseSync
			d, err := os.Open(filepath.Dir(current))
			if err != nil {
				return err
			}
			return errors.Join(d.Sync(), d.Close())
		})
		workErr <- err
		return updateAppliedMsg{version: "test"}
	})
	workDone := make(chan tea.Msg, 1)
	go func() { workDone <- cmd() }()

	select {
	case <-renamed:
	case <-time.After(5 * time.Second):
		t.Fatal("update did not reach the Unix rename/fsync boundary")
	}
	if got, err := os.ReadFile(exe); err != nil || string(got) != "new" {
		t.Fatalf("renamed executable = %q, %v", got, err)
	}

	exited := make(chan error, 1)
	go func() {
		exited <- runTUIProgram(tuiProgramFunc(func() (tea.Model, error) { return m, nil }), m)
	}()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("TUI exit did not cancel the updater lifetime")
	}
	select {
	case err := <-exited:
		t.Fatalf("TUI returned before the post-rename directory sync: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	close(releaseSync)
	select {
	case err := <-exited:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TUI did not join Unix update publication")
	}
	select {
	case err := <-workErr:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("TUI returned before update staging cleanup completed")
	}
	select {
	case <-workDone:
	case <-time.After(5 * time.Second):
		t.Fatal("update command remained stuck after TUI exit")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".sb-update-") {
			t.Fatalf("dropped update result retained staged file %s", entry.Name())
		}
	}
}
