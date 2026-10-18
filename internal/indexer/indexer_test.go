package indexer

import (
	"path/filepath"
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
