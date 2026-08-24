package session

import (
	"bufio"
	"strings"
)

// OpeningSummary is the provenance-aware opening used by presentation
// surfaces. Text is populated only from a verified authored projection or a
// harness-marked synthetic opening. Found distinguishes an empty log from a
// legacy opening whose provider-expanded wording is deliberately withheld.
type OpeningSummary struct {
	Text          string
	Found         bool
	AuthoredKnown bool
	Synthetic     bool
}

// ReadOpeningSummary reads only far enough to classify the first textual turn
// opening. Legacy provider-expanded Content is inspected only for presence;
// its bytes are never returned as user-authored wording.
func ReadOpeningSummary(path string) (OpeningSummary, error) {
	f, err := openPublishedLog(path)
	if err != nil {
		return OpeningSummary{}, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	if err := checkHeader(r, path); err != nil {
		return OpeningSummary{}, err
	}

	lastSeq := 0
	budget := newReplayBudget(defaultReplayLimits, len(magic)+4)
	for {
		rec, _, err := budget.decode(r, &lastSeq)
		if isRecoverableRecordEnd(err) {
			return OpeningSummary{}, nil
		}
		if err != nil {
			return OpeningSummary{}, err
		}
		message, ok, err := conversationMessage(rec)
		if err != nil {
			return OpeningSummary{}, err
		}
		if !ok || !OpensTurn(message) {
			continue
		}

		authored, known := message.AuthoredProjection()
		authored = strings.TrimSpace(authored)
		if known {
			if authored == "" {
				// An image-only opening has no words to label; keep looking.
				continue
			}
			return OpeningSummary{
				Text: authored, Found: true, AuthoredKnown: true, Synthetic: message.Synthetic,
			}, nil
		}

		providerText := strings.TrimSpace(message.AuthoredText())
		if providerText == "" {
			continue
		}
		if message.Synthetic {
			// Compaction seeds predate the authored projection by design: they are
			// harness context, not words typed by the user. Return their text only
			// with the durable Synthetic bit so consumers can recognize the exact
			// compact-seed format without trusting a content lookalike.
			return OpeningSummary{Text: providerText, Found: true, Synthetic: true}, nil
		}
		return OpeningSummary{Found: true}, nil
	}
}
