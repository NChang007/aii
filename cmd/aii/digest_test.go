package main

import (
	"strings"
	"testing"

	"github.com/ericmason/aii/internal/store"
)

func TestDigestTranscriptKeepsProseOnly(t *testing.T) {
	rows := []store.Row{
		{Ordinal: 0, Role: "system", Content: "Base directory for this skill"},
		{Ordinal: 1, Role: "user", Content: "make the tabs smaller"},
		{Ordinal: 2, Role: "assistant", Content: "Shrinking them.\n[tool_use name=Edit] {\"file\":\"x\"}"},
		{Ordinal: 3, Role: "tool", Content: "[tool_result] ok"},
		{Ordinal: 4, Role: "thinking", Content: "hmm"},
		{Ordinal: 5, Role: "assistant", Content: "[tool_use name=Bash] {}"},
		{Ordinal: 6, Role: "assistant", Content: "Done: tabs are 12px."},
		{Ordinal: 7, Role: "agent", Content: "Review the diff"},
	}
	got := digestTranscript(rows, 10000)
	want := "user: make the tabs smaller\nassistant: Shrinking them.\nassistant: Done: tabs are 12px.\ntask: Review the diff\n"
	if got != want {
		t.Errorf("digestTranscript =\n%q\nwant\n%q", got, want)
	}
}

func TestDigestTranscriptKeepsHeadAndTail(t *testing.T) {
	var rows []store.Row
	for i := 0; i < 200; i++ {
		rows = append(rows, store.Row{Ordinal: i, Role: "user", Content: strings.Repeat("x", 50)})
	}
	rows[0].Content = "FIRST ASK"
	rows[199].Content = "FINAL STATE"
	got := digestTranscript(rows, 2000)
	if len(got) > 2200 || !strings.Contains(got, "FIRST ASK") || !strings.Contains(got, "FINAL STATE") || !strings.Contains(got, "…") {
		t.Errorf("len %d; want ≤ ~2000 chars keeping the first ask, the end, and an elision marker", len(got))
	}
}

func TestCleanSummary(t *testing.T) {
	for in, want := range map[string]string{
		"\n\n  \"Fixed tab sizes in kitty; done.\"  \nextra": "Fixed tab sizes in kitty; done.",
		"Summary: Built the hook.":                           "Built the hook.",
		"   ":                                                "",
	} {
		if got := cleanSummary(in); got != want {
			t.Errorf("cleanSummary(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanSummarySkipsFencesAndKeepsApostrophe(t *testing.T) {
	for in, want := range map[string]string{
		"```\nBuilt the parser; done.\n```": "Built the parser; done.",
		"\"\nFixed the users' tabs'":        "Fixed the users' tabs'",
	} {
		if got := cleanSummary(in); got != want {
			t.Errorf("cleanSummary(%q) = %q, want %q", in, got, want)
		}
	}
}
