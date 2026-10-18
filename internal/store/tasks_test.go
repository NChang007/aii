package store

import (
	"strings"
	"testing"
)

func TestSessionTasksLifecycle(t *testing.T) {
	db := openTestDB(t)
	var ms []Message
	for i := 0; i < 8; i++ {
		ms = append(ms, Message{Ordinal: i, Role: "user", Content: "m"})
	}
	id := seed(t, db, &Session{Agent: "claude_code", UID: "s1", Workspace: "/w", EndedAt: 1000, SourcePath: "/a"}, ms)

	f := TaskFilter{EndedBefore: 5000, MinMessages: 4, Limit: 10}
	todo, err := db.SessionsNeedingTasks(f)
	if err != nil || len(todo) != 1 || todo[0].ID != id || todo[0].TasksThrough != -1 {
		t.Fatalf("needing tasks = %+v, %v; want s1 from the start", todo, err)
	}

	if err := db.SaveTasks(id, []Task{{Ordinal: 0, Text: "Built the webhook retry queue"}, {Ordinal: 5, Text: "Fixed flaky tab colors"}}, 7, "Retry queue and tab colors; done."); err != nil {
		t.Fatal(err)
	}
	if todo, _ = db.SessionsNeedingTasks(f); len(todo) != 0 {
		t.Fatalf("after SaveTasks still needing: %+v", todo)
	}
	s, _ := db.SessionByUID("claude_code", "s1")
	if s.Summary != "Retry queue and tab colors; done." {
		t.Errorf("summary = %q", s.Summary)
	}

	// The session is resumed: only the new part needs tasks.
	tx, _ := db.Begin()
	db.InsertMessages(tx, id, []Message{{Ordinal: 8, Role: "user", Content: "n"}, {Ordinal: 9, Role: "assistant", Content: "n"}})
	tx.Commit()
	if todo, _ = db.SessionsNeedingTasks(f); len(todo) != 1 || todo[0].TasksThrough != 7 {
		t.Fatalf("after resume = %+v, want s1 from ordinal 8", todo)
	}
	if err := db.SaveTasks(id, []Task{{Ordinal: 8, Text: "Added a retry test"}}, 9, "Retry queue, tab colors, test; done."); err != nil {
		t.Fatal(err)
	}
	tasks, err := db.SessionTasks(id)
	if err != nil || len(tasks) != 3 || tasks[2].Text != "Added a retry test" {
		t.Fatalf("tasks = %+v, %v", tasks, err)
	}

	hits, err := db.SearchTasks(TaskQuery{Text: "retry", Workspace: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, h := range hits {
		got = append(got, h.Text)
	}
	if len(hits) != 2 || !strings.Contains(strings.Join(got, "|"), "retry queue") || hits[0].SessionUID != "s1" {
		t.Errorf("SearchTasks = %+v", hits)
	}
	if hits, _ = db.SearchTasks(TaskQuery{Text: "colors"}); len(hits) != 1 || hits[0].Ordinal != 5 {
		t.Errorf("SearchTasks(colors) = %+v, want the task anchored at ordinal 5", hits)
	}
}
