package credential

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/switchboard-code/switchboard/internal/execution"
)

// OSStore is the macOS Keychain, reached through the `security` command.
//
// The command rather than the framework, because binding SecKeychain means cgo
// and cgo means the static, cross-compiled binary §18 asks for stops being
// possible. §5.3 already describes replaceable auth integrations as helper
// processes with a narrow protocol; this is that shape applied to the platform
// store itself.
//
// The secret never appears in argv. Writes go through `security -i`, where the
// whole command arrives on standard input, so a process listing on a shared
// machine sees only "security -i". Reads and deletes take ordinary arguments,
// because neither carries a secret.
//
// That parser splits on unquoted spaces, unescapes backslashes, and reads one
// command per line. It expands nothing else: command substitution, backticks,
// variables, semicolons, and pipes are all stored literally, which was checked
// rather than assumed. So arguments are quoted for the first two, and the line
// split is handled by refusing control characters outright, since no quoting
// survives it.
type OSStore struct {
	// bin exists for tests. Production always uses the system binary by its
	// absolute path: a checkout must not be able to receive credentials by
	// putting a lookalike earlier on PATH.
	bin string
}

const securityToolPath = "/usr/bin/security"

func NewOSStore() *OSStore { return &OSStore{} }

func (s *OSStore) Name() string { return "macOS Keychain" }

func (s *OSStore) tool() string {
	if s.bin != "" {
		return s.bin
	}
	return securityToolPath
}

func (s *OSStore) command(ctx context.Context, args ...string) *exec.Cmd {
	// runCredentialCommand below owns cancellation for the complete process
	// group. Constructing a CommandContext here would add a competing
	// direct-child-only kill path.
	cmd := exec.Command(s.tool(), args...)
	cmd.Env = execution.ScrubbedChildEnv()
	return cmd
}

// notFoundStatus is what `security` exits with when the item is absent. It is
// checked by number because the message is prose and has changed between
// releases.
const notFoundStatus = 44

// deniedStatus is what a write exits with when the keychain will not authorize
// it. The message it prints, "the authorization was canceled by the user",
// describes a dialog the user may never have seen, so the two conditions that
// actually produce it are named instead.
const deniedStatus = 154

func (s *OSStore) Get(ctx context.Context, ref Ref) (Secret, error) {
	if err := ref.valid(); err != nil {
		return Secret{}, err
	}

	cmd := s.command(ctx,
		"find-generic-password", "-s", service(ref), "-a", account(ref), "-w")

	var stdout, stderr boundedHelperCapture
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := runCredentialCommand(ctx, cmd); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return Secret{}, contextErr
		}
		if unavailable := s.unavailable(err); unavailable != nil {
			return Secret{}, unavailable
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == notFoundStatus {
			return Secret{}, ErrNotFound
		}
		return Secret{}, fmt.Errorf("reading the keychain: %w%s", err, diagnostics(stderr.String(), stderr.overflow))
	}
	if stdout.overflow {
		return Secret{}, fmt.Errorf("the keychain returned more than %d credential bytes; output withheld", maxHelperCaptureBytes)
	}

	// -w writes the password followed by a newline and nothing else.
	return New(stdout.String(), SourceKeychain, "login keychain"), nil
}

func (s *OSStore) Set(ctx context.Context, ref Ref, value string) error {
	if err := ref.valid(); err != nil {
		return err
	}
	if strings.TrimSpace(value) == "" {
		return errors.New("refusing to store an empty credential")
	}
	if err := printable(value, "value"); err != nil {
		// The tool reads one command per line. A newline in the value ends the
		// intended command and has the remainder parsed as another, which was
		// confirmed against the real tool: it created a second keychain item
		// from the tail of an injected value. Quoting does not help, because
		// the split happens before quotes are considered.
		return err
	}

	// The command is fed through the tool's own interactive mode rather than
	// given as arguments, which is the only shape that gets both properties
	// this write needs.
	//
	// Passing the value in argv would publish it to every process listing for
	// the length of the call. Letting the tool prompt for it on standard input
	// instead silently truncates at 128 characters, which was measured rather
	// than assumed: a 500 character value written that way reads back as 128,
	// with a success exit code. That is how an OAuth token document lands in
	// the keychain as unparseable JSON. Here the whole command line arrives on
	// standard input, so `ps` sees only "security -i" and nothing is truncated.
	cmd := s.command(ctx, "-i")
	cmd.Stdin = strings.NewReader(fmt.Sprintf(
		"add-generic-password -s %s -a %s -D %s -U -w %s\n",
		quoteArg(service(ref)),
		quoteArg(account(ref)),
		quoteArg("Switchboard provider credential"),
		quoteArg(value),
	))

	var stderr boundedHelperCapture
	cmd.Stderr = &stderr

	if err := runCredentialCommand(ctx, cmd); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		if unavailable := s.unavailable(err); unavailable != nil {
			return unavailable
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == deniedStatus {
			return fmt.Errorf("the keychain refused the write%s\n\n"+
				"This is usually one of two things: the login keychain is locked and there is no\n"+
				"desktop session to unlock it, which happens over SSH, or HOME points somewhere\n"+
				"without a login keychain. Either way an environment variable or a credential\n"+
				"helper will work where this will not.", diagnostics(stderr.String(), stderr.overflow))
		}
		return fmt.Errorf("storing in the keychain: %w%s", err, diagnostics(stderr.String(), stderr.overflow))
	}
	return nil
}

func (s *OSStore) Delete(ctx context.Context, ref Ref) error {
	if err := ref.valid(); err != nil {
		return err
	}

	cmd := s.command(ctx,
		"delete-generic-password", "-s", service(ref), "-a", account(ref))

	var stderr boundedHelperCapture
	cmd.Stderr = &stderr

	if err := runCredentialCommand(ctx, cmd); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		if unavailable := s.unavailable(err); unavailable != nil {
			return unavailable
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == notFoundStatus {
			return ErrNotFound
		}
		return fmt.Errorf("removing from the keychain: %w%s", err, diagnostics(stderr.String(), stderr.overflow))
	}
	return nil
}

// quoteArg wraps a value for the tool's interactive parser, which splits on
// spaces and treats a backslash as an escape. Both were established by feeding
// it awkward values and reading back what it stored.
func quoteArg(value string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range value {
		if r == '"' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

func (s *OSStore) unavailable(err error) error {
	if execErr := new(exec.Error); errors.As(err, &execErr) {
		return &Unavailable{Store: s.Name(), Reason: "the system security command is unavailable at /usr/bin/security"}
	}
	return nil
}
