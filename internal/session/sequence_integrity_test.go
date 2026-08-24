package session

import (
	"errors"
	"math"
	"os"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/provider"
)

func TestCompleteOutOfSequenceFramesAreCorruptionAndNeverAppendBasis(t *testing.T) {
	tests := map[string]int{
		"duplicate": 2,
		"backward":  1,
		"gap":       4,
		"zero":      0,
		"negative":  -1,
		"max-int":   math.MaxInt,
	}
	for name, sequence := range tests {
		t.Run(name, func(t *testing.T) {
			store, workspace := newStore(t)
			sess, err := store.Create(workspace, "test/local/model", "rev")
			if err != nil {
				t.Fatal(err)
			}
			if err := sess.AppendMessage(provider.UserText("preserve the valid prefix")); err != nil {
				t.Fatal(err)
			}
			id, path := sess.ID(), sess.Path()
			frame, err := encodeRecord(Record{
				Seq: sequence, At: time.Now().UTC(), Type: RecordNote,
				Payload: []byte(`{"level":"info","text":"complete but out of sequence"}`),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := sess.writeFrame(frame, sequence); err != nil {
				t.Fatal(err)
			}
			if err := sess.f.Sync(); err != nil {
				t.Fatal(err)
			}
			if err := sess.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			infos, err := store.List(workspace)
			if err != nil {
				t.Fatal(err)
			}
			if len(infos) != 1 || infos[0].ID != id || !infos[0].Health.CorruptRecord {
				t.Fatalf("out-of-sequence session was hidden or called healthy: %+v", infos)
			}
			if infos[0].Health.RecoveredCorruptTail || infos[0].Health.Messages != 1 {
				t.Fatalf("out-of-sequence health crossed or repaired the bad frame: %+v", infos[0].Health)
			}
			all, err := store.ListAll()
			if err != nil {
				t.Fatal(err)
			}
			if len(all[workspace]) != 1 || !all[workspace][0].Health.CorruptRecord {
				t.Fatalf("ListAll hid or called the out-of-sequence session healthy: %+v", all[workspace])
			}

			if reopened, err := store.Open(id); !errors.Is(err, ErrCorruptRecord) {
				if reopened != nil {
					_ = reopened.Close()
				}
				t.Fatalf("writable resume error = %v, want ErrCorruptRecord", err)
			}
			if latest, err := store.Latest(workspace); !errors.Is(err, ErrCorruptRecord) {
				if latest != nil {
					_ = latest.Close()
				}
				t.Fatalf("Latest error = %v, want ErrCorruptRecord", err)
			}
			readers := map[string]func() error{
				"state":       func() error { _, err := ReadState(path); return err },
				"races":       func() error { _, err := ReadRaces(path); return err },
				"permissions": func() error { _, err := ReadPermissions(path); return err },
				"timeline":    func() error { _, err := ReadTimeline(path); return err },
				"usage":       func() error { _, err := ReadUsages(path); return err },
				"file edits":  func() error { _, err := ReadFileEdits(path); return err },
				"accounting":  func() error { _, err := ReadAccountingLedger(path); return err },
				"turn costs":  func() error { _, err := ReadTurnCosts(path); return err },
			}
			for reader, read := range readers {
				if err := read(); !errors.Is(err, ErrCorruptRecord) {
					t.Errorf("%s error = %v, want ErrCorruptRecord", reader, err)
				}
			}
			if forked, err := store.Fork(id, 1); !errors.Is(err, ErrCorruptRecord) {
				if forked != nil {
					_ = forked.Close()
				}
				t.Fatalf("Fork error = %v, want ErrCorruptRecord", err)
			}

			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatal("out-of-sequence refusal modified or truncated the log")
			}
		})
	}
}

func TestAppendRefusesSequenceOverflowWithoutWriting(t *testing.T) {
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	path := sess.Path()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// The durable decoder can never produce this state from a valid log. Keep
	// the writer defensive anyway: int overflow must not manufacture sequence
	// zero and turn a healthy prefix into a log that no reader can resume.
	sess.seq = math.MaxInt
	if err := sess.AppendNote("info", "must not be written"); !errors.Is(err, ErrSessionPoisoned) {
		t.Fatalf("AppendNote error = %v, want ErrSessionPoisoned", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("overflow refusal wrote to the session log")
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.OpenInWorkspace(sess.ID(), workspace)
	if err != nil {
		t.Fatalf("opening the unchanged durable prefix: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}
