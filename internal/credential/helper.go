package credential

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"unicode/utf8"
)

const maxHelperCaptureBytes = 64 << 10

type boundedHelperCapture struct {
	buf      bytes.Buffer
	overflow bool
}

func (c *boundedHelperCapture) Write(p []byte) (int, error) {
	written := len(p)
	remaining := maxHelperCaptureBytes + 1 - c.buf.Len()
	if remaining > 0 {
		if remaining < len(p) {
			_, _ = c.buf.Write(p[:remaining])
		} else {
			_, _ = c.buf.Write(p)
		}
	}
	if c.buf.Len() > maxHelperCaptureBytes || len(p) > remaining {
		c.overflow = true
	}
	return written, nil
}

func (c *boundedHelperCapture) String() string { return c.buf.String() }

// HelperStore runs a command the user configured and takes its standard output
// as the credential.
//
// This is the second headless path in §5.3, and the one that lets a password
// manager or a cloud credential chain be the source of truth without this
// program learning about either. The contract is deliberately small:
//
//	stdout   the credential, and nothing else. Never logged, never included in
//	         an error, not even on failure.
//	stderr   diagnostics. Included in errors, so a helper must not write the
//	         credential here.
//	exit 0   success. Any other status is a configuration error that stops the
//	         chain rather than falling through, because a helper that is present
//	         and broken is not the same as no helper at all.
type HelperStore struct {
	// Command is argv, not a shell line. There is no shell, so nothing in a
	// reference or a provider name can be read as shell syntax.
	Command []string

	// Env supplies the variables named in the contract below, in addition to
	// the parent environment.
	Env []string
}

func (s *HelperStore) Name() string {
	if len(s.Command) == 0 {
		return "credential helper"
	}
	return "credential helper (" + s.Command[0] + ")"
}

func (s *HelperStore) Get(ctx context.Context, ref Ref) (Secret, error) {
	if len(s.Command) == 0 {
		return Secret{}, ErrNotFound
	}

	// runCredentialCommand owns cancellation for the whole process group. Do
	// not use CommandContext here: its direct-child kill can race the bounded
	// descendant and retained-pipe cleanup.
	cmd := exec.Command(s.Command[0], s.Command[1:]...)
	// The helper is told which credential is wanted, so one command can serve
	// every provider without the config repeating itself.
	cmd.Env = append(cmd.Environ(),
		"SB_CREDENTIAL_PROVIDER="+ref.Provider,
		"SB_CREDENTIAL_ACCOUNT="+ref.Account,
	)
	cmd.Env = append(cmd.Env, s.Env...)

	var stdout, stderr boundedHelperCapture
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := runCredentialCommand(ctx, cmd); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return Secret{}, contextErr
		}
		if execErr := new(exec.Error); errors.As(err, &execErr) {
			return Secret{}, &Unavailable{
				Store:  s.Name(),
				Reason: fmt.Sprintf("%s is not on PATH", s.Command[0]),
			}
		}
		// stdout is the credential channel and is never quoted back, including
		// here: a helper that fails partway may well have written part of a
		// secret to it.
		return Secret{}, fmt.Errorf("%s failed: %w%s", s.Name(), err, diagnostics(stderr.String(), stderr.overflow))
	}
	if stdout.overflow {
		return Secret{}, fmt.Errorf("%s returned more than %d credential bytes; output withheld", s.Name(), maxHelperCaptureBytes)
	}

	value := strings.TrimSpace(stdout.String())
	if value == "" {
		return Secret{}, ErrNotFound
	}
	return New(value, SourceHelper, s.Command[0]), nil
}

// diagnostics formats a helper's stderr for an error message, capped so a
// helper that dumps a page of output does not bury the failure.

func diagnostics(stderr string, overflow ...bool) string {
	if len(overflow) > 0 && overflow[0] {
		return fmt.Sprintf(": diagnostics exceeded %d bytes and were withheld", maxHelperCaptureBytes)
	}
	stderr = strings.ToValidUTF8(stderr, "�")
	stderr = Redact(stderr, ScanPrompt(stderr))
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return ""
	}
	const limit = 400
	if len(stderr) > limit {
		keep := limit
		for keep > 0 && !utf8.RuneStart(stderr[keep]) {
			keep--
		}
		stderr = stderr[:keep] + "..."
	}
	return ": " + strings.ReplaceAll(stderr, "\n", "; ")
}
