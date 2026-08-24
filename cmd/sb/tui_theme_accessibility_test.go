package main

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// TestThemeContrast pins the semantic palette to the xterm-256 colors the TUI
// actually emits. Text clears WCAG's 4.5:1 normal-text threshold; structural
// marks such as rails and empty gauges clear the 3:1 UI-component threshold.
// The test is deliberately independent of the host terminal's palette and
// color profile, so a CI runner cannot turn an inaccessible color into a pass.
func TestThemeContrast(t *testing.T) {
	tests := []struct {
		name string
		th   *theme
		page lipgloss.Color
	}{
		// Use each theme's explicit raised surface as the conservative page
		// reference. If text clears that shade, it also clears the canonical
		// black/white terminal background the theme targets.
		{name: "dark", th: darkTheme(), page: lipgloss.Color("235")},
		{name: "light", th: lightTheme(), page: lipgloss.Color("255")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			th := test.th
			text := map[string]lipgloss.Style{
				"text": th.text, "dim": th.dim, "faint": th.faint,
				"accent": th.accent, "user": th.user, "ok": th.ok,
				"warn": th.warn, "error": th.err, "thinking": th.thinking,
			}
			for name, style := range text {
				assertContrast(t, name+" on page", style.GetForeground(), test.page, 4.5)
				assertContrast(t, name+" on status bar", style.GetForeground(), th.barBg, 4.5)
			}

			assertContrast(t, "card text", th.text.GetForeground(), th.surface, 4.5)
			assertContrast(t, "card marker", th.user.GetForeground(), th.surface, 4.5)
			assertContrast(t, "selected text", th.selected.GetForeground(), th.selected.GetBackground(), 4.5)
			assertContrast(t, "border", th.border.GetForeground(), test.page, 3)
			assertContrast(t, "empty context gauge", th.barEmpty.GetForeground(), test.page, 3)

			assertContrast(t, "tier chip", th.tierChip.GetForeground(), th.tierChip.GetBackground(), 4.5)
			for name, style := range th.modeChip {
				assertContrast(t, "mode chip "+name, style.GetForeground(), style.GetBackground(), 4.5)
				if name != "default" {
					// Non-default modes use the chip's fill as status-bar text,
					// making a widened permission posture visible without a fill.
					assertContrast(t, "mode status "+name, style.GetBackground(), th.barBg, 4.5)
				}
			}

			seen := make(map[int]struct{}, len(th.rungs))
			for rank, style := range th.rungs {
				index := terminalColorIndex(t, style.GetForeground())
				if _, duplicate := seen[index]; duplicate {
					t.Errorf("rung %d reuses ANSI color %d", rank+1, index)
				}
				seen[index] = struct{}{}
				assertContrast(t, fmt.Sprintf("rung %d label", rank+1), style.GetForeground(), test.page, 4.5)
				assertContrast(t, fmt.Sprintf("rung %d status mark", rank+1), style.GetForeground(), th.barBg, 3)
				chip := th.rungChip(rank)
				assertContrast(t, fmt.Sprintf("rung %d chip", rank+1), chip.GetForeground(), chip.GetBackground(), 4.5)
			}
		})
	}
}

func assertContrast(t *testing.T, name string, foreground, background lipgloss.TerminalColor, minimum float64) {
	t.Helper()
	ratio := contrastRatio(xterm256RGB(terminalColorIndex(t, foreground)), xterm256RGB(terminalColorIndex(t, background)))
	if ratio+0.001 < minimum {
		t.Errorf("%s contrast = %.2f:1, want at least %.1f:1", name, ratio, minimum)
	}
}

func terminalColorIndex(t *testing.T, color lipgloss.TerminalColor) int {
	t.Helper()
	value, ok := color.(lipgloss.Color)
	if !ok {
		t.Fatalf("palette color has type %T, want lipgloss.Color", color)
	}
	index, err := strconv.Atoi(string(value))
	if err != nil || index < 0 || index > 255 {
		t.Fatalf("palette color %q is not an ANSI-256 index", value)
	}
	return index
}

func xterm256RGB(index int) [3]float64 {
	base := [16][3]float64{
		{0, 0, 0}, {128, 0, 0}, {0, 128, 0}, {128, 128, 0},
		{0, 0, 128}, {128, 0, 128}, {0, 128, 128}, {192, 192, 192},
		{128, 128, 128}, {255, 0, 0}, {0, 255, 0}, {255, 255, 0},
		{0, 0, 255}, {255, 0, 255}, {0, 255, 255}, {255, 255, 255},
	}
	if index < len(base) {
		return base[index]
	}
	if index >= 232 {
		gray := float64(8 + 10*(index-232))
		return [3]float64{gray, gray, gray}
	}
	levels := [...]float64{0, 95, 135, 175, 215, 255}
	cube := index - 16
	return [3]float64{levels[cube/36], levels[(cube%36)/6], levels[cube%6]}
}

func contrastRatio(a, b [3]float64) float64 {
	luminance := func(rgb [3]float64) float64 {
		linear := func(channel float64) float64 {
			channel /= 255
			if channel <= 0.04045 {
				return channel / 12.92
			}
			return math.Pow((channel+0.055)/1.055, 2.4)
		}
		return 0.2126*linear(rgb[0]) + 0.7152*linear(rgb[1]) + 0.0722*linear(rgb[2])
	}
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// NO_COLOR is read when lipgloss first detects its output profile, so exercise
// it in a fresh copy of the test process rather than depending on global test
// order. The labels remain: color never carries the only copy of TUI state.
func TestThemeHonorsNoColor(t *testing.T) {
	if os.Getenv("SB_THEME_NO_COLOR_HELPER") == "1" {
		if profile := lipgloss.ColorProfile(); profile != termenv.Ascii {
			t.Fatalf("NO_COLOR profile = %v, want ASCII", profile)
		}
		th := darkTheme()
		rendered := strings.Join([]string{
			th.accent.Render("route t2"),
			th.onBar(th.warn).Render("ctx 85%"),
			th.rungChip(3).Render(" t4 "),
			th.selected.Render("selected"),
		}, "\n")
		if strings.Contains(rendered, "\x1b") {
			t.Fatalf("NO_COLOR rendering contains an escape sequence: %q", rendered)
		}
		for _, label := range []string{"route t2", "ctx 85%", "t4", "selected"} {
			if !strings.Contains(rendered, label) {
				t.Errorf("NO_COLOR rendering lost %q: %q", label, rendered)
			}
		}
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestThemeHonorsNoColor$")
	cmd.Env = append(withoutColorEnvironment(os.Environ()),
		"NO_COLOR=1", "SB_THEME_NO_COLOR_HELPER=1", "TERM=xterm-256color")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("NO_COLOR helper failed: %v\n%s", err, output)
	}
}

func withoutColorEnvironment(environment []string) []string {
	clean := make([]string, 0, len(environment))
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "NO_COLOR", "CLICOLOR", "CLICOLOR_FORCE", "SB_THEME_NO_COLOR_HELPER", "TERM":
			continue
		}
		clean = append(clean, entry)
	}
	return clean
}

func TestThemesPreserveNarrowFallbacks(t *testing.T) {
	previous := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(previous) })

	for _, th := range []*theme{darkTheme(), lightTheme()} {
		t.Run(th.name, func(t *testing.T) {
			m := testModel(t)
			m.th = th
			m.tr.setTheme(th)
			m.md.setDark(th.dark)
			m.width, m.height = 20, 6
			m.ta.SetWidth(14)
			m.ta.SetValue("編集中")
			assertCellBound(t, m.inputZoneView(), 20)

			picker := &pickerDialog{
				title: "theme",
				items: []pickerItem{
					{id: "dark", label: "dark", desc: "dark terminal"},
					{id: "light", label: "light", desc: "light terminal"},
				},
			}
			assertCellBound(t, picker.view(20, th), 20)
		})
	}
}
