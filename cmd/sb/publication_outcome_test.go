package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/agent"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

func assertRetainedUnpublishedStage(t *testing.T, store *session.Store, workspace, id, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("unpublished stage was not retained for bounded maintenance: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("retained unpublished stage is not a regular file: %v", info.Mode())
	}
	if published, statusErr := session.PublicationStatus(path); published {
		t.Fatalf("retained stage became published: status error %v", statusErr)
	}
	opened, openErr := store.Open(id)
	if opened != nil {
		_ = opened.Close()
		t.Fatal("retained unpublished stage was openable as a session")
	}
	if !errors.Is(openErr, session.ErrSessionUnpublished) {
		t.Fatalf("retained unpublished stage open error = %v, want ErrSessionUnpublished", openErr)
	}
	infos, err := store.List(workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range infos {
		if info.ID == id || info.Path == path {
			t.Fatalf("retained unpublished stage became discoverable: %+v", info)
		}
	}
}

func TestPublicationResultKeepsVisibilityAndDurabilityDistinct(t *testing.T) {
	injected := errors.New("injected persistence failure")
	tests := []struct {
		name    string
		outcome session.PublicationOutcome
		err     error
		want    publicationDisposition
		text    string
	}{
		{"unpublished", session.PublicationOutcome{}, injected, publicationUnpublished, "publishing child"},
		{"visible uncertain", session.PublicationOutcome{Visible: true}, injected, publicationVisibleUncertain, "restart Switchboard"},
		{"durable", session.PublicationOutcome{Visible: true, Durable: true}, nil, publicationDurable, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := publicationResult(tt.outcome, tt.err, "child")
			if got != tt.want {
				t.Fatalf("disposition = %d, want %d", got, tt.want)
			}
			if tt.text == "" {
				if err != nil {
					t.Fatalf("durable result error = %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.text) {
				t.Fatalf("result error = %v, want %q", err, tt.text)
			}
		})
	}
}

func TestNonRetrySessionSwapsStopAfterVisibleUncertainPublication(t *testing.T) {
	tests := []struct {
		name           string
		fresh          bool
		continuePrompt string
	}{
		{name: "clear", fresh: true},
		{name: "fork"},
		{name: "compact", continuePrompt: compactContinuePrompt},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := testModel(t)
			fresh, err := m.app.store.CreateStaged(m.app.workspace, m.app.tier.Target.ID(), "test")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = fresh.CloseDiscardingStaged() })
			injected := errors.New("injected marker directory sync failure")
			cmd := m.onSessionSwap(sessionSwapMsg{
				sess: fresh, tier: m.app.tier, client: m.app.loop.Binding().Provider,
				fresh: tt.fresh, continuePrompt: tt.continuePrompt,
				publishDurably: func(*session.Session) (session.PublicationOutcome, error) {
					return session.PublicationOutcome{Visible: true}, injected
				},
			})
			if cmd == nil {
				t.Fatal("visible-but-uncertain adoption did not request shutdown")
			}
			if _, ok := cmd().(tea.QuitMsg); !ok {
				t.Fatal("visible-but-uncertain adoption returned work instead of tea.Quit")
			}
			if m.app.loop.Session != fresh {
				t.Fatal("visible publication was rolled back to the source session")
			}
			if !m.quitting || m.shutdownErr == nil || !strings.Contains(m.shutdownErr.Error(), "restart Switchboard") {
				t.Fatalf("shutdown state = quitting:%v err:%v", m.quitting, m.shutdownErr)
			}
			for _, message := range fresh.State().Messages {
				if strings.Contains(message.Text(), compactContinuePrompt) {
					t.Fatal("automatic continuation ran after uncertain publication")
				}
			}
			if err := fresh.AppendNote("info", "must not append after uncertain publication"); err == nil {
				t.Fatal("durability-uncertain adopted session remained writable during shutdown")
			}
		})
	}
}

func TestNonRetrySessionSwapRollsBackOnlyWhenPublicationIsInvisible(t *testing.T) {
	m := testModel(t)
	source := m.app.loop.Session
	fresh, err := m.app.store.CreateStaged(m.app.workspace, m.app.tier.Target.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	freshID := fresh.ID()
	path := fresh.Path()
	cmd := m.onSessionSwap(sessionSwapMsg{
		sess: fresh, tier: m.app.tier, client: m.app.loop.Binding().Provider,
		publishDurably: func(*session.Session) (session.PublicationOutcome, error) {
			return session.PublicationOutcome{}, errors.New("injected pre-commit failure")
		},
	})
	if cmd != nil {
		t.Fatal("invisible publication failure scheduled follow-up work")
	}
	if m.app.loop.Session != source || m.quitting || m.shutdownErr != nil {
		t.Fatalf("invisible failure state = source:%v quitting:%v err:%v", m.app.loop.Session == source, m.quitting, m.shutdownErr)
	}
	assertRetainedUnpublishedStage(t, m.app.store, m.app.workspace, freshID, path)
}

func TestUncertainRaceWinnerDoesNotPublishLoserOrAppendAfterCommit(t *testing.T) {
	m := testModel(t)
	winner, err := m.app.store.CreateStaged(m.app.workspace, m.app.tier.Target.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	loser, err := m.app.store.CreateStaged(m.app.workspace, m.app.tier.Target.ID(), "test")
	if err != nil {
		t.Fatal(err)
	}
	loserID := loser.ID()
	loserPath := loser.Path()
	calls := 0
	injected := errors.New("injected winner marker directory sync failure")
	cmd := m.onSessionSwap(sessionSwapMsg{
		sess: winner, tier: m.app.tier, client: m.app.loop.Binding().Provider,
		note: "fallback note committed with the binding", warnNote: true,
		publishAfter: loser, publishAfterNote: ", other branch published",
		publishDurably: func(sess *session.Session) (session.PublicationOutcome, error) {
			calls++
			if sess != winner {
				t.Errorf("published race loser after uncertain winner: %s", sess.ID())
			}
			return session.PublicationOutcome{Visible: true}, injected
		},
	})
	if calls != 1 {
		t.Fatalf("publication calls = %d, want only the winner", calls)
	}
	if cmd == nil {
		t.Fatal("uncertain race winner did not request shutdown")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("uncertain race winner scheduled work instead of tea.Quit")
	}
	assertRetainedUnpublishedStage(t, m.app.store, m.app.workspace, loserID, loserPath)
	log, err := os.ReadFile(winner.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(log), "fallback note committed with the binding"); got != 1 {
		t.Fatalf("atomic pre-publication fallback note count = %d, want one", got)
	}
	if err := winner.AppendNote("info", "must not append"); err == nil {
		t.Fatal("uncertain race winner remained writable")
	}
}

func TestNoKeptRaceStopsBeforePublishingSiblingAfterUncertainCommit(t *testing.T) {
	m := raceModel(t)
	armA, err := assembleRaceArm(m.app, m.app.config.Tiers[0], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	armB, err := assembleRaceArm(m.app, m.app.config.Tiers[1], &racedProvider{}, agent.NopObserver{})
	if err != nil {
		t.Fatal(err)
	}
	armA.status, armB.status = "completed", "completed"
	armAPath, armBPath := armA.sess.Path(), armB.sess.Path()
	armBID := armB.sess.ID()
	calls := 0
	injected := errors.New("injected alternative marker directory sync failure")
	run := &raceRun{
		typed: "neither answer", arms: [2]*raceArm{armA, armB}, before: m.app.loop.Session.State(),
		publishDurably: func(sess *session.Session) (session.PublicationOutcome, error) {
			calls++
			if sess != armA.sess {
				t.Errorf("published sibling after uncertain race alternative: %s", sess.ID())
			}
			return session.PublicationOutcome{Visible: true}, injected
		},
	}
	cmd := m.finishRace(run, "", "abandoned")
	if calls != 1 {
		t.Fatalf("publication calls = %d, want only the first alternative", calls)
	}
	if cmd == nil {
		t.Fatal("uncertain race alternative did not request shutdown")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("uncertain race alternative scheduled work instead of tea.Quit")
	}
	if _, err := os.Stat(armAPath); err != nil {
		t.Fatalf("visible uncertain alternative was discarded: %v", err)
	}
	assertRetainedUnpublishedStage(t, m.app.store, m.app.workspace, armBID, armBPath)
	if err := m.app.loop.Session.AppendNote("info", "must not append"); err == nil {
		t.Fatal("continuing source remained writable after uncertain alternative publication")
	}
	if !m.quitting || m.shutdownErr == nil || !strings.Contains(m.shutdownErr.Error(), "restart Switchboard") {
		t.Fatalf("shutdown state = quitting:%v err:%v", m.quitting, m.shutdownErr)
	}
}

func TestREPLCompactionStopsBeforeContinuationAfterVisibleUncertainPublication(t *testing.T) {
	r, _, _ := newOverrideREPL(t, "small")
	source := r.loop.Session
	if err := source.AppendMessage(provider.UserText("finish the parser repair")); err != nil {
		t.Fatal(err)
	}
	if err := source.AppendMessage(provider.Message{Role: provider.RoleAssistant,
		Content: []provider.Block{provider.Text{Text: "in progress"}}}); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Dir(filepath.Dir(source.Path())))
	if err != nil {
		t.Fatal(err)
	}
	r.store = store
	binding := r.loop.Binding()
	binding.Provider = &racedProvider{turns: []racedTurn{racedText(testCompactHandoff("Continue the active objective."))}}
	r.loop.Bind(binding)
	injected := errors.New("injected compact marker directory sync failure")
	r.publishDurably = func(*session.Session) (session.PublicationOutcome, error) {
		return session.PublicationOutcome{Visible: true}, injected
	}

	r.compact(context.Background(), "")
	fresh := r.loop.Session
	if fresh == source {
		t.Fatal("visible compact publication was rolled back")
	}
	t.Cleanup(func() { _ = fresh.CloseDiscardingStaged() })
	var restart *publicationRestartRequiredError
	if !errors.As(r.restartRequired, &restart) || !strings.Contains(r.restartRequired.Error(), "restart Switchboard") {
		t.Fatalf("REPL restart state = %v", r.restartRequired)
	}
	for _, message := range fresh.State().Messages {
		if strings.Contains(message.Text(), compactContinuePrompt) {
			t.Fatal("REPL sent its automatic continuation after uncertain publication")
		}
	}
	if err := fresh.AppendNote("info", "must not append after uncertain publication"); err == nil {
		t.Fatal("REPL left durability-uncertain compacted session writable")
	}
}

// Publish intentionally preserves an old visibility-only API for tests and
// compatibility. Production adoption must consume PublishDurably's outcome;
// this guard prevents a new call site from silently reintroducing rollback or
// continuation after a durability-uncertain commit.
func TestProductionDoesNotCallLegacySessionPublish(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate publication call-site test")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	for _, subtree := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, subtree), func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			files := token.NewFileSet()
			file, err := parser.ParseFile(files, path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(node ast.Node) bool {
				selector, ok := node.(*ast.SelectorExpr)
				if ok && selector.Sel.Name == "Publish" {
					t.Errorf("production code references legacy Publish at %s; use PublishDurably and handle all three outcomes", files.Position(selector.Pos()))
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
