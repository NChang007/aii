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
#   sh integrations/claude-code/install.sh --with-backup  # weekly `aii backup` (macOS)
#   (flags combine: --with-mcp --with-digest --with-backup)
#   AII_SKIP_BUILD=1 sh integrations/claude-code/install.sh

set -eu

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
HERE="$ROOT/integrations/claude-code"
CLAUDE_DIR="${CLAUDE_CONFIG_DIR:-$HOME/.claude}"
BIN_DIR="${AII_INSTALL_DIR:-$HOME/.local/bin}"

die() { echo "error: $*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

# Parse flags before touching anything, so a typo changes nothing.
WITH_MCP=0; WITH_DIGEST=0; WITH_BACKUP=0
for arg in "$@"; do
    case "$arg" in
        --with-mcp) WITH_MCP=1 ;;
        --with-digest) WITH_DIGEST=1 ;;
        --with-backup) WITH_BACKUP=1 ;;
        -h|--help) sed -n '2,14p' "$0"; exit 0 ;;
        *) die "unknown flag $arg (see --help)" ;;
    esac
done

# launchd and the settings file need an absolute path.
case "$BIN_DIR" in /*) ;; *) BIN_DIR="$(pwd)/$BIN_DIR" ;; esac

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
    # launchd pins a code requirement to the binary it last spawned and
    # refuses a rebuilt one (exit 78, EX_CONFIG) until the job is reloaded.
    if [ "$(uname -s)" = Darwin ]; then
        for label in com.aii.index com.aii.digest com.aii.backup; do
            plist="$HOME/Library/LaunchAgents/$label.plist"
            [ -f "$plist" ] || continue
            launchctl unload "$plist" 2>/dev/null || true
            launchctl load "$plist" 2>/dev/null && echo "reload  $label"
        done
    fi
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

if [ "$WITH_MCP" = 1 ]; then
    have claude || die "claude CLI not on PATH; cannot register the MCP server"
    if claude mcp list 2>/dev/null | grep -q '^aii:'; then
        echo "mcp     already registered"
    else
        claude mcp add -s user aii -- "$BIN_DIR/aii" mcp
        echo "mcp     registered (user scope)"
    fi
fi

# --- scheduled jobs (optional, macOS) ----------------------------------

xml() { printf '%s' "$1" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g'; }

# install_agent LABEL SCHEDULE_XML ARGS...: write and load a LaunchAgent
# that runs `aii ARGS...` on SCHEDULE_XML, carrying PATH (pinned to the
# directories of claude and node, since launchd starts jobs with a
# minimal PATH) and the env vars aii reads.
install_agent() {
    label=$1; schedule=$2; shift 2
    plist="$HOME/Library/LaunchAgents/$label.plist"
    log="$HOME/.local/share/aii/${label#com.aii.}.log"
    mkdir -p "$(dirname "$plist")" "$(dirname "$log")"
    job_path=""
    for tool in claude node; do
        if have "$tool"; then
            bin=$(command -v "$tool")
            case "$bin" in /*) job_path="$job_path$(dirname "$bin"):" ;; esac
        fi
    done
    job_path="${job_path}/usr/bin:/bin:/usr/sbin:/sbin"
    extra_env=""
    for var in AII_DB AII_DIGEST_CMD; do
        eval "val=\${$var:-}"
        [ -n "$val" ] && extra_env="$extra_env    <key>$var</key>
    <string>$(xml "$val")</string>
"
    done
    prog_args="    <string>$(xml "$BIN_DIR/aii")</string>
"
    for a in "$@"; do
        prog_args="$prog_args    <string>$(xml "$a")</string>
"
    done
    cat > "$plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>$label</string>
  <key>ProgramArguments</key>
  <array>
$prog_args  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>$(xml "$job_path")</string>
$extra_env  </dict>
$schedule
  <key>StandardOutPath</key>
  <string>$(xml "$log")</string>
  <key>StandardErrorPath</key>
  <string>$(xml "$log")</string>
</dict>
</plist>
PLIST
    plutil -lint "$plist" >/dev/null || die "generated $plist is not a valid plist"
    launchctl unload "$plist" 2>/dev/null || true
    if launchctl load "$plist" 2>/dev/null; then
        echo "agent   $label (log: $log)"
    else
        echo "warning: wrote $plist but launchctl could not load it (no GUI session?); it loads at next login" >&2
    fi
}

if [ "$WITH_DIGEST" = 1 ] || [ "$WITH_BACKUP" = 1 ]; then
    [ "$(uname -s)" = Darwin ] || die "--with-digest/--with-backup install LaunchAgents and are macOS only; schedule \`aii digest\` / \`aii backup\` with cron instead"
fi
if [ "$WITH_DIGEST" = 1 ]; then
    have claude || die "claude CLI not on PATH; aii digest needs it"
    install_agent com.aii.digest "  <key>StartInterval</key>
  <integer>3600</integer>" digest --limit 20
fi
if [ "$WITH_BACKUP" = 1 ]; then
    # Weekly, Sunday 03:00. launchd runs a time missed while asleep at next
    # wake; one missed while powered off is skipped until the next Sunday.
    install_agent com.aii.backup "  <key>StartCalendarInterval</key>
  <dict>
    <key>Weekday</key>
    <integer>0</integer>
    <key>Hour</key>
    <integer>3</integer>
    <key>Minute</key>
    <integer>0</integer>
  </dict>" backup --keep 2
fi

echo "done. Open /hooks once in a running session, or restart it, to load the hook."
