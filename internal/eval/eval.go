// Package eval is the harness the router is measured with.
//
// §8.6 says built early rather than late, because it is the difference between
// the router being a product and being a demo. It also sets the rule this
// package enforces above all others: the harness does not ship before the tier-1
// corpus is populated, since an eval harness with an empty corpus produces
// confident numbers about nothing, and those numbers get quoted.
//
// So a verdict is refused rather than approximated when the corpus is too small.
// That refusal is the feature. Everything else here is arithmetic over runs, and
// arithmetic over four tasks looks exactly like arithmetic over forty.
package eval

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/provider"
)

// Provenance is where a task came from, which decides how much its result is
// worth. §8.6 keeps the three separate because they carry different trust.
type Provenance int

const (
	// HandWritten is tier 1: authored from repositories whose ground truth the
	// author can establish directly. Small and uncontaminated, and the only
	// tier the gate measurement depends on.
	HandWritten Provenance = 1

	// FromPullRequest is tier 2: extracted from merged public pull requests
	// where the suite fails at the parent and passes at the merge. Credible at
	// volume, and contaminated by any model whose training predates the PR,
	// which is why its results are reported separately and annotated with each
	// target's cutoff.
	FromPullRequest Provenance = 2

	// Synthetic is tier 3: volume on top of the other two. It validates the
	// harness rather than the models, because the generator's notion of
	// difficulty is the thing under test.
	Synthetic Provenance = 3
)

func (p Provenance) String() string {
	switch p {
	case HandWritten:
		return "hand-written"
	case FromPullRequest:
		return "from a pull request"
	case Synthetic:
		return "synthetic"
	}
	return "unknown"
}

// MinimumTier1Tasks is the floor §8.6 sets at twenty to thirty tasks. Below it
// no verdict is produced.
const MinimumTier1Tasks = 20

// Task is one unit of work with an executable verifier.
//
// Verify is a function rather than a description because §8.6 wants independent
// verification: a harness that asks the model whether it succeeded is measuring
// the model's opinion of itself.
type Task struct {
	ID         string
	Provenance Provenance
	Prompt     string

	// Setup materialises the workspace. It runs fresh per attempt, so one
	// attempt cannot see another's edits.
	Setup func(dir string) error

	// Verify decides whether the task was solved, and says why when it was not.
	// It shares the attempt deadline: a verifier is part of the measurement,
	// not unbounded cleanup after the measured turn.
	Verify func(ctx context.Context, dir string) (solved bool, detail string, err error)
}

// Run is one attempt.
type Run struct {
	TaskID     string
	Provenance Provenance
	Target     provider.RouteTargetID

	// Arm names what produced this run: a fixed target used as a baseline, or
	// the router. §7.1 compares against the best fixed-target baseline, so the
	// arms have to stay distinguishable.
	Arm string

	Solved bool
	Detail string
	// Failure gives unsolved attempts a machine-readable category. It is
	// optional so journals written before the field existed remain readable;
	// FailureKind derives the category from their Detail text.
	Failure  FailureKind `json:",omitempty"`
	Cost     catalog.Money
	Usage    provider.Usage
	Duration time.Duration

	// EstimatedCost is what the model predicted before the run, which §7.1
	// requires be reported against the actual per target. EstimatedTarget names
	// the target that estimate priced. An opening estimate is deliberately kept
	// after an escalation for diagnosis, but it is not compared with spend from
	// a different target (or a run that touched multiple targets).
	EstimatedCost   catalog.Money
	EstimatedTarget provider.RouteTargetID `json:",omitempty"`

	Escalations int

	// Denials counts approval requests the harness refused. A run with many is
	// a run that spent its rounds asking for things policy will never grant.
	Denials int

	// Rounds is how many model calls the attempt spent. A change that claims
	// to shorten exploration has to move this, and solve rate cannot see it:
	// a saturated corpus reports the same score whether a task took four
	// rounds or thirty. Optional so journals written before it stay readable.
	Rounds int `json:",omitempty"`

	// ToolErrors counts failed tool calls, keyed by tool and a coarse class of
	// failure. A change that claims to remove a malformed-call class has to
	// drive its own key down, and a key per class is what separates "exec was
	// called wrongly" from "the command exec ran exited non-zero" — those are
	// the same tool and opposite findings.
	ToolErrors map[string]int `json:",omitempty"`

	Seed int

	// EvaluationID binds a journal row to the exact harness, corpus, arms and
	// fixed-arm targets, replicates, worker concurrency, prompt, catalog, and
	// model snapshots that produced it. Older journals remain readable, but a
	// strict gate refuses to relabel their rows with later pins.
	EvaluationID string `json:",omitempty"`
}

// Pins are what makes a report reproducible. §8.6 requires every one of these,
// because a number without them cannot be compared to a later number.
type Pins struct {
	HarnessCommit   string
	CatalogRevision string
	PromptVersion   string

	// Snapshots maps each target to the dated model it actually resolved to. An
	// alias moves; a report that recorded only the alias describes a
	// measurement nobody can repeat.
	Snapshots map[provider.RouteTargetID]string
}

func (p Pins) complete(targets []provider.RouteTargetID) []string {
	var missing []string
	if p.HarnessCommit == "" {
		missing = append(missing, "harness commit")
	}
	if p.CatalogRevision == "" {
		missing = append(missing, "catalog revision")
	}
	if p.PromptVersion == "" {
		missing = append(missing, "prompt version")
	}
	if len(targets) == 0 && len(p.Snapshots) == 0 {
		missing = append(missing, "model snapshots")
	}
	uniqueTargets := map[provider.RouteTargetID]bool{}
	for _, target := range targets {
		if target != "" {
			uniqueTargets[target] = true
		}
	}
	for _, target := range sortedTargets(uniqueTargets) {
		if p.Snapshots[target] == "" {
			missing = append(missing, fmt.Sprintf("model snapshot for %s", target))
		}
	}
	return missing
}

// ArmResult is one arm's performance over a corpus.
type ArmResult struct {
	Arm   string
	Runs  int
	Tasks int

	Solved    int
	SolveRate float64

	// MedianCostPerSolved is the §7.1 figure. It is per *solved* task on
	// purpose: cost per attempt rewards an arm that fails cheaply, which is the
	// opposite of what is being bought.
	MedianCostPerSolved catalog.Money
	MedianLatency       time.Duration

	// EstimateError is actual over estimated, per §7.1's requirement that this
	// be reported by target with no systematic underestimation.
	EstimateError map[provider.RouteTargetID]float64

	// EstimatesUnavailable counts runs whose only estimate was for an opening
	// target that the run later left. Mixing that estimate with the final
	// target's actual spend would manufacture an error ratio for neither model.
	EstimatesUnavailable int
}

// Summarize reduces runs to one arm's result.
func Summarize(arm string, runs []Run) ArmResult {
	res := ArmResult{Arm: arm, EstimateError: map[provider.RouteTargetID]float64{}}

	tasks := map[string]bool{}
	var solvedCosts []catalog.Money
	var solvedLatency []time.Duration
	estimated := map[provider.RouteTargetID][2]float64{}

	for _, r := range runs {
		if r.Arm != arm {
			continue
		}
		if !modelQualityOutcome(r) {
			continue
		}
		res.Runs++
		tasks[r.TaskID] = true
		if r.Solved {
			res.Solved++
			solvedCosts = append(solvedCosts, r.Cost)
			solvedLatency = append(solvedLatency, r.Duration)
		}
		if r.EstimatedCost > 0 {
			if !comparableEstimate(r) {
				res.EstimatesUnavailable++
				continue
			}
			target := r.EstimatedTarget
			if target == "" {
				// Legacy fixed-arm and non-escalating routed journals predate
				// EstimatedTarget. Their one observed target is still like-for-like.
				target = r.Target
			}
			acc := estimated[target]
			estimated[target] = [2]float64{acc[0] + float64(r.Cost), acc[1] + float64(r.EstimatedCost)}
		}
	}

	res.Tasks = len(tasks)
	if res.Runs > 0 {
		res.SolveRate = float64(res.Solved) / float64(res.Runs)
	}
	res.MedianCostPerSolved = medianMoney(solvedCosts)
	res.MedianLatency = medianDuration(solvedLatency)

	for target, acc := range estimated {
		if acc[1] > 0 {
			res.EstimateError[target] = acc[0] / acc[1]
		}
	}
	return res
}

// comparableEstimate is intentionally stricter than final-target equality. A
// run that went A -> B -> A still includes B spend, so A's opening estimate is
// not like-for-like even though the final target happens to be A again.
func comparableEstimate(r Run) bool {
	if r.Escalations > 0 {
		return false
	}
	return r.EstimatedTarget == "" || r.EstimatedTarget == r.Target
}

func medianMoney(values []catalog.Money) catalog.Money {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]catalog.Money(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

func medianDuration(values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

// Gate is the §7.1 falsification gate.
//
// The thresholds are provisional product targets and §7.1 is explicit that they
// must be changed before seeing decisive results rather than after, which is why
// they are fields with documented defaults rather than constants buried in the
// comparison.
type Gate struct {
	// MinCostReduction against the best fixed-target baseline.
	MinCostReduction float64

	// MaxSolveRateDrop in absolute percentage points.
	MaxSolveRateDrop float64

	// MinRunsPerTask is how many seeds are needed before a median means
	// anything. §8.6 asks for results over multiple runs with uncertainty
	// intervals.
	MinRunsPerTask int

	// ExpectedArms and ExpectedTargets let a live run state the configuration
	// that produced it. When omitted, a journal report derives them from the
	// recorded runs, but every observed arm still has to fill the same matrix.
	ExpectedArms    []string
	ExpectedTargets []provider.RouteTargetID

	// RequireEvaluationID makes the gate prove every row belongs to the exact
	// configuration being evaluated. Live and journal-report harnesses enable
	// it; the opt-in keeps the library API compatible with older callers that
	// construct Run values directly.
	RequireEvaluationID bool

	// EvaluationWorkers is part of the evaluation identity because provider
	// throttling changes with concurrency. It must be positive when strict
	// identity checking is enabled.
	EvaluationWorkers int
}

const (
	DefaultMinCostReduction = 0.20
	DefaultMaxSolveRateDrop = 0.02
	DefaultMinRunsPerTask   = 3
)

func (g Gate) minCostReduction() float64 {
	if g.MinCostReduction > 0 {
		return g.MinCostReduction
	}
	return DefaultMinCostReduction
}

func (g Gate) maxSolveRateDrop() float64 {
	if g.MaxSolveRateDrop > 0 {
		return g.MaxSolveRateDrop
	}
	return DefaultMaxSolveRateDrop
}

func (g Gate) minRuns() int {
	if g.MinRunsPerTask > 0 {
		return g.MinRunsPerTask
	}
	return DefaultMinRunsPerTask
}

// Verdict is the gate's answer.
//
// Refused is separate from Passed and Failed on purpose. A gate that cannot be
// measured has not been passed and has not been failed, and collapsing those
// into one boolean is how an unmeasured gate gets reported as a green one.
type Verdict struct {
	Passed  bool
	Refused bool

	// Reasons explains the verdict in full. A gate reported as a single word is
	// a gate nobody can argue with, which §0 of the design asks for the
	// opposite of.
	Reasons []string

	Routed   ArmResult
	Baseline ArmResult

	CostReduction float64
	SolveRateDrop float64
}

// Evaluate runs the gate over a corpus of runs.
//
// It refuses before it compares. §8.6's rule about an unpopulated corpus is not
// advice: numbers computed over four tasks are indistinguishable in shape from
// numbers over forty, and only one of them means anything.
func (g Gate) Evaluate(tasks []Task, runs []Run, pins Pins) Verdict {
	var v Verdict

	tier1 := 0
	for _, t := range tasks {
		if t.Provenance == HandWritten {
			tier1++
		}
	}
	if tier1 < MinimumTier1Tasks {
		v.Refused = true
		v.Reasons = append(v.Reasons, fmt.Sprintf(
			"the corpus has %d hand-written tasks and the gate needs %d; "+
				"§8.6 keeps the harness from shipping before this is populated, because confident numbers "+
				"about an empty corpus are worse than no numbers at all",
			tier1, MinimumTier1Tasks))
	}

	evidence := g.validateMatrix(tasks, runs)
	eligible := evidence.Runs
	if len(evidence.Reasons) > 0 {
		v.Refused = true
		v.Reasons = append(v.Reasons, evidence.Reasons...)
	}
	if missing := pins.complete(evidence.Targets); len(missing) > 0 {
		v.Refused = true
		v.Reasons = append(v.Reasons, fmt.Sprintf(
			"the report is not reproducible: no %s recorded", joinWords(missing)))
	}
	if g.RequireEvaluationID {
		if g.EvaluationWorkers < 1 {
			v.Refused = true
			v.Reasons = append(v.Reasons,
				"the evaluation identity does not record a positive worker count")
		}
		expected := evaluationID(
			tasks, evidence.Arms, evidence.ArmTargets, evidence.Targets, evidence.Replicates,
			g.EvaluationWorkers, pins)
		missing, mismatched := 0, 0
		for _, run := range eligible {
			switch run.EvaluationID {
			case "":
				missing++
			case expected:
			default:
				mismatched++
			}
		}
		if missing > 0 || mismatched > 0 {
			v.Refused = true
			v.Reasons = append(v.Reasons, fmt.Sprintf(
				"journal rows are not bound to this evaluation configuration: %d missing identity, %d mismatched",
				missing, mismatched))
		}
	}

	arms := map[string]bool{}
	for _, arm := range evidence.Arms {
		arms[arm] = true
	}
	if !arms[RoutedArm] {
		v.Refused = true
		v.Reasons = append(v.Reasons, "no routed runs to compare")
	}

	// The best fixed-target baseline is the one that beats the router hardest,
	// which is the comparison §7.1 asks for: an improvement over the worst
	// alternative is not an improvement.
	v.Routed = Summarize(RoutedArm, eligible)
	best := ArmResult{}
	for arm := range arms {
		if arm == RoutedArm {
			continue
		}
		candidate := Summarize(arm, eligible)
		if candidate.Solved == 0 {
			continue
		}
		// Cheapest wins, and a tie is broken by solve rate rather than by map
		// order. On a ladder where every arm bills the same -- which is every
		// plan-metered ladder -- the cost comparison never separates them, and
		// leaving it there picked whichever arm the map yielded first. That
		// chose a baseline solving 58% over one solving 97% and reported the
		// routed arm as ahead when it was well behind.
		//
		// "Best" has to mean the strongest competitor. Beating a weak one is
		// not the claim §7.1 is asking about.
		switch {
		case best.Arm == "":
			best = candidate
		case candidate.MedianCostPerSolved < best.MedianCostPerSolved:
			best = candidate
		case candidate.MedianCostPerSolved == best.MedianCostPerSolved &&
			candidate.SolveRate > best.SolveRate:
			best = candidate
		}
	}
	if best.Arm == "" {
		v.Refused = true
		v.Reasons = append(v.Reasons, "no fixed-target baseline solved anything to compare against")
	}
	v.Baseline = best

	if v.Refused {
		v.Reasons = append(v.Reasons,
			"refused rather than failed: this gate has not been measured, which is not the same as not having been met")
		return v
	}

	if best.MedianCostPerSolved > 0 {
		v.CostReduction = 1 - float64(v.Routed.MedianCostPerSolved)/float64(best.MedianCostPerSolved)
	}
	v.SolveRateDrop = best.SolveRate - v.Routed.SolveRate

	costOK := v.CostReduction >= g.minCostReduction()
	safetyOK := v.SolveRateDrop <= g.maxSolveRateDrop()

	if costOK {
		v.Reasons = append(v.Reasons, fmt.Sprintf(
			"median cost per verified solved task fell %.1f%% against %s, clearing the %.0f%% threshold",
			v.CostReduction*100, best.Arm, g.minCostReduction()*100))
	} else {
		v.Reasons = append(v.Reasons, fmt.Sprintf(
			"median cost per verified solved task moved %.1f%% against %s, short of the %.0f%% the gate requires",
			v.CostReduction*100, best.Arm, g.minCostReduction()*100))
	}

	if safetyOK {
		if v.SolveRateDrop < 0 {
			v.Reasons = append(v.Reasons, fmt.Sprintf(
				"verified solve rate rose %.1f points against %s", -v.SolveRateDrop*100, best.Arm))
		} else {
			v.Reasons = append(v.Reasons, fmt.Sprintf(
				"verified solve rate fell %.1f points, inside the %.0f point allowance",
				v.SolveRateDrop*100, g.maxSolveRateDrop()*100))
		}
	} else {
		v.Reasons = append(v.Reasons, fmt.Sprintf(
			"verified solve rate fell %.1f points, past the %.0f point allowance",
			v.SolveRateDrop*100, g.maxSolveRateDrop()*100))
	}

	// §7.1 requires estimate-versus-actual by target with no systematic
	// underestimation, because a router that under-predicts spend can pass a
	// cost gate while overrunning a budget.
	for target, ratio := range v.Routed.EstimateError {
		if ratio > 1.05 {
			safetyOK = false
			v.Reasons = append(v.Reasons, fmt.Sprintf(
				"%s cost %.0f%% more than estimated, which is systematic underestimation and fails the gate regardless of the saving",
				target, (ratio-1)*100))
		}
	}
	if v.Routed.EstimatesUnavailable > 0 {
		v.Reasons = append(v.Reasons, fmt.Sprintf(
			"%d routed estimate(s) are unavailable for reconciliation because the run changed targets; opening estimates were not attributed to another target's actual spend",
			v.Routed.EstimatesUnavailable))
	}

	v.Passed = costOK && safetyOK
	return v
}

// RoutedArm is the arm under test. The baselines are named by the caller, since
// "always lowest" and "always highest" depend on the ladder.
const RoutedArm = "routed"

func joinWords(words []string) string {
	switch len(words) {
	case 0:
		return ""
	case 1:
		return words[0]
	case 2:
		return words[0] + " or " + words[1]
	}
	return words[0] + ", " + joinWords(words[1:])
}

// Interval is a simple uncertainty interval over a sample, reported because
// §8.6 requires one and a bare median invites over-reading a difference that a
// second run would erase.
type Interval struct {
	Median catalog.Money
	Low    catalog.Money
	High   catalog.Money
	N      int
}

// CostInterval reports the median with a bootstrap-free spread: the lowest and
// highest observed. With the handful of seeds §8.6 asks for, a percentile
// interval would be a confident-sounding restatement of the range.
func CostInterval(runs []Run, arm string) Interval {
	var costs []catalog.Money
	for _, r := range runs {
		if r.Arm == arm && r.Solved && modelQualityOutcome(r) {
			costs = append(costs, r.Cost)
		}
	}
	if len(costs) == 0 {
		return Interval{}
	}
	sort.Slice(costs, func(i, j int) bool { return costs[i] < costs[j] })
	return Interval{
		Median: costs[len(costs)/2],
		Low:    costs[0],
		High:   costs[len(costs)-1],
		N:      len(costs),
	}
}

// Overlaps reports whether two intervals overlap, which is the honest way to
// say a difference has not been established at this sample size.
func (i Interval) Overlaps(o Interval) bool {
	return math.Max(float64(i.Low), float64(o.Low)) <= math.Min(float64(i.High), float64(o.High))
}
