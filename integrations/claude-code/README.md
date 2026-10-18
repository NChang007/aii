# aii for Claude Code

Makes aii behave like memory in Claude Code without loading old
transcripts into every session.

| Piece | What it does | Context cost |
|---|---|---|
| `skills/aii/SKILL.md` | Teaches the agent how to search and cite past sessions | Description only, until used |
| `hooks/aii-recall.sh` | On prompts that refer to past work ("back to", "did we", "last time", "remember"…), injects up to 3 aii cite tokens with one-line snippets | Zero on other prompts |
| `hooks/aii-recall.sh` on SubagentStart | Gives each subagent one line saying aii exists (the event carries no task text to search on) | ~40 tokens per subagent |
| `aii mcp` (optional) | Exposes `search`, `list_sessions`, `get_session`, `related`, `stats` as tools | Tool names only |

## Install

From the repo root:

```sh
sh integrations/claude-code/install.sh            # build aii, install skill + hook
sh integrations/claude-code/install.sh --with-mcp # also register the MCP server
sh integrations/claude-code/install.sh --with-digest # hourly one-line session summaries (macOS)
sh integrations/claude-code/install.sh --with-backup # weekly index snapshot, newest 2 kept (macOS)
```

`--with-backup` runs `aii backup --keep 2` every Sunday at 03:00 (or at
next wake), writing a compacted `VACUUM INTO` snapshot to
`~/Backups/aii/aii-<timestamp>.db` and removing older `aii-*.db`
snapshots there. Claude Code deletes transcripts after
`cleanupPeriodDays`, so the index can be the only copy of old sessions.

`--with-digest` runs `aii digest --limit 20` every hour. It summarises
finished sessions with `claude -p --model haiku` (no transcript written,
hooks off), about 5 s and one small Haiku call per session; summaries
show up in `aii sessions`, search results and `list_sessions`.

The installer builds `./cmd/aii` into `~/.local/bin/aii`, backs up any
different existing skill to `SKILL.md.bak.<timestamp>`, and adds the hook to
`~/.claude/settings.json` next to your other `UserPromptSubmit` hooks.
Re-running it is safe.

## How the hook decides

1. Regex match on recall phrasing. No match → exit silently.
2. Keywords from the prompt (stopwords dropped, first 4 kept) are
   searched in the current workspace over the last 30 days; the
   leading word is dropped until something matches.
3. No hits → falls back to the 3 most recent sessions in the workspace.
4. The live session is excluded so the agent doesn't find itself.

Edit `TRIGGER` in `hooks/aii-recall.py` to catch phrasing it misses.

## Limits

- Subagents get a standing pointer, not search results: `SubagentStart`
  input has no task text.
- Hooks run with a 10 s timeout; if aii is slow or missing the hook
  prints nothing and the prompt goes through.
