package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode"
	"unicode/utf8"

	"github.com/switchboard-code/switchboard/internal/credential"
)

const (
	// MaxProviderJSONBodyBytes bounds successful, non-streaming provider
	// responses. Unlike an error diagnostic, a discovery or count response is
	// useful only when it is complete, so callers refuse rather than decode a
	// prefix.
	MaxProviderJSONBodyBytes = 8 << 20

	// MaxProviderModelEntries is deliberately above the largest discovery page
	// requested by a built-in adapter. It prevents a compact JSON array from
	// amplifying into an unbounded slice allocation.
	MaxProviderModelEntries = 4096

	// MaxProviderModelIDBytes keeps an externally supplied semantic identifier
	// finite before it can become a picker label, route identity, or config key.
	MaxProviderModelIDBytes = 512

	// Reasoning effort values are short protocol identifiers ("high",
	// "xhigh", and similar), not free-form provider text.
	MaxProviderReasoningEffortBytes = 64
)

// DecodeBoundedJSON decodes one complete successful provider response. It
// withholds over-limit responses instead of parsing a prefix whose next byte
// could change its meaning.
func DecodeBoundedJSON(r io.Reader, providerName, detail string, out any) error {
	raw, err := readBoundedProviderJSON(r, providerName, detail)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return &ProtocolError{Provider: providerName, Detail: detail, Err: err}
	}
	return nil
}

// DecodeBoundedModelList additionally checks the cardinality of a named
// top-level array before json.Unmarshal is allowed to allocate the destination
// slice. Unknown fields remain format-compatible and are still covered by the
// whole-body bound.
func DecodeBoundedModelList(r io.Reader, providerName, detail, field string, out any) error {
	raw, err := readBoundedProviderJSON(r, providerName, detail)
	if err != nil {
		return err
	}
	if err := checkTopLevelArrayCardinality(raw, field, MaxProviderModelEntries); err != nil {
		return &ProtocolError{Provider: providerName, Detail: detail, Err: err}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return &ProtocolError{Provider: providerName, Detail: detail, Err: err}
	}
	return nil
}

func readBoundedProviderJSON(r io.Reader, providerName, detail string) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, MaxProviderJSONBodyBytes+1))
	if err != nil {
		return nil, &ProtocolError{Provider: providerName, Detail: detail, Err: err}
	}
	if len(raw) > MaxProviderJSONBodyBytes {
		return nil, &ProtocolError{
			Provider: providerName,
			Detail:   fmt.Sprintf("%s exceeded the %d-byte response limit", detail, MaxProviderJSONBodyBytes),
		}
	}
	return raw, nil
}

func checkTopLevelArrayCardinality(raw []byte, field string, max int) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	opening, err := dec.Token()
	if err != nil {
		return err
	}
	if opening != json.Delim('{') {
		return errors.New("expected a JSON object")
	}
	found := false
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return err
		}
		name, ok := key.(string)
		if !ok {
			return errors.New("expected a JSON object key")
		}
		if name != field {
			var ignored json.RawMessage
			if err := dec.Decode(&ignored); err != nil {
				return err
			}
			continue
		}

		found = true
		start, err := dec.Token()
		if err != nil {
			return err
		}
		if start != json.Delim('[') {
			return fmt.Errorf("field %q is not an array", field)
		}
		count := 0
		for dec.More() {
			count++
			if count > max {
				return fmt.Errorf("field %q contains more than %d entries", field, max)
			}
			var ignored json.RawMessage
			if err := dec.Decode(&ignored); err != nil {
				return err
			}
		}
		if end, err := dec.Token(); err != nil || end != json.Delim(']') {
			if err != nil {
				return err
			}
			return fmt.Errorf("field %q has no closing array delimiter", field)
		}
	}
	if end, err := dec.Token(); err != nil || end != json.Delim('}') {
		if err != nil {
			return err
		}
		return errors.New("expected the end of a JSON object")
	}
	if !found {
		// A missing list is compatible with the existing zero-value behavior.
		return nil
	}
	if dec.More() {
		return errors.New("unexpected data after the JSON object")
	}
	return nil
}

// ValidateModelID rejects server-supplied identifiers that cannot safely be
// used as semantic route identities. The error never includes the identifier:
// it may itself be a credential.
func ValidateModelID(id string) error {
	return validateExternalIdentifier("provider model ID", id, MaxProviderModelIDBytes)
}

// ValidateReasoningEffort applies the same external-identifier boundary to a
// provider-discovered effort value before it becomes a picker value or is
// persisted in target parameters.
func ValidateReasoningEffort(effort string) error {
	return validateExternalIdentifier("provider reasoning effort", effort, MaxProviderReasoningEffortBytes)
}

func validateExternalIdentifier(label, value string, maxBytes int) error {
	switch {
	case value == "":
		return fmt.Errorf("%s is empty", label)
	case len(value) > maxBytes:
		return fmt.Errorf("%s exceeds the %d-byte limit", label, maxBytes)
	case !utf8.ValidString(value):
		return fmt.Errorf("%s is not valid UTF-8", label)
	}
	for _, r := range value {
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
			return fmt.Errorf("%s contains control characters", label)
		}
	}
	if len(credential.ScanPrompt(value)) > 0 {
		return fmt.Errorf("%s contains credential-shaped text", label)
	}
	return nil
}
