package tools

// The computer tool drives macOS applications through the system
// accessibility tree: read an app's windows and controls, click them, type,
// press keys, pick menu items. It exists because a coding session regularly
// dead-ends at an app no CLI reaches — the simulator that needs one button
// pressed, the browser showing the rendered page, the dialog holding the
// build hostage — and the choice at that point is this tool or the user's
// hands.
//
// Everything here rides two macOS facts, both verified live on a real
// machine before the schema froze (the capability rule: tested against the
// target, not its docs). First, System Events serves any app's
// accessibility tree and posts clicks and keystrokes under one permission —
// the Accessibility grant to the terminal — while scripting a target app
// directly needs a separate Automation consent per app and hangs on the
// consent dialog. So this tool speaks only to System Events and launches
// apps with open(1); no target app is ever scripted. Second, per-element
// attribute reads are one Apple event each (~25ms), which makes a naive
// walk of a 3000-element window take minutes, while per-container bulk
// reads answer a whole sibling list in one event. The state walk is
// breadth-first with bulk reads and a stated time budget: app chrome
// arrives first and cheap, deep web content is read partially and says so.
//
// The permission posture is the MCP one, because the action is the same
// kind: this tool acts outside the workspace and outside any sandbox, on
// the user's own screen, so every call carries EffectExternal — no bounded
// mode auto-allows it, bypass included; yolo alone does, because that grant
// exempts nothing. The request puts the app name in Path,
// so a remembered answer covers that app for the session rather than one
// byte-exact call, the way web approvals cover a host. Outbound text (what
// type and set would put into another app) passes the credential scan and
// is refused on a hit, since typing a key into a form is the exfiltration
// webfetch's URL scan exists to stop; returned text (another app's UI can
// hold anything, a password manager included) is redacted unconditionally,
// the injected-report posture, because mid-turn there is no one to ask.
//
// Element indexes name entries in the last state call's element list. Each
// entry remembers its accessibility path plus opaque front-window and element
// fingerprints. An action re-resolves the path and verifies both fingerprints
// immediately before mutation; an identity change means "run state again",
// never a click on whatever sits there now. Raw accessibility values remain
// inside the fixed scripts, and arguments travel as argv, so no user string is
// ever spliced into script source.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/permission"
)

const (
	// computerMaxShown bounds the element list a state call hands the
	// context; ~60 characters a line keeps the worst case near the webfetch
	// cap. The walk cap and budget bound the Apple-event cost of producing
	// it: measured on a 3000-element Safari window, bulk reads cover ~25
	// elements a second, so the budget is what actually stops a deep walk.
	computerMaxShown    = 150
	computerMaxWalk     = 600
	computerWalkBudget  = 10 * time.Second
	computerStateLimit  = 25 * time.Second
	computerActionLimit = 15 * time.Second

	// computerMaxType caps one type call. Keystrokes post at UI speed;
	// pasting a file through the keyboard is the wrong tool and the error
	// says so.
	computerMaxType = 4000
)

type computerTool struct {
	binary string

	// mu guards seen. The instance is shared across registry branches and
	// subagent registries the way astgrep's is, and ParallelSafe is false,
	// but two surfaces can still hold the pointer at once.
	mu   sync.Mutex
	seen map[string][]computerElement

	// runScript is the osascript seam, replaced by tests with recorded
	// output. Production runs the fixed script with arguments as argv.
	// launch is the open(1) seam beside it, for the same reason.
	runScript func(ctx context.Context, script string, args []string, limit time.Duration) (string, error)
	launch    func(ctx context.Context, app string) error
}

// computerElement is one entry of an app's last state. The opaque identities
// bind a later action to the front window and element that were shown before
// approval. UI-derived labels are deliberately not cached: permission details
// and the action description use only the role and index.
type computerElement struct {
	path        []int
	role        string
	windowID    string
	fingerprint string
}

// NewComputer wires the tool to a resolved osascript path. The caller
// looked the binary up at session assembly, darwin only, so presence is
// decided once and the frozen zone never changes shape mid-session.
func NewComputer(binary string) Tool {
	t := &computerTool{binary: binary, seen: map[string][]computerElement{}}
	t.runScript = t.osascript
	t.launch = t.runOpen
	return t
}

func (t *computerTool) Name() string { return "computer" }

func (t *computerTool) Description() string {
	return "Read and drive macOS applications through the accessibility tree — windows, " +
		"buttons, menus, text fields — visibly, on the user's own screen. apps lists " +
		"running applications. state reads one app's windows, menu names, and an " +
		"indexed element list, launching the app if needed. click presses an element " +
		"by index or a point by x,y; type sends keystrokes; key presses a key or " +
		"combo like cmd+s; set writes a field's value directly; menu picks an item by " +
		"path like File > Save. Element indexes are valid only against the latest " +
		"state of that app — when an action reports the window changed, call state " +
		"again. Prefer menu items and key combos over clicking window chrome: close " +
		"buttons often ignore synthetic clicks. An element with no label can be " +
		"clicked by the coordinates state shows for it. A newline in type presses " +
		"Return. Deep web page content walks slowly and partially; webfetch reads a " +
		"page's text better. Each app needs one approval per session."
}

// ParallelSafe: actions post events into a live UI and state reads must not
// interleave with them; one at a time is the only honest ordering.
func (t *computerTool) ParallelSafe() bool { return false }

func (t *computerTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "action": {"type": "string", "enum": ["apps", "state", "click", "type", "key", "set", "menu"], "description": "What to do."},
    "app": {"type": "string", "description": "The application's process name as apps lists it, e.g. Safari. Required for every action but apps."},
    "element": {"type": "integer", "description": "An element index from the latest state of this app, for click and set."},
    "x": {"type": "integer", "description": "Point to click, in screen coordinates from an element's position in state. Give both x and y."},
    "y": {"type": "integer"},
    "text": {"type": "string", "description": "What type sends as keystrokes, or the value set writes."},
    "key": {"type": "string", "description": "A key or combo for key: return, tab, esc, up, pagedown, cmd+s, shift+tab, cmd+shift+t."},
    "menu": {"type": "string", "description": "A menu path for menu: File > Save, or Format > Font > Bold."}
  },
  "required": ["action"]
}`)
}

type computerInput struct {
	Action  string `json:"action"`
	App     string `json:"app"`
	Element *int   `json:"element"`
	X       *int   `json:"x"`
	Y       *int   `json:"y"`
	Text    string `json:"text"`
	Key     string `json:"key"`
	Menu    string `json:"menu"`
}

func (t *computerTool) Plan(input json.RawMessage) (Plan, error) {
	var in computerInput
	if err := json.Unmarshal(input, &in); err != nil {
		return Plan{}, fmt.Errorf("computer: %w", err)
	}
	in.App = strings.TrimSpace(in.App)
	if in.Action != "apps" && in.App == "" {
		return Plan{}, fmt.Errorf("computer: %s needs an app; the apps action lists what is running", in.Action)
	}

	var detail string
	var run func(ctx context.Context) (Result, error)
	switch in.Action {
	case "apps":
		detail = "list running applications"
		run = func(ctx context.Context) (Result, error) { return t.apps(ctx) }
	case "state":
		detail = "read the state of " + in.App
		run = func(ctx context.Context) (Result, error) { return t.state(ctx, in.App) }
	case "click":
		switch {
		case in.Element != nil && (in.X != nil || in.Y != nil):
			return Plan{}, fmt.Errorf("computer: click takes an element or a point, not both")
		case in.Element != nil:
			el, err := t.recalled(in.App, *in.Element)
			if err != nil {
				return Plan{}, err
			}
			detail = fmt.Sprintf("click [%d] %s in %s", *in.Element, el.describe(), in.App)
			run = func(ctx context.Context) (Result, error) { return t.clickElement(ctx, in.App, el) }
		case in.X != nil && in.Y != nil:
			x, y := *in.X, *in.Y
			detail = fmt.Sprintf("click the point %d,%d in %s", x, y, in.App)
			run = func(ctx context.Context) (Result, error) { return t.clickPoint(ctx, in.App, x, y) }
		default:
			return Plan{}, fmt.Errorf("computer: click needs an element index or both x and y")
		}
	case "type":
		if in.Text == "" {
			return Plan{}, fmt.Errorf("computer: type needs text")
		}
		if len(in.Text) > computerMaxType {
			return Plan{}, fmt.Errorf("computer: type sends keystrokes and caps at %d characters; put long content in a file instead", computerMaxType)
		}
		if err := scanOutbound("text", in.Text); err != nil {
			return Plan{}, fmt.Errorf("computer: %w", err)
		}
		detail = fmt.Sprintf("type %d characters into %s", len(in.Text), in.App)
		run = func(ctx context.Context) (Result, error) { return t.typeText(ctx, in.App, in.Text) }
	case "key":
		spec, display, err := parseKeySpec(in.Key)
		if err != nil {
			return Plan{}, fmt.Errorf("computer: %w", err)
		}
		detail = fmt.Sprintf("press %s in %s", display, in.App)
		run = func(ctx context.Context) (Result, error) { return t.pressKey(ctx, in.App, spec, display) }
	case "set":
		if in.Element == nil {
			return Plan{}, fmt.Errorf("computer: set needs an element index from state")
		}
		if err := scanOutbound("value", in.Text); err != nil {
			return Plan{}, fmt.Errorf("computer: %w", err)
		}
		el, err := t.recalled(in.App, *in.Element)
		if err != nil {
			return Plan{}, err
		}
		detail = fmt.Sprintf("set the value of [%d] %s in %s", *in.Element, el.describe(), in.App)
		run = func(ctx context.Context) (Result, error) { return t.setValue(ctx, in.App, el, in.Text) }
	case "menu":
		items := splitMenuPath(in.Menu)
		if len(items) < 2 {
			return Plan{}, fmt.Errorf("computer: menu needs a path like File > Save")
		}
		detail = fmt.Sprintf("menu %s in %s", strings.Join(items, " > "), in.App)
		run = func(ctx context.Context) (Result, error) { return t.pickMenu(ctx, in.App, items) }
	default:
		return Plan{}, fmt.Errorf("computer: unknown action %q", in.Action)
	}

	return Plan{
		Request: permission.Request{
			Tool:   t.Name(),
			Effect: permission.EffectExternal,
			Path:   in.App,
			Detail: detail,
		},
		Run: run,
	}, nil
}

func (e computerElement) describe() string {
	return e.role
}

// recalled looks an index up in the app's last state. Failing at Plan time
// rather than in the script keeps a stale index from ever reaching the UI.
func (t *computerTool) recalled(app string, index int) (computerElement, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	els, ok := t.seen[app]
	if !ok {
		return computerElement{}, fmt.Errorf("computer: no state recorded for %s; call state first", app)
	}
	if index < 0 || index >= len(els) {
		return computerElement{}, fmt.Errorf("computer: element %d is outside the last state of %s (0–%d); call state again", index, app, len(els)-1)
	}
	return els[index], nil
}

// redactComputer scrubs key-shaped strings from anything read back off
// another app's UI before it reaches the record. Unconditional, the
// injected-report posture: mid-turn there is no one to ask.
func redactComputer(s string) string {
	if leaks := credential.ScanPrompt(s); len(leaks) > 0 {
		return credential.Redact(s, leaks)
	}
	return s
}

// --- the actions -------------------------------------------------------------

// computerStateWire is the slice of the state script's JSON this tool
// reads. Captured live from a real walk (testdata/computer_state.json);
// unknown fields are ignored so a script revision that adds one still
// parses.
type computerStateWire struct {
	Running   bool     `json:"running"`
	Frontmost bool     `json:"frontmost"`
	Windows   []string `json:"windows"`
	WindowID  string   `json:"window_id"`
	Menus     []string `json:"menus"`
	Els       []struct {
		Path []int  `json:"path"`
		R    string `json:"r"`
		T    string `json:"t"`
		D    string `json:"d"`
		V    string `json:"v"`
		P    []int  `json:"p"`
		S    []int  `json:"s"`
		F    string `json:"f"`
	} `json:"els"`
	Walked   int  `json:"walked"`
	TimedOut bool `json:"timedOut"`
}

func (t *computerTool) state(ctx context.Context, app string) (Result, error) {
	out, err := t.stateWire(ctx, app)
	if err != nil {
		return errorf("computer: %v", err)
	}
	if !out.Running {
		// open(1) launches without scripting the app; -g leaves the user's
		// focus alone, because reading state is not a reason to steal it.
		if lerr := t.launch(ctx, app); lerr != nil {
			return errorf("computer: %s is not running and launching it failed: %v; the apps action lists what is running", app, lerr)
		}
		for i := 0; i < 10; i++ {
			select {
			case <-ctx.Done():
				return Result{}, ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
			if out, err = t.stateWire(ctx, app); err != nil {
				return errorf("computer: %v", err)
			}
			if out.Running {
				break
			}
		}
		if !out.Running {
			return errorf("computer: launched %s, but no process by that name appeared; "+
				"the process may go by a different name — the apps action lists them", app)
		}
	}

	var els []computerElement
	var b strings.Builder
	fmt.Fprintf(&b, "%s — frontmost: %s; windows: %s\n", app, yesNo(out.Frontmost), windowList(out.Windows))
	if len(out.Menus) > 0 {
		fmt.Fprintf(&b, "menus: %s\n", strings.Join(out.Menus, ", "))
	}
	if len(out.Windows) == 0 {
		b.WriteString("no windows are open; a menu item or key combo can usually open one\n")
	}
	for i, e := range out.Els {
		el := computerElement{
			path:        e.Path,
			role:        e.R,
			windowID:    out.WindowID,
			fingerprint: e.F,
		}
		els = append(els, el)
		fmt.Fprintf(&b, "[%d] %s", i, e.R)
		if e.T != "" {
			fmt.Fprintf(&b, " %q", e.T)
		}
		if e.D != "" && e.D != e.T {
			fmt.Fprintf(&b, " (%s)", e.D)
		}
		if e.V != "" {
			fmt.Fprintf(&b, " = %q", e.V)
		}
		if len(e.P) == 2 && len(e.S) == 2 {
			fmt.Fprintf(&b, " at %d,%d %dx%d", e.P[0], e.P[1], e.S[0], e.S[1])
		}
		b.WriteByte('\n')
	}
	if hidden := out.Walked - len(out.Els); hidden > 0 && len(out.Els) > 0 {
		fmt.Fprintf(&b, "[%d low-signal elements hidden: groups, scroll bars, decorations]\n", hidden)
	}
	if out.TimedOut {
		fmt.Fprintf(&b, "[the walk stopped at its time budget after %d elements; deeper content was not read]\n", out.Walked)
	} else if out.Walked >= computerMaxWalk {
		fmt.Fprintf(&b, "[the walk stopped at its %d-element cap; deeper content was not read]\n", computerMaxWalk)
	}

	t.mu.Lock()
	t.seen[app] = els
	t.mu.Unlock()
	return Result{Content: redactComputer(strings.TrimRight(b.String(), "\n"))}, nil
}

func (t *computerTool) stateWire(ctx context.Context, app string) (computerStateWire, error) {
	args := []string{app,
		fmt.Sprint(computerMaxWalk), fmt.Sprint(computerMaxShown),
		fmt.Sprint(computerWalkBudget.Milliseconds())}
	raw, err := t.runScript(ctx, computerStateScript, args, computerStateLimit)
	if err != nil {
		return computerStateWire{}, err
	}
	var out computerStateWire
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return computerStateWire{}, fmt.Errorf("could not read the state script's output: %v", err)
	}
	return out, nil
}

func (t *computerTool) apps(ctx context.Context) (Result, error) {
	raw, err := t.runScript(ctx, computerAppsScript, nil, computerStateLimit)
	if err != nil {
		return errorf("computer: %v", err)
	}
	var list []struct {
		Name      string `json:"name"`
		Frontmost bool   `json:"frontmost"`
		Windows   int    `json:"windows"`
	}
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return errorf("computer: could not read the apps script's output: %v", err)
	}
	var b strings.Builder
	for _, a := range list {
		fmt.Fprintf(&b, "%s — %d window(s)", a.Name, a.Windows)
		if a.Frontmost {
			b.WriteString(", frontmost")
		}
		b.WriteByte('\n')
	}
	if b.Len() == 0 {
		b.WriteString("no applications with a user interface are running")
	}
	return Result{Content: redactComputer(strings.TrimRight(b.String(), "\n"))}, nil
}

// computerActWire is every action script's answer: what happened, or why
// nothing did. Reason is a fixed category, never UI content or a fingerprint.
type computerActWire struct {
	OK      bool   `json:"ok"`
	Running bool   `json:"running"`
	Stale   bool   `json:"stale"`
	Role    string `json:"role"`
	Reason  string `json:"reason"`
	Value   string `json:"value"`
	Window  string `json:"window"`
	Error   string `json:"error"`
	Menus   string `json:"menus"`
}

func (t *computerTool) act(ctx context.Context, app, script string, args []string) (computerActWire, error) {
	raw, err := t.runScript(ctx, script, append([]string{app}, args...), computerActionLimit)
	if err != nil {
		return computerActWire{}, err
	}
	var out computerActWire
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return computerActWire{}, fmt.Errorf("could not read the action script's output: %v", err)
	}
	if !out.Running {
		return computerActWire{}, fmt.Errorf("%s is not running; call state first", app)
	}
	return out, nil
}

// finish renders an action's wire answer with the shared failure shapes
// handled once: staleness says "state again", a script-side error arrives
// verbatim, and success reports the front window so the model knows where
// it landed without a full state call.
func (t *computerTool) finish(out computerActWire, err error, did string) (Result, error) {
	if err != nil {
		return errorf("computer: %v", err)
	}
	if out.Stale {
		if out.Reason == "window" {
			return errorf("computer: the front window identity has changed since the last state; call state again")
		}
		return errorf("computer: the element identity has changed since the last state; call state again")
	}
	if out.Error != "" {
		msg := out.Error
		if out.Menus != "" {
			msg += "; this menu holds: " + out.Menus
		}
		return errorf("computer: %s", redactComputer(msg))
	}
	msg := did
	if out.Window != "" {
		msg += "; front window: " + out.Window
	}
	return Result{Content: redactComputer(msg)}, nil
}

func (t *computerTool) clickElement(ctx context.Context, app string, el computerElement) (Result, error) {
	if err := el.requireIdentity(); err != nil {
		return errorf("computer: %v", err)
	}
	out, err := t.act(ctx, app, computerClickScript, []string{pathArg(el.path), el.role, el.windowID, el.fingerprint})
	return t.finish(out, err, fmt.Sprintf("clicked %s in %s", el.describe(), app))
}

func (t *computerTool) clickPoint(ctx context.Context, app string, x, y int) (Result, error) {
	out, err := t.act(ctx, app, computerClickAtScript, []string{fmt.Sprint(x), fmt.Sprint(y)})
	return t.finish(out, err, fmt.Sprintf("clicked %d,%d in %s", x, y, app))
}

func (t *computerTool) typeText(ctx context.Context, app, text string) (Result, error) {
	out, err := t.act(ctx, app, computerTypeScript, []string{text})
	return t.finish(out, err, fmt.Sprintf("typed %d characters into %s", len(text), app))
}

func (t *computerTool) pressKey(ctx context.Context, app, spec, display string) (Result, error) {
	out, err := t.act(ctx, app, computerKeyScript, []string{spec})
	return t.finish(out, err, fmt.Sprintf("pressed %s in %s", display, app))
}

func (t *computerTool) setValue(ctx context.Context, app string, el computerElement, value string) (Result, error) {
	if err := el.requireIdentity(); err != nil {
		return errorf("computer: %v", err)
	}
	out, err := t.act(ctx, app, computerSetScript, []string{pathArg(el.path), el.role, value, el.windowID, el.fingerprint})
	if err == nil && out.OK {
		return t.finish(out, nil, fmt.Sprintf("set %s in %s; it now reads %q", el.describe(), app, out.Value))
	}
	return t.finish(out, err, "")
}

func (e computerElement) requireIdentity() error {
	if e.windowID == "" || e.fingerprint == "" {
		return fmt.Errorf("the recorded window or element identity is missing; call state again")
	}
	return nil
}

func (t *computerTool) pickMenu(ctx context.Context, app string, items []string) (Result, error) {
	arg, _ := json.Marshal(items)
	out, err := t.act(ctx, app, computerMenuScript, []string{string(arg)})
	return t.finish(out, err, fmt.Sprintf("picked %s in %s", strings.Join(items, " > "), app))
}

// --- plumbing ----------------------------------------------------------------

func (t *computerTool) osascript(ctx context.Context, script string, args []string, limit time.Duration) (string, error) {
	argv := append([]string{t.binary, "-l", "JavaScript", "-e", script}, args...)
	res, err := execution.Run(ctx, execution.Command{Argv: argv, Timeout: limit})
	if err != nil {
		return "", err
	}
	return interpretOSAScriptResult(res, limit)
}

func interpretOSAScriptResult(res execution.Result, limit time.Duration) (string, error) {
	if res.TimedOut {
		return "", fmt.Errorf("osascript did not answer within %s; the app may be busy, or — on the first "+
			"computer call from this terminal — a system consent dialog may be waiting on the user's screen", limit)
	}
	if res.Truncated {
		return "", fmt.Errorf("osascript output exceeded the bounded capture and was withheld")
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("osascript failed: %s", strings.TrimSpace(res.Output))
	}
	// The runner combines stdout and stderr, so any warning sits beside the
	// script's one JSON line; the first bracket-open line is the answer.
	for line := range strings.Lines(res.Output) {
		line = strings.TrimRight(line, "\n")
		if strings.HasPrefix(line, "{") || strings.HasPrefix(line, "[") {
			return line, nil
		}
	}
	return "", fmt.Errorf("osascript answered without a result: %s", strings.TrimSpace(res.Output))
}

func (t *computerTool) runOpen(ctx context.Context, app string) error {
	res, err := execution.Run(ctx, execution.Command{
		Argv:    []string{"/usr/bin/open", "-ga", app},
		Timeout: computerActionLimit,
	})
	if err != nil {
		return err
	}
	if res.Truncated {
		return fmt.Errorf("open output exceeded the bounded capture and was withheld")
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s", strings.TrimSpace(res.Output))
	}
	return nil
}

func pathArg(path []int) string {
	parts := make([]string, len(path))
	for i, p := range path {
		parts[i] = fmt.Sprint(p)
	}
	return strings.Join(parts, ",")
}

func splitMenuPath(s string) []string {
	var items []string
	for _, part := range strings.Split(s, ">") {
		if part = strings.TrimSpace(part); part != "" {
			items = append(items, part)
		}
	}
	return items
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func windowList(titles []string) string {
	if len(titles) == 0 {
		return "none"
	}
	quoted := make([]string, len(titles))
	for i, t := range titles {
		quoted[i] = fmt.Sprintf("%q", t)
	}
	return strings.Join(quoted, ", ")
}

// parseKeySpec turns "cmd+shift+s" into the JSON the key script takes: a
// character for keystroke or a code for keyCode, plus System Events
// modifier phrases. Named keys map to the hardware codes verified live;
// an unknown name lists what would have worked, because "unknown key" is
// the difference between correcting the call and abandoning the tool.
func parseKeySpec(s string) (spec, display string, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", fmt.Errorf("key needs a key name or combo, like return or cmd+s")
	}
	parts := strings.Split(s, "+")
	keyName := strings.TrimSpace(parts[len(parts)-1])
	var mods, shown []string
	for _, m := range parts[:len(parts)-1] {
		switch strings.ToLower(strings.TrimSpace(m)) {
		case "cmd", "command":
			mods, shown = append(mods, "command down"), append(shown, "cmd")
		case "shift":
			mods, shown = append(mods, "shift down"), append(shown, "shift")
		case "opt", "option", "alt":
			mods, shown = append(mods, "option down"), append(shown, "opt")
		case "ctrl", "control":
			mods, shown = append(mods, "control down"), append(shown, "ctrl")
		default:
			return "", "", fmt.Errorf("unknown modifier %q: use cmd, shift, opt, or ctrl", strings.TrimSpace(m))
		}
	}

	payload := struct {
		Char string   `json:"char,omitempty"`
		Code *int     `json:"code,omitempty"`
		Mods []string `json:"mods"`
	}{Mods: mods}

	lower := strings.ToLower(keyName)
	if code, ok := computerKeyCodes[lower]; ok {
		payload.Code = &code
		shown = append(shown, lower)
	} else if len([]rune(keyName)) == 1 {
		payload.Char = keyName
		shown = append(shown, keyName)
	} else {
		names := make([]string, 0, len(computerKeyCodes))
		for name := range computerKeyCodes {
			names = append(names, name)
		}
		sort.Strings(names)
		return "", "", fmt.Errorf("unknown key %q: use a single character or one of %s", keyName, strings.Join(names, ", "))
	}
	raw, _ := json.Marshal(payload)
	return string(raw), strings.Join(shown, "+"), nil
}

// ProbeComputer asks System Events whether the accessibility interface is
// enabled for this process — the doctor's active check, verified to answer
// {"enabled":true} on a granted terminal. It can pop the system consent
// dialog, which is why session assembly never calls it: doctor is the one
// moment a dialog on the user's screen is the point.
func ProbeComputer(ctx context.Context, binary string) error {
	res, err := execution.Run(ctx, execution.Command{
		Argv: []string{binary, "-l", "JavaScript", "-e",
			`JSON.stringify({enabled: Application("System Events").uiElementsEnabled()})`},
		Timeout: 5 * time.Second,
	})
	if err != nil {
		return err
	}
	if res.TimedOut {
		return fmt.Errorf("the probe timed out; a consent dialog may be waiting on screen")
	}
	if !strings.Contains(res.Output, `"enabled":true`) {
		return fmt.Errorf("accessibility is not granted to this terminal")
	}
	return nil
}

// computerKeyCodes maps key names to macOS virtual key codes. return,
// delete, and the arrows were exercised live; the rest are the standard
// layout-independent codes from the Carbon HIToolbox table.
var computerKeyCodes = map[string]int{
	"return": 36, "enter": 76, "tab": 48, "space": 49,
	"delete": 51, "backspace": 51, "forwarddelete": 117,
	"esc": 53, "escape": 53,
	"left": 123, "right": 124, "down": 125, "up": 126,
	"home": 115, "end": 119, "pageup": 116, "pagedown": 121,
	"f1": 122, "f2": 120, "f3": 99, "f4": 118, "f5": 96, "f6": 97,
	"f7": 98, "f8": 100, "f9": 101, "f10": 109, "f11": 103, "f12": 111,
}
