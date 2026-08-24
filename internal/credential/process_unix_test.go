//go:build unix

package credential

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const credentialHelperCancelTimeout = 150 * time.Millisecond

// pipeHoldingCredentialHelper exits its direct process immediately but leaves
// a descendant holding stdout and stderr. That is the shape that made
// exec.CommandContext wait forever after it had already killed (or observed
// the exit of) the direct child.
func pipeHoldingCredentialHelper(t *testing.T, exitCode int) (string, string) {
	t.Helper()
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "descendant.pid")
	helper := filepath.Join(dir, "credential-helper")
	body := fmt.Sprintf(`#!/bin/sh
/bin/sh -c 'trap "exit 0" TERM INT HUP; sleep 30' &
child=$!
printf '%%s\n' "$child" > '%s'
exit %d
`, strings.ReplaceAll(pidPath, "'", "'\\''"), exitCode)
	if err := os.WriteFile(helper, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return helper, pidPath
}

func overflowingCredentialHelper(t *testing.T) string {
	t.Helper()
	helper := filepath.Join(t.TempDir(), "credential-helper")
	body := "#!/bin/sh\nprintf '%s' '" + strings.Repeat("s", maxHelperCaptureBytes+1) + "'\n"
	if err := os.WriteFile(helper, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return helper
}

func assertCredentialHelperCanceled(t *testing.T, pidPath string, started time.Time, err error) {
	t.Helper()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("credential helper error = %v, want context deadline", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("credential helper cancellation was misreported as a miss: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Fatalf("credential helper cancellation took %s; retained-pipe cleanup was not bounded", elapsed)
	}

	rawPID, readErr := os.ReadFile(pidPath)
	if readErr != nil {
		t.Fatalf("pipe-holding descendant did not publish its pid: %v", readErr)
	}
	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(rawPID)))
	if parseErr != nil || pid <= 0 {
		t.Fatalf("invalid pipe-holding descendant pid %q: %v", rawPID, parseErr)
	}
	gone := false
	t.Cleanup(func() {
		if !gone {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	deadline := time.Now().Add(2 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			gone = true
			return
		}
		if err != nil {
			t.Fatalf("checking pipe-holding descendant %d: %v", pid, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("pipe-holding credential helper descendant %d survived cancellation", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHelperCancellationStopsPipeHoldingDescendant(t *testing.T) {
	helper, pidPath := pipeHoldingCredentialHelper(t, 0)
	store := &HelperStore{Command: []string{helper}}
	ctx, cancel := context.WithTimeout(context.Background(), credentialHelperCancelTimeout)
	defer cancel()

	started := time.Now()
	_, err := store.Get(ctx, Ref{Provider: "anthropic"})
	assertCredentialHelperCanceled(t, pidPath, started, err)
}
