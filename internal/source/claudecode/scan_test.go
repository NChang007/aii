package claudecode_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ericmason/aii/internal/indexer"
	"github.com/ericmason/aii/internal/source"
	"github.com/ericmason/aii/internal/source/claudecode"
	"github.com/ericmason/aii/internal/store"
)

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

func userRecord(text string) string {
	return fmt.Sprintf(`{"type":"user","sessionId":"s1","cwd":"/w","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":%q}}`, text)
}

// TestIncrementalRunsKeepFirstTitle indexes a session across three runs.
// The first run sees no user message, so the title comes from the second
// run, and the third run must not replace it with a later turn.
func TestIncrementalRunsKeepFirstTitle(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "s1.jsonl")

	db, err := store.Open(filepath.Join(t.TempDir(), "aii.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	src := []source.Source{&claudecode.Source{Root: root}}
	run := func() {
		t.Helper()
		if _, err := indexer.Run(context.Background(), db, src, indexer.Options{}); err != nil {
			t.Fatal(err)
		}
	}
	title := func() string {
		t.Helper()
		s, err := db.SessionByUID("claude_code", "s1")
		if err != nil {
			t.Fatal(err)
		}
		if s == nil {
			return ""
		}
		return s.Title
	}

	appendLine(t, path, `{"type":"permission-mode","sessionId":"s1","permissionMode":"default"}`)
	run()
	if got := title(); got != "" {
		t.Fatalf("after run 1 title = %q, want empty", got)
	}

	appendLine(t, path, userRecord("real title"))
	run()
	if got := title(); got != "real title" {
		t.Fatalf("after run 2 title = %q, want %q", got, "real title")
	}

	appendLine(t, path, userRecord("a later turn"))
	run()
	if got := title(); got != "real title" {
		t.Fatalf("after run 3 title = %q, want %q", got, "real title")
	}
}

// Subagent transcripts live at <project>/<session>/subagents/agent-<id>.jsonl.
// Each is indexed as its own session, linked to its parent: its records
// carry the parent's sessionId, so the agent id must become the uid or
// its messages would collide with the parent's.
func TestSubagentTranscriptsAreIndexed(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "proj")
	sub := filepath.Join(proj, "s1", "subagents")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	appendLine(t, filepath.Join(proj, "s1.jsonl"), userRecord("parent prompt"))
	agent := filepath.Join(sub, "agent-abc123.jsonl")
	appendLine(t, agent, `{"type":"user","isSidechain":true,"agentId":"abc123","sessionId":"s1","cwd":"/w","timestamp":"2026-01-01T00:01:00Z","message":{"role":"user","content":"Review the diff"}}`)
	appendLine(t, agent, `{"type":"assistant","isSidechain":true,"agentId":"abc123","sessionId":"s1","cwd":"/w","timestamp":"2026-01-01T00:02:00Z","message":{"role":"assistant","content":[{"type":"text","text":"Found two bugs."}]}}`)
	if err := os.WriteFile(filepath.Join(sub, "agent-abc123.meta.json"), []byte(`{"agentType":"code-reviewer","description":"Review role fix diff"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	db, err := store.Open(filepath.Join(t.TempDir(), "aii.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := indexer.Run(context.Background(), db, []source.Source{&claudecode.Source{Root: root}}, indexer.Options{}); err != nil {
		t.Fatal(err)
	}

	parent, _ := db.SessionByUID("claude_code", "s1")
	child, _ := db.SessionByUID("claude_code", "abc123")
	if parent == nil || child == nil {
		t.Fatalf("parent %v, child %v; want both indexed", parent, child)
	}
	if child.ParentUID != "s1" || child.Title != "code-reviewer: Review role fix diff" {
		t.Errorf("child parent=%q title=%q", child.ParentUID, child.Title)
	}
	rows, _ := db.SessionMessages(child.ID)
	if len(rows) != 2 || rows[0].Role != "agent" || rows[1].Role != "assistant" {
		t.Errorf("child rows = %+v, want the parent agent's prompt as role agent", rows)
	}
	prow, _ := db.SessionMessages(parent.ID)
	if len(prow) != 1 || prow[0].Content != "parent prompt" {
		t.Errorf("parent rows = %+v, want only its own message", prow)
	}
	kids, err := db.ListSessions(store.SessionFilter{Parent: "s1"})
	if err != nil || len(kids) != 1 || kids[0].UID != "abc123" {
		t.Errorf("ListSessions(Parent) = %+v, %v", kids, err)
	}
}

// A subagent file whose first record has no agentId (any non-message
// line) must still be indexed as the subagent, never as its parent.
func TestSubagentIdentityComesFromPath(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "proj")
	sub := filepath.Join(proj, "s1", "subagents")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	appendLine(t, filepath.Join(proj, "s1.jsonl"), userRecord("parent prompt"))
	agent := filepath.Join(sub, "agent-xyz789.jsonl")
	appendLine(t, agent, `{"type":"permission-mode","sessionId":"s1","permissionMode":"default"}`)
	appendLine(t, agent, `{"type":"user","isSidechain":true,"agentId":"xyz789","sessionId":"s1","cwd":"/w","timestamp":"2026-01-01T00:01:00Z","message":{"role":"user","content":"Do the thing"}}`)

	db, err := store.Open(filepath.Join(t.TempDir(), "aii.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := indexer.Run(context.Background(), db, []source.Source{&claudecode.Source{Root: root}}, indexer.Options{}); err != nil {
		t.Fatal(err)
	}
	parent, _ := db.SessionByUID("claude_code", "s1")
	child, _ := db.SessionByUID("claude_code", "xyz789")
	if child == nil || child.ParentUID != "s1" {
		t.Fatalf("child = %+v, want uid xyz789 with parent s1", child)
	}
	if rows, _ := db.SessionMessages(parent.ID); len(rows) != 1 {
		t.Errorf("parent has %d messages, want only its own 1", len(rows))
	}
	if parent.SourcePath == child.SourcePath {
		t.Errorf("parent source_path overwritten by the subagent file")
	}
}
