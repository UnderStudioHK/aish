# aish — design notes

An AI-driven shell REPL in Go that replaces zsh/bash *from the user-facing side*
(not for script running). Intent parsing for natural-language lines is done by
LiquidAI **LFM3-1.7B** (bake-off winner) served locally via llama.cpp, with
optional Vulkan GPU offload.

## Reference setup

- Linux, AMD Ryzen AI 9 HX 370 (24 threads, Radeon 890M iGPU, 23 GB
  unified RAM) — the machine the bake-off numbers were measured on
- LLM backend: llama.cpp b12034 (prebuilt Vulkan release)
- Model: `LiquidAI/LFM3-1.7B-GGUF — QAD-Q4_0` (~1.05 GB), 94% on the
  shell suite, ~2.1 s/turn, 256K context, ~1.4x faster decode than 2.6B
- Serving API: OpenAI-compatible at `127.0.0.1:8081`

## Architecture

Two-speed REPL — critical for shell feel with a local model:

- **Fast path**: line starts with a PATH executable, aish builtin, or plain shell
  syntax → execute natively via `sh -c`, zero model latency.
- **Intent path**: otherwise send to the LLM exposing two OpenAI tools,
  `run_command(command)` and `find_package(name)`; llama-server (`--jinja`)
  parses LFM3's native tool-call format so Go receives structured
  `{name, arguments}`. Output and exit codes are fed back for
  self-correction (bounded loop).

State & builtins (intercepted in-process so state persists across prompts):
`cd`, `export`, plus `exit`, `history`, `help`, `!` force-native prefix,
`??` ask-only mode (answer without running).

Safety: every LLM-proposed command is displayed. Classify → safe commands
auto-run; anything mutating/destructive (`sudo`, `rm -r`, `dd`, force-push,
package installs, `kill -9`…) gets a `[y/N/e]` preview where `y` alone is not
enough for flagged-irreversible actions (type `yes`). Classifier lives in Go, so
a custom system prompt cannot weaken it. `AISH_CONFIRM=never` overrides.

## Packages

```
aish/
  DESIGN.md
  install.sh                   end-user installer (binary/llama.cpp/model)
  go.mod                       module github.com/UnderStudioHK/aish
  cmd/aish/main.go             wiring, flags, doctor dispatch
  internal/repl/               REPL loop, agent turn/follow-up, routing, typo aid
  internal/prompt/             prompt rendering + cached git detection
  internal/execx/              dispatch, builtins, state (cwd/env), exit codes
  internal/llm/                net/http client, tool schemas, prompt assembly
  internal/llm/default-prompt.md   embedded default system prompt
  internal/safety/             classifier + confirmation UI
  internal/servermgmt/         llama-server health check, spawn, stop
  internal/pkgfind/            dnf/flatpak package resolution
  internal/luatools/           model-authored Lua runtime, registry, hot reload
  internal/history/            persistent history
  internal/config/             all knobs (below), prompt file loader
  third_party/readline/        vendored line editor with ESC-timeout patch
```

Dependencies kept minimal: `chzyer/readline` (vendored) + `x/term`, no LLM
SDK, no heavy TUI.

## Configuration

Precedence: **flag > env > `~/.config/aish/config.toml` (optional) > default**.

| Knob | Env var | Default | Meaning |
|---|---|---|---|
| Max agentic steps | `AISH_MAX_STEPS` | `5` | tool-call round-trips per request (0 = intent mode off; 1 = single-step) |
| Context turns | `AISH_HISTORY_TURNS` | `5` | recent exchanges fed back |
| Command timeout | `AISH_CMD_TIMEOUT` | `2h` | per-command |
| LLM call timeout | `AISH_LLM_TIMEOUT` | `2m` | per model request |
| Confirm policy | `AISH_CONFIRM` | `safe-auto` | `safe-auto` / `always` / `never` |
| Narrate results | `AISH_NARRATE` | `0` | print the model explanation after commands |
| Follow-up mode | `AISH_FOLLOWUP` | `eager` | background follow-up / Enter-to-continue / off |
| Server port | `AISH_LLM_PORT` | `8081` | also `AISH_LLM_HOST`, `AISH_LLM_CTX` |
| Server lifecycle | `AISH_KEEP_ALIVE` | `0` | `1` = leave llama-server running on exit |
| Server binary | `AISH_LLM_BIN` | `$XDG_DATA_HOME/aish/llama.cpp/llama-server` | legacy `~/tools/...` detected |
| Model file | `AISH_MODEL` | `$XDG_DATA_HOME/aish/models/LFM3-1.7B-QAD-Q4_0.gguf` | legacy `~/models/...` detected |
| Data dir | `AISH_HOME` | `$XDG_DATA_HOME/aish` | models + llama.cpp |
| State dir | `AISH_STATE_DIR` | `$XDG_STATE_HOME/aish` | history + audit log |
| System prompt | `AISH_PROMPT_FILE` | `~/.config/aish/prompt.md` | missing → embedded default |
| Lua tools | `AISH_LUA` | `1` | enable model-authored Lua (`0` disables) |
| Tools dir | `AISH_LUA_DIR` | `~/.config/aish/lua` | saved `.lua` tools |
| Lua timeout | `AISH_LUA_TIMEOUT` | `30s` | per-tool call |
| Session tool limit | `AISH_LUA_MAX_TOOLS` | `8` | max loaded tools |
| Tool source limit | `AISH_LUA_MAX_BYTES` | `65536` | max bytes per tool |

The system prompt is byte-identical across calls (KV prefix cache hits). Only the
persona/rules section is user-replaceable; tool/safety contract is enforced
client-side.

### Default system prompt

The built-in prompt lives in
[`internal/llm/default-prompt.md`](internal/llm/default-prompt.md) and is
compiled into the binary. It covers terse shell-style answers, cwd-scoped
grounding, package lookup before installs, `cd`/`export` persistence, tool-only
actions, silent-success handling, and destructive-operation warnings. It is
user-replaceable via `AISH_PROMPT_FILE`; the tool/safety contract is enforced
client-side and cannot be weakened by the prompt.

## Milestones

- **M0 — model foundation**: parallel-range download of the GGUF
  (single stream measured ~1.6 MB/s ≈ 2 h; parallel ≈ 15–20 min), SHA-256
  verification, boot llama-server, verify OpenAI tool-call parsing with
  `--jinja`, record real tok/s on the 890M.
  - [x] llama.cpp Vulkan build installed (`~/tools/llama-b12034`, 890M detected, 20 GB VRAM)
  - [x] tool-call path verified end-to-end (`finish_reason: tool_calls`, structured args)
  - [x] intent loop validated on LFM2.5-2.6B + 230M proxies (correct kernel/disk answers from real tool output, ~5 s/turn)
  - [x] 24B trial cancelled: too slow for a shell; partial download removed
- **M1 — native skeleton** (done): reedline→chzyer/readline REPL, router (PATH/builtin),
  `sh -c` dispatch with streams attached, `cd`/`export` builtins, exit-code display,
  persistent history, config package.
- **M2 — intent path** (done): llm client + `run_command` tool, servermgmt
  (auto-start/health-check/auto-stop), safety classifier + confirm UI,
  execute + error feedback loop.
- **M3 — agentic loop & ergonomics** (done): multi-step loop (AISH_MAX_STEPS),
  session transcript context (user/assistant pairs), `??` mode, `!` prefix,
  PATH completion, special-token sanitizing, robust tool-arg recovery.
- **Tools & grounding** (done): `find_package(name)` local resolver
  (dnf/flatpak) for package provenance; context block with OS, package
  manager, Python; typo hints from PATH edit-distance + system dictionary.
  Bake-off harness `scripts/bakeoff.py` (17 tasks). Search scope defaults
  to the cwd (advisory warning when a proposal leaves it) and every executed
  command is audited to `commands.log`.
- **Esc cancel** (done): vendored `third_party/readline` with a Linux
  `select(2)` ESC-timeout patch; `Esc` dismisses a queued follow-up or
  clears the line.
- **Prompt & behavior docs** (done): Starship-style prompt (smart path,
  git branch/dirty, `✗N`, `⏎` pending marker, root glyph), configurable via
  `AISH_PROMPT_STYLE`/`AISH_PROMPT_TMPL`; full behavior reference in
  `docs/behavior.md`.
- **Confirmation input** (done): confirmations read through readline (own
  history-free instance) instead of a raw `bufio.Reader`, fixing stray
  newlines/double echo and making sudo-style interactive commands reliable;
  verb-like binaries (`install`, `make`, `test`, `yes`) route to the model
  when followed by natural-language arguments.
- **F1 — instant prompt & eager follow-up** (done): sync turn + background
  follow-up with queued proposals, prompt-safe inline hints (readline
  Refresh), Enter-to-accept, cancel/dismiss on new input, epoch-guarded
  state, `AISH_FOLLOWUP=eager|lazy|off`, race-tested under `-race`.
- **Model bake-off** (done): 17-task harness; LFM3-1.7B replaced
  LFM2.5-2.6B (both 94% accuracy, LFM3 ~20% faster, 1.4x decode, 256K
  context, lower hallucination). Harness now declares both tools.
- **Tab completion** (done): command names at the first word, cwd-relative
  file/directory paths everywhere else; `~`, quotes, escaped spaces and
  `--flag=value` supported. Unit-tested and verified through a PTY.
- **Lua tools** (done): model-authored `propose_lua`, stable `run_lua`
  execution, session-only registration, explicit `:lua save`, hot reload,
  no shadowing, narrow gopher-lua sandbox. Design:
  [`docs/lua-tools.md`](docs/lua-tools.md).
- **Packaging** (done): portable XDG defaults with legacy fallback,
  `install.sh` (release binary or source build, llama.cpp Vulkan/CPU, model
  download + SHA-256), Makefile, MIT license, CI and release workflows.
- **M4 — polish** (partial): CLI flags (`--max-steps`, `--model`, `--port`,
  `--ctx`, `--confirm`, `--version`), `aish doctor` done; remaining:
  configurable never-auto pattern list, optional systemd user unit,
  non-Linux support.
