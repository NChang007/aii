package store

import (
	"strings"
	"testing"
)

func TestSearchSortMixesRecency(t *testing.T) {
	db := openTestDB(t)
	const day = 86400
	now := int64(1_800_000_000)
	// Same text, so relevance ties: recency decides.
	seed(t, db, &Session{Agent: "claude_code", UID: "old", StartedAt: now - 400*day, EndedAt: now - 400*day, SourcePath: "/a"},
		[]Message{{Ordinal: 0, Role: "user", Content: "webhook retry logic"}})
	seed(t, db, &Session{Agent: "claude_code", UID: "new", StartedAt: now - day, EndedAt: now - day, SourcePath: "/b"},
		[]Message{{Ordinal: 0, Role: "user", Content: "webhook retry logic"}})
	// Much stronger match, but old: relevance must still be able to win.
	seed(t, db, &Session{Agent: "claude_code", UID: "strong", StartedAt: now - 300*day, EndedAt: now - 300*day, SourcePath: "/c"},
		[]Message{{Ordinal: 0, Role: "user", Content: "zebra zebra zebra"}})
	seed(t, db, &Session{Agent: "claude_code", UID: "weak", StartedAt: now - day, EndedAt: now - day, SourcePath: "/d"},
		[]Message{{Ordinal: 0, Role: "user", Content: "zebra " + strings.Repeat("filler words about other things ", 40)}})

	order := func(text, sort string) string {
		t.Helper()
		res, err := db.Search(Query{Text: text, Sort: sort, NowUnix: now})
		if err != nil {
			t.Fatal(err)
		}
		var uids []string
		for _, r := range res {
			uids = append(uids, r.SessionUID)
		}
		return strings.Join(uids, ",")
	}

	if got := order("webhook retry", ""); got != "new,old" {
		t.Errorf("mixed (default) on equal relevance = %s, want new,old", got)
	}
	if got := order("webhook retry", "recent"); got != "new,old" {
		t.Errorf("recent = %s, want new,old", got)
	}
	if got := order("zebra", "relevance"); got != "strong,weak" {
		t.Errorf("relevance = %s, want strong,weak", got)
	}
	if got := order("zebra", "recent"); got != "weak,strong" {
		t.Errorf("recent = %s, want weak,strong", got)
	}
	if got := order("zebra", "mixed"); got != "strong,weak" {
		t.Errorf("mixed = %s, want the much stronger old match first", got)
	}
}
