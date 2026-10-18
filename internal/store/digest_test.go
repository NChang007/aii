package store

import "testing"

// Sessions with no prose, or that failed digest maxDigestAttempts times,
// stop taking a run's slots.
func TestSessionsNeedingTasksSkipsHopelessOnes(t *testing.T) {
	db := openTestDB(t)
	var toolOnly, prose []Message
	for i := 0; i < 6; i++ {
		toolOnly = append(toolOnly, Message{Ordinal: i, Role: "tool", Content: "out"})
		prose = append(prose, Message{Ordinal: i, Role: "user", Content: "m"})
	}
	seed(t, db, &Session{Agent: "claude_code", UID: "tools", EndedAt: 1000, SourcePath: "/a"}, toolOnly)
	flaky := seed(t, db, &Session{Agent: "claude_code", UID: "flaky", EndedAt: 1000, SourcePath: "/b"}, prose)
	seed(t, db, &Session{Agent: "claude_code", UID: "live", EndedAt: 9000, SourcePath: "/c"}, prose)
	f := TaskFilter{EndedBefore: 5000, MinMessages: 4, Limit: 10}

	got, err := db.SessionsNeedingTasks(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != flaky {
		t.Fatalf("got %+v, want only the ended session with prose", got)
	}
	for i := 0; i < maxDigestAttempts; i++ {
		if err := db.RecordDigestFailure(flaky); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ = db.SessionsNeedingTasks(f); len(got) != 0 {
		t.Fatalf("after %d failures got %d sessions, want 0", maxDigestAttempts, len(got))
	}
}
