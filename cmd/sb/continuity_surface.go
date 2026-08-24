package main

import (
	"fmt"

	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

// turnOpening assembles the complete provider-visible opening. Callers stamp
// this message before any routing, token, context, or cost calculation, then
// carry that exact value through to TurnMessage.
func turnOpening(prompt string, images []provider.Image) provider.Message {
	return turnOpeningAuthored(prompt, prompt, images)
}

// turnOpeningAuthored keeps the provider request and the user-visible
// projection together. Prompt may contain file attachments, shell output, or
// other harness context; authored is the exact text the surface showed as the
// user's instruction.
func turnOpeningAuthored(prompt, authored string, images []provider.Image) provider.Message {
	opening := provider.UserText(prompt).WithAuthoredText(authored)
	for _, image := range images {
		opening.Content = append(opening.Content, image)
	}
	return opening
}

// syntheticTurnOpening is opening-shaped for provider protocol compatibility,
// but it is not attributed to the user. The durable bit, rather than matching
// its text, lets compaction distinguish an automatic continuation from a user
// who intentionally typed the same sentence.
func syntheticTurnOpening(prompt string, images []provider.Image) provider.Message {
	opening := turnOpeningAuthored(prompt, "", images)
	opening.Authored = ""
	opening.AuthoredKnown = false
	opening.Synthetic = true
	return opening
}

func stampTurnOpening(sess *session.Session, opening provider.Message) (provider.Message, error) {
	if sess == nil {
		return provider.Message{}, fmt.Errorf("stamp turn opening: no active session")
	}
	stamped, _, err := sess.StampContinuityOpening(opening)
	if err != nil {
		return provider.Message{}, fmt.Errorf("stamp turn opening: %w", err)
	}
	return stamped, nil
}

// stampRecordedTurnOpening validates the exact opening selected by /retry
// against the rewound session. A modern stamped opening is accepted unchanged;
// a plain opening remains plain. If a legacy rewind would require injecting a
// capsule the original provider never saw, fail closed instead of calling the
// result an exact replay.
func stampRecordedTurnOpening(sess *session.Session, opening provider.Message) (provider.Message, error) {
	originalRef := opening.ContinuityRef
	stamped, err := stampTurnOpening(sess, opening)
	if err != nil {
		return provider.Message{}, err
	}
	if originalRef == "" && stamped.ContinuityRef != "" {
		return provider.Message{}, fmt.Errorf("retry opening cannot be replayed exactly because the rewound session has an undelivered continuity capsule")
	}
	return stamped, nil
}
