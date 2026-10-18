import json, os, re, shutil, subprocess, sys

AII = shutil.which("aii") or os.path.expanduser("~/.local/bin/aii")
TRIGGER = re.compile(
    r"\b(back to|where (were|did) we|left off|last time|last session|earlier session|"
    r"we (decided|agreed|discussed|fixed|tried|built|talked)|did we|you (said|told|suggested)|"
    r"remember|again|already (fixed|did|tried|solved)|before\b.*\b(fix|did|said)|"
    r"previous(ly)?|pick up|continue (with|on)|resume)\b",
    re.I,
)
STOP = set("""a an the and or but so to of in on at for with from by is are was were be been
it this that these those i you we me my our your us back where did do does last time session
remember again already before previously pick up continue resume left off decided agreed
discussed fixed tried built talked said told suggested what how why when which can could
should would will just alright ok okay hey so like about there here then now please let""".split())

try:
    data = json.load(sys.stdin)
except Exception:
    sys.exit(0)
prompt = data.get("prompt") or ""
if not TRIGGER.search(prompt):
    sys.exit(0)

cwd = data.get("cwd") or ""
live = (data.get("session_id") or "")[:8]
words = [w for w in re.findall(r"[A-Za-z0-9_\-]{3,}", prompt.lower()) if w not in STOP]
query = " ".join(dict.fromkeys(words[:4]))

def run(args):
    try:
        out = subprocess.run([AII, *args], capture_output=True, text=True, timeout=4).stdout
    except Exception:
        return []
    rows = []
    for line in out.splitlines():
        try:
            rows.append(json.loads(line))
        except Exception:
            pass
    return [r for r in rows if live not in (r.get("cite") or "")]

lines = []
# aii ANDs the terms, so drop the leading word until something matches.
terms = query.split()
hits = []
while terms and not hits:
    hits = run(["search", " ".join(terms), "--workspace", cwd, "--since", "30d", "--limit", "8", "--ndjson"])
    terms = terms[1:]
if hits:
    seen = set()
    for r in hits:
        sid = r["cite"].split(":")[0]
        if sid in seen:
            continue
        seen.add(sid)
        snip = re.sub(r"\s+", " ", (r.get("snippet") or "").replace("«", "").replace("»", ""))[:140]
        lines.append(f"- {r['cite']}  {snip}")
        if len(lines) == 3:
            break
if not lines:
    for r in run(["sessions", "--workspace", cwd, "--limit", "3", "--ndjson"]):
        lines.append(f"- {r['cite']}  ({r.get('message_count', '?')} msgs) {(r.get('title') or '')[:100]}")
if not lines:
    sys.exit(0)

ctx = ("This prompt refers to earlier work. Possibly relevant past sessions (aii pointers, not full context):\n"
       + "\n".join(lines)
       + "\nIf you lack the context, recover it with the aii skill (`aii show <uid> --max-msg-chars 600 | tail -60`) "
         "before asking the user to repeat themselves.")
print(json.dumps({"hookSpecificOutput": {"hookEventName": "UserPromptSubmit", "additionalContext": ctx}}))
