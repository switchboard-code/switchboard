//go:build unix

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/rootedfs"
	"golang.org/x/sys/unix"
)

func TestRepoLanguagesRootSwapCannotHangOrReadReplacement(t *testing.T) {
	limits := rootedfs.WalkLimits{MaxEntries: 16, MaxDirectories: 4, MaxDepth: 4, ReadDirBatch: 2}
	for _, replacement := range []string{"fifo", "symlink"} {
		t.Run(replacement, func(t *testing.T) {
			parent := t.TempDir()
			workspace := filepath.Join(parent, "workspace")
			outside := filepath.Join(parent, "outside")
			if err := os.Mkdir(workspace, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(outside, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(workspace, "inside.go"), []byte("package inside"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(outside, "outside.py"), []byte("outside"), 0o600); err != nil {
				t.Fatal(err)
			}

			type outcome struct {
				languages []string
				err       error
			}
			done := make(chan outcome, 1)
			go func() {
				var swapErr error
				languages := repoLanguagesWithLimits(workspace, limits, func(real string) {
					moved := real + "-moved"
					if swapErr = os.Rename(real, moved); swapErr != nil {
						return
					}
					if replacement == "fifo" {
						swapErr = unix.Mkfifo(real, 0o600)
					} else {
						swapErr = os.Symlink(outside, real)
					}
				})
				done <- outcome{languages: languages, err: swapErr}
			}()
			select {
			case got := <-done:
				if got.err != nil {
					t.Fatal(got.err)
				}
				if got.languages != nil {
					t.Fatalf("replacement root supplied routing evidence: %v", got.languages)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("language traversal blocked on a replaced workspace root")
			}
		})
	}
}
