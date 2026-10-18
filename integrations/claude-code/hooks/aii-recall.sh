#!/usr/bin/env bash
# Recall hook (UserPromptSubmit, SubagentStart): when a prompt refers to past work, inject a few aii
# pointers (cite tokens + one-line snippets) instead of whole transcripts.
# Silent on every other prompt, so it costs nothing unless it fires.
exec python3 -I "$(dirname "$0")/aii-recall.py"
