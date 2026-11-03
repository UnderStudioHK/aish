# aish

[![ci](https://github.com/UnderStudioHK/aish/actions/workflows/ci.yml/badge.svg)](https://github.com/UnderStudioHK/aish/actions/workflows/ci.yml)

**An AI shell for daily interactive use.** aish is a Go REPL that replaces the
zsh/bash *user experience* — not a script runner. Plain shell lines execute
instantly; natural-language lines are parsed locally by
[LiquidAI LFM3-1.7B](https://huggingface.co/LiquidAI/LFM3-1.7B-GGUF) through
llama.cpp, turned into concrete commands, previewed, and executed. Nothing
leaves the machine.

```console
~/D/C/aish  master* ❯ how much disk is left
df -h /
Filesystem      Size  Used Avail Use% Mounted on
/dev/mapper/…   1.9T  792G  1.1T  43% /

~/D/C/aish  master* ❯ install ripgrep
? find_package ripgrep
↳ follow-up: sudo dnf install ripgrep   (⏎ run · any input dismisses)
```

> ## ⚠️ Proof of concept
>
> aish is an experiment, not a hardened product. It runs real commands with
> your privileges, has **no sandbox**, and a local model can still propose
> something wrong. Every model-proposed command is shown and gated (see
> [Safety](#safety)), but **review what it runs**. Expect rough edges, Linux
> only, and APIs that may change. Do not use it for unattended or privileged
> automation.

## Features

| | |
|---|---|
| **Two-speed REPL** | Unambiguous shell (`ls -la`, pipes, redirects, `git status`) runs natively with zero model latency. Anything else becomes an intent. |
| **Local model only** | LFM3-1.7B (~1 GB, QAD-Q4_0) served by llama.cpp on `127.0.0.1`. No cloud calls, no API keys, no telemetry. |
| **Agentic loop** | The model calls `run_command` and `find_package` tools, sees output and exit codes, and can take several steps (`AISH_MAX_STEPS`). |
| **Live output** | Command output streams straight to your terminal. Silent success and failures are both reported (`(no output, exit 0)`, `✗ exit 1`). |
| **Instant prompt** | The prompt returns as soon as a command finishes; follow-up reasoning happens in the background. Accept a queued step with **Enter**, dismiss it with **Esc** or any input. |
| **Safety gates** | Go-side classifier: safe commands auto-run, risky ones ask `y`, destructive ones require typing `yes`. `e` opens `$EDITOR` before running. The prompt cannot weaken these. |
| **Grounded in your machine** | Live cwd/OS/package-manager context, `dnf`/`flatpak` package lookup before installs, typo hints (`gti` → `git`), and cwd-scoped answers instead of scanning `$HOME`. |
| **Shell state persists** | `cd` and `export` are applied in-process, so they survive across prompts and agent steps. |
| **History & replay** | Persistent history; repeating a request re-runs its previous commands with fresh output instead of asking the model again. |
| **Useful prompt** | Starship-style line with abbreviated path, git branch/dirty marker, last exit status and pending-follow-up indicator. Custom templates supported. |
| **Auditable** | Every executed command is appended to `~/.local/state/aish/commands.log` with cwd and exit code. |
| **Self-checking** | `aish doctor` validates the model, server binary, prompt, endpoint and knobs. |

## Install

### Quick install (recommended)

```bash
curl -fsSL https://raw.githubusercontent.com/UnderStudioHK/aish/main/install.sh | sh
```

The installer:

1. installs the `aish` binary to `~/.local/bin` (release binary, or builds from
   source when Go 1.26+ is available);
2. downloads a prebuilt **llama.cpp** server (Vulkan when a GPU is detected,
   otherwise CPU) into `~/.local/share/aish/llama.cpp`;
3. downloads **LFM3-1.7B-QAD-Q4_0** (~1.0 GB) into
   `~/.local/share/aish/models`, verifying its SHA-256;
4. runs `aish doctor`.

It is idempotent: existing pieces are detected and skipped.

```bash
./install.sh --help          # all options
./install.sh --cpu           # force the CPU llama.cpp build
./install.sh --skip-model    # bring your own GGUF via AISH_MODEL
./install.sh --from-source   # build aish from source
```

If `~/.local/bin` is not on your `PATH`:

```bash
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.zshrc && exec zsh
```

### From source

```bash
git clone https://github.com/UnderStudioHK/aish
cd aish
make build          # -> bin/aish
./bin/aish doctor
```

`go.mod` replaces `github.com/chzyer/readline` with the vendored
`third_party/readline` (MIT). The vendored copy adds a ~60 ms `select(2)`
timeout so a bare **Esc** is surfaced immediately instead of blocking while
the terminal waits for an escape sequence.

### Requirements

- Linux (x86_64 or arm64). Developed and tested on Fedora 44.
- ~2.5 GB free disk for the server + model.
- A Vulkan-capable GPU is optional but strongly recommended; the CPU build
  works and is slower.
- Go 1.26+ **only** when building from source.
- `dnf`/`flatpak` for the package-lookup tool. Other systems still work; the
  model is told which package manager is present.

The default model is distributed under Liquid AI's LFM Open License; see the
[model card](https://huggingface.co/LiquidAI/LFM3-1.7B-GGUF) before
redistributing it. aish's own code is MIT (see [LICENSE](LICENSE)).

## Usage

```console
$ aish

~/D/C/aish  master* ❯ ls -la                 # native: no model call
~/D/C/aish  master* ❯ find files changed today
find . -type f -newermt today
...
```

| Input | What happens |
|---|---|
| `ls -la`, `git status`, `a \| b`, `echo x > f` | **Native** — executed immediately through `/bin/sh`. |
| `cd src`, `export FOO=1`, `pwd`, `history`, `help`, `exit` | **Builtin** — applied in-process so state persists. |
| `how much RAM is free?` | **Intent** — the model plans tool calls. |
| `!make test` | Force native execution (skip the model). |
| `!!` | Repeat the last command. |
| `??why is this build failing` | Ask-only: the model may inspect/explain but is told not to change anything. |
| `Esc` | Dismiss a queued follow-up, or clear the current line. |
| `Ctrl-C` | Interrupt the running child process; aish keeps running. |

### Follow-ups

After a command, the model may want one more step. aish shows it without
blocking:

```text
  ↳ follow-up: ls -la /tmp/out   (⏎ run · any input dismisses)
```

Press **Enter** to run it (same safety gates), or just keep typing to dismiss
it. Modes: `AISH_FOLLOWUP=eager` (default, background), `lazy` (only ask the
model when you press Enter), `off`.

### Prompt

```text
~/D/C/aish  master*  ✗1 ⏎ ❯
```

Smart-abbreviated path, git branch (`*` when dirty), `✗N` only after a failure,
`⏎` while a follow-up is queued, and a colored `❯` (`#` as root).
`AISH_PROMPT_STYLE=minimal` reduces it to `cwd ❯`; a custom template can live
at `~/.config/aish/prompt.tmpl`
(`{cwd} {branch} {dirty} {exit} {pending} {busy} {user} {host} {glyph}`).
See [docs/behavior.md](docs/behavior.md#7-prompt-display).

## How it works

```text
 stdin ──► router ──► native /bin/sh ──► terminal (streamed)
             │
             └──► intent ──► llama-server (127.0.0.1:8081)
                               │  tool call: run_command / find_package
                               ▼
                          safety classifier ──► preview ──► execute
                               ▲                              │
                               └──── output + exit code ◄────┘
```

- **Router** — first word in `PATH`, a builtin, or shell syntax → native.
  Verb-like binaries (`install`, `test`, `yes`, `make`) followed by
  natural-language arguments are routed to the model instead.
- **Model** — an OpenAI-compatible endpoint exposing two tools:
  `run_command` and `find_package`. aish starts `llama-server` on first intent
  and stops it on exit (`AISH_KEEP_ALIVE=1` keeps it running). If aish dies,
  the server is killed with it (`Pdeathsig`).
- **Safety** — classification happens in Go, after the model answers and
  before anything runs. See below.

## Safety

Every proposed command is displayed before execution.

| Class | Examples | Gate |
|---|---|---|
| **safe** | `ls`, `grep`, `git status`, `find`, `df` | runs automatically under `AISH_CONFIRM=safe-auto` |
| **confirm** | writes, `git commit`, most mutations | `y` / Enter, `n` cancels, `e` edits |
| **destructive** | `rm -rf`, `dd`, `git push --force`, package managers, service restarts, `kill` | must type `yes` |

`AISH_CONFIRM=always` gates everything; `never` only skips the *safe* gate —
destructive commands still require `yes`. There is deliberately no setting that
disables the destructive gate.

**Limits:** the classifier is pattern-based, not a sandbox. Shell metacharacters,
scripts, and novel commands can still be harmful. Run aish as a normal user,
keep backups, and read what it proposes.

## Configuration

Precedence: CLI flag > environment > built-in default. (`~/.config/aish/config.toml`
is planned, not implemented.)

| Knob | Env var | Default | Meaning |
|---|---|---|---|
| Max agentic steps | `AISH_MAX_STEPS` | `5` | tool-call round-trips per request (`0` disables the model) |
| Context turns | `AISH_HISTORY_TURNS` | `5` | recent lines fed back for follow-ups |
| Command timeout | `AISH_CMD_TIMEOUT` | `2h` | per command |
| LLM call timeout | `AISH_LLM_TIMEOUT` | `2m` | per model request |
| Confirm policy | `AISH_CONFIRM` | `safe-auto` | `safe-auto` / `always` / `never` |
| Narrate results | `AISH_NARRATE` | `0` | print the model's explanation after commands |
| Follow-up mode | `AISH_FOLLOWUP` | `eager` | `eager` / `lazy` / `off` |
| Prompt style | `AISH_PROMPT_STYLE` | `full` | `full` / `minimal` |
| Prompt template | `AISH_PROMPT_TMPL` | `~/.config/aish/prompt.tmpl` | optional placeholder template |
| Server host/port | `AISH_LLM_HOST` / `AISH_LLM_PORT` | `127.0.0.1` / `8081` | llama-server endpoint |
| Model server ctx | `AISH_LLM_CTX` | `32768` | context length passed to llama-server |
| Server binary | `AISH_LLM_BIN` | `$XDG_DATA_HOME/aish/llama.cpp/llama-server` | llama-server executable |
| Model file | `AISH_MODEL` | `$XDG_DATA_HOME/aish/models/LFM3-1.7B-QAD-Q4_0.gguf` | GGUF to serve |
| Data dir | `AISH_HOME` | `$XDG_DATA_HOME/aish` | base for models + llama.cpp |
| Keep alive | `AISH_KEEP_ALIVE` | `0` | don't stop llama-server on exit |
| Prompt file | `AISH_PROMPT_FILE` | `~/.config/aish/prompt.md` | persona/system prompt |
| State dir | `AISH_STATE_DIR` | `$XDG_STATE_HOME/aish` | history + audit log |

Debug timings and request dumps: `AISH_DEBUG=1` (dumps to
`$TMPDIR/aish-dbg/aish_dbg_*.json`).

## Documentation

- [docs/behavior.md](docs/behavior.md) — complete behavior reference:
  routing and replay, tool contract, status lines, follow-ups, safety,
  scope/grounding, prompt, configuration, audit log, troubleshooting.
- [DESIGN.md](DESIGN.md) — design rationale, model bake-off, milestones.
- [scripts/bakeoff.py](scripts/bakeoff.py) — the 17-task model comparison
  harness used to pick the default model.

## Project layout

```text
cmd/aish/            entry point, flags, doctor dispatch
internal/repl/       REPL loop, agent (sync turn + follow-up), routing, typo aid
internal/prompt/     prompt rendering + cached git detection
internal/execx/      dispatch, builtins, tee/captured execution, state
internal/llm/        OpenAI-compatible client, tool schemas, system prompt
internal/safety/     classifier + confirmation UI
internal/servermgmt/ llama-server lifecycle
internal/pkgfind/    dnf/flatpak package resolution
internal/config/     env/flag knobs
internal/history/    history store
third_party/readline patched line editor (Esc timeout)
scripts/bakeoff.py   model comparison harness
install.sh           end-user installer
```

## Status

Proof of concept, version `0.1.0`. Working: native fast path, agentic intent
path, safety gates, follow-ups, grounding, prompt, installer, CI. Not yet:
scripting/automation mode, non-Linux support, sandboxing, additional package
managers, config file.

Issues and experiments are welcome — please include your distro, GPU/CPU,
model file and `aish doctor` output.

## License

aish is [MIT licensed](LICENSE). The default model and llama.cpp are separate
works under their own licenses.
