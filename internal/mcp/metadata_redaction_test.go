package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/tools"
)

func TestHTTPToolMetadataRedactsExactRequestSecrets(t *testing.T) {
	const authorization = "opaque-authorization<4f91c7d2>"
	const requestContext = "opaque-context<e36b08a5>"
	const numericCredential = "731946285013"
	for _, value := range []string{authorization, requestContext, numericCredential} {
		if leaks := credential.ScanPrompt(value); len(leaks) != 0 {
			t.Fatalf("fixture %q is credential-shaped; test requires opaque values", value)
		}
	}

	for _, listMediaType := range []string{"application/json", "text/event-stream"} {
		t.Run(listMediaType, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				var request struct {
					ID     json.RawMessage `json:"id"`
					Method string          `json:"method"`
				}
				if err := json.Unmarshal(body, &request); err != nil {
					http.Error(w, "bad request", http.StatusBadRequest)
					return
				}

				var result any
				switch request.Method {
				case "server/discover":
					result = map[string]any{
						"resultType":        "complete",
						"supportedVersions": []string{modernProtocolVersion},
						"capabilities":      map[string]any{"tools": map[string]any{}},
					}
				case "tools/list":
					auth := r.Header.Get("Authorization")
					contextValue := r.Header.Get("X-Request-Context")
					numericValue := r.Header.Get("X-API-Key")
					if auth != authorization || contextValue != requestContext || numericValue != numericCredential {
						http.Error(w, "configured headers missing", http.StatusBadRequest)
						return
					}
					result = map[string]any{
						"resultType": "complete",
						"tools": []any{
							map[string]any{
								"name":        "probe",
								"description": "ordinary description; authorization=" + auth,
								"inputSchema": map[string]any{
									"type":  "object",
									"title": "ordinary title",
									"properties": map[string]any{
										// json.Marshal escapes '<' in this secret-bearing key.
										contextValue: map[string]any{
											"type":        "string",
											"description": "nested authorization " + auth,
											"default":     auth,
										},
										"nested": map[string]any{
											"type": "object",
											"properties": map[string]any{
												"value": map[string]any{
													"type":        "string",
													"description": "nested context " + contextValue,
													"examples":    []string{contextValue, "ordinary-example"},
												},
											},
										},
									},
									"required":             []string{contextValue},
									"additionalProperties": false,
									"x-ordinary":           "kept exactly",
								},
							},
							map[string]any{
								"name":        "numeric",
								"description": "numeric primitive safety",
								"inputSchema": map[string]any{
									"type":       "number",
									"maximum":    json.Number(numericValue),
									"x-ordinary": "must not survive an unsafe schema",
								},
							},
							// A tool name is both provider metadata and the remote call
							// identity, so a credential-bearing one must be skipped rather
							// than rewritten into an invocation of a different name.
							map[string]any{
								"name":        auth,
								"description": "must not survive discovery",
								"inputSchema": map[string]any{"type": "object"},
							},
						},
					}
				default:
					http.Error(w, "unexpected method", http.StatusBadRequest)
					return
				}

				envelope, err := json.Marshal(map[string]any{
					"jsonrpc": "2.0",
					"id":      request.ID,
					"result":  result,
				})
				if err != nil {
					http.Error(w, "bad response", http.StatusInternalServerError)
					return
				}
				if request.Method == "tools/list" && listMediaType == "text/event-stream" {
					w.Header().Set("Content-Type", listMediaType)
					fmt.Fprintf(w, "data: %s\n\n", envelope)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(envelope)
			})
			srv := httptest.NewServer(handler)
			t.Cleanup(srv.Close)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, err := Connect(ctx, Spec{
				Name: "metadata-redaction",
				URL:  srv.URL,
				Headers: map[string]string{
					"Authorization":     authorization,
					"X-API-Key":         numericCredential,
					"X-Request-Context": requestContext,
				},
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()

			inventory := client.Tools()
			if len(inventory) != 2 {
				t.Fatalf("tools = %+v, want probe and numeric while the credential-named tool is skipped", inventory)
			}
			inventoryByName := make(map[string]ToolInfo, len(inventory))
			for _, info := range inventory {
				inventoryByName[info.Name] = info
			}
			probe, ok := inventoryByName["probe"]
			if !ok {
				t.Fatalf("tools = %+v, want the ordinary probe identity unchanged", inventory)
			}
			assertNoMetadataValues(t, probe.Description, probe.InputSchema, authorization, requestContext, numericCredential)
			if !strings.Contains(probe.Description, "ordinary description") ||
				!strings.Contains(probe.Description, "[redacted]") {
				t.Fatalf("description lost ordinary content or marker: %q", probe.Description)
			}
			assertOrdinarySchemaFields(t, probe.InputSchema)
			numeric, ok := inventoryByName["numeric"]
			if !ok {
				t.Fatalf("tools = %+v, want numeric primitive coverage", inventory)
			}
			assertNoMetadataValues(t, numeric.Description, numeric.InputSchema, authorization, requestContext, numericCredential)
			assertEmptyObjectSchema(t, numeric.InputSchema)

			registry, err := tools.NewRegistry(t.TempDir(), execution.Capability{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(registry.StopBackgroundCommands)
			for _, tool := range client.BridgedTools() {
				if err := registry.AddExternal(tool); err != nil {
					t.Fatal(err)
				}
			}
			found := map[string]bool{}
			for _, definition := range registry.Definitions() {
				switch definition.Name {
				case Namespaced("metadata-redaction", "probe"):
					found["probe"] = true
					assertNoMetadataValues(t, definition.Description, definition.Schema, authorization, requestContext, numericCredential)
					assertOrdinarySchemaFields(t, definition.Schema)
				case Namespaced("metadata-redaction", "numeric"):
					found["numeric"] = true
					assertNoMetadataValues(t, definition.Description, definition.Schema, authorization, requestContext, numericCredential)
					assertEmptyObjectSchema(t, definition.Schema)
				}
			}
			if !found["probe"] || !found["numeric"] {
				t.Fatalf("sanitized MCP provider definitions found = %v", found)
			}
		})
	}
}

func TestToolMetadataRedactionDoesNotMutateDiscoverySource(t *testing.T) {
	const secret = "opaque-source<91d4>"
	originalSchema := json.RawMessage(`{"type":"object","properties":{"opaque-source\u003c91d4\u003e":{"description":"use opaque-source\u003c91d4\u003e"}},"maximum":42}`)
	original := ToolInfo{
		Name:        "probe",
		Description: "description " + secret,
		InputSchema: originalSchema,
	}
	wantDescription := original.Description
	wantSchema := string(original.InputSchema)

	redacted, safe := redactToolMetadata(original, []string{secret})
	if !safe {
		t.Fatal("ordinary tool name was rejected")
	}
	assertNoMetadataValues(t, redacted.Description, redacted.InputSchema, secret)
	if original.Description != wantDescription || string(original.InputSchema) != wantSchema {
		t.Fatal("redaction mutated the source ToolInfo")
	}
	if !strings.Contains(string(redacted.InputSchema), `"maximum":42`) {
		t.Fatalf("unrelated schema bytes changed: %s", redacted.InputSchema)
	}

	if _, safe := redactToolMetadata(ToolInfo{Name: secret}, []string{secret}); safe {
		t.Fatal("credential-bearing protocol name was rewritten instead of rejected")
	}
}

func assertNoMetadataValues(t *testing.T, description string, schema json.RawMessage, values ...string) {
	t.Helper()
	for _, value := range values {
		if strings.Contains(description, value) {
			t.Fatalf("description contains request secret %q: %q", value, description)
		}
	}
	var decoded any
	decoder := json.NewDecoder(bytes.NewReader(schema))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("schema is not valid JSON: %s: %v", schema, err)
	}
	var visit func(any)
	visit = func(value any) {
		switch value := value.(type) {
		case string:
			for _, secret := range values {
				if strings.Contains(value, secret) {
					t.Fatalf("schema string contains request secret %q: %q", secret, value)
				}
			}
		case []any:
			for _, child := range value {
				visit(child)
			}
		case map[string]any:
			for key, child := range value {
				visit(key)
				visit(child)
			}
		case json.Number:
			visit(value.String())
		case bool:
			visit(fmt.Sprint(value))
		case nil:
			visit("null")
		}
	}
	visit(decoded)
}

func assertOrdinarySchemaFields(t *testing.T, schema json.RawMessage) {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(schema, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["type"] != "object" || decoded["title"] != "ordinary title" ||
		decoded["x-ordinary"] != "kept exactly" || decoded["additionalProperties"] != false {
		t.Fatalf("ordinary schema fields changed: %s", schema)
	}
	if !strings.Contains(string(schema), "ordinary-example") {
		t.Fatalf("ordinary nested schema value changed: %s", schema)
	}
}

func assertEmptyObjectSchema(t *testing.T, schema json.RawMessage) {
	t.Helper()
	var decoded struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Maximum    json.RawMessage            `json:"maximum"`
	}
	if err := json.Unmarshal(schema, &decoded); err != nil {
		t.Fatalf("safe fallback is invalid JSON: %s: %v", schema, err)
	}
	if decoded.Type != "object" || decoded.Properties == nil || len(decoded.Properties) != 0 || len(decoded.Maximum) != 0 {
		t.Fatalf("unsafe primitive did not produce a valid empty-object schema: %s", schema)
	}
}
