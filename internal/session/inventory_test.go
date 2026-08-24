package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestReadSessionDirectoryExactLimitAndDeterministicOrder(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"z", "c", "a", "m", "b", "x", "d", "q"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := readSessionDirectory(dir, 8)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(entries); i++ {
		if entries[i-1].Name() >= entries[i].Name() {
			t.Fatalf("inventory is not deterministically sorted: %v", entries)
		}
	}
}

func TestReadSessionDirectoryRefusesLimitPlusOneWithoutPartialInventory(t *testing.T) {
	dir := t.TempDir()
	for i := range 9 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("entry-%02d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := readSessionDirectory(dir, 8)
	if !errors.Is(err, ErrSessionInventoryTooLarge) {
		t.Fatalf("limit+1 error = %v", err)
	}
	if entries != nil {
		t.Fatalf("over-limit inventory returned a partial result: %v", entries)
	}
}

func TestListToleratesManyBoundedIrrelevantEntries(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 512 {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("irrelevant-%04d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	workspace := t.TempDir()
	infos, err := store.List(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 0 {
		t.Fatalf("irrelevant entries produced sessions: %+v", infos)
	}
}
