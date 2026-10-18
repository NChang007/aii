package store

import "testing"

func TestSessionsNeedingSummary(t *testing.T) {
	db := openTestDB(t)
	msgs := func(n int) []Message {
		var out []Message
		for i := 0; i < n; i++ {
			out = append(out, Message{Ordinal: i, Role: "user", Content: "m"})
		}
		return out
	}
	old := seed(t, db, &Session{Agent: "claude_code", UID: "old", EndedAt: 1000, SourcePath: "/a"}, msgs(6))
	seed(t, db, &Session{Agent: "claude_code", UID: "short", EndedAt: 1000, SourcePath: "/b"}, msgs(2))
	seed(t, db, &Session{Agent: "claude_code", UID: "live", EndedAt: 9000, SourcePath: "/c"}, msgs(6))
	seed(t, db, &Session{Agent: "claude_code", UID: "done", EndedAt: 1000, Summary: "already", SourcePath: "/d"}, msgs(6))
	seed(t, db, &Session{Agent: "codex", UID: "other", EndedAt: 1000, SourcePath: "/e"}, msgs(6))

	got, err := db.SessionsNeedingSummary(SummaryFilter{Agent: "claude_code", EndedBefore: 5000, MinMessages: 4, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != old {
		t.Fatalf("got %+v, want only the ended, long-enough, unsummarised claude_code session", got)
	}

	if err := db.SetSummary(old, "Fixed the tabs; done."); err != nil {
		t.Fatal(err)
	}
	got, err = db.SessionsNeedingSummary(SummaryFilter{Agent: "claude_code", EndedBefore: 5000, MinMessages: 4, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("after SetSummary got %d sessions, want 0", len(got))
	}
	s, err := db.SessionByUID("claude_code", "old")
	if err != nil || s.Summary != "Fixed the tabs; done." {
		t.Fatalf("summary = %q (err %v)", s.Summary, err)
	}
}
