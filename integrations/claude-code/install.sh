#!/bin/sh
# Wire aii into Claude Code as memory: build the binary from this checkout,
# install the aii skill, and register the recall hook (UserPromptSubmit and
# SubagentStart).
# Safe to re-run: existing hooks are kept and nothing is added twice.
#
# Usage (from the repo root):
#   sh integrations/claude-code/install.sh            # binary + skill + hook
#   sh integrations/claude-code/install.sh --with-mcp # also register `aii mcp`
#   sh integrations/claude-code/install.sh --with-digest  # hourly `aii digest` (macOS)
#   (flags combine: --with-mcp --with-digest)
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
    # Build beside the target, then rename: overwriting a running binary in
    # place gets the running process (cron indexer, `aii mcp`) SIGKILLed on
    # macOS; a rename leaves it on the old inode.
    (cd "$ROOT" && go build -o "$BIN_DIR/.aii.new" ./cmd/aii)
    mv -f "$BIN_DIR/.aii.new" "$BIN_DIR/aii"
    echo "built   $BIN_DIR/aii ($("$BIN_DIR/aii" version 2>/dev/null | head -1))"
fi

# --- skill --------------------------------------------------------------

SKILL_DST="$CLAUDE_DIR/skills/aii/SKILL.md"
mkdir -p "$(dirname "$SKILL_DST")"
if [ -f "$SKILL_DST" ] && ! cmp -s "$ROOT/skills/aii/SKILL.md" "$SKILL_DST"; then
    # Timestamped so a re-run never overwrites an earlier backup.
    BAK="$SKILL_DST.bak.$(date +%Y%m%d%H%M%S)"
    cp "$SKILL_DST" "$BAK"
    echo "backup  $BAK"
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
changed = False
for event in ("UserPromptSubmit", "SubagentStart"):
    groups = settings.setdefault("hooks", {}).setdefault(event, [])
    if any(h.get("command") in (cmd, script) for g in groups for h in g.get("hooks", [])):
        print("hook    %s already registered" % event)
        continue
    groups.append({"hooks": [{"type": "command", "command": cmd, "timeout": 10}]})
    changed = True
    print("hook    %s registered in %s" % (event, path))
if not changed:
    sys.exit(0)
tmp = path + ".tmp"
with open(tmp, "w") as f:
    json.dump(settings, f, indent=2)
    f.write("\n")
os.replace(tmp, path)
PY

# --- MCP (optional) -----------------------------------------------------

WITH_MCP=0; WITH_DIGEST=0
for arg in "$@"; do
    case "$arg" in
        --with-mcp) WITH_MCP=1 ;;
        --with-digest) WITH_DIGEST=1 ;;
        *) die "unknown flag $arg" ;;
    esac
done

if [ "$WITH_MCP" = 1 ]; then
    have claude || die "claude CLI not on PATH; cannot register the MCP server"
    if claude mcp list 2>/dev/null | grep -q '^aii:'; then
        echo "mcp     already registered"
    else
        claude mcp add -s user aii -- "$BIN_DIR/aii" mcp
        echo "mcp     registered (user scope)"
    fi
fi

# --- digest (optional, macOS) ------------------------------------------

if [ "$WITH_DIGEST" = 1 ]; then
    [ "$(uname -s)" = Darwin ] || die "--with-digest installs a LaunchAgent and is macOS only; schedule \`aii digest\` with cron instead"
    have claude || die "claude CLI not on PATH; aii digest needs it"
    PLIST="$HOME/Library/LaunchAgents/com.aii.digest.plist"
    LOG="$HOME/.local/share/aii/digest.log"
    # launchd starts jobs with a minimal PATH, so pin claude's directory.
    JOB_PATH="$(dirname "$(command -v claude)"):/usr/bin:/bin:/usr/sbin:/sbin"
    cat > "$PLIST" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.aii.digest</string>
  <key>ProgramArguments</key>
  <array>
    <string>$BIN_DIR/aii</string>
    <string>digest</string>
    <string>--limit</string>
    <string>20</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>$JOB_PATH</string>
  </dict>
  <key>StartInterval</key>
  <integer>3600</integer>
  <key>StandardOutPath</key>
  <string>$LOG</string>
  <key>StandardErrorPath</key>
  <string>$LOG</string>
</dict>
</plist>
PLIST
    launchctl unload "$PLIST" 2>/dev/null || true
    launchctl load "$PLIST"
    echo "digest  hourly LaunchAgent $PLIST (log: $LOG)"
fi

echo "done. Open /hooks once in a running session, or restart it, to load the hook."
