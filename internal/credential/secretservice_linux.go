package credential

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/switchboard-code/switchboard/internal/safeexec"
)

// OSStore is the freedesktop Secret Service, reached through `secret-tool`.
//
// As on macOS this is the command rather than the library, so that no cgo and
// no D-Bus dependency enter a binary §18 wants static. The service is optional
// on Linux in a way the Keychain is not on macOS: a server, a container, or a
// bare tty session frequently has no session bus and no keyring agent at all.
// That case is reported as unavailable rather than as an error, so the resolver
// can carry on to the sources that do work headlessly and the final message can
// say the chain was short rather than empty.
type OSStore struct {
	bin       string // explicit absolute test fixture; production uses helper
	helper    safeexec.Executable
	helperErr error
}

func NewOSStore() *OSStore {
	return newSecretServiceStore("secret-tool",
		"/usr/bin/secret-tool",
		"/usr/local/bin/secret-tool",
		"/bin/secret-tool",
	)
}

func newSecretServiceStore(name string, preferred ...string) *OSStore {
	helper, err := resolveLinuxHelper(name, preferred...)
	return &OSStore{helper: helper, helperErr: err}
}

func (s *OSStore) Name() string { return "Secret Service keyring" }

func (s *OSStore) command(ctx context.Context, args ...string) (*exec.Cmd, error) {
	if s.bin != "" {
		env, err := linuxHelperEnvironment(false)
		if err != nil {
			return nil, &Unavailable{Store: s.Name(), Reason: "the helper environment could not be safely prepared"}
		}
		cmd := exec.Command(s.bin, args...)
		cmd.Env = env
		return cmd, nil
	}
	if s.helperErr != nil {
		return nil, &Unavailable{
			Store:  s.Name(),
			Reason: "secret-tool was not found at a safe system location outside the current workspace; install libsecret-tools or use an environment variable or credential helper",
		}
	}
	cmd, err := s.helper.Command(args...)
	if err != nil {
		return nil, &Unavailable{
			Store:  s.Name(),
			Reason: "the resolved secret-tool executable changed; refusing to send it credential material",
		}
	}
	env, err := linuxHelperEnvironment(true)
	if err != nil {
		return nil, &Unavailable{
			Store:  s.Name(),
			Reason: "the helper environment could not be bound outside the current workspace; refusing to send credential material",
		}
	}
	cmd.Env = env
	return cmd, nil
}

// attributes are the lookup key. They are passed as separate argv elements, so
// nothing in a provider or surface name can be read as an option.
func attributes(ref Ref) []string {
	return []string{"service", service(ref), "account", account(ref)}
}

func (s *OSStore) Get(ctx context.Context, ref Ref) (Secret, error) {
	if err := ref.valid(); err != nil {
		return Secret{}, err
	}
	if err := s.reachable(); err != nil {
		return Secret{}, err
	}

	args := append([]string{"lookup"}, attributes(ref)...)
	cmd, err := s.command(ctx, args...)
	if err != nil {
		return Secret{}, err
	}

	var stdout, stderr boundedHelperCapture
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := runCredentialCommand(ctx, cmd); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return Secret{}, contextErr
		}
		if unavailable := s.unavailable(err, stderr.String()); unavailable != nil {
			return Secret{}, unavailable
		}
		// A miss and a broken bus are both a nonzero exit. They are told apart
		// by whether anything was said about it: the tool is silent on a miss
		// and explains itself on a failure.
		if strings.TrimSpace(stderr.String()) == "" {
			return Secret{}, ErrNotFound
		}
		return Secret{}, fmt.Errorf("reading the keyring: %w%s", err, diagnostics(stderr.String(), stderr.overflow))
	}
	if stdout.overflow {
		return Secret{}, fmt.Errorf("the keyring returned more than %d credential bytes; output withheld", maxHelperCaptureBytes)
	}

	// lookup writes the secret with no trailing newline.
	if strings.TrimSpace(stdout.String()) == "" {
		return Secret{}, ErrNotFound
	}
	return New(stdout.String(), SourceKeychain, "secret service"), nil
}

func (s *OSStore) Set(ctx context.Context, ref Ref, value string) error {
	if err := ref.valid(); err != nil {
		return err
	}
	if strings.TrimSpace(value) == "" {
		return errors.New("refusing to store an empty credential")
	}
	if err := s.reachable(); err != nil {
		return err
	}

	args := append([]string{"store", "--label=Switchboard " + ref.String()}, attributes(ref)...)
	cmd, err := s.command(ctx, args...)
	if err != nil {
		return err
	}
	// The tool reads the secret from standard input, which keeps it out of argv
	// and out of any process listing.
	cmd.Stdin = strings.NewReader(value)

	var stderr boundedHelperCapture
	cmd.Stderr = &stderr

	if err := runCredentialCommand(ctx, cmd); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		if unavailable := s.unavailable(err, stderr.String()); unavailable != nil {
			return unavailable
		}
		return fmt.Errorf("storing in the keyring: %w%s", err, diagnostics(stderr.String(), stderr.overflow))
	}
	return nil
}

func (s *OSStore) Delete(ctx context.Context, ref Ref) error {
	if err := ref.valid(); err != nil {
		return err
	}
	if err := s.reachable(); err != nil {
		return err
	}

	// clear succeeds whether or not anything matched, so absence is confirmed
	// first. Reporting "removed" for an item that was never there would let a
	// user believe a credential is gone when it is stored under another name.
	if _, err := s.Get(ctx, ref); err != nil {
		return err
	}

	args := append([]string{"clear"}, attributes(ref)...)
	cmd, err := s.command(ctx, args...)
	if err != nil {
		return err
	}

	var stderr boundedHelperCapture
	cmd.Stderr = &stderr

	if err := runCredentialCommand(ctx, cmd); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		if unavailable := s.unavailable(err, stderr.String()); unavailable != nil {
			return unavailable
		}
		return fmt.Errorf("removing from the keyring: %w%s", err, diagnostics(stderr.String(), stderr.overflow))
	}
	return nil
}

// reachable checks for a session bus before running anything. Without one the
// tool fails with a message about D-Bus that reads like a bug in this program,
// and the honest answer is that the machine has no keyring.
func (s *OSStore) reachable() error {
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		return &Unavailable{
			Store:  s.Name(),
			Reason: "no D-Bus session bus, so this machine has no keyring to read; use an environment variable or a credential helper",
		}
	}
	return nil
}

func (s *OSStore) unavailable(err error, stderr string) error {
	if execErr := new(exec.Error); errors.As(err, &execErr) {
		return &Unavailable{
			Store:  s.Name(),
			Reason: "secret-tool is not installed; it ships in libsecret-tools on Debian and Ubuntu",
		}
	}
	if strings.Contains(stderr, "org.freedesktop.secrets") || strings.Contains(stderr, "Secret Service") {
		return &Unavailable{
			Store:  s.Name(),
			Reason: "no Secret Service is running on the session bus",
		}
	}
	return nil
}
