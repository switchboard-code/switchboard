package schedule

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
)

func migrationEntry(prompt string) Entry {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	return Entry{
		ID:       "s1",
		Every:    time.Hour,
		Prompt:   prompt,
		Created:  now,
		NextFire: now.Add(time.Hour),
	}
}

func writeMigrationLedger(t *testing.T, dir string, entries []Entry) string {
	t.Helper()
	path := filepath.Join(dir, FileName)
	if err := writeLedgerFile(path, entries, false); err != nil {
		t.Fatal(err)
	}
	return path
}

func readMigrationBytes(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func migrationArtifactsOnDisk(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var artifacts []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), migrationQuarantinePrefix) {
			artifacts = append(artifacts, filepath.Join(dir, entry.Name()))
		}
	}
	return artifacts
}

func TestOpenMigratingPublishesThenRemovesHistoricalLedgerUnderBothLocks(t *testing.T) {
	canonicalDir := t.TempDir()
	historicalDir := t.TempDir()
	historicalPath := writeMigrationLedger(t, historicalDir, []Entry{migrationEntry("keep this reminder")})
	canonicalPath := filepath.Join(canonicalDir, FileName)

	validated := 0
	ledger, err := OpenMigrating(canonicalDir, []string{historicalDir}, func(got string) error {
		validated++
		if got != historicalDir {
			t.Fatalf("validator dir = %q, want %q", got, historicalDir)
		}
		if second, lockErr := Open(canonicalDir); !errors.Is(lockErr, ErrLocked) {
			if second != nil {
				second.Close()
			}
			t.Fatalf("canonical ledger was not locked during validation: %v", lockErr)
		}
		if second, lockErr := Open(historicalDir); !errors.Is(lockErr, ErrLocked) {
			if second != nil {
				second.Close()
			}
			t.Fatalf("historical ledger was not locked during validation: %v", lockErr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	if validated != 1 {
		t.Fatalf("identity validation calls = %d, want 1", validated)
	}
	got := ledger.List()
	if len(got) != 1 || got[0].Prompt != "keep this reminder" {
		t.Fatalf("migrated ledger = %+v", got)
	}
	if _, err := os.Stat(canonicalPath); err != nil {
		t.Fatalf("canonical ledger was not published: %v", err)
	}
	if _, err := os.Stat(historicalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("historical ledger survived completed migration: %v", err)
	}
	// Only the canonical lifetime lock is retained after migration.
	historical, err := Open(historicalDir)
	if err != nil {
		t.Fatalf("historical lock was not released: %v", err)
	}
	historical.Close()
}

func TestOpenMigratingRefusesNonEquivalentExistingLedgersWithoutMutation(t *testing.T) {
	canonicalDir := t.TempDir()
	historicalDir := t.TempDir()
	canonicalPath := writeMigrationLedger(t, canonicalDir, []Entry{})
	historicalPath := writeMigrationLedger(t, historicalDir, []Entry{migrationEntry("do not silently merge")})
	beforeCanonical := readMigrationBytes(t, canonicalPath)
	beforeHistorical := readMigrationBytes(t, historicalPath)

	ledger, err := OpenMigrating(canonicalDir, []string{historicalDir}, func(string) error { return nil })
	if ledger != nil {
		ledger.Close()
	}
	if !errors.Is(err, ErrMigrationConflict) || !strings.Contains(err.Error(), canonicalPath) || !strings.Contains(err.Error(), historicalPath) {
		t.Fatalf("migration error = %v, want named ErrMigrationConflict with both paths", err)
	}
	if got := readMigrationBytes(t, canonicalPath); string(got) != string(beforeCanonical) {
		t.Fatal("conflict rewrote the canonical ledger")
	}
	if got := readMigrationBytes(t, historicalPath); string(got) != string(beforeHistorical) {
		t.Fatal("conflict rewrote the historical ledger")
	}
}

func TestOpenMigratingCollapsesEquivalentExistingLedgers(t *testing.T) {
	canonicalDir := t.TempDir()
	historicalDir := t.TempDir()
	entry := migrationEntry("same reminder")
	writeMigrationLedger(t, canonicalDir, []Entry{entry})
	historicalPath := filepath.Join(historicalDir, FileName)
	// Whitespace is not a semantic conflict.
	if err := os.WriteFile(historicalPath, []byte("[\n  {\n    \"id\": \"s1\",\n    \"every\": 3600000000000,\n    \"prompt\": \"same reminder\",\n    \"created\": \"2026-08-24T12:00:00Z\",\n    \"next_fire\": \"2026-08-24T13:00:00Z\"\n  }\n]"), 0o600); err != nil {
		t.Fatal(err)
	}

	ledger, err := OpenMigrating(canonicalDir, []string{historicalDir}, func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	if got := ledger.List(); len(got) != 1 || got[0].Prompt != entry.Prompt {
		t.Fatalf("equivalent migration ledger = %+v", got)
	}
	if _, err := os.Stat(historicalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("equivalent historical duplicate was not removed: %v", err)
	}
}

func TestOpenMigratingRetainsHistoricalWhenCanonicalChangesBeforeCleanup(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(t *testing.T, path string, replacement []byte)
		backed bool
	}{
		{
			name: "same inode",
			mutate: func(t *testing.T, path string, replacement []byte) {
				t.Helper()
				if err := os.WriteFile(path, replacement, 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "replacement inode",
			mutate: func(t *testing.T, path string, replacement []byte) {
				t.Helper()
				if err := os.Rename(path, path+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, replacement, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			backed: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			canonicalDir := t.TempDir()
			historicalDir := t.TempDir()
			originalEntries := []Entry{migrationEntry("equivalent original")}
			canonicalPath := writeMigrationLedger(t, canonicalDir, originalEntries)
			historicalPath := writeMigrationLedger(t, historicalDir, originalEntries)
			original := readMigrationBytes(t, historicalPath)
			replacement, err := json.MarshalIndent([]Entry{migrationEntry("ignoring writer")}, "", "  ")
			if err != nil {
				t.Fatal(err)
			}

			mutated := false
			migrationCanonicalBeforeVerifyTestHook = func(gotCanonical, gotHistorical string) error {
				if gotCanonical != canonicalPath || gotHistorical != historicalPath {
					t.Fatalf("canonical revalidation hook = %q, %q; want %q, %q", gotCanonical, gotHistorical, canonicalPath, historicalPath)
				}
				if !mutated {
					mutated = true
					test.mutate(t, canonicalPath, replacement)
				}
				return nil
			}
			t.Cleanup(func() { migrationCanonicalBeforeVerifyTestHook = nil })

			ledger, err := OpenMigrating(canonicalDir, []string{historicalDir}, func(string) error { return nil })
			if ledger != nil {
				ledger.Close()
			}
			if !mutated || !errors.Is(err, ErrMigrationConflict) {
				t.Fatalf("migration after canonical mutation = mutated %v, %v; want conflict", mutated, err)
			}
			if got := readMigrationBytes(t, historicalPath); !bytes.Equal(got, original) {
				t.Fatalf("canonical mutation removed or changed historical copy: %q", got)
			}
			if got := readMigrationBytes(t, canonicalPath); !bytes.Equal(got, replacement) {
				t.Fatalf("canonical replacement was overwritten: %q", got)
			}
			if test.backed {
				if got := readMigrationBytes(t, canonicalPath+".original"); !bytes.Equal(got, original) {
					t.Fatalf("replaced canonical preimage was changed: %q", got)
				}
			}
			if artifacts := migrationArtifactsOnDisk(t, historicalDir); len(artifacts) != 0 {
				t.Fatalf("canonical conflict left migration artifacts: %v", artifacts)
			}
		})
	}
}

func TestOpenMigratingRequiresIdentityProofAndPreservesSourceOnFailure(t *testing.T) {
	canonicalDir := t.TempDir()
	historicalDir := t.TempDir()
	historicalPath := writeMigrationLedger(t, historicalDir, []Entry{migrationEntry("identity first")})
	proofErr := errors.New("alias was retargeted")

	for name, validate := range map[string]MigrationValidator{
		"absent": nil,
		"failed": func(string) error { return proofErr },
	} {
		t.Run(name, func(t *testing.T) {
			ledger, err := OpenMigrating(canonicalDir, []string{historicalDir}, validate)
			if ledger != nil {
				ledger.Close()
			}
			if err == nil {
				t.Fatal("migration without live identity proof succeeded")
			}
			if _, err := os.Stat(filepath.Join(canonicalDir, FileName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed proof published a canonical ledger: %v", err)
			}
			if _, err := os.Stat(historicalPath); err != nil {
				t.Fatalf("failed proof removed the historical ledger: %v", err)
			}
		})
	}
}

func TestOpenMigratingRespectsHistoricalOwnerLock(t *testing.T) {
	canonicalDir := t.TempDir()
	historicalDir := t.TempDir()
	historicalPath := writeMigrationLedger(t, historicalDir, []Entry{migrationEntry("one poller")})
	owner, err := Open(historicalDir)
	if err != nil {
		t.Fatal(err)
	}

	ledger, err := OpenMigrating(canonicalDir, []string{historicalDir}, func(string) error { return nil })
	if ledger != nil {
		ledger.Close()
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("migration while historical owner runs = %v, want ErrLocked", err)
	}
	if _, err := os.Stat(filepath.Join(canonicalDir, FileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("locked migration published canonical state: %v", err)
	}
	if _, err := os.Stat(historicalPath); err != nil {
		t.Fatalf("locked migration removed source: %v", err)
	}
	owner.Close()

	migrated, err := OpenMigrating(canonicalDir, []string{historicalDir}, func(string) error { return nil })
	if err != nil {
		t.Fatalf("migration after owner close: %v", err)
	}
	migrated.Close()
}

func TestOpenMigratingFinalGapReplacementIsRestoredWithoutOverwrite(t *testing.T) {
	canonicalDir := t.TempDir()
	historicalDir := t.TempDir()
	historicalPath := writeMigrationLedger(t, historicalDir, []Entry{migrationEntry("read image")})
	replacementBytes, err := json.MarshalIndent([]Entry{migrationEntry("replacement image")}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	backupPath := historicalPath + ".read-image"

	migrationCleanupTestHook = func(boundary migrationCleanupBoundary, source, _ string) error {
		if boundary != migrationBeforeQuarantine {
			return nil
		}
		if err := os.Rename(source, backupPath); err != nil {
			return err
		}
		return os.WriteFile(source, replacementBytes, 0o600)
	}
	t.Cleanup(func() { migrationCleanupTestHook = nil })

	ledger, err := OpenMigrating(canonicalDir, []string{historicalDir}, func(string) error { return nil })
	if ledger != nil {
		ledger.Close()
	}
	if !errors.Is(err, ErrMigrationConflict) {
		t.Fatalf("final-gap replacement migration = %v, want ErrMigrationConflict", err)
	}
	if got := readMigrationBytes(t, historicalPath); !bytes.Equal(got, replacementBytes) {
		t.Fatalf("replacement was not restored exactly: %q", got)
	}
	canonical, readErr := os.ReadFile(filepath.Join(canonicalDir, FileName))
	if readErr != nil || !bytes.Contains(canonical, []byte("read image")) {
		t.Fatalf("durable canonical image = %q, %v", canonical, readErr)
	}
	if artifacts := migrationArtifactsOnDisk(t, historicalDir); len(artifacts) != 0 {
		t.Fatalf("restored replacement left migration artifacts: %v", artifacts)
	}
}

func TestOpenMigratingInPlaceChangeIsNotDeleted(t *testing.T) {
	canonicalDir := t.TempDir()
	historicalDir := t.TempDir()
	historicalPath := writeMigrationLedger(t, historicalDir, []Entry{migrationEntry("read image")})
	replacementBytes, err := json.MarshalIndent([]Entry{migrationEntry("same inode, new bytes")}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	migrationCleanupTestHook = func(boundary migrationCleanupBoundary, source, _ string) error {
		if boundary == migrationBeforeQuarantine {
			return os.WriteFile(source, replacementBytes, 0o600)
		}
		return nil
	}
	t.Cleanup(func() { migrationCleanupTestHook = nil })

	ledger, err := OpenMigrating(canonicalDir, []string{historicalDir}, func(string) error { return nil })
	if ledger != nil {
		ledger.Close()
	}
	if !errors.Is(err, ErrMigrationConflict) {
		t.Fatalf("in-place replacement migration = %v, want ErrMigrationConflict", err)
	}
	if got := readMigrationBytes(t, historicalPath); !bytes.Equal(got, replacementBytes) {
		t.Fatalf("in-place replacement was deleted or changed: %q", got)
	}
}

func TestOpenMigratingQuarantineReplacementIsRestoredNotDeleted(t *testing.T) {
	canonicalDir := t.TempDir()
	historicalDir := t.TempDir()
	historicalPath := writeMigrationLedger(t, historicalDir, []Entry{migrationEntry("verified image")})
	replacementBytes, err := json.MarshalIndent([]Entry{migrationEntry("quarantine replacement")}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	var quarantinePath string
	stolenPath := filepath.Join(historicalDir, "read-image-backup")
	migrationCleanupTestHook = func(boundary migrationCleanupBoundary, _, quarantine string) error {
		switch boundary {
		case migrationAfterQuarantine:
			quarantinePath = quarantine
		case migrationBeforeQuarantineDelete:
			if err := os.Rename(quarantinePath, stolenPath); err != nil {
				return err
			}
			return os.WriteFile(quarantinePath, replacementBytes, 0o600)
		}
		return nil
	}
	t.Cleanup(func() { migrationCleanupTestHook = nil })

	ledger, err := OpenMigrating(canonicalDir, []string{historicalDir}, func(string) error { return nil })
	if ledger != nil {
		ledger.Close()
	}
	if !errors.Is(err, ErrMigrationConflict) {
		t.Fatalf("quarantine replacement migration = %v, want ErrMigrationConflict", err)
	}
	if got := readMigrationBytes(t, historicalPath); !bytes.Equal(got, replacementBytes) {
		t.Fatalf("quarantine replacement was not restored: %q", got)
	}
	if got := readMigrationBytes(t, stolenPath); !bytes.Contains(got, []byte("verified image")) {
		t.Fatalf("read image was not retained after quarantine substitution: %q", got)
	}
	if artifacts := migrationArtifactsOnDisk(t, historicalDir); len(artifacts) != 0 {
		t.Fatalf("restored quarantine replacement left artifacts: %v", artifacts)
	}
}

func TestOpenMigratingQuarantineCollisionNeverOverwrites(t *testing.T) {
	canonicalDir := t.TempDir()
	historicalDir := t.TempDir()
	historicalPath := writeMigrationLedger(t, historicalDir, []Entry{migrationEntry("source remains")})
	const sentinel = "quarantine sentinel"
	var occupied string
	migrationCleanupTestHook = func(boundary migrationCleanupBoundary, _, quarantine string) error {
		if boundary != migrationBeforeQuarantine {
			return nil
		}
		occupied = quarantine
		return os.WriteFile(quarantine, []byte(sentinel), 0o600)
	}
	t.Cleanup(func() { migrationCleanupTestHook = nil })

	ledger, err := OpenMigrating(canonicalDir, []string{historicalDir}, func(string) error { return nil })
	if ledger != nil {
		ledger.Close()
	}
	if !errors.Is(err, ErrMigrationConflict) {
		t.Fatalf("occupied quarantine migration = %v, want ErrMigrationConflict", err)
	}
	if got := string(readMigrationBytes(t, occupied)); got != sentinel {
		t.Fatalf("occupied quarantine was overwritten: %q", got)
	}
	if _, err := os.Stat(historicalPath); err != nil {
		t.Fatalf("source changed after no-replace collision: %v", err)
	}
}

func TestOpenMigratingCrashQuarantineIsRecovered(t *testing.T) {
	canonicalDir := t.TempDir()
	historicalDir := t.TempDir()
	historicalPath := writeMigrationLedger(t, historicalDir, []Entry{migrationEntry("survive cleanup crash")})
	crashErr := errors.New("simulated process death after quarantine")
	migrationCleanupTestHook = func(boundary migrationCleanupBoundary, _, _ string) error {
		if boundary == migrationAfterQuarantine {
			return crashErr
		}
		return nil
	}

	ledger, err := OpenMigrating(canonicalDir, []string{historicalDir}, func(string) error { return nil })
	if ledger != nil {
		ledger.Close()
	}
	if !errors.Is(err, crashErr) {
		t.Fatalf("crash seam migration = %v, want injected failure", err)
	}
	if _, err := os.Stat(historicalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source should be atomically quarantined at crash seam: %v", err)
	}
	if artifacts := migrationArtifactsOnDisk(t, historicalDir); len(artifacts) != 1 {
		t.Fatalf("crash recovery artifacts = %v, want one", artifacts)
	}

	migrationCleanupTestHook = nil
	recovered, err := OpenMigrating(canonicalDir, []string{historicalDir}, func(string) error { return nil })
	if err != nil {
		t.Fatalf("recovering crash quarantine: %v", err)
	}
	defer recovered.Close()
	if got := recovered.List(); len(got) != 1 || got[0].Prompt != "survive cleanup crash" {
		t.Fatalf("recovered ledger = %+v", got)
	}
	if artifacts := migrationArtifactsOnDisk(t, historicalDir); len(artifacts) != 0 {
		t.Fatalf("completed recovery retained artifacts: %v", artifacts)
	}
}

func TestOpenMigratingRestoresUnreadableCrashArtifactWithoutOverwrite(t *testing.T) {
	canonicalDir := t.TempDir()
	historicalDir := t.TempDir()
	writeMigrationLedger(t, canonicalDir, []Entry{migrationEntry("canonical")})
	quarantineDir := filepath.Join(historicalDir, migrationQuarantinePrefix+"fixture")
	if err := os.Mkdir(quarantineDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const replacement = "not JSON, but still user state"
	if err := os.WriteFile(filepath.Join(quarantineDir, migrationQuarantineEntry), []byte(replacement), 0o600); err != nil {
		t.Fatal(err)
	}

	ledger, err := OpenMigrating(canonicalDir, []string{historicalDir}, func(string) error { return nil })
	if ledger != nil {
		ledger.Close()
	}
	if !errors.Is(err, ErrMigrationConflict) {
		t.Fatalf("unreadable artifact recovery = %v, want ErrMigrationConflict", err)
	}
	if got := string(readMigrationBytes(t, filepath.Join(historicalDir, FileName))); got != replacement {
		t.Fatalf("unreadable artifact was not restored exactly: %q", got)
	}
	if artifacts := migrationArtifactsOnDisk(t, historicalDir); len(artifacts) != 0 {
		t.Fatalf("unreadable artifact restore retained quarantine: %v", artifacts)
	}
}

func TestOpenMigratingCleanupStaysOnBoundAlias(t *testing.T) {
	for _, action := range []string{"missing", "retargeted"} {
		t.Run(action, func(t *testing.T) {
			canonicalDir := t.TempDir()
			historicalDir := t.TempDir()
			historicalPath := writeMigrationLedger(t, historicalDir, []Entry{migrationEntry("bound source")})
			externalDir := t.TempDir()
			externalPath := writeMigrationLedger(t, externalDir, []Entry{migrationEntry("external sentinel")})
			aliasParent := t.TempDir()
			alias := filepath.Join(aliasParent, "historical-alias")
			if err := os.Symlink(historicalDir, alias); err != nil {
				t.Skipf("symlink setup unavailable: %v", err)
			}
			migrationCleanupTestHook = func(boundary migrationCleanupBoundary, _, _ string) error {
				if boundary != migrationBeforeQuarantine {
					return nil
				}
				if err := os.Remove(alias); err != nil {
					return err
				}
				if action == "retargeted" {
					return os.Symlink(externalDir, alias)
				}
				return nil
			}
			t.Cleanup(func() { migrationCleanupTestHook = nil })

			ledger, err := OpenMigrating(canonicalDir, []string{alias}, func(string) error { return nil })
			if err != nil {
				t.Fatalf("migration through %s alias: %v", action, err)
			}
			ledger.Close()
			if _, err := os.Stat(historicalPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("bound historical source survived cleanup: %v", err)
			}
			if got := readMigrationBytes(t, externalPath); !bytes.Contains(got, []byte("external sentinel")) {
				t.Fatalf("external retarget was changed: %q", got)
			}
		})
	}
}

func TestOpenMigratingCleanupCannotFollowDirectoryReplacement(t *testing.T) {
	canonicalDir := t.TempDir()
	parent := t.TempDir()
	historicalDir := filepath.Join(parent, "historical")
	if err := os.Mkdir(historicalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeMigrationLedger(t, historicalDir, []Entry{migrationEntry("bound directory")})
	movedDir := filepath.Join(parent, "historical-bound")
	replacementBytes, err := json.MarshalIndent([]Entry{migrationEntry("replacement directory")}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	migrationCleanupTestHook = func(boundary migrationCleanupBoundary, _, _ string) error {
		if boundary != migrationBeforeQuarantine {
			return nil
		}
		if err := os.Rename(historicalDir, movedDir); err != nil {
			return err
		}
		if err := os.Mkdir(historicalDir, 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(historicalDir, FileName), replacementBytes, 0o600)
	}
	t.Cleanup(func() { migrationCleanupTestHook = nil })

	ledger, err := OpenMigrating(canonicalDir, []string{historicalDir}, func(string) error { return nil })
	if err != nil {
		t.Fatalf("migration across directory replacement: %v", err)
	}
	ledger.Close()
	if got := readMigrationBytes(t, filepath.Join(historicalDir, FileName)); !bytes.Equal(got, replacementBytes) {
		t.Fatalf("replacement directory ledger was changed: %q", got)
	}
	if _, err := os.Stat(filepath.Join(movedDir, FileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bound historical ledger survived completed migration: %v", err)
	}
}

func TestOpenMigratingLiveProofRefusesMissingOrRetargetedAlias(t *testing.T) {
	for _, action := range []string{"missing", "retargeted"} {
		t.Run(action, func(t *testing.T) {
			canonicalDir := t.TempDir()
			historicalDir := t.TempDir()
			historicalPath := writeMigrationLedger(t, historicalDir, []Entry{migrationEntry("proof required")})
			expected, err := os.Stat(historicalDir)
			if err != nil {
				t.Fatal(err)
			}
			externalDir := t.TempDir()
			alias := filepath.Join(t.TempDir(), "historical-alias")
			if err := os.Symlink(historicalDir, alias); err != nil {
				t.Skipf("symlink setup unavailable: %v", err)
			}
			proofErr := errors.New("historical alias identity changed")
			ledger, openErr := OpenMigrating(canonicalDir, []string{alias}, func(got string) error {
				if got != alias {
					return fmt.Errorf("validator path = %q, want %q", got, alias)
				}
				if err := os.Remove(alias); err != nil {
					return err
				}
				if action == "retargeted" {
					if err := os.Symlink(externalDir, alias); err != nil {
						return err
					}
				}
				current, err := os.Stat(alias)
				if err != nil || !os.SameFile(expected, current) {
					return errors.Join(proofErr, err)
				}
				return nil
			})
			if ledger != nil {
				ledger.Close()
			}
			if !errors.Is(openErr, proofErr) {
				t.Fatalf("%s alias proof = %v, want proof failure", action, openErr)
			}
			if _, err := os.Stat(historicalPath); err != nil {
				t.Fatalf("failed identity proof changed historical state: %v", err)
			}
			if _, err := os.Stat(filepath.Join(canonicalDir, FileName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed identity proof published canonical state: %v", err)
			}
		})
	}
}
