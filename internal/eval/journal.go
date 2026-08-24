package eval

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
)

// Journal writes each attempt as it finishes.
//
// A long corpus run is hours of billable work, and a harness that holds its
// results in memory until the end loses all of them to one timeout. That is not
// hypothetical: a three hour run against twenty tasks died on its deadline and
// left nothing but a stack trace, because the test framework buffers its own
// log until the test returns and a panic never returns.
//
// So results are durable at the moment they exist. A run that dies half way
// through is half a measurement, which is worth considerably more than none.
type Journal struct {
	mu   sync.Mutex
	file *os.File
	enc  *json.Encoder
}

func NewJournal(path string) (*Journal, error) {
	// Windows does not grant a handle opened with O_APPEND the write authority
	// SetEndOfFile needs, so a killed final record must be repaired through a
	// non-append handle. Reopen for kernel append afterwards: every completed
	// result still lands after what survived rather than replacing it.
	repair, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := repairJournalTail(repair); err != nil {
		_ = repair.Close()
		return nil, err
	}
	repairedInfo, err := repair.Stat()
	if err != nil {
		_ = repair.Close()
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		_ = repair.Close()
		return nil, err
	}
	appendInfo, statErr := f.Stat()
	if statErr != nil || !os.SameFile(repairedInfo, appendInfo) {
		return nil, errors.Join(statErr, f.Close(), repair.Close(),
			fmt.Errorf("journal changed while binding its append handle"))
	}
	if err := repair.Close(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &Journal{file: f, enc: json.NewEncoder(f)}, nil
}

// repairJournalTail removes only an invalid, unterminated final record. That
// is the write a killed process can leave behind. Without removing it, the next
// append would concatenate valid JSON onto the fragment and make every resumed
// result after it unreadable.
func repairJournalTail(f *os.File) error {
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return err
	}
	data, err := io.ReadAll(io.NewSectionReader(f, 0, info.Size()))
	if err != nil {
		return err
	}
	if data[len(data)-1] == '\n' {
		return nil
	}

	start := bytes.LastIndexByte(data, '\n') + 1
	tail := bytes.TrimSpace(data[start:])
	switch {
	case len(tail) == 0:
		if err := f.Truncate(int64(start)); err != nil {
			return err
		}
	case json.Valid(tail):
		if _, err := f.WriteAt([]byte{'\n'}, info.Size()); err != nil {
			return err
		}
	default:
		if err := f.Truncate(int64(start)); err != nil {
			return err
		}
	}
	return f.Sync()
}

// Append records one attempt and syncs it. The sync is the point: without it
// the last minutes of a run sit in a buffer the kill signal discards.
func (j *Journal) Append(r Run) error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	if err := j.enc.Encode(r); err != nil {
		return err
	}
	return j.file.Sync()
}

func (j *Journal) Close() error {
	if j == nil {
		return nil
	}
	return j.file.Close()
}

// ReadJournal recovers the runs a previous invocation completed, so a killed run
// can be reported on or continued rather than repeated.
func ReadJournal(path string) ([]Run, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []Run
	reader := bufio.NewReader(f)
	for lineNumber := 1; ; lineNumber++ {
		line, readErr := reader.ReadBytes('\n')
		if len(line) == 0 && errors.Is(readErr, io.EOF) {
			return out, nil
		}

		var r Run
		if err := json.Unmarshal(line, &r); err != nil {
			// A truncated final line is what a killed run leaves behind. The
			// records before it are still good, and discarding them because the
			// last one is short would repeat the mistake this file prevents.
			if errors.Is(readErr, io.EOF) && truncatedJSON(err) {
				return out, nil
			}
			return nil, fmt.Errorf("decode journal line %d: %w", lineNumber, err)
		}
		out = append(out, r)

		switch {
		case readErr == nil:
			continue
		case errors.Is(readErr, io.EOF):
			return out, nil
		default:
			return nil, readErr
		}
	}
}

func truncatedJSON(err error) bool {
	var syntax *json.SyntaxError
	return errors.As(err, &syntax) && syntax.Error() == "unexpected end of JSON input"
}

// Done reports which attempts a journal already holds, keyed the way a run is
// identified, so a resumed run can skip them.
func Done(runs []Run) map[string]bool {
	out := map[string]bool{}
	for _, r := range runs {
		out[attemptKey(r.Arm, r.TaskID, r.Seed)] = true
	}
	return out
}

func attemptKey(arm, task string, seed int) string {
	return arm + "\x00" + task + "\x00" + strconv.Itoa(seed)
}
