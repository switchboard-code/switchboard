package main

import (
	"errors"
	"fmt"

	"github.com/switchboard-code/switchboard/internal/session"
)

// publicationDisposition is the three-state result of a staged-session
// publication. Visibility is an adoption commit, while durability decides
// whether the process may safely continue doing work after that commit.
type publicationDisposition uint8

const (
	publicationUnpublished publicationDisposition = iota
	publicationVisibleUncertain
	publicationDurable
)

type durableSessionPublisher func(*session.Session) (session.PublicationOutcome, error)

func publishDurablyWith(sess *session.Session, publisher durableSessionPublisher) (session.PublicationOutcome, error) {
	if publisher != nil {
		return publisher(sess)
	}
	return sess.PublishDurably()
}

// publicationResult converts PublishDurably's outcome into the only three
// caller actions that are safe. In particular, a visible marker must never be
// treated as a rollbackable error even when its persistence barrier failed.
func publicationResult(outcome session.PublicationOutcome, err error, label string) (publicationDisposition, error) {
	if outcome.Durable && !outcome.Visible {
		return publicationUnpublished, fmt.Errorf("publishing %s returned an invalid durability outcome", label)
	}
	if outcome.Visible && outcome.Durable {
		return publicationDurable, nil
	}
	if err == nil {
		err = errors.New("session publication returned an incomplete durability outcome")
	}
	if outcome.Visible {
		return publicationVisibleUncertain, fmt.Errorf(
			"publishing %s became visible, but durability could not be confirmed; restart Switchboard before continuing: %w",
			label, err)
	}
	return publicationUnpublished, fmt.Errorf("publishing %s: %w", label, err)
}

// publicationRestartRequiredError marks a publication that crossed the
// visibility commit but not its durability barrier. Surfaces use the type to
// stop their event/read loop rather than presenting an ordinary recoverable
// command error and accepting more work.
type publicationRestartRequiredError struct{ err error }

func (e *publicationRestartRequiredError) Error() string { return e.err.Error() }
func (e *publicationRestartRequiredError) Unwrap() error { return e.err }
