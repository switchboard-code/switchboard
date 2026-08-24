//go:build unix

package schedule

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

const (
	scheduleCrashHelperEnv     = "SWITCHBOARD_SCHEDULE_CRASH_HELPER"
	scheduleCrashCanonicalEnv  = "SWITCHBOARD_SCHEDULE_CRASH_CANONICAL"
	scheduleCrashHistoricalEnv = "SWITCHBOARD_SCHEDULE_CRASH_HISTORICAL"
	scheduleCrashMarkerEnv     = "SWITCHBOARD_SCHEDULE_CRASH_MARKER"
)

func TestOpenMigratingRecoversSIGKILLAfterQuarantine(t *testing.T) {
	canonicalDir := t.TempDir()
	historicalDir := t.TempDir()
	historicalPath := writeMigrationLedger(t, historicalDir, []Entry{migrationEntry("survive SIGKILL")})
	marker := filepath.Join(t.TempDir(), "quarantined")
	command := exec.Command(os.Args[0], "-test.run=^TestScheduleMigrationCrashHelper$")
	command.Env = append(os.Environ(),
		scheduleCrashHelperEnv+"=1",
		scheduleCrashCanonicalEnv+"="+canonicalDir,
		scheduleCrashHistoricalEnv+"="+historicalDir,
		scheduleCrashMarkerEnv+"="+marker,
	)
	err := command.Run()
	if err == nil {
		t.Fatal("crash helper returned normally")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("helper did not reach durable quarantine seam: %v", err)
	}
	if _, err := os.Stat(historicalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SIGKILL seam left the historical source name: %v", err)
	}
	if artifacts := migrationArtifactsOnDisk(t, historicalDir); len(artifacts) != 1 {
		t.Fatalf("SIGKILL artifacts = %v, want one", artifacts)
	}

	recovered, err := OpenMigrating(canonicalDir, []string{historicalDir}, func(string) error { return nil })
	if err != nil {
		t.Fatalf("recovering SIGKILL quarantine: %v", err)
	}
	defer recovered.Close()
	if got := recovered.List(); len(got) != 1 || got[0].Prompt != "survive SIGKILL" {
		t.Fatalf("SIGKILL recovery ledger = %+v", got)
	}
	if artifacts := migrationArtifactsOnDisk(t, historicalDir); len(artifacts) != 0 {
		t.Fatalf("SIGKILL recovery retained artifacts: %v", artifacts)
	}
}

func TestScheduleMigrationCrashHelper(t *testing.T) {
	if os.Getenv(scheduleCrashHelperEnv) != "1" {
		t.Skip("helper process only")
	}
	canonicalDir := os.Getenv(scheduleCrashCanonicalEnv)
	historicalDir := os.Getenv(scheduleCrashHistoricalEnv)
	marker := os.Getenv(scheduleCrashMarkerEnv)
	migrationCleanupTestHook = func(boundary migrationCleanupBoundary, _, _ string) error {
		if boundary != migrationAfterQuarantine {
			return nil
		}
		if err := os.WriteFile(marker, []byte("quarantined"), 0o600); err != nil {
			return err
		}
		if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
			return err
		}
		return errors.New("SIGKILL returned")
	}
	if _, err := OpenMigrating(canonicalDir, []string{historicalDir}, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	t.Fatal("migration completed past crash seam")
}
