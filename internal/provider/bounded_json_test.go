package provider

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestDecodeBoundedJSONAcceptsExactBoundaryAndRefusesOneByteMore(t *testing.T) {
	prefix := `{"value":"`
	suffix := `"}`
	atLimit := prefix + strings.Repeat("x", MaxProviderJSONBodyBytes-len(prefix)-len(suffix)) + suffix
	var decoded struct {
		Value string `json:"value"`
	}
	if err := DecodeBoundedJSON(strings.NewReader(atLimit), "test", "decoding response", &decoded); err != nil {
		t.Fatalf("exact boundary: %v", err)
	}
	if len(decoded.Value) == 0 {
		t.Fatal("exact-boundary value was not decoded")
	}

	err := DecodeBoundedJSON(strings.NewReader(atLimit+" "), "test", "decoding response", &decoded)
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) || !strings.Contains(err.Error(), "response limit") {
		t.Fatalf("one byte over boundary: err = %v, want bounded ProtocolError", err)
	}
}

func TestDecodeBoundedModelListRefusesTooManyEntriesBeforeUnmarshal(t *testing.T) {
	var body bytes.Buffer
	body.WriteString(`{"models":[`)
	for i := 0; i <= MaxProviderModelEntries; i++ {
		if i > 0 {
			body.WriteByte(',')
		}
		body.WriteString(`{}`)
	}
	body.WriteString(`]}`)
	var decoded struct {
		Models []struct{} `json:"models"`
	}
	err := DecodeBoundedModelList(&body, "test", "decoding models", "models", &decoded)
	if err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("err = %v, want cardinality refusal", err)
	}
	if decoded.Models != nil {
		t.Fatalf("destination was partially populated: %d models", len(decoded.Models))
	}
}

func TestValidateModelID(t *testing.T) {
	token := "ghp_" + strings.Repeat("A", 40)
	for name, id := range map[string]string{
		"empty":       "",
		"oversize":    strings.Repeat("m", MaxProviderModelIDBytes+1),
		"invalid utf": string([]byte{'m', 0xff}),
		"newline":     "model\nspoof",
		"bidi":        "model\u202Espoof",
		"credential":  "model-" + token,
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidateModelID(id)
			if err == nil {
				t.Fatal("invalid model ID was accepted")
			}
			if strings.Contains(err.Error(), token) {
				t.Fatalf("error leaked credential-shaped ID: %v", err)
			}
		})
	}
	if err := ValidateModelID(strings.Repeat("m", MaxProviderModelIDBytes)); err != nil {
		t.Fatalf("exact-boundary valid model ID: %v", err)
	}
}

func TestValidateReasoningEffortRejectsExternalControlsAndCredentials(t *testing.T) {
	token := "ghp_" + strings.Repeat("A", 40)
	for _, effort := range []string{"high\nspoof", "high\u202Espoof", token, strings.Repeat("x", MaxProviderReasoningEffortBytes+1)} {
		err := ValidateReasoningEffort(effort)
		if err == nil {
			t.Fatalf("unsafe reasoning effort %q was accepted", effort)
		}
		if strings.Contains(err.Error(), token) {
			t.Fatalf("error leaked credential-shaped effort: %v", err)
		}
	}
	if err := ValidateReasoningEffort("xhigh"); err != nil {
		t.Fatalf("valid reasoning effort: %v", err)
	}
}
