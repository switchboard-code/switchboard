package main

import (
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/execution"
)

// These settings deliberately take effect in the running process even when
// persistence fails. Their failure text is the contract boundary: unlike
// routing, compaction, updates, and context allowances, they must not imply
// that the live change was rolled back.
func TestProcessOnlySettingsSayTheyRemainLiveWhenSaveFails(t *testing.T) {
	t.Run("theme", func(t *testing.T) {
		m := testModel(t)
		m.app.config.Path = t.TempDir()
		msg := cmdTheme(m, "light")().(noticeMsg)
		if m.app.config.Theme != "light" || !strings.Contains(msg.text, "theme is now light") ||
			!strings.Contains(msg.text, "saving it failed") {
			t.Fatalf("theme failure contract = state %q, notice %#v", m.app.config.Theme, msg)
		}
	})

	t.Run("notify", func(t *testing.T) {
		m := testModel(t)
		m.app.config.Path = t.TempDir()
		msg := cmdNotify(m, "off")().(noticeMsg)
		if m.app.config.NotifyOn() || !strings.Contains(msg.text, "notify is now off") ||
			!strings.Contains(msg.text, "saving it failed") {
			t.Fatalf("notify failure contract = state %v, notice %#v", m.app.config.NotifyOn(), msg)
		}
	})

	t.Run("mouse", func(t *testing.T) {
		m := testModel(t)
		m.app.config.Path = t.TempDir()
		said := strings.Join(notices(t, cmdMouse(m, "off")), " ")
		if m.app.config.MouseOn() || !strings.Contains(said, "mouse is off") ||
			!strings.Contains(said, "saving it failed") {
			t.Fatalf("mouse failure contract = state %v, notice %q", m.app.config.MouseOn(), said)
		}
	})

	t.Run("budget", func(t *testing.T) {
		m := testModel(t)
		m.app.config.Path = t.TempDir()
		m.app.budget = &budgetState{}
		msg := cmdBudget(m, "1.25")().(noticeMsg)
		if m.app.budget.get() == 0 || m.app.config.Budget == 0 ||
			!strings.Contains(msg.text, "set for this session, but not saved") {
			t.Fatalf("budget failure contract = live %v config %v, notice %#v",
				m.app.budget.get(), m.app.config.Budget, msg)
		}
	})

	t.Run("sandbox", func(t *testing.T) {
		m := testModel(t)
		m.app.config.Path = t.TempDir()
		msg := cmdSandbox(m, "auto")().(noticeMsg)
		mode := m.app.loop.Perms.Execution().SandboxMode()
		if mode != execution.SandboxAuto || m.app.config.Sandbox != execution.SandboxAuto ||
			!strings.Contains(msg.text, "sandbox changed for this process") ||
			!strings.Contains(msg.text, "config was not saved") {
			t.Fatalf("sandbox failure contract = runtime %q config %q, notice %#v",
				mode, m.app.config.Sandbox, msg)
		}
	})
}
