package schedule

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestReadRootDirBoundedExactLimitAndOrdering(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"z", "c", "a", "m", "b", "x", "d", "q"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	entries, err := readRootDirBounded(root, 8)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(entries); i++ {
		if entries[i-1].Name() >= entries[i].Name() {
			t.Fatalf("schedule inventory is not sorted: %v", entries)
		}
	}
}

func TestReadRootDirBoundedRefusesLimitPlusOne(t *testing.T) {
	dir := t.TempDir()
	for i := range 9 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("entry-%02d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	entries, err := readRootDirBounded(root, 8)
	if !errors.Is(err, errScheduleInventoryTooLarge) {
		t.Fatalf("limit+1 error = %v", err)
	}
	if entries != nil {
		t.Fatalf("over-limit schedule inventory returned a partial result: %v", entries)
	}
}

func TestOpenToleratesManyBoundedIrrelevantStateEntries(t *testing.T) {
	dir := t.TempDir()
	for i := range 512 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("irrelevant-%04d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
}
