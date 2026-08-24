package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/provider"
)

const invalidCandidateID = "20991231T235959.999999-deadbeef"

func candidateRecord(t *testing.T, seq int, typ RecordType, payload any) []byte {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := encodeRecord(Record{
		Seq: seq, At: time.Date(2026, 8, 23, 12, 0, seq, 0, time.UTC),
		Type: typ, Payload: raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func candidateStart(t *testing.T, seq int, id, workspace string) []byte {
	t.Helper()
	return candidateRecord(t, seq, RecordSessionStart, SessionStart{
		ID: id, Workspace: workspace, Target: "test/local/model", Binary: "test",
	})
}

func writeCandidate(t *testing.T, path string, body ...[]byte) []byte {
	t.Helper()
	var data bytes.Buffer
	fmt.Fprintf(&data, "%s %d\n", magic, SchemaVersion)
	for _, part := range body {
		data.Write(part)
	}
	raw := data.Bytes()
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// Make the invalid file unquestionably newer. If List admitted it, Latest
	// would choose it and the test would fail for the user-visible reason.
	future := time.Date(2099, 12, 31, 23, 59, 59, 0, time.UTC)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), raw...)
}

func realCandidate(t *testing.T, store *Store, workspace string) (id, path string) {
	t.Helper()
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.UserText("resume this session")); err != nil {
		t.Fatal(err)
	}
	id, path = sess.ID(), sess.Path()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	return id, path
}

// Every case used to look newer than the real log and could either be listed
// or opened far enough for replay to erase its bad first frame. Candidate
// identity must be proved before --continue ranks it or Open mutates it.
func TestInvalidSessionCandidatesCannotHijackLatest(t *testing.T) {
	tests := []struct {
		name string
		body func(t *testing.T, workspace string) [][]byte
	}{
		{
			name: "magic header only",
			body: func(*testing.T, string) [][]byte { return nil },
		},
		{
			name: "torn first record",
			body: func(t *testing.T, workspace string) [][]byte {
				frame := candidateStart(t, 1, invalidCandidateID, workspace)
				return [][]byte{frame[:len(frame)/2]}
			},
		},
		{
			name: "duplicate start",
			body: func(t *testing.T, workspace string) [][]byte {
				return [][]byte{
					candidateStart(t, 1, invalidCandidateID, workspace),
					candidateStart(t, 2, invalidCandidateID, workspace),
				}
			},
		},
		{
			name: "start is not first",
			body: func(t *testing.T, workspace string) [][]byte {
				return [][]byte{
					candidateRecord(t, 1, RecordMessage, provider.UserText("not an identity")),
					candidateStart(t, 2, invalidCandidateID, workspace),
				}
			},
		},
		{
			name: "first start has wrong sequence",
			body: func(t *testing.T, workspace string) [][]byte {
				return [][]byte{candidateStart(t, 2, invalidCandidateID, workspace)}
			},
		},
		{
			name: "id mismatches filename",
			body: func(t *testing.T, workspace string) [][]byte {
				return [][]byte{candidateStart(t, 1, "different-id", workspace)}
			},
		},
		{
			name: "workspace mismatches directory",
			body: func(t *testing.T, workspace string) [][]byte {
				other := filepath.Join(filepath.Dir(workspace), "other-workspace")
				return [][]byte{candidateStart(t, 1, invalidCandidateID, other)}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, workspace := newStore(t)
			realID, realPath := realCandidate(t, store, workspace)
			badPath := filepath.Join(filepath.Dir(realPath), invalidCandidateID+".log")
			before := writeCandidate(t, badPath, tt.body(t, workspace)...)

			infos, err := store.List(workspace)
			if err != nil {
				t.Fatal(err)
			}
			if len(infos) != 1 || infos[0].ID != realID {
				t.Fatalf("List admitted invalid candidate: %+v", infos)
			}
			all, err := store.ListAll()
			if err != nil {
				t.Fatal(err)
			}
			if len(all) != 1 || len(all[workspace]) != 1 || all[workspace][0].ID != realID {
				t.Fatalf("ListAll admitted or re-keyed invalid candidate: %+v", all)
			}

			latest, err := store.Latest(workspace)
			if err != nil {
				t.Fatalf("invalid newer candidate blocked --continue: %v", err)
			}
			if latest.ID() != realID {
				t.Fatalf("Latest = %s, want valid session %s", latest.ID(), realID)
			}
			if err := latest.Close(); err != nil {
				t.Fatal(err)
			}

			if _, err := store.Open(invalidCandidateID); err == nil {
				t.Fatal("Open accepted an invalid session candidate")
			}
			forkOperations := []struct {
				name string
				run  func() (*Session, error)
			}{
				{"Fork", func() (*Session, error) { return store.Fork(invalidCandidateID, 1) }},
				{"ForkOnto", func() (*Session, error) {
					return store.ForkOnto(invalidCandidateID, 1, "other/local/model")
				}},
				{"ForkForRetry", func() (*Session, error) { return store.ForkForRetry(invalidCandidateID, 0) }},
			}
			for _, operation := range forkOperations {
				fork, err := operation.run()
				if fork != nil {
					fork.Close()
				}
				if err == nil {
					t.Errorf("%s accepted an invalid session candidate", operation.name)
				}
			}
			after, err := os.ReadFile(badPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, before) {
				t.Fatal("rejecting an invalid candidate modified or truncated it")
			}
		})
	}
}

func TestUnsupportedLowSchemaCandidatesCannotHijackLatest(t *testing.T) {
	for _, version := range []int{0, -1} {
		t.Run(fmt.Sprintf("schema_%d", version), func(t *testing.T) {
			store, workspace := newStore(t)
			realID, realPath := realCandidate(t, store, workspace)
			badPath := filepath.Join(filepath.Dir(realPath), invalidCandidateID+".log")
			raw := writeCandidate(t, badPath, candidateStart(t, 1, invalidCandidateID, workspace))
			raw = bytes.Replace(raw,
				[]byte(fmt.Sprintf("%s %d\n", magic, SchemaVersion)),
				[]byte(fmt.Sprintf("%s %d\n", magic, version)), 1)
			if err := os.WriteFile(badPath, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			future := time.Date(2099, 12, 31, 23, 59, 59, 0, time.UTC)
			if err := os.Chtimes(badPath, future, future); err != nil {
				t.Fatal(err)
			}

			infos, err := store.List(workspace)
			if err != nil {
				t.Fatal(err)
			}
			if len(infos) != 1 || infos[0].ID != realID {
				t.Fatalf("List admitted schema-%d candidate: %+v", version, infos)
			}
			latest, err := store.Latest(workspace)
			if err != nil {
				t.Fatalf("schema-%d candidate blocked --continue: %v", version, err)
			}
			if latest.ID() != realID {
				t.Fatalf("Latest = %s, want valid session %s", latest.ID(), realID)
			}
			if err := latest.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Open(invalidCandidateID); err == nil || !strings.Contains(err.Error(), "unsupported session schema") {
				t.Fatalf("Open schema-%d error = %v", version, err)
			}
			after, err := os.ReadFile(badPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, raw) {
				t.Fatal("rejecting unsupported low schema modified the candidate")
			}
		})
	}
}

func TestIDOperationsRefuseAmbiguousValidCandidates(t *testing.T) {
	store, workspace := newStore(t)
	id, _ := realCandidate(t, store, workspace)

	otherWorkspace := filepath.Join(filepath.Dir(workspace), "other-workspace")
	if err := os.MkdirAll(otherWorkspace, 0o755); err != nil {
		t.Fatal(err)
	}
	otherDir := filepath.Join(store.root, workspaceKey(otherWorkspace))
	if err := os.MkdirAll(otherDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCandidate(t, filepath.Join(otherDir, id+".log"), candidateStart(t, 1, id, otherWorkspace))

	if _, err := store.Open(id); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("Open ambiguity error = %v", err)
	}
	for name, run := range map[string]func() (*Session, error){
		"Fork":         func() (*Session, error) { return store.Fork(id, 1) },
		"ForkOnto":     func() (*Session, error) { return store.ForkOnto(id, 1, "other/local/model") },
		"ForkForRetry": func() (*Session, error) { return store.ForkForRetry(id, 0) },
	} {
		fork, err := run()
		if fork != nil {
			fork.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Errorf("%s ambiguity error = %v", name, err)
		}
	}
}

func TestLiveSessionForkRefusesLaterDuplicateStart(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.AppendMessage(provider.UserText("keep this")); err != nil {
		t.Fatal(err)
	}

	duplicate := candidateStart(t, sess.seq+1, sess.ID(), workspace)
	sess.mu.Lock()
	err = sess.writeFrame(duplicate, sess.seq+1)
	if err == nil {
		err = sess.f.Sync()
	}
	sess.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	if fork, err := store.ForkSession(sess, 1); err == nil {
		fork.Close()
		t.Fatal("live-source fork accepted a later duplicate session_start")
	} else if !strings.Contains(err.Error(), "duplicate session_start") {
		t.Fatalf("live-source fork error = %v, want duplicate start refusal", err)
	}
}

func TestCandidateAllowsTornTailOnlyAfterValidStart(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	id, path := sess.ID(), sess.Path()
	if err := sess.AppendMessage(provider.UserText("complete prefix")); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.UserText("torn tail")); err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lastStart := bytes.LastIndex(raw[:len(raw)-1], []byte{'\n'}) + 1
	if lastStart <= 0 || lastStart >= len(raw) {
		t.Fatalf("could not locate final frame in %d bytes", len(raw))
	}
	cut := lastStart + (len(raw)-lastStart)/2
	torn := append([]byte(nil), raw[:cut]...)
	if err := os.WriteFile(path, torn, 0o600); err != nil {
		t.Fatal(err)
	}

	infos, err := store.List(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].ID != id {
		t.Fatalf("valid-start/torn-tail session disappeared from List: %+v", infos)
	}
	reopened, err := store.Open(id)
	if err != nil {
		t.Fatalf("torn tail after a valid start must remain recoverable: %v", err)
	}
	defer reopened.Close()
	if reopened.TruncatedBytes() == 0 {
		t.Fatal("recovery did not report the torn tail")
	}
	state := reopened.State()
	if len(state.Messages) != 1 || strings.TrimSpace(state.Messages[0].Text()) != "complete prefix" {
		t.Fatalf("recovered state crossed the torn tail: %+v", state.Messages)
	}
}

func TestCorruptStagedCandidateMustBePublishedBeforeInventory(t *testing.T) {
	for _, published := range []bool{false, true} {
		name := "unpublished"
		if published {
			name = "published"
		}
		t.Run(name, func(t *testing.T) {
			store, workspace := newStore(t)
			sess, err := store.CreateStaged(workspace, "test/local/model", "rev")
			if err != nil {
				t.Fatal(err)
			}
			for _, text := range []string{"valid prefix", "damage middle", "valid suffix"} {
				if err := sess.AppendMessage(provider.UserText(text)); err != nil {
					t.Fatal(err)
				}
			}
			if published {
				if err := sess.Publish(); err != nil {
					t.Fatal(err)
				}
			}
			id, path := sess.ID(), sess.Path()
			if err := sess.Close(); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			tampered := strings.Replace(string(raw), "damage middle", "DAMAGE MIDDLE", 1)
			if tampered == string(raw) {
				t.Fatal("staged fixture did not contain its middle payload")
			}
			if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
				t.Fatal(err)
			}

			infos, err := store.List(workspace)
			if err != nil {
				t.Fatal(err)
			}
			if published {
				if len(infos) != 1 || infos[0].ID != id || !infos[0].Health.CorruptRecord {
					t.Fatalf("published corrupt session was not discoverable as blocked: %+v", infos)
				}
				if _, err := store.Open(id); !errors.Is(err, ErrCorruptRecord) {
					t.Fatalf("published corrupt Open error = %v, want ErrCorruptRecord", err)
				}
			} else {
				if len(infos) != 0 {
					t.Fatalf("unpublished corrupt stage leaked into inventory: %+v", infos)
				}
				if _, err := store.Open(id); !errors.Is(err, ErrSessionUnpublished) {
					t.Fatalf("unpublished corrupt Open error = %v, want ErrSessionUnpublished", err)
				}
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != tampered {
				t.Fatal("inventory or Open modified the corrupt staged log")
			}
		})
	}
}

func TestNoncanonicalIDsAreRejectedBeforePathResolution(t *testing.T) {
	store, workspace := newStore(t)
	outside := filepath.Join(filepath.Dir(store.root), "outside.log")
	want := []byte("must not be opened or migrated")
	if err := os.WriteFile(outside, want, 0o600); err != nil {
		t.Fatal(err)
	}
	ids := []string{
		"../../outside",
		"../outside",
		"/tmp/outside",
		"*",
		"[abc]",
		"20261340T256199.999999-deadbeef",
		"20991231T235959.999999-DEADBEEF",
	}
	for _, id := range ids {
		t.Run(strings.ReplaceAll(id, "/", "_"), func(t *testing.T) {
			calls := []struct {
				name string
				call func() error
			}{
				{"Open", func() error { _, err := store.Open(id); return err }},
				{"OpenInWorkspace", func() error { _, err := store.OpenInWorkspace(id, workspace); return err }},
				{"Fork", func() error { _, err := store.Fork(id, 1); return err }},
				{"ForkOnto", func() error { _, err := store.ForkOnto(id, 1, "test/local/other"); return err }},
				{"ForkForRetry", func() error { _, err := store.ForkForRetry(id, 0); return err }},
			}
			for _, call := range calls {
				if err := call.call(); err == nil || !strings.Contains(err.Error(), "invalid session id") {
					t.Fatalf("%s(%q) error = %v", call.name, id, err)
				}
			}
		})
	}
	got, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("invalid id reached and changed an outside path")
	}
}

func TestInvalidStartIDRefusesBeforeSchemaMigration(t *testing.T) {
	store, workspace := newStore(t)
	id := invalidCandidateID
	dir := filepath.Join(store.root, workspaceKey(workspace))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".log")
	raw := writeCandidate(t, path, candidateStart(t, 1, "../../outside", workspace))
	raw = bytes.Replace(raw, []byte(fmt.Sprintf("%s %d\n", magic, SchemaVersion)), []byte(magic+" 4\n"), 1)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(id); err == nil || !strings.Contains(err.Error(), "invalid id") {
		t.Fatalf("Open error = %v, want invalid start id", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, raw) || !bytes.HasPrefix(after, []byte(magic+" 4\n")) {
		t.Fatal("invalid identity was migrated or mutated before refusal")
	}
}

func TestGeneratedAndLegacySessionIDShapes(t *testing.T) {
	for _, id := range []string{
		"20260823T120102-deadbeef",
		"20260823T120102.123456-deadbeef",
	} {
		if !validSessionID(id) {
			t.Fatalf("generated session id %q rejected", id)
		}
	}
}

func TestOpenInWorkspaceRefusesAnotherWorkspacesSessionWithoutMutation(t *testing.T) {
	store, selectedWorkspace := newStore(t)
	recordedWorkspace := t.TempDir()
	sess, err := store.Create(recordedWorkspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.UserText("work only in the recorded workspace")); err != nil {
		t.Fatal(err)
	}
	id, path := sess.ID(), sess.Path()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if opened, err := store.OpenInWorkspace(id, selectedWorkspace); err == nil {
		opened.Close()
		t.Fatal("OpenInWorkspace adopted a transcript into the wrong tool workspace")
	} else if !strings.Contains(err.Error(), recordedWorkspace) || !strings.Contains(err.Error(), "-workspace") {
		t.Fatalf("workspace refusal does not explain how to resume safely: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("workspace refusal modified or reconciled the rejected session")
	}

	opened, err := store.OpenInWorkspace(id, recordedWorkspace)
	if err != nil {
		t.Fatalf("OpenInWorkspace refused the session in its own workspace: %v", err)
	}
	opened.Close()
}
