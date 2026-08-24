package credential

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeSecurity stands in for the system command and records exactly what it was
// invoked with. It is a shell script rather than a Go test binary re-exec so
// that the recorded argv is the real argv the operating system saw.
func fakeSecurity(t *testing.T) (store *OSStore, argvLog string) {
	t.Helper()
	dir := t.TempDir()
	argvLog = filepath.Join(dir, "argv")
	vault := filepath.Join(dir, "vault")

	// Writes go through the tool's interactive mode, where the command arrives
	// on standard input and the value is a quoted argument. Reads and deletes
	// still take arguments, because neither carries a secret.
	//
	// The parser is modelled with `eval`, which reproduces the two behaviours
	// that were measured on the real tool: it splits on unquoted spaces and
	// unescapes backslashes. A fake that just took the rest of the line would
	// pass whether or not the quoting is right.
	body := `#!/bin/sh
printf '%s\n' "$*" >> ` + argvLog + `
if [ "$1" = "-i" ]; then
  read -r line
  eval "set -- $line"
  while [ $# -gt 0 ]; do
    case "$1" in
      -w) shift; printf '%s' "$1" > ` + vault + `; exit 0 ;;
      *) shift ;;
    esac
  done
  exit 1
fi
case "$1" in
  find-generic-password)
    [ -f ` + vault + ` ] || exit 44
    cat ` + vault + `
    printf '\n'
    ;;
  delete-generic-password)
    [ -f ` + vault + ` ] || exit 44
    rm -f ` + vault + `
    ;;
esac
`
	path := filepath.Join(dir, "security")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return &OSStore{bin: path}, argvLog
}

func TestKeychainGetCancellationStopsPipeHoldingDescendant(t *testing.T) {
	helper, pidPath := pipeHoldingCredentialHelper(t, notFoundStatus)
	store := &OSStore{bin: helper}
	ctx, cancel := context.WithTimeout(context.Background(), credentialHelperCancelTimeout)
	defer cancel()

	started := time.Now()
	_, err := store.Get(ctx, Ref{Provider: "anthropic"})
	assertCredentialHelperCanceled(t, pidPath, started, err)
}

func TestKeychainWithholdsOversizedCredentialOutput(t *testing.T) {
	store := &OSStore{bin: overflowingCredentialHelper(t)}
	_, err := store.Get(context.Background(), Ref{Provider: "anthropic"})
	if err == nil || !strings.Contains(err.Error(), "output withheld") || strings.Contains(err.Error(), strings.Repeat("s", 100)) {
		t.Fatalf("oversized keychain output was not withheld: %v", err)
	}
}

// The command line of every process is readable by every user on the machine.
// Passing a credential as an argument would publish it there for as long as the
// call runs, which is why the value goes over standard input instead.
func TestKeychainKeepsTheSecretOutOfArgv(t *testing.T) {
	store, argvLog := fakeSecurity(t)
	ctx := context.Background()
	ref := Ref{Provider: "anthropic", Account: "first-party"}

	if err := store.Set(ctx, ref, value); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if got.Expose() != value {
		t.Errorf("read back %q", got.Expose())
	}

	argv, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(argv), value) {
		t.Errorf("the credential appeared on a command line, where any user on the machine can read it:\n%s", argv)
	}
	// The only argument the write may carry is -i. Anything else means the
	// command moved back onto the command line, which is what this exists to
	// catch.
	if !strings.Contains(string(argv), "-i") {
		t.Errorf("no store call was made through the interactive parser:\n%s", argv)
	}
}

func TestKeychainPinsSystemSecurityAndScrubsChildEnvironment(t *testing.T) {
	shadow := t.TempDir()
	t.Setenv("PATH", shadow)
	t.Setenv("SB_KEYCHAIN_TOKEN", value)

	cmd := NewOSStore().command(context.Background(), "help")
	if cmd.Path != securityToolPath {
		t.Fatalf("security path = %q, want pinned %q", cmd.Path, securityToolPath)
	}
	for _, entry := range cmd.Env {
		if strings.Contains(entry, value) || strings.HasPrefix(entry, "SB_KEYCHAIN_TOKEN=") {
			t.Fatalf("credential-bearing environment reached security: %q", entry)
		}
	}
}

// Quoting is what keeps the interactive parser from splitting or unescaping a
// value on its way in. Without it a key with a space in it arrives truncated
// and a Windows-style path loses its separators, both reported as stored.
func TestKeychainQuotesAwkwardValues(t *testing.T) {
	for name, awkward := range map[string]string{
		"spaces":    "a value with spaces",
		"quotes":    `he said "hi"`,
		"backslash": `C:\Users\someone`,
		"json":      `{"a":"b c","d":"e\"f"}`,
	} {
		t.Run(name, func(t *testing.T) {
			store, _ := fakeSecurity(t)
			ref := Ref{Provider: "anthropic"}

			if err := store.Set(context.Background(), ref, awkward); err != nil {
				t.Fatal(err)
			}
			got, err := store.Get(context.Background(), ref)
			if err != nil {
				t.Fatal(err)
			}
			if got.Expose() != awkward {
				t.Errorf("stored %q and read back %q", awkward, got.Expose())
			}
		})
	}
}

// The interactive prompt this replaced truncated at 128 characters and exited
// zero, so a token document was stored as unparseable JSON and nothing said so.
func TestKeychainStoresMoreThanThePromptBufferHeld(t *testing.T) {
	store, _ := fakeSecurity(t)
	long := strings.Repeat("x", 1000)

	if err := store.Set(context.Background(), Ref{Provider: "anthropic"}, long); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(context.Background(), Ref{Provider: "anthropic"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Expose()) != len(long) {
		t.Errorf("stored %d characters and read back %d", len(long), len(got.Expose()))
	}
}

func TestKeychainMissIsNotAFailure(t *testing.T) {
	store, _ := fakeSecurity(t)

	_, err := store.Get(context.Background(), Ref{Provider: "anthropic"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want a miss so the resolver moves on", err)
	}
}

// The command reads the value as a line, so an embedded newline would store a
// truncated secret and report success.
func TestKeychainRefusesAMultilineValue(t *testing.T) {
	store, _ := fakeSecurity(t)

	err := store.Set(context.Background(), Ref{Provider: "anthropic"}, "first-line\nsecond-line")
	if err == nil {
		t.Fatal("a value with a newline was accepted; it would have been silently truncated")
	}
}

// requireLiveKeychain guards the tests that touch the user's real login
// keychain. They are not part of an ordinary run, because a test suite should
// not write to a credential store as a side effect of `go test ./...`.
func requireLiveKeychain(t *testing.T) {
	t.Helper()
	if os.Getenv("SB_LIVE") == "" {
		t.Skip("set SB_LIVE=1 to exercise the real login keychain")
	}
}

func TestLiveKeychainRoundTrip(t *testing.T) {
	requireLiveKeychain(t)

	ctx := context.Background()
	store := NewOSStore()
	ref := Ref{Provider: "sb-selftest", Account: "round-trip"}

	// A leftover item from an interrupted run would make the write look like it
	// worked when it had not.
	_ = store.Delete(ctx, ref)
	if _, err := store.Get(ctx, ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the test item exists before the test wrote it: %v", err)
	}
	t.Cleanup(func() { _ = store.Delete(ctx, ref) })

	if err := store.Set(ctx, ref, value); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if got.Expose() != value {
		t.Errorf("read back %q, want the value that was stored", got.Expose())
	}
	if got.Source != SourceKeychain {
		t.Errorf("source = %q", got.Source)
	}

	// Updating in place rather than failing on an existing item is what -U
	// buys, and a store that silently kept the old value would authenticate
	// with a key the user had already replaced.
	const rotated = "sk-rotated-9876543210"
	if err := store.Set(ctx, ref, rotated); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Get(ctx, ref); err != nil {
		t.Fatal(err)
	} else if got.Expose() != rotated {
		t.Errorf("after rotation the store returned %q", got.Expose())
	}

	if err := store.Delete(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, ref); !errors.Is(err, ErrNotFound) {
		t.Errorf("after deletion, err = %v, want a miss", err)
	}
}

// A credential longer than the tool's interactive prompt buffer used to be
// truncated at 128 characters and reported as stored. Every test here passed,
// because every value in them was short. An OAuth token document is not: it
// carries two JWTs and lands in the keychain as unparseable JSON.
//
// This is the shape that broke, at a length that broke it.
func TestLiveKeychainStoresALongDocument(t *testing.T) {
	requireLiveKeychain(t)

	ctx := context.Background()
	store := NewOSStore()
	ref := Ref{Provider: "sb-selftest", Account: "long-document"}

	_ = store.Delete(ctx, ref)
	t.Cleanup(func() { _ = store.Delete(ctx, ref) })

	document := `{"access_token":"` + strings.Repeat("A", 900) +
		`","refresh_token":"` + strings.Repeat("B", 200) +
		`","expires_at":"2026-08-13T12:00:00Z"}`

	if err := store.Set(ctx, ref, document); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Expose()) != len(document) {
		t.Fatalf("stored %d characters and read back %d; the value was truncated and the write reported success",
			len(document), len(got.Expose()))
	}
	if got.Expose() != document {
		t.Error("the document did not round-trip byte for byte")
	}
	if !json.Valid([]byte(got.Expose())) {
		t.Error("what came back is not valid JSON, so a token document would be unreadable")
	}
}

// Values the interactive parser would otherwise split or unescape.
func TestLiveKeychainStoresAwkwardValues(t *testing.T) {
	requireLiveKeychain(t)

	ctx := context.Background()
	store := NewOSStore()

	for name, awkward := range map[string]string{
		"spaces":     "a value with spaces in it",
		"quotes":     `he said "hi" to me`,
		"backslash":  `C:\Users\someone\key`,
		"json":       `{"a":"b","c":[1,2],"d":"with space"}`,
		"everything": `{"t":"a b \"c\" d\\e"}`,
	} {
		t.Run(name, func(t *testing.T) {
			ref := Ref{Provider: "sb-selftest", Account: "awkward-" + name}
			_ = store.Delete(ctx, ref)
			t.Cleanup(func() { _ = store.Delete(ctx, ref) })

			if err := store.Set(ctx, ref, awkward); err != nil {
				t.Fatal(err)
			}
			got, err := store.Get(ctx, ref)
			if err != nil {
				t.Fatal(err)
			}
			if got.Expose() != awkward {
				t.Errorf("stored %q and read back %q", awkward, got.Expose())
			}
		})
	}
}

// End to end through Set: a value carrying a newline and a second command must
// not reach the tool at all. The fake records every invocation, so a second
// command would show up in the log.
func TestKeychainRefusesAnInjectedCommand(t *testing.T) {
	store, argvLog := fakeSecurity(t)

	payload := "harmless\nadd-generic-password -a attacker -s attacker -U -w pwned"
	err := store.Set(context.Background(), Ref{Provider: "anthropic"}, payload)
	if err == nil {
		t.Fatal("a value containing a command was accepted")
	}
	if body, readErr := os.ReadFile(argvLog); readErr == nil && strings.Contains(string(body), "attacker") {
		t.Errorf("the injected command reached the tool:\n%s", body)
	}

	// The same payload in a reference, which is built from configuration rather
	// than from a credential and was previously interpolated unchecked.
	if err := store.Set(context.Background(), Ref{Provider: payload}, "ok-value"); err == nil {
		t.Error("a provider name containing a command was accepted")
	}
	if err := store.Set(context.Background(), Ref{Provider: "anthropic", Account: payload}, "ok-value"); err == nil {
		t.Error("an account name containing a command was accepted")
	}
}
