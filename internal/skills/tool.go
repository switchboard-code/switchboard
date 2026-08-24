package skills

// The skill tool. Its description enumerates what loaded — name and
// one-line description each — so knowing what exists costs the frozen zone
// a few lines, and a body costs tokens only in the sessions that ask for
// it. Serving is read-effect: the bodies were read at assembly, and a
// supporting file is served from the skill's own directory and nowhere
// else, so a pack can carry references beside its SKILL.md — including
// packs living under ~/.switchboard, which the workspace-rooted read tool
// could not reach.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/rootedfs"
	"github.com/switchboard-code/switchboard/internal/tools"
)

type skillTool struct {
	byName map[string][]Skill
	names  []string
}

// NewTool builds the tool over the loaded set. The caller decided the set at
// session assembly; with no skills it should register nothing, keeping the
// schemas byte-identical to a build without the feature — that absence is
// the cache promise, and the test that pins it lives beside this package's.
func NewTool(list []Skill) tools.Tool {
	t := &skillTool{byName: map[string][]Skill{}}
	for _, sk := range ModelVisible(list) {
		// Work on the value copy produced by the range. The complete description
		// and body must be scanned before the description enters the frozen tool
		// schema or the body can become a tool result, while the caller's source
		// inventory remains an exact account of what discovery read.
		if sk.rootDir == "" {
			if resolved, err := filepath.EvalSymlinks(sk.Dir); err == nil {
				sk.rootDir = resolved
			}
		}
		if sk.rootInfo == nil && sk.rootDir != "" {
			if root, err := rootedfs.OpenRoot(sk.rootDir); err == nil {
				sk.rootInfo, _ = root.Stat(".")
				root.Close()
			}
		}
		// Identity and provenance are provider-visible too: Key and Name are
		// rendered into the frozen tool description, while Dir is returned with a
		// loaded body. Resolve the real resource root above, then redact only this
		// value copy so serving retains its filesystem identity and the caller's
		// discovery inventory remains exact.
		sk.Name = redactSkillEgress(sk.Name)
		sk.Selector = redactSkillSelector(sk.Selector)
		sk.Description = redactSkillEgress(sk.Description)
		sk.Body = redactSkillEgress(sk.Body)
		sk.Dir = redactSkillEgress(sk.Dir)
		key := sk.Key()
		if len(t.byName[key]) == 0 {
			t.names = append(t.names, key)
		}
		t.byName[key] = append(t.byName[key], sk)
	}
	return t
}

func (t *skillTool) Name() string { return "skill" }

func (t *skillTool) Description() string {
	var b strings.Builder
	b.WriteString("Load a skill: standing instructions for a kind of task, written by the user. " +
		"When a task matches a skill's description, call this before doing the work and follow " +
		"what it says. The body may reference supporting files beside it; pass file to fetch one. " +
		"Available skills:\n")
	for _, name := range t.names {
		// YAML literal blocks may preserve newlines. Keep one skill on one
		// schema line so a description cannot masquerade as another entry.
		sk := t.byName[name][0]
		description := strings.Join(strings.Fields(sk.Description), " ")
		label := name
		if name != sk.Name {
			label += " (" + sk.Name + ")"
		}
		b.WriteString("- " + redactSkillEgress(label) + ": " + redactSkillEgress(description) + "\n")
	}
	return redactSkillEgress(strings.TrimRight(b.String(), "\n"))
}

// ParallelSafe: serving is memory and read-only files, no shared state.
func (t *skillTool) ParallelSafe() bool { return true }

func (t *skillTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
		"name": {"type": "string", "description": "The canonical skill selector to load, from the available list."},
    "file": {"type": "string", "description": "A supporting file the skill references, relative to the skill's own directory, e.g. references/style.md. Omit for the skill itself."}
  },
  "required": ["name"]
}`)
}

type skillInput struct {
	Name string `json:"name"`
	File string `json:"file"`
}

func (t *skillTool) Plan(input json.RawMessage) (tools.Plan, error) {
	var in skillInput
	if err := json.Unmarshal(input, &in); err != nil {
		return tools.Plan{}, fmt.Errorf("skill: %s", redactSkillEgress(err.Error()))
	}
	matches := t.byName[in.Name]
	if len(matches) == 0 {
		return tools.Plan{}, fmt.Errorf("skill: no skill named %q; the available ones are listed in this tool's description", redactSkillEgress(in.Name))
	}
	if len(matches) != 1 {
		return tools.Plan{}, fmt.Errorf("skill: selector %q is ambiguous across %d definitions", redactSkillEgress(in.Name), len(matches))
	}
	sk := matches[0]

	detail := in.Name
	if in.File != "" {
		detail += " " + in.File
	}
	detail = redactSkillEgress(detail)
	return tools.Plan{
		Request: permission.Request{Tool: t.Name(), Effect: permission.EffectRead, Detail: detail},
		Run: func(ctx context.Context) (tools.Result, error) {
			if in.File != "" {
				return serveFile(sk, in.File)
			}
			return skillResult(fmt.Sprintf("Skill %s, from %s:\n\n%s", sk.Name, sk.Dir, sk.Body), false), nil
		},
	}, nil
}

// skillResult is the final provider-result boundary. Components are redacted
// before composition above so punctuation cannot hide their credential word
// boundaries; scanning the completed value again closes combinations that
// become recognizable only after composition.
func skillResult(content string, isError bool) tools.Result {
	return tools.Result{Content: redactSkillEgress(content), IsError: isError}
}

// serveFile answers with a file from the skill's directory and refuses
// everything else: the skill named its own references, not the filesystem.
// os.Root holds the confinement through the read itself, so a symlink swap
// cannot carry the operation outside the directory after a separate check.
func serveFile(sk Skill, rel string) (tools.Result, error) {
	if filepath.IsAbs(rel) {
		return skillResult("skill files are relative to the skill's directory; "+redactSkillEgress(rel)+" is absolute", true), nil
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return skillResult(redactSkillEgress(rel)+" leaves skill "+sk.Name+"'s directory, which this tool does not serve", true), nil
	}
	rootDir := sk.rootDir
	if rootDir == "" {
		rootDir = sk.Dir
	}
	root, err := rootedfs.OpenRoot(rootDir)
	if err != nil {
		return skillResult(err.Error(), true), nil
	}
	defer root.Close()
	openedInfo, err := root.Stat(".")
	if err != nil {
		return skillResult(err.Error(), true), nil
	}
	if sk.rootInfo == nil || !os.SameFile(sk.rootInfo, openedInfo) {
		return skillResult("skill "+sk.Name+"'s directory changed after discovery; refusing to serve supporting files", true), nil
	}
	data, err := readFileFromRoot(root, rel, maxSupportingBytes)
	if err != nil {
		displayRel := redactSkillEgress(rel)
		if os.IsNotExist(err) {
			return skillResult(displayRel+" does not exist in skill "+sk.Name+"'s directory", true), nil
		}
		if strings.Contains(err.Error(), "path escapes from parent") {
			return skillResult(displayRel+" leaves skill "+sk.Name+"'s directory, which this tool does not serve", true), nil
		}
		return skillResult(displayRel+" cannot be read within skill "+sk.Name+"'s directory: "+redactSkillEgress(err.Error()), true), nil
	}
	// Scan the complete bounded resource before constructing the tool result.
	// In particular, do not truncate first: a key that begins near a display or
	// read boundary still has to meet the scanner's full length floor.
	return skillResult(redactSkillEgress(string(data)), false), nil
}
