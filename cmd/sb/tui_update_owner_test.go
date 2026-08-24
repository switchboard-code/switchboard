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

func TestAbnormalTUIExitWaitsForWindowsUpdateBackupExchange(t *testing.T) {
	m := testModel(t)
	dir := t.TempDir()
	exe := filepath.Join(dir, "sb.exe")
	staged := filepath.Join(dir, "staged.exe")
	mustWriteUpdateTestFile(t, exe, "old")
	mustWriteUpdateTestFile(t, staged, "new")

	firstMove := make(chan struct{})
	release := make(chan struct{})
	workDone := make(chan tea.Msg, 1)
	cmd := m.ownUpdateCmd(func(context.Context) tea.Msg {
		calls := 0
		_ = replaceExecutableWithBackup(exe, staged, func(from, to string) error {
			calls++
			if err := os.Rename(from, to); err != nil {
				return err
			}
			if calls == 1 {
				close(firstMove)
				<-release
			}
			return nil
		})
		return noticeMsg{text: "update finished"}
	})
	go func() { workDone <- cmd() }()

	select {
	case <-firstMove:
	case <-time.After(5 * time.Second):
		t.Fatal("update did not enter the Windows backup exchange")
	}
	if _, err := os.Stat(exe); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("executable at the deliberate between-moves boundary: %v", err)
	}
	if got, err := os.ReadFile(exe + ".old"); err != nil || string(got) != "old" {
		t.Fatalf("backup at the between-moves boundary = %q, %v", got, err)
	}

	exited := make(chan error, 1)
	go func() {
		exited <- runTUIProgram(tuiProgramFunc(func() (tea.Model, error) { return m, nil }), m)
	}()
	select {
	case err := <-exited:
		t.Fatalf("TUI returned inside the Windows update exchange: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-exited:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TUI did not join the completed Windows update exchange")
	}
	select {
	case <-workDone:
	case <-time.After(5 * time.Second):
		t.Fatal("update command remained stuck after TUI exit")
	}
	if got, err := os.ReadFile(exe); err != nil || string(got) != "new" {
		t.Fatalf("published executable = %q, %v", got, err)
	}
	if got, err := os.ReadFile(exe + ".old"); err != nil || string(got) != "old" {
		t.Fatalf("retained Windows backup = %q, %v", got, err)
	}
}

func TestAbnormalTUIExitWaitsForWindowsUpdateRollback(t *testing.T) {
	m := testModel(t)
	dir := t.TempDir()
	exe := filepath.Join(dir, "sb.exe")
	staged := filepath.Join(dir, "staged.exe")
	mustWriteUpdateTestFile(t, exe, "old")
	mustWriteUpdateTestFile(t, staged, "new")

	backupMoved := make(chan struct{})
	releasePublish := make(chan struct{})
	rollbackEntered := make(chan struct{})
	releaseRollback := make(chan struct{})
	workErr := make(chan error, 1)
	cmd := m.ownUpdateCmd(func(context.Context) tea.Msg {
		calls := 0
		injected := errors.New("injected publication failure")
		err := replaceExecutableWithBackup(exe, staged, func(from, to string) error {
			calls++
			switch calls {
			case 1:
				if err := os.Rename(from, to); err != nil {
					return err
				}
				close(backupMoved)
				<-releasePublish
				return nil
			case 2:
				return injected
			default:
				close(rollbackEntered)
				<-releaseRollback
				return os.Rename(from, to)
			}
		})
		workErr <- err
		return noticeMsg{text: "update rolled back"}
	})
	workDone := make(chan tea.Msg, 1)
	go func() { workDone <- cmd() }()
	select {
	case <-backupMoved:
	case <-time.After(5 * time.Second):
		t.Fatal("update did not move the current executable to its backup")
	}

	exited := make(chan error, 1)
	go func() {
		exited <- runTUIProgram(tuiProgramFunc(func() (tea.Model, error) { return m, nil }), m)
	}()
	close(releasePublish)
	select {
	case <-rollbackEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("failed publication did not enter rollback")
	}
	select {
	case err := <-exited:
		t.Fatalf("TUI returned inside the Windows update rollback: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	close(releaseRollback)
	select {
	case err := <-exited:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TUI did not join Windows update rollback")
	}
	select {
	case err := <-workErr:
		if err == nil || !strings.Contains(err.Error(), "current executable restored") {
			t.Fatalf("rollback result = %v", err)
		}
	default:
		t.Fatal("TUI returned before rollback result cleanup")
	}
	select {
	case <-workDone:
	case <-time.After(5 * time.Second):
		t.Fatal("rollback command remained stuck after TUI exit")
	}
	if got, err := os.ReadFile(exe); err != nil || string(got) != "old" {
		t.Fatalf("restored executable = %q, %v", got, err)
	}
	if _, err := os.Stat(exe + ".old"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback retained backup: %v", err)
	}
}

func TestAbnormalTUIExitPreventsUnstartedUpdate(t *testing.T) {
	m := testModel(t)
	ran := false
	cmd := m.ownUpdateCmd(func(context.Context) tea.Msg {
		ran = true
		return updateAppliedMsg{version: "unexpected"}
	})

	if err := runTUIProgram(tuiProgramFunc(func() (tea.Model, error) { return m, nil }), m); err != nil {
		t.Fatal(err)
	}
	if msg := cmd(); msg != nil {
		t.Fatalf("stopped update command returned %#v", msg)
	}
	if ran {
		t.Fatal("an update registered before exit started after the TUI stopped")
	}
}

func TestUpdateOwnerSerializesStartupAndManualChecks(t *testing.T) {
	m := testModel(t)
	started := make(chan struct{})
	cancelled := make(chan struct{})
	first := m.ownUpdateCmd(func(ctx context.Context) tea.Msg {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return updateCheckMsg{}
	})
	firstDone := make(chan tea.Msg, 1)
	go func() { firstDone <- first() }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first update did not start")
	}

	secondRan := false
	second := m.ownUpdateCmd(func(context.Context) tea.Msg {
		secondRan = true
		return updateAppliedMsg{version: "unexpected"}
	})
	msg, ok := second().(noticeMsg)
	if !ok || msg.level != "warn" {
		t.Fatalf("overlapping update result = %#v", msg)
	}
	if secondRan {
		t.Fatal("a second update ran beside the first")
	}

	if err := runTUIProgram(tuiProgramFunc(func() (tea.Model, error) { return m, nil }), m); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("TUI exit did not cancel the active update")
	}
	select {
	case <-firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled update command remained stuck after TUI exit")
	}
}
