// Command sb is Switchboard's terminal entry point.
//
// Interactive sessions open the Bubble Tea TUI; the phase-0 line-oriented REPL
// remains behind -repl and is what the phase gates and single-prompt (-p) runs
// use, because both need a scriptable surface.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/checkpoint"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/prefix"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/provider/ollama"
	route "github.com/switchboard-code/switchboard/internal/router"
	"github.com/switchboard-code/switchboard/internal/schedule"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/tools"
	"github.com/switchboard-code/switchboard/internal/trust"
)

func main() {
	if err := run(); err != nil {
		os.Exit(writeCLIError(os.Stderr, err))
	}
}

func writeCLIError(w io.Writer, err error) int {
	if errors.Is(err, context.Canceled) {
		return 130
	}
	var parseErr *cliParseError
	if errors.As(err, &parseErr) {
		fmt.Fprintln(w, cliText(parseErr.Error()))
		writeRootHelp(w)
		return 2
	}
	fmt.Fprintln(w, "sb: "+cliText(err.Error()))
	return 1
}

type options struct {
	model     string
	tier      string
	host      string
	mode      string
	sandbox   string
	think     string
	workspace string
	prompt    string
	workflow  string
	output    string
	resume    string
	profile   string
	cont      bool
	list      bool
	showTiers bool
	repl      bool
	version   bool

	// cliSetFlags records only command-line flags explicitly supplied before a
	// subcommand. It is parser metadata, not session state: dispatch uses it to
	// reject combinations whose session intent would otherwise be ignored.
	cliSetFlags map[string]bool

	// allowSecrets widens the outbound credential gate for a scripted run,
	// the way -mode widens permissions: deliberately, on the command line,
	// never by default.
	allowSecrets bool
}

func run() error {
	// Help is a pure, bounded dispatch path. Keep it ahead of the Windows old
	// binary sweep and every config, inventory, session, and provider open so a
	// question about a command can never change the machine it is describing.
	if handled, err := handleCLIHelp(os.Stdout, os.Args[1:]); handled {
		return err
	}
	sweepOldBinary()

	opts, args, err := parseCLIOptions(os.Args[1:])
	if err != nil {
		return err
	}
	// -version has always been terminal, even when positional words follow it.
	// Preserve that precedence so a typo cannot turn `sb -version update` into
	// an updater invocation.
	if opts.version {
		v := currentVersion()
		if v == "" {
			v = "dev"
		}
		fmt.Println(cliText("sb " + v))
		return nil
	}
	if err := validateSessionInvocation(opts); err != nil {
		return err
	}
	args = consumeWorkflowArguments(&opts, args)
	if err := validateSubcommandFlags(opts, args); err != nil {
		return err
	}
	if handled, err := runCLISubcommand(context.Background(), os.Stdout, opts, args); handled {
		return err
	}
	switch opts.output {
	case "", "text":
	case "json", "stream-json":
		if opts.prompt == "" {
			return fmt.Errorf("-output %s reports one completed prompt, so it needs -p", opts.output)
		}
	default:
		return fmt.Errorf("unknown output format %q: text, json, or stream-json", opts.output)
	}

	ctx := context.Background()

	workspace := opts.workspace
	if workspace == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		workspace = cwd
	}
	workspace, err = filepath.Abs(workspace)
	if err != nil {
		return err
	}
	workspace, err = filepath.EvalSymlinks(workspace)
	if err != nil {
		return fmt.Errorf("resolving workspace identity: %w", err)
	}
	workspace = filepath.Clean(workspace)

	cat, err := catalog.Load()
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// The profile swap happens here, before anything reads the ladder:
	// session opening, routing, onboarding's empty-ladder check, and the
	// TUI all see the profile's tiers as the ladder, while Save keeps the
	// main ladder for the file.
	if opts.profile != "" {
		if err := cfg.ApplyProfile(opts.profile); err != nil {
			return err
		}
	}
	if opts.showTiers {
		return listTiers(cfg, cat)
	}

	store, err := session.DefaultStore()
	if err != nil {
		return err
	}
	retryRecovery, err := recoverInterruptedRetry(store, workspace)
	if err != nil {
		return err
	}
	retryRecoveryNotice := interruptedRetryRecoveryNotice(retryRecovery)
	if retryRecoveryNotice != "" && (opts.list || opts.prompt != "" || opts.workflow != "") {
		fmt.Fprintln(os.Stderr, "sb: "+cliText(retryRecoveryNotice))
	}
	if opts.list {
		return listSessions(store, workspace)
	}
	id, status, unresolved, err := store.UnresolvedRetry(workspace)
	if err != nil {
		return fmt.Errorf("checking for an interrupted /retry handoff: %w", err)
	}
	if unresolved {
		interactiveRetrySurface := !opts.repl && opts.prompt == "" && opts.workflow == "" && isTerminal(os.Stdin) && isTerminal(os.Stdout)
		note, retryErr := constrainUnresolvedRetryStartup(&opts, id, status, interactiveRetrySurface)
		if retryErr != nil {
			return retryErr
		}
		if note != "" {
			if retryRecoveryNotice == "" {
				retryRecoveryNotice = note
			} else {
				retryRecoveryNotice += "; " + note
			}
		}
	}

	mode, err := permission.ParseMode(opts.mode)
	if err != nil {
		return err
	}

	reg := newProviders(opts.host, cfg)

	// An empty ladder on an interactive terminal is the first run, not an
	// error: walk through binding t1 before anything needs a target. Every
	// non-interactive path still gets the explanatory error from resolveTier,
	// because a wizard on a pipe would hang whatever is driving it.
	onboarded := false
	if shouldRunOnboarding(opts, len(cfg.Tiers), isTerminal(os.Stdin), isTerminal(os.Stdout)) {
		if err := runOnboarding(reg, cat, cfg, workspace); err != nil {
			return err
		}
		onboarded = true
	}

	var chosen route.Decision
	sess, tier, client, resumed, fallbackNote, err := openSession(ctx, store, reg, cfg, cat, workspace, &opts, &chosen)
	if err != nil {
		return err
	}
	// A newly created startup log stays hidden until the complete runtime is
	// assembled. Any return before durable publication discards the owned stage, so a failed
	// MCP/LSP/tool setup cannot replace a real prior session under --continue.
	defer sess.CloseDiscardingStaged()

	capability := execution.Detect()
	sandboxMode := cfg.Sandbox
	if opts.sandbox != "" {
		sandboxMode, err = execution.ParseSandboxMode(opts.sandbox)
		if err != nil {
			return err
		}
	}
	if err := validateExecutionPosture(mode, sandboxMode); err != nil {
		return err
	}
	executionController, err := execution.NewController(capability, sandboxMode)
	if err != nil {
		return err
	}

	registry, err := tools.NewRegistryWithExecution(workspace, executionController)
	if err != nil {
		return err
	}

	// The undo recorder captures prior file states before write and edit
	// mutate them, scoped per turn by the loop.
	undoRec := checkpoint.NewRecorder()
	cleanupDir, err := store.WorkspaceDir(workspace)
	if err != nil {
		return fmt.Errorf("opening checkpoint cleanup directory: %w", err)
	}
	if err := undoRec.ConfigureRestoreCleanup(cleanupDir, workspace); err != nil {
		return err
	}
	registry.SetCheckpoints(undoRec)

	// Structural search joins the suite only where the machine has the
	// binary: looked up once, so the frozen zone never changes mid-session.
	addStructuralSearch(registry)

	// Computer use joins the same way, where the platform can serve it.
	addComputerUse(registry)

	// Plugins are inventory before they are behavior. Native Codex/Claude state
	// contributes provenance only; Switchboard's own activation ledger decides
	// which prompt-only skill roots may join this frozen session. Executable
	// components remain behind their separate digest-bound trust gate.
	// Full native provenance is needed here even though only Switchboard-owned
	// cached copies can activate. Codex managed plugin-MCP rules use the exact
	// marketplace-qualified native ID; deriving it from a manifest namespace
	// would let an unrelated local plugin spoof another plugin's policy entry.
	pluginEnv := openPluginInventory(workspace, true)
	pluginSkillRoots, pluginSkillNotes := pluginEnv.enabledSkillRoots()

	// Skills load the same way named agents do — native trees in place plus
	// explicitly enabled plugin roots, no executable trust implied. With none
	// found the tool is absent, keeping the schemas byte-identical.
	skillList, skillNotes := addSkills(registry, workspace, pluginSkillRoots...)

	// Path-scoped rules load with the skills, and for the same reason they
	// need no trust grant: nothing executes at read time, and a rule is a
	// prompt whose effects pass the permission engine on their own merits.
	ruleSetForSession, ruleNotes := loadRules(workspace)

	// The trust store is opened before MCP assembly because it is what
	// decides whether a repository's declared servers may start.
	trustStore, trustErr := trust.Open()

	// Whether anyone can answer a question is settled here, before the servers
	// connect, because the elicitation capability is declared at initialize
	// and a client that advertised it with no user attached would be promising
	// an answer it cannot produce. Piped -p and headless workflows are surfaces
	// with no one to ask, the same condition that leaves the permission asker
	// unset further down. Every other surface gets a relay for its dialog.
	var questions *questionRelay
	if plainSurfaceCanAsk(opts, isTerminal(os.Stdin)) {
		questions = &questionRelay{}
		registry.SetQuestioner(questions)
	}

	// Native definitions are discovered read-only, then independently gated by
	// Switchboard activation, managed policy, runtime feature support, and
	// executable trust. Plugin MCP gets the same gates plus the plugin's exact
	// digest-bound execution grant. All of this happens before connectMCP freezes
	// the tool registry for the session.
	nativeMCPEnv := openNativeMCPInventory(ctx, workspace, pluginEnv.requiresCodexAppServer())
	nativePolicy, nativePolicyNotes := loadNativeMCPPolicy(workspace, nativeMCPEnv.codexRequirementsChecked)
	nativeSpecs, nativeNotes, err := activatedNativeMCPSpecs(nativeMCPEnv, trustStore, nativePolicy)
	if err != nil {
		return err
	}
	pluginSpecs, pluginMCPNotes, err := enabledPluginMCPSpecs(pluginEnv, workspace, nativePolicy)
	if err != nil {
		return err
	}
	additionalMCP := append(nativeSpecs, pluginSpecs...)
	mcpEnv, mcpRules, err := connectMCP(ctx, workspace, trustStore, registry, questions, additionalMCP...)
	if err != nil {
		return err
	}
	defer mcpEnv.Close()
	if retryRecoveryNotice != "" && opts.prompt == "" && opts.workflow == "" {
		mcpEnv.add(mcpNote{"warn", retryRecoveryNotice})
	}
	// Whatever exec left running goes with the session. A background process
	// this program started and then forgot is this program's fault, and the
	// exit is the last moment it can still be sure the group is its own.
	defer registry.StopBackgroundCommands()
	for _, n := range ruleNotes {
		mcpEnv.add(mcpNote{"warn", n})
	}
	for _, n := range nativePolicyNotes {
		mcpEnv.add(n)
	}
	for _, n := range nativeNotes {
		mcpEnv.add(n)
	}
	for _, n := range pluginMCPNotes {
		mcpEnv.add(n)
	}

	hookSet, hookNotes := loadHooks(workspace, trustStore)
	for _, n := range hookNotes {
		mcpEnv.add(n)
	}
	for _, n := range skillNotes {
		mcpEnv.add(n)
	}
	for _, n := range pluginSkillNotes {
		mcpEnv.add(n)
	}
	for _, n := range pluginEnv.diagnostics {
		mcpEnv.add(n)
	}

	// Precise symbol lookup joins the suite when a module, a server, and a
	// trust grant line up; the note says which one did not.
	lspServer, lspNote := setupLSP(workspace, trustStore, registry)
	if lspServer != nil {
		defer lspServer.Close()
	}
	if lspNote != "" {
		mcpEnv.add(mcpNote{"", lspNote})
	}

	// §6 is only live if something wires it. The loop assembles a request from
	// the session by default, so without this the zones, the breakpoint
	// manager, and the tracker are all present and never consulted.
	cache := cacheFor(tier.Target, cat)

	// The user's own file goes first. Among non-deny rules the engine takes the
	// first match, so a rule the user wrote to tighten a server's tool has to
	// sit ahead of the allow list that server declared for itself. A deny in
	// either wins over every allow wherever it sits, so ordering only decides
	// this one direction.
	rules := append(append([]permission.Rule(nil), cfg.Permissions...), mcpRules...)
	permEngine := permission.NewEngineWithExecution(mode, executionController, rules...)
	loop := &agent.Loop{
		Provider:    client,
		Target:      tier.Target,
		Tools:       registry,
		Perms:       permEngine,
		Session:     sess,
		Catalog:     cat,
		Cache:       cache,
		System:      agent.SystemPrompt(workspace, mode, capability),
		Hooks:       hookSet,
		Checkpoints: undoRec,
		ContextWindow: func(target provider.RouteTarget) int {
			return effectiveContextWindow(cfg, reg, cat, target)
		},
		OutputAllowance: reg.outputTokenAllowance,
	}
	// Even the initial process binding goes through the session boundary: a
	// resumed continuity capsule hydrates its todo list, while a fresh or
	// tombstoned session explicitly starts with none. The same operation drops
	// any registry read authority before the first turn can observe it.
	if err := loop.BindSession(sess); err != nil {
		return fmt.Errorf("restoring session context: %w", err)
	}

	// The ceiling gates the loop before each call, whatever surface drives
	// it; /budget adjusts the shared state mid-session.
	budget := &budgetState{}
	budget.set(cfg.Budget)
	wireBudget(loop, primaryGate(budget, loop, cat))

	// The delegate tool joins after the loop exists because its subagents
	// share the loop's permission engine and asker; it still lands before the
	// first request, which is what the frozen zone requires.
	agents, agentNotes, err := registerDelegate(registry, cfg, cat, reg, loop, hookSet, capability, workspace, undoRec, budget, skillList)
	if err != nil {
		mcpEnv.add(mcpNote{"warn", "delegate unavailable: " + err.Error()})
	}
	for _, n := range agentNotes {
		mcpEnv.add(mcpNote{"warn", n})
	}

	// Resume is admitted only after the exact frozen system and tool zones have
	// been assembled. The preliminary binding above exists solely so runtime
	// components can be constructed; no resumed binding is recorded until the
	// full replay passes destination, capability, context/output, and remaining
	// hard-budget checks against the concrete primary or availability fallback.
	if resumed {
		tier, client, fallbackNote, err = finalizeStartupResume(ctx, sess, loop, cfg, cat, reg, budget, &opts)
		if err != nil {
			return err
		}
	}
	wireCommandReviewer(loop, cfg, cat, reg, tier, budget)

	// A fallback substitution renders before any content is sent and is
	// recorded on the session (§5.4): the user must know which server this
	// conversation is actually going to.
	if fallbackNote != "" {
		mcpEnv.add(mcpNote{"warn", fallbackNote})
	}

	// The sticky primary starts wherever routing landed, and the watcher feeds
	// it what happens inside a turn. Without that connection the escalation
	// policy is built and never consulted.
	startRank := 0
	for i, t := range cfg.Tiers {
		if t.ID == tier.ID {
			startRank = i
		}
	}
	sticky := route.NewSticky(route.Policy{}, startRank)
	resumedPin := resumed && sess.State().RuntimeBinding.Target != "" && sess.State().RuntimeBinding.Pinned
	if opts.tier != "" || opts.model != "" || resumedPin {
		sticky.Pin(startRank)
	}

	var routeDec *route.Decision
	if chosen.Source != "" {
		routeDec = &chosen
	}

	// The TUI is the default interactive surface; the REPL remains for
	// scripting, for gates, and for terminals that are not terminals. A single
	// -p prompt keeps the plain renderer either way.
	tuiSurface := !opts.repl && opts.prompt == "" && opts.workflow == "" && isTerminal(os.Stdin) && isTerminal(os.Stdout)
	if intent := sess.State().RetryIntent; intent != nil && !tuiSurface {
		action := "show the explicit recovery action"
		if intent.Status == session.RetryIntentPending {
			action = "resume its proven-unstarted replay"
		}
		return fmt.Errorf("session %s has an unresolved /retry handoff (%s); reopen it in the interactive TUI so Switchboard can %s without duplicating provider or tool work",
			sess.ID(), intent.Status, action)
	}
	if tuiSurface {
		updateCheck := cfg.UpdateCheck && os.Getenv("SB_NO_UPDATE_CHECK") == ""
		return runTUI(loop, store, cfg, cat, capability, workspace, tier, reg, sticky, routeDec, sess, resumed, updateCheck, trustStore, trustErr, mcpEnv, lspServer, lspNote, undoRec, agents, agentNotes, budget, skillList, onboarded, questions, ruleSetForSession)
	}

	// With -output json, stdout carries exactly one JSON line and nothing
	// else; the transcript still renders, on stderr, so a person watching a
	// scripted run sees the work happen.
	outDest := os.Stdout
	if opts.output == "json" || opts.output == "stream-json" {
		// stdout belongs to the machine-readable stream; the transcript a
		// person reads keeps going, on stderr, so a run can be watched and
		// consumed at once.
		outDest = os.Stderr
	}
	out := newRenderer(outDest)
	in := bufio.NewReader(os.Stdin)

	// Piped stdin under -p is content, not a control channel: it rides into
	// the prompt as an attachment, and the asker is left unset so anything
	// needing approval is refused with a reason rather than answered by
	// whatever bytes happened to be next in the pipe. Widening -mode is the
	// deliberate way to let a scripted run do more.
	authoredPrompt := opts.prompt
	piped := opts.prompt != "" && !isTerminal(os.Stdin)
	if piped {
		data, err := readPipedInput(os.Stdin)
		if err != nil {
			return err
		}
		opts.prompt = attachPipedInput(opts.prompt, data)
	} else {
		attachPlainSurfaceRelays(opts, isTerminal(os.Stdin), loop, questions, in, out)
	}
	observer := selectPlainObserver(opts.output, os.Stdout, out,
		sess.ID(), tier.ID, string(tier.Target.ID()), string(mode))
	loop.SetObserver(observer)
	subagentForward.set(out)

	r := &repl{
		loop:       loop,
		out:        out,
		in:         in,
		capability: capability,
		workspace:  workspace,
		config:     cfg,
		catalog:    cat,
		tier:       tier,
		providers:  reg,
		budget:     budget,
		caches:     newCacheSet(tier.Target, loop.Cache),
		store:      store,
	}
	r.route = routeDec
	r.sticky = sticky
	r.watcher = newWatcher(observer, sticky, len(cfg.Tiers)-1, r.moveTo)
	r.watcher.setPaused(!cfg.RouteAutoOn())
	loop.SetObserver(r.watcher)

	// The schedule ledger rides the per-workspace directory the session logs
	// already live in. A ledger that will not load costs the feature, never
	// the session: the commands say why, and nothing fires. Headless runs
	// skip the load entirely — they cannot fire, so loading would hold the
	// workspace's lock for nothing and print a feature notice on a surface
	// that cannot use it.
	if opts.prompt == "" && opts.workflow == "" {
		if ledger, err := openWorkspaceSchedule(store, workspace); err != nil {
			if errors.Is(err, schedule.ErrLocked) {
				r.schedulesErr = ": another sb process in this workspace holds them"
			} else {
				r.schedulesErr = ": " + err.Error()
			}
		} else {
			r.schedules = ledger
			defer ledger.Close()
		}
	}

	// Scripted startup can still fail without sending a turn: a secret gate may
	// refuse -p, or a workflow name/runner may be unavailable. Resolve those
	// facts before publication so the rejected empty child cannot replace the
	// previous real session as --continue's Latest candidate.
	if err := publishSessionAfterStartupPreflight(sess, opts.prompt, opts.allowSecrets, opts.workflow); err != nil {
		return err
	}
	r.banner(sess, resumed)
	if r.schedulesErr != "" {
		out.Notice("warn", "schedules are unavailable"+r.schedulesErr)
	}
	// The REPL drains what buffered and attaches no live target: the
	// renderer is driven from the loop's goroutine, and a client's read
	// loop writing to it concurrently would race. Later notes buffer,
	// capped, and are simply not the REPL's concern.
	startupNotes, droppedStartupNotes := mcpEnv.attachCounted(nil)
	r.startupNotes = aggregateStartupNotes(startupNotes, droppedStartupNotes)
	writeStartupNoteReport(out, r.startupNotes)

	// A workflow is the one thing on this surface that is not a prompt: its
	// stages were decided when the file was written, so nothing here is asked
	// of a model. It runs after assembly for the same reason the TUI's does,
	// because it needs the ladder, the permission engine, and the budget the
	// session was built with.
	if opts.workflow != "" {
		return runHeadlessWorkflow(ctx, out, opts.workflow, opts.allowSecrets)
	}

	if opts.prompt != "" {
		err := r.onceAuthored(ctx, opts.prompt, authoredPrompt)
		if opts.output == "json" || opts.output == "stream-json" {
			rep := buildHeadlessReport(loop.Session.State(), cat, r.tier, err)
			rep.PermissionMode = string(loop.Perms.Mode())
			rep.Sandbox = string(loop.Perms.Execution().SandboxMode())
			rep.ExecutionPosture = loop.Perms.Execution().Summary()
			rep.FullHostAccess = !loop.Perms.Execution().SandboxActive()
			wErr := writeHeadlessReport(os.Stdout, rep)
			if opts.output == "stream-json" {
				// The same report, tagged, as the stream's last line: a
				// consumer reading the stream recognizes the end without
				// counting, and one reading only the last line gets what
				// -output json would have given it.
				wErr = writeStreamResult(os.Stdout, rep)
			}
			if wErr != nil {
				return wErr
			}
		}
		return err
	}
	return r.interactive(ctx)
}

func shouldRunOnboarding(opts options, tierCount int, stdinTerminal, stdoutTerminal bool) bool {
	return tierCount == 0 && opts.model == "" && opts.resume == "" && !opts.cont &&
		!opts.repl && opts.prompt == "" && opts.workflow == "" && stdinTerminal && stdoutTerminal
}

// plainSurfaceCanAsk is false for every -workflow invocation, even from a
// terminal. The headless workflow flag promises an unattended surface; its
// delegates inherit the primary loop's nil asker and therefore fail closed on
// an approval instead of stealing bytes from stdin or hanging a CI job.
func plainSurfaceCanAsk(opts options, stdinTerminal bool) bool {
	return opts.workflow == "" && (opts.prompt == "" || stdinTerminal)
}

func attachPlainSurfaceRelays(opts options, stdinTerminal bool, loop *agent.Loop, questions *questionRelay, in *bufio.Reader, out *renderer) {
	if !plainSurfaceCanAsk(opts, stdinTerminal) {
		return
	}
	loop.Asker = &terminalAsker{in: in, out: out}
	// The ask tool follows the asker: a surface that can answer a permission
	// prompt can answer a question. An unattended surface built no relay above,
	// so its tool refuses rather than reading an answer out of the input stream.
	if questions != nil {
		questions.set(&terminalQuestioner{in: in, out: out})
	}
}

func publishSessionAfterStartupPreflight(sess *session.Session, prompt string, allowSecrets bool, workflow string) error {
	if workflow != "" {
		if _, err := prepareHeadlessWorkflow(workflow, allowSecrets); err != nil {
			return err
		}
	}
	if prompt != "" {
		// The gate has no one to ask on this surface, so a key-shaped string
		// refuses the run outright; -allow-secrets is the deliberate widening,
		// the way -mode is for permissions.
		if err := refuseLeakedSecrets(prompt, allowSecrets); err != nil {
			return err
		}
	}
	if sess.PublicationPending() {
		if err := publishSessionDurably(sess, "assembled session"); err != nil {
			return err
		}
	}
	return nil
}

// publishSessionDurably keeps a visibility-uncertain failure out of the
// rollback path. Once a marker is discoverable the session has been adopted,
// even if its persistence barrier failed; the only honest response is to stop
// and require a restart, leaving the visible log intact for recovery.
func publishSessionDurably(sess *session.Session, label string) error {
	outcome, err := sess.PublishDurably()
	disposition, resultErr := publicationResult(outcome, err, label)
	if disposition == publicationDurable {
		return nil
	}
	return resultErr
}

func validateExecutionPosture(mode permission.Mode, sandbox execution.SandboxMode) error {
	if mode == permission.ModeYOLO && sandbox != execution.SandboxOff {
		return fmt.Errorf("-mode yolo requires sandbox off; remove -sandbox or set [execution] sandbox = \"off\"")
	}
	return nil
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// openSession resolves the session and the starting tier together, because a
// resumed session names the target it was recorded with and a new one is named
// by the tier.
func openSession(
	ctx context.Context,
	store *session.Store,
	reg *providers,
	cfg *config.Config,
	cat *catalog.Catalog,
	workspace string,
	opts *options,
	chosen *route.Decision,
) (*session.Session, config.Tier, provider.Provider, bool, string, error) {
	var (
		sess *session.Session
		err  error
	)
	switch {
	case opts.resume != "":
		sess, err = store.OpenInWorkspace(opts.resume, workspace)
	case opts.cont:
		sess, err = store.Latest(workspace)
		if errors.Is(err, session.ErrNoSessions) {
			err = fmt.Errorf("no session to continue in %s", workspace)
		}
	default:
		tier, client, note, buildErr := resolveTier(ctx, reg, cfg, cat, opts, "", chosen)
		if buildErr != nil {
			return nil, config.Tier{}, nil, false, "", buildErr
		}
		sess, err = store.CreateStaged(workspace, tier.Target.ID(), cat.Revision)
		if err == nil {
			err = persistRuntimeBindingFallback(sess, tier, opts.tier != "" || opts.model != "", note)
		}
		if err != nil && sess != nil {
			_ = sess.CloseDiscardingStaged()
		}
		return sess, tier, client, false, note, err
	}

	if err != nil {
		return nil, config.Tier{}, nil, false, "", err
	}
	state := sess.State()
	tier, configured, err := resumeTier(cfg, state, opts)
	var client provider.Provider
	var note string
	if err == nil {
		if configured {
			tier, client, note, err = reg.probeTierFallback(ctx, tier)
		} else {
			tier, client, err = reg.probeTier(ctx, tier)
		}
	}
	if err != nil {
		sess.Close()
		return nil, config.Tier{}, nil, false, "", err
	}
	// This is deliberately a probe-only preliminary binding. Startup still has
	// to assemble MCP, LSP, skills, delegate, and the exact frozen system zone;
	// finalizeStartupResume validates that complete replay and is the only place
	// that records the resumed runtime binding.
	return sess, tier, client, true, note, nil
}

// resolveTier picks the starting target. An explicit model wins, then an
// explicit tier, then the target a resumed session recorded, then the bottom of
// the ladder.
func resolveTier(ctx context.Context, reg *providers, cfg *config.Config, cat *catalog.Catalog, opts *options, recorded string, chosen *route.Decision) (config.Tier, provider.Provider, string, error) {
	switch {
	case opts.model != "":
		target := ollama.Target(opts.model)
		applyEffort(&target, opts.think)
		return reg.probeTierFallback(ctx, config.Tier{ID: "-model", Label: "ad hoc", Target: target})

	case opts.tier != "":
		tier, ok := cfg.Tier(opts.tier)
		if !ok {
			return config.Tier{}, nil, "", fmt.Errorf("no tier %s is configured; run sb -tiers to see the ladder", opts.tier)
		}
		applyEffort(&tier.Target, opts.think)
		return reg.probeTierFallback(ctx, tier)

	case recorded != "":
		// A resumed session stays on the target it was recorded with unless the
		// user asked otherwise, so replaying it means what it meant.
		tier, ok, matchErr := tierForTarget(cfg, recorded)
		if matchErr != nil {
			return config.Tier{}, nil, "", matchErr
		}
		if ok {
			return reg.probeTierFallback(ctx, tier)
		}
		target, err := parseRecordedTarget(recorded)
		if err != nil {
			return config.Tier{}, nil, "", err
		}
		applyEffort(&target, opts.think)
		return reg.probeTierFallback(ctx, config.Tier{ID: "-resumed", Label: "resumed", Target: target})
	}

	if len(cfg.Tiers) == 0 {
		return config.Tier{}, nil, "", noTargetError(ctx, reg.localServer(), cfg)
	}
	if opts.prompt == "" {
		// Interactive startup needs a live provider to assemble the session, but
		// there is no routing decision until a user turn exists. Bootstrap on the
		// first reachable rung and let planUserTurn route the full prospective
		// request. An unavailable bottom rung must not prevent reaching that
		// router, and recording an empty-prompt "decision" would be false
		// provenance.
		return probeAutomaticBootstrap(ctx, reg, cfg, opts.think)
	}

	// Routing off is the user owning every rung change, on this surface too:
	// bootstrap on the first reachable rung exactly as an interactive session
	// does, and the per-turn path then holds it.
	if !cfg.RouteAutoOn() {
		return probeAutomaticBootstrap(ctx, reg, cfg, opts.think)
	}

	// A headless prompt is already known, so choose a feasible bootstrap target.
	// The assembled loop still re-routes immediately before the call with exact
	// system/tool/history token counts; this early pass avoids probing a rung the
	// visible request has already ruled out.
	bootstrapRequest := provider.Request{Messages: []provider.Message{provider.UserText(opts.prompt)}}
	input := route.Input{
		Prompt:     opts.prompt,
		Candidates: withLiveCandidateCapabilities(candidatesForContext(cfg, cat, prefix.RequestTokens(bootstrapRequest), prefix.RequestTokenCeiling(bootstrapRequest), nil), reg),
		// Tool capability is established by the live probe below. Applying the
		// catalog filter first would make an unlisted but tool-capable local model
		// impossible to bootstrap in headless mode.
		Requirements: route.Requirements{},
		// The session opens with nothing spent, so the whole ceiling is what
		// a rung's upper bound is checked against (§15).
		Budgets: route.Budgets{MaxCost: cfg.Budget},
	}
	return routeAutomaticBootstrap(ctx, reg, cfg, cat, input, opts.think, chosen)
}

func probeAutomaticBootstrap(ctx context.Context, reg *providers, cfg *config.Config, effort string) (config.Tier, provider.Provider, string, error) {
	var rejected []string
	for _, configured := range cfg.Tiers {
		tier := configured
		applyEffort(&tier.Target, effort)
		probed, client, fallbackNote, err := reg.probeTierFallback(ctx, tier)
		if err == nil {
			return probed, client, bootstrapNote(rejected, probed.ID, fallbackNote), nil
		}
		rejected = append(rejected, fmt.Sprintf("tier %s was unavailable at startup: %v", configured.ID, err))
	}
	return config.Tier{}, nil, "", fmt.Errorf("no configured tier is reachable:\n  %s", strings.Join(rejected, "\n  "))
}

func routeAutomaticBootstrap(ctx context.Context, reg *providers, cfg *config.Config, cat *catalog.Catalog,
	input route.Input, effort string, chosen *route.Decision,
) (config.Tier, provider.Provider, string, error) {
	rejected := map[string]string{}
	for {
		candidates := make([]route.Candidate, 0, len(input.Candidates))
		for _, candidate := range input.Candidates {
			if rejected[candidate.Tier] == "" {
				candidates = append(candidates, candidate)
			}
		}
		attempt := input
		attempt.Candidates = candidates
		decision, err := (route.Heuristic{}).Route(attempt)
		for _, configured := range cfg.Tiers {
			if reason := rejected[configured.ID]; reason != "" {
				decision.Infeasible = append(decision.Infeasible, reason)
			}
		}
		if err != nil {
			if len(decision.Infeasible) == 0 {
				return config.Tier{}, nil, "", err
			}
			return config.Tier{}, nil, "", fmt.Errorf("%v\n  %s", err, strings.Join(decision.Infeasible, "\n  "))
		}
		tier, ok := cfg.Tier(decision.Tier)
		if !ok {
			return config.Tier{}, nil, "", fmt.Errorf("the router chose %q, which is not on the ladder", decision.Tier)
		}
		var selected route.Candidate
		for _, candidate := range input.Candidates {
			if candidate.Tier == tier.ID {
				selected = candidate
				break
			}
		}
		applyEffort(&tier.Target, effort)
		probed, client, fallbackNote, probeErr := reg.probeTierFallbackFeasible(ctx, tier, func(concrete config.Tier) error {
			candidate := withLiveCapabilities(candidateForTierContext(concrete, selected.Rank, cat,
				selected.PromptTokens, selected.ContextTokens, 0), reg)
			_, checkErr := (route.Heuristic{}).Route(route.Input{
				Candidates: []route.Candidate{candidate}, Requirements: attempt.Requirements,
				Budgets: attempt.Budgets, Pin: concrete.ID,
			})
			return checkErr
		})
		if probeErr != nil {
			rejected[tier.ID] = fmt.Sprintf("tier %s was unavailable at startup: %v", tier.ID, probeErr)
			continue
		}

		// The concrete target (including a configured fallback or effort
		// override) is what receives and bills the request.
		concreteCandidate := candidateForTierContext(probed, selected.Rank, cat,
			selected.PromptTokens, selected.ContextTokens, 0)
		decision.EstimatedCost = concreteCandidate.Estimate
		decision.Target = probed.Target.ID()
		*chosen = decision
		var ordered []string
		for _, configured := range cfg.Tiers {
			if reason := rejected[configured.ID]; reason != "" {
				ordered = append(ordered, reason)
			}
		}
		return probed, client, bootstrapNote(ordered, probed.ID, fallbackNote), nil
	}
}

func bootstrapNote(rejected []string, selected, fallbackNote string) string {
	parts := append([]string(nil), rejected...)
	if len(rejected) > 0 {
		parts = append(parts, "startup continued on tier "+selected)
	}
	if fallbackNote != "" {
		parts = append(parts, fallbackNote)
	}
	return strings.Join(parts, "; ")
}

func applyEffort(target *provider.RouteTarget, effort string) {
	if effort != "" {
		target.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: effort}
	}
}

func tierForTarget(cfg *config.Config, recorded string) (config.Tier, bool, error) {
	// New IDs are injective, but an exact canonical ID can still equal another
	// configured target's pre-escaping spelling. Gather both forms and accept
	// only one concrete target, so an old ambiguous record never picks a rung by
	// iteration order.
	var matched config.Tier
	matches := 0
	type matchKey struct {
		tierID string
		target provider.RouteTargetID
	}
	seen := map[matchKey]bool{}
	consider := func(tier config.Tier, target provider.RouteTarget) {
		exact := string(target.ID()) == recorded
		legacyLossless := target.Params.MaxOutputTokens == 0 && target.Params.Temperature == nil &&
			(target.Params.Reasoning == nil || target.Params.Reasoning.Enabled)
		if !exact && (!legacyLossless || string(target.LegacyID()) != recorded) {
			return
		}
		key := matchKey{tierID: tier.ID, target: target.ID()}
		if seen[key] {
			return
		}
		seen[key] = true
		matched, matches = tierWithActiveTargetFirst(tier, target), matches+1
	}
	for _, t := range cfg.Tiers {
		consider(t, t.Target)
		for _, fallback := range t.Fallbacks {
			consider(t, fallback)
		}
	}
	if matches > 1 {
		return config.Tier{}, false, fmt.Errorf(
			"session target %q matches %d configured targets under legacy identity rules; choose -tier or -model explicitly",
			provider.DisplayRouteTargetID(provider.RouteTargetID(recorded)), matches)
	}
	return matched, matches == 1, nil
}

// parseRecordedTarget reads a target back out of a session record. The catalog
// owns target identity, so this is deliberately narrow: it recovers what was
// recorded rather than inventing a target the user never configured.
func parseRecordedTarget(recorded string) (provider.RouteTarget, error) {
	target, err := provider.ParseRouteTargetID(provider.RouteTargetID(recorded))
	if err != nil {
		return provider.RouteTarget{}, fmt.Errorf("session recorded an unreadable target %q: %w", recorded, err)
	}
	return target, nil
}

func noTargetError(ctx context.Context, client *ollama.Client, cfg *config.Config) error {
	models, err := client.Models(ctx)
	if err != nil {
		return fmt.Errorf("no tiers configured and no model given, and the Ollama server could not be reached: %w", err)
	}

	var b strings.Builder
	b.WriteString("no tiers configured and no -model given.\n")
	if cfg.Path != "" {
		fmt.Fprintf(&b, "\nConfigure a ladder in %s:\n\n", cliText(cfg.Path))
		b.WriteString("  [tiers.t1]\n  label = \"light\"\n  model = \"ollama/<model>\"\n\n")
		b.WriteString("  [tiers.t2]\n  label = \"deep\"\n  model = \"ollama/<model>\"\n")
	}
	if len(models) > 0 {
		for i := range models {
			models[i] = cliText(models[i])
		}
		fmt.Fprintf(&b, "\nModels this server has pulled:\n  %s", strings.Join(models, "\n  "))
	}
	return errors.New(b.String())
}

func listTiers(cfg *config.Config, cat *catalog.Catalog) error {
	if len(cfg.Tiers) == 0 {
		fmt.Printf("no tiers configured in %s\n", cliText(cfg.Path))
		return nil
	}
	fmt.Printf("catalog %s (%s)\n\n", cliText(cat.Revision), cliText(cat.Source))
	for _, t := range cfg.Tiers {
		fmt.Println(cliText(t.String()))
		info, confidence, ok := cat.Lookup(t.Target)
		if !ok {
			fmt.Println("      no catalog entry")
			continue
		}
		fmt.Printf("      %s", describePricing(info))
		if confidence == catalog.Prior {
			fmt.Print("  (surface default, not verified for this model)")
		}
		fmt.Println()
	}
	return nil
}

func describePricing(info catalog.ModelInfo) string {
	switch info.Metering {
	case catalog.Local:
		return "runs locally, nothing meters it"
	case catalog.Plan:
		// Not the same as free. Nothing here models quota yet, so the honest
		// answer names what is actually finite rather than reporting zero.
		return "billed as a plan, not per token; quota rather than cost is the limit"
	}
	if info.Free() {
		return "no per-token cost recorded"
	}
	band, ok := info.Band(0)
	if !ok {
		return "no price band"
	}
	return fmt.Sprintf("%s in / %s out per MTok", band.InputPerMTok, band.OutputPerMTok)
}

func listSessions(store *session.Store, workspace string) error {
	infos, err := store.List(workspace)
	if err != nil {
		return err
	}
	if len(infos) == 0 {
		fmt.Printf("no sessions recorded for %s\n", cliText(workspace))
		return nil
	}
	for _, info := range infos {
		// The same first-words label the /resume picker shows; the read
		// stops at the head of each log and takes no lock.
		line := fmt.Sprintf("%s  %s  %d bytes  %s", info.ID, info.Modified.Local().Format("2006-01-02 15:04:05"), info.Size,
			resumeHealthChips(info.Health, true))
		if opening := openingLabel(info.Path); opening != "" {
			line += "  " + opening
		}
		fmt.Println(cliText(line))
	}
	return nil
}
