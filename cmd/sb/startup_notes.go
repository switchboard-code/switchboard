package main

// Startup can discover the same extension problem through several native
// compatibility paths. The full diagnostics still belong in /doctor, but
// repeating every one before the first prompt can consume an entire small
// terminal. This file keeps those two concerns separate: Highlights are facts
// that must remain visible, Summary is bounded, and Details loses neither an
// entry nor its original order.

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

const (
	startupSummaryMaxLines           = 3
	startupSummaryMaxColumns         = 79
	startupNoncriticalHighlightLimit = 5
	// 72 ASCII columns plus the TUI's two-cell notice rail fit one row in an
	// 80-column terminal. The full sanitized text is never truncated in
	// Details, only in this first-prompt preview.
	startupHighlightMaxRunes = 72
)

type startupNoteCategory string

const (
	startupNoteSkills  startupNoteCategory = "skills"
	startupNotePlugins startupNoteCategory = "plugins"
	startupNoteMCP     startupNoteCategory = "MCP"
	startupNoteHooks   startupNoteCategory = "hooks"
	startupNoteLSP     startupNoteCategory = "LSP"
	startupNotePolicy  startupNoteCategory = "policy"
	startupNoteOther   startupNoteCategory = "other"
)

var startupNoteCategoryOrder = [...]startupNoteCategory{
	startupNoteSkills,
	startupNotePlugins,
	startupNoteMCP,
	startupNoteHooks,
	startupNoteLSP,
	startupNotePolicy,
	startupNoteOther,
}

// startupNoteGroup reports both the raw number of diagnostics and the number
// of distinct problems they represent. Mandatory duplicates remain individual
// highlights, but the count still uses semantic identity so "unique" always
// means distinct.
type startupNoteGroup struct {
	Category startupNoteCategory
	Unique   int
	Total    int
}

// startupNoteReport is the integration contract for startup and
// `/doctor extensions`:
//
//   - Highlights contains every fatal/critical/explicitly-required failure,
//     deduplicated security/trust/fallback warnings and representative
//     ordinary errors, with a shared hard limit for all noncritical notes.
//   - Summary contains no more than three 79-column plain-text lines.
//   - Details is a terminal-safe, otherwise exact copy of every input note in
//     input order, including duplicates, followed by an explicit overflow
//     disclosure when Dropped is nonzero.
//   - Groups always follows startupNoteCategoryOrder, including zero counts.
//   - Retained and Dropped distinguish stored raw diagnostics from observed
//     diagnostics whose text could not fit the bounded pre-surface buffer.
type startupNoteReport struct {
	Highlights []mcpNote
	Summary    []string
	Details    []mcpNote
	Groups     []startupNoteGroup
	Retained   int
	Dropped    int
}

func aggregateStartupNotes(notes []mcpNote, droppedCounts ...int) startupNoteReport {
	dropped := 0
	for _, count := range droppedCounts {
		if count > 0 {
			dropped += count
		}
	}
	report := startupNoteReport{
		Details:  make([]mcpNote, 0, len(notes)),
		Groups:   make([]startupNoteGroup, len(startupNoteCategoryOrder)),
		Retained: len(notes),
		Dropped:  dropped,
	}
	groupIndex := make(map[startupNoteCategory]int, len(startupNoteCategoryOrder))
	for i, category := range startupNoteCategoryOrder {
		report.Groups[i].Category = category
		groupIndex[category] = i
	}

	// Semantic keys count distinct issues for every severity. Mandatory
	// failures still bypass deduplication in Highlights, so identical required
	// components remain individually visible without being mislabeled unique.
	seen := make(map[string]struct{}, len(notes))
	var visibleWarnings, ordinaryErrors, otherWarnings []mcpNote
	unique := 0
	for _, original := range notes {
		note := sanitizeStartupNote(original)
		report.Details = append(report.Details, note)

		category := classifyStartupNote(note.text)
		index := groupIndex[category]
		report.Groups[index].Total++

		high := isHighSeverityStartupNote(note)
		if high {
			highlight := note
			highlight.level = mandatoryStartupLevel(note)
			report.Highlights = append(report.Highlights, boundedStartupHighlight(highlight))
		}
		key := semanticStartupNoteKey(note, category)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		report.Groups[index].Unique++
		unique++
		if high {
			continue
		}
		switch {
		case isVisibleStartupWarning(note):
			visibleWarnings = append(visibleWarnings, note)
		case isOrdinaryStartupError(note):
			ordinaryErrors = append(ordinaryErrors, note)
		case severityRank(note.level) >= severityRank("warn"):
			// Everything else that still calls itself a warning. It ranks
			// below the security vocabulary and below an error, but a warning
			// that is the only one of its kind should not be unseeable: with
			// classes collapsed, one such finding costs one line, and the
			// count in the summary no longer stands for something a user was
			// never shown at all.
			otherWarnings = append(otherWarnings, note)
		}
	}

	// Security/trust/policy warnings win the bounded noncritical slots, then
	// ordinary errors. Full text for every omitted note remains in Details.
	sortStartupNotes(visibleWarnings)
	sortStartupNotes(ordinaryErrors)
	sortStartupNotes(otherWarnings)
	noncritical := append(append(visibleWarnings, ordinaryErrors...), otherWarnings...)
	// One finding repeated across five plugins is one thing to know, not five,
	// and it was taking every slot the screen had — so the other four findings
	// went unseen behind a count. Notes that differ only in which name they
	// quote collapse to their first, which then says how many it stands for.
	noncritical, classCounts := collapseStartupNoteClasses(noncritical)
	visibleNoncritical := min(len(noncritical), startupNoncriticalHighlightLimit)
	for _, note := range noncritical[:visibleNoncritical] {
		if more := classCounts[startupNoteClassKey(note)] - 1; more > 0 {
			note.text = boundedStartupHighlight(note).text +
				" (and " + strconv.Itoa(more) + " more like it)"
			report.Highlights = append(report.Highlights, note)
			continue
		}
		report.Highlights = append(report.Highlights, boundedStartupHighlight(note))
	}
	omittedNoncritical := len(noncritical) - visibleNoncritical
	if dropped > 0 {
		disclosure := sanitizeStartupNote(startupNoteOverflowDisclosure(dropped))
		report.Details = append(report.Details, disclosure)
		report.Highlights = append(report.Highlights, boundedStartupHighlight(disclosure))
	}

	// Highlights are independent of discovery concurrency. Details deliberately
	// retains discovery order for exact diagnostic expansion.
	sortStartupNotes(report.Highlights)

	if len(notes) > 0 || dropped > 0 {
		report.Summary = renderStartupNoteSummary(
			len(notes)+dropped, len(notes), dropped, unique, omittedNoncritical, report.Groups)
	}
	return report
}

func boundedStartupHighlight(note mcpNote) mcpNote {
	runes := []rune(note.text)
	if len(runes) <= startupHighlightMaxRunes {
		return note
	}
	note.text = string(runes[:startupHighlightMaxRunes-1]) + "…"
	return note
}

func sortStartupNotes(notes []mcpNote) {
	sort.SliceStable(notes, func(i, j int) bool {
		left, right := notes[i], notes[j]
		if severityRank(left.level) != severityRank(right.level) {
			return severityRank(left.level) > severityRank(right.level)
		}
		if classifyStartupNote(left.text) != classifyStartupNote(right.text) {
			return categoryRank(classifyStartupNote(left.text)) < categoryRank(classifyStartupNote(right.text))
		}
		if left.text != right.text {
			return left.text < right.text
		}
		return left.level < right.level
	})
}

func sanitizeStartupNote(note mcpNote) mcpNote {
	return mcpNote{
		level: cliText(note.level),
		text:  cliText(note.text),
	}
}

func classifyStartupNote(text string) startupNoteCategory {
	words := startupNoteWords(text)
	componentWords := words
	if colon := strings.IndexByte(text, ':'); colon >= 0 {
		componentWords = startupNoteWords(text[:colon])
	}
	// Managed-policy diagnostics can mention a plugin or MCP server, but their
	// actionable owner is policy, so that category wins before the component.
	if hasStartupWord(componentWords, "policy") || hasStartupWordPrefix(componentWords, "policy") {
		return startupNotePolicy
	}
	if hasStartupWord(componentWords, "skill") || hasStartupWord(componentWords, "skills") {
		return startupNoteSkills
	}
	if hasStartupWord(componentWords, "plugin") || hasStartupWord(componentWords, "plugins") {
		return startupNotePlugins
	}
	if hasStartupWord(componentWords, "mcp") {
		return startupNoteMCP
	}
	if hasStartupWord(componentWords, "hook") || hasStartupWord(componentWords, "hooks") {
		return startupNoteHooks
	}
	if hasStartupWord(componentWords, "lsp") ||
		(hasStartupWord(componentWords, "language") && hasStartupWord(componentWords, "server")) {
		return startupNoteLSP
	}
	// Messages without a component prefix (notably loader errors that begin
	// with a source path) still get a best-effort full-text classification.
	if len(componentWords) != len(words) {
		return classifyStartupNote(strings.Join(words, " "))
	}
	return startupNoteOther
}

// collapseStartupNoteClasses keeps the first note of each class, in order, and
// counts how many the class held. Details keeps every one of them verbatim;
// this governs only which get a line on the opening screen.
func collapseStartupNoteClasses(notes []mcpNote) ([]mcpNote, map[string]int) {
	counts := make(map[string]int, len(notes))
	kept := make([]mcpNote, 0, len(notes))
	for _, note := range notes {
		key := startupNoteClassKey(note)
		counts[key]++
		if counts[key] == 1 {
			kept = append(kept, note)
		}
	}
	return kept, counts
}

// startupNoteClassKey is the shape of a finding with the particulars removed:
// quoted names and bracketed paths drop out, so "plugin \"a\" is not loaded"
// and "plugin \"b\" is not loaded" are one class. Deliberately coarser than
// semanticStartupNoteKey, which decides what is genuinely a duplicate; this
// one decides only what is worth a separate line.
func startupNoteClassKey(note mcpNote) string {
	text := quotedStartupParticulars.ReplaceAllString(note.text, `""`)
	text = strings.ToLower(strings.Join(strings.Fields(text), " "))
	return strings.ToLower(strings.TrimSpace(note.level)) + "\x00" + trimStartupSourceLocation(text)
}

var quotedStartupParticulars = regexp.MustCompile(`"[^"]*"`)

func semanticStartupNoteKey(note mcpNote, category startupNoteCategory) string {
	level := strings.ToLower(strings.TrimSpace(note.level))
	if level == "warning" {
		level = "warn"
	}
	text := strings.ToLower(strings.Join(strings.Fields(note.text), " "))
	text = strings.NewReplacer(
		" :", ":",
		" ;", ";",
		" ,", ",",
		"( ", "(",
		" )", ")",
	).Replace(text)
	text = strings.TrimRight(text, ".")
	text = trimStartupSourceLocation(text)
	return string(category) + "\x00" + level + "\x00" + text
}

// Native extension diagnostics commonly append the source path in
// parentheses. The path remains verbatim in Details; it is omitted only from
// the routine dedupe key when the suffix is unambiguously path-shaped.
func trimStartupSourceLocation(text string) string {
	start := strings.LastIndex(text, " (")
	if start < 0 || !strings.HasSuffix(text, ")") {
		return text
	}
	location := text[start+2 : len(text)-1]
	if strings.Contains(location, "/") || strings.Contains(location, `\`) {
		return strings.TrimSpace(text[:start])
	}
	return text
}

func isHighSeverityStartupNote(note mcpNote) bool {
	return mandatoryStartupLevel(note) != ""
}

func mandatoryStartupLevel(note mcpNote) string {
	switch strings.ToLower(strings.TrimSpace(note.level)) {
	case "fatal", "critical", "high", "required":
		return strings.ToLower(strings.TrimSpace(note.level))
	}
	words := startupNoteWords(note.text)
	for _, level := range []string{"fatal", "critical", "required"} {
		if hasStartupWord(words, level) {
			return level
		}
	}
	return ""
}

func isOrdinaryStartupError(note mcpNote) bool {
	level := strings.ToLower(strings.TrimSpace(note.level))
	return level == "error" || level == "err"
}

func isVisibleStartupWarning(note mcpNote) bool {
	if strings.TrimSpace(note.level) == "" {
		return false
	}
	words := startupNoteWords(note.text)
	for _, word := range []string{
		"credential", "fallback", "permission", "policy", "sandbox", "secret",
		"security", "signature", "substitute", "substituted", "substitution",
		"trust", "trusted", "untrusted", "unsafe",
	} {
		if hasStartupWord(words, word) {
			return true
		}
	}
	return hasStartupWordPrefix(words, "contain")
}

func severityRank(level string) int {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "fatal", "critical":
		return 4
	case "error", "high", "required":
		return 3
	case "warn", "warning":
		return 2
	case "info", "notice":
		return 1
	default:
		return 0
	}
}

func categoryRank(category startupNoteCategory) int {
	for i, candidate := range startupNoteCategoryOrder {
		if candidate == category {
			return i
		}
	}
	return len(startupNoteCategoryOrder)
}

func startupNoteWords(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func hasStartupWord(words []string, wanted string) bool {
	for _, word := range words {
		if word == wanted {
			return true
		}
	}
	return false
}

func hasStartupWordPrefix(words []string, prefix string) bool {
	for _, word := range words {
		if strings.HasPrefix(word, prefix) {
			return true
		}
	}
	return false
}

func renderStartupNoteSummary(total, retained, dropped, unique, omittedNoncritical int, groups []startupNoteGroup) []string {
	if dropped > 0 {
		head := "extensions: " + strconv.Itoa(total) + " startup notes: " +
			strconv.Itoa(retained) + " retained, " + strconv.Itoa(dropped) +
			" dropped; /doctor extensions"
		if len(head) > startupSummaryMaxColumns {
			head = "extensions: " + strconv.Itoa(total) + " seen; " +
				strconv.Itoa(retained) + " kept, " + strconv.Itoa(dropped) +
				" dropped; /doctor extensions"
		}
		return renderStartupNoteSourceLines([]string{head}, groups)
	}
	duplicateCount := total - unique
	head := "extensions: " + strconv.Itoa(total) + " startup notes, " + strconv.Itoa(unique) + " unique"
	if duplicateCount > 0 {
		head += ", " + strconv.Itoa(duplicateCount) + " repeated"
	}
	if omittedNoncritical > 0 {
		head += "; " + strconv.Itoa(omittedNoncritical) + " more in /doctor extensions"
	} else {
		head += "; details: /doctor extensions"
	}
	if len(head) > startupSummaryMaxColumns {
		head = "extensions: " + strconv.Itoa(total) + " notes, " + strconv.Itoa(unique) + " unique"
		if duplicateCount > 0 {
			head += ", " + strconv.Itoa(duplicateCount) + " repeated"
		}
		if omittedNoncritical > 0 {
			head += "; " + strconv.Itoa(omittedNoncritical) + " more in /doctor extensions"
		} else {
			head += "; /doctor extensions"
		}
	}

	return renderStartupNoteSourceLines([]string{head}, groups)
}

func renderStartupNoteSourceLines(lines []string, groups []startupNoteGroup) []string {
	const prefix = "sources: "
	line := prefix
	for _, group := range groups {
		if group.Total == 0 {
			continue
		}
		item := string(group.Category) + " " + strconv.Itoa(group.Unique)
		if group.Total != group.Unique {
			item += "/" + strconv.Itoa(group.Total)
		}
		separator := ""
		if line != prefix {
			separator = ", "
		}
		if len(line)+len(separator)+len(item) > startupSummaryMaxColumns && line != prefix {
			lines = append(lines, line)
			line = prefix + item
			continue
		}
		line += separator + item
	}
	if line != prefix {
		lines = append(lines, line)
	}
	if len(lines) > startupSummaryMaxLines {
		// Seven fixed category labels and machine-sized integers fit in the two
		// source lines above. Keep this guard as part of the API's hard bound if
		// the category list grows later.
		lines = lines[:startupSummaryMaxLines]
	}
	return lines
}
