# aish

An AI shell for daily interactive use — a REPL that replaces the zsh/bash
*user experience* (not for running scripts). Natural-language lines are
parsed for intent by **LiquidAI LFM2.5-24B-A2B** running locally via
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

## Requirements

- Linux, Go 1.22+ to build
- [llama.cpp](https://github.com/ggml-org/llama.cpp) `llama-server` (built with
  Vulkan works on the AMD Radeon 890M iGPU this machine ships with)
- The model GGUF: `LiquidAI/LFM2.5-24B-A2B-GGUF` (any quant; QAD-Q4_0 default)

## Build

```
go build -o bin/aish ./cmd/aish
```

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
| Server host/port | `AISH_LLM_HOST` / `AISH_LLM_PORT` | `127.0.0.1` / `8081` | llama-server endpoint |
| Model server ctx | `AISH_LLM_CTX` | `32768` | context length passed to llama-server |
| Server binary | `AISH_LLM_BIN` | `~/tools/llama-b12034/llama-server` | llama-server executable |
| Model file | `AISH_MODEL` | `~/models/LFM2.5-24B-A2B-QAD-Q4_0.gguf` | GGUF to serve |
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

## Project layout

See [PLAN.md](PLAN.md) for the design rationale and milestones.
