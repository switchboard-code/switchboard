package main

import (
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/switchboard-code/switchboard/internal/credential"
)

// redactCredentialText is the unattended-output posture shared by verifier
// and shell folds. It must run before any length cap: truncating first can cut
// a recognized credential below the scanner's precision-oriented length floor
// and leave a sendable prefix that no longer matches.
func redactCredentialText(text string) string {
	if leaks := credential.ScanPrompt(text); len(leaks) > 0 {
		return credential.Redact(text, leaks)
	}
	return text
}

func redactCredentialTextBeforeTruncate(text string, maxRunes int) string {
	return truncate(redactCredentialText(text), maxRunes)
}

// truncateBytesAtGrapheme caps valid text by bytes without splitting a UTF-8
// encoding or an extended grapheme cluster. Shell output is byte-budgeted,
// while the rest of the TUI truncates by visible clusters.
func truncateBytesAtGrapheme(text string, maxBytes int) (string, bool) {
	text = strings.ToValidUTF8(text, "�")
	if maxBytes < 0 {
		maxBytes = 0
	}
	if len(text) <= maxBytes {
		return text, false
	}
	end := 0
	for remaining := text; remaining != ""; {
		cluster, _ := ansi.FirstGraphemeCluster(remaining, ansi.GraphemeWidth)
		if cluster == "" {
			cluster = remaining[:1]
		}
		if end+len(cluster) > maxBytes {
			break
		}
		end += len(cluster)
		remaining = remaining[len(cluster):]
	}
	return text[:end], true
}
