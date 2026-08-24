package mcp

// Elicitation: a server asking the user a question, answered through the same
// surface the ask tool asks through.
//
// The role is granted, never assumed. Sampling and roots stay declined because
// each hands the server something the user never offered — a sampling request
// spends the user's model budget, a roots request describes the filesystem —
// but a question is neither. It is the posture ask already states: a question
// is interaction, not an effect, and the answer channel is a person who can
// refuse in person. What keeps an unattended surface from being asked is the
// absent questioner, exactly as it is for the tool: headless runs, delegate
// subagents, and race branches never set one, the capability is therefore not
// declared at initialize, and a server that asks anyway is declined the way it
// was before this file existed.
//
// Two things travel in from a server that is not trusted with either. The
// message is text on the user's screen, so the dialog names the server that
// wrote it and the text is capped; a question that looked like Switchboard's
// own would be the whole attack. And the answer travels outward to an
// unconfined process, which is a stronger reason to redact than the tool has:
// it passes credential.ScanPrompt and redacts unconditionally, the same
// posture, applied where the consequence is larger.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/tools"
)

const (
	// maxElicitFields bounds how many dialogs one request can open. A server
	// that wants a form can ask again; a server that wants the screen cannot
	// have it.
	maxElicitFields = 4

	// maxElicitMessage caps the server's own prose. Past this the dialog stops
	// being a question and starts being a page.
	maxElicitMessage = 500

	// maxElicitOptions matches what the ask dialog reads comfortably. An enum
	// longer than this is refused rather than scrolled.
	maxElicitOptions = 12
)

// elicitAction is the protocol's three answers. Declining and cancelling are
// different facts and the server is entitled to both: the user waving the
// question away is a decision, the turn ending underneath it is not.
const (
	elicitAccept  = "accept"
	elicitDecline = "decline"
	elicitCancel  = "cancel"
)

type elicitRequest struct {
	Message         string          `json:"message"`
	RequestedSchema json.RawMessage `json:"requestedSchema"`
}

type elicitResult struct {
	Action  string         `json:"action"`
	Content map[string]any `json:"content,omitempty"`
}

// elicitField is the subset of JSON Schema this client answers. Anything a
// server declares beyond it blocks the request rather than being ignored,
// because a control that changes what the answer means cannot be dropped and
// still called an answer.
type elicitField struct {
	Type        string   `json:"type"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Enum        []string `json:"enum"`
	EnumNames   []string `json:"enumNames"`
}

// unsupportedElicit is a schema this client will not answer. It is separate
// from a transport or protocol failure: the request arrived intact and was
// refused on its contents, so the server is told which part.
type unsupportedElicit struct{ reason string }

func (e *unsupportedElicit) Error() string { return e.reason }

// answerElicitation resolves one elicitation/create against the user.
//
// A returned error is a schema this client does not serve. Everything else,
// including the user saying no, is a result: the protocol has words for
// declining and cancelling, and reporting either as an error would make a
// server retry something a person already answered.
func (c *Client) answerElicitation(ctx context.Context, params json.RawMessage) (elicitResult, error) {
	questioner := c.questioner
	if questioner == nil {
		return elicitResult{}, &unsupportedElicit{"no user is attached to this session"}
	}

	var req elicitRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return elicitResult{}, &unsupportedElicit{"malformed elicitation/create params"}
	}

	names, err := propertyOrder(req.RequestedSchema)
	if err != nil {
		return elicitResult{}, err
	}
	if len(names) == 0 {
		return elicitResult{}, &unsupportedElicit{"requestedSchema declares no properties"}
	}
	if len(names) > maxElicitFields {
		return elicitResult{}, &unsupportedElicit{fmt.Sprintf(
			"requestedSchema declares %d properties; this client asks at most %d per request",
			len(names), maxElicitFields)}
	}

	var schema struct {
		Properties map[string]elicitField `json:"properties"`
	}
	if err := json.Unmarshal(req.RequestedSchema, &schema); err != nil {
		return elicitResult{}, &unsupportedElicit{"requestedSchema is not an object schema"}
	}

	content := make(map[string]any, len(names))
	for _, name := range names {
		field := schema.Properties[name]
		question, choices, err := elicitQuestion(c.spec.Name, req.Message, name, field)
		if err != nil {
			return elicitResult{}, err
		}

		answer, err := questioner.AskUser(ctx, question)
		if err != nil {
			// The turn ended or the program quit underneath the dialog. That
			// is not the user declining, and the protocol distinguishes them.
			return elicitResult{Action: elicitCancel}, nil
		}

		value, ok := elicitValue(field, choices, answer)
		if !ok {
			if answer.Text != "" {
				// The one place an unusable answer is not a lie to report as a
				// decline: the user answered, the answer does not fit the type
				// the server asked for, and nobody but the user can fix that.
				c.logf("warn", fmt.Sprintf("mcp %s asked for a %s in %q and the answer was not one; declining",
					c.spec.Name, field.Type, name))
			}
			return elicitResult{Action: elicitDecline}, nil
		}
		content[name] = value
	}
	return elicitResult{Action: elicitAccept, Content: content}, nil
}

// elicitQuestion renders one field as a question the ask surface can show.
//
// The server's name leads, because the user is being asked by something that
// is not Switchboard and not the model, and no other part of the dialog says
// so.
func elicitQuestion(server, message, name string, field elicitField) (tools.Question, map[string]string, error) {
	// Property names and enum values are protocol semantics: accepting the
	// question would put them back into the MCP reply. Redacting either would
	// answer a different schema, while echoing one would violate the outbound
	// credential boundary. Metadata used only for display can be redacted below;
	// semantic credential-shaped values make this elicitation unsupported.
	if len(credential.ScanPrompt(name)) > 0 {
		return tools.Question{}, nil, &unsupportedElicit{"requestedSchema contains a credential-shaped property name"}
	}
	for _, value := range field.Enum {
		if len(credential.ScanPrompt(value)) > 0 {
			return tools.Question{}, nil, &unsupportedElicit{"requestedSchema contains a credential-shaped enum value"}
		}
	}

	label := field.Title
	if label == "" {
		label = name
	}

	var b strings.Builder
	fmt.Fprintf(&b, "MCP server %s asks: ", truncateElicit(server))
	if message != "" {
		b.WriteString(truncateElicit(message))
		b.WriteString("\n\n")
	}
	b.WriteString(truncateElicit(label))
	if field.Description != "" {
		b.WriteString(" — ")
		b.WriteString(truncateElicit(field.Description))
	}
	question := tools.Question{Question: b.String()}

	switch field.Type {
	case "string":
		if len(field.Enum) == 0 {
			// No options is the free-text dialog: the type-your-own row and
			// esc, which is exactly what an unconstrained string wants.
			return question, nil, nil
		}
		if len(field.Enum) > maxElicitOptions {
			return tools.Question{}, nil, &unsupportedElicit{fmt.Sprintf(
				"property %q offers %d enum values; this client shows at most %d",
				truncateElicit(name), len(field.Enum), maxElicitOptions)}
		}
		labels := uniqueElicitOptionLabels(field.Enum)
		choices := make(map[string]string, len(labels))
		for i, value := range field.Enum {
			option := tools.QuestionOption{Label: labels[i]}
			if i < len(field.EnumNames) && field.EnumNames[i] != "" {
				// enumNames is the display text and enum is the wire value.
				// Showing the value and answering with it keeps the two from
				// drifting; the name rides as the detail so the user reads
				// what the server meant.
				option.Detail = truncateElicit(field.EnumNames[i])
			}
			question.Options = append(question.Options, option)
			choices[option.Label] = value
		}
		return question, choices, nil

	case "boolean":
		question.Options = []tools.QuestionOption{{Label: "yes"}, {Label: "no"}}
		return question, nil, nil

	case "number", "integer":
		return question, nil, nil

	default:
		// An empty type included: a property with no declared type is not a
		// string by default here, because guessing one would put an arbitrary
		// value into a field the server will act on.
		return tools.Question{}, nil, &unsupportedElicit{fmt.Sprintf(
			"property %q has type %q, which this client does not ask for",
			truncateElicit(name), truncateElicit(field.Type))}
	}
}

// uniqueElicitOptionLabels gives the question surface only bounded display
// values while retaining a one-to-one key for the protocol value. Credential-
// shaped semantic values were refused above, but two long ordinary values can
// truncate to the same label and an ordinary value can collide with a
// generated suffix. Resolve both cases deterministically so selecting a safe
// label never guesses which server value it represented.
func uniqueElicitOptionLabels(values []string) []string {
	bases := make([]string, len(values))
	counts := make(map[string]int, len(values))
	for i, value := range values {
		base := truncateElicit(value)
		if base == "" {
			base = "(empty value)"
		}
		bases[i] = base
		counts[base]++
	}

	labels := make([]string, len(values))
	used := make(map[string]struct{}, len(values))
	for i, base := range bases {
		label := base
		if counts[base] > 1 {
			label = elicitLabelWithSuffix(base, fmt.Sprintf(" (option %d)", i+1))
		}
		for attempt := 2; ; attempt++ {
			if _, exists := used[label]; !exists {
				break
			}
			label = elicitLabelWithSuffix(base, fmt.Sprintf(" (option %d.%d)", i+1, attempt))
		}
		labels[i] = label
		used[label] = struct{}{}
	}
	return labels
}

func elicitLabelWithSuffix(base, suffix string) string {
	if len(suffix) >= maxElicitMessage {
		return truncateUTF8(suffix, maxElicitMessage)
	}
	return truncateUTF8(base, maxElicitMessage-len(suffix)) + suffix
}

// elicitValue maps what the user did onto the type the server declared. The
// false return is "no usable answer", which covers declining and covers an
// answer that does not fit; the caller separates them for the log, not for the
// server, because the server gets the same word either way.
func elicitValue(field elicitField, choices map[string]string, answer tools.Answer) (any, bool) {
	if answer.Declined {
		return nil, false
	}

	text := answer.Text
	if len(answer.Picked) > 0 {
		text = answer.Picked[0]
		if len(field.Enum) > 0 {
			value, ok := choices[text]
			if !ok {
				return nil, false
			}
			text = value
		}
	} else if len(field.Enum) > 0 {
		// A person may type the safe label instead of selecting it. Treat that
		// exactly like the corresponding pick; a raw enum value remains accepted
		// below for compatibility with non-dialog Questioner implementations.
		if value, ok := choices[text]; ok {
			text = value
		}
	}
	if text == "" {
		return nil, false
	}

	// Outbound to a process this program does not confine. The tool redacts a
	// typed answer for a weaker reason than this one.
	if leaks := credential.ScanPrompt(text); len(leaks) > 0 {
		text = credential.Redact(text, leaks)
	}

	switch field.Type {
	case "boolean":
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "yes", "true", "y":
			return true, true
		case "no", "false", "n":
			return false, true
		default:
			return nil, false
		}
	case "integer":
		n, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
		if err != nil {
			return nil, false
		}
		return n, true
	case "number":
		n, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if err != nil {
			return nil, false
		}
		return n, true
	default:
		if len(field.Enum) > 0 && !containsString(field.Enum, text) {
			// A typed answer past a closed set is not one of the values the
			// server said it accepts, so sending it would break the contract
			// the enum is.
			return nil, false
		}
		return text, true
	}
}

// propertyOrder reads the property names in the order the server wrote them.
//
// Decoding into a map would hand back Go's randomized iteration order, so the
// same schema would ask its questions in a different order on each run. The
// order the document carries is the only ordering evidence there is, and a
// form whose fields shuffle between runs reads as a broken program.
func propertyOrder(raw json.RawMessage) ([]string, error) {
	var envelope struct {
		Properties json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || len(envelope.Properties) == 0 {
		return nil, &unsupportedElicit{"requestedSchema has no properties object"}
	}

	dec := json.NewDecoder(bytes.NewReader(envelope.Properties))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, &unsupportedElicit{"requestedSchema properties is not an object"}
	}
	var names []string
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, &unsupportedElicit{"requestedSchema properties is malformed"}
		}
		name, ok := key.(string)
		if !ok {
			return nil, &unsupportedElicit{"requestedSchema properties is malformed"}
		}
		names = append(names, name)
		// Skip the value whole; only the key order is wanted here.
		var discard json.RawMessage
		if err := dec.Decode(&discard); err != nil {
			return nil, &unsupportedElicit{"requestedSchema properties is malformed"}
		}
	}
	return names, nil
}

func truncateElicit(text string) string {
	// Scan the whole semantic component before any cap can turn a recognized
	// credential into an unrecognized prefix. The question surface performs
	// terminal escaping later; this layer preserves the original semantic value
	// only in the private enum mapping above, never in what it renders.
	if leaks := credential.ScanPrompt(text); len(leaks) > 0 {
		text = credential.Redact(text, leaks)
	}
	text = strings.TrimSpace(text)
	if len(text) <= maxElicitMessage {
		return text
	}
	const suffix = "…"
	cut := maxElicitMessage - len(suffix)
	// Keep a generated redaction marker whole when it lands on the cap. A
	// half marker is safe in the narrow sense but reads like corrupt output and
	// hides why the server's prose changed. Sacrifice earlier prose instead.
	if markerStart := strings.LastIndex(text[:cut], "[redacted:"); markerStart >= 0 {
		if markerTail := strings.IndexByte(text[markerStart:], ']'); markerTail >= 0 {
			markerEnd := markerStart + markerTail + 1
			if markerEnd > cut {
				marker := text[markerStart:markerEnd]
				prefixLimit := maxElicitMessage - len(suffix) - len(marker)
				return truncateUTF8(text[:markerStart], prefixLimit) + marker + suffix
			}
		}
	}
	return truncateUTF8(text, cut) + suffix
}

func truncateUTF8(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.ValidString(text[:cut]) {
		cut--
	}
	return text[:cut]
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
