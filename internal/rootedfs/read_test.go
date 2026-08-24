package rootedfs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadFileRefusesOversizeWithoutReturningPrefix(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "large")
	if err := os.WriteFile(path, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := ReadFile(root, "large", 4)
	if !errors.Is(err, ErrTooLarge) || len(data) != 0 {
		t.Fatalf("oversize read = %q, %v", data, err)
	}
}
