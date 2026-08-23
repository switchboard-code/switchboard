package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/switchboard-code/switchboard/internal/advisor"
	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/checkpoint"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/delegate"
	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/lsp"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/prefix"
	"github.com/switchboard-code/switchboard/internal/provider"
	route "github.com/switchboard-code/switchboard/internal/router"
	"github.com/switchboard-code/switchboard/internal/schedule"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/skills"
	"github.com/switchboard-code/switchboard/internal/tools"
	"github.com/switchboard-code/switchboard/internal/trust"
)

// Messages flowing from the loop goroutine (and async commands) into the
// program. The loop never touches the model; everything arrives through these.
type deltaMsg struct {
	thinking bool
	text     string
}
type toolStartMsg struct {
	id   string
	name string
	req  permission.Request
}
type toolEndMsg struct {
	id   string
	name string
	res  tools.Result
	took time.Duration
}
type noticeMsg struct {
	level, text string

	// resumed marks a notice whose flow has already queued what comes next.
	// A host that advances on every notice would otherwise take a step of its
	// own alongside the one the flow scheduled.
	resumed bool

	// operation identifies an exclusive asynchronous UI operation. Such a
	// notice is also its completion signal, so Update can release busy state
	// only if it still belongs to the session that launched it.
	operation uint64
	sourceID  string
}
type usageMsg struct{ u session.Usage }
type askMsg struct {
	req     permission.Request
	out     permission.Outcome
	respond chan permission.Response
}
type turnDoneMsg struct {
	generation uint64
	err        error
	after      session.State
}
type turnPlanMsg struct {
	generation uint64
	opening    provider.Message
	// prompt/images are display-only projections kept for UI diagnostics and
	// tests. Routing and sending use opening exclusively.
	prompt string
	images []provider.Image
	tier   config.Tier
	client provider.Provider
	note   string
	plan   turnPlan
	err    error
}
type tierNowMsg struct {
	line string
	rank int // destination rung, for the junction marker's heat color
	tier config.Tier

	// abandoned is the priced warmth the move left behind, "" when there
	// was nothing honest to price. It rides the same message as the move
	// so the two facts cannot arrive apart, and /why keeps them together.
	abandoned string
}
type tierSwitchMsg struct {
	tier      config.Tier
	client    provider.Provider
	silent    bool   // a /tN override restoring what it borrowed, not a user switch
	note      string // a fallback substitution, rendered before content is sent
	err       error
	operation uint64
	sourceID  string
}
type sessionSwapMsg struct {
	sess     *session.Session
	tier     config.Tier
	client   provider.Provider
	fresh    bool
	note     string // rendered after the swap; how a fork says where it came from
	warnNote bool   // a fallback substitution, persisted on the resumed session
	pinned   bool   // reconstructed durable manual/automatic routing posture

	// preserveRuntimeTarget keeps a process-only /think parameter change out
	// of a derived session. Ordinary clear/fork/retry/compact operations carry
	// the previous durable target even while the new process keeps serving the
	// live parameter override. Resume and race select their target explicitly.
	preserveRuntimeTarget bool
	err                   error

	// operation/sourceID bind an asynchronous swap to the exclusive operation
	// and source session that launched it. A late result can never replace a
	// session that has since moved on.
	operation uint64
	sourceID  string

	// release runs after the new session has been installed (or the swap was
	// rejected). Fork/compaction use it to keep asynchronous advisor calls out
	// of the source ledger between their snapshot and the bind.
	release func()

	// andThen, when set, runs once the swap has landed — how /retry sends
	// its replay into the forked session rather than the one it left.
	andThen tea.Cmd
	// continueTurn makes onSessionSwap claim turn-planning ownership before
	// returning andThen, closing the event-loop gap between bind and replay.
	continueTurn bool

	// keepFold carries a queued watch or bisect verdict across the swap.
	// A fold is a report to the conversation that made the edits, so only
	// the swaps that continue that conversation set this: compaction, and
	// keeping a race arm. A clear, a resume, a fork, a retry replace the
	// conversation — or revert the files — and the verdict dies with it.
	keepFold bool

	// continuePrompt, when set, opens a turn on the new session the moment
	// the swap lands. Compaction sets it: a compacted session should pick
	// the work back up, not wait for the user to say "continue" to a model
	// that already has the summary.
	continuePrompt string
}
type overrideProbeMsg struct {
	generation uint64
	opening    provider.Message
	// prompt/images never reconstruct the outbound message; opening is the
	// single routing and provider value.
	prompt string
	images []provider.Image
	tier   config.Tier
	client provider.Provider
	note   string
	plan   turnPlan
	err    error
}
type updateCheckMsg struct {
	latest string
	err    error
}
type updateAppliedMsg struct{ version string }
type copyMsg struct {
	n    int
	what string // "response" or "code block"; the notice names what landed
	err  error
}
type disarmQuitMsg struct{}
type doctorDoneMsg struct{ report string }
type extensionActionMsg struct {
	kind      string
	output    string
	err       error
	operation uint64
	sourceID  string
}

type turnExecution struct {
	generation uint64
	watcher    *watcher
	advisor    *advisor.Advisor
	startedOn  config.Tier
	before     session.State
	usage      session.UsageCursor
	started    time.Time
	decision   *route.Decision
	features   route.SessionFeatures
}

func noticeCmd(level, text string) tea.Cmd {
	return func() tea.Msg { return noticeMsg{level: level, text: text} }
}

type tuiModel struct {
	app  *tuiApp
	tr   *transcript
	ta   textarea.Model
	spin spinner.Model
	th   *theme
	md   *markdown

	width, height int

	busy        bool
	started     time.Time
	turnCancel  context.CancelFunc
	turnPrompt  string
	turnStarted config.Tier
	turnBefore  session.State
	queue       []string

	// Status-line state. The model renders from its own copies rather than
	// reading loop fields, because the loop's goroutine can be mutating them.
	tierLine        string
	mode            permission.Mode
	costLine        string
	costPct         int // spend as a percentage of the /budget ceiling; 0 when ungoverned
	turnIn, turnOut int
	callTokens      int
	ctxWindow       int

	// callEstimated marks occupancy that came from the local estimator
	// because the provider reported none. It is displayed as approximate,
	// since the estimator is measured to run low (docs/estimator.md).
	callEstimated bool
	updateAvail   string

	// moves is every rung the session landed on after a switch, in order:
	// the status bar's routing-history dots. /why keeps the reasons; this
	// keeps the shape of the day.
	moves []int

	// sessionAt anchors the status clock: how long this session has been
	// open, not how long a turn has run.
	sessionAt time.Time

	// The streaming-rate sparkline: samples holds recent tokens-per-second
	// estimates (chars/4, which is why the readout says ~), tokChars counts
	// stream bytes since tokAt.
	samples  []float64
	tokChars int
	tokAt    time.Time

	history   []string
	histIdx   int
	sugSel    int
	sugClosed bool

	// pendingShell holds ! command transcripts awaiting the next turn, and
	// the mention fields back @path completion (tui_mentions.go).
	pendingShell  []string
	mentionSel    int
	mentionList   []string
	mentionListAt time.Time

	// routeLog records every tier move this session, for /why. The transcript
	// scrolls; the question "how did I end up on t3" should not.
	routeLog []string

	// race is the paired trial in flight, nil otherwise; raceLog keeps each
	// verdict's one-line summary for /why, the way routeLog keeps moves.
	race    *raceRun
	raceLog []string

	// bisect is the checkpoint bisect in flight, nil otherwise. It holds
	// busy the way a race does: the tree is being rewritten under the
	// probes, and a turn started mid-bisect would edit a reconstruction.
	bisect *bisectRun

	// bisectHinted marks that a red turn-end watch verdict has already
	// named /bisect once; the lesson is not repeated.
	bisectHinted bool

	// Reverse history search (tui_history.go).
	histSearch bool
	histQuery  string
	histMatch  int

	// Transcript search, ctrl+f (tui_search.go).
	trSearch  bool
	trQuery   string
	trMatches []int
	trMatch   int

	// custom holds the markdown-file commands loaded at startup
	// (tui_custom.go).
	custom []customCommand

	dlg  dialog
	full fullscreen

	workspaceRuntime    *workspaceRuntime
	workspaceGeneration uint64
	lspGeneration       uint64

	pendingAsk chan permission.Response

	// pendingQuestion is the ask tool's open question, held so a quit can
	// resolve it as declined: the loop is blocked on this channel, and an
	// exit that left it hanging would leave the turn unable to end.
	pendingQuestion chan tools.Answer

	restoreTier      *config.Tier
	restoreBinding   agent.Binding
	restoreSticky    route.StickySnapshot
	restoreStickySet bool
	lastTitle        string
	quitArmed        bool
	quitting         bool

	// watchFails is the last /watch run's failure count for the status
	// chip: 0 is green, -1 means the verifier itself could not run.
	watchFails int

	turnCtx        context.Context
	turnGeneration uint64
	turnPlanning   bool

	// An operation owns the same busy/cancel surface as a turn while work that
	// can replace or materially mutate the session runs off the UI goroutine.
	// It serializes /clear, /resume, /fork, /retry, /compact, /learn, and race
	// setup, plus extension and advisor lifecycle actions, and makes every late
	// result conditional on the launching session.
	operationActive     bool
	operationCancelling bool
	operationGeneration uint64
	operationSourceID   string
	operationName       string
	initialCmd          tea.Cmd
}

// runTUI is the Bubble Tea front end: same wiring as the REPL, with the
// observer and asker pointed at the program instead of stdin/stdout.
func runTUI(
	loop *agent.Loop,
	store *session.Store,
	cfg *config.Config,
	cat *catalog.Catalog,
	capability execution.Capability,
	workspace string,
	tier config.Tier,
	reg *providers,
	sticky *route.Sticky,
	routeDec *route.Decision,
	sess *session.Session,
	resumed bool,
	updateCheck bool,
	trustStore *trust.Store,
	trustErr error,
	mcpEnv *mcpState,
	lspServer *lsp.Server,
	lspNote string,
	undoRec *checkpoint.Recorder,
	agents []delegate.Agent,
	agentNotes []string,
	budget *budgetState,
	skillList []skills.Skill,
	onboarded bool,
	questions *questionRelay,
	rules *ruleSet,
) error {
	// Background detection uses COLORFGBG rather than an OSC query: querying
	// the terminal races Bubble Tea for stdin and, on a terminal that does not
	// answer, stalls the first paint (§14's 50ms). Absent the variable, dark
	// is the default. A theme chosen with /theme is saved to the config and
	// beats detection: the user said, the terminal only hinted.
	dark := detectDark()
	switch cfg.Theme {
	case "dark":
		dark = true
	case "light":
		dark = false
	}
	th := themeFor(dark)
	md := newMarkdown(100, dark)
	ta := newTextarea()

	obs := &tuiObserver{}
	app := &tuiApp{
		rules:       rules,
		loop:        loop,
		store:       store,
		config:      cfg,
		catalog:     cat,
		tier:        tier,
		runtimeTier: tier,
		providers:   reg,
		capability:  capability,
		workspace:   workspace,
		route:       routeDec,
		sticky:      sticky,
		obs:         obs,
		trust:       trustStore,
		mcp:         mcpEnv,
		lsp:         optionalLSPRuntime(lspServer),
		lspNote:     lspNote,
		undo:        undoRec,
		agents:      agents,
		agentNotes:  agentNotes,
		skills:      skillList,
		budget:      budget,
		caches:      newCacheSet(tier.Target, loop.Cache),
		onboarded:   onboarded,
	}
	if trustErr != nil {
		app.trustErr = trustErr.Error()
	}
	// The schedule ledger rides the per-workspace directory the session logs
	// already live in. A ledger that will not load costs the feature, never
	// the session: the commands say why, and nothing fires.
	if dir, err := store.WorkspaceDir(workspace); err != nil {
		app.schedulesErr = ": " + err.Error()
	} else if ledger, err := schedule.Open(dir); err != nil {
		if errors.Is(err, schedule.ErrLocked) {
			app.schedulesErr = ": another sb process in this workspace holds them"
		} else {
			app.schedulesErr = ": " + err.Error()
		}
	} else {
		app.schedules = ledger
		defer ledger.Close()
	}
	if app.lsp != nil {
		app.lspProblems, app.lspProblemsCancel = app.lsp.Problems().Subscribe()
		defer app.lspProblemsCancel()
	}

	m := newTUIModel(app, th, md, ta)

	// The mouse is on unless it was turned off: the wheel scrolls the
	// transcript and a click expands a rail, and drag-to-select still works
	// through the terminal's modifier (shift, option, or fn). /mouse off
	// hands the mouse back to the terminal entirely.
	opts := []tea.ProgramOption{tea.WithAltScreen()}
	if cfg.MouseOn() {
		opts = append(opts, tea.WithMouseCellMotion())
	}
	p := tea.NewProgram(m, opts...)
	obs.p = p
	app.p = p
	// Subagent rails render through the raw observer, not the watcher:
	// a delegate's stumbles must never escalate the primary.
	subagentForward.set(obs)
	app.watcher = newWatcher(obs, sticky, len(cfg.Tiers)-1, app.moveTo)
	// The setting outlives the process, so a rebuilt watcher inherits it.
	app.watcher.setPaused(!cfg.RouteAutoOn())
	loop.SetObserver(app.watcher)
	loop.Asker = &tuiAsker{p: p}
	// The relay was installed as the registry's questioner before the servers
	// connected; this is the moment it gets something to relay to, because a
	// dialog needs the running program.
	questions.set(&tuiQuestioner{p: p})
	// The injection seam is composed once and never swapped: the advisor and
	// the watch each contribute nothing while off.
	loop.Inject = app.inject
	// The ladder answers a round the bound rung will not take: a request it
	// cannot hold, or a target that did not answer. Both checks a move makes
	// are made here too, and a pin refuses relief outright.
	loop.Relief = app.relief
	// An external tool's pictures are queued only when the bound rung can see
	// one, and the check reads the live binding because a move changes it.
	loop.Tools.SetVision(app.targetReadsImages)

	m.addBanner(sess, resumed)
	// Startup notes render into the model directly, because the program is
	// not consuming messages yet; whatever a server says later arrives as a
	// notice through the observer, which is how the user learns a server
	// died an hour into the session.
	startupNotes, droppedStartupNotes := mcpEnv.attachCounted(func(n mcpNote) {
		obs.Notice(n.level, n.text)
	})
	app.startupNotes = aggregateStartupNotes(startupNotes, droppedStartupNotes)
	addStartupNoteReport(m, app.startupNotes)
	if app.schedulesErr != "" {
		m.addNotice("warn", "schedules are unavailable"+app.schedulesErr)
	}
	if routeDec != nil {
		m.addRoute(routeSummary(*routeDec), describeRoute(*routeDec))
	}
	if resumed {
		m.replayHistory(sess.State())
	}

	var initial []tea.Cmd
	if app.lspProblems != nil {
		initial = append(initial, waitLSPProblems(app.lsp, app.lspProblems))
	}
	// The schedule poller starts here and re-arms itself from its handler
	// until the program ends; a ledger that did not load has nothing to fire
	// and gets no clock.
	if app.schedules != nil {
		initial = append(initial, scheduleTick())
	}
	if updateCheck {
		initial = append(initial, startupUpdate(cfg))
	}
	// An advisor slot in the config is the standing request to watch every
	// session; /advisor off remains the per-session override.
	if _, bound := cfg.Slots["advisor"]; bound {
		ctx, generation, sourceID, startErr := m.startOperation("advisor on")
		if startErr != nil {
			m.addNotice("error", "advisor could not start: "+startErr.Error())
		} else {
			initial = append(initial, startAdvisor(ctx, app, generation, sourceID))
		}
	}
	// The tab's title answers "which terminal was that" for a user with six
	// of them: this workspace, this tier. It goes through syncTitle so the
	// startup title and every later update are the same format by
	// construction.
	if cmd := m.syncTitle(); cmd != nil {
		initial = append(initial, cmd)
	}
	m.initialCmd = tea.Batch(initial...)

	_, err := p.Run()
	return err
}

// detectDark reads COLORFGBG, whose last field is the background color index.
func detectDark() bool {
	fgbg := os.Getenv("COLORFGBG")
	if fgbg == "" {
		return true
	}
	last := fgbg
	if i := strings.LastIndex(fgbg, ";"); i >= 0 {
		last = fgbg[i+1:]
	}
	n, err := strconv.Atoi(last)
	if err != nil {
		return true
	}
	// 0-6 and 8 are dark backgrounds; 7 and 9-15 are light.
	return n < 7 || n == 8
}

func themeFor(dark bool) *theme {
	if dark {
		return darkTheme()
	}
	return lightTheme()
}

// newTextarea builds the prompt box: enter submits, newline is a modifier
// chord, and the box grows with its content. The bubbles defaults paint the
// cursor line with their own background; that slab is cleared here and the
// composer's frame does the marking instead.
func newTextarea() textarea.Model {
	ta := textarea.New()
	ta.Prompt = "› "
	ta.Placeholder = "describe a task · / commands · @ files · ! shell"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.SetHeight(1)
	ta.SetWidth(94)
	ta.KeyMap.InsertNewline.SetKeys("ctrl+j", "alt+enter")
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.BlurredStyle.CursorLine = lipgloss.NewStyle()
	ta.Focus()
	return ta
}

// newTUIModel assembles the model around an app. It is separate from runTUI so
// tests can drive the model without a terminal.
func newTUIModel(app *tuiApp, th *theme, md *markdown, ta textarea.Model) *tuiModel {
	if app.watchSt == nil {
		app.watchSt = &watchState{}
	}
	app.runtimeMu.Lock()
	if app.runtimeTier.ID == "" {
		app.runtimeTier = app.tier
	}
	app.runtimeMu.Unlock()
	m := &tuiModel{
		app:              app,
		th:               th,
		md:               md,
		ta:               ta,
		spin:             spinner.New(spinner.WithSpinner(spinner.Dot)),
		tierLine:         app.tierLine(),
		mode:             app.loop.Perms.Mode(),
		history:          loadHistory(app.workspace),
		custom:           loadCustomCommands(app.workspace),
		sessionAt:        time.Now(),
		workspaceRuntime: newWorkspaceRuntime(app.workspace),
	}
	m.histIdx = len(m.history)
	m.tr = newTranscript(100, th, md)
	m.refreshCost(app.loop.Session.State())
	m.refreshCtxWindow()
	return m
}

// initialCmd is set by runTUI before the program starts; Init hands it to tea.
func (m *tuiModel) Init() tea.Cmd { return m.initialCmd }

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.tr.setWidth(msg.Width)
		m.ta.SetWidth(msg.Width - 6) // margin, frame, and padding
		// A narrower pane rewraps what is already typed, so the prompt has to
		// be resized against the new width rather than the one it grew under.
		m.growInput()
		return m, m.syncTitle()

	case tea.MouseMsg:
		if m.full != nil {
			return m, m.full.mouse(msg)
		}
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			m.tr.scrollBy(3)
		case tea.MouseButtonWheelDown:
			m.tr.scrollBy(-3)
		case tea.MouseButtonLeft:
			if m.dlg != nil {
				return m, nil
			}
			switch msg.Action {
			case tea.MouseActionPress:
				// A press starts a possible selection; whether it becomes one
				// or stays a click is the motion's call.
				m.tr.beginSelect(m.tr.lineAt(msg.Y))
			case tea.MouseActionMotion:
				m.tr.extendSelect(m.tr.lineAt(msg.Y))
			case tea.MouseActionRelease:
				if m.tr.selOn && m.tr.selMoved {
					cmd := m.copySelection()
					m.tr.clearSelect()
					return m, cmd
				}
				m.tr.clearSelect()
				// A click on a tool rail or a route line toggles it, the same
				// toggle ctrl+o applies to the most recent one: the transcript
				// is directly manipulable where it has something to show.
				if i := m.tr.entryAt(msg.Y); i >= 0 {
					if e := m.tr.entries[i]; e.kind == kindTool || e.kind == kindRoute {
						e.expanded = !e.expanded
						m.tr.invalidate(i)
					}
				}
			}
		}
		return m, nil

	case tea.KeyMsg:
		return m, m.key(msg)

	case spinner.TickMsg:
		if m.busy {
			var cmd tea.Cmd
			m.spin, cmd = m.spin.Update(msg)
			m.sampleRate()
			return m, tea.Batch(cmd, m.syncTitle())
		}
		return m, m.syncTitle()

	case deltaMsg:
		m.onDelta(msg)
		return m, nil

	case toolStartMsg:
		m.onToolStart(msg)
		return m, nil

	case toolEndMsg:
		m.onToolEnd(msg)
		return m, nil

	case noticeMsg:
		if msg.operation != 0 {
			return m, m.onOperationNotice(msg)
		}
		m.addNotice(msg.level, msg.text)
		return m, nil

	case auditReportMsg:
		return m, m.onAuditReport(msg)

	case watchReportMsg:
		m.onWatchReport(msg)
		return m, nil

	case scheduleTickMsg:
		return m, m.fireScheduled()

	case clipboardMsg:
		m.addNotice("", fmt.Sprintf("copied %d lines from the transcript", msg.lines))
		return m, nil

	case bisectProbeMsg:
		m.onBisectProbe(msg)
		return m, nil

	case bisectDoneMsg:
		return m, m.onBisectDone(msg)

	case retryStartMsg:
		return m, m.retryStart(msg)

	case usageMsg:
		m.turnIn += msg.u.Usage.InputTokens + msg.u.Usage.CacheWriteTokens
		m.turnOut += msg.u.Usage.OutputTokens
		m.callTokens = msg.u.Usage.InputTokens + msg.u.Usage.CacheReadTokens + msg.u.Usage.CacheWriteTokens
		m.callEstimated = false
		if m.callTokens == 0 {
			// The server reported nothing. Not every compatible endpoint
			// answers with a usage block, and occupancy that stays at zero
			// reads as an empty window: the meter shows nothing and
			// auto-compaction, which is gated on it, never fires. An estimate
			// that is known to run low is a worse number than the provider's
			// and a far better one than pretending the context is empty.
			m.callTokens = m.estimatedOccupancy()
			m.callEstimated = m.callTokens > 0
		}
		// The loop's goroutine reads this at round boundaries to decide
		// whether a boundary is close enough to warn about. It is published
		// here because this is where the number is known, and read through a
		// lock because the two goroutines are different ones.
		m.app.publishOccupancy(m.callTokens, m.ctxWindow)
		return m, nil

	case askMsg:
		m.pendingAsk = msg.respond
		m.dlg = newPermissionDialog(msg.req, msg.out, msg.respond)
		m.ring()
		return m, nil

	case questionMsg:
		if m.dlg != nil {
			// Something already owns the input zone. The ask tool cannot
			// overlap itself, but an MCP server may elicit at any moment, and
			// replacing a permission prompt with a question would strand the
			// answer the loop is blocked on. Closing the channel says nobody
			// could be asked, which is the truth and is not the same as the
			// user declining.
			close(msg.respond)
			return m, nil
		}
		m.pendingQuestion = msg.respond
		m.dlg = newQuestionDialog(msg.q, msg.respond)
		m.ring()
		return m, nil

	case pickerMsg:
		m.dlg = &pickerDialog{title: msg.title, items: msg.items, onPick: msg.action}
		return m, nil

	case workspaceOpenedMsg:
		return m, m.onWorkspaceOpened(msg)

	case workspaceFilteredMsg:
		return m, m.onWorkspaceFiltered(msg)

	case workspacePreviewMsg:
		return m, m.onWorkspacePreview(msg)

	case workspaceCopiedMsg:
		m.onWorkspaceCopied(msg)
		return m, nil

	case workspaceEditorReadyMsg:
		return m, m.onWorkspaceEditorReady(msg)

	case workspaceEditorDoneMsg:
		return m, m.onWorkspaceEditorDone(msg)

	case workspaceInvalidatedMsg:
		if m.workspaceRuntime != nil {
			m.workspaceRuntime.invalidate()
		}
		if view, ok := m.full.(*lspView); ok {
			view.stale = true
		}
		return m, nil

	case lspLoadedMsg:
		return m, m.onLSPLoaded(msg)

	case lspProblemsChangedMsg:
		return m, m.onLSPProblemsChanged(msg)

	case lspCopiedMsg:
		m.onLSPCopied(msg)
		return m, nil

	case lspEditorReadyMsg:
		return m, m.onLSPEditorReady(msg)

	case lspEditorDoneMsg:
		return m, m.onLSPEditorDone(msg)

	case workflowDoneMsg:
		return m, m.onWorkflowDone(msg)

	case textPromptMsg:
		m.dlg = newTextDialog(msg)
		return m, nil

	case secretPromptMsg:
		m.dlg = newSecretDialog(msg.ref, msg.storeName, func(value string) tea.Cmd {
			store := storeSecretCmd(m.app.providers, msg.ref, msg.writer, msg.storeName, value)
			if msg.then != nil {
				return tea.Sequence(store, msg.then)
			}
			return store
		})
		return m, nil

	case turnDoneMsg:
		m.ring()
		return m, tea.Batch(m.onTurnDone(msg), m.syncTitle())

	case turnPlanMsg:
		return m, m.onTurnPlan(msg)

	case tierNowMsg:
		// The policy moved the primary mid-turn: the junction marker wears
		// the destination rung's heat, the same color every routing surface
		// speaks. The warmth the move priced rides beside it, in the
		// transcript and in /why's record both.
		m.tr.finalize(m.tr.last())
		m.app.tier = msg.tier
		m.tr.add(&entry{kind: kindNotice, level: "route", text: msg.line, rank: msg.rank})
		m.routeLog = append(m.routeLog, msg.line)
		if msg.abandoned != "" {
			m.addInfo(msg.abandoned)
			m.routeLog = append(m.routeLog, msg.abandoned)
		}
		m.recordMove(msg.rank)
		m.tierLine = m.app.tierLine()
		m.refreshCtxWindow()
		return m, m.syncTitle()

	case tierSwitchMsg:
		return m, tea.Batch(m.onTierSwitch(msg), m.syncTitle())

	case overrideProbeMsg:
		return m, m.onOverrideProbe(msg)

	case sessionSwapMsg:
		return m, m.onSessionSwap(msg)

	case updateCheckMsg:
		if msg.err == nil && msg.latest != "" {
			m.updateAvail = msg.latest
			m.addNotice("", "switchboard "+msg.latest+" is available; run /update to install it")
		}
		return m, nil

	case updateAppliedMsg:
		m.updateAvail = msg.version + " ready"
		m.addNotice("", "updated to "+msg.version+" in the background; the next start runs it")
		return m, nil

	case doctorDoneMsg:
		m.addInfo(msg.report)
		return m, nil

	case copyMsg:
		what := msg.what
		if what == "" {
			what = "response"
		}
		if msg.err != nil {
			m.addNotice("error", "copy failed: "+msg.err.Error())
		} else {
			m.addNotice("", "copied "+what+" "+itoa(msg.n)+" to the clipboard")
		}
		return m, nil

	case diffLoadedMsg:
		if msg.generation != 0 && (msg.generation != m.workspaceGeneration || msg.sessionID != currentSessionID(m) || m.full != nil) {
			return m, nil
		}
		if msg.err != nil {
			m.addNotice("error", "diff failed: "+msg.err.Error())
			return m, nil
		}
		m.full = &diffView{lines: msg.lines}
		return m, nil

	case turnReviewLoadedMsg:
		if msg.generation != 0 && (msg.generation != m.workspaceGeneration || msg.sessionID != currentSessionID(m) ||
			msg.turnEpoch != m.turnGeneration || m.busy || m.turnPlanning || m.full != nil ||
			msg.bound && (msg.recorder == nil || !msg.recorder.ReviewCursorValid(msg.cursor))) {
			return m, nil
		}
		if msg.err != nil {
			m.addNotice("error", "review failed: "+msg.err.Error())
			return m, nil
		}
		m.full = &turnReviewView{index: msg.index, label: msg.label, lines: msg.lines}
		return m, nil

	case shellDoneMsg:
		return m, m.onShellDone(msg)

	case editorDoneMsg:
		m.onEditorDone(msg)
		return m, nil

	case adviceMsg:
		m.addNotice("advisor", msg.text)
		return m, nil

	case raceProbeMsg:
		return m, m.onRaceProbe(msg)

	case raceSetupMsg:
		return m, m.onRaceSetup(msg)

	case raceToolMsg:
		m.onRaceTool(msg)
		return m, nil

	case raceUsageMsg:
		m.onRaceUsage(msg)
		return m, nil

	case raceNoticeMsg:
		m.onRaceNotice(msg)
		return m, nil

	case raceArmDoneMsg:
		return m, m.onRaceArmDone(msg)

	case expandedCustomMsg:
		return m, m.enqueue(msg.prompt, "")

	case advisorReadyMsg:
		return m, m.onAdvisorReady(msg)

	case extensionActionMsg:
		return m, m.onExtensionAction(msg)

	case disarmQuitMsg:
		m.quitArmed = false
		return m, nil
	}
	return m, nil
}

// key routes one keypress. Dialogs and the fullscreen panel get first claim;
// what remains goes to the input area.
func (m *tuiModel) key(msg tea.KeyMsg) tea.Cmd {
	if m.full != nil {
		close, cmd := m.full.key(msg)
		if close {
			m.closeFullscreen()
		}
		return cmd
	}
	if m.dlg != nil {
		// A modal may not swallow the interrupt. Every dialog here returns
		// before the ctrl+c case below, so a user facing an approval they do
		// not want to grant — a subagent's, most often, since those arrive
		// unbidden — had no way out of the turn that raised it. Answering no
		// on the way past is what makes this safe rather than abrupt: the
		// loop is waiting on that channel and would otherwise block.
		if msg.String() == "ctrl+c" {
			if m.pendingAsk != nil {
				m.pendingAsk <- permission.Response{}
			}
			m.dlg = nil
			m.pendingAsk = nil
			m.pendingQuestion = nil
			return m.interrupt()
		}
		done, cmd := m.dlg.update(msg, m.th)
		if done {
			m.dlg = nil
			m.pendingAsk = nil
			m.pendingQuestion = nil
		}
		return cmd
	}
	if m.histSearch {
		m.historySearchKey(msg)
		return nil
	}
	if m.trSearch {
		m.transcriptSearchKey(msg)
		return nil
	}

	switch msg.String() {
	case "ctrl+c":
		return m.interrupt()
	case "ctrl+r":
		m.startHistorySearch()
		return nil
	case "ctrl+f":
		m.startTranscriptSearch()
		return nil
	case "esc":
		if m.busy {
			return m.interrupt()
		}
		if m.suggestionsVisible() || m.mentionsVisible() {
			// One dismissal covers both popups, and the next enter submits
			// what was typed instead of accepting a completion.
			m.sugClosed = true
			return nil
		}
		return nil
	case "shift+tab":
		// Mid-turn is exactly when a mode change earns its place: the engine
		// publishes mode and reach under one lock, every later tool call in
		// the turn is checked against the new mode, and an approval that
		// straddled the change fails closed. Clamping a wandering turn to
		// plan without killing it is the point.
		return m.cycleMode()
	case "ctrl+t":
		return m.openTierPicker()
	case "ctrl+p":
		return m.openPalette()
	case "ctrl+g":
		return m.openEditor()
	case "ctrl+o":
		if i := m.tr.lastExpandable(); i >= 0 {
			e := m.tr.entries[i]
			e.expanded = !e.expanded
			m.tr.invalidate(i)
		}
		return nil
	case "pgup":
		m.tr.scrollBy(m.pageSize())
		return nil
	case "pgdown":
		m.tr.scrollBy(-m.pageSize())
		return nil
	case "shift+up":
		// The wheel's granularity on keys: several terminals keep pgup for
		// their own scrollback, and the plain arrows are the composer's.
		m.tr.scrollBy(3)
		return nil
	case "shift+down":
		m.tr.scrollBy(-3)
		return nil
	case "ctrl+s":
		return m.steerKey()
	case "ctrl+u":
		m.tr.scrollBy(m.pageSize() / 2)
		return nil
	case "ctrl+d":
		m.tr.scrollBy(-m.pageSize() / 2)
		return nil
	case "home":
		// The endpoints of the scroll story: home is the session's opening,
		// end is where the work is. Both are one press because reaching
		// either by page is a chore proportional to the day's length.
		m.tr.scrollBy(len(m.tr.flat))
		return nil
	case "end":
		m.tr.scrollToBottom()
		return nil
	case "alt+1", "alt+2", "alt+3", "alt+4", "alt+5", "alt+6", "alt+7", "alt+8", "alt+9":
		// The ladder under the fingers: alt+N jumps straight to rung N.
		// Plain digits belong to the composer; the modifier is what makes
		// a rung switch deliberate rather than a typo.
		pressed := msg.String()
		idx := int(pressed[len(pressed)-1] - '1')
		if idx >= len(m.app.config.Tiers) {
			return noticeCmd("", fmt.Sprintf("the ladder has %d rungs; alt+%c names none", len(m.app.config.Tiers), pressed[len(pressed)-1]))
		}
		if m.busy {
			return noticeCmd("warn", "a turn is running; esc to interrupt it first")
		}
		return m.switchTier(m.app.config.Tiers[idx].ID)
	}

	if m.suggestionsVisible() {
		switch msg.String() {
		case "up":
			m.sugSel--
			if m.sugSel < 0 {
				m.sugSel = len(m.suggestions()) - 1
			}
			return nil
		case "down":
			m.sugSel = (m.sugSel + 1) % len(m.suggestions())
			return nil
		case "tab":
			m.acceptSuggestion()
			return nil
		case "enter":
			if !m.exactCommand() {
				m.acceptSuggestion()
				return nil
			}
			return m.submit()
		}
	}

	if m.mentionsVisible() {
		switch msg.String() {
		case "up":
			m.mentionSel--
			if m.mentionSel < 0 {
				m.mentionSel = len(m.mentionMatches()) - 1
			}
			return nil
		case "down":
			m.mentionSel = (m.mentionSel + 1) % len(m.mentionMatches())
			return nil
		case "tab", "enter":
			m.acceptMention()
			return nil
		}
	}

	switch msg.String() {
	case "enter":
		// A trailing backslash is a line continuation, the one multiline
		// route that works in every terminal ever made.
		if v := m.ta.Value(); strings.HasSuffix(v, "\\") {
			m.ta.SetValue(strings.TrimSuffix(v, "\\") + "\n")
			m.ta.CursorEnd()
			m.growInput()
			return nil
		}
		return m.submit()
	case "up":
		if !strings.Contains(m.ta.Value(), "\n") {
			m.historyMove(-1)
			return nil
		}
	case "down":
		if !strings.Contains(m.ta.Value(), "\n") {
			m.historyMove(1)
			return nil
		}
	}

	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	m.sugClosed = false
	m.sugSel = 0
	m.growInput()
	return cmd
}

func (m *tuiModel) pageSize() int {
	h := m.height - 8
	if h < 4 {
		return 4
	}
	return h
}

// interrupt cancels a running turn; at the prompt it clears the input, and a
// second ctrl-c leaves.
func (m *tuiModel) interrupt() tea.Cmd {
	if m.bisect != nil && m.bisect.cancel != nil {
		m.bisect.cancelled = true
		m.bisect.cancel()
		m.addNotice("", "cancelling the bisect; the workspace is being restored")
		return nil
	}
	if m.race != nil && m.race.cancel != nil {
		m.race.cancelled = true
		m.race.cancel()
		m.addNotice("", "cancelling the race; the session stays where it was")
		return nil
	}
	if m.operationActive && m.turnCancel != nil {
		name := m.operationName
		if !m.operationCancelling {
			m.operationCancelling = true
			m.turnCancel()
			// Keep exclusive ownership until the command reports completion. A
			// metered provider may ignore cancellation long enough to settle its
			// durable attempt; a fork in that gap would copy pending debt that is
			// later settled only on the source.
			m.addNotice("", "cancelling "+name+"; waiting for its ledger to settle")
		}
		return nil
	}
	if m.busy && m.turnCancel != nil {
		m.turnCancel()
		if m.turnPlanning {
			// Invalidate the async result before freeing the prompt. A provider
			// probe is allowed to return after cancellation; its old generation
			// must never bind a target or launch a model call.
			m.turnGeneration++
			m.turnPlanning = false
			m.busy = false
			m.turnCancel = nil
			m.turnCtx = nil
			m.addNotice("", "routing cancelled; nothing was sent")
			return m.nextQueuedTurn()
		}
		m.addNotice("", "cancelling the turn; the session stays resumable")
		return nil
	}
	if m.ta.Value() != "" {
		m.ta.Reset()
		m.growInput()
		m.quitArmed = false
		return nil
	}
	if m.quitArmed {
		if m.pendingAsk != nil {
			m.pendingAsk <- permission.Response{}
		}
		if m.pendingQuestion != nil {
			m.pendingQuestion <- tools.Answer{Declined: true}
		}
		m.quitting = true
		return tea.Quit
	}
	m.quitArmed = true
	m.addNotice("", "ctrl-c again to exit")
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return disarmQuitMsg{} })
}

// submit dispatches the input: a slash command, a tier-prefixed prompt
// (/t3 <prompt> runs this one prompt on t3), or a plain turn.
func (m *tuiModel) submit() tea.Cmd {
	v := strings.TrimSpace(m.ta.Value())
	if v == "" {
		return nil
	}
	m.ta.Reset()
	m.growInput()
	m.sugClosed = false
	m.sugSel = 0
	// Consecutive duplicates collapse: resubmitting "go on" five times is one
	// history entry, not five up-arrow presses of the same thing.
	if len(m.history) == 0 || m.history[len(m.history)-1] != v {
		m.history = append(m.history, v)
		appendHistory(m.app.workspace, v)
	}
	m.histIdx = len(m.history)
	m.quitArmed = false

	if strings.HasPrefix(v, "!") {
		return m.runShell(strings.TrimSpace(strings.TrimPrefix(v, "!")))
	}
	if strings.HasPrefix(v, "/") {
		return m.runSlash(v)
	}
	return m.enqueue(v, "")
}

// enqueue starts the turn now or lines it up behind the running one. Planning
// counts as running: a turn whose route is still being probed is a turn about
// to exist, and a second start would race it.
func (m *tuiModel) enqueue(prompt, override string) tea.Cmd {
	if m.busy || m.turnPlanning {
		m.queue = append(m.queue, prompt)
		m.addNotice("", "queued; it runs when the current turn finishes")
		return nil
	}
	return m.startTurn(prompt, override)
}

func (m *tuiModel) startTurn(prompt, override string) tea.Cmd {
	var overrideTier config.Tier
	if override != "" {
		var ok bool
		overrideTier, ok = m.app.config.Tier(override)
		if !ok {
			return noticeCmd("error", "no tier "+override+" is configured; try /tiers")
		}
	}

	// The transcript shows what was typed; the model gets that plus what the
	// @mentions attach and what recent ! commands produced. Expansion happens
	// here, not at submit, so a queued prompt reads its files when it runs.
	m.addUser(prompt)
	expanded, images := m.expandMentions(prompt)
	prompt = m.watchContext(m.adviceContext(m.shellContext(expanded)))
	launch := func(p string) tea.Cmd {
		if override != "" {
			return m.launchOverrideTurn(p, images, overrideTier)
		}
		return m.launchTurn(p, images)
	}
	// The scan runs on the expanded prompt, because an @mentioned .env or a
	// `!env` transcript is exactly the outbound copy a key rides in on.
	if leaks := credential.ScanPrompt(prompt); len(leaks) > 0 {
		return m.openSecretGate(leaks, prompt, launch)
	}
	return launch(prompt)
}

// launchOverrideTurn uses the same fully expanded opening as an automatically
// routed turn, but pins feasibility to the requested rung for this turn only.
func (m *tuiModel) launchOverrideTurn(prompt string, images []provider.Image, tier config.Tier) tea.Cmd {
	ctx, generation := m.startPlanning()
	sticky := m.app.sticky
	unstamped := turnOpening(prompt, images)
	return func() tea.Msg {
		result := overrideProbeMsg{generation: generation, prompt: prompt, images: images}
		if err := ctx.Err(); err != nil {
			result.err = err
			return result
		}
		opening, err := stampTurnOpening(m.app.loop.Session, unstamped)
		if err != nil {
			result.err = err
			return result
		}
		result.opening = opening
		plan := prospectiveTurnPlan(m.app.loop, sticky, opening, m.app.workspace)
		result.plan = plan
		rank := m.app.rankOf(tier)
		if rank < 0 {
			result.err = fmt.Errorf("the requested tier %s is not on the configured ladder", tier.ID)
			return result
		}
		probed, client, note, err := m.app.providers.probeTierFallbackFeasible(ctx, tier, func(candidate config.Tier) error {
			return checkTurnFeasible(m.app.loop, m.app.catalog, m.app.providers, m.app.budget, m.app.config.Destinations, candidate, rank, plan, opening)
		})
		if err != nil {
			result.err = fmt.Errorf("the requested tier %s cannot serve the turn: %w", tier.ID, err)
			return result
		}
		if err := ctx.Err(); err != nil {
			result.err = err
			return result
		}
		retargetTurnPlan(&plan, m.app.loop, m.app.catalog, m.app.caches, probed, rank, opening)
		result.plan = plan
		result.tier, result.client, result.note = probed, client, note
		return result
	}
}

// startPlanning gives every async route/probe a cancellable generation that
// stays with the model turn if planning succeeds.
func (m *tuiModel) startPlanning() (context.Context, uint64) {
	ctx, cancel := context.WithCancel(context.Background())
	m.turnGeneration++
	m.turnCtx = ctx
	m.turnCancel = cancel
	m.turnPlanning = true
	m.busy = true
	return ctx, m.turnGeneration
}

func (m *tuiModel) finishPlanning() {
	if m.turnCancel != nil {
		m.turnCancel()
	}
	m.turnCancel = nil
	m.turnCtx = nil
	m.turnPlanning = false
	m.busy = false
}

// startOperation gives an asynchronous session-affecting action exclusive
// ownership of the prompt and a cancellable generation. The model is the sole
// caller, so the check and claim are atomic with respect to every UI command.
func (m *tuiModel) startOperation(name string) (context.Context, uint64, string, error) {
	if m.busy || m.turnPlanning || m.operationActive {
		return nil, 0, "", fmt.Errorf("a turn or session operation is already running")
	}
	if m.app == nil || m.app.loop == nil || m.app.loop.Session == nil {
		return nil, 0, "", fmt.Errorf("there is no active session")
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.operationGeneration++
	m.operationActive = true
	m.operationCancelling = false
	m.operationSourceID = m.app.loop.Session.ID()
	m.operationName = name
	m.turnCtx = ctx
	m.turnCancel = cancel
	m.busy = true
	return ctx, m.operationGeneration, m.operationSourceID, nil
}

func (m *tuiModel) operationMatches(generation uint64, sourceID string) bool {
	return generation != 0 && m.operationActive &&
		generation == m.operationGeneration && sourceID == m.operationSourceID &&
		m.app != nil && m.app.loop != nil && m.app.loop.Session != nil &&
		m.app.loop.Session.ID() == sourceID && !m.turnPlanning
}

// finishOperation releases exclusive ownership. keepBusy hands ownership to a
// race whose arms are about to launch; ordinary completions free the prompt.
func (m *tuiModel) finishOperation(generation uint64, keepBusy bool) bool {
	if generation == 0 || !m.operationActive || generation != m.operationGeneration {
		return false
	}
	if m.turnCancel != nil {
		m.turnCancel()
	}
	m.turnCancel = nil
	m.turnCtx = nil
	m.operationActive = false
	m.operationCancelling = false
	m.operationSourceID = ""
	m.operationName = ""
	m.busy = keepBusy
	return true
}

func (m *tuiModel) onOperationNotice(msg noticeMsg) tea.Cmd {
	if !m.operationMatches(msg.operation, msg.sourceID) {
		return nil
	}
	m.finishOperation(msg.operation, false)
	if msg.text != "" {
		m.addNotice(msg.level, msg.text)
	}
	return m.nextQueuedTurn()
}

// launchTurn is startTurn's tail, split off so the secret gate can hold a
// turn while the user decides what leaves the machine.
func (m *tuiModel) launchTurn(prompt string, images []provider.Image) tea.Cmd {
	ctx, generation := m.startPlanning()
	currentTier := m.app.tier
	binding := m.app.loop.Binding()
	sticky := m.app.sticky
	unstamped := turnOpening(prompt, images)
	return func() tea.Msg {
		result := turnPlanMsg{generation: generation, prompt: prompt, images: images}
		if err := ctx.Err(); err != nil {
			result.err = err
			return result
		}
		opening, err := stampTurnOpening(m.app.loop.Session, unstamped)
		if err != nil {
			result.err = err
			return result
		}
		result.opening = opening
		if _, onLadder := m.app.config.Tier(currentTier.ID); !onLadder {
			result.plan = prospectiveTurnPlan(m.app.loop, sticky, opening, m.app.workspace)
			probed, client := currentTier, binding.Provider
			if m.app.providers != nil {
				var err error
				probed, client, err = m.app.providers.probeTier(ctx, currentTier)
				if err != nil {
					result.err = fmt.Errorf("the current target cannot serve the turn: %w", err)
					return result
				}
			}
			if err := checkTurnFeasible(m.app.loop, m.app.catalog, m.app.providers, m.app.budget, m.app.config.Destinations,
				probed, 0, result.plan, opening); err != nil {
				result.err = fmt.Errorf("the current target cannot serve the turn: %w", err)
				return result
			}
			result.tier = probed
			result.client = client
			result.err = ctx.Err()
			return result
		}
		tier, client, note, plan, err := resolveUserTurn(ctx, m.app.loop, m.app.config, m.app.catalog,
			m.app.providers, m.app.budget, m.app.caches, sticky, currentTier, binding.Provider, opening, m.app.workspace)
		result.plan = plan
		if err != nil {
			result.err = err
			return result
		}
		if err := ctx.Err(); err != nil {
			result.err = err
			return result
		}
		result.tier, result.client, result.note = tier, client, note
		return result
	}
}

func (m *tuiModel) onTurnPlan(msg turnPlanMsg) tea.Cmd {
	if msg.generation != m.turnGeneration || !m.turnPlanning {
		return nil
	}
	if msg.err != nil {
		m.finishPlanning()
		if !errors.Is(msg.err, context.Canceled) {
			m.addNotice("error", "routing refused the turn: "+msg.err.Error())
		}
		return m.nextQueuedTurn()
	}
	if msg.tier.ID != m.app.tier.ID || msg.tier.Target.ID() != m.app.tier.Target.ID() {
		pinned := m.app.sticky != nil && m.app.sticky.Pinned()
		if err := persistRuntimeBinding(m.app.loop.Session, msg.tier, pinned); err != nil {
			m.finishPlanning()
			m.addNotice("error", "automatic tier selection was not saved: "+err.Error())
			return m.nextQueuedTurn()
		}
		_, oldBinding := m.app.runtimeSnapshot()
		abandoned := ""
		if oldBinding.Target.ID() != msg.tier.Target.ID() {
			abandoned = abandonedCacheNote(oldBinding.Cache, m.app.catalog, time.Now())
		}
		m.app.tier = msg.tier
		m.app.bindRuntime(msg.tier, msg.client)
		if abandoned != "" {
			m.addInfo(abandoned)
			m.routeLog = append(m.routeLog, abandoned)
			m.app.loop.Session.AppendNote("info", abandoned)
		}
		m.tierLine = m.app.tierLine()
		m.refreshCtxWindow()
		m.recordMove(m.app.rankOf(msg.tier))
	}
	if msg.note != "" {
		m.addNotice("warn", msg.note)
		m.app.loop.Session.AppendNote("warn", msg.note)
	}
	if msg.plan.Decision.Source != "" {
		m.app.route = &msg.plan.Decision
		m.app.routeFeatures = msg.plan.Features
		routeLine := fmt.Sprintf("%s: %s", msg.plan.Decision.Tier, msg.plan.Decision.Rationale)
		m.addNotice("route", routeLine)
		m.routeLog = append(m.routeLog, routeLine)
	} else {
		m.app.route = nil
		m.app.routeFeatures = msg.plan.Features
	}
	if m.app.sticky != nil {
		if rank := m.app.rankOf(m.app.tier); rank >= 0 {
			m.app.sticky.Rebase(rank)
		}
	}
	m.turnPlanning = false
	m.beginTurn(msg.opening.AuthoredText())
	m.launchModelTurn(msg.opening)
	return m.spin.Tick
}

// onOverrideProbe rebinds to the named tier for one turn, remembering what to
// restore when it ends.
func (m *tuiModel) onOverrideProbe(msg overrideProbeMsg) tea.Cmd {
	if msg.generation != m.turnGeneration || !m.turnPlanning {
		return nil
	}
	if msg.err != nil {
		m.finishPlanning()
		if !errors.Is(msg.err, context.Canceled) {
			m.addNotice("error", msg.err.Error())
		}
		return m.nextQueuedTurn()
	}
	m.applyOverrideBinding(msg)
	m.turnPlanning = false
	m.beginTurn(msg.opening.AuthoredText())
	m.launchModelTurn(msg.opening)
	return m.spin.Tick
}

func (m *tuiModel) applyOverrideBinding(msg overrideProbeMsg) {
	prev, binding := m.app.runtimeSnapshot()
	m.restoreTier = &prev
	m.restoreBinding = binding
	m.restoreStickySet = m.app.sticky != nil
	if m.app.sticky != nil {
		m.restoreSticky = m.app.sticky.Snapshot()
	}
	changed := msg.tier.ID != m.app.tier.ID || msg.tier.Target.ID() != m.app.tier.Target.ID()
	if changed {
		m.app.tier = msg.tier
		m.app.bindRuntime(msg.tier, msg.client)
		m.tierLine = m.app.tierLine()
		m.refreshCtxWindow()
	}
	rank := m.app.rankOf(msg.tier)
	if m.app.sticky != nil && rank >= 0 {
		m.app.sticky.Pin(rank)
	}
	m.app.routeFeatures = msg.plan.Features
	m.app.route = &route.Decision{
		Tier: msg.tier.ID, Target: msg.tier.Target.ID(), Confidence: 1,
		Source: route.SourceUserPin, Rationale: "one-turn tier override requested by you",
		PolicyRevision: route.PolicyRevision, EstimatedCost: msg.plan.Decision.EstimatedCost,
	}
}

func (m *tuiModel) nextQueuedTurn() tea.Cmd {
	// Turn exits that bypass the verdict — a refused route, a cancelled plan —
	// come through here; a steer caught in one still leads what runs next.
	if !m.busy && !m.turnPlanning {
		m.foldSteers()
	}
	if len(m.queue) == 0 || m.busy {
		return nil
	}
	next := m.queue[0]
	m.queue = m.queue[1:]
	return m.startTurn(next, "")
}

func (m *tuiModel) beginTurn(prompt string) {
	if m.turnCtx == nil || m.turnCancel == nil {
		ctx, cancel := context.WithCancel(context.Background())
		m.turnGeneration++
		m.turnCancel = cancel
		m.turnCtx = ctx
	}
	m.turnPlanning = false
	m.busy = true
	m.started = time.Now()
	m.turnIn, m.turnOut = 0, 0
	m.samples, m.tokChars, m.tokAt = nil, 0, time.Time{}
	m.turnPrompt = prompt
	m.turnStarted = m.app.tier
	m.turnBefore = m.app.loop.Session.State()
	m.tr.scrollToBottom()
	m.app.watchSt.beginTurn(m.turnCtx)
}

func (m *tuiModel) launchModelTurn(opening provider.Message) {
	decision := m.app.route
	if decision != nil {
		copy := *decision
		decision = &copy
	}
	features := m.app.routeFeatures
	features.RepoLanguages = append([]string(nil), features.RepoLanguages...)
	run := turnExecution{
		generation: m.turnGeneration,
		watcher:    m.app.watcher,
		advisor:    m.app.currentAdvisor(),
		startedOn:  m.turnStarted,
		before:     m.turnBefore,
		usage:      m.app.loop.Session.BeginUsageWindow(),
		started:    m.started,
		decision:   decision,
		features:   features,
	}
	go m.runTurn(m.turnCtx, opening, run)
}

// runTurn drives one turn on its own goroutine. Everything it reports arrives
// as messages; the session stays the only thing it writes.
func (m *tuiModel) runTurn(ctx context.Context, opening provider.Message, run turnExecution) {
	prompt := opening.AuthoredText()
	if run.watcher != nil {
		run.watcher.StartTurn()
	}
	if run.advisor != nil {
		run.advisor.StartTurn(prompt)
	}
	err := m.app.loop.TurnMessage(ctx, opening)

	after := m.app.loop.Session.State()
	moves := 0
	if run.watcher != nil {
		moves = run.watcher.MoveCount()
	}
	endedOn, _ := m.app.runtimeSnapshot()
	if rerr := appendRouteRecord(m.app.loop.Session, prompt, run.startedOn, endedOn, run.before, run.usage, run.started, err, run.decision, run.features, moves); rerr != nil {
		m.app.p.Send(noticeMsg{level: "warn", text: "the routing record for this turn was not saved: " + rerr.Error()})
	}
	m.app.p.Send(turnDoneMsg{generation: run.generation, err: err, after: after})
}

func (m *tuiModel) onTurnDone(msg turnDoneMsg) tea.Cmd {
	if msg.generation != m.turnGeneration {
		return nil
	}
	m.busy = false
	if m.turnCancel != nil {
		m.turnCancel()
	}
	m.turnCancel = nil
	m.turnCtx = nil
	m.turnPlanning = false
	// The final round's edits have no later round boundary; this is theirs.
	// Batched into every exit path, because a tier restore or a queued
	// prompt does not unhappen the edits.
	watchCmd := m.watchTurnEnd()
	m.tr.finalizeAll()
	m.refreshCost(msg.after)
	// Keep the completed turn's opening decision inspectable through /why.
	// The next turn replaces it before sending anything, and a manual switch
	// below replaces it with explicit user-pin provenance.

	// The working line's past tense: what ran, for how long, on how many
	// tokens, said once and left in the record. It speaks the rail's own
	// verdict language, closing the rail when one is open directly above.
	if msg.err == nil {
		done := fmt.Sprintf("%s · %s", m.turnStarted.ID, time.Since(m.started).Round(time.Second))
		if m.turnIn+m.turnOut > 0 {
			done += fmt.Sprintf(" · ↓%s ↑%s tokens", compact(m.turnIn), compact(m.turnOut))
		}
		last := m.tr.last()
		m.tr.add(&entry{kind: kindNotice, level: "done", text: done,
			rank: m.activeRank(), rail: last != nil && last.kind == kindTool})
	}
	m.samples = nil
	m.tokChars, m.tokAt = 0, time.Time{}

	switch {
	case errors.Is(msg.err, context.Canceled):
		m.addNotice("", "turn cancelled; the session is intact and can continue")
	case errors.Is(msg.err, agent.ErrRoundLimit):
		// The loop already said why.
	case msg.err != nil:
		m.addNotice("error", msg.err.Error())
	}

	// A /tN override borrows a binding and policy state for one turn. Its pin
	// prevents mid-turn moves, so restoration is an exact in-memory transaction
	// and never depends on a second provider probe.
	if m.restoreTier != nil {
		m.restoreOverride()
	}

	// A steer that outlived its turn was typed before anything queued behind
	// it, so it leads the queue rather than dying with the turn it missed.
	// This runs before the compact branch on purpose: the queue survives that
	// swap, and a folded steer is an ordinary queued prompt by then.
	m.foldSteers()

	// Auto-compaction runs ahead of the queue: a queued prompt sent into a
	// nearly-full window would inherit the failure this exists to prevent,
	// and the queue survives the swap (onSessionSwap drains it).
	if m.shouldAutoCompact() {
		pct := m.callTokens * 100 / m.ctxWindow
		m.addNotice("", fmt.Sprintf("context at %d%% of %s tokens; compacting automatically (/compact auto off disables this)",
			pct, compact(m.ctxWindow)))
		return tea.Batch(watchCmd, compactCmd(m, "", true))
	}

	if len(m.queue) > 0 {
		next := m.queue[0]
		m.queue = m.queue[1:]
		return tea.Batch(watchCmd, m.startTurn(next, ""))
	}
	return watchCmd
}

// foldSteers moves undrained steers to the head of the prompt queue. It runs
// at turn end, before the queue is consulted.
func (m *tuiModel) foldSteers() {
	if steers := m.app.takeSteers(); len(steers) > 0 {
		m.queue = append(steers, m.queue...)
	}
}

// estimatedOccupancy is what the next request would carry, counted locally.
// It is the fallback for a server that reports no usage at all, and it counts
// the same three zones /context breaks out: the frozen system and tool
// definitions, plus the conversation that grows.
func (m *tuiModel) estimatedOccupancy() int {
	state := m.app.loop.Session.State()
	return prefix.RequestTokens(provider.Request{
		System:   m.app.loop.System,
		Tools:    m.app.loop.Tools.Definitions(),
		Messages: state.Messages,
	})
}

// shouldAutoCompact decides at turn end. callTokens is the size of the last
// request the provider actually saw — input plus cache reads and writes —
// which is the honest measure of occupancy, and the reason this waits for a
// turn boundary rather than trusting a mid-turn estimate the estimator is
// known to undercount (docs/estimator.md).
func (m *tuiModel) shouldAutoCompact() bool {
	cfg := m.app.config
	if !cfg.CompactAuto || m.ctxWindow <= 0 || m.callTokens <= 0 {
		return false
	}
	at := cfg.CompactAtPercent
	if at == 0 {
		at = 85
	}
	return m.callTokens >= m.ctxWindow*at/100
}

func (m *tuiModel) restoreOverride() {
	tier := *m.restoreTier
	m.app.runtimeMu.Lock()
	m.app.loop.Bind(m.restoreBinding)
	m.app.runtimeTier = tier
	m.app.runtimeMu.Unlock()
	m.app.tier = tier
	if m.restoreStickySet && m.app.sticky != nil {
		m.app.sticky.Restore(m.restoreSticky)
	}
	m.restoreTier = nil
	m.restoreBinding = agent.Binding{}
	m.restoreStickySet = false
	m.tierLine = m.app.tierLine()
	m.refreshCtxWindow()
}

func (m *tuiModel) onTierSwitch(msg tierSwitchMsg) tea.Cmd {
	if !m.operationMatches(msg.operation, msg.sourceID) {
		return nil
	}
	if m.operationCancelling {
		name := m.operationName
		m.finishOperation(msg.operation, false)
		m.addNotice("", name+" cancelled")
		return m.nextQueuedTurn()
	}
	if msg.err != nil {
		name := m.operationName
		m.finishOperation(msg.operation, false)
		if errors.Is(msg.err, context.Canceled) {
			m.addNotice("", name+" cancelled")
		} else {
			m.addNotice("error", msg.err.Error())
		}
		return m.nextQueuedTurn()
	}
	if !msg.silent {
		if err := persistRuntimeBinding(m.app.loop.Session, msg.tier, true); err != nil {
			m.finishOperation(msg.operation, false)
			m.addNotice("error", "tier switch was not saved: "+err.Error())
			return m.nextQueuedTurn()
		}
	}
	changed := msg.tier.ID != m.app.tier.ID || msg.tier.Target.ID() != m.app.tier.Target.ID()
	targetChanged := msg.tier.Target.ID() != m.app.tier.Target.ID()
	// A fallback substitution is a fact about the configured rung even when
	// two rungs ultimately share the same concrete target. Only cache
	// abandonment is conditional on the target identity changing.
	if msg.note != "" {
		m.addNotice("warn", msg.note)
		m.app.loop.Session.AppendNote("warn", msg.note)
	}
	// What the old target held warm is priced before the bind discards its
	// tracker: afterwards there is nothing left to ask. A spoken note goes
	// in the session record too, so the export and a later reading keep it.
	abandoned := ""
	if targetChanged {
		abandoned = abandonedCacheNote(m.app.loop.Binding().Cache, m.app.catalog, time.Now())
	}
	if abandoned != "" {
		m.app.loop.Session.AppendNote("info", abandoned)
	}
	pin := true
	if changed {
		m.app.bind(msg.tier, msg.client, pin)
	} else if m.app.sticky != nil {
		if rank := m.app.rankOf(msg.tier); pin && rank >= 0 {
			m.app.sticky.Pin(rank)
		} else if !pin {
			m.app.sticky.Unpin()
		}
	}
	if msg.silent && !pin {
		m.app.route = nil
	} else {
		rationale := "tier selected by you"
		if msg.silent {
			rationale = "restored after a one-turn override"
		}
		m.app.route = &route.Decision{
			Tier: msg.tier.ID, Target: msg.tier.Target.ID(), Confidence: 1,
			Source: route.SourceUserPin, Rationale: rationale, PolicyRevision: route.PolicyRevision,
		}
	}
	m.tierLine = m.app.tierLine()
	m.refreshCtxWindow()
	if changed {
		m.recordMove(m.app.rankOf(msg.tier))
	}
	if !msg.silent {
		m.tr.add(&entry{kind: kindNotice, level: "route", text: "now on " + m.tierLine,
			rank: m.app.rankOf(msg.tier)})
		m.routeLog = append(m.routeLog, "you switched to "+msg.tier.ID)
		if abandoned != "" {
			m.routeLog = append(m.routeLog, abandoned)
		}
		if targetChanged {
			m.cacheSwitchNote(msg.tier, abandoned)
		}
		m.finishOperation(msg.operation, false)
		return m.nextQueuedTurn()
	}
	// A silent switch is a /tN override restoring what it borrowed; a queued
	// prompt waits for the restore so it runs on the tier the user is on.
	m.finishOperation(msg.operation, false)
	return m.nextQueuedTurn()
}

// cacheSwitchNote says what a switch abandons: cache state is scoped to a
// target, so whatever was warm on the old one stays with it. When the old
// target's warmth can be priced honestly, the modeled number is the note;
// when it cannot - nothing observed, a metering that is not dollars, a
// value that would round to free money - the fact is stated without one.
func (m *tuiModel) cacheSwitchNote(tier config.Tier, abandoned string) {
	if abandoned != "" {
		m.addInfo(abandoned)
		return
	}
	if info, _, ok := m.app.catalog.Lookup(tier.Target); ok && !info.Free() {
		m.addInfo("a target switch leaves the previous target's cache behind")
	}
}

func (m *tuiModel) onSessionSwap(msg sessionSwapMsg) tea.Cmd {
	if msg.release != nil {
		defer msg.release()
	}
	if msg.operation != 0 {
		if !m.operationMatches(msg.operation, msg.sourceID) {
			if msg.sess != nil && msg.sess != m.app.loop.Session {
				_ = msg.sess.Close()
			}
			return nil
		}
		if m.operationCancelling {
			if msg.sess != nil && msg.sess != m.app.loop.Session {
				_ = msg.sess.Close()
			}
			m.finishOperation(msg.operation, false)
			return m.nextQueuedTurn()
		}
	} else if m.busy || m.turnPlanning || m.operationActive {
		// Synchronous swaps (currently a finished race) are valid only while
		// idle. This final guard prevents a future caller from closing the log
		// underneath a live turn.
		if msg.sess != nil && msg.sess != m.app.loop.Session {
			_ = msg.sess.Close()
		}
		m.addNotice("error", "session change refused while another operation is running")
		return nil
	}
	if msg.err != nil {
		if msg.operation != 0 {
			m.finishOperation(msg.operation, false)
		}
		m.addNotice("error", msg.err.Error())
		return m.nextQueuedTurn()
	}
	if msg.sess == nil {
		if msg.operation != 0 {
			m.finishOperation(msg.operation, false)
		}
		m.addNotice("error", "session change returned no session")
		return m.nextQueuedTurn()
	}
	m.closeFullscreen()
	m.workspaceGeneration++
	old := m.app.loop.Session
	runtimeBinding := session.RuntimeBinding{Tier: msg.tier.ID, Target: msg.tier.Target.ID(), Pinned: msg.pinned}
	if msg.preserveRuntimeTarget {
		// A fork or retry may cut before the source's latest binding record.
		// The operation nevertheless continues from the source's current durable
		// routing posture, so that posture must win over an older binding copied
		// into the child. Otherwise the live child can run on the current tier
		// while its log says to resume on the earlier one. A process-only /think
		// still stays out of the log because this reads the source WAL rather than
		// the live, parameter-adjusted msg.tier.
		durable := session.RuntimeBinding{}
		if old != nil {
			durable = old.State().RuntimeBinding
		}
		if durable.Target == "" {
			durable = msg.sess.State().RuntimeBinding
		}
		if durable.Target != "" {
			runtimeBinding = durable
		}
	}
	if err := msg.sess.AppendRuntimeBinding(runtimeBinding.Tier, runtimeBinding.Target, runtimeBinding.Pinned); err != nil {
		if msg.operation != 0 {
			m.finishOperation(msg.operation, false)
		}
		if msg.sess != m.app.loop.Session {
			_ = msg.sess.Close()
		}
		m.addNotice("error", "session runtime binding was not saved: "+err.Error())
		return m.nextQueuedTurn()
	}
	if err := m.app.loop.BindSession(msg.sess); err != nil {
		if msg.operation != 0 {
			m.finishOperation(msg.operation, false)
		}
		if msg.sess != old {
			_ = msg.sess.Close()
		}
		m.addNotice("error", "session context was not restored: "+err.Error())
		return m.nextQueuedTurn()
	}
	if m.app.caches != nil {
		m.app.caches.Reset(msg.tier.Target, cacheFor(msg.tier.Target, m.app.catalog))
	}
	m.app.bind(msg.tier, msg.client, runtimeBinding.Pinned)
	if old != nil && old != msg.sess {
		old.Close()
	}
	if !msg.keepFold {
		m.app.watchSt.takeFold()
	}
	// BindSession made the context switch indivisible: it restored the new
	// session's todos and dropped every prior-session file-read token before
	// publishing the session to the loop.
	m.tr.reset()
	// A new log is a new day for the routing dots and the clock; a resumed
	// session's earlier moves live in its record, not the bar.
	m.moves = nil
	m.sessionAt = time.Now()
	m.addBanner(msg.sess, !msg.fresh)
	if !msg.fresh {
		m.replayHistory(msg.sess.State())
	}
	if msg.note != "" {
		level := ""
		if msg.warnNote {
			level = "warn"
			_ = m.app.loop.Session.AppendNote("warn", msg.note)
		}
		m.addNotice(level, msg.note)
	}
	m.tierLine = m.app.tierLine()
	m.mode = m.app.loop.Perms.Mode()
	m.refreshCost(msg.sess.State())
	m.refreshCtxWindow()
	// The old session's occupancy does not describe the new one, and leaving
	// it would re-trigger the auto-compaction that produced this swap.
	m.callTokens = 0
	// A steer nobody drained was typed into the session this swap replaces.
	// It does not follow into the new one — the same answer undelivered tool
	// images get, and for the same reason: it answered a context that is gone.
	if dropped := m.app.takeSteers(); len(dropped) > 0 {
		m.addNotice("", fmt.Sprintf("%d steered note(s) dropped with the session they were typed into", len(dropped)))
	}
	if msg.operation != 0 {
		m.finishOperation(msg.operation, false)
	}

	// A swap that carries its own continuation runs it now; /retry's replay
	// belongs to the fork it just landed in, ahead of anything queued.
	if msg.andThen != nil {
		if !msg.continueTurn {
			return msg.andThen
		}
		_, generation := m.startPlanning()
		continuation := msg.andThen
		return func() tea.Msg {
			next := continuation()
			if retry, ok := next.(retryStartMsg); ok {
				retry.generation = generation
				return retry
			}
			return next
		}
	}

	// A compacted session does not wait to be told what it already knows:
	// with nothing queued, the continuation opens the new session's first
	// turn. Queued prompts are themselves the continuation when they exist.
	if msg.continuePrompt != "" && len(m.queue) == 0 {
		return m.startTurn(msg.continuePrompt, "")
	}

	// Prompts queued behind the turn that triggered an auto-compaction run
	// now, in the fresh context they were waiting for.
	if len(m.queue) > 0 && !m.busy {
		next := m.queue[0]
		m.queue = m.queue[1:]
		return m.startTurn(next, "")
	}
	return nil
}

// replayHistory draws a resumed session's recorded conversation, so picking up
// a session looks like continuing it rather than opening an empty window.
func (m *tuiModel) replayHistory(state session.State) {
	for _, msg := range state.Messages {
		if msg.Role == provider.RoleUser {
			// A continuity capsule is provider-visible metadata, not something
			// the user typed. Render the authored projection once so a stamped
			// opening neither leaks the hidden block nor splits multi-text input
			// into several apparent turns.
			if text := msg.AuthoredText(); text != "" {
				m.addUser(text)
			}
		}
		for _, b := range msg.Content {
			switch b := b.(type) {
			case provider.Text:
				if msg.Role == provider.RoleAssistant {
					e := m.tr.add(&entry{kind: kindAssistant, text: b.Text})
					m.tr.finalize(e)
				}
			case provider.Thinking:
				if b.Text != "" {
					e := m.tr.add(&entry{kind: kindThinking, text: b.Text})
					m.tr.finalize(e)
				}
			case provider.ToolUse:
				// A replayed session does not record which rung ran each
				// call, so history renders neutral rather than guessing.
				m.tr.add(&entry{kind: kindTool, tool: toolEntry{name: b.Name, done: true}, rank: -1})
			}
		}
	}
	m.tr.scrollToBottom()
}

// --- transcript events -----------------------------------------------------

func (m *tuiModel) onDelta(msg deltaMsg) {
	m.tokChars += len(msg.text)
	last := m.tr.last()
	want := kindAssistant
	if msg.thinking {
		want = kindThinking
	}
	if last != nil && last.live && last.kind == want {
		m.tr.appendText(len(m.tr.entries)-1, msg.text)
		return
	}
	// A new block closes the one before it: the completed text now renders
	// through glamour once instead of on every remaining delta.
	m.tr.finalize(last)
	m.tr.add(&entry{kind: want, text: msg.text, live: true})
}

// activeRank is the current tier's position on the ladder, for the heat ramp;
// an ad-hoc target (-model, a resumed unknown) has no rung and renders
// neutral.
func (m *tuiModel) activeRank() int {
	return m.app.rankOf(m.app.tier)
}

func (m *tuiModel) onToolStart(msg toolStartMsg) {
	m.tr.finalize(m.tr.last())
	m.tr.add(&entry{kind: kindTool, tool: toolEntry{id: msg.id, name: msg.name, desc: describeRequest(msg.req)}, rank: m.activeRank()})
}

func (m *tuiModel) onToolEnd(msg toolEndMsg) {
	// The call ID is the only sound correlation key when same-name tools run
	// concurrently. Keep the name fallback only for synthetic UI events whose
	// producers do not have a provider call ID.
	idx := -1
	for i := len(m.tr.entries) - 1; i >= 0; i-- {
		e := m.tr.entries[i]
		if e.kind != kindTool || e.tool.done {
			continue
		}
		if (msg.id != "" && e.tool.id == msg.id) ||
			(msg.id == "" && e.tool.id == "" && e.tool.name == msg.name) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	e := m.tr.entries[idx]
	// A finished todo call renders as the list itself rather than a verdict
	// line: the list is the result. A failed call keeps the rail so the
	// error shows the way every other tool error does.
	if msg.name == "todo" && !msg.res.IsError {
		e.kind = kindTodo
		e.todos = m.app.loop.Tools.Todos()
		m.tr.invalidate(idx)
		return
	}
	e.tool.done = true
	e.tool.failed = msg.res.IsError
	e.tool.took = msg.took
	e.tool.detail = msg.res.Content
	m.tr.invalidate(idx)
}

func (m *tuiModel) addUser(text string) {
	m.tr.add(&entry{kind: kindUser, text: text})
	m.tr.scrollToBottom()
}

func (m *tuiModel) addNotice(level, text string) {
	m.tr.finalize(m.tr.last())
	m.tr.add(&entry{kind: kindNotice, level: level, text: text})
}

func (m *tuiModel) addInfo(text string) {
	m.tr.add(&entry{kind: kindInfo, text: text})
}

func (m *tuiModel) addRoute(summary string, lines []string) {
	m.tr.add(&entry{kind: kindRoute, routeSummary: summary, routeLines: lines, rank: m.activeRank()})
}

func routeSummary(d route.Decision) string {
	return fmt.Sprintf("%s via %s (%s)", d.Tier, d.Source, d.Rationale)
}

// --- status state ----------------------------------------------------------

func (m *tuiModel) refreshCost(state session.State) {
	// The ratio resets before the branch, not inside one: a switch from a
	// priced rung to a local one must not leave "local" wearing the old
	// ceiling's warning color.
	m.costPct = 0
	spent := catalog.Money(state.AccountedCostMicroUSD())
	if spent > 0 {
		m.costLine = spent.String()
		if state.ExternalCostMicroUSD > 0 {
			m.costLine += " incl delegate/race"
		} else {
			m.costLine += " routed"
		}
		// Accumulated spend remains governed even when the active target is now
		// local or plan-metered. Hiding the ratio after a move would hide the
		// dollars the same session has already consumed.
		if m.app.budget != nil {
			if c := m.app.budget.get(); c > 0 {
				debt := m.app.budget.syncRetryDebt(state.ID, catalog.Money(state.RetryReserveMicroUSD))
				if debt > 0 {
					m.costLine += " + " + debt.String() + " reserve"
				}
				m.costLine += " of " + c.String()
				accounted := addMoney(spent, debt)
				if accounted >= c {
					m.costPct = 100
				} else {
					m.costPct = int(float64(accounted) * 100 / float64(c))
				}
			}
		}
		return
	}
	if kinds, known := routedMeteringKinds(m.app.catalog, state); known {
		if len(kinds) > 1 {
			m.costLine = "mixed: " + strings.Join(kinds, " + ")
			return
		}
		switch kinds[0] {
		case "local":
			m.costLine = "local"
		case "plan":
			m.costLine = "plan"
		case "dollar-metered":
			m.costLine = spent.String()
		case "no per-token cost":
			m.costLine = "free"
		default:
			m.costLine = "unpriced"
		}
		return
	}
	// The three zero-dollar meterings stay distinct (§4), here as everywhere:
	// a plan target consumed quota, not nothing.
	info, _, ok := m.app.catalog.Lookup(m.app.loop.Binding().Target)
	switch {
	case !ok:
		m.costLine = "unpriced"
	case info.Metering == catalog.Local:
		m.costLine = "local"
		if state.ExternalCostMicroUSD > 0 {
			m.costLine += " + " + catalog.Money(state.ExternalCostMicroUSD).String() + " delegate/race"
		}
	case info.Metering == catalog.Plan:
		m.costLine = "plan"
		if state.ExternalCostMicroUSD > 0 {
			m.costLine += " + " + catalog.Money(state.ExternalCostMicroUSD).String() + " delegate/race"
		}
	case info.Free():
		m.costLine = "free"
	default:
		m.costLine = spent.String()
		// The ceiling rides the readout so a governed session shows it at
		// rest, the same principle as the tier: visible, not on demand. The
		// percentage feeds the readout's color, so a ceiling being neared
		// warms the same way the context gauge does: the warning comes
		// before the refusal, not as it.
		if m.app.budget != nil {
			if c := m.app.budget.get(); c > 0 {
				debt := m.app.budget.syncRetryDebt(state.ID, catalog.Money(state.RetryReserveMicroUSD))
				if debt > 0 {
					m.costLine += " + " + debt.String() + " reserve"
				}
				m.costLine += " of " + c.String()
				accounted := addMoney(spent, debt)
				if accounted >= c {
					m.costPct = 100
				} else {
					m.costPct = int(float64(accounted) * 100 / float64(c))
				}
			}
		}
	}
}

// refreshCtxWindow settles how much room this target has, from the most
// direct source that has an answer.
//
// The user first when the probe's number is a metadata inference rather than
// an enforced limit: a server whose fields contradict each other is not a
// better witness than the person who configured it. An enforced window — an
// allocation, a per-request limit, the endpoint's own statement — outranks
// the declaration, because the server will hold the request to it. The
// catalog last, and its local surfaces deliberately record zero rather than
// guess. Zero at the end is unknown, and unknown is reported as unknown: the
// number gates auto-compaction, so inventing one would compact against a
// window that does not exist.
func (m *tuiModel) refreshCtxWindow() {
	target := m.app.loop.Binding().Target
	probed, enforced := m.app.providers.probedContextWindow(target)
	declared := m.app.config.ProviderForTarget(target.Provider, target.Surface).ContextWindow
	switch {
	case declared > 0 && !enforced:
		m.ctxWindow = declared
	case probed > 0:
		m.ctxWindow = probed
	case declared > 0:
		m.ctxWindow = declared
	default:
		if info, _, ok := m.app.catalog.Lookup(target); ok {
			m.ctxWindow = info.ContextWindow
			return
		}
		m.ctxWindow = 0
	}
}

// --- view ------------------------------------------------------------------

func (m *tuiModel) View() string {
	if m.quitting {
		return ""
	}
	if m.full != nil {
		return m.full.view(m.width, m.height, m.th)
	}

	inputZone := m.inputZoneView()
	chrome := 1 // status line
	if m.busy {
		chrome++
	}
	rail := m.height >= 15 // a short pane spends its rows on content
	if rail {
		chrome++
	}
	transH := m.height - lipgloss.Height(inputZone) - chrome
	if transH < 1 {
		transH = 1
	}

	parts := []string{m.tr.view(transH), inputZone}
	if m.busy {
		parts = append(parts, m.workingLine())
	}
	if rail {
		parts = append(parts, m.ctxRail())
	}
	parts = append(parts, m.statusLine())
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// inputZoneView is the composer: a rounded frame one shade off the page,
// the cursor in the active rung's heat — what you type is marked with the
// color it will run on — and the frame itself takes the permission mode's
// color the moment the mode is anything but default, so a widened posture
// is visible at the exact place the next instruction is typed. Popups dock
// above the frame.
func (m *tuiModel) inputZoneView() string {
	if m.dlg != nil {
		return m.dlg.view(m.width, m.th)
	}

	m.ta.FocusedStyle.Prompt = m.th.faint
	m.ta.BlurredStyle.Prompt = m.th.faint
	m.ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	m.ta.BlurredStyle.CursorLine = lipgloss.NewStyle()
	m.ta.FocusedStyle.Placeholder = m.th.faint
	m.ta.BlurredStyle.Placeholder = m.th.faint
	if rank := m.activeRank(); rank >= 0 && !m.busy {
		m.ta.Cursor.Style = lipgloss.NewStyle().Foreground(m.th.rung(rank).GetForeground())
	} else {
		m.ta.Cursor.Style = lipgloss.NewStyle()
	}

	frame := m.th.border.GetForeground()
	if m.mode != permission.ModeDefault {
		if chip, ok := m.th.modeChip[string(m.mode)]; ok {
			frame = chip.GetBackground()
		}
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(frame).
		Padding(0, 1).
		Width(max(m.width-4, 20))

	var parts []string
	switch {
	case m.trSearch:
		parts = append(parts, m.transcriptSearchView())
	case m.histSearch:
		parts = append(parts, m.historySearchView())
	case m.suggestionsView() != "":
		parts = append(parts, m.suggestionsView())
	case m.mentionsVisible():
		parts = append(parts, m.mentionsView())
	}
	parts = append(parts, box.Render(m.ta.View()))

	lines := strings.Split(lipgloss.JoinVertical(lipgloss.Left, parts...), "\n")
	for i, l := range lines {
		lines[i] = " " + l
	}
	return strings.Join(lines, "\n")
}

// workVerbs are the operator's verbs: what the person behind a switchboard
// did all day. One is chosen every few seconds of a running turn, so the
// working line has a pulse beyond the spinner without inventing progress.
var workVerbs = []string{"patching through", "on the line", "connecting", "holding the line", "splicing"}

// workingLine is the row that appears under the input while a turn runs:
// spinner and verb in the active rung's heat, then the rung and the clock,
// then the way out. Color answers "who is working" before text does. Token
// counts live in the completion line and /cost.
func (m *tuiModel) workingLine() string {
	verb := workVerbs[int(time.Since(m.started).Seconds()/4)%len(workVerbs)]
	who := m.spin.View() + " " + verb
	mid := ""
	if rank := m.activeRank(); rank >= 0 {
		who = m.th.rung(rank).Render(who)
		mid = m.th.dim.Render(" · " + m.app.tier.ID)
	}
	elapsed := time.Since(m.started).Round(time.Second)
	line := " " + who + mid + m.th.dim.Render(" · "+elapsed.String())
	line += m.th.faint.Render("  esc interrupts · ctrl+s steers")
	if len(m.queue) > 0 {
		line += m.th.faint.Render(fmt.Sprintf("  %d queued", len(m.queue)))
	}
	return line
}

// recordMove appends a landed switch to the session's routing history, the
// status bar's dots. Every rebind counts, however it was asked for: the dots
// have to agree with /why about how much the session moved.
func (m *tuiModel) recordMove(rank int) {
	if rank < 0 {
		return
	}
	m.moves = append(m.moves, rank)
}

// sampleRate folds the stream bytes seen since the last sample into a
// tokens-per-second estimate for the sparkline. Chars over four is an
// estimate and the readout marks it as one.
func (m *tuiModel) sampleRate() {
	if m.tokAt.IsZero() {
		m.tokAt = time.Now()
		return
	}
	since := time.Since(m.tokAt)
	if since < 400*time.Millisecond {
		return
	}
	rate := float64(m.tokChars) / 4 / since.Seconds()
	m.samples = append(m.samples, rate)
	if len(m.samples) > 10 {
		m.samples = m.samples[len(m.samples)-10:]
	}
	m.tokChars = 0
	m.tokAt = time.Now()
}

// ring speaks when the session needs its person: on the ask and on the
// turn's end. It goes to stderr because the renderer owns stdout and a BEL
// prints nothing; /notify off keeps the quiet.
func (m *tuiModel) ring() {
	if m.app.config.NotifyOn() {
		os.Stderr.WriteString("\a")
	}
}

// syncTitle keeps the terminal title naming the workspace and the active
// tier, marked while a turn runs, so the working pane is findable from a
// wall of terminals. It returns nil when nothing changed, because a title
// rewrite per tick would be chatter.
func (m *tuiModel) syncTitle() tea.Cmd {
	title := m.titleText()
	if title == m.lastTitle {
		return nil
	}
	m.lastTitle = title
	return tea.SetWindowTitle(title)
}

func (m *tuiModel) titleText() string {
	title := "sb · " + filepath.Base(m.app.workspace) + " · " + m.app.tier.ID
	if m.busy {
		title = "● " + title
	}
	return title
}

func itoa(n int) string { return fmt.Sprint(n) }

// compact renders token counts in the status line: 1234 becomes 1.2k.
func compact(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 10_000:
		return fmt.Sprintf("%dk", n/1000)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprint(n)
	}
}
