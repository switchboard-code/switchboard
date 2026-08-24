package eval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/rootedfs"
	"github.com/switchboard-code/switchboard/internal/safeexec"
)

// The tier-1 corpus.
//
// §8.6 wants twenty to thirty hand-written tasks from the author's own
// repositories, each with an executable verifier, small enough that ground truth
// is established directly and uncontaminated by any model's training. This
// repository qualifies on every count: it did not exist before this work, so no
// target has seen it.
//
// Each task breaks something real and asks for it back. The verifier is the
// repository's own test suite scoped to the affected package, which is as
// independent as a verifier gets: it was written before the task and does not
// know a task exists. Nothing here asks a model whether it succeeded.
//
// The tasks are ordered roughly by the breadth they demand, because that is the
// axis the router is being asked to read. They are not labelled with an
// "appropriate tier": §8.6 says those labels are derived by running pinned
// targets and finding the Pareto front, not assigned from reputation.

// breakage is a single edit that makes a package fail its own tests.
type breakage struct {
	file string
	old  string
	new  string
}

type spec struct {
	id     string
	prompt string
	pkg    string
	breaks []breakage
	// goos constrains the task to one platform: a breakage to a file the
	// running platform never compiles cannot fail its verifier there.
	goos string
	// mustContain re-checks a property the test suite alone would not catch, so
	// a task cannot be solved by deleting the test.
	mustContain map[string]string
}

// maxVerifierOutputBytes is the complete output budget for one corpus
// verifier. The tail is retained because go test normally reports the useful
// failure last, while Write continues accepting bytes so a noisy test cannot
// deadlock on a full pipe.
const maxVerifierOutputBytes = 1 << 20

const maxVerifierSourceBytes = 4 << 20

type verifierOutput struct {
	mu        sync.Mutex
	tail      []byte
	truncated bool
}

func (o *verifierOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := len(p)
	if len(p) >= maxVerifierOutputBytes {
		previous := len(o.tail)
		o.tail = append(o.tail[:0], p[len(p)-maxVerifierOutputBytes:]...)
		o.truncated = o.truncated || previous > 0 || len(p) > maxVerifierOutputBytes
		return n, nil
	}
	if overflow := len(o.tail) + len(p) - maxVerifierOutputBytes; overflow > 0 {
		copy(o.tail, o.tail[overflow:])
		o.tail = o.tail[:len(o.tail)-overflow]
		o.truncated = true
	}
	o.tail = append(o.tail, p...)
	return n, nil
}

func (o *verifierOutput) String() (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return string(o.tail), o.truncated
}

// specs are the corpus. Each is a real invariant this codebase depends on, and
// the comment on each is why breaking it is a meaningful thing to ask for back.
var specs = []spec{
	{
		id:     "money-precision",
		prompt: "Money is failing to render sub-cent amounts correctly. Fix it so the tests pass.",
		pkg:    "./internal/catalog/",
		breaks: []breakage{{"internal/catalog/catalog.go", "func (m Money) String() string {", "func (m Money) String() string {\n\tif true {\n\t\treturn fmt.Sprintf(\"$%.2f\", float64(m)/1e6)\n\t}"}},
	},
	{
		id:     "tier-order",
		prompt: "Tiers are coming back in the wrong order. Fix it so the tests pass.",
		pkg:    "./internal/config/",
		breaks: []breakage{{"internal/config/config.go", "return a < b", "return ids[i] < ids[j]"}},
	},
	{
		id:     "cache-read-subset",
		prompt: "The OpenAI-compatible adapter is double-counting cached prompt tokens. Fix it so the tests pass.",
		pkg:    "./internal/provider/openaicompat/",
		breaks: []breakage{{"internal/provider/openaicompat/stream.go", "s.usage.InputTokens -= d.CachedTokens", "s.usage.InputTokens += d.CachedTokens"}},
	},
	{
		id:     "anthropic-disjoint-usage",
		prompt: "Cache usage from the Anthropic adapter looks wrong. Fix it so the tests pass.",
		pkg:    "./internal/provider/anthropic/",
		breaks: []breakage{{"internal/provider/anthropic/stream.go", "s.usage.CacheWriteTokens = u.CacheCreationInputTokens", "s.usage.CacheWriteTokens = 0"}},
	},
	{
		id:     "thinking-signature",
		prompt: "Thinking blocks are losing their signature somewhere between the stream and the assembled message. Fix it so the tests pass.",
		pkg:    "./internal/provider/anthropic/",
		breaks: []breakage{{"internal/provider/anthropic/stream.go", `case "signature_delta":`, `case "signature_delta_disabled":`}},
		mustContain: map[string]string{
			"internal/provider/anthropic/stream.go": "signature_delta",
		},
	},
	{
		id:     "tool-role-mapping",
		prompt: "Tool results are being rejected by the Anthropic API. Fix it so the tests pass.",
		pkg:    "./internal/provider/anthropic/",
		breaks: []breakage{{"internal/provider/anthropic/anthropic.go", "case provider.RoleUser, provider.RoleTool:", "case provider.RoleUser:"}},
	},
	{
		id:     "secret-redaction",
		prompt: "A credential is leaking into formatted output. Fix it so the tests pass.",
		pkg:    "./internal/credential/",
		breaks: []breakage{{"internal/credential/credential.go", "func (s Secret) String() string   { return redacted }", "func (s Secret) String() string   { return s.value }"}},
		mustContain: map[string]string{
			"internal/credential/credential.go": "redacted",
		},
	},
	{
		id:     "keychain-argv",
		prompt: "The keychain write is putting the credential somewhere it can be read by other processes. Fix it so the tests pass.",
		pkg:    "./internal/credential/",
		goos:   "darwin",
		breaks: []breakage{{"internal/credential/keychain_darwin.go", `cmd := s.command(ctx, "-i")`, `cmd := s.command(ctx, "add-generic-password", "-s", service(ref), "-a", account(ref), "-U", "-w", value)`}},
	},
	{
		id:     "control-character-injection",
		prompt: "A credential reference can inject a second command into the credential tool. Fix it so the tests pass.",
		pkg:    "./internal/credential/",
		breaks: []breakage{{"internal/credential/credential.go", "return printable(r.Account, \"account\")", "return nil"}},
	},
	{
		id:     "oauth-state-check",
		prompt: "The OAuth callback is accepting redirects that did not come from the flow it started. Fix it so the tests pass.",
		pkg:    "./internal/credential/",
		breaks: []breakage{{"internal/credential/oauth.go", `if q.Get("state") != state {`, `if false {`}},
	},
	{
		id:     "oauth-refresh-rotation",
		prompt: "Sessions are losing their refresh token against servers that do not rotate it. Fix it so the tests pass.",
		pkg:    "./internal/credential/",
		breaks: []breakage{{"internal/credential/oauth.go", "refreshed.RefreshToken = tokens.RefreshToken", "refreshed.RefreshToken = \"\""}},
	},
	{
		id:     "session-ordering",
		prompt: "Resuming the most recent session picks the wrong one when several were created quickly. Fix it so the tests pass.",
		pkg:    "./internal/session/",
		breaks: []breakage{{"internal/session/session.go", "return infos[i].ID > infos[j].ID", "return infos[i].ID < infos[j].ID"}},
	},
	{
		id:     "prefix-seal",
		prompt: "Documents can be inserted into the stable zone after a session has started, which breaks the cached prefix. Fix it so the tests pass.",
		pkg:    "./internal/prefix/",
		breaks: []breakage{{"internal/prefix/prefix.go", "if l.sealed {\n\t\treturn fmt.Errorf(", "if false {\n\t\treturn fmt.Errorf("}},
	},
	{
		id:     "prefix-tail-hash",
		prompt: "Rewriting the volatile tail is changing the prefix hash, so no turn ever hits cache. Fix it so the tests pass.",
		pkg:    "./internal/prefix/",
		breaks: []breakage{{"internal/prefix/prefix.go", "\tfor _, m := range l.history {\n\t\tfmt.Fprintf(h, \"msg\\x00%s\\x00\", m.Role)\n\t\twriteBlocks(h, m.Content)\n\t}", "\tfor _, m := range l.history {\n\t\tfmt.Fprintf(h, \"msg\\x00%s\\x00\", m.Role)\n\t\twriteBlocks(h, m.Content)\n\t}\n\twriteBlocks(h, l.tail)"}},
	},
	{
		id:     "tool-sort-determinism",
		prompt: "Two sessions with the same tools registered in a different order are not sharing a cache. Fix it so the tests pass.",
		pkg:    "./internal/prefix/",
		breaks: []breakage{{"internal/prefix/prefix.go", "sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })", "_ = sorted"}},
	},
	{
		id:     "breakpoint-minimum",
		prompt: "Cache markers are being placed on prefixes too short for the target to cache. Fix it so the tests pass.",
		pkg:    "./internal/breakpoint/",
		breaks: []breakage{{"internal/breakpoint/breakpoint.go", "if m.Policy.MinTokens > 0 && b.TokensBefore < m.Policy.MinTokens {", "if false {"}},
	},
	{
		id:     "breakpoint-automatic",
		prompt: "Markers are being sent to targets that cache automatically and do not take them. Fix it so the tests pass.",
		pkg:    "./internal/breakpoint/",
		breaks: []breakage{{"internal/breakpoint/breakpoint.go", "case catalog.CacheAutomatic, catalog.CacheImplicit:", "case catalog.CacheImplicit:"}},
	},
	{
		id:     "routing-key-scope",
		prompt: "Two different targets are sharing a cache routing key. Fix it so the tests pass.",
		pkg:    "./internal/breakpoint/",
		breaks: []breakage{{"internal/breakpoint/breakpoint.go", `sum := sha256.Sum256([]byte(string(m.Target) + "\x00" + layout.PrefixHash()))`, `sum := sha256.Sum256([]byte(layout.PrefixHash()))`}},
	},
	{
		id:     "cache-silent-target",
		prompt: "A target that reports nothing about caching is being recorded as missing every time. Fix it so the tests pass.",
		pkg:    "./internal/cachestate/",
		breaks: []breakage{{"internal/cachestate/cachestate.go", "if obs.Accounting == catalog.AccountingNone {", "if false {"}},
	},
	{
		id:     "cache-alarm-threshold",
		prompt: "The cache alarm is firing on a single miss, which happens routinely under early eviction. Fix it so the tests pass.",
		pkg:    "./internal/cachestate/",
		breaks: []breakage{{"internal/cachestate/cachestate.go", "if worst != nil && worst.ConsecutiveMiss >= alarmAfter {", "if worst != nil && worst.ConsecutiveMiss >= 1 {"}},
	},
	{
		id:     "cache-write-not-a-hit",
		prompt: "Writing a prefix is being counted as a cache hit. Fix it so the tests pass.",
		pkg:    "./internal/cachestate/",
		breaks: []breakage{{"internal/cachestate/cachestate.go", "\t\tentry.State = WriteObserved\n\t\tentry.LastWriteSeen = obs.At", "\t\tentry.State = WriteObserved\n\t\tentry.LastWriteSeen = obs.At\n\t\tentry.Hits++"}},
	},
	{
		id:     "cost-sunk-write",
		prompt: "Switching targets is charging the whole prior cache write, which double-counts a sunk cost. Fix it so the tests pass.",
		pkg:    "./internal/costmodel/",
		breaks: []breakage{{"internal/costmodel/costmodel.go", "s.LostWarmValue = scaleMoney(fromEstimate.Spread(), clamp(returnProbability)*expiry)", "s.LostWarmValue = fromEstimate.MissCost"}},
	},
	{
		id:     "estimator-widening",
		prompt: "Cost bounds are not accounting for the token estimator's measured bias. Fix it so the tests pass.",
		pkg:    "./internal/costmodel/",
		breaks: []breakage{{"internal/costmodel/costmodel.go", "if !in.TokensAreExact {", "if false {"}},
	},
	{
		id:     "router-feasibility-order",
		prompt: "A target that is not an approved destination is being reported as too expensive. Fix it so the tests pass.",
		pkg:    "./internal/router/",
		breaks: []breakage{{"internal/router/router.go", "case !approved(c.Target.Provider, in.Requirements.ApprovedProviders):", "case false:"}},
	},
	{
		id:     "router-budget-bound",
		prompt: "The budget ceiling is being checked against the expected cost rather than the upper bound. Fix it so the tests pass.",
		pkg:    "./internal/router/",
		breaks: []breakage{{"internal/router/router.go", "case (in.Budgets.MaxCostSet || in.Budgets.MaxCost > 0) && candidateCeiling(c) > in.Budgets.MaxCost:", "case (in.Budgets.MaxCostSet || in.Budgets.MaxCost > 0) && c.Estimate.Expected > in.Budgets.MaxCost:"}},
	},
	{
		id:     "router-pin-error",
		prompt: "A pinned target that cannot serve a turn is being silently replaced with a different one. Fix it so the tests pass.",
		pkg:    "./internal/router/",
		breaks: []breakage{{"internal/router/router.go", "\t\treturn Decision{Infeasible: excluded}, fmt.Errorf(\n\t\t\t\"the pinned target %q cannot serve this turn:\\n  %s\", in.Pin, strings.Join(excluded, \"\\n  \"))", "\t\t_ = in.Pin"}},
	},
	{
		id:     "escalation-hedging",
		prompt: "The router is escalating on model hedging alone, which §8.3 says it must never do. Fix it so the tests pass.",
		pkg:    "./internal/router/",
		breaks: []breakage{{"internal/router/escalation.go", "\tif up > 0 {\n\t\tup += weak\n\t}", "\tup += weak * 4"}},
	},
	{
		id:     "escalation-dwell",
		prompt: "The primary target is switching every other call inside a single turn. Fix it so the tests pass.",
		pkg:    "./internal/router/",
		breaks: []breakage{{"internal/router/escalation.go", "\tif p.MinimumDwell > 0 {\n\t\treturn p.MinimumDwell\n\t}\n\treturn DefaultMinimumDwell", "\treturn 0"}},
	},
	{
		id:     "outcome-labels",
		prompt: "An escalation is being treated as a negative training label, which it is not. Fix it so the tests pass.",
		pkg:    "./internal/router/",
		breaks: []breakage{{"internal/router/escalation.go", "\tcase Escalated:\n\t\treturn Label{Censored: true, Weight: 0,", "\tcase Escalated:\n\t\treturn Label{Negative: true, Weight: 1,"}},
	},
	{
		id:     "detector-signature",
		prompt: "The same test failure is being reported as new on every retry. Fix it so the tests pass.",
		pkg:    "./internal/router/",
		breaks: []breakage{{"internal/router/detect.go", "\tnormalized := strings.Map(func(r rune) rune {\n\t\tif r >= '0' && r <= '9' {\n\t\t\treturn -1\n\t\t}\n\t\treturn r\n\t}, strings.TrimSpace(line))", "\tnormalized := line"}},
	},
}

// Tier1 builds the corpus against a checkout of this repository.
//
// repoRoot is copied per attempt, so one attempt cannot see another's edits and
// a failed attempt cannot poison the next.
func Tier1(repoRoot string) []Task {
	tasks := make([]Task, 0, len(specs))
	for _, s := range specs {
		tasks = append(tasks, taskFor(repoRoot, s))
	}
	return tasks
}

func taskFor(repoRoot string, s spec) Task {
	return Task{
		ID:         s.id,
		Provenance: HandWritten,
		Prompt:     s.prompt + " Run the tests to confirm.",

		Setup: func(dir string) error {
			if err := copyTree(repoRoot, dir); err != nil {
				return err
			}
			for _, b := range s.breaks {
				path := filepath.Join(dir, b.file)
				body, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				if !strings.Contains(string(body), b.old) {
					// The corpus is pinned to this repository's source. When an
					// edit no longer matches, the task is stale and saying so
					// beats silently handing out a task that is already solved.
					return fmt.Errorf("task %s is stale: %s no longer contains the text it breaks", s.id, b.file)
				}
				replaced := strings.Replace(string(body), b.old, b.new, 1)
				if err := os.WriteFile(path, []byte(replaced), 0o644); err != nil {
					return err
				}
			}
			return nil
		},

		Verify: func(ctx context.Context, dir string) (bool, string, error) {
			for path, want := range s.mustContain {
				if err := ctx.Err(); err != nil {
					return false, "", err
				}
				body, err := rootedfs.ReadFile(dir, path, maxVerifierSourceBytes)
				if err != nil {
					return false, "", err
				}
				if !strings.Contains(string(body), want) {
					// Without this a task can be "solved" by deleting whatever
					// fails, which passes the suite and fixes nothing.
					return false, fmt.Sprintf("%s no longer contains %q, so the fix removed the behaviour rather than restoring it", path, want), nil
				}
			}
			if err := ctx.Err(); err != nil {
				return false, "", err
			}

			// The copied checkout is adversarial verifier input. Resolve a compatible
			// local Go toolchain only after removing source/copy authority from PATH,
			// then retain its exact identity. A distributed sb binary cannot use its
			// build host's compiled-in GOROOT: that directory does not exist here.
			// The copied module's go directive checks compatibility; exact equality
			// with runtime.Version is neither required nor relocatable, and the
			// pinned GOTOOLCHAIN=local policy below forbids a substitute download.
			goEnv, err := verifierGoEnv(repoRoot, dir)
			if err != nil {
				return false, "", err
			}
			goExecutable, err := verifierGoExecutable(goEnv, repoRoot, dir)
			if err != nil {
				return false, "", err
			}
			cmd, err := goExecutable.Command("test", s.pkg)
			if err != nil {
				return false, "", fmt.Errorf("binding the verifier Go executable: %w", err)
			}
			cmd.Dir = dir
			// The verifier runs the repository's offline suite and nothing else.
			// Inheriting SB_LIVE would have it run the live tests, which need
			// network and credentials the sandbox correctly denies, so a task
			// would fail for a reason that has nothing to do with the fix.
			cmd.Env = goEnv
			out := &verifierOutput{}
			cmd.Stdout = out
			cmd.Stderr = out
			err = execution.RunProcess(ctx, cmd)
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return false, "", err
				}
				text, truncated := out.String()
				detail := strings.TrimSpace(lastLines(text, 12))
				if truncated {
					detail = fmt.Sprintf("verifier output exceeded %d bytes; showing the final lines\n%s", maxVerifierOutputBytes, detail)
				}
				return false, detail, nil
			}
			return true, "", nil
		},
	}
}

func verifierGoExecutable(environ []string, untrustedRoots ...string) (safeexec.Executable, error) {
	authorityRoots, err := safeexec.WorkspaceAndCurrentAuthorityRoots(untrustedRoots...)
	if err != nil {
		return safeexec.Executable{}, fmt.Errorf("resolving verifier workspace authority: %w", err)
	}
	name := "go"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	var pathValue string
	for _, entry := range environ {
		key, value, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(strings.TrimSpace(key), "PATH") {
			pathValue = value
		}
	}
	for _, directory := range filepath.SplitList(pathValue) {
		candidate := filepath.Join(directory, name)
		if _, err := os.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return safeexec.Executable{}, fmt.Errorf("inspecting the verifier Go executable: %w", err)
		}
		executable, err := safeexec.ResolvePathOutside(candidate, authorityRoots...)
		if err != nil {
			return safeexec.Executable{}, fmt.Errorf("binding the verifier Go executable outside verifier input: %w", err)
		}
		return executable, nil
	}
	return safeexec.Executable{}, errors.New("no trusted Go executable remains on PATH outside verifier input")
}

func verifierGoEnv(untrustedRoots ...string) ([]string, error) {
	authorityRoots, err := safeexec.WorkspaceAndCurrentAuthorityRoots(untrustedRoots...)
	if err != nil {
		return nil, fmt.Errorf("resolving verifier workspace authority: %w", err)
	}
	base := cleanEnv()
	out := make([]string, 0, len(base)+8)
	for _, entry := range base {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		upper := strings.ToUpper(strings.TrimSpace(name))
		// GOENV, GOFLAGS, GOTOOLCHAIN, GOWORK, and the module/VCS controls can
		// all redirect execution before the requested package tests run. Start
		// from no ambient Go policy and add the verifier's exact offline policy
		// below. CGO tool selectors are removed even though CGO is disabled.
		if strings.HasPrefix(upper, "GO") || upper == "CC" || upper == "CXX" ||
			upper == "AR" || upper == "FC" || upper == "PKG_CONFIG" {
			continue
		}
		out = append(out, entry)
	}
	filtered, err := safeexec.FilterEnvironmentPath(out, authorityRoots...)
	if err != nil {
		return nil, fmt.Errorf("preparing the verifier interpreter path: %w", err)
	}
	return append(filtered,
		"GOENV=off",
		"GOFLAGS=-count=1",
		"GOTOOLCHAIN=local",
		"GOWORK=off",
		"GOPROXY=off",
		"GOSUMDB=off",
		"GOVCS=*:off",
		"CGO_ENABLED=0",
	), nil
}

// cleanEnv strips what a verifier must not see: the switch that turns on live
// tests, and every credential. A corpus that can reach a provider is a corpus
// that can be solved by asking one.
func cleanEnv() []string {
	var out []string
	for _, kv := range execution.ScrubbedChildEnv() {
		name, _, _ := strings.Cut(kv, "=")
		switch {
		case name == "SB_LIVE",
			strings.HasSuffix(name, "_API_KEY"),
			strings.HasPrefix(name, "SB_"),
			name == "ANTHROPIC_API_KEY", name == "OPENAI_API_KEY":
			continue
		}
		out = append(out, kv)
	}
	return out
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// copyTree copies a checkout, skipping what a task neither needs nor should be
// able to reach.
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}

		// .git is skipped so a task cannot recover the answer from history, and
		// the rest is bulk that would slow every attempt down.
		switch {
		case info.IsDir() && (rel == ".git" || info.Name() == ".gocache" || info.Name() == "node_modules"):
			return filepath.SkipDir
		case info.IsDir():
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		case !info.Mode().IsRegular():
			return nil
		}

		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), body, info.Mode().Perm())
	})
}
