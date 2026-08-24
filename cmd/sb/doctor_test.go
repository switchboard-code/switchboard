package main

import (
	"context"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
)

func TestDoctorSaysEverythingAnswersWhenItDoes(t *testing.T) {
	isolateTestHome(t, t.TempDir())
	ts := fakeOllama(t, "small", "big")
	cfg := &config.Config{
		Path:  "test.toml",
		Tiers: []config.Tier{ollamaTier("t1", "small"), ollamaTier("t2", "big")},
	}
	cat, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}

	var b strings.Builder
	if err := runDoctorCLI(context.Background(), &b, cfg, cat, newProviders(ts.URL, cfg), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"everything a session needs answers",
		"ollama/local/small answers",
		"ollama/local/big answers",
		"local server, no key needed",
		"no ecosystem marker",
		"no servers declared",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "! ") {
		t.Errorf("a healthy machine grew a ! mark:\n%s", out)
	}
}

func TestDoctorMarksARungNothingCanServe(t *testing.T) {
	isolateTestHome(t, t.TempDir())
	ts := fakeOllama(t, "present")
	cfg := &config.Config{
		Path:  "test.toml",
		Tiers: []config.Tier{ollamaTier("t1", "present"), ollamaTier("t2", "missing")},
	}
	cat, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}

	var b strings.Builder
	if err := runDoctorCLI(context.Background(), &b, cfg, cat, newProviders(ts.URL, cfg), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "1 check needs attention") {
		t.Errorf("one dead rung must produce one marked check:\n%s", out)
	}
	if !strings.Contains(out, "! t2") {
		t.Errorf("the dead rung is not the one marked:\n%s", out)
	}
	if strings.Contains(out, "! t1") {
		t.Errorf("the healthy rung was marked:\n%s", out)
	}
}

func TestDoctorReportsAFallbackServingItsRung(t *testing.T) {
	isolateTestHome(t, t.TempDir())
	ts := fakeOllama(t, "backup")
	cfg := &config.Config{
		Path:  "test.toml",
		Tiers: []config.Tier{ollamaTier("t1", "missing", "backup")},
	}
	cat, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}

	var b strings.Builder
	if err := runDoctorCLI(context.Background(), &b, cfg, cat, newProviders(ts.URL, cfg), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "everything a session needs answers") {
		t.Errorf("a rung served by its fallback is a served rung, not a failure:\n%s", out)
	}
	if !strings.Contains(out, "fallback") {
		t.Errorf("the substitution must be said, not smoothed over:\n%s", out)
	}
}

func TestDoctorPointsAnEmptyLadderAtSetup(t *testing.T) {
	isolateTestHome(t, t.TempDir())
	cfg := &config.Config{Path: "test.toml"}
	cat, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}

	var b strings.Builder
	if err := runDoctorCLI(context.Background(), &b, cfg, cat, newProviders("", cfg), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "no tiers bound") {
		t.Errorf("an empty ladder must name its next action:\n%s", b.String())
	}
}

func TestDoctorSectionRedactsAndEscapesDynamicRows(t *testing.T) {
	token := "ghp_" + strings.Repeat("d", 36)
	unsafe := "row\n\x1b]2;spoof\a\u202eright " + token
	rows := []doctorRow{{label: unsafe, detail: unsafe, bad: true}}
	var out strings.Builder
	printDoctorSection(&out, unsafe, rows)
	got := out.String()
	if strings.Contains(got, token) || !strings.Contains(got, "[redacted: a GitHub token]") {
		t.Fatalf("doctor exposed a credential: %q", got)
	}
	for _, control := range []string{"\x1b", "\a", "\u202e"} {
		if strings.Contains(got, control) {
			t.Fatalf("doctor retained terminal control %q: %q", control, got)
		}
	}
	if strings.Count(got, "\n") != 3 {
		t.Fatalf("untrusted newline changed doctor row structure: %q", got)
	}
}
