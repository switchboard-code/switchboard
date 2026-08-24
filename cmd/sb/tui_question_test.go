package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/tools"
)

func questionFixture(multi bool) tools.Question {
	return tools.Question{
		Question: "which store should the cache use?",
		Multi:    multi,
		Options: []tools.QuestionOption{
			{Label: "sqlite", Detail: "one file, no server"},
			{Label: "bolt"},
			{Label: "memory"},
		},
	}
}

func openQuestion(t *testing.T, multi bool) (*tuiModel, chan tools.Answer) {
	t.Helper()
	m := testModel(t)
	respond := make(chan tools.Answer, 1)
	m.dlg = newQuestionDialog(questionFixture(multi), respond)
	return m, respond
}

func answered(t *testing.T, respond chan tools.Answer) tools.Answer {
	t.Helper()
	select {
	case ans := <-respond:
		return ans
	default:
		t.Fatal("the dialog closed without resolving; the loop would hang on this channel")
		return tools.Answer{}
	}
}

func TestQuestionDialogSingleSelect(t *testing.T) {
	m, respond := openQuestion(t, false)

	view := m.dlg.view(80, m.th)
	if !strings.Contains(view, "which store should the cache use?") ||
		!strings.Contains(view, "sqlite") || !strings.Contains(view, "one file, no server") {
		t.Fatalf("the dialog must show the question, the options, and their details:\n%s", view)
	}

	m.dlg.update(tea.KeyMsg{Type: tea.KeyDown}, m.th)
	m.dlg.update(tea.KeyMsg{Type: tea.KeyDown}, m.th)
	done, _ := m.dlg.update(tea.KeyMsg{Type: tea.KeyEnter}, m.th)
	if !done {
		t.Fatal("enter did not close the dialog")
	}
	if ans := answered(t, respond); len(ans.Picked) != 1 || ans.Picked[0] != "bolt" {
		t.Fatalf("answer = %+v, want the highlighted option", ans)
	}
}

func TestQuestionDialogComposerRunesDoNotChooseAnAsyncAnswer(t *testing.T) {
	m, respond := openQuestion(t, false)

	for _, typed := range []rune{'j', '2'} {
		if done, cmd := m.dlg.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{typed}}, m.th); done || cmd != nil {
			t.Fatalf("composer rune %q resolved an asynchronously opened question", typed)
		}
	}
	if done, cmd := m.dlg.update(tea.KeyMsg{Type: tea.KeyEnter}, m.th); done || cmd != nil {
		t.Fatal("Enter after unrelated composer runes resolved the neutral question")
	}
	select {
	case ans := <-respond:
		t.Fatalf("composer text answered the question: %+v", ans)
	default:
	}

	m.dlg.update(tea.KeyMsg{Type: tea.KeyDown}, m.th)
	done, _ := m.dlg.update(tea.KeyMsg{Type: tea.KeyEnter}, m.th)
	if !done {
		t.Fatal("arrow navigation followed by Enter did not answer")
	}
	if ans := answered(t, respond); len(ans.Picked) != 1 || ans.Picked[0] != "sqlite" {
		t.Fatalf("answer = %+v, want first deliberately selected option", ans)
	}
}

func TestQuestionDialogStrayEnterDoesNotAnswer(t *testing.T) {
	m, respond := openQuestion(t, false)

	if done, cmd := m.dlg.update(tea.KeyMsg{Type: tea.KeyEnter}, m.th); done || cmd != nil {
		t.Fatalf("neutral Enter = done %v, cmd %v; want no action", done, cmd != nil)
	}
	select {
	case ans := <-respond:
		t.Fatalf("neutral Enter answered an asynchronously opened question: %+v", ans)
	default:
	}

	m.dlg.update(tea.KeyMsg{Type: tea.KeyDown}, m.th)
	done, _ := m.dlg.update(tea.KeyMsg{Type: tea.KeyEnter}, m.th)
	if !done {
		t.Fatal("navigation followed by Enter did not answer")
	}
	if ans := answered(t, respond); len(ans.Picked) != 1 || ans.Picked[0] != "sqlite" {
		t.Fatalf("answer after navigation = %+v, want sqlite", ans)
	}
}

func TestQuestionDialogMultiMarksInOfferedOrder(t *testing.T) {
	m, respond := openQuestion(t, true)

	// Mark the third option first, then the first: the answer must come
	// back in offered order, the shape the model asked the question in.
	for range 3 {
		m.dlg.update(tea.KeyMsg{Type: tea.KeyDown}, m.th)
	}
	m.dlg.update(tea.KeyMsg{Type: tea.KeySpace}, m.th)
	for range 2 {
		m.dlg.update(tea.KeyMsg{Type: tea.KeyUp}, m.th)
	}
	m.dlg.update(tea.KeyMsg{Type: tea.KeySpace}, m.th)

	if view := m.dlg.view(80, m.th); !strings.Contains(view, "[x]") {
		t.Fatalf("a marked option must show its mark:\n%s", view)
	}

	done, _ := m.dlg.update(tea.KeyMsg{Type: tea.KeyEnter}, m.th)
	if !done {
		t.Fatal("enter did not close the dialog")
	}
	if ans := answered(t, respond); strings.Join(ans.Picked, ",") != "sqlite,memory" {
		t.Fatalf("answer = %+v, want sqlite,memory in offered order", ans)
	}
}

func TestQuestionDialogMultiEnterWithNoMarksPicksTheHighlighted(t *testing.T) {
	m, respond := openQuestion(t, true)

	m.dlg.update(tea.KeyMsg{Type: tea.KeyDown}, m.th)
	m.dlg.update(tea.KeyMsg{Type: tea.KeyDown}, m.th)
	done, _ := m.dlg.update(tea.KeyMsg{Type: tea.KeyEnter}, m.th)
	if !done {
		t.Fatal("enter did not close the dialog")
	}
	if ans := answered(t, respond); strings.Join(ans.Picked, ",") != "bolt" {
		t.Fatalf("answer = %+v, want the highlighted option alone", ans)
	}
}

func TestQuestionDialogTypedAnswer(t *testing.T) {
	m, respond := openQuestion(t, false)

	// Down past the options lands on the type-your-own row.
	for range 4 {
		m.dlg.update(tea.KeyMsg{Type: tea.KeyDown}, m.th)
	}
	if done, _ := m.dlg.update(tea.KeyMsg{Type: tea.KeyEnter}, m.th); done {
		t.Fatal("arming the input must not close the dialog")
	}
	m.dlg.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("neither, keep it in memory")}, m.th)
	done, _ := m.dlg.update(tea.KeyMsg{Type: tea.KeyEnter}, m.th)
	if !done {
		t.Fatal("enter did not send the typed answer")
	}
	if ans := answered(t, respond); ans.Text != "neither, keep it in memory" {
		t.Fatalf("answer = %+v, want the typed text", ans)
	}
}

func TestQuestionDialogEscapeDeclines(t *testing.T) {
	m, respond := openQuestion(t, false)

	done, _ := m.dlg.update(tea.KeyMsg{Type: tea.KeyEscape}, m.th)
	if !done {
		t.Fatal("esc did not close the dialog")
	}
	if ans := answered(t, respond); !ans.Declined {
		t.Fatalf("answer = %+v, want a decline: the loop is blocked and must hear something", ans)
	}
}

func TestQuestionDialogEscapesModelAuthoredTerminalControls(t *testing.T) {
	respond := make(chan tools.Answer, 1)
	d := newQuestionDialog(tools.Question{
		Question: "choose\x1b]2;forged\a",
		Options: []tools.QuestionOption{{
			Label: "safe\x1b[2J", Detail: "detail\r\u202e spoof",
		}},
	}, respond)
	view := d.view(80, darkTheme())
	plain := stripANSI(view)
	for _, unsafe := range []string{"\x1b", "\a", "\r", "\u202e"} {
		if strings.Contains(plain, unsafe) {
			t.Fatalf("question retained terminal control %q: %q", unsafe, plain)
		}
	}
	// Rendering is escaped, but answering keeps the exact offered label; UI
	// hardening must not mutate the protocol between the user and the model.
	d.update(tea.KeyMsg{Type: tea.KeyDown}, darkTheme())
	d.update(tea.KeyMsg{Type: tea.KeyEnter}, darkTheme())
	if answer := answered(t, respond); len(answer.Picked) != 1 || answer.Picked[0] != "safe\x1b[2J" {
		t.Fatalf("answer mutated by display escaping: %+v", answer)
	}
}

func TestQuestionDialogEscapeWhileTypingReturnsToTheList(t *testing.T) {
	m, respond := openQuestion(t, false)

	for range 4 {
		m.dlg.update(tea.KeyMsg{Type: tea.KeyDown}, m.th)
	}
	m.dlg.update(tea.KeyMsg{Type: tea.KeyEnter}, m.th)
	if done, _ := m.dlg.update(tea.KeyMsg{Type: tea.KeyEscape}, m.th); done {
		t.Fatal("esc while typing must back out to the options, not decline")
	}
	select {
	case ans := <-respond:
		t.Fatalf("nothing should have resolved yet, got %+v", ans)
	default:
	}
}

func TestParseQuestionPicks(t *testing.T) {
	q := questionFixture(false)
	multi := questionFixture(true)

	cases := []struct {
		name   string
		answer string
		q      tools.Question
		want   string
		ok     bool
	}{
		{"single number", "2", q, "bolt", true},
		{"single refuses two numbers", "1 2", q, "", false},
		{"multi numbers", "3 1", multi, "sqlite,memory", true},
		{"multi with commas", "1,2", multi, "sqlite,bolt", true},
		{"out of range", "4", q, "", false},
		{"words are words", "just cache in memory", q, "", false},
		{"half-numeric is words", "1 maybe", multi, "", false},
	}
	for _, tc := range cases {
		got, ok := parseQuestionPicks(tc.answer, tc.q)
		if ok != tc.ok || strings.Join(got, ",") != tc.want {
			t.Errorf("%s: parseQuestionPicks(%q) = %v, %v; want %q, %v", tc.name, tc.answer, got, ok, tc.want, tc.ok)
		}
	}
}

// An MCP server may elicit at any moment, including while another picker is
// on screen. The broker keeps both: the visible picker finishes first, then
// the waiting question is shown and can resolve its blocked caller.
func TestAQuestionArrivingOverAnOpenDialogIsQueued(t *testing.T) {
	m := testModel(t)
	m.dlg = &pickerDialog{title: "already open", items: []pickerItem{{id: "a", label: "a"}}}

	respond := make(chan tools.Answer, 1)
	m.Update(questionMsg{q: tools.Question{Question: "may I?"}, respond: respond})

	if _, ok := m.dlg.(*questionDialog); ok {
		t.Fatal("the open dialog was replaced instead of queued")
	}
	if len(m.dialogQueue) != 1 {
		t.Fatalf("queued dialogs = %d, want 1", len(m.dialogQueue))
	}
	select {
	case answer := <-respond:
		t.Fatalf("queued question resolved before it was shown: %+v", answer)
	default:
	}

	m.key(tea.KeyMsg{Type: tea.KeyEsc})
	if _, ok := m.dlg.(*questionDialog); !ok {
		t.Fatalf("next dialog is %T, want question", m.dlg)
	}
	m.key(tea.KeyMsg{Type: tea.KeyEsc})
	if answer := <-respond; !answer.Declined {
		t.Fatalf("dismissed question = %+v, want declined", answer)
	}
}
