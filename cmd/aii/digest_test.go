package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ericmason/aii/internal/store"
)

func rowsOf(spec ...string) []store.Row {
	var out []store.Row
	for i := 0; i+1 < len(spec); i += 2 {
		out = append(out, store.Row{Ordinal: len(out), Role: spec[i], Content: spec[i+1]})
	}
	return out
}

func TestPromptOutline(t *testing.T) {
	rows := rowsOf(
		"system", "Base directory for this skill",
		"user", "<command-name>/model</command-name>",
		"user", "make the tabs smaller",
		"assistant", "Looking.\n[tool_use name=Edit] {}",
		"tool", "[tool_result] ok",
		"assistant", "Done: tabs are 12px.",
		"user", "now fix the colors",
		"assistant", "Colors fixed.",
	)
	turns := promptOutline(rows, -1)
	if len(turns) != 2 || turns[0].Ordinal != 2 || turns[1].Ordinal != 6 {
		t.Fatalf("turns = %+v, want prompts at 2 and 6 (slash command skipped)", turns)
	}
	if turns[0].Reply != "Looking. | Done: tabs are 12px." || turns[1].Reply != "Colors fixed." {
		t.Errorf("replies = %q, %q; want every assistant message's opening in the turn", turns[0].Reply, turns[1].Reply)
	}
	// Resuming after ordinal 5 skips the first turn.
	if turns = promptOutline(rows, 5); len(turns) != 1 || turns[0].Ordinal != 6 {
		t.Errorf("resumed turns = %+v", turns)
	}
}

func TestBatchTurnsSplitsLongSessions(t *testing.T) {
	var turns []turn
	for i := 0; i < 50; i++ {
		turns = append(turns, turn{Ordinal: i * 3, Prompt: strings.Repeat("p", 500), Reply: strings.Repeat("r", 300)})
	}
	batches := batchTurns(turns, 10000)
	if len(batches) < 4 {
		t.Fatalf("got %d batches, want the 40k outline split", len(batches))
	}
	n := 0
	for _, b := range batches {
		n += len(b)
	}
	if n != 50 {
		t.Errorf("batches hold %d turns, want all 50", n)
	}
}

func TestParseTaskReply(t *testing.T) {
	out := "Here you go:\n- [#5] Shrank kitty tabs to 12px — done\n- [#9] Fixed tab colors — unfinished\n- [#7] Bogus anchor — kept\n- no anchor line\nSUMMARY: Tabs resized and recolored; colors unfinished.\n"
	tasks, summary := parseTaskReply(out, []int{5, 8, 9})
	if summary != "Tabs resized and recolored; colors unfinished." {
		t.Errorf("summary = %q", summary)
	}
	want := map[int]string{5: "Shrank kitty tabs to 12px — done", 9: "Fixed tab colors — unfinished"}
	got := map[int]string{}
	for _, tk := range tasks {
		got[tk.Ordinal] = tk.Text
	}
	if !strings.HasPrefix(got[5], want[5]) || got[9] != want[9] {
		t.Errorf("tasks = %+v", tasks)
	}
	// #7 isn't a prompt: it snaps to the prompt before it (#5) and joins
	// that task rather than overwriting it.
	if !strings.Contains(got[5], "Shrank kitty tabs") || !strings.Contains(got[5], "Bogus anchor") || got[7] != "" {
		t.Errorf("unanchored ordinal not snapped: %+v", tasks)
	}
}

func TestDigestSessionEndToEnd(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "aii.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var ms []store.Message
	for i, r := range rowsOf("user", "build the queue", "assistant", "Queue built.", "user", "add retries", "assistant", "Retries added.") {
		ms = append(ms, store.Message{Ordinal: i, Role: r.Role, Content: r.Content})
	}
	id, _ := db.UpsertSession(&store.Session{Agent: "claude_code", UID: "s1", EndedAt: 1, SourcePath: "/f"})
	tx, _ := db.Begin()
	db.InsertMessages(tx, id, ms)
	tx.Commit()

	var prompts []string
	fake := func(p string) (string, error) {
		prompts = append(prompts, p)
		return "- [#0] Built the queue — done\n- [#2] Added retries — done\nSUMMARY: Queue with retries; done.", nil
	}
	todo, _ := db.SessionsNeedingTasks(store.TaskFilter{EndedBefore: 10, MinMessages: 2})
	if len(todo) != 1 {
		t.Fatalf("todo = %+v", todo)
	}
	n, err := digestSession(db, todo[0], fake, 12000)
	if err != nil || n != 2 || len(prompts) != 1 {
		t.Fatalf("digestSession = %d, %v; %d calls", n, err, len(prompts))
	}
	if !strings.Contains(prompts[0], "[#0] build the queue") || !strings.Contains(prompts[0], "Queue built.") {
		t.Errorf("prompt missing the outline:\n%s", prompts[0])
	}
	s, _ := db.SessionByUID("claude_code", "s1")
	tasks, _ := db.SessionTasks(id)
	if s.Summary != "Queue with retries; done." || len(tasks) != 2 {
		t.Errorf("summary %q, tasks %+v", s.Summary, tasks)
	}
	if todo, _ = db.SessionsNeedingTasks(store.TaskFilter{EndedBefore: 10, MinMessages: 2}); len(todo) != 0 {
		t.Errorf("still needing tasks: %+v", todo)
	}
}

func TestCleanSummary(t *testing.T) {
	for in, want := range map[string]string{
		"\n\n  \"Fixed tab sizes in kitty; done.\"  \nextra": "Fixed tab sizes in kitty; done.",
		"Summary: Built the hook.":                           "Built the hook.",
		"   ":                                                "",
		"```\nBuilt the parser; done.\n```":                  "Built the parser; done.",
		"\"\nFixed the users' tabs'":                         "Fixed the users' tabs'",
	} {
		if got := cleanSummary(in); got != want {
			t.Errorf("cleanSummary(%q) = %q, want %q", in, got, want)
		}
	}
}

// The real command path must hand back every line of the reply.
func TestRunDigestCmdReturnsWholeReply(t *testing.T) {
	out, err := runDigestCmd(context.Background(), "cat", "- [#0] one\n- [#2] two\nSUMMARY: both\n")
	if err != nil {
		t.Fatal(err)
	}
	if tasks, sum := parseTaskReply(out, []int{0, 2}); len(tasks) != 2 || sum != "both" {
		t.Errorf("tasks %+v, summary %q from %q", tasks, sum, out)
	}
}

func newDigestDB(t *testing.T, spec ...string) (*store.DB, int64) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "aii.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var ms []store.Message
	for i, r := range rowsOf(spec...) {
		ms = append(ms, store.Message{Ordinal: i, Role: r.Role, Content: r.Content})
	}
	id, _ := db.UpsertSession(&store.Session{Agent: "claude_code", UID: "s1", EndedAt: 1, SourcePath: "/f"})
	tx, _ := db.Begin()
	db.InsertMessages(tx, id, ms)
	tx.Commit()
	return db, id
}

func needing(t *testing.T, db *store.DB) []*store.TaskSession {
	t.Helper()
	todo, err := db.SessionsNeedingTasks(store.TaskFilter{EndedBefore: 10, MinMessages: 1})
	if err != nil {
		t.Fatal(err)
	}
	return todo
}

// When a later call fails, tasks from the calls before it are kept and
// only the rest is retried.
func TestDigestKeepsCompletedBatches(t *testing.T) {
	long := strings.Repeat("x", 400)
	db, id := newDigestDB(t, "user", "a "+long, "assistant", "A.", "user", "b "+long, "assistant", "B.")
	calls := 0
	failSecond := func(p string) (string, error) {
		calls++
		if calls == 2 {
			return "", errors.New("timeout")
		}
		return "- [#0] Did a — done", nil
	}
	if _, err := digestSession(db, needing(t, db)[0], failSecond, 500); err == nil {
		t.Fatal("want the second call's error")
	}
	tasks, _ := db.SessionTasks(id)
	todo := needing(t, db)
	if len(tasks) != 1 || len(todo) != 1 || todo[0].TasksThrough != 1 {
		t.Fatalf("tasks %+v, todo %+v; want batch 1 saved and only prompt #2 left", tasks, todo)
	}
	ok := func(p string) (string, error) {
		if strings.Contains(p, "[#0] a") {
			t.Errorf("retry re-sent the finished batch")
		}
		return "- [#2] Did b — done\nSUMMARY: a and b.", nil
	}
	if _, err := digestSession(db, todo[0], ok, 500); err != nil {
		t.Fatal(err)
	}
	if tasks, _ = db.SessionTasks(id); len(tasks) != 2 {
		t.Errorf("tasks = %+v", tasks)
	}
}

// A reply without a usable SUMMARY line gets one follow-up call for it.
func TestDigestAsksAgainForMissingSummary(t *testing.T) {
	db, _ := newDigestDB(t, "user", "build it", "assistant", "Built.")
	var prompts []string
	run := func(p string) (string, error) {
		prompts = append(prompts, p)
		if len(prompts) == 1 {
			return "- [#0] Built it — done", nil
		}
		return "**Summary:** Built it; done.", nil
	}
	if _, err := digestSession(db, needing(t, db)[0], run, 12000); err != nil {
		t.Fatal(err)
	}
	s, _ := db.SessionByUID("claude_code", "s1")
	if len(prompts) != 2 || s.Summary != "Built it; done." {
		t.Errorf("%d calls, summary %q", len(prompts), s.Summary)
	}
}

func TestParseTaskReplyFormats(t *testing.T) {
	out := "1. [#4] numbered — done\n[#6] no dash — done\n- [#1] echoed earlier task — done\n**SUMMARY:** fine."
	tasks, sum := parseTaskReply(out, []int{4, 6})
	if len(tasks) != 2 || tasks[0].Ordinal != 4 || tasks[1].Ordinal != 6 || sum != "fine." {
		t.Errorf("tasks %+v, summary %q; want numbered and dashless lines kept, the earlier-task echo dropped", tasks, sum)
	}
}

// --dry-run must not write: not even for a session with no new prompts.
func TestDigestDryRunWritesNothing(t *testing.T) {
	db, id := newDigestDB(t, "assistant", "only a reply")
	s := &store.TaskSession{Session: store.Session{ID: id, Agent: "claude_code", UID: "s1"}, TasksThrough: -1}
	if _, err := digestSessionOpts(db, s, func(string) (string, error) { return "", errDryRun }, 12000, true); err != nil && !errors.Is(err, errDryRun) {
		t.Fatal(err)
	}
	if todo, _ := db.SessionsNeedingTasks(store.TaskFilter{EndedBefore: 10, MinMessages: 0}); len(todo) != 1 || todo[0].TasksThrough != -1 {
		t.Errorf("dry run advanced tasks_through: %+v", todo)
	}
}
