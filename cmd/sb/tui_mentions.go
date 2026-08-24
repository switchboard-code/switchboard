package main

// @path mentions: completion while typing, attachment on submit. The
// completion saves the typing; the attachment saves a tool round trip, which
// on a small local model is the difference between answering from the file
// and hallucinating it. A token that resolves to nothing is left alone — an
// email address is not a file, and the agent can still read paths itself.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/terminaltext"
	workspacefs "github.com/switchboard-code/switchboard/internal/workspace"
)

const (
	mentionMaxResults         = 8
	mentionMaxAttempts        = 8
	mentionFileCap            = 32 << 10
	mentionDiagnosticLabelCap = 160
	mentionListTTL            = 5 * time.Second

	// mentionImageCap is the strictest per-image limit among the surfaces
	// this program speaks; refusing above it here beats a provider error
	// after the upload.
	mentionImageCap = 5 << 20
)

// mentionImageTypes maps the extensions every reachable surface accepts to
// their media types. A mentioned image attaches as an image block rather
// than as text, when the target has evidence of taking one.
var mentionImageTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// mentionToken returns the @-token the cursor is completing, or "".
func mentionToken(value string) string {
	fragment, _, ok := mentionAt(value, len([]rune(value)))
	if !ok {
		return ""
	}
	return fragment
}

type mentionSpan struct {
	start  int
	cursor int
	end    int
}

// mentionAt returns the unquoted mention fragment immediately before a rune
// cursor and the whole token around it. Completion therefore works in the
// middle of a prompt and replaces only that token, not whatever came after
// the cursor.
func mentionAt(value string, cursor int) (string, mentionSpan, bool) {
	runes := []rune(value)
	cursor = workspaceClamp(cursor, 0, len(runes))
	start := cursor
	for start > 0 && !unicode.IsSpace(runes[start-1]) {
		start--
	}
	end := cursor
	for end < len(runes) && !unicode.IsSpace(runes[end]) {
		end++
	}
	if cursor-start < 2 || runes[start] != '@' {
		return "", mentionSpan{}, false
	}
	fragment := string(runes[start+1 : cursor])
	if fragment == "" || strings.HasPrefix(fragment, `"`) {
		return "", mentionSpan{}, false
	}
	return fragment, mentionSpan{start: start, cursor: cursor, end: end}, true
}

func textareaCursorRuneOffset(input textarea.Model) int {
	lines := strings.Split(input.Value(), "\n")
	row := workspaceClamp(input.Line(), 0, max(len(lines)-1, 0))
	offset := 0
	for i := 0; i < row; i++ {
		offset += len([]rune(lines[i])) + 1
	}
	info := input.LineInfo()
	column := workspaceClamp(info.StartColumn+info.ColumnOffset, 0, len([]rune(lines[row])))
	return offset + column
}

func (m *tuiModel) currentMention() (string, mentionSpan, bool) {
	if strings.HasPrefix(m.ta.Value(), "/") {
		return "", mentionSpan{}, false
	}
	return mentionAt(m.ta.Value(), textareaCursorRuneOffset(m.ta))
}

func (m *tuiModel) mentionMatches() []string {
	frag, _, ok := m.currentMention()
	if !ok {
		return nil
	}
	if m.mentionList == nil {
		if m.mentionResultQuery != frag {
			return nil
		}
		if m.workspaceRuntime != nil && m.mentionResultEpoch != m.workspaceRuntime.epoch.Load() {
			return nil
		}
		return m.mentionResults
	}

	// A small explicit inventory remains useful to deterministic tests and
	// embedders. Production keeps this nil and filters the shared index in the
	// background through refreshMentionMatches.
	fragLower := strings.ToLower(frag)
	var exact, contains []string
	for _, f := range m.mentionList {
		lower := strings.ToLower(f)
		base := strings.ToLower(filepath.Base(f))
		switch {
		case strings.HasPrefix(base, fragLower) || strings.HasPrefix(lower, fragLower):
			exact = append(exact, f)
		case strings.Contains(lower, fragLower):
			contains = append(contains, f)
		}
	}
	sort.Slice(exact, func(i, j int) bool { return len(exact[i]) < len(exact[j]) })
	out := append(exact, contains...)
	if len(out) > mentionMaxResults {
		out = out[:mentionMaxResults]
	}
	return out
}

func (m *tuiModel) mentionsVisible() bool {
	return m.dlg == nil && !m.sugClosed && len(m.mentionMatches()) > 0
}

func (m *tuiModel) mentionsView() string {
	matches := m.mentionMatches()
	if len(matches) == 0 {
		return ""
	}
	if m.mentionSel >= len(matches) {
		m.mentionSel = len(matches) - 1
	}
	var rows []string
	for i, f := range matches {
		// A checkout can contain control bytes in a filename. Paint the same
		// safe spelling acceptance inserts; never hand repository-controlled
		// bytes to the terminal parser.
		row := " " + terminaltext.Escape(formatMention(f))
		if i == m.mentionSel {
			row = m.th.selected.Render(row)
		} else {
			row = m.th.dim.Render(row)
		}
		rows = append(rows, row)
	}
	return strings.Join(rows, "\n")
}

func (m *tuiModel) acceptMention() {
	matches := m.mentionMatches()
	if len(matches) == 0 {
		return
	}
	if m.mentionSel >= len(matches) {
		m.mentionSel = 0
	}
	_, span, ok := m.currentMention()
	if !ok {
		return
	}
	runes := []rune(m.ta.Value())
	deleteRight := span.end - span.cursor
	// Replace one existing horizontal delimiter with the completion's own
	// trailing space so the cursor lands after it, ready for the next word.
	if span.end < len(runes) && runes[span.end] == ' ' {
		deleteRight++
	}
	for i := span.start; i < span.cursor; i++ {
		m.ta, _ = m.ta.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	}
	for i := 0; i < deleteRight; i++ {
		m.ta, _ = m.ta.Update(tea.KeyMsg{Type: tea.KeyDelete})
	}
	m.ta.InsertString(formatMention(matches[m.mentionSel]) + " ")
	m.resetHistoryNavigation()
	m.mentionSel = 0
	m.growInput()
}

type mentionMatchesMsg struct {
	request uint64
	query   string
	epoch   uint64
	paths   []string
	err     error
}

// refreshMentionMatches schedules both inventory refresh and fuzzy filtering
// off the event loop. Only one query runs at a time; when typing outruns it,
// its completion immediately schedules the newest fragment and never paints
// stale matches.
func (m *tuiModel) refreshMentionMatches() tea.Cmd {
	query, _, ok := m.currentMention()
	if !ok || m.mentionList != nil {
		return nil
	}
	epoch := uint64(0)
	if m.workspaceRuntime != nil {
		epoch = m.workspaceRuntime.epoch.Load()
	}
	fresh := m.mentionResultQuery == query && m.mentionResultEpoch == epoch &&
		time.Since(m.mentionListAt) < mentionListTTL
	if fresh || m.mentionLoading {
		return nil
	}
	if m.workspaceRuntime == nil {
		m.workspaceRuntime = newWorkspaceRuntime(m.app.workspace)
	}
	m.mentionRequest++
	request := m.mentionRequest
	m.mentionLoading = true
	forceRefresh := m.mentionListAt.IsZero() || time.Since(m.mentionListAt) >= mentionListTTL
	runtime := m.workspaceRuntime
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		paths, resultEpoch, err := runtime.completeFiles(ctx, query, mentionMaxResults, forceRefresh)
		return mentionMatchesMsg{request: request, query: query, epoch: resultEpoch, paths: paths, err: err}
	}
}

func (m *tuiModel) onMentionMatches(msg mentionMatchesMsg) tea.Cmd {
	if msg.request != m.mentionRequest {
		return nil
	}
	m.mentionLoading = false
	query, _, active := m.currentMention()
	currentEpoch := uint64(0)
	if m.workspaceRuntime != nil {
		currentEpoch = m.workspaceRuntime.epoch.Load()
	}
	if msg.err == nil && msg.epoch == currentEpoch {
		m.mentionResults = msg.paths
		m.mentionResultQuery = msg.query
		m.mentionResultEpoch = msg.epoch
		m.mentionListAt = time.Now()
	} else if msg.err != nil {
		// Brief negative caching keeps an unavailable index from starting an I/O
		// command on every keystroke. Completion is a convenience; the path can
		// still be typed and attached normally.
		m.mentionListAt = time.Now()
		m.mentionResultQuery = msg.query
		m.mentionResultEpoch = currentEpoch
		m.mentionResults = nil
	}
	if active && (query != msg.query || msg.epoch != currentEpoch) {
		return m.refreshMentionMatches()
	}
	return nil
}

// formatMention keeps the common @path form terse and quotes paths whose
// spelling cannot survive whitespace tokenization. Go string quoting gives
// quotes, backslashes, control bytes, and non-UTF-8 path bytes one reversible
// representation instead of inventing another escaping language.
func formatMention(path string) string {
	if !utf8.ValidString(path) || strings.IndexFunc(path, func(r rune) bool {
		return unicode.IsSpace(r) || !strconv.IsPrint(r)
	}) >= 0 || strings.ContainsAny(path, `\"`) {
		return "@" + strconv.Quote(path)
	}
	return "@" + path
}

// promptMentionPaths parses the spellings acceptMention can produce. A bare
// mention ends at whitespace; a quoted mention uses strconv's string grammar
// and can therefore carry spaces without absorbing the prose after it.
func promptMentionPaths(prompt string) []string {
	var paths []string
	for offset := 0; offset < len(prompt); {
		relative := strings.IndexByte(prompt[offset:], '@')
		if relative < 0 {
			break
		}
		at := offset + relative
		offset = at + 1
		if at > 0 {
			previous, _ := utf8.DecodeLastRuneInString(prompt[:at])
			if !unicode.IsSpace(previous) {
				continue // an email address or another mid-token @ is not a mention
			}
		}
		if offset >= len(prompt) {
			continue
		}

		if prompt[offset] == '"' {
			end := offset + 1
			for end < len(prompt) {
				switch prompt[end] {
				case '\\':
					end += 2
					continue
				case '"':
					if path, err := strconv.Unquote(prompt[offset : end+1]); err == nil && path != "" {
						paths = append(paths, path)
					}
					offset = end + 1
					end = len(prompt)
					continue
				}
				end++
			}
			continue
		}

		end := offset
		for end < len(prompt) {
			r, size := utf8.DecodeRuneInString(prompt[end:])
			if unicode.IsSpace(r) {
				break
			}
			end += size
		}
		if path := strings.TrimRight(prompt[offset:end], ".,;:!?"); path != "" {
			paths = append(paths, path)
		}
		offset = end
	}
	return paths
}

// expandMentions attaches the contents of every mentioned file to the prompt.
// The prompt keeps the @path where the user typed it — that is what they
// said — and the attachments follow, labelled, so the model knows why they
// are there. The augmented prompt is what the session records. A mentioned
// image comes back as an image block instead of text, with a labelled line
// in the prompt tying the attachment to the mention.
func (m *tuiModel) expandMentions(prompt string) (string, []provider.Image) {
	return expandPromptMentions(m.app.workspace, prompt)
}

// expandPromptMentions is shared by the interactive surfaces so `/tN prompt`
// and an ordinary prompt assemble identical text and image blocks.
func expandPromptMentions(workspace, prompt string) (string, []provider.Image) {
	paths := promptMentionPaths(prompt)
	if len(paths) == 0 {
		return prompt, nil
	}
	root, rootErr := workspacefs.Open(workspace)
	return expandPromptMentionPaths(prompt, paths, root, rootErr)
}

type mentionWorkspaceRoot interface {
	Read(string, int64) (workspacefs.Document, error)
	ReadBinary(string, int64) (workspacefs.Document, error)
}

// expandPromptMentionPaths keeps all accounting local to one prompt. In
// particular, a missing path still consumes an attempt and a duplicate never
// opens the workspace again. That makes explicit @mentions bounded even when
// none of them can become an attachment.
func expandPromptMentionPaths(prompt string, paths []string, root mentionWorkspaceRoot, rootErr error) (string, []provider.Image) {
	var attached []string
	var images []provider.Image
	seen := make(map[string]struct{}, min(len(paths), mentionMaxAttempts+1))
	attempts := 0
	omitted := false
	rootFailureReported := false
	for _, token := range paths {
		if _, duplicate := seen[token]; duplicate {
			continue
		}
		seen[token] = struct{}{}
		if attempts >= mentionMaxAttempts {
			omitted = true
			break
		}
		attempts++
		if rootErr != nil || root == nil {
			if !rootFailureReported {
				attached = append(attached, "Mentioned paths were not attached: the workspace could not be opened securely.")
				rootFailureReported = true
			}
			continue
		}
		mediaType, isImage := mentionImageTypes[strings.ToLower(filepath.Ext(token))]
		limit := int64(workspacefs.DefaultDocumentLimit)
		if isImage {
			limit = mentionImageCap
		}
		readFile := root.Read
		if isImage {
			readFile = root.ReadBinary
		}
		doc, err := readFile(token, limit)
		if err != nil {
			label := mentionOmissionLabel(token)
			switch {
			case errors.Is(err, workspacefs.ErrOutsideRoot):
				attached = append(attached, fmt.Sprintf("%s (mentioned above) was not attached: the path resolves outside the workspace.", label))
			case errors.Is(err, workspacefs.ErrTooLarge):
				kind := "file"
				if isImage {
					kind = "image"
				}
				attached = append(attached, fmt.Sprintf("%s (mentioned above) was not attached: it exceeds the %d-byte %s cap.",
					label, limit, kind))
			case errors.Is(err, workspacefs.ErrBinary):
				attached = append(attached, fmt.Sprintf("%s (mentioned above) was not attached: it is binary data, not text.", label))
			case errors.Is(err, workspacefs.ErrStaleLocation):
				attached = append(attached, fmt.Sprintf("%s (mentioned above) was not attached: it changed while it was being read.", label))
			case errors.Is(err, workspacefs.ErrSecureReadUnsupported):
				attached = append(attached, fmt.Sprintf("%s (mentioned above) was not attached: secure workspace reads are unsupported on this platform.", label))
			case errors.Is(err, workspacefs.ErrNotRegular):
				attached = append(attached, fmt.Sprintf("%s (mentioned above) was not attached: it is not a regular file.", label))
			case errors.Is(err, fs.ErrNotExist):
				// A plain missing path stays quiet. It may be prose that happens
				// to look like a mention, and the agent can still read it later.
			case errors.Is(err, fs.ErrPermission):
				attached = append(attached, fmt.Sprintf("%s (mentioned above) was not attached: access was denied.", label))
			default:
				attached = append(attached, fmt.Sprintf("%s (mentioned above) was not attached: a secure workspace read failed.", label))
			}
			continue
		}

		label := mentionAttachmentLabel(token, doc.Location.Path)
		if isImage {
			images = append(images, provider.Image{MediaType: mediaType, Data: doc.Content})
			attached = append(attached, fmt.Sprintf("Image %s (mentioned above) is attached.", label))
			continue
		}

		text := string(doc.Content)
		if len(text) > mentionFileCap {
			cut := mentionFileCap
			for cut > 0 && !utf8.ValidString(text[:cut]) {
				cut--
			}
			// Keep complete in-cap credentials for the one outbound secret gate,
			// where the user can redact, send, or drop. A credential that straddles
			// this smaller display cap is replaced here because either fragment on
			// its own would fall below the scanner's issuer length floor.
			var sourceCut int
			text, sourceCut = credential.SafePrefixForTruncation(text, cut)
			switch {
			case sourceCut < cut:
				text += fmt.Sprintf("\n[truncated at %d bytes before a credential crossed the %d-byte cap; read the file for the rest]", sourceCut, mentionFileCap)
			case cut == mentionFileCap:
				text += fmt.Sprintf("\n[truncated at %d bytes; read the file for the rest]", mentionFileCap)
			default:
				text += fmt.Sprintf("\n[truncated at %d bytes to preserve UTF-8 before the %d-byte cap; read the file for the rest]", cut, mentionFileCap)
			}
		}
		attached = append(attached, fmt.Sprintf("Contents of %s (mentioned above):\n```\n%s\n```", label, strings.TrimRight(text, "\n")))
	}
	if omitted {
		attached = append(attached, fmt.Sprintf("Additional distinct @mentions were not inspected: at most %d paths are inspected per prompt.", mentionMaxAttempts))
	}
	if len(attached) == 0 {
		return prompt, nil
	}
	return prompt + "\n\n" + strings.Join(attached, "\n\n"), images
}

// mentionOmissionLabel identifies a failed mention without copying an
// absolute host path or its directories into a model-visible diagnostic.
// Credential-shaped substrings and terminal controls are removed before the
// label is bounded, so truncation cannot reveal a secret prefix or split
// UTF-8.
func mentionOmissionLabel(token string) string {
	label := filepath.Base(filepath.Clean(token))
	if label == "." || label == string(filepath.Separator) || strings.TrimSpace(label) == "" {
		label = "mentioned path"
	}
	return sanitizeMentionLabel(label)
}

// A successful read gives us a canonical workspace-relative path. Prefer it
// to the spelling in the prompt so an absolute @mention is never repeated in
// generated attachment text.
func mentionAttachmentLabel(token, relative string) string {
	label := relative
	if label == "" {
		label = filepath.Base(filepath.Clean(token))
	}
	return sanitizeMentionLabel(label)
}

func sanitizeMentionLabel(label string) string {
	if !utf8.ValidString(label) {
		label = strconv.Quote(label)
	}
	label = credential.Redact(label, credential.ScanPrompt(label))
	label = terminaltext.Escape(label)
	if len(label) <= mentionDiagnosticLabelCap {
		return label
	}
	cut := mentionDiagnosticLabelCap - len("...")
	for cut > 0 && !utf8.ValidString(label[:cut]) {
		cut--
	}
	return label[:cut] + "..."
}
