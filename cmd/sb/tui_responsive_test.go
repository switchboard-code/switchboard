package main

import (
	"fmt"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/tools"
)

func assertCellBound(t *testing.T, rendered string, width int) int {
	t.Helper()
	if !utf8.ValidString(rendered) {
		t.Fatalf("rendered output is not valid UTF-8: %q", rendered)
	}
	maximum := 0
	for row, line := range strings.Split(rendered, "\n") {
		cells := ansi.StringWidth(line)
		maximum = max(maximum, cells)
		if cells > width {
			t.Errorf("row %d is %d cells, want at most %d: %q", row, cells, width, line)
		}
	}
	return maximum
}

func TestBannerFitsUnicodeByTerminalCellsAfterResize(t *testing.T) {
	for _, size := range []struct{ width, height int }{
		{width: 20, height: 4},
		{width: 31, height: 6},
		{width: 80, height: 10},
		{width: 160, height: 12},
	} {
		t.Run(fmt.Sprintf("%dx%d", size.width, size.height), func(t *testing.T) {
			m := testModel(t)
			legal := "階層🙂e\u0301" + strings.Repeat("非常に長い", 30)
			target := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: legal}
			m.app.config.Tiers = []config.Tier{{ID: legal, Label: legal, Target: target}}
			m.app.tier = m.app.config.Tiers[0]
			m.app.workspace = "/tmp/" + strings.Repeat(legal, 3)
			m.tr.reset()
			m.tr.setWidth(size.width)
			m.addBanner(m.app.loop.Session, false)

			rendered := strings.Join(m.tr.flat, "\n")
			wantMaximum := min(size.width, maxTextWidth)
			if got := assertCellBound(t, rendered, wantMaximum); got != wantMaximum {
				t.Errorf("maximum row width = %d, want exact fitted width %d", got, wantMaximum)
			}
			if !strings.Contains(ansi.Strip(rendered), "階層") {
				t.Fatalf("legal Unicode label disappeared: %q", ansi.Strip(rendered))
			}

			visible := m.tr.view(size.height)
			if got := len(strings.Split(visible, "\n")); got != size.height {
				t.Errorf("visible banner rows = %d, want %d", got, size.height)
			}
			assertCellBound(t, visible, wantMaximum)
		})
	}
}

func TestSuggestionsFitUnicodeAcrossWidthsAndShortHeights(t *testing.T) {
	tests := []struct {
		width, height int
		wantRows      int
	}{
		{width: 20, height: 6, wantRows: 1},
		{width: 31, height: 8, wantRows: 3},
		{width: 80, height: 12, wantRows: 6},
		{width: 160, height: 30, wantRows: 6},
	}
	for _, test := range tests {
		t.Run(fmt.Sprintf("%dx%d", test.width, test.height), func(t *testing.T) {
			m := testModel(t)
			m.width, m.height = test.width, test.height
			m.ta.SetWidth(max(test.width-6, 1))
			m.ta.SetValue("/novel")
			legal := "階層🙂e\u0301" + strings.Repeat("長い", 20)
			for i := 0; i < 8; i++ {
				m.custom = append(m.custom, customCommand{
					name: fmt.Sprintf("novel%d-%s", i, legal),
					desc: "説明🚀e\u0301 " + strings.Repeat(legal, 2),
				})
			}
			m.sugSel = 7

			rendered := m.suggestionsView()
			if got := len(strings.Split(rendered, "\n")); got != test.wantRows {
				t.Errorf("suggestion rows = %d, want %d: %q", got, test.wantRows, rendered)
			}
			if got := assertCellBound(t, rendered, test.width-1); got != test.width-1 {
				t.Errorf("maximum row width = %d, want exact fitted width %d", got, test.width-1)
			}
			if !strings.Contains(ansi.Strip(rendered), "novel") {
				t.Fatalf("command label disappeared: %q", ansi.Strip(rendered))
			}
		})
	}
}

func TestComposerFitsNarrowTerminalWidths(t *testing.T) {
	for _, width := range []int{20, 31, 80} {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			m := testModel(t)
			m.width = width
			m.ta.SetWidth(max(width-6, 1))
			m.ta.SetValue("編集中🙂e\u0301")

			rendered := m.inputZoneView()
			if got := assertCellBound(t, rendered, width); got > width {
				t.Fatalf("composer maximum row width = %d, want at most %d", got, width)
			}
			if !strings.Contains(ansi.Strip(rendered), "編集中") {
				t.Fatalf("composer lost legal Unicode input: %q", ansi.Strip(rendered))
			}
		})
	}
}

func TestPickerFitsUnicodeAcrossTerminalWidths(t *testing.T) {
	for _, width := range []int{20, 31, 80, 160} {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			legal := "選択🙂e\u0301" + strings.Repeat("非常に長い", 24)
			items := make([]pickerItem, 14)
			for i := range items {
				items[i] = pickerItem{
					id:      fmt.Sprintf("item-%d", i),
					label:   fmt.Sprintf("%02d-%s", i, legal),
					desc:    "説明🚀e\u0301 " + strings.Repeat(legal, 2),
					current: i == 2,
				}
			}
			d := &pickerDialog{title: legal, items: items, sel: 9}
			rendered := d.view(width, darkTheme())

			if got := assertCellBound(t, rendered, width); got != width {
				t.Errorf("maximum row width = %d, want exact fitted width %d", got, width)
			}
			plain := ansi.Strip(rendered)
			if !strings.Contains(plain, "選択") || !strings.Contains(plain, "09-") {
				t.Fatalf("picker lost its legal Unicode title or selected row: %q", plain)
			}
			if !strings.Contains(plain, "more") || !strings.Contains(plain, "of 14") {
				t.Fatalf("fitted picker lost its fold affordances: %q", plain)
			}
		})
	}
}

func withoutWhitespace(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || strings.ContainsRune("│╭╮╰╯─", r) {
			return -1
		}
		return r
	}, s)
}

func TestDialogViewsBoundUnicodeAndUntrustedTextAcrossTerminalWidths(t *testing.T) {
	const unsafe = "\x1b]0;forged-title\x07"
	legal := "法務🙂e\u0301" + strings.Repeat("超長契約", 240) + unsafe
	secret := "sk-test-" + strings.Repeat("秘🙂", 80)

	tests := []struct {
		name     string
		maxLines int
		render   func(width int, th *theme) string
		want     []string
	}{
		{
			name: "permission", maxLines: 36,
			render: func(width int, th *theme) string {
				d := newPermissionDialog(permission.Request{
					Tool: legal, Effect: permission.EffectExecute,
					Argv: []string{legal}, Network: true,
				}, permission.Outcome{Reason: legal, SandboxAbsent: true}, make(chan permission.Response, 1))
				return d.view(width, th)
			},
			want: []string{"法務", "…", "FULLHOSTACCESS", "FULLNETWORKACCESSREQUESTED", "yes", "no"},
		},
		{
			name: "text", maxLines: 14,
			render: func(width int, th *theme) string {
				d := newTextDialog(textPromptMsg{title: legal, help: legal, initial: legal})
				return d.view(width, th)
			},
			want: []string{"法務", "…", "entersave"},
		},
		{
			name: "secret", maxLines: 16,
			render: func(width int, th *theme) string {
				d := newSecretDialog(credential.Ref{Provider: legal, Account: legal}, legal, nil)
				d.input.SetValue(secret)
				return d.view(width, th)
			},
			want: []string{"法務", "…", "enterstore"},
		},
		{
			name: "question", maxLines: 19,
			render: func(width int, th *theme) string {
				d := newQuestionDialog(tools.Question{
					Question: legal,
					Options: []tools.QuestionOption{
						{Label: legal, Detail: legal},
						{Label: "二🚀e\u0301" + legal, Detail: legal},
					},
				}, make(chan tools.Answer, 1))
				return d.view(width, th)
			},
			want: []string{"法務", "二", "…", "typeyourownanswer"},
		},
	}

	for _, width := range []int{20, 31, 80} {
		for _, test := range tests {
			t.Run(fmt.Sprintf("%s_%d", test.name, width), func(t *testing.T) {
				rendered := test.render(width, darkTheme())
				if got, want := assertCellBound(t, rendered, width), width-2; got != want {
					t.Errorf("maximum row width = %d, want exact dialog width %d", got, want)
				}
				if got := len(strings.Split(rendered, "\n")); got > test.maxLines {
					t.Errorf("dialog grew to %d rows, want at most %d", got, test.maxLines)
				}
				plain := withoutWhitespace(ansi.Strip(rendered))
				for _, want := range test.want {
					if !strings.Contains(plain, want) {
						t.Errorf("dialog lost %q:\n%s", want, ansi.Strip(rendered))
					}
				}
				for _, control := range []string{"\x1b]", "\x07", "\r"} {
					if strings.Contains(rendered, control) {
						t.Errorf("dialog retained terminal control %q: %q", control, rendered)
					}
				}
				if test.name == "secret" && strings.Contains(rendered, secret) {
					t.Fatal("secret dialog echoed the credential")
				}
			})
		}
	}
}

func TestRaceDialogBoundsUnicodeAndUntrustedLabelsAcrossTerminalWidths(t *testing.T) {
	const unsafe = "\x1b]8;;https://example.invalid\x07"
	legal := "競合🚀e\u0301" + strings.Repeat("長大な候補", 240) + unsafe
	for _, width := range []int{20, 31, 80} {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			d := &raceDialog{lbl: []string{legal, "二🙂e\u0301" + legal}, sel: 1}
			rendered := d.view(width, darkTheme())
			if got := assertCellBound(t, rendered, width); got != width {
				t.Errorf("maximum row width = %d, want exact fitted width %d", got, width)
			}
			if got := len(strings.Split(rendered, "\n")); got > 16 {
				t.Errorf("race dialog grew to %d rows, want at most 16", got)
			}
			plain := ansi.Strip(rendered)
			for _, want := range []string{"競合", "二", "…", "which answer"} {
				if !strings.Contains(plain, want) {
					t.Errorf("race dialog lost %q:\n%s", want, plain)
				}
			}
			for _, control := range []string{"\x1b]", "\x07", "\r"} {
				if strings.Contains(rendered, control) {
					t.Errorf("race dialog retained terminal control %q: %q", control, rendered)
				}
			}
		})
	}
}

func TestComposerAndSearchSurfacesEscapeTerminalControls(t *testing.T) {
	m := testModel(t)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 12})

	m.ta.SetValue("safe\u202ereverse")
	m.ta.CursorEnd()
	if view := m.View(); strings.Contains(view, "\u202e") || !strings.Contains(view, `\u202e`) {
		t.Fatalf("composer did not render bidi control visibly: %q", view)
	}

	m.ta.Reset()
	m.histSearch = true
	m.histQuery = "query\u202e"
	m.history = []string{"hit\x1b]0;forged\x07\u202ereverse" + strings.Repeat("界", 34)}
	m.histMatch = 0
	view := m.View()
	for _, unsafe := range []string{"\x1b]", "\x07", "\u202e"} {
		if strings.Contains(view, unsafe) {
			t.Fatalf("history search retained control %q: %q", unsafe, view)
		}
	}
	if !utf8.ValidString(view) {
		t.Fatalf("history search produced invalid UTF-8: %q", view)
	}

	m.histSearch = false
	m.trSearch = true
	m.trQuery = "needle\x1b]0;forged\x07\u202e"
	view = m.View()
	for _, unsafe := range []string{"\x1b]", "\x07", "\u202e"} {
		if strings.Contains(view, unsafe) {
			t.Fatalf("transcript search retained control %q: %q", unsafe, view)
		}
	}
}

func TestWrapCellsMakesStyledRowsIndependentlyRenderable(t *testing.T) {
	rows := strings.Split(wrapCells("\x1b[31mabcdef\x1b[0m", 3), "\n")
	if len(rows) != 2 {
		t.Fatalf("wrapped rows = %q, want two", rows)
	}
	for i, row := range rows {
		if !strings.HasSuffix(row, ansi.ResetStyle) {
			t.Errorf("row %d does not close its style: %q", i, row)
		}
	}
	if !strings.Contains(rows[1], "\x1b[31m") {
		t.Fatalf("continuation row did not resume red: %q", rows[1])
	}
}
