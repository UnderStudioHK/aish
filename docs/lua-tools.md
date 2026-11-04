# Model-authored Lua tools — plan

Status: implemented (v0.3.0).

This document records the design for optional, model-authored Lua tools in
aish. The short version: the model can define a small Lua tool for the current
session, use it immediately, and suggest saving it; only the user can persist
it. Tools are namespaced (`lua_<name>` tools plus stable `run_lua`), never
shadow builtins or `PATH`, and hot-reload from disk.

## Goals

- Let the model extend its own toolset at runtime without restarting aish.
- Let the user save a useful generated tool for future sessions.
- Keep extension names from shadowing builtins, `PATH` commands, or aish tools.
- Reload edited tool files without restarting.
- Keep the sandbox minimal but meaningful: narrow host API, no raw OS access,
  timeouts, output caps, gated command execution.

Non-goals for this version:

- Full process isolation / seccomp / WASM sandbox.
- Automatic persistence of model-generated code.
- Allowing tools to replace or modify the safety classifier.
- User-authored plugin marketplace / registry.

## Model flow

```text
request
  │
  ├─ model calls propose_lua {name, description, parameters, code, args?, persist?}
  │     ├─ validate name, schema, size and collisions
  │     ├─ compile the Lua function without running it
  │     └─ register the tool for this session
  │
  ├─ model uses the tool in one of two ways:
  │     ├─ pass args in the same propose_lua call (immediate result), or
  │     └─ call run_lua {name, args} (stable tool; no dynamic tool needed)
  │     └─ lua_<name>(args) also works for models that support the added tool
  │           └─ aish.exec(argv) → classifier → confirm → audit → output
  │
  └─ persist=true
        └─ show a suggestion; user runs `:lua save <name>` to keep it
```

The tool list is rebuilt per model turn, so a newly registered tool is
available on the next turn.

## Tool format

On-disk tools live in `$XDG_CONFIG_HOME/aish/lua/*.lua` (default
`~/.config/aish/lua/`).

```lua
return {
  name = "disk_report",
  description = "Show disk usage for a path",
  parameters = {
    type = "object",
    properties = {
      path = { type = "string", description = "Path to inspect" },
    },
    required = { "path" },
  },
  run = function(args)
    local r = aish.exec({"df", "-h", args.path})
    return r.stdout
  end,
}
```

Loading a file compiles and validates it but does not call `run`. For
model-authored tools, `propose_lua` carries the same metadata plus the
function body.

## Host API

```lua
aish.exec(argv)         -- argv table; returns {code, stdout, stderr}
aish.print(text)
aish.log(text)
aish.cwd()
aish.env_get(name)
aish.json_encode(value)
aish.json_decode(text)
```

- `aish.exec` uses argv, not a shell string. It runs through the same
  classifier, confirmation and audit log as `run_command`, and streams output.
- No `os`, `io`, `package`, `debug`, `load`, `loadfile`, `dofile`, or
  `require` globals.
- `aish.shell` is not exposed to model-authored tools in this version.

## Namespacing / no shadowing

- Built-in tools stay `run_command`, `find_package`.
- New built-in: `propose_lua`.
- Model tools are exposed only as `lua_<name>`.
- In this version, Lua tools are invoked only as `lua_<name>` or through the
  stable `run_lua` tool; bare names never resolve to a tool. `:<name>`
  commands are deliberately future work.
- Name collisions with an existing tool, builtin, or `PATH` command are
  rejected.

## Sandbox and limits

- Pure-Go `github.com/yuin/gopher-lua` per tool state.
- Only `base`, `table`, `string`, `math` libraries; dangerous base functions
  removed.
- Per-call timeout (`AISH_LUA_TIMEOUT`, default `30s`) via `SetContext`.
- Max source size (`AISH_LUA_MAX_BYTES`, default 64 KiB).
- Max session tools (`AISH_LUA_MAX_TOOLS`, default 8).
- Tool result and command output caps.
- Panic recovery; per-tool mutex because Lua states are not concurrency-safe.
- In-process Lua is **not** a security boundary; this is API minimization.

## Hot reload

- Load `*.lua` at startup.
- When a command is submitted, stat the tools directory and reload changed
  files.
- Reload into a fresh Lua state, validate, then swap atomically.
- On compile/validation failure, keep the previous version and print an error.
- `:lua reload` forces a reload.
- Ephemeral tools are memory-only; `:lua save <name>` writes them out and
  triggers the reload path.

## Management commands

```text
:lua                 list session + on-disk tools
:lua show <name>      print metadata and source
:lua save <name>      persist an ephemeral tool
:lua rm <name>        remove an on-disk tool
:lua reload           re-read the tools directory
```

## Integration points

| Area | Change |
|---|---|
| `internal/luatools/` | Lua runtime, validation, registry, hot reload, host API |
| `internal/llm/llm.go` | Dynamic tool list; `propose_lua` schema |
| `internal/repl/agent.go` | Dispatch `propose_lua` and `lua_*` tools |
| `internal/repl/repl.go` | Manager lifecycle, `:lua` commands, hot reload |
| `internal/execx/exec.go` | argv-based tee execution + shell-display quoting |
| `internal/safety/` | unchanged classifier, reused by the exec bridge |
| `internal/config/config.go` | Lua knobs |
| `internal/doctor/doctor.go` | report Lua state + tools |
| `internal/repl/complete.go` | complete `:lua` commands |
| docs/README | behavior, security caveats, examples |

## Configuration

| Env var | Default | Meaning |
|---|---|---|
| `AISH_LUA` | `1` | enable model-authored tools (`0` disables) |
| `AISH_LUA_DIR` | `~/.config/aish/lua` | on-disk tools |
| `AISH_LUA_TIMEOUT` | `30s` | per tool call |
| `AISH_LUA_MAX_TOOLS` | `8` | session tool cap |
| `AISH_LUA_MAX_BYTES` | `65536` | max tool source size |

## Phases

1. Runtime + validation + ephemeral model tools + `propose_lua` (done).
2. Persistence, `:lua` management, hot reload, stable `run_lua` (done).
3. `:<name>` commands and command completion (not in v0.3.0; tools are
   sufficient for the model-authored flow).
4. Hardening: fuzzing, memory notes, optional child-process sandbox (partial).

## Security notes

- Lua scripts are code. A tool can call `aish.exec`, so it can do anything the
  user can, subject to the safety gate. The gate is not a sandbox.
- Model-generated tools are ephemeral by default and never auto-saved.
- `propose_lua` is session-only. `run_lua` is the stable tool for using a
  tool; a small model does not need to discover a newly added `lua_<name>`
  tool to run it.
- `aish.exec` streams output and is audited, so a tool cannot silently read
  secrets and feed them back to the model without the user seeing the command.
- A malicious tool can still burn CPU until the timeout or consume memory.
  Do not run aish with untrusted local users.
