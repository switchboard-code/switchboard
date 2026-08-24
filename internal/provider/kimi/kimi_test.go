package kimi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/provider/anthropic"
)

func TestReplayExcludesIncompleteAssistantFromKimiWire(t *testing.T) {
	const partial = "PARTIAL MUST NOT REPLAY"
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("request path = %q, want /v1/messages", r.URL.Path)
		}
		var err error
		body, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	client := New("",
		anthropic.WithBaseURL(server.URL),
		anthropic.WithHTTPClient(server.Client()),
	)
	stream, err := client.Stream(context.Background(), Target("k3-256k"), provider.Request{
		Messages: []provider.Message{
			provider.UserText("before"),
			{
				Role:       provider.RoleAssistant,
				Incomplete: true,
				Content:    []provider.Block{provider.Text{Text: partial}},
			},
			provider.UserText("after"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if client.Name() != Name {
		t.Fatalf("client name = %q, want %q", client.Name(), Name)
	}
	if strings.Contains(string(body), partial) {
		t.Fatalf("incomplete assistant reached Kimi wire:\n%s", body)
	}
	if !strings.Contains(string(body), "before") || !strings.Contains(string(body), "after") {
		t.Fatalf("projection removed surrounding durable messages:\n%s", body)
	}
}
