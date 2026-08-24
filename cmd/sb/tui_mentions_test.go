package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/switchboard-code/switchboard/internal/config"
	"github.com/switchboard-code/switchboard/internal/provider"
	workspacefs "github.com/switchboard-code/switchboard/internal/workspace"
)

type mentionReadResult struct {
	doc workspacefs.Document
	err error
}

type countingMentionRoot struct {
	mu      sync.Mutex
	results map[string]mentionReadResult
	calls   map[string]int
}

func newCountingMentionRoot(results map[string]mentionReadResult) *countingMentionRoot {
	return &countingMentionRoot{results: results, calls: make(map[string]int)}
}

func (r *countingMentionRoot) Read(name string, _ int64) (workspacefs.Document, error) {
	return r.read(name)
}

func (r *countingMentionRoot) ReadBinary(name string, _ int64) (workspacefs.Document, error) {
	return r.read(name)
}

func (r *countingMentionRoot) read(name string) (workspacefs.Document, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[name]++
	if result, ok := r.results[name]; ok {
		return result.doc, result.err
	}
	return workspacefs.Document{}, os.ErrNotExist
}

func (r *countingMentionRoot) count(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[name]
}

func (r *countingMentionRoot) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	total := 0
	for _, count := range r.calls {
		total += count
	}
	return total
}

func TestMentionTokenFindsOnlyTheActiveToken(t *testing.T) {
	cases := []struct {
		input, want string
	}{
		{"look at @cmd/sb", "cmd/sb"},
		{"@a", "a"},
		{"@", ""},                         // nothing typed yet
		{"mail me user@example.com ", ""}, // cursor past a space
		{"plain text", ""},
		{"two @first then @sec", "sec"},
	}
	for _, c := range cases {
		if got := mentionToken(c.input); got != c.want {
			t.Errorf("mentionToken(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestExpandMentionsAttachesOnlyRealFiles(t *testing.T) {
	m := testModel(t)
	ws := t.TempDir()
	m.app.workspace = ws
	if err := os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("the contents"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, images := m.expandMentions("summarize @notes.txt and email bob@example.com")
	if !strings.Contains(got, "the contents") {
		t.Fatalf("mentioned file was not attached:\n%s", got)
	}
	if !strings.Contains(got, "summarize @notes.txt") {
		t.Fatal("the prompt text itself was altered")
	}
	if strings.Contains(got, "Contents of example.com") || strings.Count(got, "Contents of") != 1 {
		t.Fatalf("something that is not a file was attached:\n%s", got)
	}
	if len(images) != 0 {
		t.Fatalf("a text mention produced image blocks: %d", len(images))
	}

	if got, _ := m.expandMentions("no mentions here"); got != "no mentions here" {
		t.Fatalf("a mention-free prompt should pass through untouched, got %q", got)
	}
}

func TestMentionCompletionQuotesAndAttachesAPathWithSpaces(t *testing.T) {
	m := testModel(t)
	ws := t.TempDir()
	m.app.workspace = ws
	if err := os.WriteFile(filepath.Join(ws, "design notes.txt"), []byte("space-bearing evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Pin the completion inventory so the test exercises selection and parsing
	// without depending on a filesystem walk's ordering.
	m.mentionList = []string{"design notes.txt"}
	m.mentionListAt = time.Now()
	m.ta.SetValue("summarize @design")

	m.acceptMention()
	prompt := m.ta.Value()
	if want := `summarize @"design notes.txt" `; prompt != want {
		t.Fatalf("completed mention = %q, want %q", prompt, want)
	}

	expanded, images := m.expandMentions(prompt)
	if len(images) != 0 {
		t.Fatalf("text path produced %d image blocks", len(images))
	}
	if !strings.Contains(expanded, "space-bearing evidence") ||
		!strings.Contains(expanded, "Contents of design notes.txt") {
		t.Fatalf("quoted path was not attached:\n%s", expanded)
	}
}

func TestQuotedMentionRoundTripsQuotesAndBackslashes(t *testing.T) {
	path := `docs/a "quoted" \\ path.txt`
	mention := formatMention(path)
	got := promptMentionPaths("read " + mention + " next")
	if len(got) != 1 || got[0] != path {
		t.Fatalf("promptMentionPaths(%q) = %#v, want %q", mention, got, path)
	}
}

func TestMentionCompletionEscapesTerminalControls(t *testing.T) {
	m := testModel(t)
	path := "notes\x1b]0;spoof\a\u202e.txt"
	m.mentionList = []string{path}
	m.mentionListAt = time.Now()
	m.ta.SetValue("inspect @notes")

	view := stripANSI(m.mentionsView())
	for _, unsafe := range []string{"\x1b", "\a", "\u202e"} {
		if strings.Contains(view, unsafe) {
			t.Fatalf("mention picker retained terminal control %q: %q", unsafe, view)
		}
	}

	m.acceptMention()
	inserted := m.ta.Value()
	for _, unsafe := range []string{"\x1b", "\a", "\u202e"} {
		if strings.Contains(inserted, unsafe) {
			t.Fatalf("accepted mention retained terminal control %q: %q", unsafe, inserted)
		}
	}
	if got := promptMentionPaths(inserted); len(got) != 1 || got[0] != path {
		t.Fatalf("safe mention spelling did not round-trip: %q -> %#v", inserted, got)
	}
}

func TestExpandMentionsRefusesTraversalOutsideWorkspace(t *testing.T) {
	parent := t.TempDir()
	ws := filepath.Join(parent, "workspace")
	if err := os.Mkdir(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	const outside = "outside-file-content-must-not-cross-the-boundary"
	if err := os.WriteFile(filepath.Join(parent, "secret.txt"), []byte(outside), 0o600); err != nil {
		t.Fatal(err)
	}

	expanded, images := expandPromptMentions(ws, "inspect @../secret.txt")
	if strings.Contains(expanded, outside) || len(images) != 0 {
		t.Fatalf("workspace traversal attached outside content: prompt=%q images=%d", expanded, len(images))
	}
	if !strings.Contains(expanded, "not attached") || !strings.Contains(expanded, "outside the workspace") {
		t.Fatalf("workspace traversal was silently ignored:\n%s", expanded)
	}
}

func TestExpandMentionsRefusesSymlinkOutsideWorkspace(t *testing.T) {
	parent := t.TempDir()
	ws := filepath.Join(parent, "workspace")
	if err := os.Mkdir(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "outside.png")
	if err := os.WriteFile(outside, []byte("outside-image-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "escape.png")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	expanded, images := expandPromptMentions(ws, "inspect @escape.png")
	if len(images) != 0 || strings.Contains(expanded, "outside-image-content") {
		t.Fatalf("symlink escape attached outside content: prompt=%q images=%d", expanded, len(images))
	}
	if !strings.Contains(expanded, "not attached") || !strings.Contains(expanded, "outside the workspace") {
		t.Fatalf("symlink escape was silently ignored:\n%s", expanded)
	}
}

func TestMentionedImageAttachesAsABlock(t *testing.T) {
	m := testModel(t)
	ws := t.TempDir()
	m.app.workspace = ws
	// The bytes only have to be bytes: the block carries them as they are,
	// and no image parser runs on this side of the wire.
	if err := os.WriteFile(filepath.Join(ws, "shot.png"), []byte{0x89, 0x50, 0x4e, 0x47, 1, 2, 3}, 0o600); err != nil {
		t.Fatal(err)
	}

	got, images := m.expandMentions("what is wrong in @shot.png here")
	if len(images) != 1 || images[0].MediaType != "image/png" || len(images[0].Data) != 7 {
		t.Fatalf("image did not attach as a block: %+v", images)
	}
	if !strings.Contains(got, "Image shot.png (mentioned above) is attached.") {
		t.Fatalf("the prompt must tie the attachment to the mention:\n%s", got)
	}
	if strings.Contains(got, "Contents of shot.png") {
		t.Fatalf("an image must not also attach as text:\n%s", got)
	}
}

func TestOversizedImageIsRefusedWithItsReason(t *testing.T) {
	m := testModel(t)
	ws := t.TempDir()
	m.app.workspace = ws
	big := make([]byte, mentionImageCap+1)
	if err := os.WriteFile(filepath.Join(ws, "huge.png"), big, 0o600); err != nil {
		t.Fatal(err)
	}

	got, images := m.expandMentions("look at @huge.png please")
	if len(images) != 0 {
		t.Fatal("an oversized image must not attach")
	}
	if !strings.Contains(got, "was not attached") {
		t.Fatalf("the refusal must be said in the prompt, not silent:\n%s", got)
	}
}

func TestMentionedTextKeepsItsBoundedTruncationBehavior(t *testing.T) {
	m := testModel(t)
	ws := t.TempDir()
	m.app.workspace = ws
	text := strings.Repeat("x", mentionFileCap+1)
	if err := os.WriteFile(filepath.Join(ws, "long.txt"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}

	got, images := m.expandMentions("summarize @long.txt")
	if len(images) != 0 {
		t.Fatalf("text mention produced %d image blocks", len(images))
	}
	if !strings.Contains(got, strings.Repeat("x", mentionFileCap)) ||
		!strings.Contains(got, "[truncated at 32768 bytes; read the file for the rest]") {
		t.Fatalf("long text mention lost bounded truncation:\n%s", got)
	}
}

func TestMentionedTextTruncatesAtAUTF8Boundary(t *testing.T) {
	m := testModel(t)
	ws := t.TempDir()
	m.app.workspace = ws
	text := strings.Repeat("x", mentionFileCap-1) + "€tail"
	if err := os.WriteFile(filepath.Join(ws, "unicode.txt"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}

	got, images := m.expandMentions("summarize @unicode.txt")
	if len(images) != 0 {
		t.Fatalf("text mention produced %d image blocks", len(images))
	}
	if !utf8.ValidString(got) || strings.ContainsRune(got, utf8.RuneError) {
		t.Fatalf("truncated mention is not valid UTF-8: %q", got[len(got)-128:])
	}
	if !strings.Contains(got, "[truncated at 32767 bytes to preserve UTF-8 before the 32768-byte cap; read the file for the rest]") {
		t.Fatalf("truncated mention did not report its UTF-8 boundary:\n%s", got[len(got)-256:])
	}
}

func TestMentionedTextRedactsCredentialBeforeAttachmentCap(t *testing.T) {
	secret := "ghp_" + strings.Repeat("S", 36)
	text := strings.Repeat("x", mentionFileCap-len(secret)+1) + secret + "tail"
	root := newCountingMentionRoot(map[string]mentionReadResult{
		"secret.txt": {doc: workspacefs.Document{
			Location: workspacefs.Location{Path: "secret.txt"},
			Content:  []byte(text),
		}},
	})
	prompt := "inspect @secret.txt"

	got, images := expandPromptMentionPaths(prompt, promptMentionPaths(prompt), root, nil)
	if len(images) != 0 {
		t.Fatalf("text mention produced %d image blocks", len(images))
	}
	if strings.Contains(got, secret) || strings.Contains(got, "ghp_SSSS") {
		t.Fatalf("attachment cap exposed a credential or its boundary fragment: %q", got[len(got)-256:])
	}
	if !strings.Contains(got, "[redacted: a GitHub token]") {
		t.Fatalf("credential redaction was not visible in the attachment: %q", got[len(got)-256:])
	}
}

func TestNonImageBinaryMentionIsRefusedAsText(t *testing.T) {
	m := testModel(t)
	ws := t.TempDir()
	m.app.workspace = ws
	if err := os.WriteFile(filepath.Join(ws, "bytes.dat"), []byte{'x', 0, 'y'}, 0o600); err != nil {
		t.Fatal(err)
	}

	got, images := m.expandMentions("inspect @bytes.dat")
	if len(images) != 0 || strings.ContainsRune(got, 0) {
		t.Fatalf("binary text mention crossed as content: prompt=%q images=%d", got, len(images))
	}
	if !strings.Contains(got, "binary data, not text") {
		t.Fatalf("binary text refusal was silent:\n%s", got)
	}
}

func TestMentionSafetyRefusalIsExplicitButMissingPathStaysSilent(t *testing.T) {
	m := testModel(t)
	ws := t.TempDir()
	m.app.workspace = ws
	if err := os.Mkdir(filepath.Join(ws, "directory"), 0o700); err != nil {
		t.Fatal(err)
	}

	got, images := m.expandMentions("inspect @directory and @missing.txt")
	if len(images) != 0 || !strings.Contains(got, "directory (mentioned above) was not attached: it is not a regular file") {
		t.Fatalf("special-file refusal was not explicit: prompt=%q images=%d", got, len(images))
	}
	if strings.Contains(got, "missing.txt (mentioned above)") {
		t.Fatalf("ordinary missing path should stay silent:\n%s", got)
	}
}

func TestMentionAttemptsAreDistinctAndCappedBeforeReads(t *testing.T) {
	root := newCountingMentionRoot(nil)
	parts := make([]string, 0, 80)
	for range 64 {
		parts = append(parts, "@missing-0.txt")
	}
	for i := 1; i < mentionMaxAttempts; i++ {
		parts = append(parts, fmt.Sprintf("@missing-%d.txt", i))
	}
	parts = append(parts, "@would-attach.txt")
	prompt := "inspect " + strings.Join(parts, " ")
	paths := promptMentionPaths(prompt)

	got, images := expandPromptMentionPaths(prompt, paths, root, nil)
	if len(images) != 0 {
		t.Fatalf("missing mentions produced %d image blocks", len(images))
	}
	if root.count("missing-0.txt") != 1 {
		t.Fatalf("a duplicate missing mention was opened %d times, want 1", root.count("missing-0.txt"))
	}
	if root.total() != mentionMaxAttempts {
		t.Fatalf("secure reads = %d, want the %d-attempt cap", root.total(), mentionMaxAttempts)
	}
	if root.count("would-attach.txt") != 0 {
		t.Fatal("a path beyond the distinct-attempt cap was opened")
	}
	if !strings.Contains(got, "Additional distinct @mentions were not inspected") {
		t.Fatalf("the attempt cap was silent:\n%s", got)
	}
	if strings.Contains(got, "missing-0.txt (mentioned above)") {
		t.Fatalf("ordinary missing mentions should remain quiet:\n%s", got)
	}
}

func TestDuplicateSuccessfulMentionAttachesOnce(t *testing.T) {
	const path = "notes.txt"
	root := newCountingMentionRoot(map[string]mentionReadResult{
		path: {doc: workspacefs.Document{
			Location: workspacefs.Location{Path: path},
			Content:  []byte("one snapshot"),
		}},
	})
	prompt := "compare @notes.txt with @notes.txt and @notes.txt"

	got, images := expandPromptMentionPaths(prompt, promptMentionPaths(prompt), root, nil)
	if len(images) != 0 || root.count(path) != 1 || strings.Count(got, "Contents of notes.txt") != 1 {
		t.Fatalf("duplicate attachment was not deduplicated: reads=%d images=%d prompt=%q", root.count(path), len(images), got)
	}
}

func TestMentionRootOpenFailureIsVisibleWithoutEchoingTheRoot(t *testing.T) {
	secret := "ghp_" + strings.Repeat("A", 36)
	root := filepath.Join(t.TempDir(), "missing-"+secret+"\x1b")
	prompt := "inspect @evidence.txt"

	got, images := expandPromptMentions(root, prompt)
	if len(images) != 0 || !strings.Contains(got, "workspace could not be opened securely") {
		t.Fatalf("workspace-open failure was silent: prompt=%q images=%d", got, len(images))
	}
	generated := strings.TrimPrefix(got, prompt+"\n\n")
	for _, unsafe := range []string{root, secret, "\x1b"} {
		if strings.Contains(generated, unsafe) {
			t.Fatalf("workspace-open diagnostic leaked %q: %q", unsafe, generated)
		}
	}
}

func TestMentionInvalidPathIOFailureIsVisibleAndSanitized(t *testing.T) {
	workspace := t.TempDir()
	token := "naïve\x00control.txt"
	prompt := "inspect " + formatMention(token)

	got, images := expandPromptMentions(workspace, prompt)
	if len(images) != 0 || !strings.Contains(got, "a secure workspace read failed") {
		t.Fatalf("invalid-path I/O failure was silent: prompt=%q images=%d", got, len(images))
	}
	generated := strings.TrimPrefix(got, prompt+"\n\n")
	if !utf8.ValidString(generated) || strings.ContainsRune(generated, 0) || !strings.Contains(generated, "naïve") {
		t.Fatalf("invalid-path diagnostic was not safe Unicode: %q", generated)
	}
}

func TestMentionReadFailuresUseBoundedRedactedLabels(t *testing.T) {
	secret := "ghp_" + strings.Repeat("B", 36)
	permissionPath := strings.Repeat("文", 90) + "-naïve\u202e-" + secret + ".txt"
	ioPath := "io\x1b-" + secret + ".txt"
	root := newCountingMentionRoot(map[string]mentionReadResult{
		permissionPath: {err: os.ErrPermission},
		ioPath:         {err: errors.New("device error at /private/" + secret + "\x1b")},
	})
	prompt := "inspect " + formatMention(permissionPath) + " " + formatMention(ioPath)

	got, images := expandPromptMentionPaths(prompt, promptMentionPaths(prompt), root, nil)
	if len(images) != 0 || !strings.Contains(got, "access was denied") || !strings.Contains(got, "a secure workspace read failed") {
		t.Fatalf("read failures were not explicit: prompt=%q images=%d", got, len(images))
	}
	generated := strings.TrimPrefix(got, prompt+"\n\n")
	if !utf8.ValidString(generated) {
		t.Fatalf("generated diagnostics are not valid UTF-8: %q", generated)
	}
	for _, unsafe := range []string{secret, "\x1b", "\u202e"} {
		if strings.Contains(generated, unsafe) {
			t.Fatalf("generated diagnostics retained unsafe text %q: %q", unsafe, generated)
		}
	}
	if !strings.Contains(generated, "[redacted: a GitHub token]") {
		t.Fatalf("credential-shaped filename was not visibly redacted: %q", generated)
	}
	for _, paragraph := range strings.Split(generated, "\n\n") {
		label, _, ok := strings.Cut(paragraph, " (mentioned above)")
		if ok && len(label) > mentionDiagnosticLabelCap {
			t.Fatalf("diagnostic label is %d bytes, cap is %d: %q", len(label), mentionDiagnosticLabelCap, label)
		}
	}
}

func TestMentionAttemptCapConcurrentStress(t *testing.T) {
	root := newCountingMentionRoot(nil)
	parts := make([]string, 0, 256)
	for i := 0; i < 256; i++ {
		parts = append(parts, fmt.Sprintf("@missing-%d.txt", i%32))
	}
	prompt := "inspect " + strings.Join(parts, " ")
	paths := promptMentionPaths(prompt)

	const workers = 24
	const rounds = 20
	errCh := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				got, images := expandPromptMentionPaths(prompt, paths, root, nil)
				if len(images) != 0 || !strings.Contains(got, "Additional distinct @mentions were not inspected") {
					errCh <- fmt.Errorf("unbounded stress result: images=%d prompt=%q", len(images), got)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	if want := workers * rounds * mentionMaxAttempts; root.total() != want {
		t.Fatalf("stress secure reads = %d, want %d", root.total(), want)
	}
}

func TestShellContextDrainsIntoThePrompt(t *testing.T) {
	m := testModel(t)
	m.onShellDone(shellDoneMsg{command: "git status", output: "clean tree"})

	got := m.shellContext("what changed?")
	for _, want := range []string{"$ git status", "clean tree", "what changed?"} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt is missing %q:\n%s", want, got)
		}
	}
	if again := m.shellContext("next"); again != "next" {
		t.Fatal("shell context should drain once, not repeat on every turn")
	}
}

func TestTierOverrideUsesExpandedPromptAndImageBlocks(t *testing.T) {
	m := testModel(t)
	workspace := t.TempDir()
	m.app.workspace = workspace
	if err := os.WriteFile(filepath.Join(workspace, "shot.png"), []byte{0x89, 0x50, 0x4e, 0x47}, 0o600); err != nil {
		t.Fatal(err)
	}
	server := capabilityOllama(t, map[string]bool{"vision": true})
	tier := ollamaTier("t1", "vision")
	m.app.config.Tiers = []config.Tier{tier}
	m.app.tier = tier
	m.app.providers = newProviders(server.URL, m.app.config)
	m.onShellDone(shellDoneMsg{command: "git status", output: "clean tree"})

	cmd := m.startTurn("inspect @shot.png", "t1")
	if cmd == nil || !m.turnPlanning {
		t.Fatal("override did not enter planning")
	}
	msg := cmd().(overrideProbeMsg)
	if msg.err != nil {
		t.Fatal(msg.err)
	}
	if len(msg.images) != 1 || !strings.Contains(msg.prompt, "Image shot.png") ||
		!strings.Contains(msg.prompt, "$ git status") || !strings.Contains(msg.prompt, "clean tree") {
		t.Fatalf("override opening lost ordinary prompt construction: prompt=%q images=%d", msg.prompt, len(msg.images))
	}
	if authored, known := msg.opening.AuthoredProjection(); !known || authored != "inspect @shot.png" {
		t.Fatalf("override authored projection = %q known=%v, want exact typed prompt", authored, known)
	}
	m.finishPlanning()
}

func TestOffLadderVisionCheckDoesNotBlockValidOverride(t *testing.T) {
	m := testModel(t)
	workspace := t.TempDir()
	m.app.workspace = workspace
	if err := os.WriteFile(filepath.Join(workspace, "shot.png"), []byte{0x89, 0x50, 0x4e, 0x47}, 0o600); err != nil {
		t.Fatal(err)
	}
	server := capabilityOllama(t, map[string]bool{"vision": true})
	tier := ollamaTier("t2", "vision")
	m.app.config.Tiers = []config.Tier{tier}
	m.app.providers = newProviders(server.URL, m.app.config)
	m.app.tier = config.Tier{ID: "-resumed", Target: provider.RouteTarget{
		Provider: "ollama", Surface: "local", ModelID: "text-only",
	}}

	cmd := m.startTurn("inspect @shot.png", "t2")
	if cmd == nil {
		t.Fatal("valid override was refused against the unrelated current target")
	}
	msg := cmd().(overrideProbeMsg)
	if msg.err != nil || len(msg.images) != 1 {
		t.Fatalf("override result: err=%v images=%d", msg.err, len(msg.images))
	}
	m.finishPlanning()
}

func TestOffLadderImageIsRefusedEvenWhenLadderExists(t *testing.T) {
	m := testModel(t)
	workspace := t.TempDir()
	m.app.workspace = workspace
	if err := os.WriteFile(filepath.Join(workspace, "shot.png"), []byte{0x89, 0x50, 0x4e, 0x47}, 0o600); err != nil {
		t.Fatal(err)
	}
	m.app.config.Tiers = []config.Tier{ollamaTier("t1", "vision")}
	server := capabilityOllama(t, map[string]bool{"text-only": false})
	m.app.providers = newProviders(server.URL, m.app.config)
	m.app.tier = config.Tier{ID: "-resumed", Target: provider.RouteTarget{
		Provider: "ollama", Surface: "local", ModelID: "text-only",
	}}

	cmd := m.startTurn("inspect @shot.png", "")
	if cmd == nil || !m.turnPlanning {
		t.Fatal("off-ladder turn did not enter owned asynchronous planning")
	}
	msg := cmd().(turnPlanMsg)
	if msg.err == nil || !strings.Contains(strings.ToLower(msg.err.Error()), "image") {
		t.Fatalf("off-ladder non-vision target was not refused before launch: %v", msg.err)
	}
	m.finishPlanning()
}

func TestOffLadderParameterizedTargetRefreshesLiveVisionEvidence(t *testing.T) {
	m := testModel(t)
	workspace := t.TempDir()
	m.app.workspace = workspace
	if err := os.WriteFile(filepath.Join(workspace, "shot.png"), []byte{0x89, 0x50, 0x4e, 0x47}, 0o600); err != nil {
		t.Fatal(err)
	}
	server := capabilityOllama(t, map[string]bool{"unlisted-vision": true})
	m.app.providers = newProviders(server.URL, m.app.config)
	target := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "unlisted-vision"}
	target.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: "high"}
	m.app.tier = config.Tier{ID: "-model", Target: target}

	cmd := m.startTurn("inspect @shot.png", "")
	if cmd == nil {
		t.Fatal("off-ladder image turn did not plan")
	}
	msg := cmd().(turnPlanMsg)
	if msg.err != nil || len(msg.images) != 1 {
		t.Fatalf("fresh positive live vision evidence was not honored: err=%v images=%d", msg.err, len(msg.images))
	}
	if vision, known := m.app.providers.probedVision(target); !known || !vision {
		t.Fatalf("parameterized target probe evidence = vision %v known %v", vision, known)
	}
	m.finishPlanning()
}
