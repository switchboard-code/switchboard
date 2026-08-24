//go:build unix

package eval

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

func TestTaskVerifierRefusesRequiredFileFIFOWithoutBlocking(t *testing.T) {
	source := t.TempDir()
	for name, body := range map[string]string{
		"go.mod":     "module example.com/switchboard/fifoverifier\n\ngo 1.20\n",
		"fixture.go": "package fixture\n",
		"required":   "needle\n",
	} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	task := taskFor(source, spec{
		id: "fifo-verifier", pkg: "./", mustContain: map[string]string{"required": "needle"},
	})
	if err := task.Setup(dir); err != nil {
		t.Fatal(err)
	}
	required := filepath.Join(dir, "required")
	if err := os.Remove(required); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(required, 0o600); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	_, _, err := task.Verify(context.Background(), dir)
	if !errors.Is(err, rootedfs.ErrNotRegular) {
		t.Fatalf("FIFO verifier error = %v, want non-regular refusal", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("FIFO verifier blocked for %s", elapsed)
	}
}
