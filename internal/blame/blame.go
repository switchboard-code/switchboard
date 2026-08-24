// Package blame attributes each line of a file to the recorded turn that
// wrote it, by replaying the write and edit calls the session logs carry.
//
// The replay is against the record, not the filesystem: a write's content
// and an edit's replacement are in the log byte for byte, so the sequence
// of recorded states is reconstructable without ever having watched the
// file. What the record cannot explain stays unexplained — a line the
// user typed, a formatter rewrote, or a shell command produced reads as
// outside the record, because only write and edit put their bytes where
// this replay can see them. That is the absent-not-guessed rule applied
// to provenance: attribution here is evidence, and a guessed line would
// poison every claim built on it.
package blame

import (
	"strconv"
	"strings"
	"time"

	"github.com/switchboard-code/switchboard/internal/session"
)

// Origin is one distinct source of lines: a turn, on a target, in a
// session. Two edits from the same turn and target are one origin.
type Origin struct {
	SessionID           string
	Turn                int
	Prompt              string
	PromptAuthoredKnown bool
	PromptSynthetic     bool
	Tier                string // "" when the log's route record cannot vouch for one
	Target              string
	At                  time.Time
}

// Annotation is a file's lines mapped to what wrote them.
type Annotation struct {
	// Origins holds every origin at least one current line survives from.
	Origins []Origin

	// Lines has one entry per line of the file as it is on disk: an index
	// into Origins, or -1 for a line no recorded call explains.
	Lines []int

	// Unplaced counts recorded edits the replay could not apply — the file
	// had been changed outside the record by the time they ran. Their
	// lines read as outside the record rather than being guessed at.
	Unplaced int
}

// Annotate replays edits, oldest first, and aligns the result against the
// file's current bytes. Lines the replay explains carry their origin;
// everything else is -1.
func Annotate(disk []byte, edits []session.FileEdit) Annotation {
	var (
		text    string
		tags    []int
		origins []Origin
		index   = map[string]int{}
		ann     Annotation
	)

	for _, e := range edits {
		var next string
		if e.Write {
			next = e.Content
		} else {
			count := strings.Count(text, e.Old)
			switch {
			case count == 1 && !e.ReplaceAll:
				next = strings.Replace(text, e.Old, e.New, 1)
			case count >= 1 && e.ReplaceAll:
				next = strings.ReplaceAll(text, e.Old, e.New)
			default:
				// No match — the edit ran before any recorded write, or
				// the file drifted outside the record — or several where
				// the real call had exactly one. Either way the record
				// cannot say where these bytes landed.
				ann.Unplaced++
				continue
			}
		}

		key := e.SessionID + "#" + strconv.Itoa(e.Turn) + "@" + e.Target
		origin, ok := index[key]
		if !ok {
			origin = len(origins)
			index[key] = origin
			origins = append(origins, Origin{
				SessionID:           e.SessionID,
				Turn:                e.Turn,
				Prompt:              e.Prompt,
				PromptAuthoredKnown: e.PromptAuthoredKnown,
				PromptSynthetic:     e.PromptSynthetic,
				Tier:                e.Tier,
				Target:              e.Target,
				At:                  e.At,
			})
		}

		oldLines, newLines := splitLines(text), splitLines(next)
		matches := align(oldLines, newLines)
		newTags := make([]int, len(newLines))
		for i, from := range matches {
			if from >= 0 {
				newTags[i] = tags[from]
			} else {
				newTags[i] = origin
			}
		}
		text, tags = next, newTags
	}

	diskLines := splitLines(string(disk))
	ann.Lines = make([]int, len(diskLines))
	matches := align(splitLines(text), diskLines)

	// Only origins a current line survives from make the cut; the rest
	// were churn the session itself overwrote.
	remap := make([]int, len(origins))
	for i := range remap {
		remap[i] = -1
	}
	for i, from := range matches {
		if from < 0 {
			ann.Lines[i] = -1
			continue
		}
		origin := tags[from]
		if remap[origin] < 0 {
			remap[origin] = len(ann.Origins)
			ann.Origins = append(ann.Origins, origins[origin])
		}
		ann.Lines[i] = remap[origin]
	}
	return ann
}

// splitLines drops the empty element a trailing newline leaves, so a file
// ending in "\n" is its lines, not its lines plus a phantom.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
