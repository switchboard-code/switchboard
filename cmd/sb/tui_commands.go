package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/hooks"
	"github.com/switchboard-code/switchboard/internal/mcp"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

// commandItem is one slash command. busySafe commands may run while a turn is
// in flight; everything else waits, because touching the session mid-turn
// would race the loop.
type commandItem struct {
	name     string
	aliases  []string
	usage    string
	desc     string
	busySafe bool
	run      func(m *tuiModel, args string) tea.Cmd
}

func commands() []commandItem {
	return []commandItem{
		{name: "help", desc: "show commands and keybindings", busySafe: true, run: cmdHelp},
		{name: "exit", aliases: []string{"quit"}, desc: "leave", busySafe: true, run: cmdExit},
		{name: "clear", aliases: []string{"new", "reset"}, desc: "start a fresh session", run: cmdClear},
		{name: "resume", usage: "[id]", desc: "pick up an earlier session", run: cmdResume},
		{name: "recap", usage: "[id]", desc: "where you left off: the last session's story from its record", busySafe: true, run: cmdRecap},
		{name: "fork", usage: "[n|pin]", desc: "branch this session, less its last n user turns, or back to a pin", run: cmdFork},
		{name: "pin", usage: "[name]", desc: "name this point in the session; /fork <name> branches back to it", run: cmdPin},
		{name: "tier", usage: "[id|auto]", desc: "switch tier, or return routing to automatic", run: cmdTier},
		{name: "tiers", desc: "show the configured ladder", busySafe: true, run: cmdTiers},
		{name: "ladder", desc: "where recorded turns opened and where they ended, summed per rung", busySafe: true, run: cmdLadder},
		{name: "why", desc: "how this tier was chosen, and what the others would have cost", busySafe: true, run: cmdWhy},
		{name: "race", usage: "[tier [tier]] <prompt>", desc: "one prompt on two rungs at once; bare form races the next rung up", run: cmdRace},
		{name: "races", usage: "[all]", desc: "every paired verdict collected, tallied by pair; all spans workspaces", busySafe: true, run: cmdRaces},
		{name: "advisor", usage: "[on|off|status]", desc: "a second model that watches and advises", busySafe: true, run: cmdAdvisor},
		{name: "audit", desc: "check the last turn's claims against what the record says it did", run: cmdAudit},
		{name: "routing", usage: "[on|off|status]", desc: "whether the policy may move the primary on its own signals", busySafe: true, run: cmdRouting},
		{name: "destinations", usage: "[<provider>… |any]", desc: "which providers this workspace's turns may reach", busySafe: true, run: cmdDestinations},
		{name: "workflow", usage: "[list|show <name>|run <name> [args]]", desc: "run a multi-stage subagent script from a file", run: cmdWorkflow},
		{name: "mode", usage: "[plan|default|acceptEdits|auto|yolo|bypass]", desc: "show or change the permission mode, including mid-turn", busySafe: true, run: cmdMode},
		{name: "cost", aliases: []string{"usage"}, usage: "[rungs|turns]", desc: "tokens and cost; rungs reprices the session per rung, turns orders its asks by bill", busySafe: true, run: cmdCost},
		{name: "estimate", usage: "[prompt]", desc: "price the next turn on every rung before it is sent", run: cmdEstimate},
		{name: "stats", usage: "[all]", desc: "every session this workspace has recorded, repriced on today's ladder; all spans workspaces", busySafe: true, run: cmdStats},
		{name: "find", usage: "[all] <text>", desc: "search recorded sessions for what was said; all spans workspaces", busySafe: true, run: cmdFind},
		{name: "cache", desc: "what the provider is believed to hold warm, and the evidence", run: cmdCache},
		{name: "notify", usage: "[on|off]", desc: "ring the bell when a turn finishes or an approval waits", busySafe: true, run: cmdNotify},
		{name: "mouse", usage: "[on|off]", desc: "give the wheel to sb, at the cost of the terminal's own text selection", busySafe: true, run: cmdMouse},
		{name: "queue", usage: "[clear]", desc: "what is waiting to run after this turn", busySafe: true, run: cmdQueue},
		{name: "steer", usage: "<text>", desc: "send your words into the running turn at its next round boundary", busySafe: true, run: cmdSteer},
		{name: "every", usage: "<interval> <prompt>", desc: "run a prompt on an interval, firing as a turn while sb runs", busySafe: true, run: cmdEvery},
		{name: "at", usage: "<HH:MM> <prompt>", desc: "run a prompt once at a local clock time", busySafe: true, run: cmdAt},
		{name: "schedule", usage: "[cancel <id>]", desc: "armed reminders and recurring prompts, kept per workspace", busySafe: true, run: cmdSchedule},
		{name: "budget", usage: "[amount|off]", desc: "a dollar ceiling the session must stay under", busySafe: true, run: cmdBudget},
		{name: "compact", usage: "[guidance|preview|auto|at]", desc: "summarize into a fresh context; preview says what that would take", run: cmdCompact},
		{name: "context", usage: "[tokens]", desc: "how much of the window is in use; a count records what this target accepts", busySafe: true, run: cmdContext},
		{name: "init", desc: "write an AGENTS.md for this repository", run: cmdInit},
		{name: "export", usage: "[file]", desc: "save the conversation as markdown", busySafe: true, run: cmdExport},
		{name: "session", desc: "session id, target, and message count", busySafe: true, run: cmdSession},
		{name: "sandbox", usage: "[off|on|auto]", desc: "show or change command confinement", run: cmdSandbox},
		{name: "doctor", usage: "[extensions]", desc: "probe session gates, or inspect every startup extension diagnostic", run: cmdDoctor},
		{name: "trust", usage: "[grant|revoke|list]", desc: "let this workspace run what it declares (MCP servers, hooks)", busySafe: true, run: cmdTrust},
		{name: "mcp", usage: "[list|inspect|enable|disable] [server]", desc: "connected servers or native MCP activation", busySafe: true, run: cmdMCP},
		{name: "plugins", usage: "[list|inspect|install|enable|disable|trust|untrust] [plugin]", desc: "native plugin inventory and Switchboard activation", run: cmdPlugins},
		{name: "hooks", desc: "commands that run around each tool call", busySafe: true, run: cmdHooks},
		{name: "permissions", desc: "the standing rules that answer without asking", busySafe: true, run: cmdPermissions},
		{name: "tasks", usage: "[cancel <id>]", desc: "running and completed delegate tasks; cancel one without stopping the rest", busySafe: true, run: cmdTasks},
		{name: "agents", desc: "named subagents the model can delegate to", busySafe: true, run: cmdAgents},
		{name: "skills", desc: "discovered instruction packs, origins, and invocation status", busySafe: true, run: cmdSkills},
		{name: "skill", usage: "<canonical-selector> [args]", desc: "invoke one instruction pack explicitly", run: cmdSkill},
		{name: "learn", usage: "<name>", desc: "distill this session's method into a reusable skill pack", run: cmdLearn},
		{name: "files", usage: "[query]", desc: "quick-open workspace files with a revision-aware source lens", busySafe: true, run: cmdFiles},
		{name: "search", usage: "<literal>", desc: "search workspace text and inspect exact source locations", busySafe: true, run: cmdWorkspaceSearch},
		{name: "lsp", desc: "language-server state and advertised capabilities (never starts it)", busySafe: true, run: cmdLSP},
		{name: "outline", usage: "<path>", desc: "semantic declarations in one source file", busySafe: true, run: cmdOutline},
		{name: "symbols", usage: "<query>", desc: "search workspace declarations by semantic name", busySafe: true, run: cmdSymbols},
		{name: "problems", usage: "[path]", desc: "published diagnostics with explicit freshness and coverage", busySafe: true, run: cmdProblems},
		{name: "definition", usage: "<path>:<line> <symbol>", desc: "open where a symbol is defined", busySafe: true, run: cmdDefinition},
		{name: "references", usage: "<path>:<line> <symbol>", desc: "browse every semantic reference to a symbol", busySafe: true, run: cmdReferences},
		{name: "diff", desc: "review uncommitted changes", busySafe: true, run: cmdDiff},
		{name: "review", usage: "[turn]", desc: "review one turn's recorded mutations", busySafe: false, run: cmdReview},
		{name: "changes", desc: "which files each turn touched, via write and edit", busySafe: true, run: cmdChanges},
		{name: "blame", usage: "[path[:line]]", desc: "which recorded turn wrote each line, on which rung and model; bare, the workspace's yield", busySafe: true, run: cmdBlame},
		{name: "mistakes", desc: "the failures more than one session met, from the workspace's own record", busySafe: true, run: cmdMistakes},
		{name: "undo", usage: "[list|path]", desc: "take back the last turn's file changes, or one file's", run: cmdUndo},
		{name: "watch", usage: "[cmd|off]", desc: "run your verifier after the model's edits; only changes are reported", run: cmdWatch},
		{name: "bisect", usage: "[cmd]", desc: "binary-search this session's turns for the one that turned the verifier red", run: cmdBisect},
		{name: "retry", usage: "[tier]", desc: "take back the last turn and run it again, optionally on another rung", run: cmdRetry},
		{name: "copy", usage: "[n|code [n]]", desc: "copy the last response, or its code: /copy code takes the newest block", busySafe: true, run: cmdCopy},
		{name: "setup", desc: "connect providers: keys, local server, an existing codex login", run: cmdSetup},
		{name: "models", desc: "browse models and bind tiers", run: cmdModels},
		{name: "think", aliases: []string{"effort"}, usage: "[level]", desc: "reasoning effort for the active model, this session", run: cmdThink},
		{name: "login", usage: "[provider[/surface]]", desc: "store an API key in the OS keychain", busySafe: true, run: cmdLogin},
		{name: "logout", usage: "[provider[/surface]]", desc: "remove a stored API key", busySafe: true, run: cmdLogout},
		{name: "theme", usage: "[dark|light|auto]", desc: "switch the color theme, or follow the terminal", run: cmdTheme},
		{name: "update", usage: "[channel|auto …]", desc: "install a newer switchboard, or set the update posture", busySafe: true, run: cmdUpdate},
	}
}

// matchingCommands filters the registry plus the dynamic tier-switching
// entries by prefix, for the autocomplete list.
func matchingCommands(prefix string, cfg *config.Config) []commandItem {
	var out []commandItem
	for _, c := range commands() {
		if strings.HasPrefix(c.name, prefix) {
			out = append(out, c)
			continue
		}
		for _, a := range c.aliases {
			if strings.HasPrefix(a, prefix) {
				out = append(out, c)
				break
			}
		}
	}
	for _, t := range cfg.Tiers {
		if strings.HasPrefix(t.ID, prefix) {
			out = append(out, commandItem{name: t.ID, desc: "switch to tier " + t.ID + "; /" + t.ID + " <prompt> runs one prompt there"})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// helpGroups orders the command list by what a hand reaches for, because
// forty entries in registry order is a wall. Grouping is presentation, so
// it lives here with help rather than as registry structure; the test that
// every command appears in exactly one group is what keeps a new command
// from silently missing the page.
var helpGroups = []struct {
	title string
	names []string
}{
	{"session", []string{"clear", "resume", "recap", "fork", "pin", "retry", "compact", "context", "session", "export", "find", "queue", "steer", "every", "at", "schedule", "exit"}},
	{"the ladder", []string{"tier", "tiers", "ladder", "why", "race", "races", "cost", "estimate", "stats", "budget", "think", "cache", "advisor"}},
	{"files and work", []string{"files", "search", "diff", "review", "audit", "changes", "blame", "mistakes", "undo", "watch", "bisect", "copy", "init", "skill", "learn"}},
	{"language intelligence", []string{"lsp", "outline", "symbols", "problems", "definition", "references"}},
	{"safety and reach", []string{"mode", "permissions", "routing", "destinations", "workflow", "trust", "sandbox", "doctor", "plugins", "mcp", "hooks", "tasks", "agents", "skills", "login", "logout"}},
	{"the surface", []string{"help", "theme", "notify", "mouse", "models", "setup", "update"}},
}

func cmdHelp(m *tuiModel, _ string) tea.Cmd {
	byName := map[string]commandItem{}
	for _, c := range commands() {
		byName[c.name] = c
	}
	var b strings.Builder
	b.WriteString("commands\n")
	for _, g := range helpGroups {
		b.WriteString("\n " + g.title + "\n")
		for _, name := range g.names {
			c, ok := byName[name]
			if !ok {
				continue
			}
			entry := "  /" + c.name
			if c.usage != "" {
				entry += " " + c.usage
			}
			fmt.Fprintf(&b, "%s%s%s\n", entry, strings.Repeat(" ", max(46-len(entry), 2)), c.desc)
		}
	}
	if tiers := m.app.config.Tiers; len(tiers) > 0 {
		var ids []string
		for _, t := range tiers {
			ids = append(ids, "/"+t.ID)
		}
		fmt.Fprintf(&b, "  %s%sswitch tier; /<tier> <prompt> runs one prompt there\n",
			strings.Join(ids, ", "), strings.Repeat(" ", 2))
	}
	b.WriteString(`
input
  @path            attach a file (tab completes)   !cmd    run a shell command yourself
  \ then enter     continue the line               alt+enter / ctrl+j   newline
  /every 30m …     run a prompt on an interval     /at 14:30 …         run it once at a clock time
                   (/schedule lists and cancels; entries persist per workspace and fire only while sb runs)

keys
  enter            send                  tab                complete
  ↑↓               history / choose      ctrl+r             search prompt history
  ctrl+f           search the transcript
  shift+tab        cycle permission mode ctrl+t             tier picker
  alt+1…alt+9      jump straight to that rung
  ctrl+p           command palette       ctrl+g             edit the prompt in $EDITOR
  ctrl+o           expand the last route or tool entry
  esc              interrupt the turn    ctrl+c ctrl+c      exit
  ctrl+s           steer the running turn with what you typed
  pgup/pgdn        scroll                ctrl+u / ctrl+d    half a page
  shift+↑/↓        scroll a few lines    home/end           top / bottom

  the mouse is on by default: the wheel scrolls, a click expands a rail,
  and a drag selects lines and copies them on release.
  /mouse off gives the terminal the mouse (a plain drag selects there).`)
	m.addInfo(b.String())
	return nil
}

func cmdExit(m *tuiModel, _ string) tea.Cmd {
	if m.pendingAsk != nil {
		m.pendingAsk <- permission.Response{}
	}
	m.quitting = true
	return tea.Quit
}

func cmdClear(m *tuiModel, _ string) tea.Cmd { return m.clearSession() }

func cmdResume(m *tuiModel, args string) tea.Cmd {
	if args != "" {
		return m.reopen(args)
	}
	infos, err := m.app.store.List(m.app.workspace)
	if err != nil {
		return noticeCmd("error", err.Error())
	}
	if len(infos) == 0 {
		return noticeCmd("", "no sessions recorded for "+m.app.workspace)
	}
	items := make([]pickerItem, 0, len(infos))
	for _, info := range infos {
		// A menu of timestamped ids asks the user to remember which opaque
		// string held which conversation; openingLabel reads a few records
		// from the head of each log, so labelling the list stays cheap
		// however long the sessions grew.
		label := info.ID
		desc := info.Modified.Local().Format("2006-01-02 15:04:05")
		if opening := openingLabel(info.Path); opening != "" {
			label = opening
			desc = info.ID + "  " + desc
		}
		items = append(items, pickerItem{
			id:      info.ID,
			label:   label,
			desc:    desc,
			current: m.app.loop.Session != nil && info.ID == m.app.loop.Session.ID(),
		})
	}
	m.dlg = &pickerDialog{
		title:  "resume a session",
		items:  items,
		onPick: func(id string) tea.Cmd { return m.reopen(id) },
	}
	return nil
}

// cmdFork branches the session at a turn boundary (§12). Bare /fork branches
// at the tip — a safe point to explore from — and /fork n leaves the last n
// user turns behind, which is the cache-honest form of "go back two turns":
// the original log is never rewritten, and the fork's prefix stays warm on
// the provider because it is byte-identical to what was already sent.
func cmdFork(m *tuiModel, args string) tea.Cmd {
	state := m.app.loop.Session.State()
	if len(state.Messages) == 0 {
		return noticeCmd("", "nothing to fork; the session is empty")
	}

	n := 0
	if args = strings.TrimSpace(args); args != "" {
		v, err := strconv.Atoi(args)
		if err != nil {
			// Not a number: a pin name. The pin recorded its cut when it was
			// set, so the fork lands exactly where the user stood then.
			pin, ok := state.Pin(args)
			if !ok {
				return noticeCmd("error", "no pin named "+args+"; /pin lists them, /fork n counts turns instead")
			}
			if pin.Messages < 1 {
				return noticeCmd("error", "pin "+args+" marks the session's start; /clear is how an empty session starts")
			}
			dropped := 0
			for _, msg := range state.Messages[min(pin.Messages, len(state.Messages)):] {
				if msg.Role == provider.RoleUser {
					dropped++
				}
			}
			return m.forkSession(m.app.loop.Session.ID(), pin.Messages, dropped)
		}
		if v < 0 {
			return noticeCmd("error", "/fork takes how many user turns to leave behind, e.g. /fork 2, or a /pin name")
		}
		n = v
	}

	keep := len(state.Messages)
	if n > 0 {
		var userAt []int
		for i, msg := range state.Messages {
			if msg.Role == provider.RoleUser {
				userAt = append(userAt, i)
			}
		}
		if n >= len(userAt) {
			return noticeCmd("error", fmt.Sprintf(
				"the session has %d user turns; dropping %d would leave nothing, and /clear is how an empty session starts", len(userAt), n))
		}
		keep = userAt[len(userAt)-n]
	}
	return m.forkSession(m.app.loop.Session.ID(), keep, n)
}

func cmdTier(m *tuiModel, args string) tea.Cmd {
	if args == "" {
		return m.openTierPicker()
	}
	if args == "auto" {
		if err := persistAutomaticPosture(m.app.loop.Session, m.app.tier); err != nil {
			return noticeCmd("error", "automatic routing was not enabled: "+err.Error())
		}
		if m.app.sticky != nil {
			m.app.sticky.Unpin()
		}
		m.app.route = nil
		if !m.app.config.RouteAutoOn() {
			m.addNotice("route", "pin removed; routing is off, so the rung still changes only when you change it (/routing on resumes)")
			return nil
		}
		m.addNotice("route", "automatic per-turn routing resumed from "+m.app.tier.ID)
		return nil
	}
	return m.switchTier(args)
}

func cmdTiers(m *tuiModel, _ string) tea.Cmd {
	if len(m.app.config.Tiers) == 0 {
		return noticeCmd("", "no tiers configured in "+m.app.config.Path)
	}
	var b strings.Builder
	if p := m.app.config.ActiveProfile; p != "" {
		// A ladder that came from a profile says so, because "which ladder
		// am I on" is the first question a surprising route decision raises.
		b.WriteString("profile " + p + " — the main ladder stands aside for this session\n")
	}
	for _, t := range m.app.config.Tiers {
		marker := "  "
		if t.ID == m.app.tier.ID {
			marker = "* "
		}
		b.WriteString(marker + t.String() + "\n")
		if len(t.Fallbacks) > 0 {
			var ids []string
			for _, fb := range t.Fallbacks {
				ids = append(ids, fb.Display())
			}
			b.WriteString("      falls back to " + strings.Join(ids, ", ") + "\n")
		}
		info, confidence, ok := m.app.catalog.Lookup(t.Target)
		if !ok {
			b.WriteString("      no catalog entry\n")
			continue
		}
		b.WriteString("      " + describePricing(info))
		if confidence == catalog.Prior {
			b.WriteString("  (surface default, not verified for this model)")
		}
		b.WriteString("\n")
	}
	m.addInfo(strings.TrimRight(b.String(), "\n"))
	return nil
}

func cmdMode(m *tuiModel, args string) tea.Cmd {
	if args != "" {
		mode, err := permission.ParseMode(args)
		if err != nil {
			return noticeCmd("error", err.Error())
		}
		return m.setMode(mode)
	}
	descs := map[permission.Mode]string{
		permission.ModePlan:        "read-only: no writes, no commands",
		permission.ModeDefault:     "writes and commands ask first",
		permission.ModeAcceptEdits: "edits apply, commands ask first",
		permission.ModeAuto:        "edits apply; a cheap model reviews confined commands, while host-direct commands ask you",
		permission.ModeYOLO:        "FULL HOST ACCESS: edits, commands, and external tools all run without asking",
		permission.ModeBypass:      "promptless only with verified confinement that isolates host network and IPC",
	}
	var items []pickerItem
	for _, mode := range []permission.Mode{permission.ModePlan, permission.ModeDefault, permission.ModeAcceptEdits, permission.ModeAuto, permission.ModeYOLO, permission.ModeBypass} {
		items = append(items, pickerItem{
			id:      string(mode),
			label:   string(mode),
			desc:    descs[mode],
			current: m.mode == mode,
		})
	}
	m.dlg = &pickerDialog{
		title: "permission mode",
		items: items,
		onPick: func(id string) tea.Cmd {
			mode, err := permission.ParseMode(id)
			if err != nil {
				return noticeCmd("error", err.Error())
			}
			return m.setMode(mode)
		},
	}
	return nil
}

func cmdCost(m *tuiModel, args string) tea.Cmd {
	switch strings.TrimSpace(args) {
	case "":
		state := m.app.loop.Session.State()
		m.refreshCost(state)
		m.addInfo(strings.Join(summaryLines(state, m.app.catalog, m.app.loop.Binding().Target), "\n"))
		return nil
	case "rungs":
		// Read-only on the session's own log, which is open for appending:
		// same posture as `sb cost`, and busy-safe for the same reason.
		usages, err := session.ReadUsages(m.app.loop.Session.Path())
		if err != nil {
			return noticeCmd("error", err.Error())
		}
		m.addInfo(strings.Join(costRungsLines(m.app.config.Tiers, m.app.catalog, m.app.tier.ID, usages), "\n"))
		return nil
	case "turns":
		// Same read-only posture as rungs; the question is per ask rather
		// than per rung: which prompts cost the money.
		turns, err := session.ReadTurnCosts(m.app.loop.Session.Path())
		if err != nil {
			return noticeCmd("error", err.Error())
		}
		m.addInfo("the session's turns, by what they billed\n" + strings.Join(costTurnsLines(turns), "\n"))
		return nil
	default:
		return noticeCmd("error", "/cost shows this session; /cost rungs reprices it on every rung, /cost turns orders its asks by what they billed")
	}
}

// cmdBudget shows or sets the session's dollar ceiling. Setting persists the
// way /theme does. It stays busy-safe on purpose: the loop's gate reads the
// shared state before every call, so lowering the ceiling mid-turn is how a
// runaway turn gets stopped without waiting for it.
func cmdBudget(m *tuiModel, args string) tea.Cmd {
	args = strings.TrimSpace(args)
	bs := m.app.budget
	if bs == nil {
		return noticeCmd("error", "no budget state is wired for this session")
	}
	switch args {
	case "":
		ceiling := bs.get()
		if ceiling == 0 {
			m.addInfo("  no ceiling set\n" +
				"  /budget 2.50 caps what this session may spend: the router refuses rungs whose\n" +
				"  upper bound could cross it, escalation cannot move onto one, and the loop stops\n" +
				"  before the call that would. /budget off clears it. The setting persists.")
			return nil
		}
		state := m.app.loop.Session.State()
		spent := catalog.Money(state.AccountedCostMicroUSD())
		debt := bs.syncRetryDebt(state.ID, catalog.Money(state.RetryReserveMicroUSD))
		accounted := addMoney(spent, debt)
		left := ceiling - accounted
		if left < 0 {
			left = 0
		}
		var b strings.Builder
		fmt.Fprintf(&b, "  ceiling  %s\n  spent    %s\n  reserved %s  pending or failed provider attempts may still bill\n  accounted %s\n  left     %s",
			ceiling, spent, debt, accounted, left)
		info, _, ok := m.app.catalog.Lookup(m.app.loop.Binding().Target)
		switch {
		case !ok:
			b.WriteString("\n  the active target has no catalog entry, so its calls are unpriced and pass the gate")
		case info.Metering == catalog.Local:
			b.WriteString("\n  the active rung runs locally; the ceiling governs the rungs that bill dollars")
		case info.Metering == catalog.Plan:
			b.WriteString("\n  the active rung bills quota, not dollars; the ceiling governs the rungs that bill dollars")
		}
		b.WriteString("\n  a delegate errand counts its own log and this one against the same ceiling while it runs")
		m.addInfo(b.String())
		return nil
	case "off":
		bs.set(0)
		m.app.config.Budget = 0
		m.refreshCost(m.app.loop.Session.State())
		if err := m.app.config.Save(); err != nil {
			return noticeCmd("warn", "ceiling cleared for this session, but not saved: "+err.Error())
		}
		return noticeCmd("", "ceiling cleared")
	default:
		var money catalog.Money
		if err := money.UnmarshalText([]byte(args)); err != nil || money <= 0 {
			return noticeCmd("error", "/budget takes a dollar amount like 2.50, or off")
		}
		bs.set(money)
		m.app.config.Budget = money
		m.refreshCost(m.app.loop.Session.State())
		if err := m.app.config.Save(); err != nil {
			return noticeCmd("warn", fmt.Sprintf("ceiling %s set for this session, but not saved: %v", money, err))
		}
		return noticeCmd("", fmt.Sprintf("ceiling %s set; the session stops before the call that would cross it", money))
	}
}

func cmdSession(m *tuiModel, _ string) tea.Cmd {
	state := m.app.loop.Session.State()
	m.addInfo(fmt.Sprintf("  %s\n  target   %s\n  catalog  %s\n  messages %d\n  log      %s",
		state.ID, m.app.loop.Binding().Target.Display(), state.CatalogRevision, len(state.Messages), m.app.loop.Session.Path()))
	return nil
}

func cmdSandbox(m *tuiModel, args string) tea.Cmd {
	cap := m.app.capability
	controller := m.app.loop.Perms.Execution()
	args = strings.TrimSpace(args)
	if args == "" || args == "status" {
		m.addInfo(fmt.Sprintf("  platform  %s\n  mechanism %s\n  requested %s\n  %s",
			cap.Platform, cap.Mechanism, controller.SandboxMode(), controller.Summary()))
		return nil
	}
	mode, err := execution.ParseSandboxMode(args)
	if err != nil {
		return noticeCmd("error", err.Error())
	}
	if m.mode == permission.ModeYOLO && mode != execution.SandboxOff {
		return noticeCmd("error", "yolo mode requires the sandbox to stay off; leave yolo before enabling confinement")
	}
	if err := controller.SetSandbox(mode); err != nil {
		return noticeCmd("error", err.Error())
	}
	m.app.config.Sandbox = mode
	m.addInfo("sandbox setting is now " + string(mode) + "\n  " + controller.Summary())
	if err := m.app.config.Save(); err != nil {
		return noticeCmd("warn", "sandbox changed for this process, but the config was not saved: "+err.Error())
	}
	return nil
}

func cmdDiff(m *tuiModel, _ string) tea.Cmd {
	m.workspaceGeneration++
	generation := m.workspaceGeneration
	sessionID := currentSessionID(m)
	load := openDiff(m.app.workspace, m.th.dark)
	return func() tea.Msg {
		msg := load().(diffLoadedMsg)
		msg.sessionID = sessionID
		msg.generation = generation
		return msg
	}
}

// cmdMCP reports the session's external tooling: which servers connected,
// what each brought, and which died since. It reads live state rather than
// startup state, because a server that crashed an hour ago is the thing the
// user is here to find out.
func cmdMCP(m *tuiModel, args string) tea.Cmd {
	args = strings.TrimSpace(args)
	if args != "" {
		cliArgs := splitExtensionAction(args)
		return m.startExtensionAction("mcp "+cliArgs[0], "mcp", func(ctx context.Context, w io.Writer) error {
			return runMCPCLIContext(ctx, w, m.app.workspace, cliArgs)
		})
	}
	st := m.app.mcp
	var clients []*mcp.Client
	if st != nil {
		clients = st.clientList()
	}
	if len(clients) == 0 {
		m.addInfo("  no MCP servers connected\n" +
			"  /mcp list shows native Codex and Claude declarations; /mcp enable <id> activates one on the next Switchboard run.\n" +
			"  Or declare servers in ~/.switchboard/mcp.toml, or in this repository's .switchboard/mcp.toml behind /trust grant:\n\n" +
			"    [mcp.github]\n" +
			"    command = \"github-mcp-server\"\n" +
			"    args = [\"stdio\"]\n\n" +
			"  a url key instead of command reaches a Streamable HTTP server")
		return nil
	}
	var b strings.Builder
	for _, c := range clients {
		fmt.Fprintf(&b, "  %s  %s", c.Name(), c.ServerLine())
		if err := c.Err(); err != nil {
			fmt.Fprintf(&b, "\n    dead: %v", err)
		}
		for _, t := range c.Tools() {
			fmt.Fprintf(&b, "\n    %s", mcp.Namespaced(c.Name(), t.Name))
		}
		b.WriteString("\n")
	}
	m.addInfo(strings.TrimRight(b.String(), "\n"))
	return nil
}

func cmdPlugins(m *tuiModel, args string) tea.Cmd {
	cliArgs := splitExtensionAction(args)
	action := "list"
	if len(cliArgs) > 0 {
		action = cliArgs[0]
	}
	return m.startExtensionAction("plugins "+action, "plugins", func(ctx context.Context, w io.Writer) error {
		return runPluginsCLIContext(ctx, w, m.app.workspace, cliArgs)
	})
}

func invalidateRestoredWorkspace(m *tuiModel) {
	if m.workspaceRuntime != nil {
		m.workspaceRuntime.invalidate()
	}
	if view, ok := m.full.(*lspView); ok {
		view.stale = true
	}
}

// cmdUndo takes back the most recent turn's write and edit effects. It is
// not busy-safe on purpose: undoing under a turn still capturing into its
// own scope would restore a state the model is mid-way through changing.
func cmdUndo(m *tuiModel, args string) tea.Cmd {
	rec := m.app.undo
	if rec == nil {
		return noticeCmd("error", "undo is unavailable in this session")
	}
	if strings.TrimSpace(args) == "list" {
		turns := rec.Turns()
		if len(turns) == 0 {
			m.addInfo("  no turns have changed files")
			return nil
		}
		var b strings.Builder
		for i, t := range turns {
			partial := ""
			if t.Partial {
				partial = "  (partial: some files were over the snapshot cap)"
			}
			fmt.Fprintf(&b, "  %2d  %d file(s)  %s%s\n", len(turns)-i, t.Files, t.Label, partial)
		}
		b.WriteString("  /undo takes back the most recent; repeat to walk further")
		m.addInfo(strings.TrimRight(b.String(), "\n"))
		return nil
	}

	// Any other argument is a file: restore it to what it was before the
	// newest turn that captured it, matched against the recorder's own
	// paths so the argument can be typed the way /changes displays it.
	if arg := strings.TrimSpace(args); arg != "" {
		var abs string
		for _, d := range rec.Details() {
			for _, p := range d.Paths {
				if p == arg || m.app.displayPath(p) == arg {
					abs = p
				}
			}
		}
		if abs == "" {
			return noticeCmd("error", "no turn captured "+arg+"; /changes lists what write and edit touched")
		}
		outcome, label, err := rec.UndoFile(abs)
		if !outcome.Published {
			if err == nil {
				err = fmt.Errorf("undo did not publish a file change")
			}
			return noticeCmd("error", err.Error())
		}
		// Publication invalidates the model's read authority even when a later
		// durability or final-state check reports a warning.
		m.app.loop.Tools.ForgetVersions([]string{abs})
		invalidateRestoredWorkspace(m)
		verb := "restored"
		if outcome.Removed {
			verb = "removed"
		}
		if err != nil {
			m.app.loop.Session.AppendNote("warn", fmt.Sprintf("undo: %s %s from %q; %v", verb, m.app.displayPath(abs), label, err))
		} else {
			m.app.loop.Session.AppendNote("info", fmt.Sprintf("undo: %s %s from %q", verb, m.app.displayPath(abs), label))
		}
		m.addInfo(fmt.Sprintf("  %s %s, from before %q; the turn's other files stand", verb, m.app.displayPath(abs), truncate(firstLine(label), 50)))
		if err != nil {
			return noticeCmd("warn", err.Error())
		}
		return nil
	}

	restored, removed, skipped, failed, label, err := rec.Undo()
	if err != nil {
		return noticeCmd("", err.Error())
	}
	// Restored files changed under the model's feet; the stale check must
	// force a re-read before the next write.
	changed := append(append([]string(nil), restored...), removed...)
	m.app.loop.Tools.ForgetVersions(changed)
	if len(changed) > 0 {
		invalidateRestoredWorkspace(m)
	}
	m.app.loop.Session.AppendNote("info", fmt.Sprintf("undo: reverted %q (%d restored, %d removed)", label, len(restored), len(removed)))

	var b strings.Builder
	fmt.Fprintf(&b, "  took back %q\n", label)
	for _, p := range restored {
		fmt.Fprintf(&b, "  restored %s\n", m.app.displayPath(p))
	}
	for _, p := range removed {
		fmt.Fprintf(&b, "  removed  %s\n", m.app.displayPath(p))
	}
	for _, p := range skipped {
		fmt.Fprintf(&b, "  not covered (over the snapshot cap): %s\n", m.app.displayPath(p))
	}
	for _, f := range failed {
		fmt.Fprintf(&b, "  restore issue: %s\n", f)
	}
	m.addInfo(strings.TrimRight(b.String(), "\n"))
	if len(failed) > 0 {
		return noticeCmd("warn", "some files were not restored or could not be fully verified; see above")
	}
	return nil
}

// cmdHooks lists the loaded hooks: which event, which tools, what runs.
func cmdHooks(m *tuiModel, _ string) tea.Cmd {
	set := m.app.loop.Hooks
	if set.Empty() {
		m.addInfo("  no hooks loaded\n" +
			"  declare them in ~/.switchboard/hooks.toml, or in this repository's .switchboard/hooks.toml behind /trust grant:\n\n" +
			"    [[hooks.pre_tool]]\n" +
			"    tools = [\"exec\"]\n" +
			"    run = \"./scripts/audit.sh\"\n\n" +
			"  a pre_tool hook that exits non-zero blocks the call; a post_tool hook's output rides back on the result")
		return nil
	}
	var b strings.Builder
	for _, h := range set.Hooks() {
		scope := "every tool"
		if len(h.Tools) > 0 {
			scope = strings.Join(h.Tools, ", ")
		}
		fmt.Fprintf(&b, "  %-9s %-20s %s\n", h.Event, scope, h.Run)
	}
	m.addInfo(strings.TrimRight(b.String(), "\n"))
	return nil
}

// cmdAgents lists the named subagent definitions this session discovered,
// with each one's rung, grant, and which directory spoke.
func cmdAgents(m *tuiModel, _ string) tea.Cmd {
	if len(m.app.agents) == 0 && len(m.app.agentNotes) == 0 {
		m.addInfo("  no agents defined\n" +
			"  a markdown file per agent, in this repository's .switchboard/agents/ or in ~/.switchboard/agents/:\n\n" +
			"    ---\n" +
			"    description: reviews a diff for correctness\n" +
			"    tier: t2\n" +
			"    tools: read, grep, glob\n" +
			"    ---\n" +
			"    You review changes. Report problems; do not fix them.\n\n" +
			"  the model runs one by calling delegate with its name; a new file is picked up on the next Switchboard run")
		return nil
	}
	var b strings.Builder
	for _, ag := range m.app.agents {
		rung := ag.Tier
		if rung == "" && len(m.app.config.Tiers) > 0 {
			rung = m.app.config.Tiers[0].ID
		}
		src := "~/.switchboard/agents"
		if !ag.FromHome {
			src = ".switchboard/agents"
		}
		fmt.Fprintf(&b, "  %-14s %-4s %s\n", ag.Name, rung, ag.Description)
		grant := "the full core suite"
		if len(ag.Tools) > 0 {
			grant = strings.Join(ag.Tools, ", ")
		}
		fmt.Fprintf(&b, "  %-14s %-4s tools: %s · from %s\n", "", "", grant, src)
	}
	for _, n := range m.app.agentNotes {
		fmt.Fprintf(&b, "  ! %s\n", n)
	}
	m.addInfo(strings.TrimRight(b.String(), "\n"))
	return nil
}

// cmdTrust shows and edits the standing grant that lets a checkout start the
// processes it declares. The wording stays concrete about what a grant
// enables, because "trust this workspace?" answered without knowing the
// stakes is the permission-prompt-as-sandbox mistake in another costume.
// trustDeclarations reads what this checkout's .switchboard/ actually
// declares, without executing any of it - a declaration is data until
// trust says otherwise, the same read-only posture skills and agent
// definitions already hold. The moment of granting is the moment that has
// to be plain, and "MCP servers and hooks" is a category, not a fact; the
// fact is which servers, which hooks, which language server.
func trustDeclarations(m *tuiModel) []string {
	var lines []string
	ws := m.app.workspace
	if specs, err := mcp.LoadSpecs(filepath.Join(ws, ".switchboard", mcp.SpecFileName)); err == nil {
		for _, spec := range specs {
			what := spec.Command
			if what == "" {
				what = spec.URL
			}
			line := fmt.Sprintf("  mcp server %q - %s", spec.Name, truncate(what, 50))
			if len(spec.Allow) > 0 {
				line += fmt.Sprintf(" (%d tools pre-allowed)", len(spec.Allow))
			}
			lines = append(lines, line)
		}
	}
	if set, err := hooks.Load(filepath.Join(ws, ".switchboard", hooks.FileName), ws); err == nil {
		for _, h := range set.Hooks() {
			scope := "every tool"
			if len(h.Tools) > 0 {
				scope = strings.Join(h.Tools, ", ")
			}
			lines = append(lines, fmt.Sprintf("  %s hook on %s - %s", h.Event, scope, truncate(h.Run, 50)))
		}
	}
	if argv, marker, ok := lspCandidate(ws); ok {
		lines = append(lines, fmt.Sprintf("  language server candidate %s for this workspace (%s present; verified only after trust)", filepath.Base(argv[0]), marker))
	}
	return lines
}

func cmdTrust(m *tuiModel, args string) tea.Cmd {
	s := m.app.trust
	if s == nil {
		return noticeCmd("error", "the trust store is unavailable: "+m.app.trustErr)
	}
	ws := m.app.workspace
	decls := trustDeclarations(m)
	switch strings.TrimSpace(args) {
	case "":
		state := "not trusted: what this repository's .switchboard/ declares stays off"
		if s.Trusted(ws) {
			state = "trusted: what this repository's .switchboard/ declares may run"
		}
		var b strings.Builder
		fmt.Fprintf(&b, "  %s\n  %s\n", ws, state)
		if len(decls) > 0 {
			b.WriteString("  a grant covers, specifically:\n" + strings.Join(decls, "\n") + "\n")
		} else {
			b.WriteString("  this checkout declares nothing a grant would enable\n")
		}
		b.WriteString("  /trust grant enables, /trust revoke withdraws; ~/.switchboard config always runs")
		m.addInfo(b.String())
	case "grant":
		if err := s.Grant(ws); err != nil {
			return noticeCmd("error", "grant failed: "+err.Error())
		}
		if len(decls) > 0 {
			m.addInfo("  workspace trusted; from the next run of sb this enables:\n" +
				strings.Join(decls, "\n"))
		} else {
			m.addInfo("  workspace trusted; this checkout currently declares nothing, so the grant enables nothing until it does")
		}
	case "revoke":
		if err := s.Revoke(ws); err != nil {
			return noticeCmd("error", "revoke failed: "+err.Error())
		}
		m.addInfo("  trust withdrawn; repository-declared MCP servers and hooks stay off from the next run of sb")
	case "list":
		granted := s.Granted()
		if len(granted) == 0 {
			m.addInfo("  no workspaces are trusted")
			break
		}
		m.addInfo("  " + strings.Join(granted, "\n  "))
	default:
		return noticeCmd("error", "/trust takes grant, revoke, or list")
	}
	return nil
}

func cmdCopy(m *tuiModel, args string) tea.Cmd {
	// /copy code [n] takes the nth-newest fenced block across the
	// transcript's responses: a block is what a mouse selection across
	// wrapped, styled lines mangles, which is exactly when copying is
	// wanted.
	if rest, ok := strings.CutPrefix(strings.TrimSpace(args), "code"); ok {
		n := 1
		if rest = strings.TrimSpace(rest); rest != "" {
			v, err := strconv.Atoi(rest)
			if err != nil || v < 1 {
				return noticeCmd("error", "/copy code takes a positive number: /copy code 2 copies the second-newest block")
			}
			n = v
		}
		var blocks []string
		for i := len(m.tr.entries) - 1; i >= 0; i-- {
			if m.tr.entries[i].kind != kindAssistant {
				continue
			}
			found := codeBlocks(m.tr.entries[i].text)
			for j := len(found) - 1; j >= 0; j-- {
				blocks = append(blocks, found[j])
			}
		}
		if len(blocks) == 0 {
			return noticeCmd("error", "no fenced code blocks in this session's responses")
		}
		if n > len(blocks) {
			return noticeCmd("error", fmt.Sprintf("only %d code blocks to copy", len(blocks)))
		}
		block := blocks[n-1]
		return func() tea.Msg {
			return copyMsg{n: n, what: "code block", err: clipboard.WriteAll(block)}
		}
	}

	n := 1
	if args != "" {
		v, err := strconv.Atoi(args)
		if err != nil || v < 1 {
			return noticeCmd("error", "/copy takes a positive number: /copy 2 copies the second-to-last response")
		}
		n = v
	}
	var texts []string
	for i := len(m.tr.entries) - 1; i >= 0; i-- {
		if m.tr.entries[i].kind == kindAssistant && m.tr.entries[i].text != "" {
			texts = append(texts, m.tr.entries[i].text)
		}
	}
	if n > len(texts) {
		return noticeCmd("error", fmt.Sprintf("only %d responses to copy", len(texts)))
	}
	text := texts[n-1]
	return func() tea.Msg {
		return copyMsg{n: n, err: clipboard.WriteAll(text)}
	}
}

// codeBlocks extracts fenced blocks in document order, fence lines
// excluded. Both fence styles markdown admits are read, and an unclosed
// fence yields what it opened: a streaming response cut mid-block still
// hands over the code it showed.
func codeBlocks(text string) []string {
	var blocks []string
	var current []string
	fence := ""
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if fence == "" {
			switch {
			case strings.HasPrefix(trimmed, "```"):
				fence = "```"
			case strings.HasPrefix(trimmed, "~~~"):
				fence = "~~~"
			}
			continue
		}
		if strings.HasPrefix(trimmed, fence) {
			blocks = append(blocks, strings.Join(current, "\n"))
			current, fence = nil, ""
			continue
		}
		current = append(current, line)
	}
	if fence != "" && len(current) > 0 {
		blocks = append(blocks, strings.Join(current, "\n"))
	}
	return blocks
}

func cmdTheme(m *tuiModel, args string) tea.Cmd {
	apply := func(name string) tea.Cmd {
		switch name {
		case "dark":
			m.setTheme(true)
		case "light":
			m.setTheme(false)
		case "auto":
			// A persisted choice beats detection forever, so un-choosing has
			// to be sayable: auto clears the setting and asks the terminal
			// again, which is where a fresh install starts.
			m.setTheme(detectDark())
			m.app.config.Theme = ""
			if err := m.app.config.Save(); err != nil {
				return noticeCmd("error", "theme now follows the terminal, but saving that failed: "+err.Error())
			}
			return noticeCmd("", "theme now follows the terminal's own background")
		default:
			return noticeCmd("error", "theme is dark, light, or auto (follow the terminal)")
		}
		m.app.config.Theme = name
		if err := m.app.config.Save(); err != nil {
			return noticeCmd("error", "theme is now "+name+", but saving it failed: "+err.Error())
		}
		return noticeCmd("", "theme is now "+name)
	}
	if args != "" {
		return apply(args)
	}
	m.dlg = &pickerDialog{
		title: "theme",
		items: []pickerItem{
			{id: "dark", label: "dark", current: m.app.config.Theme == "dark"},
			{id: "light", label: "light", current: m.app.config.Theme == "light"},
			{id: "auto", label: "auto", desc: "follow the terminal", current: m.app.config.Theme == ""},
		},
		onPick: apply,
	}
	return nil
}

func (m *tuiModel) setTheme(dark bool) {
	m.th = themeFor(dark)
	m.md.setDark(dark)
	m.tr.setTheme(m.th)
}

// cmdRaces is sb races reachable where races are actually run: the
// paired-trial corpus, tallied by pair, read-only over the workspace's
// logs the way /stats reads them. The wording is the CLI's own, because
// two renderings of one tally would eventually disagree about what a tie
// means.
func cmdRaces(m *tuiModel, args string) tea.Cmd {
	var b strings.Builder
	var err error
	switch strings.TrimSpace(args) {
	case "all":
		err = runRacesAllCLI(&b, m.app.store)
	case "":
		err = runRacesCLI(&b, m.app.store, m.app.workspace)
	default:
		return noticeCmd("error", "/races takes no argument, or all")
	}
	if err != nil {
		return noticeCmd("error", err.Error())
	}
	m.addInfo(strings.TrimRight(b.String(), "\n"))
	return nil
}

// cmdDoctor is sb doctor reachable from inside the session, because the
// moment something breaks mid-session is the moment quitting to diagnose it
// costs the most. The probes run off the UI goroutine and the report lands
// as one entry; the MCP section is the one deliberate difference, stated in
// the output, since probing would spawn each declared server a second time
// beside this session's own.
func cmdDoctor(m *tuiModel, args string) tea.Cmd {
	switch strings.TrimSpace(args) {
	case "extensions":
		m.closeFullscreen()
		m.full = newStartupNotesView(m.app.startupNotes)
		return nil
	case "":
		// Continue with the live gate probes below.
	default:
		return noticeCmd("error", "usage: /doctor [extensions]")
	}
	if m.app.providers == nil {
		return noticeCmd("error", "doctor needs the provider registry; run sb doctor from a shell")
	}
	cfg, cat, reg, workspace := m.app.config, m.app.catalog, m.app.providers, m.app.workspace
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var b strings.Builder
		if err := runDoctor(ctx, &b, cfg, cat, reg, workspace, false); err != nil {
			return noticeMsg{level: "error", text: err.Error()}
		}
		return doctorDoneMsg{report: strings.TrimRight(b.String(), "\n")}
	}
}

// cmdChanges maps the session's file changes to the turns that made them:
// the review surface between /diff (the workspace's own view) and /undo
// (taking a turn back). The evidence is the checkpoint recorder's, so the
// scope is the recorder's scope, stated rather than implied: what write
// and edit touched, with a shell command's side effects absent because
// the recorder cannot see them - the absent-not-guessed rule, applied
// here the way /watch applies it. A turn /undo took back is gone from the
// list, because its changes are no longer in the workspace.
func cmdChanges(m *tuiModel, _ string) tea.Cmd {
	rec := m.app.undo
	if rec == nil {
		return noticeCmd("error", "change tracking is unavailable in this session")
	}
	details := rec.Details()
	if len(details) == 0 {
		m.addInfo("  no turn has changed files, as far as write and edit saw")
		return nil
	}
	var b strings.Builder
	b.WriteString("files this session touched, newest turn first:\n")
	for i := len(details) - 1; i >= 0; i-- {
		d := details[i]
		fmt.Fprintf(&b, "  %2d  %s\n", i+1, truncate(firstLine(d.Label), 60))
		for _, p := range d.Paths {
			fmt.Fprintf(&b, "        %s\n", m.app.displayPath(p))
		}
		for _, p := range d.Skipped {
			fmt.Fprintf(&b, "        %s  (over the snapshot cap; /undo cannot restore it)\n", m.app.displayPath(p))
		}
	}
	b.WriteString("  via write and edit; a shell command's side effects are not captured\n")
	b.WriteString("  /diff shows the workspace's own view; /undo takes back the newest\n")
	b.WriteString("  turn, /undo <path> just that file")
	m.addInfo(strings.TrimRight(b.String(), "\n"))
	return nil
}

// cmdQueue shows what waits behind the running turn, because a prompt that
// silently queued is a prompt the user may believe was lost. clear empties
// it; a queued prompt was never sent, so dropping it erases nothing.
func cmdQueue(m *tuiModel, args string) tea.Cmd {
	switch strings.TrimSpace(args) {
	case "":
		if len(m.queue) == 0 {
			return noticeCmd("", "nothing is queued")
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d queued, in order:\n", len(m.queue))
		for i, q := range m.queue {
			fmt.Fprintf(&b, "  %d. %s\n", i+1, truncate(firstLine(q), 70))
		}
		b.WriteString("  /queue clear drops them")
		m.addInfo(strings.TrimRight(b.String(), "\n"))
		return nil
	case "clear":
		n := len(m.queue)
		m.queue = nil
		if n == 0 {
			return noticeCmd("", "nothing was queued")
		}
		return noticeCmd("", fmt.Sprintf("dropped %d queued prompt(s); none had been sent", n))
	default:
		return noticeCmd("error", "/queue shows what waits; /queue clear drops it")
	}
}

// cmdNotify flips the bell. The setting persists the way /theme does: the
// TUI owns the file, absent means on.
func cmdNotify(m *tuiModel, args string) tea.Cmd {
	switch strings.TrimSpace(args) {
	case "":
		word := "on"
		if !m.app.config.NotifyOn() {
			word = "off"
		}
		return noticeCmd("", "notify is "+word+"; /notify on|off changes it")
	case "on", "off":
		on := args == "on"
		m.app.config.Notify = &on
		if err := m.app.config.Save(); err != nil {
			return noticeCmd("error", "notify is now "+args+", but saving it failed: "+err.Error())
		}
		return noticeCmd("", "notify is now "+args)
	default:
		return noticeCmd("error", "/notify takes on or off")
	}
}

// cmdMouse hands the mouse to sb, or gives it back to the terminal.
//
// On is the default: the wheel scrolls and a click expands a rail, and
// selection still works through the terminal's modifier, because a terminal
// reporting mouse events to a program needs shift, option, or fn to know a
// drag is its own. Off returns the mouse wholly. The setting persists the
// way /theme does, and the mode changes on the running terminal at once
// rather than at the next launch.
func cmdMouse(m *tuiModel, args string) tea.Cmd {
	switch strings.TrimSpace(args) {
	case "":
		if m.app.config.MouseOn() {
			return noticeCmd("", "mouse is on: the wheel scrolls and a click expands a rail, "+
				"and drag-to-select works through your terminal's modifier (shift, option, or fn). /mouse off gives plain selection back")
		}
		return noticeCmd("", "mouse is off, so selection and copy are the terminal's; "+
			"pgup, ctrl+u, home, and ctrl+o do what the wheel and a click would. /mouse on hands it the wheel")
	case "on", "off":
		on := args == "on"
		m.app.config.Mouse = &on
		mode := tea.Cmd(tea.DisableMouse)
		told := "mouse is off; the terminal selects text again"
		if on {
			mode = tea.EnableMouseCellMotion
			told = "mouse is on; drag-to-select works through your terminal's modifier"
		}
		if err := m.app.config.Save(); err != nil {
			return tea.Batch(mode, noticeCmd("error", told+", but saving it failed: "+err.Error()))
		}
		return tea.Batch(mode, noticeCmd("", told))
	default:
		return noticeCmd("error", "/mouse takes on or off")
	}
}
