package claudecode

import (
	"bufio"
	"context"
	"fmt"
	"strings"
	"testing"
)

func userLine(uid, text string) string {
	return fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":"/w","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":%q}}`, uid, text) + "\n"
}

func TestParseDeltaUnterminatedTailIsRetried(t *testing.T) {
	first := userLine("s1", "first")
	partial := `{"type":"user","sessionId":"s1","message":{"role":"user","content":"sec`
	data := first + partial

	_, msgs, off, err := parseDelta(context.Background(), strings.NewReader(data), 0, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if off != int64(len(first)) {
		t.Fatalf("offset = %d, want %d (start of the unfinished line)", off, len(first))
	}
}

func TestParseDeltaStopsAtStatSize(t *testing.T) {
	first := userLine("s1", "first")
	grown := userLine("s1", "appended during the read")
	data := first + grown

	// size is what scanFile stat'd before the second line was appended.
	_, msgs, off, err := parseDelta(context.Background(), strings.NewReader(data), 0, int64(len(first)))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Content != "first" {
		t.Fatalf("got %d messages, want only the line within size", len(msgs))
	}
	if off != int64(len(first)) {
		t.Fatalf("offset = %d, want %d", off, len(first))
	}
}

func TestReadLineExactLimit(t *testing.T) {
	cases := []struct {
		in      string
		tooLong bool
	}{
		{"0123456789\n", false},
		{"0123456789", false},
		{"0123456789a\n", true},
		{"0123456789a", true},
	}
	for _, c := range cases {
		br := bufio.NewReaderSize(strings.NewReader(c.in), 16)
		_, n, _, tooLong, err := readLine(br, 10)
		if err != nil {
			t.Fatal(err)
		}
		if tooLong != c.tooLong {
			t.Errorf("readLine(%q, 10) tooLong = %v, want %v", c.in, tooLong, c.tooLong)
		}
		if n != int64(len(c.in)) {
			t.Errorf("readLine(%q) n = %d, want %d", c.in, n, len(c.in))
		}
	}
}

func TestParseDeltaSkipsOversizedLine(t *testing.T) {
	before := userLine("s1", "before")
	huge := userLine("s1", strings.Repeat("x", scannerMax+1024))
	after := userLine("s1", "after")
	data := before + huge + after

	sess, msgs, off, err := parseDelta(context.Background(), strings.NewReader(data), 0, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if off != int64(len(data)) {
		t.Fatalf("offset = %d, want %d (past the oversized line)", off, len(data))
	}
	if len(msgs) != 2 || msgs[0].Content != "before" || msgs[1].Content != "after" {
		var got []string
		for _, m := range msgs {
			got = append(got, truncateRunes(m.Content, 20))
		}
		t.Fatalf("messages = %q, want [before after]", got)
	}
	if msgs[1].SourceOffset != int64(len(before)+len(huge)) {
		t.Fatalf("after SourceOffset = %d, want %d", msgs[1].SourceOffset, len(before)+len(huge))
	}
	if sess.UID != "s1" {
		t.Fatalf("UID = %q", sess.UID)
	}
}

func TestParseDeltaOversizedUnterminatedTailIsRetried(t *testing.T) {
	before := userLine("s1", "before")
	partial := `{"type":"user","message":"` + strings.Repeat("x", scannerMax+1024)
	data := before + partial

	_, msgs, off, err := parseDelta(context.Background(), strings.NewReader(data), 0, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if off != int64(len(before)) {
		t.Fatalf("offset = %d, want %d (start of the unfinished line)", off, len(before))
	}
}

func partsLine(uid, partsJSON string) string {
	return fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":"/w","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":%s}}`, uid, partsJSON) + "\n"
}

func TestToolResultsAreToolRole(t *testing.T) {
	data := partsLine("s1", `[{"type":"tool_result","tool_use_id":"toolu_01ABCDEFGHIJK","content":"ls output"}]`) +
		partsLine("s1", `[{"type":"text","text":"typed by the user"},{"type":"tool_result","tool_use_id":"toolu_02ABCDEFGHIJK","is_error":true,"content":[{"type":"text","text":"boom"}]}]`) +
		userLine("s1", "plain prompt")

	sess, msgs, _, err := parseDelta(context.Background(), strings.NewReader(data), 0, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ role, content string }{
		{"tool", "[tool_result id=toolu_01ABCD] ls output"},
		{"user", "typed by the user"},
		{"tool", "[tool_result error id=toolu_02ABCD] boom"},
		{"user", "plain prompt"},
	}
	if len(msgs) != len(want) {
		t.Fatalf("got %d messages, want %d: %+v", len(msgs), len(want), msgs)
	}
	for i, w := range want {
		if msgs[i].Role != w.role || msgs[i].Content != w.content {
			t.Errorf("msg %d = {%s %q}, want {%s %q}", i, msgs[i].Role, msgs[i].Content, w.role, w.content)
		}
		if msgs[i].Ordinal != i {
			t.Errorf("msg %d ordinal = %d", i, msgs[i].Ordinal)
		}
	}
	if sess.Title != "typed by the user" {
		t.Errorf("title = %q, want the first thing the user typed", sess.Title)
	}
}
