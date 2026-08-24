package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/tools"
)

func assertTUIViewBounds(t *testing.T, view string, width, height int) {
	t.Helper()
	rows := strings.Split(view, "\n")
	if len(rows) > height {
		t.Fatalf("view has %d rows at %dx%d:\n%s", len(rows), width, height, view)
	}
	for row, line := range rows {
		if cells := ansi.StringWidth(line); cells > width {
			t.Fatalf("view row %d has %d cells at %dx%d: %q", row, cells, width, height, line)
		}
	}
}

func TestShortPermissionModalPinsConsequenceDefaultAndSelection(t *testing.T) {
	for _, height := range []int{6, 10} {
		t.Run(fmt.Sprintf("height_%d", height), func(t *testing.T) {
			m := testModel(t)
			m.Update(tea.WindowSizeMsg{Width: 20, Height: height})
			m.busy = true
			d := newPermissionDialog(permission.Request{
				Tool: "exec", Effect: permission.EffectExecute, Network: true,
				Argv: []string{"sh", "-c", strings.Repeat("dangerous command ", 20)},
			}, permission.Outcome{
				Decision: permission.Ask, SandboxAbsent: true,
				Reason: strings.Repeat("untrusted reason ", 20),
			}, make(chan permission.Response, 1))
			m.dlg = d

			for selection := 0; selection < 3; selection++ {
				d.sel = selection
				view := m.View()
				assertTUIViewBounds(t, view, 20, height)
				plain := ansi.Strip(view)
				if !strings.Contains(plain, "HOST ACCESS") {
					t.Errorf("selection %d hid the essential consequence:\n%s", selection, plain)
				}
				if !strings.Contains(plain, "safe") {
					t.Errorf("selection %d hid the denial/default row:\n%s", selection, plain)
				}
				if !strings.Contains(plain, "exec") {
					t.Errorf("selection %d hid the request identity:\n%s", selection, plain)
				}
				want := "no"
				if selection == 1 {
					want = "▌ yes once"
				} else if selection == 2 {
					want = "▌ allow exact"
				}
				if !strings.Contains(plain, want) {
					t.Errorf("selection %d is off-screen (missing %q):\n%s", selection, want, plain)
				}
			}

			// Navigation changes the highlighted row, and the short viewport follows
			// it without ever sacrificing the safe default or host warning.
			d.sel = 0
			for range 2 {
				d.update(tea.KeyMsg{Type: tea.KeyDown}, m.th)
			}
			plain := ansi.Strip(m.View())
			for _, want := range []string{"HOST ACCESS", "safe", "▌ allow exact"} {
				if !strings.Contains(plain, want) {
					t.Errorf("navigated short modal lost %q:\n%s", want, plain)
				}
			}
		})
	}
}

func TestShortQuestionModalKeepsWholeSemanticRows(t *testing.T) {
	for _, height := range []int{6, 10} {
		m := testModel(t)
		m.Update(tea.WindowSizeMsg{Width: 20, Height: height})
		d := newQuestionDialog(tools.Question{
			Question: "Which safe deployment target should be used for this operation?",
			Options: []tools.QuestionOption{
				{Label: "first", Detail: "detail one"},
				{Label: "second", Detail: "detail two"},
				{Label: "third", Detail: "detail three"},
			},
		}, make(chan tools.Answer, 1))
		m.dlg = d

		for _, selection := range []int{-1, 2} {
			d.sel = selection
			view := m.View()
			assertTUIViewBounds(t, view, 20, height)
			plain := ansi.Strip(view)
			if !strings.Contains(plain, "Which") || !strings.Contains(plain, "esc") {
				t.Fatalf("height %d selection %d lost question/decline context:\n%s", height, selection, plain)
			}
			want := "first"
			if selection == 2 {
				want = "▌ third"
			}
			if !strings.Contains(plain, want) {
				t.Fatalf("height %d selection %d hid %q:\n%s", height, selection, want, plain)
			}
			for _, line := range strings.Split(plain, "\n") {
				if strings.Contains(line, "detail two") && !strings.Contains(line, "second") {
					t.Fatalf("height %d orphaned an option detail:\n%s", height, plain)
				}
			}
		}
	}
}

func TestShortRaceModalKeepsEvidenceSafeChoiceAndSelectedCostTogether(t *testing.T) {
	m := raceModel(t)
	armA, err := assembleRaceArm(m.app, m.app.config.Tiers[0], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	armB, err := assembleRaceArm(m.app, m.app.config.Tiers[1], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	armA.status, armB.status = "completed", "completed"
	run := &raceRun{arms: [2]*raceArm{armA, armB}}
	run.labels = [2]string{"a · first", "b · second"}
	d := newRaceDialog(m, run)
	m.Update(tea.WindowSizeMsg{Width: 20, Height: 6})
	m.dlg = d

	for _, selection := range []int{0, 1, len(d.ids) - 1} {
		d.sel = selection
		plain := ansi.Strip(m.View())
		for _, want := range []string{"ROUTING EVIDENCE", "neither · stay", "▌"} {
			if !strings.Contains(plain, want) {
				t.Fatalf("selection %d lost %q:\n%s", selection, want, plain)
			}
		}
		if selection < 2 && !strings.Contains(plain, "keep") {
			t.Fatalf("selection %d hid the selected arm identity/cost:\n%s", selection, plain)
		}
	}
}

func TestShortTextAndSecretDialogsKeepEditableFieldVisible(t *testing.T) {
	for _, height := range []int{6, 10} {
		t.Run(fmt.Sprintf("height_%d", height), func(t *testing.T) {
			m := testModel(t)
			m.Update(tea.WindowSizeMsg{Width: 20, Height: height})
			text := newTextDialog(textPromptMsg{
				title: "Ollama server address", help: "where the local server listens",
				initial: "http://127.0.0.1:11434",
			})
			m.dlg = text
			plain := ansi.Strip(m.View())
			if !strings.Contains(plain, "Ollama") || !strings.Contains(plain, "▌") || !strings.Contains(plain, "7.0.0.1") {
				t.Fatalf("short text dialog hid identity or editable value:\n%s", plain)
			}

			secret := newSecretDialog(credential.Ref{Provider: "anthropic", Account: "work"}, "keychain", nil)
			secret.input.SetValue("super-secret-value")
			m.dlg = secret
			plain = ansi.Strip(m.View())
			if !strings.Contains(plain, "anthropic/work") || !strings.Contains(plain, "••") || !strings.Contains(plain, "enter store") {
				t.Fatalf("short secret dialog hid identity or masked input:\n%s", plain)
			}
			if strings.Contains(plain, "super-secret-value") {
				t.Fatal("short secret dialog echoed its secret")
			}
		})
	}
}

func TestShortExternalPermissionPinsConsequenceDefaultAndSelection(t *testing.T) {
	for _, height := range []int{6, 10} {
		t.Run(fmt.Sprintf("height_%d", height), func(t *testing.T) {
			m := testModel(t)
			m.Update(tea.WindowSizeMsg{Width: 20, Height: height})
			d := newPermissionDialog(permission.Request{
				Tool: "mcp__remote__write", Effect: permission.EffectExternal,
				Path: "remote.example", Detail: strings.Repeat("remote mutation ", 20),
			}, permission.Outcome{Decision: permission.Ask}, make(chan permission.Response, 1))
			m.dlg = d

			for selection := 0; selection < 3; selection++ {
				d.sel = selection
				view := m.View()
				assertTUIViewBounds(t, view, 20, height)
				plain := ansi.Strip(view)
				for _, want := range []string{"EXTERNAL", "safe", "▌"} {
					if !strings.Contains(plain, want) {
						t.Errorf("selection %d hid %q:\n%s", selection, want, plain)
					}
				}
			}
		})
	}
}

func TestShortPickerWindowFollowsSelection(t *testing.T) {
	m := testModel(t)
	m.Update(tea.WindowSizeMsg{Width: 20, Height: 6})
	items := make([]pickerItem, 24)
	for i := range items {
		items[i] = pickerItem{id: fmt.Sprint(i), label: fmt.Sprintf("choice-%02d", i)}
	}
	d := &pickerDialog{title: "many choices", items: items}
	m.dlg = d

	for _, selected := range []int{0, 7, 23} {
		d.sel = selected
		view := m.View()
		assertTUIViewBounds(t, view, 20, 6)
		plain := ansi.Strip(view)
		if want := fmt.Sprintf("choice-%02d", selected); !strings.Contains(plain, want) || !strings.Contains(plain, "▌") {
			t.Errorf("selection %d was outside its short picker window:\n%s", selected, plain)
		}
	}
}

func TestBusyNarrowViewKeepsPhysicalRowsAndMouseMapAligned(t *testing.T) {
	for _, width := range []int{20, 31} {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			m := testModel(t)
			m.Update(tea.WindowSizeMsg{Width: width, Height: 10})
			m.tr.reset()
			for i := 0; i < 20; i++ {
				m.tr.add(&entry{kind: kindInfo, text: fmt.Sprintf("entry %02d %s", i, strings.Repeat("界", 12))})
			}
			m.busy = true
			m.started = time.Now().Add(-time.Minute)
			m.queue = []string{"queued one", "queued two"}

			if line := m.workingLine(); ansi.StringWidth(line) > width || strings.Contains(line, "\n") {
				t.Fatalf("working line exceeds %d cells: %q", width, line)
			}
			view := m.View()
			assertTUIViewBounds(t, view, width, 10)
			if m.tr.height <= 0 {
				t.Fatal("busy narrow view gave the transcript no visible row")
			}
			for row := 0; row < m.tr.height; row++ {
				flat := m.tr.lineAt(row)
				if flat < 0 {
					continue
				}
				expected := -1
				for i := len(m.tr.starts) - 1; i >= 0; i-- {
					if m.tr.starts[i] <= flat {
						expected = i
						break
					}
				}
				if got := m.tr.entryAt(row); got != expected {
					t.Fatalf("busy viewport row %d maps to entry %d, want %d", row, got, expected)
				}
			}
		})
	}
}

func TestNarrowComposerPopupsNeverPhysicallyWrap(t *testing.T) {
	for _, width := range []int{20, 31} {
		for _, setup := range []struct {
			name string
			run  func(*tuiModel)
		}{
			{
				name: "mention",
				run: func(m *tuiModel) {
					m.mentionList = []string{strings.Repeat("界/", 24) + "file.go"}
					m.ta.SetValue("@界")
					m.ta.CursorEnd()
				},
			},
			{
				name: "history search",
				run: func(m *tuiModel) {
					m.histSearch = true
					m.histQuery = strings.Repeat("界", 24)
					m.history = []string{m.histQuery + " old prompt"}
					m.histMatch = 0
				},
			},
			{
				name: "transcript search",
				run: func(m *tuiModel) {
					m.trSearch = true
					m.trQuery = strings.Repeat("界", 24)
				},
			},
		} {
			t.Run(fmt.Sprintf("%s_%d", setup.name, width), func(t *testing.T) {
				m := testModel(t)
				m.Update(tea.WindowSizeMsg{Width: width, Height: 10})
				setup.run(m)
				assertTUIViewBounds(t, m.View(), width, 10)
			})
		}
	}
}
