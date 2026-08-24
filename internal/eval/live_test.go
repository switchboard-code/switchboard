package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/provider/anthropic"
	"github.com/switchboard-code/switchboard/internal/provider/kimi"
	"github.com/switchboard-code/switchboard/internal/provider/ollama"
	"github.com/switchboard-code/switchboard/internal/provider/openai"
	"github.com/switchboard-code/switchboard/internal/provider/openaicompat"
)

// armsFor builds the ladder, lowest rung first. The baselines are its ends.
//
// SB_EVAL_LADDER selects the configuration, because the choice changes what the
// gate can establish and that is not a detail to leave implicit:
//
//	money  a paid ladder, where §7.1's cost condition means what it says
//	plan   a plan-metered ladder, where every rung bills zero and the cost
//	       condition is degenerate, so only the solve-rate half is measured
//
// compatArm builds the arm for a configured OpenAI-compatible endpoint. It
// reads the same [providers."openaicompat/generic"] base_url a session reads,
// so measuring a deployment does not mean describing it twice.
func compatArm(t *testing.T) Arm {
	t.Helper()
	model := os.Getenv("SB_EVAL_MODEL")
	if model == "" {
		t.Skip("set SB_EVAL_MODEL to the model id the compatible endpoint serves")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("reading the configuration: %v", err)
	}
	settings := cfg.ProviderForTarget(openaicompat.Name, "generic")
	if settings.BaseURL == "" {
		t.Skip(`no [providers."openaicompat/generic"] base_url is configured`)
	}
	opts := []openaicompat.Option{openaicompat.WithBaseURL(settings.BaseURL)}
	// A local server that wants no key is the ordinary case, so a missing
	// credential is not a reason to skip: the server decides.
	if secret, err := credential.Chain(cfg.AuthFor(openaicompat.Name)).Get(
		context.Background(), credential.Ref{Provider: openaicompat.Name, Account: "generic"}); err == nil {
		opts = append(opts, openaicompat.WithAPIKey(secret.Expose()))
	}
	client, err := openaicompat.New("generic", opts...)
	if err != nil {
		t.Fatalf("assembling the compatible endpoint: %v", err)
	}
	target, err := configuredCompatTarget(cfg, model)
	if err != nil {
		t.Fatalf("resolving the configured compatible target: %v", err)
	}
	return Arm{
		Name:          "pin-compat",
		Target:        target,
		Provider:      client,
		ContextWindow: settings.ContextWindow,
	}
}

// configuredCompatTarget reuses the exact target identity the TUI persisted,
// including its rung-scoped output cap. Reconstructing only provider/surface/
// model here would make a real eval refuse a custom model that the interactive
// session can run, or price a different wire request under the same label.
func configuredCompatTarget(cfg *config.Config, model string) (provider.RouteTarget, error) {
	bare := provider.RouteTarget{Provider: openaicompat.Name, Surface: "generic", ModelID: model}
	if cfg == nil {
		return bare, nil
	}
	var matched *provider.RouteTarget
	for _, tier := range cfg.Tiers {
		targets := append([]provider.RouteTarget{tier.Target}, tier.Fallbacks...)
		for _, target := range targets {
			if target.Provider != bare.Provider || target.Surface != bare.Surface || target.ModelID != model {
				continue
			}
			if matched != nil && matched.ID() != target.ID() {
				return provider.RouteTarget{}, fmt.Errorf(
					"model %q has more than one configured max_output; pin an unambiguous target", model)
			}
			copy := target
			matched = &copy
		}
	}
	if matched != nil {
		return *matched, nil
	}
	return bare, nil
}

func TestConfiguredCompatTargetKeepsRungCapAndSurfaceContext(t *testing.T) {
	target := provider.RouteTarget{
		Provider: openaicompat.Name, Surface: "generic", ModelID: "custom-eval-model",
		Params: provider.Params{MaxOutputTokens: 4096},
	}
	cfg := &config.Config{
		Tiers: []config.Tier{{ID: "t1", Target: target}},
		Providers: map[string]config.ProviderSettings{
			"openaicompat/generic": {ContextWindow: 32_768},
		},
	}
	got, err := configuredCompatTarget(cfg, target.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID() != target.ID() || got.Params.MaxOutputTokens != 4096 {
		t.Fatalf("configured compatible target = %+v, want exact capped target %+v", got, target)
	}
	if window := cfg.ProviderForTarget(got.Provider, got.Surface).ContextWindow; window != 32_768 {
		t.Fatalf("configured compatible context = %d, want 32768", window)
	}

	conflicting := target
	conflicting.Params.MaxOutputTokens = 2048
	cfg.Tiers = append(cfg.Tiers, config.Tier{ID: "t2", Target: conflicting})
	if _, err := configuredCompatTarget(cfg, target.ModelID); err == nil || !strings.Contains(err.Error(), "more than one configured max_output") {
		t.Fatalf("ambiguous configured cap error = %v", err)
	}
}

func armsFor(t *testing.T) []Arm {
	t.Helper()

	key := func(provider, surface string) string {
		secret, err := credential.Chain(credential.Settings{}).Get(
			context.Background(), credential.Ref{Provider: provider, Account: surface})
		if err != nil {
			t.Skipf("no credential for %s/%s: %v", provider, surface, err)
		}
		return secret.Expose()
	}

	switch os.Getenv("SB_EVAL_LADDER") {
	case "cache":
		// §7.1's second condition, and the cheapest experiment that tests the
		// thesis this project is named for: one model, one corpus, and the only
		// difference is whether §6 places markers at all.
		client := anthropic.New(anthropic.WithAPIKey(key(anthropic.Name, anthropic.Surface)))
		return []Arm{
			{Name: "cache-unaware", Target: anthropic.Target("claude-haiku-4-5"), Provider: client},
			{Name: "cache-aware", Target: anthropic.Target("claude-haiku-4-5"), Provider: client, CacheAware: true},
		}

	case "money":
		// Two paid models on one provider, which §19.3 allows explicitly and
		// which is the only shape where a cost comparison is about money.
		client := anthropic.New(anthropic.WithAPIKey(key(anthropic.Name, anthropic.Surface)))
		return []Arm{
			{Name: "always-lowest", Target: anthropic.Target("claude-haiku-4-5"), Provider: client},
			{Name: "always-highest", Target: anthropic.Target("claude-opus-5"), Provider: client},
		}

	case "local":
		// A genuinely free ladder. It is also slow: the local model serialises
		// on one machine and an attempt takes minutes, so a corpus this size
		// does not finish in an afternoon.
		return []Arm{
			{Name: "always-lowest", Target: ollama.Target("qwen3.5:9b-mlx"), Provider: ollama.New()},
			{Name: "always-highest", Target: kimi.Target("k3-256k"), Provider: kimi.New(key(kimi.Name, kimi.Surface))},
		}

	case "pin":
		// One candidate, pinned, alone: the §8.6 baseline unit. The front is
		// derived from every journal in hand, so a new candidate is measured
		// by itself rather than by re-running rungs already on record. No
		// routed arm accompanies it — routing over one rung measures nothing.
		switch ref := os.Getenv("SB_EVAL_PIN"); ref {
		case "openai/gpt-5.6-sol":
			client := openai.NewResponses(
				openai.WithResponsesToken(key(openai.Name, openai.Subscription)))
			return []Arm{{
				Name:     "pin-gpt-5.6-sol",
				Target:   provider.RouteTarget{Provider: openai.Name, Surface: openai.Subscription, ModelID: "gpt-5.6-sol"},
				Provider: client,
			}}
		case "ollama/qwen3.8:27b-mlx":
			return []Arm{{
				Name:     "pin-qwen3.8-27b",
				Target:   ollama.Target("qwen3.8:27b-mlx"),
				Provider: ollama.New(),
			}}
		case "openaicompat/generic":
			// The deployment this harness could not measure. A compatible
			// endpoint is the shape a local ladder actually takes now, and
			// its address and model are the user's rather than anything the
			// harness could name, so both come from the configuration the
			// session itself reads.
			return []Arm{compatArm(t)}
		default:
			t.Skipf("SB_EVAL_PIN=%q names no candidate this harness knows", ref)
			return nil
		}

	default:
		// Two plan-metered models on one provider, which is the ladder that can
		// actually be run: both rungs are fast and neither bills per token.
		//
		// That last part is the limit. Nothing here bills per token, so §7.1's
		// cost condition cannot be measured on this ladder at all. What it does
		// measure is everything else §8.6 asks for: solve rate, latency per
		// solved task, and whether escalation recovers what the cheap rung
		// misses.
		client := kimi.New(key(kimi.Name, kimi.Surface))
		return []Arm{
			{Name: "always-lowest", Target: kimi.Target("kimi-for-coding-highspeed"), Provider: client},
			{Name: "always-highest", Target: kimi.Target("k3-256k"), Provider: client},
		}
	}
}

// meteringNote reports what the chosen ladder can and cannot establish.
//
// A plan-metered ladder makes every arm cost zero, so the router "wins" on cost
// by construction. Reporting that as a saving would be the confident wrong
// number §8.6 exists to prevent.
func meteringNote(cat *catalog.Catalog, arms []Arm) string {
	paid := 0
	for _, arm := range arms {
		if info, _, ok := cat.Lookup(arm.Target); ok && info.Metering == catalog.PerToken {
			paid++
		}
	}
	if paid < 2 {
		return "this ladder is free or plan-metered, so every arm bills zero and §7.1's cost " +
			"condition is degenerate; only the solve-rate half is being measured here"
	}
	return ""
}

// TestLiveBaselineRuns is the baseline half of §8.6: pinned targets across the
// corpus, which is what "appropriate tier" labels are later derived from. It is
// scoped by SB_EVAL_TASKS because the full corpus on every arm is hours and
// real money.
func TestLiveBaselineRuns(t *testing.T) {
	if os.Getenv("SB_LIVE") == "" {
		t.Skip("set SB_LIVE=1 to run the corpus against live targets (this spends money)")
	}

	cat, err := catalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}

	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tasks := Tier1(root + "/../..")

	// A subset by default. The gate refuses below the floor, which is the
	// correct outcome for a partial run and the reason a partial run cannot be
	// mistaken for a measurement.
	tasks = selectTasks(t, tasks, 3)

	seeds := positiveIntEnv(t, "SB_EVAL_SEEDS", 1)

	runner := Runner{Catalog: cat, Timeout: 8 * time.Minute}
	ctx := context.Background()
	workers := positiveIntEnv(t, "SB_EVAL_WORKERS", 4)

	arms := armsFor(t)
	routed := RoutedArmFor{Catalog: cat, Ladder: arms}
	pinOnly := os.Getenv("SB_EVAL_LADDER") == "pin"
	expectedArms := expectedArmNames(arms, pinOnly)
	expectedTargets := expectedTargetIDs(arms)
	pins := Pins{
		HarnessCommit:   os.Getenv("SB_EVAL_COMMIT"),
		CatalogRevision: cat.Revision,
		PromptVersion:   "v1",
		Snapshots:       snapshotsFromEnv(t),
	}
	if missing := pins.complete(expectedTargets); len(missing) > 0 {
		t.Fatalf("refusing to spend on an unpinned evaluation: no %s recorded", joinWords(missing))
	}
	replicates := make([]int, seeds)
	for replicate := range seeds {
		replicates[replicate] = replicate
	}
	armTargets := map[string][]provider.RouteTargetID{}
	for _, arm := range arms {
		armTargets[arm.Name] = []provider.RouteTargetID{arm.Target.ID()}
	}
	evalID := evaluationID(tasks, expectedArms, armTargets, expectedTargets, replicates, workers, pins)
	if note := meteringNote(cat, arms); note != "" {
		t.Logf("note: %s", note)
	}

	log := func(got Run, arm string) {
		status := "solved"
		if !got.Solved {
			status = "unsolved"
		}
		denials := ""
		if got.Denials > 0 {
			denials = fmt.Sprintf(" %dd", got.Denials)
		}
		moves := ""
		if got.Escalations > 0 {
			moves = fmt.Sprintf(" %d^", got.Escalations)
		}
		t.Logf("%-28s %-16s %-8s %9s %6.1fs%s%s  %s",
			got.TaskID, arm, status, got.Cost, got.Duration.Seconds(), moves, denials, firstLine(got.Detail))
	}

	// Attempts are independent by construction: each gets its own copy of the
	// repository, its own session, and its own sandbox check. Running them
	// concurrently is what makes a corpus this size finishable, since the full
	// gate is a few hundred attempts of several minutes each.
	// Results are durable as they happen. A run this long dies to a deadline
	// often enough that holding them in memory loses the whole measurement. A
	// clean partial journal is also the resume index: completed cells are not
	// paid for twice. Duplicate cells already present remain in the evidence so
	// the gate can refuse them rather than silently choosing a winner.
	journalPath := os.Getenv("SB_EVAL_JOURNAL")
	if journalPath == "" {
		journalPath = "eval-runs.jsonl"
	}
	var runs []Run
	if recorded, err := ReadJournal(journalPath); err == nil {
		runs = recorded
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for i, run := range runs {
		if run.EvaluationID != evalID {
			t.Fatalf("journal row %d belongs to evaluation %q, not %q; use a clean journal path",
				i+1, run.EvaluationID, evalID)
		}
	}
	done := Done(runs)

	journal, err := NewJournal(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	t.Logf("recording each attempt to %s as it finishes", journalPath)

	// Interleaved by task rather than grouped by arm. A run this long is
	// frequently cut short, and grouping means the first arm completes while the
	// others have nothing at all: a partial result covering one arm answers no
	// comparison. Interleaved, whatever finishes covers every arm evenly.
	type job struct {
		run func() Run
	}
	var jobs []job
	for _, task := range tasks {
		for seed := range seeds {
			for _, arm := range arms {
				key := attemptKey(arm.Name, task.ID, seed)
				if !done[key] {
					jobs = append(jobs, job{run: func() Run { return runner.Run(ctx, task, arm, seed) }})
				}
			}
			if !pinOnly {
				key := attemptKey(RoutedArm, task.ID, seed)
				if !done[key] {
					jobs = append(jobs, job{run: func() Run { return routed.Run(ctx, runner, task, seed) }})
				}
			}
		}
	}
	t.Logf("resuming %d recorded attempt(s); %d remain", len(runs), len(jobs))

	var mu sync.Mutex
	var wg sync.WaitGroup
	queue := make(chan job)

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range queue {
				got := j.run()
				got.EvaluationID = evalID
				if err := journal.Append(got); err != nil {
					t.Errorf("recording an attempt failed: %v", err)
				}
				mu.Lock()
				runs = append(runs, got)
				log(got, got.Arm)
				mu.Unlock()
			}
		}()
	}
	for _, j := range jobs {
		queue <- j
	}
	close(queue)
	wg.Wait()

	sort.Slice(runs, func(i, j int) bool {
		if runs[i].Arm != runs[j].Arm {
			return runs[i].Arm < runs[j].Arm
		}
		return runs[i].TaskID < runs[j].TaskID
	})

	// A router that always picks the same rung is a baseline under another
	// name, and a comparison against it would be measuring nothing.
	used := TargetsUsed(runs)
	moved, totalRouted := Escalations(runs)
	t.Logf("the routed arm ended on: %v", used)
	t.Logf("escalated on %d of %d routed runs", moved, totalRouted)
	if moved == 0 && len(arms) > 1 {
		t.Logf("note: no routed run ever changed target, so this arm is a fixed baseline " +
			"under another name and the comparison answers nothing")
	}

	report(t, runs)

	// The gate must refuse a partial corpus rather than report a number.
	v := Gate{
		ExpectedArms:        expectedArms,
		ExpectedTargets:     expectedTargets,
		RequireEvaluationID: true,
		EvaluationWorkers:   workers,
	}.Evaluate(tasks, runs, pins)
	if len(tasks) < MinimumTier1Tasks && !v.Refused {
		t.Errorf("a %d task run produced a verdict rather than refusing", len(tasks))
	}
	for _, reason := range v.Reasons {
		t.Logf("gate: %s", reason)
	}
}

func selectTasks(t *testing.T, tasks []Task, defaultLimit int) []Task {
	t.Helper()
	limit := positiveIntEnv(t, "SB_EVAL_TASKS", defaultLimit)
	if limit > len(tasks) {
		limit = len(tasks)
	}
	return tasks[:limit]
}

func positiveIntEnv(t *testing.T, name string, fallback int) int {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		t.Fatalf("%s must be a positive integer, got %q", name, raw)
	}
	return value
}

func expectedArmNames(arms []Arm, pinOnly bool) []string {
	out := make([]string, 0, len(arms)+1)
	for _, arm := range arms {
		out = append(out, arm.Name)
	}
	if !pinOnly {
		out = append(out, RoutedArm)
	}
	return out
}

func expectedTargetIDs(arms []Arm) []provider.RouteTargetID {
	var out []provider.RouteTargetID
	for _, arm := range arms {
		out = append(out, arm.Target.ID())
		for _, fallback := range arm.Fallbacks {
			out = append(out, fallback.Target.ID())
		}
	}
	return out
}

func snapshotsFromEnv(t *testing.T) map[provider.RouteTargetID]string {
	t.Helper()
	raw := os.Getenv("SB_EVAL_SNAPSHOTS")
	if raw == "" {
		return nil
	}
	var snapshots map[provider.RouteTargetID]string
	if err := json.Unmarshal([]byte(raw), &snapshots); err != nil {
		t.Fatalf("SB_EVAL_SNAPSHOTS is not a JSON target-to-snapshot object: %v", err)
	}
	return snapshots
}

func report(t *testing.T, runs []Run) {
	t.Helper()

	arms := map[string]bool{}
	for _, r := range runs {
		arms[r.Arm] = true
	}
	names := make([]string, 0, len(arms))
	for a := range arms {
		names = append(names, a)
	}
	sort.Strings(names)

	t.Log("")
	for _, name := range names {
		res := Summarize(name, runs)
		interval := CostInterval(runs, name)
		t.Logf("%-16s solved %d/%d  median cost %s  spread %s to %s  median %s",
			name, res.Solved, res.Runs, res.MedianCostPerSolved,
			interval.Low, interval.High, res.MedianLatency.Round(time.Second))
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 70 {
		s = s[:70] + "..."
	}
	return s
}
