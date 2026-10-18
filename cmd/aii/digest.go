package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
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

const digestInstructions = `Summarize this AI coding session in ONE line of at most 25 words, for a searchable index of past work.
Say what the user wanted, what was decided or built, and how it ended (done, abandoned, or unfinished).
Reply with the line only: no preamble, no quotes, no markdown.

--- session ---
`

// cmdDigest writes a one-line summary for finished sessions that have
// none, so `aii sessions` and search results say what a session was
// about without opening it.
func cmdDigest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("digest", flag.ExitOnError)
	limit := fs.Int("limit", 20, "summarise at most this many sessions per run")
	agent := fs.String("agent", "", "cc|codex|cursor|pi|opencode|gemini|copilot|aider|cline|roo")
	since := fs.String("since", "", "only sessions started since: 7d | 2026-01-01")
	minMsgs := fs.Int("min-messages", 6, "skip sessions with fewer messages")
	idle := fs.Duration("idle", time.Hour, "skip sessions written to more recently than this")
	maxChars := fs.Int("max-chars", 12000, "transcript bytes sent per session")
	cmdStr := fs.String("cmd", "", "LLM command to pipe each prompt into (default: $AII_DIGEST_CMD or claude -p with haiku)")
	dryRun := fs.Bool("dry-run", false, "list the sessions that would be summarised and print the first prompt")
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

	todo, err := db.SessionsNeedingSummary(store.SummaryFilter{
		Agent:       normalizeAgent(*agent),
		SinceUnix:   sinceUnix,
		EndedBefore: time.Now().Add(-*idle).Unix(),
		MinMessages: *minMsgs,
		Limit:       *limit,
	})
	if err != nil {
		return err
	}
	if len(todo) == 0 {
		fmt.Fprintln(os.Stderr, "aii digest: nothing to summarise")
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
		rows, err := db.SessionMessages(s.ID)
		if err != nil {
			return err
		}
		transcript := digestTranscript(rows, *maxChars)
		if strings.TrimSpace(transcript) == "" {
			if !*dryRun {
				failed++
				if err := db.RecordDigestFailure(s.ID); err != nil {
					return err
				}
			}
			continue
		}
		prompt := digestInstructions + transcript
		if *dryRun {
			fmt.Printf("%s/%s  %s\n", short(s.Agent), shortUID(s.UID), s.Title)
			if !printed {
				fmt.Fprintf(os.Stderr, "--- first prompt ---\n%s\n--- end ---\n", prompt)
				printed = true
			}
			continue
		}
		summary, err := runDigestCmd(ctx, shellCmd, prompt)
		if err != nil || summary == "" {
			if ctx.Err() != nil {
				break
			}
			failed++
			fmt.Fprintf(os.Stderr, "aii digest: %s/%s: no summary (%v)\n", short(s.Agent), shortUID(s.UID), err)
			if err := db.RecordDigestFailure(s.ID); err != nil {
				return err
			}
			continue
		}
		if err := db.SetSummary(s.ID, summary); err != nil {
			return err
		}
		done++
		fmt.Printf("%s/%s  %s\n", short(s.Agent), shortUID(s.UID), summary)
	}
	if !*dryRun {
		fmt.Fprintf(os.Stderr, "aii digest: %d summarised, %d failed\n", done, failed)
	}
	return nil
}

// runDigestCmd pipes prompt into `sh -c shellCmd` with a per-call timeout.
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
	return cleanSummary(string(out)), err
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

// digestTranscript renders a session as "role: text" lines, keeping only
// what the user typed and the assistant's prose (tool calls, tool output,
// thinking and harness-injected text are dropped). Past maxChars it keeps
// the opening (the ask) and the end (the outcome) around an elision.
func digestTranscript(rows []store.Row, maxChars int) string {
	var lines []string
	for _, r := range rows {
		if r.Role != "user" && r.Role != "assistant" {
			continue
		}
		var keep []string
		for _, l := range strings.Split(r.Content, "\n") {
			if r.Role == "assistant" && strings.HasPrefix(strings.TrimSpace(l), "[tool_use") {
				continue
			}
			keep = append(keep, l)
		}
		text := strings.TrimSpace(strings.Join(keep, "\n"))
		if text == "" {
			continue
		}
		lines = append(lines, r.Role+": "+text+"\n")
	}
	full := strings.Join(lines, "")
	if maxChars <= 0 || len(full) <= maxChars {
		return full
	}
	head := maxChars * 2 / 5
	tail := maxChars - head
	return truncateBytes(full, head) + "\n…\n" + lastBytes(full, tail)
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
