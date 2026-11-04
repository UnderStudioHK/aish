package repl

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/UnderStudioHK/aish/internal/execx"
	"github.com/UnderStudioHK/aish/internal/llm"
	"github.com/UnderStudioHK/aish/internal/luatools"
)

// ---- Lua tools: model tool bridge ----

func (r *Repl) proposeLua(tc llm.ToolCall) {
	if r.luaTools == nil {
		r.appendToolError(tc, "Lua tools are disabled (AISH_LUA=0)")
		return
	}
	var args struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
		Code        string         `json:"code"`
		Args        map[string]any `json:"args"`
		Persist     bool           `json:"persist"`
	}
	if err := llm.DecodeArgs(tc.Func.Arguments, &args); err != nil {
		r.appendToolError(tc, "unreadable propose_lua arguments: "+err.Error())
		return
	}
	info, err := r.luaTools.Propose(args.Name, args.Description, args.Code, args.Parameters)
	if err != nil {
		r.appendToolError(tc, "could not register Lua tool: "+err.Error())
		return
	}

	fmt.Printf("%s⚙ lua tool %s registered (session)%s\n", cDim, info.Name, cNorm)
	msg := fmt.Sprintf("registered Lua tool %s for this session; use run_lua with {\"name\":%q,\"args\":{...}}.", info.Name, info.Name)
	if len(args.Args) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), r.cfg.LuaTimeout)
		out, err := r.luaTools.Call(ctx, info.Tool, args.Args)
		cancel()
		if err != nil {
			msg += "\nimmediate run failed: " + err.Error()
		} else {
			if len(out) > 64*1024 {
				out = out[:64*1024] + "\n… (truncated)"
			}
			msg += "\nimmediate result:\n" + out
		}
	}
	if args.Persist {
		msg += fmt.Sprintf(" The user can persist it with `:lua save %s`.", info.Name)
		fmt.Printf("%s  save with :lua save %s%s\n", cDim, info.Name, cNorm)
	}
	r.mu.Lock()
	r.appendTalkLocked(llm.Message{Role: "tool", ToolCallID: tc.ID, Content: msg})
	r.mu.Unlock()
}

func (r *Repl) runLua(tc llm.ToolCall) {
	if r.luaTools == nil {
		r.appendToolError(tc, "Lua tools are disabled (AISH_LUA=0)")
		return
	}
	var args struct {
		Name string         `json:"name"`
		Args map[string]any `json:"args"`
	}
	if err := llm.DecodeArgs(tc.Func.Arguments, &args); err != nil {
		r.appendToolError(tc, "unreadable run_lua arguments: "+err.Error())
		return
	}
	if args.Name == "" {
		r.appendToolError(tc, "run_lua requires a Lua tool name")
		return
	}
	tool := args.Name
	if !strings.HasPrefix(tool, luatools.ToolPrefix) {
		tool = luatools.ToolPrefix + tool
	}
	fmt.Printf("%s⚙ run_lua %s%s\n", cDim, args.Name, cNorm)

	ctx, cancel := context.WithTimeout(context.Background(), r.cfg.LuaTimeout)
	defer cancel()
	out, err := r.luaTools.Call(ctx, tool, args.Args)
	if err != nil {
		out = "error: " + err.Error()
	}
	if len(out) > 64*1024 {
		out = out[:64*1024] + "\n… (truncated)"
	}
	r.mu.Lock()
	r.appendTalkLocked(llm.Message{Role: "tool", ToolCallID: tc.ID, Content: out})
	r.mu.Unlock()
}

func (r *Repl) callLua(tc llm.ToolCall) {
	if r.luaTools == nil {
		r.appendToolError(tc, "Lua tools are disabled (AISH_LUA=0)")
		return
	}
	var args map[string]any
	if len(tc.Func.Arguments) > 0 && string(tc.Func.Arguments) != "null" {
		if err := llm.DecodeArgs(tc.Func.Arguments, &args); err != nil {
			r.appendToolError(tc, "unreadable tool arguments: "+err.Error())
			return
		}
	}
	fmt.Printf("%s⚙ %s%s\n", cDim, tc.Func.Name, cNorm)

	ctx, cancel := context.WithTimeout(context.Background(), r.cfg.LuaTimeout)
	defer cancel()
	out, err := r.luaTools.Call(ctx, tc.Func.Name, args)
	if err != nil {
		out = "error: " + err.Error()
	}
	if len(out) > 64*1024 {
		out = out[:64*1024] + "\n… (truncated)"
	}
	r.mu.Lock()
	r.appendTalkLocked(llm.Message{Role: "tool", ToolCallID: tc.ID, Content: out})
	r.mu.Unlock()
}

func (r *Repl) appendToolError(tc llm.ToolCall, msg string) {
	fmt.Fprintf(os.Stderr, "%saish: %s%s\n", cDim, msg, cNorm)
	r.mu.Lock()
	r.appendTalkLocked(llm.Message{Role: "tool", ToolCallID: tc.ID, Content: "error: " + msg})
	r.mu.Unlock()
}

// ---- Lua tools: host bridge ----

// Exec implements luatools.Host. It shows, classifies and audits the command
// exactly like a run_command proposal.
func (r *Repl) Exec(ctx context.Context, argv []string) (luatools.ExecResult, error) {
	if len(argv) == 0 {
		return luatools.ExecResult{Code: -1, Stderr: "empty argv"}, fmt.Errorf("empty argv")
	}
	cmd := execx.ShellJoin(argv)
	lv := r.policy.Classify(cmd)
	r.scopeWarn(cmd)
	fmt.Printf("%s%s%s%s\n", colorFor(lv), cBold, cmd, cNorm)
	if !r.mayRun(cmd, lv) {
		return luatools.ExecResult{Code: -1, Stderr: "not run: user declined"}, nil
	}

	cctx, cancel := context.WithTimeout(ctx, r.cfg.CmdTimeout)
	out, errOut, code, runErr := r.dir.RunArgvTee(cctx, argv)
	cancel()
	if runErr != nil && errOut == "" {
		errOut = runErr.Error()
	}
	r.audit(cmd, code)
	r.lastCode = code
	r.lastShown = code
	r.lastEmpty = strings.TrimSpace(out+errOut) == ""
	if code != 0 {
		fmt.Fprintf(os.Stderr, "%s✗ exit %d%s\n", cDim, code, cNorm)
	} else if strings.TrimSpace(out+errOut) == "" {
		fmt.Printf("%s(no output, exit 0)%s\n", cDim, cNorm)
	}
	return luatools.ExecResult{Code: code, Stdout: out, Stderr: errOut}, nil
}

func (r *Repl) Print(text string) { fmt.Fprintln(os.Stdout, text) }

func (r *Repl) Log(text string) {
	fmt.Fprintf(os.Stderr, "%s[lua] %s%s\n", cDim, text, cNorm)
}

func (r *Repl) Cwd() string {
	wd, _ := os.Getwd()
	return wd
}

func (r *Repl) Env(name string) string { return os.Getenv(name) }

// ---- hot reload ----

func (r *Repl) reloadLuaTools() {
	if r.luaTools == nil {
		return
	}
	errs := r.luaTools.LoadDir()
	msg := ""
	for _, err := range errs {
		msg += err.Error() + "\n"
	}
	if msg != "" && msg != r.lastLuaToolErr {
		fmt.Fprintf(os.Stderr, "%saish: lua tools: %s%s\n", cDim,
			strings.TrimSpace(msg), cNorm)
	}
	r.lastLuaToolErr = msg
}

// ---- :lua management ----

func (r *Repl) dispatchLuaCommand(line string) int {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return 0
	}
	if r.luaTools == nil {
		fmt.Fprintf(os.Stderr, "%saish: Lua tools are disabled (AISH_LUA=0)%s\n", cDim, cNorm)
		return 1
	}

	switch fields[0] {
	case ":lua":
		if len(fields) < 2 {
			return r.listLuaTools()
		}
		switch fields[1] {
		case "list":
			return r.listLuaTools()
		case "show":
			if len(fields) < 3 {
				r.luaUsage()
				return 1
			}
			return r.showLuaTool(fields[2])
		case "save":
			if len(fields) < 3 {
				r.luaUsage()
				return 1
			}
			return r.saveLuaTool(fields[2])
		case "rm", "remove":
			if len(fields) < 3 {
				r.luaUsage()
				return 1
			}
			return r.removeLuaTool(fields[2])
		case "reload":
			return r.reloadLuaToolsCommand()
		default:
			r.luaUsage()
			return 1
		}
	default:
		fmt.Fprintf(os.Stderr, "%saish: unknown command %s (try :lua)%s\n", cDim, fields[0], cNorm)
		return 1
	}
}

func (r *Repl) luaUsage() {
	fmt.Println("usage:")
	fmt.Println("  :lua                    list session and saved Lua tools")
	fmt.Println("  :lua show <name>         show metadata and source")
	fmt.Println("  :lua save <name>         save a session tool")
	fmt.Println("  :lua rm <name>           remove a saved tool")
	fmt.Println("  :lua reload             re-read the Lua tools directory")
}

func (r *Repl) listLuaTools() int {
	infos := r.luaTools.List()
	if len(infos) == 0 {
		fmt.Printf("%sno Lua tools loaded (dir: %s)%s\n", cDim, r.luaTools.Dir(), cNorm)
		return 0
	}
	fmt.Printf("%-20s %-26s %-8s %s\n", "NAME", "TOOL", "SCOPE", "DESCRIPTION")
	for _, in := range infos {
		scope := "session"
		if in.Persistent {
			scope = "saved"
		}
		desc := in.Description
		if in.Error != "" {
			desc = "ERROR: " + in.Error
		}
		fmt.Printf("%-20s %-26s %-8s %s\n", in.Name, in.Tool, scope, desc)
	}
	return 0
}

func (r *Repl) showLuaTool(name string) int {
	src, ok := r.luaTools.Source(name)
	if !ok {
		fmt.Fprintf(os.Stderr, "%saish: unknown Lua tool %q%s\n", cDim, name, cNorm)
		return 1
	}
	for _, in := range r.luaTools.List() {
		if in.Name != name {
			continue
		}
		fmt.Printf("name:        %s\n", in.Name)
		fmt.Printf("tool:        %s\n", in.Tool)
		fmt.Printf("description: %s\n", in.Description)
		fmt.Printf("scope:       %s\n", map[bool]string{true: "saved", false: "session"}[in.Persistent])
		if in.Path != "" {
			fmt.Printf("path:        %s\n", in.Path)
		}
		if in.Error != "" {
			fmt.Printf("error:       %s\n", in.Error)
		}
	}
	fmt.Println()
	fmt.Println(src)
	return 0
}

func (r *Repl) saveLuaTool(name string) int {
	path, err := r.luaTools.Save(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%saish: %v%s\n", cDim, err, cNorm)
		return 1
	}
	fmt.Printf("%ssaved %s to %s%s\n", cDim, name, path, cNorm)
	return 0
}

func (r *Repl) removeLuaTool(name string) int {
	if err := r.luaTools.Remove(name); err != nil {
		fmt.Fprintf(os.Stderr, "%saish: %v%s\n", cDim, err, cNorm)
		return 1
	}
	fmt.Printf("%sremoved %s%s\n", cDim, name, cNorm)
	return 0
}

func (r *Repl) reloadLuaToolsCommand() int {
	errs := r.luaTools.LoadDir()
	if len(errs) == 0 {
		fmt.Printf("%sLua tools reloaded from %s%s\n", cDim, r.luaTools.Dir(), cNorm)
		return 0
	}
	for _, err := range errs {
		fmt.Fprintf(os.Stderr, "%saish: %v%s\n", cDim, err, cNorm)
	}
	return 1
}
