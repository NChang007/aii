package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/ericmason/aii/internal/store"
)

// cmdTasks searches the task lines `aii digest` records for each session,
// or lists one session's tasks with --session. Each line carries a cite
// token pointing at the prompt where the task started.
func cmdTasks(args []string) error {
	fs := flag.NewFlagSet("tasks", flag.ExitOnError)
	agent := fs.String("agent", "", "cc|codex|cursor|pi|opencode|gemini|copilot|aider|cline|roo")
	workspace := fs.String("workspace", "", "exact workspace path")
	since := fs.String("since", "", "7d | 2026-01-01")
	sortBy := fs.String("sort", "mixed", "mixed (relevance weighted by recency) | relevance | recent")
	session := fs.String("session", "", "list every task of this session (uid or cite token)")
	limit := fs.Int("limit", 20, "max tasks")
	jsonOut := fs.Bool("json", false, "emit a single JSON array")
	ndjsonOut := fs.Bool("ndjson", false, "emit one JSON line per task")
	agentFlag := fs.Bool("agent-mode", false, "force agent-oriented output (default when stdout is piped)")
	pretty := fs.Bool("pretty", false, "force human-readable output even when piped")
	format := fs.String("format", "", "pretty|json|ndjson")
	fs.Parse(reorderFlags(args))

	db, err := store.Open(store.DefaultPath())
	if err != nil {
		return err
	}
	defer db.Close()
	citeDB = db

	var hits []store.TaskHit
	if strings.TrimSpace(*session) != "" {
		uid := resolveParentUID(db, *session)
		s, err := db.SessionByUIDAny(uid)
		if err != nil {
			return err
		}
		if s == nil {
			return fmt.Errorf("no session %q", *session)
		}
		tasks, err := db.SessionTasks(s.ID)
		if err != nil {
			return err
		}
		for _, t := range tasks {
			hits = append(hits, store.TaskHit{SessionUID: s.UID, Agent: s.Agent, Workspace: s.Workspace,
				Title: s.Title, ParentUID: s.ParentUID, Ordinal: t.Ordinal, Text: t.Text,
				StartedAt: s.StartedAt, EndedAt: s.EndedAt})
		}
	} else {
		sinceUnix, err := parseSince(*since)
		if err != nil {
			return err
		}
		hits, err = db.SearchTasks(store.TaskQuery{
			Text:      strings.Join(fs.Args(), " "),
			Agent:     normalizeAgent(*agent),
			Workspace: resolveWorkspaceFlag(*workspace),
			SinceUnix: sinceUnix,
			Sort:      *sortBy,
			Limit:     *limit,
		})
		if err != nil {
			return err
		}
	}

	mode, err := resolveListFormat(*format, *jsonOut, *ndjsonOut, *pretty, *agentFlag)
	if err != nil {
		return err
	}
	type row struct {
		Cite      string `json:"cite"`
		Task      string `json:"task"`
		Title     string `json:"session_title,omitempty"`
		Parent    string `json:"parent,omitempty"`
		Workspace string `json:"workspace,omitempty"`
		EndedAt   int64  `json:"ended_at,omitempty"`
	}
	var out []row
	for _, h := range hits {
		out = append(out, row{
			Cite:      fmt.Sprintf("%s/%s:%d", shortAgent(h.Agent), shortUID(h.SessionUID), h.Ordinal),
			Task:      h.Text,
			Title:     h.Title,
			Parent:    parentCite(h.Agent, h.ParentUID),
			Workspace: h.Workspace,
			EndedAt:   h.EndedAt,
		})
	}
	switch mode {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	case "ndjson":
		enc := json.NewEncoder(os.Stdout)
		for _, r := range out {
			if err := enc.Encode(r); err != nil {
				return err
			}
		}
		return nil
	}
	if len(out) == 0 {
		fmt.Println(cMuted("no tasks match (sessions get tasks from `aii digest`)"))
		return nil
	}
	lastSession := ""
	for i, r := range out {
		session := r.Cite[:strings.LastIndexByte(r.Cite, ':')]
		if session != lastSession {
			if i > 0 {
				fmt.Println()
			}
			head := session + "  " + store.FormatTime(hits[i].EndedAt)
			if r.Title != "" {
				head += "  · " + truncate(r.Title, 70)
			}
			fmt.Println(cMuted(head))
			lastSession = session
		}
		fmt.Printf("  %s  %s\n", cAccent(fmt.Sprintf("%-6s", r.Cite[len(session):])), r.Task)
	}
	return nil
}
