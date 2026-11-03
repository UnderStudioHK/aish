# Model-authored Lua skills — plan

Status: implemented (v0.2.0).

This document records the design for optional, model-authored Lua skills in
aish. The short version: the model can define a small Lua tool for the current
session, use it immediately, and suggest saving it; only the user can persist
it. Skills are namespaced (`lua_<name>` tools plus stable `run_lua`), never
shadow builtins or `PATH`, and hot-reload from disk.

## Goals

- Let the model extend its own toolset at runtime without restarting aish.
- Let the user save a useful generated skill for future sessions.
- Keep extension names from shadowing builtins, `PATH` commands, or aish tools.
- Reload edited skill files without restarting.
- Keep the sandbox minimal but meaningful: narrow host API, no raw OS access,
  timeouts, output caps, gated command execution.

Non-goals for this version:

- Full process isolation / seccomp / WASM sandbox.
- Automatic persistence of model-generated code.
- Allowing skills to replace or modify the safety classifier.
- User-authored plugin marketplace / registry.

## Model flow

```text
request
  │
  ├─ model calls propose_lua {name, description, parameters, code, args?, persist?}
  │     ├─ validate name, schema, size and collisions
  │     ├─ compile the Lua function without running it
  │     └─ register the skill for this session
  │
  ├─ model uses the skill in one of two ways:
  │     ├─ pass args in the same propose_lua call (immediate result), or
  │     └─ call run_lua {name, args} (stable tool; no dynamic tool needed)
  │     └─ lua_<name>(args) also works for models that support the added tool
  │           └─ aish.exec(argv) → classifier → confirm → audit → output
  │
  └─ persist=true
        └─ show a suggestion; user runs `:skill save <name>` to keep it
```

The tool list is rebuilt per model turn, so a newly registered skill is
available on the next turn.

## Skill format

On-disk skills live in `$XDG_CONFIG_HOME/aish/skills/*.lua` (default
`~/.config/aish/skills/`).

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
model-authored skills, `propose_lua` carries the same metadata plus the
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
- `aish.shell` is not exposed to model-authored skills in this version.

## Namespacing / no shadowing

- Built-in tools stay `run_command`, `find_package`.
- New built-in: `propose_lua`.
- Model skills are exposed only as `lua_<name>`.
- Skills are tools only in this version: `lua_<name>` and `run_lua`; bare
  names never resolve to a skill. `:<name>` commands are deliberately future
  work.
- Name collisions with an existing tool or skill are rejected.

## Sandbox and limits

- Pure-Go `github.com/yuin/gopher-lua` per skill state.
- Only `base`, `table`, `string`, `math` libraries; dangerous base functions
  removed.
- Per-call timeout (`AISH_LUA_TIMEOUT`, default `30s`) via `SetContext`.
- Max source size (`AISH_LUA_MAX_BYTES`, default 64 KiB).
- Max session skills (`AISH_LUA_MAX_SKILLS`, default 8).
- Tool result and command output caps.
- Panic recovery; per-skill mutex because Lua states are not concurrency-safe.
- In-process Lua is **not** a security boundary; this is API minimization.

## Hot reload

- Load `*.lua` at startup.
- When a command is submitted, stat the skills directory and reload changed
  files.
- Reload into a fresh Lua state, validate, then swap atomically.
- On compile/validation failure, keep the previous version and print an error.
- `:skill reload` forces a reload.
- Ephemeral skills are memory-only; `:skill save <name>` writes them out and
  triggers the reload path.

## Management commands

```text
:skills                 list session + on-disk skills
:skill show <name>      print metadata and source
:skill save <name>      persist an ephemeral skill
:skill rm <name>        remove an on-disk skill
:skill reload           re-read the skills directory
```

## Integration points

| Area | Change |
|---|---|
| `internal/skills/` | Lua runtime, validation, registry, hot reload, host API |
| `internal/llm/llm.go` | Dynamic tool list; `propose_lua` schema |
| `internal/repl/agent.go` | Dispatch `propose_lua` and `lua_*` tools |
| `internal/repl/repl.go` | Manager lifecycle, `:skill` commands, hot reload |
| `internal/execx/exec.go` | argv-based tee execution + shell-display quoting |
| `internal/safety/` | unchanged classifier, reused by the exec bridge |
| `internal/config/config.go` | Lua knobs |
| `internal/doctor/doctor.go` | report Lua state + skills |
| `internal/repl/complete.go` | complete `:skill` commands |
| docs/README | behavior, security caveats, examples |

## Configuration

| Env var | Default | Meaning |
|---|---|---|
| `AISH_LUA` | `1` | enable model-authored skills (`0` disables) |
| `AISH_SKILLS_DIR` | `~/.config/aish/skills` | on-disk skills |
| `AISH_LUA_TIMEOUT` | `30s` | per skill/tool call |
| `AISH_LUA_MAX_SKILLS` | `8` | session skill cap |
| `AISH_LUA_MAX_BYTES` | `65536` | max skill source size |

## Phases

1. Runtime + validation + ephemeral model skills + `propose_lua` (done).
2. Persistence, `:skill` management, hot reload, stable `run_lua` (done).
3. `:<name>` commands and command completion (not in v0.2.0; tools are
   sufficient for the model-authored flow).
4. Hardening: fuzzing, memory notes, optional child-process sandbox (partial).

## Security notes

- Lua scripts are code. A skill can call `aish.exec`, so it can do anything the
  user can, subject to the safety gate. The gate is not a sandbox.
- Model-generated skills are ephemeral by default and never auto-saved.
- `propose_lua` is session-only. `run_lua` is the stable tool for using a
  skill; a small model does not need to discover a newly added `lua_<name>`
  tool to run it.
- `aish.exec` streams output and is audited, so a skill cannot silently read
  secrets and feed them back to the model without the user seeing the command.
- A malicious skill can still burn CPU until the timeout or consume memory.
  Do not run aish with untrusted local users.
