package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/tools"
)

// elicitTransport records what the client sent so a test can read the reply a
// server would have received.
type elicitTransport struct {
	incoming  chan []byte
	sent      chan []byte
	closed    chan struct{}
	closeOnce sync.Once
}

func newElicitTransport() *elicitTransport {
	return &elicitTransport{
		incoming: make(chan []byte, 8),
		sent:     make(chan []byte, 8),
		closed:   make(chan struct{}),
	}
}

// Send honors the context because a real transport does, and because the
// deadline on the reply is exactly what this file has to be able to catch.
func (t *elicitTransport) Send(ctx context.Context, msg []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t.sent <- append([]byte(nil), msg...)
	return nil
}

func (t *elicitTransport) Recv() ([]byte, error) {
	select {
	case msg := <-t.incoming:
		return msg, nil
	case <-t.closed:
		return nil, errors.New("closed")
	}
}

func (t *elicitTransport) Close() error {
	t.closeOnce.Do(func() { close(t.closed) })
	return nil
}

// scriptedQuestioner answers in order and records what it was shown. delay
// stands in for a person reading the question.
type scriptedQuestioner struct {
	answers []tools.Answer
	err     error
	delay   time.Duration

	mu    sync.Mutex
	asked []tools.Question
}

func (q *scriptedQuestioner) AskUser(_ context.Context, question tools.Question) (tools.Answer, error) {
	if q.delay > 0 {
		time.Sleep(q.delay)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.asked = append(q.asked, question)
	if q.err != nil {
		return tools.Answer{}, q.err
	}
	if len(q.answers) == 0 {
		return tools.Answer{Declined: true}, nil
	}
	answer := q.answers[0]
	q.answers = q.answers[1:]
	return answer, nil
}

func (q *scriptedQuestioner) questions() []tools.Question {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]tools.Question(nil), q.asked...)
}

// elicitReply drives one server-initiated elicitation/create through a client
// and returns the JSON the client sent back.
func elicitReply(t *testing.T, questioner tools.Questioner, params string) map[string]any {
	t.Helper()
	tr := newElicitTransport()
	var opts []Option
	if questioner != nil {
		opts = append(opts, WithQuestioner(questioner))
	}
	c := newClient(Spec{Name: "asker"}, tr, nil, opts...)
	t.Cleanup(func() { _ = c.Close() })

	tr.incoming <- []byte(`{"jsonrpc":"2.0","id":"q1","method":"elicitation/create","params":` + params + `}`)
	select {
	case raw := <-tr.sent:
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("reply is not JSON: %s", raw)
		}
		return decoded
	case <-time.After(5 * time.Second):
		t.Fatal("the client never answered elicitation/create")
		return nil
	}
}

// The reply deadline bounds the send, not the thinking. A timer started before
// the dialog opened would expire while a person read the question, and the
// answer would be composed correctly and then never sent — for every human,
// every time, silently.
func TestAPersonMayTakeLongerThanTheReplyDeadline(t *testing.T) {
	tr := newElicitTransport()
	q := &scriptedQuestioner{answers: []tools.Answer{{Picked: []string{"yes"}}}, delay: 300 * time.Millisecond}
	c := newClient(Spec{Name: "patient"}, tr, nil, WithQuestioner(q))
	c.answerTimeout = 50 * time.Millisecond
	t.Cleanup(func() { _ = c.Close() })

	tr.incoming <- []byte(`{"jsonrpc":"2.0","id":"q1","method":"elicitation/create","params":` +
		`{"message":"ok?","requestedSchema":{"type":"object","properties":{"confirm":{"type":"boolean"}}}}}`)

	select {
	case raw := <-tr.sent:
		var reply map[string]any
		if err := json.Unmarshal(raw, &reply); err != nil {
			t.Fatalf("reply is not JSON: %s", raw)
		}
		if _, isError := reply["error"]; isError {
			t.Fatalf("a slow answer became an error: %s", raw)
		}
		content, _ := resultOf(t, reply)["content"].(map[string]any)
		if content["confirm"] != true {
			t.Errorf("content = %v, want the answer the person gave", content)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the answer never reached the transport")
	}
}

func resultOf(t *testing.T, reply map[string]any) map[string]any {
	t.Helper()
	result, ok := reply["result"].(map[string]any)
	if !ok {
		t.Fatalf("reply carries no result: %v", reply)
	}
	return result
}

// The whole point of the role: a server asks, the user answers, the answer
// goes back as the value the server declared rather than as prose.
func TestElicitationReturnsTheChosenEnumValue(t *testing.T) {
	q := &scriptedQuestioner{answers: []tools.Answer{{Picked: []string{"staging"}}}}
	reply := elicitReply(t, q, `{
		"message": "Which environment should I deploy to?",
		"requestedSchema": {"type":"object","properties":{
			"environment": {"type":"string","enum":["staging","production"],"enumNames":["Staging","Production"]}
		}}}`)

	result := resultOf(t, reply)
	if result["action"] != elicitAccept {
		t.Errorf("action = %v, want accept", result["action"])
	}
	content, _ := result["content"].(map[string]any)
	if content["environment"] != "staging" {
		t.Errorf("content = %v, want the enum value the user picked", content)
	}

	asked := q.questions()
	if len(asked) != 1 {
		t.Fatalf("asked %d questions for one property", len(asked))
	}
	// The user has to know who is asking. Nothing else in the dialog says a
	// server wrote this rather than Switchboard or the model.
	if !strings.Contains(asked[0].Question, "asker") {
		t.Errorf("question %q does not name the server that asked it", asked[0].Question)
	}
	if len(asked[0].Options) != 2 {
		t.Errorf("options = %v, want one per enum value", asked[0].Options)
	}
}

// Declining is an answer the protocol has a word for, and reporting it as an
// error would make a server retry what a person already refused.
func TestDeclinedElicitationIsAnAnswerAndNotAnError(t *testing.T) {
	q := &scriptedQuestioner{answers: []tools.Answer{{Declined: true}}}
	reply := elicitReply(t, q, `{"message":"pick","requestedSchema":{"type":"object","properties":{"name":{"type":"string"}}}}`)

	if _, isError := reply["error"]; isError {
		t.Fatalf("a decline was reported as a protocol error: %v", reply)
	}
	if action := resultOf(t, reply)["action"]; action != elicitDecline {
		t.Errorf("action = %v, want decline", action)
	}
}

// The turn ending underneath a dialog is not the user saying no, and the
// protocol keeps the two apart.
func TestCancelledElicitationIsNotADecline(t *testing.T) {
	q := &scriptedQuestioner{err: context.Canceled}
	reply := elicitReply(t, q, `{"message":"pick","requestedSchema":{"type":"object","properties":{"name":{"type":"string"}}}}`)

	if action := resultOf(t, reply)["action"]; action != elicitCancel {
		t.Errorf("action = %v, want cancel", action)
	}
}

// No questioner is the unattended surface: headless runs, delegate subagents,
// race branches. The method is not served at all there, which is what the
// client also said at initialize by declaring no capability.
func TestElicitationWithoutAUserIsDeclinedAsUnserved(t *testing.T) {
	reply := elicitReply(t, nil, `{"message":"pick","requestedSchema":{"type":"object","properties":{"name":{"type":"string"}}}}`)

	errBody, ok := reply["error"].(map[string]any)
	if !ok {
		t.Fatalf("an unattended client answered a question: %v", reply)
	}
	if code, _ := errBody["code"].(float64); int(code) != -32601 {
		t.Errorf("code = %v, want method-not-found", errBody["code"])
	}
}

// A schema this client will not answer is invalid params, never
// method-not-found: the method is served, this request is not, and a server
// told otherwise would stop asking altogether.
func TestUnsupportedSchemaIsInvalidParamsRatherThanUnserved(t *testing.T) {
	for _, test := range []struct {
		name   string
		params string
	}{
		{"too many properties", `{"message":"m","requestedSchema":{"type":"object","properties":{
			"a":{"type":"string"},"b":{"type":"string"},"c":{"type":"string"},
			"d":{"type":"string"},"e":{"type":"string"}}}}`},
		{"a type this client does not ask for", `{"message":"m","requestedSchema":{"type":"object","properties":{
			"files":{"type":"array"}}}}`},
		{"no properties at all", `{"message":"m","requestedSchema":{"type":"object","properties":{}}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			q := &scriptedQuestioner{answers: []tools.Answer{{Text: "anything"}}}
			reply := elicitReply(t, q, test.params)

			errBody, ok := reply["error"].(map[string]any)
			if !ok {
				t.Fatalf("an unanswerable schema produced a result: %v", reply)
			}
			if code, _ := errBody["code"].(float64); int(code) != -32602 {
				t.Errorf("code = %v, want invalid params", errBody["code"])
			}
			if len(q.questions()) != 0 {
				t.Error("the user was shown a dialog for a schema the client cannot answer")
			}
		})
	}
}

// The answer leaves for a process this program does not confine, which is a
// stronger reason to redact than the ask tool has.
func TestATypedAnswerIsRedactedBeforeItLeaves(t *testing.T) {
	const token = "sk-ant-api03-JZoUmalVvXBSXFuPPFAdMSFRLXMWZAAgvVPMNXHJIRVwvKAFFDTIJXPXBBRLDXNQ"
	q := &scriptedQuestioner{answers: []tools.Answer{{Text: "use " + token + " please"}}}
	reply := elicitReply(t, q, `{"message":"key?","requestedSchema":{"type":"object","properties":{"key":{"type":"string"}}}}`)

	raw, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), token) {
		t.Errorf("the answer carried a credential out to the server: %s", raw)
	}
}

func TestElicitationRedactsServerMetadataBeforeDisplayCaps(t *testing.T) {
	token := "ghp_" + strings.Repeat("A", 36)
	message := strings.Repeat("m", maxElicitMessage-5) + token + " after"
	params, err := json.Marshal(map[string]any{
		"message": message,
		"requestedSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"environment": map[string]any{
					"type":        "string",
					"title":       "deploy with " + token,
					"description": "the server said " + token,
					"enum":        []string{"staging"},
					"enumNames":   []string{"Staging " + token},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	q := &scriptedQuestioner{answers: []tools.Answer{{Picked: []string{"staging"}}}}
	reply := elicitReply(t, q, string(params))

	asked := q.questions()
	if len(asked) != 1 {
		t.Fatalf("asked %d questions, want one", len(asked))
	}
	rendered, err := json.Marshal(asked[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rendered), token) || strings.Contains(string(rendered), token[:12]) {
		t.Fatalf("question exposed server credential metadata: %s", rendered)
	}
	if !strings.Contains(string(rendered), "[redacted: a GitHub token]") {
		t.Fatalf("question did not explain its redaction: %s", rendered)
	}
	replyBytes, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(replyBytes), token) || strings.Contains(string(replyBytes), token[:12]) {
		t.Fatalf("MCP reply exposed server credential metadata: %s", replyBytes)
	}
}

func TestCredentialShapedElicitationSemanticsAreRefused(t *testing.T) {
	token := "ghp_" + strings.Repeat("B", 36)
	tests := []struct {
		name       string
		properties map[string]any
	}{
		{
			name: "property name",
			properties: map[string]any{
				token: map[string]any{"type": "string"},
			},
		},
		{
			name: "enum value",
			properties: map[string]any{
				"choice": map[string]any{"type": "string", "enum": []string{token}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			params, err := json.Marshal(map[string]any{
				"message": "choose",
				"requestedSchema": map[string]any{
					"type":       "object",
					"properties": test.properties,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			q := &scriptedQuestioner{answers: []tools.Answer{{Text: "answer"}}}
			reply := elicitReply(t, q, string(params))
			if len(q.questions()) != 0 {
				t.Fatal("credential-shaped protocol semantics reached the questioner")
			}
			if _, ok := reply["error"].(map[string]any); !ok {
				t.Fatalf("credential-shaped protocol semantics were accepted: %v", reply)
			}
			raw, err := json.Marshal(reply)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), token) || strings.Contains(string(raw), token[:12]) {
				t.Fatalf("refusal echoed the credential to the MCP server: %s", raw)
			}
		})
	}
}

func TestElicitationOptionLabelsStayUniqueAndMapToOriginalValues(t *testing.T) {
	shared := strings.Repeat("v", maxElicitMessage+32)
	values := []string{shared + "first", shared + "second"}
	field := elicitField{Type: "string", Enum: values}
	question, choices, err := elicitQuestion("server", "pick", "choice", field)
	if err != nil {
		t.Fatal(err)
	}
	if len(question.Options) != 2 {
		t.Fatalf("options = %v, want two", question.Options)
	}
	first, second := question.Options[0].Label, question.Options[1].Label
	if first == second {
		t.Fatalf("truncated option labels collided: %q", first)
	}
	if len(first) > maxElicitMessage || len(second) > maxElicitMessage {
		t.Fatalf("option labels exceeded the display cap: %d, %d", len(first), len(second))
	}
	value, ok := elicitValue(field, choices, tools.Answer{Picked: []string{second}})
	if !ok || value != values[1] {
		t.Fatalf("second safe label mapped to %#v, %t; want the second original value", value, ok)
	}
}

func TestElicitationControlTextDoesNotChangeOptionSemantics(t *testing.T) {
	control := "safe\x1b]0;forged\a\u202e\nnext"
	field := elicitField{
		Type:        "string",
		Title:       "title \x1b[2J\u202e",
		Description: "detail \x1b]8;;https://example.invalid\a",
		Enum:        []string{control},
		EnumNames:   []string{"name \x1b[31m\u202e"},
	}
	question, choices, err := elicitQuestion("server\x1b]0;bad\a", "message\n\x1b[2J", "choice", field)
	if err != nil {
		t.Fatal(err)
	}
	if len(question.Options) != 1 {
		t.Fatalf("options = %v, want one", question.Options)
	}
	// The core preserves semantic text; both terminal questioners escape every
	// rendered component. The selected display label must still resolve to the
	// exact server value rather than to its escaped or normalized spelling.
	value, ok := elicitValue(field, choices, tools.Answer{Picked: []string{question.Options[0].Label}})
	if !ok || value != control {
		t.Fatalf("control-bearing option mapped to %#v, %t; want the original semantic value", value, ok)
	}
}

// A boolean is two options rather than a typed word, and comes back as JSON's
// own true rather than the string the user clicked.
func TestBooleanElicitationAnswersWithABoolean(t *testing.T) {
	q := &scriptedQuestioner{answers: []tools.Answer{{Picked: []string{"yes"}}}}
	reply := elicitReply(t, q, `{"message":"ok?","requestedSchema":{"type":"object","properties":{"confirm":{"type":"boolean"}}}}`)

	content, _ := resultOf(t, reply)["content"].(map[string]any)
	if content["confirm"] != true {
		t.Errorf("content = %v, want a JSON boolean", content)
	}
}

// A typed answer past a closed set is not one of the values the server said it
// accepts, so sending it would break the contract the enum is.
func TestATypedAnswerOutsideAnEnumIsNotSent(t *testing.T) {
	q := &scriptedQuestioner{answers: []tools.Answer{{Text: "somewhere else"}}}
	reply := elicitReply(t, q, `{"message":"where?","requestedSchema":{"type":"object","properties":{
		"environment":{"type":"string","enum":["staging","production"]}}}}`)

	if action := resultOf(t, reply)["action"]; action != elicitDecline {
		t.Errorf("action = %v, want decline rather than a value the enum does not contain", action)
	}
}

// Go randomizes map iteration, so decoding the schema into a map would ask the
// same form's questions in a different order on every run.
func TestPropertiesAreAskedInTheOrderTheServerWroteThem(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{
		"zebra":{"type":"string"},"apple":{"type":"string"},"mango":{"type":"string"}}}`)
	want := []string{"zebra", "apple", "mango"}

	// Once would pass by luck; the point is that it does not vary.
	for range 8 {
		names, err := propertyOrder(schema)
		if err != nil {
			t.Fatal(err)
		}
		if len(names) != len(want) {
			t.Fatalf("names = %v, want %v", names, want)
		}
		for i := range want {
			if names[i] != want[i] {
				t.Fatalf("names = %v, want the document's own order %v", names, want)
			}
		}
	}
}

// The capability is a promise to answer. Declaring it with no user attached
// would invite a question this session cannot resolve.
func TestElicitationCapabilityIsDeclaredOnlyWithAUser(t *testing.T) {
	for _, test := range []struct {
		name     string
		attached bool
	}{{"with a user", true}, {"unattended", false}} {
		t.Run(test.name, func(t *testing.T) {
			tr := newElicitTransport()
			var opts []Option
			if test.attached {
				opts = append(opts, WithQuestioner(&scriptedQuestioner{}))
			}
			c := newClient(Spec{Name: "caps"}, tr, nil, opts...)
			t.Cleanup(func() { _ = c.Close() })

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			go func() { _ = c.initialize(ctx) }()

			select {
			case raw := <-tr.sent:
				var sent struct {
					Params struct {
						Capabilities map[string]json.RawMessage `json:"capabilities"`
					} `json:"params"`
				}
				if err := json.Unmarshal(raw, &sent); err != nil {
					t.Fatalf("initialize is not JSON: %s", raw)
				}
				_, declared := sent.Params.Capabilities["elicitation"]
				if declared != test.attached {
					t.Errorf("elicitation declared = %t, want %t: %s", declared, test.attached, raw)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("initialize was never sent")
			}
		})
	}
}
