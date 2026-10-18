#!/bin/sh
# Wire aii into Claude Code as memory: build the binary from this checkout,
# install the aii skill, and register the recall hook (UserPromptSubmit).
# Safe to re-run: existing hooks are kept and nothing is added twice.
#
# Usage (from the repo root):
#   sh integrations/claude-code/install.sh            # binary + skill + hook
#   sh integrations/claude-code/install.sh --with-mcp # also register `aii mcp`
#   AII_SKIP_BUILD=1 sh integrations/claude-code/install.sh

set -eu

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
HERE="$ROOT/integrations/claude-code"
CLAUDE_DIR="${CLAUDE_CONFIG_DIR:-$HOME/.claude}"
BIN_DIR="${AII_INSTALL_DIR:-$HOME/.local/bin}"

die() { echo "error: $*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

have python3 || die "python3 is required (the hook and settings merge use it)"

# --- binary -------------------------------------------------------------

if [ "${AII_SKIP_BUILD:-}" != "1" ]; then
    have go || die "go is required to build aii (or set AII_SKIP_BUILD=1)"
    mkdir -p "$BIN_DIR"
    (cd "$ROOT" && go build -o "$BIN_DIR/aii" ./cmd/aii)
    echo "built   $BIN_DIR/aii ($("$BIN_DIR/aii" version 2>/dev/null | head -1))"
fi

# --- skill --------------------------------------------------------------

SKILL_DST="$CLAUDE_DIR/skills/aii/SKILL.md"
mkdir -p "$(dirname "$SKILL_DST")"
if [ -f "$SKILL_DST" ] && ! cmp -s "$ROOT/skills/aii/SKILL.md" "$SKILL_DST"; then
    cp "$SKILL_DST" "$SKILL_DST.bak"
    echo "backup  $SKILL_DST.bak"
fi
cp "$ROOT/skills/aii/SKILL.md" "$SKILL_DST"
echo "skill   $SKILL_DST"

# --- hook ---------------------------------------------------------------

HOOK_DIR="$CLAUDE_DIR/hooks"
mkdir -p "$HOOK_DIR"
cp "$HERE/hooks/aii-recall.sh" "$HERE/hooks/aii-recall.py" "$HOOK_DIR/"
chmod +x "$HOOK_DIR/aii-recall.sh"
echo "hook    $HOOK_DIR/aii-recall.sh"

SETTINGS="$CLAUDE_DIR/settings.json"
python3 -I - "$SETTINGS" "$HOOK_DIR/aii-recall.sh" <<'PY'
import json, os, sys
path, script = sys.argv[1], sys.argv[2]
cmd = json.dumps(script)  # quoted, the way Claude Code writes hook commands
settings = {}
if os.path.exists(path):
    with open(path) as f:
        settings = json.load(f)
groups = settings.setdefault("hooks", {}).setdefault("UserPromptSubmit", [])
if any(h.get("command") in (cmd, script) for g in groups for h in g.get("hooks", [])):
    print("hook    already registered in " + path)
    sys.exit(0)
groups.append({"hooks": [{"type": "command", "command": cmd, "timeout": 10}]})
tmp = path + ".tmp"
with open(tmp, "w") as f:
    json.dump(settings, f, indent=2)
    f.write("\n")
os.replace(tmp, path)
print("hook    registered in " + path)
PY

# --- MCP (optional) -----------------------------------------------------

if [ "${1:-}" = "--with-mcp" ]; then
    have claude || die "claude CLI not on PATH; cannot register the MCP server"
    if claude mcp list 2>/dev/null | grep -q '^aii:'; then
        echo "mcp     already registered"
    else
        claude mcp add -s user aii -- "$BIN_DIR/aii" mcp
        echo "mcp     registered (user scope)"
    fi
fi

echo "done. Open /hooks once in a running session, or restart it, to load the hook."
