package indexer

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ericmason/aii/internal/source"
	"github.com/ericmason/aii/internal/store"
)

// A truncating reindex (--full, or a file that shrank) deletes and
// re-inserts the session. A summary written by `aii digest` must survive,
// since the transcript itself never carries it.
func TestTruncateKeepsStoredSummary(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "aii.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	b := source.Batch{
		Session:  store.Session{Agent: "claude_code", UID: "s1", Title: "t", SourcePath: "/f.jsonl"},
		Messages: []store.Message{{Ordinal: 0, Role: "user", Content: "hi"}},
		State:    store.IndexState{SourcePath: "/f.jsonl"},
	}
	if err := apply(db, b, false); err != nil {
		t.Fatal(err)
	}
	s, _ := db.SessionByUID("claude_code", "s1")
	if err := db.SetSummary(s.ID, "digested"); err != nil {
		t.Fatal(err)
	}

	b.Truncate = true
	if err := apply(db, b, false); err != nil {
		t.Fatal(err)
	}
	s, _ = db.SessionByUID("claude_code", "s1")
	if s == nil || s.Summary != "digested" {
		t.Fatalf("summary after truncating reindex = %+v, want digested", s)
	}

	// A summary the source itself provides still wins.
	b.Session.Summary = "from source"
	if err := apply(db, b, false); err != nil {
		t.Fatal(err)
	}
	s, _ = db.SessionByUID("claude_code", "s1")
	if s.Summary != "from source" {
		t.Fatalf("summary = %q, want the source's own", s.Summary)
	}
}

func msgs(contents ...string) []store.Message {
	var out []store.Message
	for i, c := range contents {
		out = append(out, store.Message{Ordinal: i, Role: "user", Content: c})
	}
	return out
}

func contents(t *testing.T, db *store.DB, uid string) []string {
	t.Helper()
	s, err := db.SessionByUID("claude_code", uid)
	if err != nil || s == nil {
		t.Fatalf("session %s: %v", uid, err)
	}
	rows, err := db.SessionMessages(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		out = append(out, r.Role+":"+r.Content)
	}
	return out
}

// Indexing only ever adds: a truncating re-read (--full, or a transcript
// that shrank or was rewritten) never removes a stored message.
func TestTruncateIsAppendOnly(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "aii.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	batch := func(m []store.Message) source.Batch {
		return source.Batch{
			Session:  store.Session{Agent: "claude_code", UID: "s1", SourcePath: "/f.jsonl"},
			Messages: m,
			State:    store.IndexState{SourcePath: "/f.jsonl"},
			Truncate: true,
		}
	}
	if err := apply(db, batch(msgs("a", "b", "c")), false); err != nil {
		t.Fatal(err)
	}

	// A full-length re-read (here, a parser that now labels "b"
	// differently) replaces the stored rows: no duplicates.
	m := msgs("a", "b", "c")
	m[1].Role = "tool"
	if err := apply(db, batch(m), false); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(contents(t, db, "s1"), ","); got != "user:a,tool:b,user:c" {
		t.Fatalf("after refresh = %s", got)
	}

	// The transcript shrank: everything already stored stays.
	if err := apply(db, batch(msgs("a")), false); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(contents(t, db, "s1"), ","); got != "user:a,tool:b,user:c" {
		t.Fatalf("after shrink = %s", got)
	}

	// A shorter rewrite with new content: old rows stay, only the unseen
	// message is appended.
	if err := apply(db, batch(msgs("a", "x")), false); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(contents(t, db, "s1"), ","); got != "user:a,tool:b,user:c,user:x" {
		t.Fatalf("after rewrite = %s", got)
	}
}

// A re-read as long as what is stored but missing some stored content
// (an edited or regenerated message) must not erase it: replacement only
// happens when every stored message is still in the re-read.
func TestRewriteOfSameLengthKeepsOldContent(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "aii.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	batch := func(m []store.Message) source.Batch {
		return source.Batch{
			Session:  store.Session{Agent: "claude_code", UID: "s1", SourcePath: "/f.jsonl"},
			Messages: m, State: store.IndexState{SourcePath: "/f.jsonl"}, Truncate: true,
		}
	}
	for _, m := range [][]store.Message{msgs("a", "b", "c"), msgs("a", "x", "c")} {
		if err := apply(db, batch(m), false); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(contents(t, db, "s1"), ","); got != "user:a,user:b,user:c,user:x" {
		t.Fatalf("after same-length rewrite = %s", got)
	}
}

// Repeated identical messages ("ok", "ok") are distinct messages, not
// duplicates of each other.
func TestMergeKeepsRepeatedMessages(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "aii.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	batch := func(m []store.Message) source.Batch {
		return source.Batch{
			Session:  store.Session{Agent: "claude_code", UID: "s1", SourcePath: "/f.jsonl"},
			Messages: m, State: store.IndexState{SourcePath: "/f.jsonl"}, Truncate: true,
		}
	}
	for _, m := range [][]store.Message{msgs("ok", "q", "z"), msgs("ok", "ok")} {
		if err := apply(db, batch(m), false); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(contents(t, db, "s1"), ","); got != "user:ok,user:q,user:z,user:ok" {
		t.Fatalf("after merge = %s", got)
	}
}
