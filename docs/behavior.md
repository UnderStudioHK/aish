# aish behavior reference

Every user-visible behavior in one place. Code lives in `internal/`; the
design rationale and milestones are in `DESIGN.md`.

## 1. Routing: shell first, model second

| Input | Path | Notes |
|---|---|---|
| `ls -la`, `git status`, `echo x > f`, `a \| b` | **native** | no model call, streams attached |
| builtins: `cd`, `export`, `pwd`, `env`, `history`, `help`, `exit` | **native** | applied in-process so state persists |
| `!<cmd>` | **native** | bypasses the model entirely |
| `!!` | **native** | re-runs the last executed command line |
| `??<question>` | **ask-only** | model answers, no tools, nothing runs |
| bare unknown word (`list`, `gti status`) | **intent** | model resolves intent; typos get a hint |
| anything else | **intent** | model proposes `run_command` calls |

The fast-path heuristic treats a line as native when the first token is a real
`PATH` executable or builtin and the tail looks like shell arguments (flags,
paths, globs, numbers). Three or more free-form English words after a real
binary, or an unknown first word, go to the model.

Some binaries are also English verbs. For `install`, `test`, `yes` one
free-form argument, and `make` two, switch the line to intent:

| Line | Path |
|---|---|
| `install figlet` | intent → model resolves it |
| `install -m 644 a b` | native (explicit flags) |
| `make the file readable` | intent |
| `make all` | native |
| `npm run build`, `time ls` | native (not verb-listed) |

### Repeating a request

An **exact repeat** of a request that previously executed commands re-runs
those same commands (through the same safety gates) without asking the model
again. Output is always fresh; latency is zero. The cache is session-scoped
and bounded; declined commands are never cached.

## 2. The model contract

Two tools, both described in the system prompt:

- `run_command(command)` — executes through `/bin/sh -c`. Output streams to
  the terminal **and** is captured (64 KB cap) for the model's next turn,
  together with the exit code.
- `find_package(name)` — read-only resolver. Probes `dnf info` then
  `flatpak search` locally and returns the ecosystem + exact install command
  (e.g. “found in dnf … sudo dnf install ripgrep”). Never gated, never
  executes anything.

Every request carries a live context block (placed after the static prompt so
the KV prefix cache still hits):

```
Search scope: the current working directory unless the user names another path.
Current working directory: /home/user/Documents/Code/aish
Home directory: /home/user
OS: Fedora Linux 44 (...)
System package manager: dnf (Fedora/RHEL). Use it for system packages; never
use pip for system packages (use pipx/uv only for Python CLI tools).
Python: python3.14.
Shell: /bin/sh. `cd` and `export` are applied in-process and persist.
```

Unknown first words get a deterministic typo note (`gti` → closest commands
`git`, …) derived from `PATH` + `/usr/share/dict/words` (real English words
are never treated as typos).

## 3. Execution and result status

- Commands run with stdin/stdout/stderr attached: progress bars, pagers and
  interactive prompts work.
- Model-proposed commands are always echoed (bold; yellow = confirm-level,
  red = destructive).
- **Status lines** make silent outcomes explicit:
  - `(no output, exit 0)` — success with no text (e.g. `git remote -v`
    with no remotes)
  - `✗ exit N` — failure (stderr, when any, is printed above it)
- `cd`/`export` emitted by the model are applied **in-process** (persist).
  A compound `cd x && …` runs in a subshell; the feedback note says so.
- Tool feedback to the model contains `exit_code=N` plus output, and an
  explicit “(no output — the command succeeded silently)” marker so the
  model does not retry silent successes.
- **Duplicate suppression**: inside one agentic chain, a proposal that
  normalizes to the command just executed (ignoring `2>&1`, `|| …`,
  redirects) is not run again; the chain stops with
  `↳ repeated command ignored`.
- **Audit log**: every executed command (typed, proposed, replayed) is
  appended to `~/.local/state/aish/commands.log`:
  `RFC3339<TAB>cwd<TAB>exit_code<TAB>command`.

## 4. Follow-ups (multi-turn)

After a tool batch executes, the prompt returns immediately and the model
reviews the output in the background (`AISH_FOLLOWUP=eager`, default):

- final answer → stored in the conversation silently (shown with
  `AISH_NARRATE=1`, or automatically when the command produced no output);
- another command → queued, shown above the prompt:
  `↳ follow-up: <cmd>   (⏎ run · any input dismisses)`.

Controls:

| Key/input | Effect |
|---|---|
| **Enter** on an empty prompt | run the queued suggestion (safety gates apply) |
| **Esc** | dismiss the queued suggestion (synthetic “not run” result keeps the conversation valid) |
| typing anything | dismisses the suggestion and processes the input; the in-flight turn is cancelled |
| `AISH_FOLLOWUP=lazy` | no background inference; `↳ press ⏎ to continue` |
| `AISH_FOLLOWUP=off` | no follow-ups at all |

Steps are bounded by `AISH_MAX_STEPS` (initial batch = 1, each accepted
follow-up +1). Exhaustion prints `aish: stopped after N steps`.

## 5. Safety

Every proposal is displayed. The classifier rates commands:

- **safe** — auto-run in `safe-auto` mode.
- **confirm** — `sudo`, package managers, `chmod`, `kill`, `git push|reset`,
  `podman/docker rm`, service changes, plain `rm`, … → `y`/`n`/`e` prompt.
- **destructive** — `rm -rf`, `dd`, `mkfs`, force-push, `git reset --hard`,
  `truncate -s 0`, `shutdown`… → red, requires typing `yes`.

`e` opens `$EDITOR` on the proposed command; `Ctrl-C`, `Esc`, or a bare
Enter cancel without running anything. `AISH_CONFIRM=always` prompts for
everything; `never` auto-runs safe/confirm commands but still demands `yes`
for destructive ones. The classifier lives in Go, so a custom system prompt
cannot weaken it.

Confirmation input goes through the same readline instance as commands (a
dedicated history-free reader), never a second `bufio.Reader` on stdin. That
single terminal owner is why typed answers echo once, arrow editing works,
and stray newlines cannot silently cancel a command or leak into the next
prompt as a request.

## 6. Scope and grounding

- Unqualified requests (“here”, “this project”, “how many X are there”)
  default to the **current working directory**; the model is told this
  explicitly and instructed to use the named package manager.
- A dim advisory appears when a proposal touches `$HOME`/`~` while cwd is a
  subdirectory and the request did not mention home:
  `⚠ scope: searches /home/user while cwd is /home/user/Documents/Code`.
- `? find_package <name>` shows local package resolution in the transcript.

## 7. Prompt display

Default (`AISH_PROMPT_STYLE=full`):

```
~/D/C/aish  master*  ✗1 ⏎ ❯
```

- **path**: `~` at home; relative under home; earlier components abbreviated
  to their first letter (dotfiles keep two) and the last kept full.
- **git**: branch name (magenta), `*` when dirty; detached HEAD shows
  `@<sha>`; hidden outside a work tree. Detection is cached (~500 ms) with
  a 120 ms timeout.
- **exit**: `✗N` (red) only when the last command failed.
- **pending**: `⏎` (yellow) while a follow-up suggestion is queued.
- **glyph**: `❯` green/red by last status, `#` when root; ASCII `>` when
  colors are off. `NO_COLOR=1` or a non-TTY disables ANSI.

`AISH_PROMPT_STYLE=minimal` renders just `cwd ❯`.

### Custom template

`~/.config/aish/prompt.tmpl` (override: `AISH_PROMPT_TMPL`) is used verbatim;
placeholders are substituted and a trailing space is added if missing:

| Placeholder | Value |
|---|---|
| `{cwd}` | shortened working directory |
| `{branch}` / `{detached}` | branch name / `@sha` (detached) |
| `{dirty}` | `*` when dirty, else empty |
| `{git}` | branch (+ `*`), empty outside a repo |
| `{exit}` | `✗N` when non-zero, else empty |
| `{pending}` / `{busy}` | `⏎` queued suggestion / `⋯` follow-up in flight |
| `{user}` / `{host}` | login values |
| `{glyph}` | `❯` / `#` / `>` |

Templates may contain their own ANSI escapes; values are inserted plain.
A missing file is ignored; a malformed style value is rejected at startup.

## 8. Configuration

Precedence: flag > env > defaults. Flags: `--max-steps`, `--model`, `--port`,
`--ctx`, `--confirm`, `--version`; `aish doctor` reports health.

| Env var | Default | Meaning |
|---|---|---|
| `AISH_MAX_STEPS` | `5` | agentic steps per request (0 disables the model) |
| `AISH_HISTORY_TURNS` | `5` | prior exchanges kept as context |
| `AISH_CMD_TIMEOUT` | `2h` | per-command timeout |
| `AISH_LLM_TIMEOUT` | `2m` | per model request |
| `AISH_CONFIRM` | `safe-auto` | `safe-auto` / `always` / `never` |
| `AISH_FOLLOWUP` | `eager` | `eager` / `lazy` / `off` |
| `AISH_NARRATE` | `0` | show model explanations after commands |
| `AISH_PROMPT_STYLE` | `full` | `full` / `minimal` |
| `AISH_PROMPT_TMPL` | `~/.config/aish/prompt.tmpl` | prompt template |
| `AISH_PROMPT_FILE` | `~/.config/aish/prompt.md` | system prompt override |
| `AISH_LLM_HOST` / `AISH_LLM_PORT` | `127.0.0.1` / `8081` | llama-server |
| `AISH_LLM_CTX` | `32768` | llama-server context |
| `AISH_LLM_BIN` | `$XDG_DATA_HOME/aish/llama.cpp/llama-server` | server binary (legacy `~/tools/...` auto-detected) |
| `AISH_MODEL` | `$XDG_DATA_HOME/aish/models/LFM3-1.7B-QAD-Q4_0.gguf` | GGUF (legacy `~/models/...` auto-detected) |
| `AISH_KEEP_ALIVE` | `0` | leave llama-server running on exit |
| `AISH_HOME` | `$XDG_DATA_HOME/aish` | models + llama.cpp base directory |
| `AISH_STATE_DIR` | `$XDG_STATE_HOME/aish` | history, logs, audit |

## 9. State and troubleshooting

- History: `~/.local/state/aish/history` (owned by readline)
- llama-server log: `~/.local/state/aish/llama-server.log`
- Command audit: `~/.local/state/aish/commands.log`
- `AISH_DEBUG=1` prints intent/follow-up timings, replay hits and model
  request dumps (`$TMPDIR/aish-dbg/aish_dbg_*.json`).
- `aish doctor` checks model file, server binary, prompt, endpoint and knobs.
- `SIGTERM` (and Ctrl-C while idle) shuts down cleanly; the spawned
  llama-server is tied to aish with `Pdeathsig`, so even a `SIGKILL` of aish
  cannot leave an orphan model server holding VRAM.

## 10. Known constraints

- Esc requires the vendored `third_party/readline` patch; on non-Linux builds
  bare Esc falls back to the upstream blocking behavior.
- The model is single-flight: one request at a time (initial turn or
  background follow-up), so llama-server never sees concurrent slots.
- Agent-proposed commands are executed via `/bin/sh`, not zsh; shell syntax
  is POSIX, not zsh-specific.
