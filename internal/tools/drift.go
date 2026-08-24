package tools

// Read drift: a file the model was shown, changed by something else since.
//
// The registry already hashes every file the model reads, because write and
// edit refuse to touch a file that moved since it was read (§6.7). That check
// fires at the worst moment — the model has finished deciding, composed an
// edit, and spent a round to be told its picture was stale. The same evidence
// answers the question a round earlier, and a round earlier is the difference
// between a correction and a wasted turn.
//
// Only what the tracker already holds is used. A file the model never read is
// not watched, because nothing is known about what it looked like; a file the
// model itself wrote is not drift, because the write path records the new hash
// as it commits. What is left is exactly the interesting case: a formatter, a
// checkout, a build step, another window.
//
// The scan is bounded twice and stat-first. A file whose size and modification
// time are unchanged is not read at all, so the ordinary case costs one stat
// per tracked file; only a suspect file is hashed, and one over the size cap is
// reported as unreadable rather than hashed at any price. A drift is reported
// once and then held until the bytes move again, because the same sentence at
// every round boundary is noise the model learns to skip.

import (
	"os"
	"sort"
	"time"
)

const (
	// maxDriftFiles bounds the stat sweep. A session that has read more files
	// than this has a context problem the tracker cannot fix, and the sweep
	// runs on the loop's goroutine between rounds.
	maxDriftFiles = 128

	// maxDriftHashBytes bounds what is re-hashed on suspicion. Past it the
	// change is reported as unverified rather than paid for.
	maxDriftHashBytes = 4 << 20
)

// DriftedRead is one file the model was shown whose bytes have since changed.
type DriftedRead struct {
	// Path is the display path, workspace-relative where it can be.
	Path string

	// Gone marks a file that no longer exists. Deletion is drift too, and the
	// most confusing kind to meet through a failed edit.
	Gone bool

	// Unverified marks a file that looks changed by size or timestamp but is
	// too large to re-hash. Saying which it is keeps the report honest: this
	// one may have been touched rather than edited.
	Unverified bool
}

// readStamp is what a read recorded about the file on disk, so a later sweep
// can tell "certainly unchanged" from "worth hashing" without reading it.
type readStamp struct {
	size    int64
	modTime time.Time
}

// DriftedReads reports files read this session whose contents no longer match
// what the model was shown, each at most once per change.
//
// It never mutates the stale-check hashes. Those are write and edit's evidence
// and belong to the moment the model was actually shown the bytes; a reporter
// that quietly refreshed them would disarm the refusal that catches the same
// problem at the point it matters most.
func (r *Registry) DriftedReads() []DriftedRead {
	r.versions.mu.Lock()
	tracked := make([]string, 0, len(r.versions.seen))
	for path := range r.versions.seen {
		tracked = append(tracked, path)
	}
	r.versions.mu.Unlock()

	// Sorted so a capped sweep covers the same files run to run rather than
	// whichever the map handed back first.
	sort.Strings(tracked)
	if len(tracked) > maxDriftFiles {
		tracked = tracked[:maxDriftFiles]
	}

	var out []DriftedRead
	for _, abs := range tracked {
		drift, changed := r.driftOf(abs)
		if !changed {
			continue
		}
		drift.Path = r.display(abs)
		out = append(out, drift)
	}
	return out
}

// driftOf decides one file, and records what it decided so the same change is
// not reported twice.
func (r *Registry) driftOf(abs string) (DriftedRead, bool) {
	return r.driftOfWithHook(abs, nil)
}

func (r *Registry) driftOfWithHook(abs string, beforeOpen func()) (DriftedRead, bool) {
	v := r.versions
	v.mu.Lock()
	defer v.mu.Unlock()

	shown, tracked := v.seen[abs]
	if !tracked {
		return DriftedRead{}, false
	}

	root, relative, err := r.openResolvedWorkspace(abs)
	if err != nil {
		return DriftedRead{}, false
	}
	defer root.Close()
	info, err := root.Lstat(relative)
	if err != nil {
		if v.reported[abs] == goneMarker {
			return DriftedRead{}, false
		}
		v.reported[abs] = goneMarker
		return DriftedRead{Gone: true}, true
	}
	if !info.Mode().IsRegular() {
		return DriftedRead{}, false
	}
	if err := r.verifyWorkspaceRoot(root); err != nil {
		return DriftedRead{}, false
	}

	// The cheap gate. A file whose size and timestamp match what the read saw
	// is not opened, which is what keeps this affordable at every boundary.
	if stamp, ok := v.stamps[abs]; ok && stamp.size == info.Size() && stamp.modTime.Equal(info.ModTime()) {
		return DriftedRead{}, false
	}

	if info.Size() > maxDriftHashBytes {
		marker := unverifiedMarker(info)
		if v.reported[abs] == marker {
			return DriftedRead{}, false
		}
		v.reported[abs] = marker
		return DriftedRead{Unverified: true}, true
	}

	data, _, err := readRegularWorkspaceFile(root, relative, r.display(abs), maxDriftHashBytes, beforeOpen)
	if err != nil {
		return DriftedRead{}, false
	}
	if err := r.verifyWorkspaceRoot(root); err != nil {
		return DriftedRead{}, false
	}
	current := hashContent(data)
	if current == shown {
		// Touched but not changed. Refresh the stamp so the next sweep is a
		// stat again rather than another read of the same bytes.
		v.stamps[abs] = readStamp{size: info.Size(), modTime: info.ModTime()}
		return DriftedRead{}, false
	}
	if v.reported[abs] == current {
		return DriftedRead{}, false
	}
	v.reported[abs] = current
	return DriftedRead{}, true
}

// goneMarker stands for "reported as deleted" in the reported map, where every
// other value is a content hash. A hash is hex, so this cannot collide with one.
const goneMarker = "gone"

func unverifiedMarker(info os.FileInfo) string {
	return "unverified:" + info.ModTime().UTC().Format(time.RFC3339Nano)
}
