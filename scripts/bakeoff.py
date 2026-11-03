#!/usr/bin/env python3
"""Model bake-off for aish.

Sends the same system prompt, tool schema, and cwd context aish uses to a
llama-server instance, and scores whether the model proposes the right
command for a set of real shell tasks. Read-only: nothing is executed.

usage: bakeoff.py --port 8082 --label LFM2.5-2.6B [--runs 2] [--verbose]
"""
import argparse
import json
import os
import re
import statistics
import sys
import time
import urllib.request

TOOLS = [{
    "type": "function",
    "function": {
        "name": "run_command",
        "description": "Run a command through /bin/sh on this machine. "
                       "The full output (truncated) and exit code come back to you next turn.",
        "parameters": {
            "type": "object",
            "properties": {"command": {"type": "string"}},
            "required": ["command"],
        },
    },
}, {
    "type": "function",
    "function": {
        "name": "find_package",
        "description": "Look up which local package ecosystem provides a piece of "
                       "software (checks dnf and flatpak). Call this BEFORE any install "
                       "command.",
        "parameters": {
            "type": "object",
            "properties": {"name": {"type": "string"}},
            "required": ["name"],
        },
    },
}]

# name, prompt, regex the proposed command must match
SCENARIOS = [
    ("kernel version",      "what kernel version is running on this machine", r"uname\b.*-(r|a)\b"),
    ("free disk space",     "how much free disk space is left on /", r"\bdf\b"),
    ("ram usage",           "how much ram is in use right now", r"\b(free|top|vmstat|ps)\b"),
    ("large files",         "find files bigger than 100MB under /var", r"\b(find|du)\b.*(size|100|M)|\bdu\b"),
    ("listener on 8080",    "what process is listening on port 8080", r"\b(ss|netstat|lsof)\b"),
    ("top cpu processes",   "show the top 5 processes by cpu usage", r"\b(ps|top)\b"),
    ("count /etc files",    "how many files are in /etc", r"\b(ls|find|wc)\b"),
    ("bare word list",      "list", r"\bls\b"),
    ("delete file",         "delete the file /tmp/bakeoff-scratch.txt", r"\brm\b"),
    ("install ripgrep",     "install the ripgrep package", r"\b(dnf|yum|apt)\b.*install|\binstall\b.*(dnf|rg)"),
    ("git changes",         "what changed in this git repository", r"\bgit\s+(status|diff|log|show)\b"),
    ("current time",        "what time is it", r"\bdate\b"),
    ("typo gti status",     "gti status", r"\bgit\s+status\b"),
    ("biggest files here",  "show me the biggest files in this directory", r"\b(du|find|ls)\b"),
    ("python version",      "which python version is installed", r"python3?(\.\d+)?\s+(-V|--version)"),
    ("process by name",     "is sshd running right now", r"\b(pgrep|ps|systemctl|pidof)\b"),
    # scope: unqualified search must stay in the cwd (never $HOME / ~ / /home)
    # scope: unqualified search must stay in the cwd; an absolute cwd path is
    # fine, scanning the home root or ~ is not.
    ("scope: count here",   "how many zentasks folders are there?",
     r"^(?!.*(\$HOME|~|find\s+/\s))(?!.*find\s+/$)"
     r"(?=.*\b(find|ls|du|fd)\b).*"),
]


def system_prompt():
    here = os.path.dirname(os.path.abspath(__file__))
    path = os.path.join(here, "..", "internal", "llm", "default-prompt.md")
    with open(path, "r", encoding="utf-8") as f:
        return f.read().strip()


def context_line():
    home = os.path.expanduser("~")
    return (f"Current working directory: {os.getcwd()}\nHome directory: {home}\n"
            "Shell: /bin/sh. `cd` and `export` are applied in-process and persist across commands.")


def ask(base, prompt, sys_prompt, ctx, timeout):
    body = json.dumps({
        "model": "lfm",
        "temperature": 0.1,
        "tools": TOOLS,
        "messages": [
            {"role": "system", "content": sys_prompt},
            {"role": "system", "content": ctx},
            {"role": "user", "content": prompt},
        ],
    }).encode()
    req = urllib.request.Request(base + "/v1/chat/completions", data=body,
                                 headers={"Content-Type": "application/json"})
    t0 = time.time()
    with urllib.request.urlopen(req, timeout=timeout) as r:
        data = json.load(r)
    wall = time.time() - t0
    msg = data["choices"][0]["message"]
    calls = msg.get("tool_calls") or []
    tool, cmd = "", ""
    if calls:
        tool = calls[0]["function"].get("name", "")
        args = calls[0]["function"].get("arguments", "")
        try:
            obj = json.loads(args)
            if tool == "find_package":
                cmd = f"find_package({obj.get('name', '?')})"
            else:
                cmd = obj.get("command", "") or obj.get("name", "")
        except Exception:
            cmd = args
    return tool, cmd, msg.get("content") or "", wall, data["choices"][0].get("finish_reason")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=8082)
    ap.add_argument("--label", default="model")
    ap.add_argument("--runs", type=int, default=2)
    ap.add_argument("--timeout", type=float, default=180)
    ap.add_argument("--verbose", action="store_true")
    ap.add_argument("--json-out", default="")
    args = ap.parse_args()

    base = f"http://127.0.0.1:{args.port}"
    sys_p = system_prompt()
    ctx = context_line()

    results = []
    print(f"bake-off: {args.label} ({base}), {len(SCENARIOS)} tasks x {args.runs} runs")
    print(f"{'task':20} {'pass':5} {'wall':>7}  command")
    print("-" * 100)
    for name, prompt, pattern in SCENARIOS:
        ok = 0
        walls, cmds = [], []
        for _ in range(args.runs):
            try:
                tool, cmd, content, wall, finish = ask(base, prompt, sys_p, ctx, args.timeout)
            except Exception as e:
                tool, cmd, content, wall, finish = "", "", f"ERROR {e}", 0, "error"
            # a find_package lookup is the correct first step for installs
            hit = bool(re.search(pattern, cmd, re.I)) or (tool == "find_package" and "install" in prompt)
            if tool == "find_package":
                cmd = cmd  # already rendered
            ok += hit
            walls.append(wall)
            cmds.append(cmd or f"(no command) {content[:70]!r}")
        results.append({"task": name, "pass": ok / args.runs,
                        "wall": statistics.mean(walls), "cmds": cmds})
        print(f"{name:20} {ok}/{args.runs:<3} {statistics.mean(walls):6.2f}s  {cmds[0][:60]}")
        if args.verbose:
            for c in cmds[1:]:
                print(f"{'':20} {'':5} {'':7}  {c[:60]}")

    total = sum(r["pass"] for r in results) / len(results)
    avg_wall = statistics.mean(r["wall"] for r in results)
    print("-" * 100)
    print(f"SUMMARY {args.label}: accuracy {total*100:.0f}%  mean wall {avg_wall:.2f}s")

    if args.json_out:
        with open(args.json_out, "w") as f:
            json.dump({"label": args.label, "port": args.port,
                       "accuracy": total, "mean_wall": avg_wall,
                       "results": results}, f, indent=1)


if __name__ == "__main__":
    main()
