// Package terminaltext renders untrusted metadata without letting terminal
// control bytes redraw or spoof the permission interface.
package terminaltext

import (
	"fmt"
	"strings"
)

// Escape replaces C0, DEL, and C1 controls with visible ASCII escapes. These
// ranges cover ANSI CSI/OSC introducers and OSC terminators; ordinary Unicode
// text is preserved.
func Escape(text string) string {
	return escape(text, false)
}

// Display escapes untrusted terminal output while retaining its line
// structure. Tabs are rendered as the two printable cells "\\t": terminal
// tab stops are configurable, while the cell-width libraries used by the TUI
// count a literal tab as zero. Leaving one in rendered output would therefore
// let the terminal wrap rows the viewport believes are still on one line.
// Command output is kept raw in the session/model transcript, but every
// terminal rendering boundary uses this form so CSI, OSC, carriage returns,
// and bidi controls cannot redraw a later permission prompt.
func Display(text string) string {
	return escape(text, true)
}

func escape(text string, preserveLayout bool) string {
	var b strings.Builder
	for _, r := range text {
		switch {
		case preserveLayout && r == '\n':
			b.WriteRune(r)
		case preserveLayout && r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r >= 0x80 && r <= 0x9f:
			fmt.Fprintf(&b, `\u%04x`, r)
		case r == 0x061c || r == 0x200e || r == 0x200f ||
			(r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
