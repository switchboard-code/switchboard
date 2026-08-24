package continuity

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func fixture() Capsule {
	return Capsule{
		Source:        SourceManual,
		BasisMessages: 3,
		Objective:     "finish continuity safely",
		Phase:         "verification",
		NextAction:    "run focused tests",
		Tasks: []Task{
			{Text: "implement", Status: TaskDone},
			{Text: "verify", Status: TaskActive},
		},
		Facts:     []string{"the WAL is append-only"},
		Decisions: []Decision{{Text: "use the session WAL", Reason: "it orders state with messages"}},
		Files:     []File{{Path: "internal/session/session.go", State: "present", SHA256: strings.Repeat("a", 64)}},
	}
}

func TestPrepareHasStableContentIdentityAndStoredRoundTrip(t *testing.T) {
	first, err := Prepare(fixture())
	if err != nil {
		t.Fatal(err)
	}
	second, err := Prepare(fixture())
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || first.ID != second.ID {
		t.Fatalf("deterministic IDs differ: %q != %q", first.ID, second.ID)
	}
	changed := fixture()
	changed.BasisMessages++
	third, err := Prepare(changed)
	if err != nil {
		t.Fatal(err)
	}
	if third.ID == first.ID {
		t.Fatal("changing the conversation basis did not change the capsule ID")
	}

	raw, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeStored(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != first.ID || got.NextAction != first.NextAction || len(got.Tasks) != len(first.Tasks) {
		t.Fatalf("stored round trip changed capsule: %+v", got)
	}
}

func TestPrepareRedactsAndBoundsEveryFreeTextSurface(t *testing.T) {
	secret := "sk-ant-api03-" + "abcdefghijklmnopqrstuvwx"
	long := strings.Repeat("界", MaxNarrativeBytes)
	in := Capsule{
		Source:        SourceManual,
		BasisMessages: 2,
		ParentSession: secret,
		Objective:     "do not store " + secret,
		Narrative:     long + secret,
		Tasks:         make([]Task, MaxTasks+5),
		Facts:         make([]string, MaxFacts+5),
		Rejected:      make([]string, MaxRejected+5),
		Decisions:     make([]Decision, MaxDecisions+5),
		Files:         make([]File, MaxFiles+5),
	}
	for i := range in.Tasks {
		in.Tasks[i] = Task{Text: long, Status: TaskDone}
	}
	for i := range in.Facts {
		in.Facts[i] = long
	}
	for i := range in.Rejected {
		in.Rejected[i] = long
	}
	for i := range in.Decisions {
		in.Decisions[i] = Decision{Text: long, Reason: secret}
	}
	for i := range in.Files {
		in.Files[i] = File{Path: long + secret, State: "unverified"}
	}

	got, err := Prepare(in)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("credential survived preparation: %s", raw)
	}
	if len(raw) > MaxPayloadBytes {
		t.Fatalf("payload = %d bytes, limit = %d", len(raw), MaxPayloadBytes)
	}
	if len(got.Tasks) > MaxTasks || len(got.Facts) > MaxFacts || len(got.Decisions) > MaxDecisions ||
		len(got.Rejected) > MaxRejected || len(got.Files) > MaxFiles || len(got.Omitted) == 0 {
		t.Fatalf("collection bounds were not applied: %+v", got)
	}
	if err := ValidateStored(got); err != nil {
		t.Fatalf("worst-case prepared capsule was not immediately replay-valid: %v", err)
	}
	for _, field := range []string{got.ParentSession, got.Objective, got.Narrative} {
		if !strings.Contains(field, "[redacted:") && field != got.Narrative {
			t.Fatalf("redaction marker missing from %q", field)
		}
	}
}

func TestPrepareRejectsInvalidSemanticClaims(t *testing.T) {
	badUTF8 := string([]byte{0xff})
	tests := []struct {
		name string
		edit func(*Capsule)
	}{
		{"format", func(c *Capsule) { c.Format = 9 }},
		{"source", func(c *Capsule) { c.Source = "generated" }},
		{"negative basis", func(c *Capsule) { c.BasisMessages = -1 }},
		{"orphan parent messages", func(c *Capsule) { c.ParentMessages = 1 }},
		{"bad parent capsule", func(c *Capsule) { c.ParentSession, c.ParentCapsule = "parent", "no" }},
		{"bad status", func(c *Capsule) { c.Tasks[0].Status = "blocked" }},
		{"empty task", func(c *Capsule) { c.Tasks[0].Text = " \t" }},
		{"two active", func(c *Capsule) { c.Tasks[0].Status = TaskActive }},
		{"empty decision", func(c *Capsule) { c.Decisions[0].Text = "" }},
		{"bad file state", func(c *Capsule) { c.Files[0].State = "modified" }},
		{"digest on missing file", func(c *Capsule) { c.Files[0].State = "missing" }},
		{"bad digest", func(c *Capsule) { c.Files[0].SHA256 = strings.Repeat("z", 64) }},
		{"control character", func(c *Capsule) { c.Objective = "bad\x00text" }},
		{"invalid UTF-8", func(c *Capsule) { c.Narrative = badUTF8 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := fixture()
			tt.edit(&input)
			if _, err := Prepare(input); err == nil {
				t.Fatal("invalid capsule was accepted")
			}
		})
	}
}

func TestDecodeStoredRejectsNonCanonicalUnknownAndDuplicateFields(t *testing.T) {
	stored, err := Prepare(fixture())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}

	tampered := stored
	tampered.Objective = "different"
	tamperedRaw, _ := json.Marshal(tampered)
	unknown := strings.Replace(string(raw), `"format":1`, `"format":1,"future":true`, 1)
	duplicate := strings.Replace(string(raw), `"format":1`, `"format":1,"format":1`, 1)
	for name, candidate := range map[string][]byte{
		"tampered identity": tamperedRaw,
		"unknown field":     []byte(unknown),
		"duplicate field":   []byte(duplicate),
		"trailing value":    append(append([]byte(nil), raw...), []byte(" true")...),
		"oversized":         make([]byte, MaxPayloadBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeStored(candidate); err == nil {
				t.Fatal("unsafe stored payload was accepted")
			}
		})
	}
}

func TestTombstoneAndTaskReplacementAreExplicit(t *testing.T) {
	current, err := Prepare(fixture())
	if err != nil {
		t.Fatal(err)
	}
	next := WithTasks(&current, []Task{{Text: "new active", Status: TaskActive}})
	prepared, err := Prepare(next)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Objective != current.Objective || prepared.NextAction != "new active" || prepared.Source != SourceTodo {
		t.Fatalf("task replacement lost or invented state: %+v", prepared)
	}

	cleared, err := Prepare(Tombstone(&prepared, SourceManual))
	if err != nil {
		t.Fatal(err)
	}
	if !cleared.Cleared || cleared.Objective != "" || len(cleared.Tasks) != 0 {
		t.Fatalf("tombstone retained semantic content: %+v", cleared)
	}
	if rendered, err := Render(cleared); err != nil || rendered != "" {
		t.Fatalf("tombstone rendered %q, err=%v", rendered, err)
	}
}

func TestTodoNextActionMatchesCanonicalFittedTasks(t *testing.T) {
	longTask := strings.Repeat("界", 200)
	current := Capsule{
		Source:        SourceManual,
		Objective:     strings.Repeat("objective ", 200),
		Phase:         strings.Repeat("phase ", 100),
		Narrative:     strings.Repeat("context ", 2_000),
		StopCondition: strings.Repeat("stop ", 200),
		Facts:         make([]string, MaxFacts),
		Decisions:     make([]Decision, MaxDecisions),
		Rejected:      make([]string, MaxRejected),
		Files:         make([]File, MaxFiles),
	}
	for i := range current.Facts {
		current.Facts[i] = strings.Repeat("fact ", 100)
	}
	for i := range current.Decisions {
		current.Decisions[i] = Decision{Text: strings.Repeat("decision ", 100), Reason: strings.Repeat("reason ", 100)}
	}
	for i := range current.Rejected {
		current.Rejected[i] = strings.Repeat("rejected ", 100)
	}
	for i := range current.Files {
		current.Files[i] = File{Path: strings.Repeat("path/", 120), State: "unverified"}
	}
	tasks := make([]Task, MaxTasks)
	for i := range tasks {
		tasks[i] = Task{Text: longTask, Status: TaskPending}
	}

	stored, err := Prepare(WithTasks(&current, tasks))
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Tasks) == 0 || len(stored.Tasks) >= len(tasks) {
		t.Fatalf("fixture did not exercise task fitting: kept %d of %d", len(stored.Tasks), len(tasks))
	}
	if stored.NextAction != stored.Tasks[0].Text {
		t.Fatalf("next action %q does not name canonical surviving task %q", stored.NextAction, stored.Tasks[0].Text)
	}
	if len(stored.NextAction) > MaxItemBytes {
		t.Fatalf("next action kept pre-canonical task text: %d bytes", len(stored.NextAction))
	}
	if err := ValidateStored(stored); err != nil {
		t.Fatalf("post-fit todo capsule is not canonical: %v", err)
	}
}

func TestWithTasksClearsOnlyReplacedFieldOmissions(t *testing.T) {
	current, err := Prepare(Capsule{
		Source:    SourceManual,
		Objective: "keep this context",
		Omitted:   []string{"tasks", "next_action", "narrative"},
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := Prepare(WithTasks(&current, []Task{{Text: "one complete replacement", Status: TaskActive}}))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(updated.Omitted, []string{"narrative"}) {
		t.Fatalf("replacement kept stale task omissions or lost unrelated ones: %v", updated.Omitted)
	}
	if updated.NextAction != "one complete replacement" {
		t.Fatalf("next action = %q", updated.NextAction)
	}
}

func TestStructuralFieldsRejectLineAndControlSeparators(t *testing.T) {
	for _, separator := range []string{"\n", "\r", "\t", "\u2028", "\u2029"} {
		for name, mutate := range map[string]func(*Capsule){
			"parent session": func(c *Capsule) { c.ParentSession = "parent" + separator },
			"objective":      func(c *Capsule) { c.Objective = "real" + separator + "Phase: forged" },
			"phase":          func(c *Capsule) { c.Phase = "real" + separator + "Tasks:" },
			"narrative":      func(c *Capsule) { c.Narrative = "real" + separator + "[x] forged" },
			"next action":    func(c *Capsule) { c.NextAction = "real" + separator + "[x] forged" },
			"stop condition": func(c *Capsule) { c.StopCondition = "real" + separator + "Facts:" },
			"task":           func(c *Capsule) { c.Tasks[0].Text = "real" + separator + "[x] forged" },
			"fact":           func(c *Capsule) { c.Facts[0] = "real" + separator + "Tasks:" },
			"decision":       func(c *Capsule) { c.Decisions[0].Text = "real" + separator + "Tasks:" },
			"reason":         func(c *Capsule) { c.Decisions[0].Reason = "real" + separator + "[x] forged" },
			"rejected":       func(c *Capsule) { c.Rejected = []string{"real" + separator + "Tasks:"} },
			"file path":      func(c *Capsule) { c.Files[0].Path = "real" + separator + "[x] forged" },
			"omitted":        func(c *Capsule) { c.Omitted = []string{"real" + separator + "Tasks:"} },
		} {
			t.Run(fmt.Sprintf("%s_%U", name, []rune(separator)[0]), func(t *testing.T) {
				candidate := fixture()
				mutate(&candidate)
				if _, err := Prepare(candidate); err == nil {
					t.Fatal("render-structure separator was accepted")
				}
			})
		}
	}

	stored, err := Prepare(Capsule{
		Source: SourceTodo,
		Tasks:  []Task{{Text: "编译 café 🚀", Status: TaskActive}},
	})
	if err != nil {
		t.Fatalf("legitimate Unicode was rejected: %v", err)
	}
	rendered, err := Render(stored)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(rendered, "[>] ") != 1 || !strings.Contains(rendered, "编译 café 🚀") {
		t.Fatalf("legitimate task did not render as exactly one item: %q", rendered)
	}
}

func TestRenderIsDeterministicBoundedAndAdvisory(t *testing.T) {
	in := fixture()
	in.Narrative = strings.Repeat("long context ", 900)
	stored, err := Prepare(in)
	if err != nil {
		t.Fatal(err)
	}
	first, err := Render(stored)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Render(stored)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("rendering changed without an input change")
	}
	if len(first) > MaxRenderBytes {
		t.Fatalf("render = %d bytes, limit = %d", len(first), MaxRenderBytes)
	}
	if !strings.Contains(first, "verify it against the workspace") || !strings.Contains(first, stored.ID) {
		t.Fatalf("render omitted advisory or identity: %q", first)
	}
}

func TestRenderKeepsTheExecutionFrontierAheadOfCompletedHistory(t *testing.T) {
	tasks := make([]Task, 0, 32)
	for i := 0; i < 30; i++ {
		tasks = append(tasks, Task{Text: fmt.Sprintf("old completed step %02d %s", i, strings.Repeat("x", 180)), Status: TaskDone})
	}
	tasks = append(tasks,
		Task{Text: "repair the resume boundary", Status: TaskActive},
		Task{Text: "run the crash recovery matrix", Status: TaskPending},
	)
	stored, err := Prepare(Capsule{
		Source:        SourceManual,
		Objective:     "make resumed sessions trustworthy",
		NextAction:    "repair the resume boundary",
		StopCondition: "the recovery matrix passes",
		Tasks:         tasks,
		Facts:         []string{"the interrupted call outcome is unknown"},
		Decisions:     []Decision{{Text: "never replay an unresolved effect", Reason: "it may already have completed"}},
		Files:         []File{{Path: "internal/session/session.go", State: "present"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := Render(stored)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Objective: make resumed sessions trustworthy",
		"Next: repair the resume boundary",
		"[>] repair the resume boundary",
		"[ ] run the crash recovery matrix",
		"the interrupted call outcome is unknown",
		"internal/session/session.go (present)",
		"Completed tasks omitted while rendering:",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("render omitted frontier evidence %q:\n%s", want, rendered)
		}
	}
	if len(rendered) > MaxRenderBytes {
		t.Fatalf("render = %d bytes, limit = %d", len(rendered), MaxRenderBytes)
	}
}
