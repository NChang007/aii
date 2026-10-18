package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ericmason/aii/internal/store"
)

// defaultDigestCmd summarises with a small model and writes no transcript
// of its own (otherwise aii would index, and later digest, its own calls).
// Hooks are off so the user's prompt hooks don't fire on every call, and
// tools and MCP servers are off: the prompt is untrusted transcript text
// and a summary needs neither.
const defaultDigestCmd = `claude -p --model haiku --no-session-persistence --settings '{"disableAllHooks":true}' --tools "" --strict-mcp-config`

// digestCallTimeout bounds one LLM call, so a hung command can't stall
// every later scheduled run.
const digestCallTimeout = 3 * time.Minute

const taskInstructions = `You are indexing one AI coding session so the user can later find what was done in it.
Below are the user's prompts in order, each tagged [#N] with its message number and followed by the start of the final reply it got.
Group consecutive prompts into tasks: a new task starts only where the user switched to a different goal. Follow-ups, refinements, corrections and short replies like "yes", "do it" or "fix that" continue the current task. List the tasks a person would name when asked what the session covered: usually 1 to 8, more only for a session that truly moved between many unrelated goals.
For each task write one line: "- [#N] <what was done or decided, at most 20 words> — <outcome: done, unfinished, abandoned, or blocked>", where #N is the prompt that started the task.
%s
Reply with the lines only: no preamble, no markdown beyond the leading "- ".
`

const summaryInstruction = `Then write one last line "SUMMARY: <at most 25 words: the overall goal, what got done, how the session ended>" covering the whole session, including any earlier tasks listed below.`

// cmdDigest records, for finished sessions, the tasks handled in them
// (one line each, anchored to the prompt that started it) and a one-line
// summary, so `aii sessions`, `aii tasks` and search say what a session
// covered without opening it. Resumed sessions are brought up to date.
func cmdDigest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("digest", flag.ExitOnError)
	limit := fs.Int("limit", 20, "process at most this many sessions per run")
	agent := fs.String("agent", "", "cc|codex|cursor|pi|opencode|gemini|copilot|aider|cline|roo")
	since := fs.String("since", "", "only sessions started since: 7d | 2026-01-01")
	minMsgs := fs.Int("min-messages", 6, "skip never-digested sessions with fewer prose messages")
	idle := fs.Duration("idle", time.Hour, "skip sessions written to more recently than this")
	maxChars := fs.Int("max-chars", 12000, "prompt-outline bytes sent per LLM call; longer sessions take several calls")
	cmdStr := fs.String("cmd", "", "LLM command to pipe each prompt into (default: $AII_DIGEST_CMD or claude -p with haiku)")
	dryRun := fs.Bool("dry-run", false, "list the sessions that would be processed and print the first prompt")
	session := fs.String("session", "", "only this session (uid or cite token), if it has prompts not yet digested")
	fs.Parse(reorderFlags(args))

	sinceUnix, err := parseSince(*since)
	if err != nil {
		return err
	}
	db, err := store.Open(store.DefaultPath())
	if err != nil {
		return err
	}
	defer db.Close()
	citeDB = db

	todo, err := db.SessionsNeedingTasks(store.TaskFilter{
		Agent:       normalizeAgent(*agent),
		SinceUnix:   sinceUnix,
		EndedBefore: time.Now().Add(-*idle).Unix(),
		MinMessages: *minMsgs,
		UID:         resolveParentUID(db, *session),
		Limit:       *limit,
	})
	if err != nil {
		return err
	}
	if len(todo) == 0 {
		fmt.Fprintln(os.Stderr, "aii digest: nothing to do")
		return nil
	}

	shellCmd := resolveDigestCmd(*cmdStr)
	if shellCmd == "" && !*dryRun {
		return errors.New("no LLM command available: install `claude`, set $AII_DIGEST_CMD, or pass --cmd")
	}

	done, failed := 0, 0
	printed := false
	for _, s := range todo {
		if ctx.Err() != nil {
			break
		}
		cite := short(s.Agent) + "/" + shortUID(s.UID)
		if *dryRun {
			fmt.Printf("%s  %s\n", cite, s.Title)
			if !printed {
				printed = true
				_, _ = digestSession(db, s, func(p string) (string, error) {
					fmt.Fprintf(os.Stderr, "--- first prompt ---\n%s\n--- end ---\n", p)
					return "", errDryRun
				}, *maxChars)
			}
			continue
		}
		n, err := digestSession(db, s, func(p string) (string, error) {
			return runDigestCmd(ctx, shellCmd, p)
		}, *maxChars)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			failed++
			fmt.Fprintf(os.Stderr, "aii digest: %s: %v\n", cite, err)
			if err := db.RecordDigestFailure(s.ID); err != nil {
				return err
			}
			continue
		}
		done++
		fresh, _ := db.SessionByUID(s.Agent, s.UID)
		summary := ""
		if fresh != nil {
			summary = fresh.Summary
		}
		fmt.Printf("%s  +%d task%s  %s\n", cite, n, pluralS(n), summary)
	}
	if !*dryRun {
		fmt.Fprintf(os.Stderr, "aii digest: %d sessions done, %d failed\n", done, failed)
	}
	return nil
}

var errDryRun = errors.New("dry run")

// turn is one prompt in a session's outline: the prompt and the start of
// the last reply it got.
type turn struct {
	Ordinal int
	Prompt  string
	Reply   string
}

const (
	maxPromptChars = 600
	// Per turn: the opening of each assistant message, joined, keeping
	// the last maxReplyChars so the outcome at the end of the turn stays.
	maxReplyStepChars = 160
	maxReplyChars     = 500
)

// promptOutline lists the prompts after ordinal `after`, each with what
// the assistant said until the next prompt: the opening of every
// assistant message in the turn, so work done midway ("the patch is in
// place") isn't hidden behind the final reply. Prompts are what the user
// typed or, in a subagent, the task it was given; tool calls, tool
// output, thinking and injected text are left out.
func promptOutline(rows []store.Row, after int) []turn {
	var out []turn
	for _, r := range rows {
		if r.Ordinal <= after {
			continue
		}
		switch r.Role {
		case "user", "agent":
			if isSlashCommand(r.Content) {
				// /model, /output-style…: settings, not tasks.
				continue
			}
			out = append(out, turn{Ordinal: r.Ordinal, Prompt: oneLineCap(r.Content, maxPromptChars)})
		case "assistant":
			if len(out) == 0 {
				continue
			}
			if text := proseOnly(r.Content); text != "" {
				t := &out[len(out)-1]
				step := oneLineCap(text, maxReplyStepChars)
				if t.Reply != "" {
					step = t.Reply + " | " + step
				}
				t.Reply = lastBytes(step, maxReplyChars)
			}
		}
	}
	return out
}

// isSlashCommand reports a Claude Code slash-command invocation or its
// echoed output, as stored in the transcript.
func isSlashCommand(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "<command-") || strings.HasPrefix(s, "<local-command-")
}

// proseOnly drops [tool_use …] lines from an assistant message.
func proseOnly(s string) string {
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "[tool_use") {
			keep = append(keep, l)
		}
	}
	return strings.TrimSpace(strings.Join(keep, "\n"))
}

func oneLineCap(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = truncateBytes(s, n) + "…"
	}
	return s
}

func (t turn) render() string {
	s := fmt.Sprintf("[#%d] %s\n", t.Ordinal, t.Prompt)
	if t.Reply != "" {
		s += "    reply: " + t.Reply + "\n"
	}
	return s
}

// batchTurns splits an outline into consecutive batches of at most
// maxChars rendered bytes (a single oversized turn gets its own batch).
func batchTurns(turns []turn, maxChars int) [][]turn {
	var out [][]turn
	var cur []turn
	size := 0
	for _, t := range turns {
		n := len(t.render())
		if len(cur) > 0 && size+n > maxChars {
			out = append(out, cur)
			cur, size = nil, 0
		}
		cur = append(cur, t)
		size += n
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// digestSession records the tasks for a session's prompts past
// s.TasksThrough and, when there are any, a fresh summary of the whole
// session. Each batch of the outline is one call to run; the last call
// also writes the summary, given every task so far as context. It
// returns how many tasks were added.
func digestSession(db *store.DB, s *store.TaskSession, run func(string) (string, error), maxChars int) (int, error) {
	rows, err := db.SessionMessages(s.ID)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	through := rows[len(rows)-1].Ordinal
	turns := promptOutline(rows, s.TasksThrough)
	if len(turns) == 0 {
		// Only replies were added since the last run: nothing to list.
		return 0, db.SaveTasks(s.ID, nil, through, "")
	}
	prior, err := db.SessionTasks(s.ID)
	if err != nil {
		return 0, err
	}

	batches := batchTurns(turns, maxChars)
	var added []store.Task
	summary := ""
	for i, batch := range batches {
		last := i == len(batches)-1
		var b strings.Builder
		extra := ""
		if last {
			extra = summaryInstruction
		}
		fmt.Fprintf(&b, taskInstructions, extra)
		if earlier := append(append([]store.Task{}, prior...), added...); len(earlier) > 0 {
			b.WriteString("\n--- earlier tasks in this session ---\n")
			for _, t := range earlier {
				fmt.Fprintf(&b, "- [#%d] %s\n", t.Ordinal, t.Text)
			}
		}
		if s.Title != "" {
			fmt.Fprintf(&b, "\nSession title: %s\n", s.Title)
		}
		if len(batches) > 1 {
			fmt.Fprintf(&b, "Part %d of %d of the prompts.\n", i+1, len(batches))
		}
		b.WriteString("\n--- prompts ---\n")
		ords := make([]int, len(batch))
		for j, t := range batch {
			ords[j] = t.Ordinal
			b.WriteString(t.render())
		}
		out, err := run(b.String())
		if err != nil {
			return 0, err
		}
		tasks, sum := parseTaskReply(out, ords)
		if len(tasks) == 0 {
			return 0, fmt.Errorf("no task lines in the reply for part %d of %d", i+1, len(batches))
		}
		added = append(added, tasks...)
		if last {
			summary = sum
		}
	}
	return len(added), db.SaveTasks(s.ID, added, through, summary)
}

var taskLine = regexp.MustCompile(`^\s*[-*•]\s*\[#(\d+)\]\s*(.+?)\s*$`)

// parseTaskReply reads "- [#N] task" lines and a "SUMMARY:" line. N must
// be one of the batch's prompt ordinals; any other number snaps to the
// prompt before it, and a task that lands on an ordinal already taken
// is joined to that task instead of replacing it.
func parseTaskReply(out string, ords []int) ([]store.Task, string) {
	var tasks []store.Task
	byOrd := map[int]int{}
	summary := ""
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(trimmed, "SUMMARY:"); ok {
			summary = cleanSummary(rest)
			continue
		}
		m := taskLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		ord := ords[0]
		for _, o := range ords {
			if o <= n {
				ord = o
			}
		}
		text := truncateBytes(strings.Join(strings.Fields(m[2]), " "), 300)
		if i, ok := byOrd[ord]; ok {
			tasks[i].Text += "; " + text
			continue
		}
		byOrd[ord] = len(tasks)
		tasks = append(tasks, store.Task{Ordinal: ord, Text: text})
	}
	return tasks, summary
}

// runDigestCmd pipes prompt into `sh -c shellCmd` with a per-call timeout
// and returns the raw reply.
// The shell runs in its own process group so a timeout kills the LLM
// process too, not just sh, and WaitDelay stops a lingering child that
// holds stdout from blocking the read.
func runDigestCmd(ctx context.Context, shellCmd, prompt string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, digestCallTimeout)
	defer cancel()
	runner := exec.CommandContext(cctx, "sh", "-c", shellCmd)
	runner.Stdin = strings.NewReader(prompt)
	runner.Stderr = os.Stderr
	setProcessGroup(runner)
	runner.WaitDelay = 5 * time.Second
	out, err := runner.Output()
	if cctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("timed out after %s", digestCallTimeout)
	}
	return string(out), err
}

// resolveDigestCmd: explicit flag > $AII_DIGEST_CMD > claude -p (haiku,
// no transcript, no hooks) if claude is on PATH > "".
func resolveDigestCmd(flag string) string {
	if strings.TrimSpace(flag) != "" {
		return flag
	}
	if env := strings.TrimSpace(os.Getenv("AII_DIGEST_CMD")); env != "" {
		return env
	}
	if _, err := exec.LookPath("claude"); err == nil {
		return defaultDigestCmd
	}
	return ""
}

func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func lastBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !isRuneStart(s[i]) {
		i++
	}
	return s[i:]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// cleanSummary takes the first line of the model's reply that has text
// once code fences, a leading "Summary:" label and wrapping quotes are
// removed, and caps its length.
func cleanSummary(out string) string {
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "```") {
			continue
		}
		l = strings.TrimSpace(strings.TrimPrefix(l, "Summary:"))
		if n := len(l); n >= 2 && strings.ContainsRune("\"'`", rune(l[0])) && l[n-1] == l[0] {
			l = strings.TrimSpace(l[1 : n-1])
		}
		if strings.Trim(l, "\"'` ") == "" {
			continue
		}
		return truncateBytes(l, 300)
	}
	return ""
}
