# aish

An AI shell for daily interactive use — a REPL that replaces the zsh/bash
*user experience* (not for running scripts). Natural-language lines are
parsed for intent by **LiquidAI LFM3-1.7B** running locally via
llama.cpp, converted to one or more concrete shell commands, previewed,
and executed — with exit codes fed back for self-correction.

```
~ (aish) explain why my laptop fan just went crazy and fix it
sensors: reports CPU temp of 92°C from the k10temp zone
ps aux --sort=-%cpu | head -8
...
```

- **Fast path** — lines that are unambiguous shell (`ls -la`, `git status`,
  pipes) execute immediately with no model involved.
- **Intent path** — natural language goes to the local model, which plans
  `run_command` tool calls; the REPL gates, runs, and reports back.
- `!cmd` force native, `!!` repeat last, `??question` ask-only (no commands),
  `cd`/`export` persist across prompts, `Ctrl-C` cancels the running child.
- `Esc` dismisses a queued follow-up suggestion or clears the current line.

### What the model can call

- `run_command` — the gate to /bin/sh (results stream live; exit codes fed back).
- `find_package(name)` — resolves software against **local** ecosystems
  (`dnf info`, `flatpak search`) before any install, so the model knows
  whether something is a system package (e.g. `ripgrep` → dnf) instead of
  guessing `pip`. Read-only, never gated.

The request context also carries live OS/package-manager/Python facts, and
a typo aid flags unknown first words with the closest real commands
(`gti` → `git`) using an edit-distance lookup + the system dictionary
(English words like `list` are not treated as typos).

### Prompt

```
~/D/C/aish  master*  ✗1 ⏎ ❯
```

Path (smart-abbreviated), git branch with `*` when dirty, `✗N` only on
failure, `⏎` while a follow-up is queued, and a colored `❯` (`#` as root).
`AISH_PROMPT_STYLE=minimal` shows `cwd ❯` only; a custom template can be
dropped at `~/.config/aish/prompt.tmpl` (`{cwd} {branch} {dirty} {exit}
{pending} {busy} {user} {host} {glyph} …`). See
[docs/behavior.md](docs/behavior.md#7-prompt-display) for the full table.

### Grounding & scope

Unqualified instructions ("here", "this project", "how many X are there")
are scoped to the **current working directory**; the model is told this
explicitly and stops defaulting to `$HOME` or `/`. A dim warning appears
when a proposal leaves the cwd for `$HOME` without being asked. Every
executed command (typed, proposed, replayed) is appended to
`~/.local/state/aish/commands.log` with cwd and exit code for debugging.

### Repeating a request

Typing the same request twice re-runs the commands it produced before
(safety gates still apply) instead of asking the model again — so output is
always fresh and latency is zero. This is why repeating `list` prints a new
directory listing rather than a narration from stale context.

### Multi-turn feel

After a command runs, the prompt returns **immediately** — the model's
follow-up reasoning happens in the background. If it wants to run another
command, a dim hint appears above the prompt:

```
  ↳ follow-up: ls -la /tmp/out   (⏎ run · any input dismisses)
```

Press **Enter** on the empty prompt to run it (same safety gates apply);
type anything else and the proposal is dismissed, with the conversation
kept intact for your next request. `AISH_FOLLOWUP=lazy` instead shows
`↳ press ⏎ to continue` and only calls the model when you press Enter;
`off` disables follow-ups entirely.

## Requirements

- Linux, Go 1.22+ to build
- [llama.cpp](https://github.com/ggml-org/llama.cpp) `llama-server` (built with
  Vulkan works on the AMD Radeon 890M iGPU this machine ships with)
- The model GGUF: **`LiquidAI/LFM3-1.7B-GGUF` QAD-Q4_0** (1.05 GB) by
  default. In the 17-task shell suite it scored 94% at ~2.1 s/turn and
  decoded ~1.4x faster than LFM2.5-2.6B on this iGPU, with 256K context and
  much lower hallucination. Alternatives via `AISH_MODEL`: LFM2.5-2.6B
  (94%, ~2.6 s, kept on disk); LFM2.5-8B-A1B is a reasoning model that
  spends ~600 tokens thinking before acting (3-5x slower here).

## Build

```
go build -o bin/aish ./cmd/aish
```

`go.mod` replaces `github.com/chzyer/readline` with `third_party/readline`
(MIT). The vendored copy carries one small patch: a bare `ESC` would
normally block waiting for the next byte to disambiguate escape sequences,
so we poll the tty for ~60 ms (`select(2)`) and surface it as a standalone
key — used to dismiss follow-up suggestions.

## Run

```
bin/aish
```

On first use aish starts `llama-server` itself (`/health`-checked), and stops
it on exit (`AISH_KEEP_ALIVE=1` to leave it running). The server and model
paths default to where this machine's install lives; see the knob table below.

## Configuration

Precedence: flag (TODO) > env > `~/.config/aish/config.toml` (TODO) > default.
The system prompt is user-replaceable via `AISH_PROMPT_FILE`
(`~/.config/aish/prompt.md`); a built-in default is compiled in. Safety gates
(enforced in Go: classify + confirm + destructive-typing) are **not**
prompt-dependent.

| Knob | Env var | Default | Meaning |
|---|---|---|---|
| Max agentic steps | `AISH_MAX_STEPS` | `5` | tool-call round-trips per request (0 disables the model) |
| Context turns | `AISH_HISTORY_TURNS` | `5` | recent lines fed back for follow-ups |
| Command timeout | `AISH_CMD_TIMEOUT` | `2h` | per command |
| LLM call timeout | `AISH_LLM_TIMEOUT` | `2m` | per model request |
| Confirm policy | `AISH_CONFIRM` | `safe-auto` | `safe-auto` / `always` / `never` |
| Narrate results | `AISH_NARRATE` | `0` | print the model's explanation after commands (default: command output speaks for itself) |
| Follow-up mode | `AISH_FOLLOWUP` | `eager` | `eager` (background) / `lazy` (Enter to continue) / `off` |
| Prompt style | `AISH_PROMPT_STYLE` | `full` | `full` / `minimal` |
| Prompt template | `AISH_PROMPT_TMPL` | `~/.config/aish/prompt.tmpl` | optional placeholder template |
| Server host/port | `AISH_LLM_HOST` / `AISH_LLM_PORT` | `127.0.0.1` / `8081` | llama-server endpoint |
| Model server ctx | `AISH_LLM_CTX` | `32768` | context length passed to llama-server |
| Server binary | `AISH_LLM_BIN` | `~/tools/llama-b12034/llama-server` | llama-server executable |
| Model file | `AISH_MODEL` | `~/models/LFM3-1.7B-QAD-Q4_0.gguf` | GGUF to serve |
| Keep alive | `AISH_KEEP_ALIVE` | `0` | don't stop llama-server on exit |
| Prompt file | `AISH_PROMPT_FILE` | `~/.config/aish/prompt.md` | persona/system prompt |
| State dir | `AISH_STATE_DIR` | `~/.local/state/aish` | history + logs |

## Safety model

Every model-proposed command is displayed. `safe-auto` runs anything the
classifier rates `safe` without a prompt; `confirm`--rated commands need
`y`/Enter; `destructive` (`rm -rf`, `dd`, `git push --force`, package
managers, service restarts, `kill`…) requires typing `yes`. `e` opens
`$EDITOR` to adjust a proposal before running. `AISH_CONFIRM=never` disables
prompts for safe commands only — destructive still asks. There is no way to
disable the destructive gate except by editing source (by design).

## Documentation

- [docs/behavior.md](docs/behavior.md) — complete behavior reference:
  routing & replay, tool contract, status lines, follow-ups, safety,
  scope/grounding, prompt, configuration, audit log, troubleshooting.
- [PLAN.md](PLAN.md) — design rationale and milestones.

## Project layout

```
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
```
