//go:build windows

package checkpoint

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsRecorderNormalizesRequestedModesToFilesystemModes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewRecorder()
	r.Begin("edit")
	r.RecordState(path, true, 0o644, []byte("before"))
	if err := os.WriteFile(path, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.Commit(path, true, 0o644, sha256.Sum256([]byte("after")))
	if restored, _, _, failed, _, err := r.Undo(); err != nil || len(failed) != 0 || len(restored) != 1 {
		t.Fatalf("undo restored=%v failed=%v err=%v", restored, failed, err)
	}
	if body, err := os.ReadFile(path); err != nil || string(body) != "before" {
		t.Fatalf("restored body=%q err=%v", body, err)
	}
}

func TestWindowsDurableFingerprintAcceptsLegacyPermissionMask(t *testing.T) {
	digest := sha256.Sum256([]byte("x"))
	value := durableFileFingerprint{
		Existed: true,
		Mode:    0o644,
		Size:    1,
		Digest:  fmt.Sprintf("%x", digest),
	}
	decoded, err := value.decode()
	if err != nil {
		t.Fatal(err)
	}
	if decoded.mode != 0o666 {
		t.Fatalf("legacy mode normalized to %o, want 666", decoded.mode)
	}
	value.Mode = 1 << 31
	if _, err := value.decode(); err == nil {
		t.Fatal("durable fingerprint accepted unsupported type bits")
	}
}
