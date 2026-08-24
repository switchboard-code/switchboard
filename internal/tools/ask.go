package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/permission"
)

// Question is one ask call: a question and the answers it offers. The user
// can always answer outside the options, so the list is coverage of the
// likely answers, never a claim of completeness.
type Question struct {
	Question string
	Options  []QuestionOption
	Multi    bool
}

type QuestionOption struct {
	Label  string
	Detail string
}

// Answer is what the user did with a Question. Picked holds chosen labels in
// the order they were offered; Text carries an answer typed past the options.
// Declined is an answer, not an error: the user waving a question away is
// something the model must hear and work around, so it travels as a result
// rather than aborting the call.
type Answer struct {
	Picked   []string
	Text     string
	Declined bool
}

// Questioner is a surface's side of the ask tool: the TUI resolves it
// against a dialog, the interactive REPL inline. It is set at assembly and
// only by surfaces that have a user attached — headless runs, delegate
// subagents, and race branches leave it unset, and the tool answers with
// the refusal instead, because a question with no one listening must fail
// closed rather than hang or invent an answer.
type Questioner interface {
	AskUser(ctx context.Context, q Question) (Answer, error)
}

// SetQuestioner wires a surface's question channel in at assembly time.
func (r *Registry) SetQuestioner(q Questioner) { r.questioner = q }

const (
	minAskOptions = 2
	maxAskOptions = 8
)

type askTool struct{ r *Registry }

func (t *askTool) Name() string { return "ask" }

func (t *askTool) Description() string {
	return "Ask the user one question and wait for the answer. Use it only when the work " +
		"genuinely forks and guessing wrong would waste the turn: which of two designs, " +
		"which behavior is intended, whether to widen scope. Offer two to eight concrete " +
		"options; the user picks one (several when multi is set), types an answer of " +
		"their own, or declines. Never ask what the workspace or the conversation " +
		"already answers. When the tool reports no one is listening, decide yourself " +
		"and state the assumption instead of asking again."
}

// ParallelSafe is false because every question resolves against the same
// user. Interactive surfaces serialize it with their other modals; the tool
// itself never creates a form by asking several questions concurrently.
func (t *askTool) ParallelSafe() bool { return false }

func (t *askTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "question": {"type": "string", "description": "The question, complete and answerable on its own."},
    "options": {
      "type": "array",
      "description": "The answers to offer, 2 to 8. The user can always type their own instead, so cover the likely answers rather than every answer.",
      "items": {
        "type": "object",
        "properties": {
          "label": {"type": "string", "description": "The answer, short enough to read at a glance."},
          "detail": {"type": "string", "description": "What choosing it means, when the label alone is not enough."}
        },
        "required": ["label"]
      }
    },
    "multi": {"type": "boolean", "description": "Allow choosing more than one answer."}
  },
  "required": ["question", "options"]
}`)
}

type askInput struct {
	Question string `json:"question"`
	Options  []struct {
		Label  string `json:"label"`
		Detail string `json:"detail"`
	} `json:"options"`
	Multi bool `json:"multi"`
}

func (t *askTool) Plan(input json.RawMessage) (Plan, error) {
	var in askInput
	if err := json.Unmarshal(input, &in); err != nil {
		return Plan{}, fmt.Errorf("ask: %w", err)
	}
	if strings.TrimSpace(in.Question) == "" {
		return Plan{}, fmt.Errorf("ask: the question is empty")
	}
	if len(in.Options) < minAskOptions {
		return Plan{}, fmt.Errorf("ask: %d option(s); a question with one answer is a statement. Offer at least %d", len(in.Options), minAskOptions)
	}
	if len(in.Options) > maxAskOptions {
		return Plan{}, fmt.Errorf("ask: %d options reads as a form, not a question. Offer the likely %d; the user can always type their own", len(in.Options), maxAskOptions)
	}
	q := Question{Question: strings.TrimSpace(in.Question), Multi: in.Multi}
	seen := map[string]int{}
	for i, opt := range in.Options {
		label := strings.TrimSpace(opt.Label)
		if label == "" {
			return Plan{}, fmt.Errorf("ask: option %d has no label", i+1)
		}
		if prev, dup := seen[label]; dup {
			return Plan{}, fmt.Errorf("ask: options %d and %d are both %q", prev+1, i+1, label)
		}
		seen[label] = i
		q.Options = append(q.Options, QuestionOption{Label: label, Detail: strings.TrimSpace(opt.Detail)})
	}

	// A question is session interaction, not an effect on the world: the
	// answer channel is the user, who can refuse in person. It carries the
	// read effect for the same reason todo does — allowed in every mode,
	// plan included, because planning is exactly when a question earns its
	// place. What keeps a branch or a scripted run from asking is the
	// absent questioner, not the permission engine.
	return Plan{
		Request: permission.Request{Tool: t.Name(), Effect: permission.EffectRead, Detail: q.Question},
		Run: func(ctx context.Context) (Result, error) {
			questioner := t.r.questioner
			if questioner == nil {
				return errorf("no one is listening: this run has no interactive user to ask. Decide yourself and state the assumption")
			}
			ans, err := questioner.AskUser(ctx, q)
			if err != nil {
				return Result{}, err
			}
			return Result{Content: renderAnswer(ans)}, nil
		},
	}, nil
}

// renderAnswer is the model-facing rendering of what the user did. A typed
// answer redacts unconditionally — the injected-report posture — because
// the question dialog is not the secret gate and must not grow into one:
// this result is recorded and sent, and it must not carry what the gate
// would have held back.
func renderAnswer(ans Answer) string {
	switch {
	case ans.Declined:
		return "The user declined to answer. Continue on your own judgment and say what you assumed."
	case ans.Text != "":
		text := ans.Text
		if leaks := credential.ScanPrompt(text); len(leaks) > 0 {
			text = credential.Redact(text, leaks)
		}
		return "The user answered in their own words: " + text
	case len(ans.Picked) > 0:
		return "The user chose: " + strings.Join(ans.Picked, ", ")
	default:
		return "The user declined to answer. Continue on your own judgment and say what you assumed."
	}
}
