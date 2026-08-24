package tools

import (
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/continuity"
)

func TestTodoReplacesListAndSnapshots(t *testing.T) {
	r, _ := newRegistry(t)

	res := run(t, r, "todo", map[string]any{"items": []map[string]any{
		{"text": "read the failing test", "status": "done"},
		{"text": "fix the off-by-one", "status": "active"},
		{"text": "run the suite", "status": "pending"},
	}})
	if res.IsError {
		t.Fatalf("todo failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "[x] read the failing test") ||
		!strings.Contains(res.Content, "[>] fix the off-by-one") ||
		!strings.Contains(res.Content, "[ ] run the suite") {
		t.Errorf("result must render the list with markers: %q", res.Content)
	}
	if !strings.Contains(res.Content, "1 of 3 done") {
		t.Errorf("result must summarize progress: %q", res.Content)
	}

	items := r.Todos()
	if len(items) != 3 || items[1].Status != TodoActive {
		t.Fatalf("snapshot = %+v, want the three items as sent", items)
	}

	// The next call replaces, never appends.
	run(t, r, "todo", map[string]any{"items": []map[string]any{
		{"text": "only one left", "status": "active"},
	}})
	if items := r.Todos(); len(items) != 1 {
		t.Errorf("second call must replace the list, got %+v", items)
	}
}

func TestTodoClearsOnEmptyList(t *testing.T) {
	r, _ := newRegistry(t)
	run(t, r, "todo", map[string]any{"items": []map[string]any{
		{"text": "something", "status": "pending"},
	}})

	res := run(t, r, "todo", map[string]any{"items": []map[string]any{}})
	if !strings.Contains(res.Content, "cleared") {
		t.Errorf("clearing must say so: %q", res.Content)
	}
	if items := r.Todos(); len(items) != 0 {
		t.Errorf("list not cleared: %+v", items)
	}
}

func TestTodoRejectsMalformedLists(t *testing.T) {
	r, _ := newRegistry(t)

	cases := []struct {
		name  string
		items []map[string]any
	}{
		{"two active items", []map[string]any{
			{"text": "a", "status": "active"},
			{"text": "b", "status": "active"},
		}},
		{"unknown status", []map[string]any{
			{"text": "a", "status": "blocked"},
		}},
		{"empty text", []map[string]any{
			{"text": "  ", "status": "pending"},
		}},
		{"control character", []map[string]any{
			{"text": "ok\x00still", "status": "pending"},
		}},
		{"newline injection", []map[string]any{
			{"text": "real\n[x] forged", "status": "pending"},
		}},
		{"carriage return injection", []map[string]any{
			{"text": "real\rforged", "status": "pending"},
		}},
		{"tab injection", []map[string]any{
			{"text": "real\tforged", "status": "pending"},
		}},
	}
	for _, c := range cases {
		if _, err := tryRun(r, "todo", map[string]any{"items": c.items}); err == nil {
			t.Errorf("%s must fail at Plan time", c.name)
		}
	}

	// A rejected call must not disturb the stored list.
	if items := r.Todos(); len(items) != 0 {
		t.Errorf("a rejected call changed state: %+v", items)
	}
}

func TestTodoCanonicalizesExactlyWhatContinuityCanStore(t *testing.T) {
	r, _ := newRegistry(t)
	long := strings.Repeat("界", 200)
	res := run(t, r, "todo", map[string]any{"items": []map[string]any{{"text": "  " + long + "  ", "status": "active"}}})
	if res.IsError {
		t.Fatalf("todo failed: %s", res.Content)
	}
	items := r.Todos()
	if len(items) != 1 || items[0].Text == long || len(items[0].Text) > 256 {
		t.Fatalf("todo did not keep the canonical bounded text: %+v", items)
	}
}

func TestRestoreContinuityReplacesClonesAndClears(t *testing.T) {
	r, _ := newRegistry(t)
	input := []TodoItem{
		{Text: "restored active", Status: TodoActive},
		{Text: "restored pending", Status: TodoPending},
	}
	working := continuity.Working{
		Objective:     "finish session B",
		NextAction:    "continue session B",
		StopCondition: "session B tests pass",
	}
	if err := r.RestoreContinuity(input, working); err != nil {
		t.Fatal(err)
	}
	input[0].Text = "mutated input"
	first := r.Todos()
	first[0].Text = "mutated snapshot"
	if got := r.Todos()[0].Text; got != "restored active" {
		t.Fatalf("restore retained caller-owned storage: %q", got)
	}
	if got := r.Working(); got != working {
		t.Fatalf("restored working context = %+v, want %+v", got, working)
	}

	// A normal todo update intentionally keeps an omitted objective and stop
	// condition within one session. They must come from the restored session,
	// never from whichever session happened to be bound before it.
	run(t, r, "todo", map[string]any{"items": []map[string]any{{
		"text": "updated session B task", "status": "active",
	}}})
	if got := r.Working(); got != (continuity.Working{
		Objective:     working.Objective,
		StopCondition: working.StopCondition,
	}) {
		t.Fatalf("todo omission did not retain only restored context: %+v", got)
	}

	if err := r.RestoreContinuity(nil, continuity.Working{}); err != nil {
		t.Fatal(err)
	}
	if got := r.Todos(); len(got) != 0 {
		t.Fatalf("nil restore did not clear old-session todos: %+v", got)
	}
	if got := r.Working(); got != (continuity.Working{}) {
		t.Fatalf("nil restore did not clear old-session working context: %+v", got)
	}

	run(t, r, "todo", map[string]any{"items": []map[string]any{{
		"text": "fresh-session task", "status": "active",
	}}})
	if got := r.Working(); got != (continuity.Working{}) {
		t.Fatalf("todo omission resurrected cleared working context: %+v", got)
	}
}

func TestRestoreContinuityRejectsMalformedStateWithoutMutation(t *testing.T) {
	r, _ := newRegistry(t)
	want := []TodoItem{{Text: "keep", Status: TodoPending}}
	wantWorking := continuity.Working{Objective: "keep objective", StopCondition: "keep stop"}
	if err := r.RestoreContinuity(want, wantWorking); err != nil {
		t.Fatal(err)
	}
	for name, items := range map[string][]TodoItem{
		"empty text": {{Text: " ", Status: TodoPending}},
		"bad status": {{Text: "bad", Status: "blocked"}},
		"two active": {{Text: "one", Status: TodoActive}, {Text: "two", Status: TodoActive}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := r.RestoreContinuity(items, continuity.Working{Objective: "must not publish"}); err == nil {
				t.Fatal("invalid restored list was accepted")
			}
			got := r.Todos()
			if len(got) != 1 || got[0] != want[0] {
				t.Fatalf("failed restore changed state: %+v", got)
			}
			if got := r.Working(); got != wantWorking {
				t.Fatalf("failed restore changed working context: %+v", got)
			}
		})
	}
}
