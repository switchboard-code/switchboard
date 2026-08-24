package session

import (
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
)

func TestReadOpeningSummaryPreservesAuthorshipBoundary(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message provider.Message
		want    OpeningSummary
	}{
		{
			name: "modern expanded opening",
			message: provider.Message{Role: provider.RoleUser, AuthoredKnown: true, Authored: "inspect @config",
				Content: []provider.Block{provider.Text{Text: "inspect @config\nSECRET FILE BYTES"}}},
			want: OpeningSummary{Text: "inspect @config", Found: true, AuthoredKnown: true},
		},
		{
			name: "legacy expanded opening",
			message: provider.Message{Role: provider.RoleUser,
				Content: []provider.Block{provider.Text{Text: "inspect @config\nSECRET FILE BYTES"}}},
			want: OpeningSummary{Found: true},
		},
		{
			name: "synthetic compact seed",
			message: provider.Message{Role: provider.RoleUser, Synthetic: true,
				Content: []provider.Block{provider.Text{Text: "This session continues an earlier one (id).\n\n## Objective\nfinish safely"}}},
			want: OpeningSummary{Text: "This session continues an earlier one (id).\n\n## Objective\nfinish safely", Found: true, Synthetic: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, workspace := newStore(t)
			sess, err := store.Create(workspace, "test/local/model", "rev")
			if err != nil {
				t.Fatal(err)
			}
			defer sess.Close()
			if err := sess.AppendMessage(tc.message); err != nil {
				t.Fatal(err)
			}
			got, err := ReadOpeningSummary(sess.Path())
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("summary = %+v, want %+v", got, tc.want)
			}
			if !tc.want.AuthoredKnown && !tc.want.Synthetic && strings.Contains(got.Text, "SECRET") {
				t.Fatalf("legacy expanded bytes escaped: %+v", got)
			}
		})
	}
}

func TestReadOpeningSummarySkipsInjectedContext(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	injected := provider.UserText("[watch] injected failure output")
	injected.Injected = true
	if err := sess.AppendMessage(injected); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.UserText("actual user opening")); err != nil {
		t.Fatal(err)
	}
	got, err := ReadOpeningSummary(sess.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "actual user opening" || !got.AuthoredKnown {
		t.Fatalf("injected context became the opening: %+v", got)
	}
}
