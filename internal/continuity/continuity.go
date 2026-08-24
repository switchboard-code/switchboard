// Package continuity defines the bounded task state that survives a context
// boundary. A capsule is advisory: it tells a continuing model where the last
// context believed it was, but it never relaxes read-before-write, permission,
// verification, or routing checks.
package continuity

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/switchboard-code/switchboard/internal/credential"
)

const (
	FormatVersion = 1

	MaxPayloadBytes = 16 << 10
	MaxRenderBytes  = 4 << 10

	MaxObjectiveBytes = 1024
	MaxNarrativeBytes = 8192
	MaxShortBytes     = 512
	MaxItemBytes      = 256
	MaxPathBytes      = 512
	MaxSessionIDBytes = 128

	MaxTasks     = 50
	MaxFacts     = 24
	MaxDecisions = 16
	MaxRejected  = 16
	MaxFiles     = 32
	MaxOmitted   = 8
)

type Source string

const (
	SourceTodo    Source = "todo"
	SourceCompact Source = "compact"
	SourceManual  Source = "manual"
)

type TaskStatus string

const (
	TaskPending TaskStatus = "pending"
	TaskActive  TaskStatus = "active"
	TaskDone    TaskStatus = "done"
)

// Capsule is the latest recorded working state. BasisMessages binds it to a
// precise point in the append-only conversation; the session package verifies
// that boundary before accepting the record.
type Capsule struct {
	Format int    `json:"format"`
	ID     string `json:"id"`
	Source Source `json:"source"`

	BasisMessages int `json:"basis_messages"`

	ParentSession  string `json:"parent_session,omitempty"`
	ParentMessages int    `json:"parent_messages,omitempty"`
	ParentCapsule  string `json:"parent_capsule,omitempty"`

	Objective     string `json:"objective,omitempty"`
	Phase         string `json:"phase,omitempty"`
	Narrative     string `json:"narrative,omitempty"`
	NextAction    string `json:"next_action,omitempty"`
	StopCondition string `json:"stop_condition,omitempty"`

	Tasks     []Task     `json:"tasks,omitempty"`
	Facts     []string   `json:"facts,omitempty"`
	Decisions []Decision `json:"decisions,omitempty"`
	Rejected  []string   `json:"rejected,omitempty"`
	Files     []File     `json:"files,omitempty"`
	Omitted   []string   `json:"omitted,omitempty"`

	// Cleared is a tombstone. It prevents an older capsule from becoming the
	// latest state again without deleting the history that produced it.
	Cleared bool `json:"cleared,omitempty"`
}

type Task struct {
	Text   string     `json:"text"`
	Status TaskStatus `json:"status"`
}

type Decision struct {
	Text   string `json:"text"`
	Reason string `json:"reason,omitempty"`
}

type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256,omitempty"`
	State  string `json:"state"`
}

// Clone returns a deep copy suitable for handing across a package boundary.
func Clone(in Capsule) Capsule {
	out := in
	out.Tasks = append([]Task(nil), in.Tasks...)
	out.Facts = append([]string(nil), in.Facts...)
	out.Decisions = append([]Decision(nil), in.Decisions...)
	out.Rejected = append([]string(nil), in.Rejected...)
	out.Files = append([]File(nil), in.Files...)
	out.Omitted = append([]string(nil), in.Omitted...)
	return out
}

// Prepare canonicalizes, redacts, bounds, and identities a capsule. It is the
// only form the session WAL accepts. Invalid status or provenance is refused;
// excess descriptive content is deterministically omitted and named.
func Prepare(in Capsule) (Capsule, error) {
	c := Clone(in)
	if c.Format == 0 {
		c.Format = FormatVersion
	}
	if c.Format != FormatVersion {
		return Capsule{}, fmt.Errorf("continuity format %d is not supported", c.Format)
	}
	if !validSource(c.Source) {
		return Capsule{}, fmt.Errorf("continuity source %q is not supported", c.Source)
	}
	if c.BasisMessages < 0 || c.ParentMessages < 0 {
		return Capsule{}, fmt.Errorf("continuity message boundaries cannot be negative")
	}
	var err error
	c.ParentSession, err = canonicalText(c.ParentSession, MaxSessionIDBytes, "parent_session", nil)
	if err != nil {
		return Capsule{}, err
	}
	if c.ParentSession == "" && (c.ParentMessages != 0 || c.ParentCapsule != "") {
		return Capsule{}, fmt.Errorf("continuity parent details require a parent session")
	}
	if c.ParentCapsule != "" && !ValidID(c.ParentCapsule) {
		return Capsule{}, fmt.Errorf("continuity parent capsule id is invalid")
	}

	c.ID = ""
	if c.Cleared {
		c.Objective, c.Phase, c.Narrative, c.NextAction, c.StopCondition = "", "", "", "", ""
		c.Tasks, c.Facts, c.Decisions, c.Rejected, c.Files, c.Omitted = nil, nil, nil, nil, nil, nil
	} else if err := canonicalizeContent(&c); err != nil {
		return Capsule{}, err
	}
	if c.Source == SourceTodo && !c.Cleared {
		reconcileTodoNextAction(&c)
	}

	if err := fitPayload(&c); err != nil {
		return Capsule{}, err
	}
	if c.Source == SourceTodo && !c.Cleared {
		// Fitting may remove pending tasks. Derive the pointer again from the
		// exact surviving list so continuity never names work it omitted.
		reconcileTodoNextAction(&c)
	}
	normalizeSlices(&c)
	c.ID = capsuleID(c)
	raw, err := json.Marshal(c)
	if err != nil {
		return Capsule{}, err
	}
	if len(raw) > MaxPayloadBytes {
		return Capsule{}, fmt.Errorf("continuity payload is %d bytes; limit is %d", len(raw), MaxPayloadBytes)
	}
	return c, nil
}

// ValidateStored verifies that persisted bytes are already in the exact safe,
// canonical form Prepare would have written. Replay fails closed on a payload
// whose identity, bounds, redaction, or normalization changed.
func ValidateStored(c Capsule) error {
	prepared, err := Prepare(c)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(c, prepared) {
		return fmt.Errorf("continuity payload is not canonical or its id does not match")
	}
	return nil
}

// DecodeStored decodes the exact WAL representation. It rejects oversized
// values, unknown fields, duplicate object keys at any depth, and trailing
// JSON before applying the canonical-content and identity checks.
func DecodeStored(raw []byte) (Capsule, error) {
	if len(raw) > MaxPayloadBytes {
		return Capsule{}, fmt.Errorf("continuity payload is %d bytes; limit is %d", len(raw), MaxPayloadBytes)
	}
	if err := rejectDuplicateKeys(raw); err != nil {
		return Capsule{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var c Capsule
	if err := decoder.Decode(&c); err != nil {
		return Capsule{}, fmt.Errorf("decode continuity: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Capsule{}, errors.New("decode continuity: unexpected trailing JSON value")
		}
		return Capsule{}, fmt.Errorf("decode continuity: %w", err)
	}
	if err := ValidateStored(c); err != nil {
		return Capsule{}, err
	}
	return Clone(c), nil
}

// WithTasks produces the next capsule after a successful todo tool batch.
// Semantic fields from the latest capsule survive; the task list and immediate
// next action are the only claims this operation has evidence to replace.
func WithTasks(current *Capsule, tasks []Task) Capsule {
	return WithWorking(current, tasks, Working{})
}

// Working is what the model says about the job beyond its task list: what it
// is trying to achieve, what it would do next, and what would mean it is
// finished.
//
// These fields have been specified, validated, redacted, bounded, and rendered
// since the capsule existed, and nothing ever wrote them, so a task list
// crossed a compaction while the reason for it did not. A model that resumes
// with five checkboxes and no objective has the shape of the work and none of
// its point.
//
// An empty field leaves what was there. The model updates its list far more
// often than its objective changes, and a capsule that forgot the objective on
// every todo call would be worse than one that never had it.
type Working struct {
	Objective     string
	NextAction    string
	StopCondition string
}

// WithWorking folds a task list and whatever the model said about the job into
// the capsule that carries them across a boundary.
func WithWorking(current *Capsule, tasks []Task, working Working) Capsule {
	var out Capsule
	if current != nil && !current.Cleared {
		out = Clone(*current)
	}
	out.Format = FormatVersion
	out.ID = ""
	out.Source = SourceTodo
	out.Cleared = false
	out.Tasks = append([]Task(nil), tasks...)

	// NextAction is cleared rather than kept because it is the one field a
	// stale value actively misleads about: it names the very next step, and
	// the call that changed the list is the moment it stopped being true.
	out.NextAction = working.NextAction
	if working.Objective != "" {
		out.Objective = working.Objective
	}
	if working.StopCondition != "" {
		out.StopCondition = working.StopCondition
	}

	dropped := map[string]bool{"tasks": true, "next_action": true}
	if working.Objective != "" {
		dropped["objective"] = true
	}
	if working.StopCondition != "" {
		dropped["stop_condition"] = true
	}
	keptOmissions := out.Omitted[:0]
	for _, omission := range out.Omitted {
		if !dropped[omission] {
			keptOmissions = append(keptOmissions, omission)
		}
	}
	out.Omitted = keptOmissions
	return out
}

// PrepareTasks applies the exact text, status, redaction, UTF-8, control-byte,
// and size rules a todo-derived capsule will apply. The todo tool uses this
// before it changes live state or emits a successful result, so the state it
// reports can always be persisted without a second semantic conversion.
func PrepareTasks(tasks []Task) ([]Task, error) {
	prepared, err := Prepare(Capsule{Source: SourceTodo, Tasks: tasks})
	if err != nil {
		return nil, err
	}
	if len(prepared.Tasks) != len(tasks) {
		return nil, fmt.Errorf("continuity task list cannot fit without dropping items")
	}
	return append([]Task(nil), prepared.Tasks...), nil
}

// Tombstone constructs an explicit cleared state while retaining lineage.
func Tombstone(current *Capsule, source Source) Capsule {
	out := Capsule{Format: FormatVersion, Source: source, Cleared: true}
	if current != nil {
		out.ParentSession = current.ParentSession
		out.ParentMessages = current.ParentMessages
		out.ParentCapsule = current.ParentCapsule
	}
	return out
}

// Render returns the bounded text placed beside a future user opening.
func Render(c Capsule) (string, error) {
	if err := ValidateStored(c); err != nil {
		return "", err
	}
	if c.Cleared {
		return "", nil
	}
	// Rendering has a smaller budget than storage. Spend it semantically:
	// identity and the execution frontier are mandatory; active and pending
	// work outrank old completed work; verification evidence and relevant files
	// outrank narrative. Appending whole lines avoids chopping a task or path
	// into a misleading fragment.
	const footerReserve = 320
	var b strings.Builder
	fmt.Fprintf(&b, "[continuity %s]\nLast recorded working state; verify it against the workspace before writing.\n", c.ID)
	writeField := func(label, value string) {
		if value != "" {
			fmt.Fprintf(&b, "%s: %s\n", label, value)
		}
	}
	writeField("Objective", c.Objective)
	writeField("Phase", c.Phase)
	writeField("Next", c.NextAction)
	writeField("Stop when", c.StopCondition)

	omitted := map[string]int{}
	appendSection := func(name string, lines []string) {
		if len(lines) == 0 {
			return
		}
		header := name + ":\n"
		wroteHeader := false
		for i, line := range lines {
			need := len(line) + 1
			if !wroteHeader {
				need += len(header)
			}
			if b.Len()+need > MaxRenderBytes-footerReserve {
				omitted[name] += len(lines) - i
				return
			}
			if !wroteHeader {
				b.WriteString(header)
				wroteHeader = true
			}
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}

	var active, pending, done []string
	for _, task := range c.Tasks {
		switch task.Status {
		case TaskActive:
			active = append(active, "[>] "+task.Text)
		case TaskPending:
			pending = append(pending, "[ ] "+task.Text)
		case TaskDone:
			done = append(done, "[x] "+task.Text)
		}
	}
	appendSection("Active task", active)
	appendSection("Pending tasks", pending)

	facts := make([]string, len(c.Facts))
	for i, fact := range c.Facts {
		facts[i] = "- " + fact
	}
	appendSection("Facts", facts)

	decisions := make([]string, len(c.Decisions))
	for i, decision := range c.Decisions {
		line := decision.Text
		if decision.Reason != "" {
			line += " — " + decision.Reason
		}
		decisions[i] = "- " + line
	}
	appendSection("Decisions", decisions)

	files := make([]string, len(c.Files))
	for i, file := range c.Files {
		files[i] = fmt.Sprintf("- %s (%s)", file.Path, file.State)
	}
	appendSection("Recorded files", files)

	rejected := make([]string, len(c.Rejected))
	for i, path := range c.Rejected {
		rejected[i] = "- " + path
	}
	appendSection("Rejected paths", rejected)

	// A bounded tail of completed tasks prevents immediate repetition without
	// letting a long chronology hide work that is still live.
	const completedTail = 3
	if len(done) > completedTail {
		omitted["Completed tasks"] += len(done) - completedTail
		done = done[len(done)-completedTail:]
	}
	appendSection("Recently completed", done)

	if c.Narrative != "" {
		line := "Context: " + c.Narrative + "\n"
		if b.Len()+len(line) <= MaxRenderBytes-footerReserve {
			b.WriteString(line)
		} else {
			omitted["Context"]++
		}
	}

	var footer []string
	if len(c.Omitted) > 0 {
		footer = append(footer, "Omitted while storing: "+strings.Join(c.Omitted, ", "))
	}
	for _, name := range []string{"Active task", "Pending tasks", "Facts", "Decisions", "Recorded files", "Rejected paths", "Completed tasks", "Recently completed", "Context"} {
		if count := omitted[name]; count > 0 {
			footer = append(footer, fmt.Sprintf("%s omitted while rendering: %d", name, count))
		}
	}
	if len(footer) > 0 {
		text := strings.Join(footer, "\n")
		available := MaxRenderBytes - b.Len()
		if available > 0 {
			text = truncateUTF8(text, available)
			b.WriteString(text)
			b.WriteByte('\n')
		}
	}

	return strings.TrimSpace(b.String()), nil
}

func canonicalizeContent(c *Capsule) error {
	var err error
	c.Objective, err = canonicalText(c.Objective, MaxObjectiveBytes, "objective", &c.Omitted)
	if err != nil {
		return err
	}
	c.Phase, err = canonicalText(c.Phase, MaxShortBytes, "phase", &c.Omitted)
	if err != nil {
		return err
	}
	c.Narrative, err = canonicalText(c.Narrative, MaxNarrativeBytes, "narrative", &c.Omitted)
	if err != nil {
		return err
	}
	c.NextAction, err = canonicalText(c.NextAction, MaxShortBytes, "next_action", &c.Omitted)
	if err != nil {
		return err
	}
	c.StopCondition, err = canonicalText(c.StopCondition, MaxShortBytes, "stop_condition", &c.Omitted)
	if err != nil {
		return err
	}

	active := 0
	for i := range c.Tasks {
		switch c.Tasks[i].Status {
		case TaskPending, TaskActive, TaskDone:
		default:
			return fmt.Errorf("continuity task %d has invalid status %q", i+1, c.Tasks[i].Status)
		}
		if c.Tasks[i].Status == TaskActive {
			active++
		}
		c.Tasks[i].Text, err = canonicalText(c.Tasks[i].Text, MaxItemBytes, "tasks", &c.Omitted)
		if err != nil {
			return err
		}
		if c.Tasks[i].Text == "" {
			return fmt.Errorf("continuity task %d has no text", i+1)
		}
	}
	if active > 1 {
		return fmt.Errorf("continuity has %d active tasks; at most one is allowed", active)
	}
	if len(c.Tasks) > MaxTasks {
		c.Tasks = c.Tasks[:MaxTasks]
		addOmitted(&c.Omitted, "tasks")
	}

	if c.Facts, err = canonicalStrings(c.Facts, MaxFacts, "facts", &c.Omitted); err != nil {
		return err
	}
	if c.Rejected, err = canonicalStrings(c.Rejected, MaxRejected, "rejected", &c.Omitted); err != nil {
		return err
	}
	for i := range c.Decisions {
		c.Decisions[i].Text, err = canonicalText(c.Decisions[i].Text, MaxItemBytes, "decisions", &c.Omitted)
		if err != nil {
			return err
		}
		c.Decisions[i].Reason, err = canonicalText(c.Decisions[i].Reason, MaxItemBytes, "decisions", &c.Omitted)
		if err != nil {
			return err
		}
		if c.Decisions[i].Text == "" {
			return fmt.Errorf("continuity decision %d has no text", i+1)
		}
	}
	if len(c.Decisions) > MaxDecisions {
		c.Decisions = c.Decisions[:MaxDecisions]
		addOmitted(&c.Omitted, "decisions")
	}

	for i := range c.Files {
		c.Files[i].Path, err = canonicalText(c.Files[i].Path, MaxPathBytes, "files", &c.Omitted)
		if err != nil {
			return err
		}
		if c.Files[i].Path == "" {
			return fmt.Errorf("continuity file %d has no path", i+1)
		}
		switch c.Files[i].State {
		case "present", "missing", "unverified":
		default:
			return fmt.Errorf("continuity file %d has invalid state %q", i+1, c.Files[i].State)
		}
		if c.Files[i].SHA256 != "" {
			if c.Files[i].State != "present" || len(c.Files[i].SHA256) != 64 {
				return fmt.Errorf("continuity file %d has invalid sha256", i+1)
			}
			if _, err := hex.DecodeString(c.Files[i].SHA256); err != nil || strings.ToLower(c.Files[i].SHA256) != c.Files[i].SHA256 {
				return fmt.Errorf("continuity file %d has invalid sha256", i+1)
			}
		}
	}
	if len(c.Files) > MaxFiles {
		c.Files = c.Files[:MaxFiles]
		addOmitted(&c.Omitted, "files")
	}

	inputOmitted := append([]string(nil), c.Omitted...)
	c.Omitted = nil
	for _, item := range inputOmitted {
		item, err = canonicalText(item, 64, "omitted", nil)
		if err != nil {
			return err
		}
		if item != "" {
			addOmitted(&c.Omitted, item)
		}
	}
	if len(c.Omitted) > MaxOmitted {
		c.Omitted = c.Omitted[:MaxOmitted]
	}
	normalizeSlices(c)
	return nil
}

// JSON omits both nil and zero-length slices. Normalize them to nil both
// before and after fitPayload, which can remove a collection's final item.
// This keeps a freshly prepared value identical to the value decoded from its
// bytes during strict replay validation.
func normalizeSlices(c *Capsule) {
	if len(c.Tasks) == 0 {
		c.Tasks = nil
	}
	if len(c.Facts) == 0 {
		c.Facts = nil
	}
	if len(c.Decisions) == 0 {
		c.Decisions = nil
	}
	if len(c.Rejected) == 0 {
		c.Rejected = nil
	}
	if len(c.Files) == 0 {
		c.Files = nil
	}
	if len(c.Omitted) == 0 {
		c.Omitted = nil
	}
}

func canonicalStrings(values []string, limit int, label string, omitted *[]string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make([]string, 0, min(len(values), limit))
	for _, value := range values {
		clean, err := canonicalText(value, MaxItemBytes, label, omitted)
		if err != nil {
			return nil, err
		}
		if clean == "" {
			return nil, fmt.Errorf("continuity %s contains an empty item", label)
		}
		if len(out) < limit {
			out = append(out, clean)
		} else {
			addOmitted(omitted, label)
		}
	}
	return out, nil
}

func canonicalText(value string, limit int, label string, omitted *[]string) (string, error) {
	if !utf8.ValidString(value) {
		return "", fmt.Errorf("continuity %s is not valid UTF-8", label)
	}
	for _, r := range value {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return "", fmt.Errorf("continuity %s contains a control character or line separator", label)
		}
	}
	value = strings.TrimSpace(value)
	if leaks := credential.ScanPrompt(value); len(leaks) > 0 {
		value = credential.Redact(value, leaks)
	}
	if len(value) > limit {
		value = strings.TrimSpace(truncateUTF8(value, limit))
		addOmitted(omitted, label)
	}
	return value, nil
}

func fitPayload(c *Capsule) error {
	for {
		c.ID = strings.Repeat("0", 32)
		raw, err := json.Marshal(c)
		c.ID = ""
		if err != nil {
			return err
		}
		if len(raw) <= MaxPayloadBytes {
			return nil
		}
		switch {
		case len(c.Files) > 0:
			c.Files = c.Files[:len(c.Files)-1]
			addOmitted(&c.Omitted, "files")
		case hasTaskStatus(c.Tasks, TaskDone):
			c.Tasks = removeLastTaskStatus(c.Tasks, TaskDone)
			addOmitted(&c.Omitted, "tasks")
		case len(c.Narrative) > 256:
			c.Narrative = strings.TrimSpace(truncateUTF8(c.Narrative, len(c.Narrative)-256))
			addOmitted(&c.Omitted, "narrative")
		case len(c.Facts) > 0:
			c.Facts = c.Facts[:len(c.Facts)-1]
			addOmitted(&c.Omitted, "facts")
		case len(c.Rejected) > 0:
			c.Rejected = c.Rejected[:len(c.Rejected)-1]
			addOmitted(&c.Omitted, "rejected")
		case len(c.Decisions) > 0:
			c.Decisions = c.Decisions[:len(c.Decisions)-1]
			addOmitted(&c.Omitted, "decisions")
		case hasTaskStatus(c.Tasks, TaskPending):
			c.Tasks = removeLastTaskStatus(c.Tasks, TaskPending)
			addOmitted(&c.Omitted, "tasks")
		case len(c.StopCondition) > 128:
			c.StopCondition = strings.TrimSpace(truncateUTF8(c.StopCondition, len(c.StopCondition)-128))
			addOmitted(&c.Omitted, "stop_condition")
		case len(c.Phase) > 128:
			c.Phase = strings.TrimSpace(truncateUTF8(c.Phase, len(c.Phase)-128))
			addOmitted(&c.Omitted, "phase")
		case len(c.Objective) > 128:
			c.Objective = strings.TrimSpace(truncateUTF8(c.Objective, len(c.Objective)-128))
			addOmitted(&c.Omitted, "objective")
		default:
			return fmt.Errorf("continuity payload cannot fit within %d bytes", MaxPayloadBytes)
		}
	}
}

func capsuleID(c Capsule) string {
	c.ID = ""
	raw, _ := json.Marshal(c)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}

func ValidID(id string) bool {
	if len(id) != 32 || strings.ToLower(id) != id {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func validSource(source Source) bool {
	switch source {
	case SourceTodo, SourceCompact, SourceManual:
		return true
	default:
		return false
	}
}

func addOmitted(omitted *[]string, item string) {
	if omitted == nil || item == "" {
		return
	}
	for _, have := range *omitted {
		if have == item {
			return
		}
	}
	if len(*omitted) < MaxOmitted {
		*omitted = append(*omitted, item)
	}
}

func truncateUTF8(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	if maxBytes <= 0 {
		return ""
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func hasTaskStatus(tasks []Task, status TaskStatus) bool {
	for _, task := range tasks {
		if task.Status == status {
			return true
		}
	}
	return false
}

func removeLastTaskStatus(tasks []Task, status TaskStatus) []Task {
	for i := len(tasks) - 1; i >= 0; i-- {
		if tasks[i].Status == status {
			return append(tasks[:i], tasks[i+1:]...)
		}
	}
	return tasks
}

func reconcileTodoNextAction(c *Capsule) {
	c.NextAction = ""
	for _, task := range c.Tasks {
		if task.Status == TaskActive {
			c.NextAction = task.Text
			return
		}
	}
	for _, task := range c.Tasks {
		if task.Status == TaskPending {
			c.NextAction = task.Text
			return
		}
	}
}

func rejectDuplicateKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var consume func(int) error
	consume = func(depth int) error {
		if depth > 16 {
			return errors.New("decode continuity: JSON nesting exceeds limit")
		}
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("decode continuity: %w", err)
		}
		delim, composite := token.(json.Delim)
		if !composite {
			return nil
		}
		switch delim {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return fmt.Errorf("decode continuity: %w", err)
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("decode continuity: object key is not a string")
				}
				if _, duplicate := seen[key]; duplicate {
					return fmt.Errorf("decode continuity: duplicate object key %q", key)
				}
				seen[key] = struct{}{}
				if err := consume(depth + 1); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim('}') {
				return errors.New("decode continuity: invalid object")
			}
		case '[':
			for decoder.More() {
				if err := consume(depth + 1); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim(']') {
				return errors.New("decode continuity: invalid array")
			}
		default:
			return fmt.Errorf("decode continuity: unexpected delimiter %q", delim)
		}
		return nil
	}
	if err := consume(0); err != nil {
		return err
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) || token != nil {
		return errors.New("decode continuity: unexpected trailing JSON")
	}
	return nil
}
