package agent

// Project instructions, composed the way the ecosystem writes them.
//
// AGENTS.md and CLAUDE.md are composition formats everywhere they are defined:
// a repository root states the house rules, a package states its own, a
// developer shadows a checked-in file locally without editing it, and a file
// pulls in another with an import. This program honored the filename and
// ignored the format — the workspace root's first hit, whole, and nothing
// else — so a monorepo's package instructions were invisible and a user's own
// standing preferences had nowhere to live.
//
// Order is general to specific, and it is the reading order for a reason: the
// last word should belong to the file closest to the work. The budget is one
// number shared by everything, because the prompt is paid for on every cold
// cache and four composed layers can triple a request as easily as one long
// file. When it binds, the most general layer is dropped first and the result
// says which, since dropping the package's own rules to keep the user's
// defaults would be exactly backwards.
//
// Two refusals. There is no `!cmd` substitution and there will not be: a
// checkout must not get a command executed by the act of being opened, which
// is the same rule that keeps a repository from declaring a /watch verifier.
// And a repository's import may not resolve outside the workspace, because an
// instruction file that can read any path is a file that can read a private
// key into a prompt.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

const (
	// maxInstructionBytes caps everything composed here, together. It is the
	// budget the frozen zone pays on every cold cache.
	maxInstructionBytes = 16 << 10

	// maxImportDepth bounds how far an import chain runs. Two hops covers a
	// root file pulling in a shared section that pulls in a fragment; past
	// that the file is a program, and this reader is not one.
	maxImportDepth = 2

	// maxLayers bounds how many directories are consulted between the
	// repository root and the working directory, so a deep tree cannot turn
	// assembly into a walk of arbitrary length.
	maxLayers = 8

	// maxInstructionSourceBytes bounds the bytes one top-level instruction
	// file and all of its imports may contribute before the much smaller
	// composed-prompt budget is applied. Without a read bound, a checkout could
	// make session assembly consume an arbitrary file merely to discard most of
	// it during rendering.
	maxInstructionSourceBytes int64 = 256 << 10

	// maxInstructionImports bounds filesystem work independently of bytes. A
	// file containing thousands of imports of missing or oversized files must
	// not turn opening a repository into thousands of reads.
	maxInstructionImports = 32
)

// instructionFiles are the names read at each layer, in order. The first hit
// at a layer wins: a directory holding both means them as one instruction set,
// and reading both would double whatever they agree on.
var instructionFiles = []string{"AGENTS.md", "CLAUDE.md"}

// overrideFiles are the uncommitted siblings a developer can use to shadow a
// checked-in file without editing it. They are read after the file they
// shadow, so the local word is the later one.
var overrideFiles = []string{"AGENTS.override.md", "CLAUDE.local.md"}

// userInstructionDirs are the roots a person's own standing instructions live
// in. All three are read because a user who already keeps one for another tool
// means it for this one too, and the alternative is asking them to duplicate a
// file they already maintain.
var userInstructionDirs = []string{".switchboard", ".agents", ".claude"}

type instructionLayer struct {
	label string
	text  string
}

// ProjectInstructions composes the workspace's agent instructions.
//
// The bool reports whether anything was found at all, which is what the caller
// uses to decide whether the system prompt grows a block.
func ProjectInstructions(workspace string) (string, bool) {
	layers := collectInstructionLayers(workspace)
	if len(layers) == 0 {
		return "", false
	}
	return renderInstructionLayers(layers), true
}

func collectInstructionLayers(workspace string) []instructionLayer {
	var layers []instructionLayer
	if home, err := os.UserHomeDir(); err == nil {
		for _, dir := range userInstructionDirs {
			root := filepath.Join(home, dir)
			// The user's own roots are explicit import boundaries: a file
			// there may pull in a neighbour, which is how a person keeps one
			// set of rules in pieces.
			layers = append(layers, readInstructionDir(root, root)...)
		}
	}
	dirs, root := instructionDirs(workspace)
	for _, dir := range dirs {
		layers = append(layers, readInstructionDir(dir, root)...)
	}
	return layers
}

// instructionDirs lists the directories to consult, general to specific: the
// repository root first, then each directory down to the workspace.
//
// The repository root is found by walking up for a .git entry. Without one the
// workspace is the only layer, which is the honest answer for a directory that
// is not a checkout.
func instructionDirs(workspace string) ([]string, string) {
	workspace = filepath.Clean(workspace)
	root := workspace
	for dir := workspace; ; {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			root = dir
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	var dirs []string
	for dir := workspace; ; {
		dirs = append(dirs, dir)
		if dir == root {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	// Collected specific-first; the prompt wants general-first.
	for i, j := 0, len(dirs)-1; i < j; i, j = i+1, j-1 {
		dirs[i], dirs[j] = dirs[j], dirs[i]
	}
	if len(dirs) > maxLayers {
		// Keep the most specific, which are the ones closest to the work.
		dirs = dirs[len(dirs)-maxLayers:]
	}
	return dirs, root
}

// readInstructionDir reads one directory's instruction file and its override
// sibling. boundary is the root an import may not escape.
func readInstructionDir(dir, boundary string) []instructionLayer {
	var out []instructionLayer
	for _, name := range instructionFiles {
		path := filepath.Join(dir, name)
		if text, ok := readInstructionFile(path, boundary); ok {
			out = append(out, instructionLayer{label: path, text: text})
			break
		}
	}
	for _, name := range overrideFiles {
		path := filepath.Join(dir, name)
		if text, ok := readInstructionFile(path, boundary); ok {
			out = append(out, instructionLayer{label: path, text: text})
		}
	}
	return out
}

func readInstructionFile(path, boundary string) (string, bool) {
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			return "", false
		}
		return instructionReadDiagnostic(path, err), true
	}

	reader, err := openInstructionReader(boundary)
	if err != nil {
		return instructionReadDiagnostic(path, err), true
	}
	defer reader.close()

	data, err := reader.read(path)
	if err != nil {
		return instructionReadDiagnostic(path, err), true
	}
	if strings.TrimSpace(string(data)) == "" {
		return "", false
	}
	text := expandImports(string(data), filepath.Dir(path), reader, map[string]bool{filepath.Clean(path): true}, 0)
	if err := reader.validateRoot(); err != nil {
		return instructionReadDiagnostic(path, err), true
	}
	return text, true
}

// instructionReader owns one anchored root for a top-level instruction file
// and every file it imports. os.Root confines intermediate symlinks while the
// identity checks make a rename or retarget during assembly a refusal rather
// than a read from whichever tree happened to win the race.
type instructionReader struct {
	logicalRoot string
	root        *os.Root
	rootInfo    os.FileInfo
	remaining   int64
	imports     int

	// afterOpen is a deterministic fault-injection seam for the replacement-race
	// tests. Product readers leave it nil.
	afterOpen func()

	// beforeOpen is the corresponding seam between the pathname inspection and
	// descriptor acquisition. It proves a regular file replaced by a FIFO at
	// that exact boundary is opened nonblocking and then refused.
	beforeOpen func()
}

func openInstructionReader(boundary string) (*instructionReader, error) {
	abs, err := filepath.Abs(boundary)
	if err != nil {
		return nil, fmt.Errorf("make instruction root absolute: %w", err)
	}
	abs = filepath.Clean(abs)
	root, err := rootedfs.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("open instruction root: %w", err)
	}
	info, err := root.Stat(".")
	if err != nil {
		root.Close()
		return nil, fmt.Errorf("inspect instruction root: %w", err)
	}
	if !info.IsDir() {
		root.Close()
		return nil, fmt.Errorf("instruction root is not a directory")
	}
	r := &instructionReader{
		logicalRoot: abs,
		root:        root,
		rootInfo:    info,
		remaining:   maxInstructionSourceBytes,
	}
	if err := r.validateRoot(); err != nil {
		root.Close()
		return nil, err
	}
	return r, nil
}

func (r *instructionReader) close() {
	if r != nil && r.root != nil {
		_ = r.root.Close()
	}
}

func (r *instructionReader) validateRoot() error {
	if r == nil || r.root == nil || r.rootInfo == nil {
		return fmt.Errorf("instruction root is unavailable")
	}
	anchored, err := r.root.Stat(".")
	if err != nil || !anchored.IsDir() || !os.SameFile(r.rootInfo, anchored) {
		return fmt.Errorf("instruction root changed while it was read")
	}
	current, err := os.Stat(r.logicalRoot)
	if err != nil || !current.IsDir() || !os.SameFile(r.rootInfo, current) {
		return fmt.Errorf("instruction root changed while it was read")
	}
	return nil
}

func (r *instructionReader) read(path string) ([]byte, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("make instruction path absolute: %w", err)
	}
	abs = filepath.Clean(abs)
	rel, err := filepath.Rel(r.logicalRoot, abs)
	if err != nil || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("path resolves outside the instruction root")
	}
	if err := r.validateRoot(); err != nil {
		return nil, err
	}
	before, err := r.root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("symbolic-link instruction files are not loaded")
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("instruction path is not a regular file")
	}
	if before.Size() > r.remaining {
		return nil, fmt.Errorf("instruction source exceeds the %d-byte read limit", maxInstructionSourceBytes)
	}

	if r.beforeOpen != nil {
		r.beforeOpen()
	}
	file, err := openInstructionReadFile(r.root, rel)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, fmt.Errorf("instruction file changed while it was opened")
	}
	if r.afterOpen != nil {
		r.afterOpen()
	}
	data, err := io.ReadAll(io.LimitReader(file, r.remaining+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > r.remaining {
		return nil, fmt.Errorf("instruction source exceeds the %d-byte read limit", maxInstructionSourceBytes)
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("instruction file is not valid UTF-8")
	}
	afterFD, err := file.Stat()
	if err != nil || !afterFD.Mode().IsRegular() || !os.SameFile(opened, afterFD) ||
		opened.Size() != afterFD.Size() || !opened.ModTime().Equal(afterFD.ModTime()) || int64(len(data)) != afterFD.Size() {
		return nil, fmt.Errorf("instruction file changed while it was read")
	}
	afterPath, err := r.root.Lstat(rel)
	if err != nil || !afterPath.Mode().IsRegular() || !os.SameFile(afterFD, afterPath) {
		return nil, fmt.Errorf("instruction path changed while it was read")
	}
	if err := r.validateRoot(); err != nil {
		return nil, err
	}
	r.remaining -= int64(len(data))
	return data, nil
}

func instructionReadDiagnostic(path string, err error) string {
	return fmt.Sprintf("[%s was not read: %v]", path, err)
}

// expandImports replaces a line that is exactly an @path reference with the
// file it names.
//
// Only a whole line counts. An @path inside a sentence is prose about a file,
// and a reader that spliced a file in wherever the character appeared would
// make every mention of an email address an import.
func expandImports(text, dir string, reader *instructionReader, seen map[string]bool, depth int) string {
	// Scan each source while it is complete. In particular, do this before a
	// composed prompt can cut a token-shaped suffix below the scanner's length
	// floor. Recursive calls apply the same boundary to every imported source.
	text = redactCredentialText(text)
	if depth >= maxImportDepth || !strings.Contains(text, "@") {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) < 2 || !strings.HasPrefix(trimmed, "@") || strings.ContainsAny(trimmed, " \t") {
			continue
		}
		name := strings.TrimPrefix(trimmed, "@")
		if filepath.IsAbs(name) {
			lines[i] = fmt.Sprintf("[%s was not imported: absolute paths are outside %s]", trimmed, reader.logicalRoot)
			continue
		}
		target := filepath.Clean(filepath.Join(dir, name))
		if !withinBoundary(target, reader.logicalRoot) {
			lines[i] = fmt.Sprintf("[%s was not imported: it resolves outside %s]", trimmed, reader.logicalRoot)
			continue
		}
		if seen[target] {
			lines[i] = fmt.Sprintf("[%s was not imported: it is already part of this file]", trimmed)
			continue
		}
		if reader.imports >= maxInstructionImports {
			lines[i] = fmt.Sprintf("[%s was not imported: the %d-import limit was reached]", trimmed, maxInstructionImports)
			continue
		}
		reader.imports++
		data, err := reader.read(target)
		if err != nil {
			lines[i] = fmt.Sprintf("[%s was not imported: %v]", trimmed, err)
			continue
		}
		seen[target] = true
		lines[i] = expandImports(string(data), filepath.Dir(target), reader, seen, depth+1)
	}
	return strings.Join(lines, "\n")
}

// withinBoundary is the lexical first gate. instructionReader's os.Root is the
// filesystem gate: it follows only links that remain beneath the anchored root
// and revalidates both the file and root identities around every bounded read.
func withinBoundary(target, root string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), target)
	if err != nil {
		return false
	}
	return rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// renderInstructionLayers joins the layers under one budget.
//
// The budget is spent specific-first so the file closest to the work survives,
// and whatever did not fit is named. A layer that would not fit at all is
// dropped whole rather than half-quoted: half a rule reads as a rule.
func renderInstructionLayers(layers []instructionLayer) string {
	// Keep the source slice immutable: callers may retain it for diagnostics,
	// and mutating a shared layer while assembling the frozen prompt would turn
	// a safety pass into surprising application state. Labels are dynamic too;
	// a credential-shaped directory name must not reach a header or omission
	// notice and then consume that diagnostic during the final block scan.
	layers = redactInstructionLayers(layers)
	kept := make(map[int]string, len(layers))
	for i := len(layers) - 1; i >= 0; i-- {
		layer := layers[i]
		header := fmt.Sprintf("Instructions from %s (follow them):\n\n", layer.label)
		candidate := cloneInstructionSections(kept)
		candidate[i] = header + layer.text
		if instructionCompositionFits(candidate, instructionDroppedLabels(layers, candidate)) {
			kept = candidate
			continue
		}

		// A partial layer carries its own diagnostic. Size that diagnostic and
		// the global omission notice before choosing a body prefix; otherwise
		// those two truthful sentences are precisely what crosses the cap.
		cutNotice := fmt.Sprintf("[This instruction file was cut here: the composed instructions reached the %d byte budget]",
			maxInstructionBytes)
		candidate[i] = header + "\n\n" + cutNotice
		dropped := instructionDroppedLabels(layers, candidate)
		bodyBudget := instructionCompositionAllowance(candidate, dropped)
		body := truncateInstruction(layer.text, bodyBudget)
		if strings.TrimSpace(body) == "" {
			continue
		}
		candidate[i] = header + body + "\n\n" + cutNotice
		if instructionCompositionFits(candidate, dropped) {
			kept = candidate
		}
	}
	return composeInstructionSections(kept, instructionDroppedLabels(layers, kept))
}

func redactInstructionLayers(layers []instructionLayer) []instructionLayer {
	if layers == nil {
		return nil
	}
	out := make([]instructionLayer, len(layers))
	for i, layer := range layers {
		out[i] = instructionLayer{
			label: redactCredentialText(layer.label),
			text:  redactCredentialText(layer.text),
		}
	}
	return out
}

func cloneInstructionSections(in map[int]string) map[int]string {
	out := make(map[int]string, len(in)+1)
	for i, section := range in {
		out[i] = section
	}
	return out
}

// instructionDroppedLabels keeps the omitted list specific-first. If the cap
// can name only some paths, the rules closest to the work are the useful ones
// to identify.
func instructionDroppedLabels(layers []instructionLayer, kept map[int]string) []string {
	dropped := make([]string, 0, len(layers)-len(kept))
	for i := len(layers) - 1; i >= 0; i-- {
		if _, ok := kept[i]; !ok {
			dropped = append(dropped, layers[i].label)
		}
	}
	return dropped
}

func instructionSectionsBase(kept map[int]string) string {
	var b strings.Builder
	maxIndex := -1
	for i := range kept {
		if i > maxIndex {
			maxIndex = i
		}
	}
	for i := 0; i <= maxIndex; i++ {
		section, ok := kept[i]
		if !ok {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(section)
	}
	return b.String()
}

func minimalInstructionOmissionNotice(count int) string {
	return fmt.Sprintf("[%d instruction files omitted by the %d byte budget]", count, maxInstructionBytes)
}

func instructionCompositionFits(kept map[int]string, dropped []string) bool {
	base := instructionSectionsBase(kept)
	if len(dropped) == 0 {
		return len(base) <= maxInstructionBytes
	}
	separator := 0
	if base != "" {
		separator = 2
	}
	return len(base)+separator+len(minimalInstructionOmissionNotice(len(dropped))) <= maxInstructionBytes
}

// instructionCompositionAllowance reports how many bytes may be added to the
// already-rendered sections while retaining the smallest truthful omission
// notice. The caller uses it only for a body prefix; headers, separators, and
// both diagnostics are already present in kept.
func instructionCompositionAllowance(kept map[int]string, dropped []string) int {
	base := instructionSectionsBase(kept)
	reserved := 0
	if len(dropped) > 0 {
		if base != "" {
			reserved += 2
		}
		reserved += len(minimalInstructionOmissionNotice(len(dropped)))
	}
	return max(0, maxInstructionBytes-len(base)-reserved)
}

func composeInstructionSections(kept map[int]string, dropped []string) string {
	base := instructionSectionsBase(kept)
	if len(dropped) == 0 {
		if len(base) <= maxInstructionBytes {
			return base
		}
		return minimalInstructionOmissionNotice(len(kept))
	}
	separator := ""
	if base != "" {
		separator = "\n\n"
	}
	remaining := maxInstructionBytes - len(base) - len(separator)
	notice := instructionOmissionNotice(dropped, remaining)
	if notice == "" {
		// Selection reserves the minimal notice, so this is a fail-closed
		// backstop for a future accounting regression: discard rule text rather
		// than slice it or exceed the documented frozen-zone cap.
		return minimalInstructionOmissionNotice(len(dropped) + len(kept))
	}
	return base + separator + notice
}

func instructionOmissionNotice(labels []string, limit int) string {
	fallback := minimalInstructionOmissionNotice(len(labels))
	if len(fallback) > limit {
		return ""
	}
	prefix := fmt.Sprintf("[Instruction files omitted by the %d byte budget: ", maxInstructionBytes)
	for count := len(labels); count > 0; count-- {
		quoted := make([]string, count)
		for i := range count {
			quoted[i] = strconv.Quote(labels[i])
		}
		rest := ""
		if count < len(labels) {
			rest = fmt.Sprintf("; %d more", len(labels)-count)
		}
		notice := prefix + strings.Join(quoted, ", ") + rest + "]"
		if len(notice) <= limit {
			return notice
		}
	}
	return fallback
}

// truncateInstruction cuts only on a line boundary.
//
// The old reader sliced bytes, which could cut a multi-byte character in half
// and hand the model an invalid string. Cutting at a line is better still: a
// sentence stopped mid-word reads as an instruction that means something else.
// If even the first complete line does not fit, the layer is dropped and its
// name rides in the omission notice; no partial rule is safer than a new rule
// made from half of the old one.
func truncateInstruction(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(text) <= limit {
		return text
	}
	cut := text[:limit]
	if newline := strings.LastIndexByte(cut, '\n'); newline > 0 {
		return cut[:newline]
	}
	return ""
}
