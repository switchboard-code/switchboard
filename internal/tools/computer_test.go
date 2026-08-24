package tools

// The offline half of the computer tool's verification: every wire shape
// here was captured from a live osascript run (computer_live_test.go is
// the capturer), so what these tests parse is what the scripts actually
// say, not what they were expected to say.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/permission"
)

// fakeComputer builds the tool over canned script output: each key is a
// script constant, each value the one JSON line osascript would print.
func fakeComputer(t *testing.T, outputs map[string]string) *computerTool {
	t.Helper()
	tool := NewComputer("/usr/bin/osascript").(*computerTool)
	tool.runScript = func(_ context.Context, script string, _ []string, _ time.Duration) (string, error) {
		out, ok := outputs[script]
		if !ok {
			return "", errors.New("no canned output for this script")
		}
		return out, nil
	}
	tool.launch = func(context.Context, string) error {
		return errors.New("nothing should launch in this test")
	}
	return tool
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func runComputer(t *testing.T, tool *computerTool, input string) Result {
	t.Helper()
	pln, err := tool.Plan(json.RawMessage(input))
	if err != nil {
		t.Fatalf("plan %s: %v", input, err)
	}
	res, err := pln.Run(context.Background())
	if err != nil {
		t.Fatalf("run %s: %v", input, err)
	}
	return res
}

const computerIdentityProbeScript = computerPrelude + `
function probeElement(title, position) {
  return {
    role: function() { return "AXButton"; },
    title: function() { return title; },
    description: function() { return title === "" ? "button" : title; },
    value: function() { return ""; },
    position: function() { return position; },
    size: function() { return [80, 24]; }
  };
}
function probeWindow(title, number) {
  const values = {AXSubrole: "AXStandardWindow", AXIdentifier: "", AXWindowNumber: number};
  return {
    role: function() { return "AXWindow"; },
    title: function() { return title; },
    description: function() { return "window"; },
    position: function() { return [0, 0]; },
    size: function() { return [1280, 800]; },
    attributes: {byName: function(name) { return {value: function() { return values[name]; }}; }}
  };
}
function probeProcess(window) { return {windows: function() { return [window]; }}; }
function run() {
  const save = probeElement("Save", [640, 480]);
  const anonymous = probeElement("", [40, 80]);
  const window = probeWindow("Save document", 1);
  const replacement = probeWindow("Delete document", 2);
  let currentWindow = window;
  const switchingProcess = {windows: function() { return [currentWindow]; }};
  const switchingElement = probeElement("Save", [640, 480]);
  switchingElement.description = function() { currentWindow = replacement; return "Save"; };
  const midReadWindowBefore = windowIdentity(switchingProcess);
  const midReadElement = liveElementIdentity(switchingElement);
  const midReadWindowAfter = windowIdentity(switchingProcess);
  return JSON.stringify({
    state: elementIdentity("AXButton", "Save", "Save", "", [640, 480], [80, 24]),
    live: liveElementIdentity(save),
    renamed: liveElementIdentity(probeElement("Delete", [640, 480])),
    anonymousState: elementIdentity("AXButton", "", "button", "", [40, 80], [80, 24]),
    anonymousLive: liveElementIdentity(anonymous),
    anonymousMoved: liveElementIdentity(probeElement("", [41, 80])),
    window: windowIdentity(probeProcess(window)),
    sameWindow: windowIdentity(probeProcess(probeWindow("Save document", 1))),
    replacedWindow: windowIdentity(probeProcess(replacement)),
    midReadWindowBefore: midReadWindowBefore,
    midReadElement: midReadElement,
    midReadWindowAfter: midReadWindowAfter
  });
}
`

func TestComputerIdentityFingerprintGolden(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("JXA identity golden requires macOS osascript")
	}
	tool := NewComputer("/usr/bin/osascript").(*computerTool)
	raw, err := tool.osascript(context.Background(), computerIdentityProbeScript, nil, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		State          string `json:"state"`
		Live           string `json:"live"`
		Renamed        string `json:"renamed"`
		AnonymousState string `json:"anonymousState"`
		AnonymousLive  string `json:"anonymousLive"`
		AnonymousMoved string `json:"anonymousMoved"`
		Window         string `json:"window"`
		SameWindow     string `json:"sameWindow"`
		ReplacedWindow string `json:"replacedWindow"`
		MidReadBefore  string `json:"midReadWindowBefore"`
		MidReadElement string `json:"midReadElement"`
		MidReadAfter   string `json:"midReadWindowAfter"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("decode identity probe %q: %v", raw, err)
	}
	if got.State != "d66fe6be7d2645d2874bb27879f70a26" {
		t.Fatalf("Save identity golden changed: %s", got.State)
	}
	if got.Live != got.State {
		t.Fatalf("bulk-state identity %s != unchanged live identity %s", got.State, got.Live)
	}
	if got.Renamed == got.State {
		t.Fatal("Save and same-role Delete controls have the same identity")
	}
	if got.AnonymousState != got.AnonymousLive {
		t.Fatalf("anonymous bulk-state identity %s != unchanged live identity %s", got.AnonymousState, got.AnonymousLive)
	}
	if got.AnonymousMoved == got.AnonymousState {
		t.Fatal("anonymous control geometry change retained the old identity")
	}
	if got.Window != got.SameWindow || got.ReplacedWindow == got.Window {
		t.Fatalf("window identity did not distinguish replacement: %+v", got)
	}
	if got.MidReadBefore != got.Window || got.MidReadElement != got.State || got.MidReadAfter == got.Window {
		t.Fatalf("window switch during element reads would not fail the final recheck: %+v", got)
	}

	var fixtureState computerStateWire
	if err := json.Unmarshal([]byte(fixture(t, "computer_identity_state.json")), &fixtureState); err != nil {
		t.Fatal(err)
	}
	if len(fixtureState.Els) != 1 || fixtureState.Els[0].F != got.State {
		t.Fatalf("fixture element identity does not match executed JXA golden: %+v", fixtureState.Els)
	}
	if fixtureState.WindowID != got.Window {
		t.Fatalf("fixture window identity %s does not match executed JXA golden %s", fixtureState.WindowID, got.Window)
	}
}

func TestComputerStateFormatsTheCapturedWalk(t *testing.T) {
	tool := fakeComputer(t, map[string]string{
		computerStateScript: fixture(t, "computer_state.json"),
	})
	res := runComputer(t, tool, `{"action":"state","app":"TextEdit"}`)
	if res.IsError {
		t.Fatalf("state errored: %s", res.Content)
	}
	for _, want := range []string{
		`TextEdit — frontmost: yes; windows: "Untitled"`,
		"menus: Apple, TextEdit, File",
		`[0] AXColorWell (text color) = "rgb 1 1 1 1" at 404,142`,
		"low-signal elements hidden",
	} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("state output missing %q:\n%s", want, res.Content)
		}
	}

	// The walk armed the element cache: a click plan may now name an index,
	// and carries the recorded role into its request detail.
	pln, err := tool.Plan(json.RawMessage(`{"action":"click","app":"TextEdit","element":0}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pln.Request.Detail, "AXColorWell") {
		t.Errorf("click detail does not name the element: %q", pln.Request.Detail)
	}
}

func TestComputerAppsListsWhatRuns(t *testing.T) {
	tool := fakeComputer(t, map[string]string{
		computerAppsScript: fixture(t, "computer_apps.json"),
	})
	res := runComputer(t, tool, `{"action":"apps"}`)
	if res.IsError {
		t.Fatalf("apps errored: %s", res.Content)
	}
	for _, want := range []string{
		"Finder — 0 window(s)",
		"Safari — 1 window(s), frontmost",
	} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("apps output missing %q:\n%s", want, res.Content)
		}
	}
}

func TestComputerActionsCarryTheExternalEffect(t *testing.T) {
	tool := fakeComputer(t, map[string]string{
		computerStateScript: fixture(t, "computer_state.json"),
	})
	runComputer(t, tool, `{"action":"state","app":"TextEdit"}`) // arms element 0

	for _, input := range []string{
		`{"action":"apps"}`,
		`{"action":"state","app":"TextEdit"}`,
		`{"action":"click","app":"TextEdit","element":0}`,
		`{"action":"click","app":"TextEdit","x":10,"y":20}`,
		`{"action":"type","app":"TextEdit","text":"hi"}`,
		`{"action":"key","app":"TextEdit","key":"cmd+s"}`,
		`{"action":"set","app":"TextEdit","element":0,"text":"v"}`,
		`{"action":"menu","app":"TextEdit","menu":"File > Save"}`,
	} {
		pln, err := tool.Plan(json.RawMessage(input))
		if err != nil {
			t.Fatalf("plan %s: %v", input, err)
		}
		if pln.Request.Effect != permission.EffectExternal {
			t.Errorf("%s carries %s, want external", input, pln.Request.Effect)
		}
		if pln.Request.Detail == "" {
			t.Errorf("%s has no display detail", input)
		}
		if strings.Contains(input, "TextEdit") && pln.Request.Path != "TextEdit" {
			t.Errorf("%s puts %q in Path; the app is the approval's grain", input, pln.Request.Path)
		}
	}
}

func TestComputerClickBeforeStateNamesTheFix(t *testing.T) {
	tool := fakeComputer(t, nil)
	_, err := tool.Plan(json.RawMessage(`{"action":"click","app":"Safari","element":3}`))
	if err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatalf("want an error naming state, got %v", err)
	}
}

func TestComputerStaleElementSaysStateAgain(t *testing.T) {
	tool := fakeComputer(t, map[string]string{
		computerStateScript: fixture(t, "computer_identity_state.json"),
		computerClickScript: `{"running":true,"stale":true,"role":"AXGroup"}`,
	})
	runComputer(t, tool, `{"action":"state","app":"TextEdit"}`)
	res := runComputer(t, tool, `{"action":"click","app":"TextEdit","element":0}`)
	if !res.IsError || !strings.Contains(res.Content, "state again") {
		t.Fatalf("a stale path must say state again, got: %s", res.Content)
	}
}

func TestComputerElementActionsBindRecordedIdentities(t *testing.T) {
	const windowID = "6a177f160b6ac33a31cb1c9035ee004e"
	const fingerprint = "d66fe6be7d2645d2874bb27879f70a26"

	tests := []struct {
		name       string
		state      string
		action     string
		answer     string
		wantError  string
		wantReason string
	}{
		{
			name:       "save button replaced by delete at the same role and path",
			state:      fixture(t, "computer_identity_state.json"),
			action:     `{"action":"click","app":"TextEdit","element":0}`,
			answer:     `{"running":true,"stale":true,"role":"AXButton","reason":"element"}`,
			wantError:  "element identity has changed",
			wantReason: "element",
		},
		{
			name:       "front window replaced",
			state:      fixture(t, "computer_identity_state.json"),
			action:     `{"action":"click","app":"TextEdit","element":0}`,
			answer:     `{"running":true,"stale":true,"role":"window","reason":"window"}`,
			wantError:  "front window identity has changed",
			wantReason: "window",
		},
		{
			name: "anonymous control geometry changed",
			state: `{"running":true,"frontmost":true,"windows":["Calculator"],` +
				`"window_id":"` + windowID + `","menus":[],"els":[` +
				`{"path":[0,4],"r":"AXButton","t":"","d":"button","v":"",` +
				`"p":[40,80],"s":[32,32],"f":"` + fingerprint + `"}],"walked":1,"timedOut":false}`,
			action:     `{"action":"click","app":"TextEdit","element":0}`,
			answer:     `{"running":true,"stale":true,"role":"AXButton","reason":"element"}`,
			wantError:  "element identity has changed",
			wantReason: "element",
		},
		{
			name:      "unchanged control succeeds",
			state:     fixture(t, "computer_identity_state.json"),
			action:    `{"action":"click","app":"TextEdit","element":0}`,
			answer:    `{"running":true,"ok":true,"window":"Save document"}`,
			wantError: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := NewComputer("/usr/bin/osascript").(*computerTool)
			var actionArgs []string
			tool.runScript = func(_ context.Context, script string, args []string, _ time.Duration) (string, error) {
				switch script {
				case computerStateScript:
					return tt.state, nil
				case computerClickScript:
					actionArgs = append([]string(nil), args...)
					return tt.answer, nil
				default:
					return "", fmt.Errorf("unexpected script")
				}
			}
			tool.launch = func(context.Context, string) error { return errors.New("unexpected launch") }

			runComputer(t, tool, `{"action":"state","app":"TextEdit"}`)
			res := runComputer(t, tool, tt.action)
			if tt.wantError == "" {
				if res.IsError || !strings.Contains(res.Content, "clicked AXButton") {
					t.Fatalf("unchanged identity did not click: %+v", res)
				}
			} else if !res.IsError || !strings.Contains(res.Content, tt.wantError) || !strings.Contains(res.Content, "state again") {
				t.Fatalf("%s mismatch did not fail closed: %+v", tt.wantReason, res)
			}
			if res.IsError {
				for _, private := range []string{windowID, fingerprint, "Save", "Delete"} {
					if strings.Contains(res.Content, private) {
						t.Fatalf("action error exposed private identity material %q: %s", private, res.Content)
					}
				}
			}

			wantArgs := []string{"TextEdit", "0,2", "AXButton", windowID, fingerprint}
			if strings.Contains(tt.name, "anonymous") {
				wantArgs[1] = "0,4"
			}
			if fmt.Sprint(actionArgs) != fmt.Sprint(wantArgs) {
				t.Fatalf("action args = %q, want %q", actionArgs, wantArgs)
			}
			joined := strings.Join(actionArgs, " ")
			for _, rawUI := range []string{"Save", "Delete", "Save document"} {
				if strings.Contains(joined, rawUI) {
					t.Fatalf("raw UI identity %q leaked into action arguments %q", rawUI, actionArgs)
				}
			}
		})
	}
}

func TestComputerMutationScriptsCheckIdentityImmediatelyBeforeAction(t *testing.T) {
	for _, tc := range []struct {
		name     string
		script   string
		mutation string
	}{
		{name: "click", script: computerClickScript, mutation: "el.click()"},
		{name: "set", script: computerSetScript, mutation: "el.value = argv[3]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			firstWindowCheck := strings.Index(tc.script, "windowIdentity(p) !==")
			finalWindowCheck := strings.LastIndex(tc.script, "windowIdentity(p) !==")
			elementCheck := strings.LastIndex(tc.script, "liveElementIdentity(el) !==")
			mutation := strings.LastIndex(tc.script, tc.mutation)
			if firstWindowCheck < 0 || elementCheck < 0 || finalWindowCheck <= firstWindowCheck || mutation < 0 ||
				!(firstWindowCheck < elementCheck && elementCheck < finalWindowCheck && finalWindowCheck < mutation) {
				t.Fatalf("window and element checks must bracket resolution and immediately precede %s", tc.mutation)
			}
		})
	}
	for _, want := range []string{
		`window_id: ""`,
		"f: elementIdentity(",
		`anonymous ? position : ""`,
		`anonymous ? size : ""`,
	} {
		if !strings.Contains(computerStateScript, want) {
			t.Errorf("state script is missing identity binding %q", want)
		}
	}
}

func TestComputerSetBindsRecordedIdentities(t *testing.T) {
	tool := NewComputer("/usr/bin/osascript").(*computerTool)
	var setArgs []string
	tool.runScript = func(_ context.Context, script string, args []string, _ time.Duration) (string, error) {
		switch script {
		case computerStateScript:
			return fixture(t, "computer_identity_state.json"), nil
		case computerSetScript:
			setArgs = append([]string(nil), args...)
			return `{"running":true,"ok":true,"value":"Draft","window":"Save document"}`, nil
		default:
			return "", fmt.Errorf("unexpected script")
		}
	}
	tool.launch = func(context.Context, string) error { return errors.New("unexpected launch") }
	runComputer(t, tool, `{"action":"state","app":"TextEdit"}`)
	res := runComputer(t, tool, `{"action":"set","app":"TextEdit","element":0,"text":"Draft"}`)
	if res.IsError {
		t.Fatalf("unchanged identity did not set: %+v", res)
	}
	want := []string{"TextEdit", "0,2", "AXButton", "Draft", "6a177f160b6ac33a31cb1c9035ee004e", "d66fe6be7d2645d2874bb27879f70a26"}
	if fmt.Sprint(setArgs) != fmt.Sprint(want) {
		t.Fatalf("set args = %q, want %q", setArgs, want)
	}
}

func TestComputerMissingRecordedIdentityFailsClosed(t *testing.T) {
	tool := fakeComputer(t, map[string]string{computerStateScript: fixture(t, "computer_state.json")})
	runComputer(t, tool, `{"action":"state","app":"TextEdit"}`)
	res := runComputer(t, tool, `{"action":"click","app":"TextEdit","element":0}`)
	if !res.IsError || !strings.Contains(res.Content, "identity is missing") || !strings.Contains(res.Content, "state again") {
		t.Fatalf("missing identity did not fail closed: %+v", res)
	}
}

func TestComputerRedactsWhatItReadsBack(t *testing.T) {
	// Another app's UI can hold anything — this fixture plants a key-shaped
	// label and value the way a password manager or a visible .env would.
	token := "sk-ant-" + strings.Repeat("a", 24)
	const windowID = "6a177f160b6ac33a31cb1c9035ee004e"
	const fingerprint = "d66fe6be7d2645d2874bb27879f70a26"
	state := fmt.Sprintf(`{"running":true,"frontmost":true,"windows":["w"],"window_id":%q,"menus":[],`+
		`"els":[{"path":[0],"r":"AXTextArea","t":%q,"d":"text entry area","v":%q,"p":[0,0],"s":[10,10],"f":%q}],`+
		`"walked":1,"timedOut":false}`, windowID, token, token, fingerprint)
	tool := fakeComputer(t, map[string]string{
		computerStateScript: state,
		computerClickScript: `{"running":true,"stale":true,"role":"AXTextArea","reason":"element"}`,
		computerSetScript:   `{"running":true,"stale":true,"role":"AXTextArea","reason":"element"}`,
	})
	res := runComputer(t, tool, `{"action":"state","app":"Vault"}`)
	if strings.Contains(res.Content, token) {
		t.Fatal("a key read off another app's UI reached the result")
	}
	if !strings.Contains(res.Content, "redacted") {
		t.Errorf("the redaction should name what it held back:\n%s", res.Content)
	}
	for _, action := range []string{
		`{"action":"click","app":"Vault","element":0}`,
		`{"action":"set","app":"Vault","element":0,"text":"public"}`,
	} {
		pln, err := tool.Plan(json.RawMessage(action))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(pln.Request.Detail, token) || strings.Contains(pln.Request.Detail, fingerprint) {
			t.Fatalf("approval detail exposed UI identity material: %s", pln.Request.Detail)
		}
		res, err = pln.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, private := range []string{token, windowID, fingerprint} {
			if strings.Contains(res.Content, private) {
				t.Fatalf("action error exposed UI identity material %q: %s", private, res.Content)
			}
		}
	}
}

func TestComputerWithholdsTruncatedScriptOutput(t *testing.T) {
	token := "ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	fragmented := token[:19] + "\n[omitted]\n" + token[20:]
	_, err := interpretOSAScriptResult(execution.Result{
		Output: fragmented, ExitCode: 1, Truncated: true,
	}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "output exceeded") {
		t.Fatalf("truncated script result = %v", err)
	}
	if strings.Contains(err.Error(), "ghp_") || strings.Contains(err.Error(), token) {
		t.Fatalf("truncated script fragments reached the error: %v", err)
	}
}

func TestComputerRefusesToTypeASecret(t *testing.T) {
	token := "sk-ant-" + strings.Repeat("b", 24)
	tool := fakeComputer(t, nil)
	for _, input := range []string{
		fmt.Sprintf(`{"action":"type","app":"Safari","text":%q}`, token),
		fmt.Sprintf(`{"action":"set","app":"Safari","element":0,"text":%q}`, token),
	} {
		_, err := tool.Plan(json.RawMessage(input))
		if err == nil {
			t.Fatalf("%s should refuse a key-shaped string", input)
		}
		if strings.Contains(err.Error(), token) {
			t.Fatal("the refusal quoted the key it exists to hold back")
		}
	}
}

func TestComputerKeySpecs(t *testing.T) {
	cases := []struct {
		in      string
		char    string
		code    int
		mods    []string
		display string
	}{
		{"return", "", 36, nil, "return"},
		{"cmd+s", "s", 0, []string{"command down"}, "cmd+s"},
		{"shift+tab", "", 48, []string{"shift down"}, "shift+tab"},
		{"cmd+shift+t", "t", 0, []string{"command down", "shift down"}, "cmd+shift+t"},
		{"Escape", "", 53, nil, "escape"},
	}
	for _, c := range cases {
		spec, display, err := parseKeySpec(c.in)
		if err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		var got struct {
			Char string   `json:"char"`
			Code *int     `json:"code"`
			Mods []string `json:"mods"`
		}
		if err := json.Unmarshal([]byte(spec), &got); err != nil {
			t.Errorf("%s: bad spec %s", c.in, spec)
			continue
		}
		if got.Char != c.char {
			t.Errorf("%s: char %q, want %q", c.in, got.Char, c.char)
		}
		if c.char == "" && (got.Code == nil || *got.Code != c.code) {
			t.Errorf("%s: code %v, want %d", c.in, got.Code, c.code)
		}
		if len(got.Mods) != len(c.mods) {
			t.Errorf("%s: mods %v, want %v", c.in, got.Mods, c.mods)
		}
		if display != c.display {
			t.Errorf("%s: display %q, want %q", c.in, display, c.display)
		}
	}

	if _, _, err := parseKeySpec("superkey"); err == nil || !strings.Contains(err.Error(), "return") {
		t.Errorf("an unknown key should list what would have worked, got %v", err)
	}
	if _, _, err := parseKeySpec("hyper+s"); err == nil || !strings.Contains(err.Error(), "cmd") {
		t.Errorf("an unknown modifier should list the real ones, got %v", err)
	}
}

func TestComputerLaunchesWhatIsNotRunning(t *testing.T) {
	tool := NewComputer("/usr/bin/osascript").(*computerTool)
	var mu sync.Mutex
	launched := 0
	stateCalls := 0
	tool.launch = func(context.Context, string) error {
		mu.Lock()
		defer mu.Unlock()
		launched++
		return nil
	}
	running := fixture(t, "computer_state.json")
	tool.runScript = func(_ context.Context, script string, _ []string, _ time.Duration) (string, error) {
		if script != computerStateScript {
			return "", errors.New("only state should run here")
		}
		mu.Lock()
		defer mu.Unlock()
		stateCalls++
		if stateCalls == 1 {
			return `{"running":false}`, nil
		}
		return running, nil
	}
	res := runComputer(t, tool, `{"action":"state","app":"TextEdit"}`)
	if res.IsError {
		t.Fatalf("state errored: %s", res.Content)
	}
	if launched != 1 {
		t.Errorf("launched %d times, want 1", launched)
	}
	if !strings.Contains(res.Content, "[0]") {
		t.Errorf("the post-launch walk went missing:\n%s", res.Content)
	}
}

func TestComputerMenuNeedsAPath(t *testing.T) {
	tool := fakeComputer(t, nil)
	for _, input := range []string{
		`{"action":"menu","app":"TextEdit","menu":"Save"}`,
		`{"action":"menu","app":"TextEdit","menu":""}`,
	} {
		if _, err := tool.Plan(json.RawMessage(input)); err == nil || !strings.Contains(err.Error(), "File > Save") {
			t.Errorf("%s should teach the path shape, got %v", input, err)
		}
	}
}
