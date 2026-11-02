# aish — plan

An AI-driven shell REPL in Go that replaces zsh/bash *from the user-facing side*
(not for script running). Intent parsing for natural-language lines is done by
LiquidAI **LFM2.5-24B-A2B** served locally via llama.cpp on the Radeon 890M iGPU.

## Machine context

- Fedora 44, AMD Ryzen AI 9 HX 370 (24 threads), Radeon 890M iGPU, 23 GB unified RAM
- LLM backend: llama.cpp b12034 (prebuilt Vulkan release) at `~/tools/llama-b12034/`
- Model: `LiquidAI/LFM2.5-24B-A2B-GGUF — QAD-Q4_0` (13.6 GB) at `~/models/`
  (fits fully on the iGPU; LiquidAI-recommended quality-aware 4-bit quant)
- Serving API: OpenAI-compatible at `127.0.0.1:8081`

## Architecture

Two-speed REPL — critical for shell feel with a local model:

- **Fast path**: line starts with a PATH executable, aish builtin, or plain shell
  syntax → execute natively via `sh -c`, zero model latency.
- **Intent path**: otherwise send to the LLM exposing one OpenAI tool
  `run_command(command: string)`; llama-server (`--jinja`) parses LFM2.5's native
  Pythonic tool-call format so Go receives structured `{name, arguments}`.
  Exit codes are fed back for self-correction (bounded loop).

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
  PLAN.md
  go.mod                       module aish
  cmd/aish/main.go             wiring, flags
  internal/repl/               reedline loop, prompt, routing
  internal/exec/               dispatch, builtins, state (cwd/env), exit codes
  internal/llm/                net/http client, tool schema, prompt assembly
  internal/llm/default-prompt.md   embedded cool default system prompt
  internal/safety/             classifier + confirmation UI
  internal/servermgmt/         llama-server health check, spawn, stop
  internal/history/            persistent history (~/.local/state/aish/)
  internal/config/             all knobs (below), prompt file loader
```

Dependencies kept minimal: `muesli/reedline` (+ `x/term`), no LLM SDK, no heavy TUI.

## Configuration

Precedence: **flag > env > `~/.config/aish/config.toml` (optional) > default**.

| Knob | Env var | Default | Meaning |
|---|---|---|---|
| Max agentic steps | `AISH_MAX_STEPS` | `5` | tool-call round-trips per request (0 = intent mode off; 1 = single-step) |
| Context turns | `AISH_HISTORY_TURNS` | `5` | recent exchanges fed back |
| Command timeout | `AISH_CMD_TIMEOUT` | `2h` | per-command |
| LLM call timeout | `AISH_LLM_TIMEOUT` | `2m` | per model request |
| Confirm policy | `AISH_CONFIRM` | `safe-auto` | `safe-auto` / `always` / `never` |
| Server port | `AISH_LLM_PORT` | `8081` | also `AISH_LLM_HOST`, `AISH_LLM_CTX` |
| Server lifecycle | `AISH_KEEP_ALIVE` | `0` | `1` = leave llama-server running on exit |
| System prompt | `AISH_PROMPT_FILE` | `~/.config/aish/prompt.md` | missing → embedded default |

The system prompt is byte-identical across calls (KV prefix cache hits). Only the
persona/rules section is user-replaceable; tool/safety contract is enforced
client-side.

### Default system prompt (sketch, tuned against real M0 latency before bake-in)

```markdown
# aish
You are aish, a shell copilot running on this machine: Fedora 44,
AMD Ryzen AI 9 HX 370 (Radeon 890M), user `lvturner`, zsh is the fallback executor.

## Style
- Terse. Reply like a shell, not a chatbot: no code fences, no "sure!",
  no restating the request.
- One `run_command` step per turn; assume output is fed back to you.
- Run boring commands (`ls`, `grep`) silently; explain only on failure
  or surprising output — one or two sentences, then done.

## Execution
- Prefer one composed, explicit command: pipes, `xargs`, `find -exec`, `git -C`.
- Machine tooling: `podman`, `dnf`, `python3.14`.
- Destructive ops must first emit a one-line consequence statement,
  e.g. `waptop: removes the 3 untracked files`.
- Ambiguous request → pick the common interpretation and act; ask only
  when the wrong choice loses data.
- Never invent files/paths — verify with a command first when unsure.
```

## Milestones

- **M0 — model foundation**: parallel-range download of the GGUF
  (single stream measured ~1.6 MB/s ≈ 2 h; parallel ≈ 15–20 min), SHA-256
  verification, boot llama-server, verify OpenAI tool-call parsing with
  `--jinja`, record real tok/s on the 890M.
- **M1 — native skeleton**: reedline REPL, router (PATH/builtin), `sh -c`
  dispatch with streams attached, `cd`/`export` builtins, exit-code display,
  persistent history, config package.
- **M2 — intent path**: llm client + `run_command` tool, servermgmt
  (auto-start/health-check/auto-stop), safety classifier + confirm UI,
  single-step execute + error feedback loop.
- **M3 — agentic loop & ergonomics**: multi-step loop (AISH_MAX_STEPS),
  follow-up context, `??` mode, `!` prefix, PATH completion.
- **M4 — polish**: README, `aish doctor`, configurable never-auto pattern
  list, optional systemd user unit for llama-server.
