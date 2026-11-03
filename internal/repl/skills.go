package repl

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/UnderStudioHK/aish/internal/execx"
	"github.com/UnderStudioHK/aish/internal/llm"
	"github.com/UnderStudioHK/aish/internal/skills"
)

// ---- Lua skills: model tool bridge ----

func (r *Repl) proposeLua(tc llm.ToolCall) {
	if r.skills == nil {
		r.appendToolError(tc, "Lua skills are disabled (AISH_LUA=0)")
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
	info, err := r.skills.Propose(args.Name, args.Description, args.Code, args.Parameters)
	if err != nil {
		r.appendToolError(tc, "could not register Lua skill: "+err.Error())
		return
	}

	fmt.Printf("%s⚙ lua skill %s registered (session)%s\n", cDim, info.Name, cNorm)
	msg := fmt.Sprintf("registered Lua skill %s for this session; use run_lua with {\"name\":%q,\"args\":{...}}.", info.Name, info.Name)
	if len(args.Args) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), r.cfg.LuaTimeout)
		out, err := r.skills.Call(ctx, info.Tool, args.Args)
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
		msg += fmt.Sprintf(" The user can persist it with `:skill save %s`.", info.Name)
		fmt.Printf("%s  save with :skill save %s%s\n", cDim, info.Name, cNorm)
	}
	r.mu.Lock()
	r.appendTalkLocked(llm.Message{Role: "tool", ToolCallID: tc.ID, Content: msg})
	r.mu.Unlock()
}

func (r *Repl) runLua(tc llm.ToolCall) {
	if r.skills == nil {
		r.appendToolError(tc, "Lua skills are disabled (AISH_LUA=0)")
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
		r.appendToolError(tc, "run_lua requires a skill name")
		return
	}
	tool := args.Name
	if !strings.HasPrefix(tool, skills.ToolPrefix) {
		tool = skills.ToolPrefix + tool
	}
	fmt.Printf("%s⚙ run_lua %s%s\n", cDim, args.Name, cNorm)

	ctx, cancel := context.WithTimeout(context.Background(), r.cfg.LuaTimeout)
	defer cancel()
	out, err := r.skills.Call(ctx, tool, args.Args)
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
	if r.skills == nil {
		r.appendToolError(tc, "Lua skills are disabled (AISH_LUA=0)")
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
	out, err := r.skills.Call(ctx, tc.Func.Name, args)
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

// ---- Lua skills: host bridge ----

// Exec implements skills.Host. It shows, classifies and audits the command
// exactly like a run_command proposal.
func (r *Repl) Exec(ctx context.Context, argv []string) (skills.ExecResult, error) {
	if len(argv) == 0 {
		return skills.ExecResult{Code: -1, Stderr: "empty argv"}, fmt.Errorf("empty argv")
	}
	cmd := execx.ShellJoin(argv)
	lv := r.policy.Classify(cmd)
	r.scopeWarn(cmd)
	fmt.Printf("%s%s%s%s\n", colorFor(lv), cBold, cmd, cNorm)
	if !r.mayRun(cmd, lv) {
		return skills.ExecResult{Code: -1, Stderr: "not run: user declined"}, nil
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
	return skills.ExecResult{Code: code, Stdout: out, Stderr: errOut}, nil
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

func (r *Repl) reloadSkills() {
	if r.skills == nil {
		return
	}
	errs := r.skills.LoadDir()
	msg := ""
	for _, err := range errs {
		msg += err.Error() + "\n"
	}
	if msg != "" && msg != r.lastSkillErr {
		fmt.Fprintf(os.Stderr, "%saish: skills: %s%s\n", cDim,
			strings.TrimSpace(msg), cNorm)
	}
	r.lastSkillErr = msg
}

// ---- :skill management ----

func (r *Repl) dispatchSkillCommand(line string) int {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return 0
	}
	if r.skills == nil {
		fmt.Fprintf(os.Stderr, "%saish: Lua skills are disabled (AISH_LUA=0)%s\n", cDim, cNorm)
		return 1
	}

	switch fields[0] {
	case ":skills":
		return r.listSkills()
	case ":skill":
		if len(fields) < 2 {
			r.skillUsage()
			return 1
		}
		switch fields[1] {
		case "list":
			return r.listSkills()
		case "show":
			if len(fields) < 3 {
				r.skillUsage()
				return 1
			}
			return r.showSkill(fields[2])
		case "save":
			if len(fields) < 3 {
				r.skillUsage()
				return 1
			}
			return r.saveSkill(fields[2])
		case "rm", "remove":
			if len(fields) < 3 {
				r.skillUsage()
				return 1
			}
			return r.removeSkill(fields[2])
		case "reload":
			return r.reloadSkillsCommand()
		default:
			r.skillUsage()
			return 1
		}
	default:
		fmt.Fprintf(os.Stderr, "%saish: unknown command %s (try :skills)%s\n", cDim, fields[0], cNorm)
		return 1
	}
}

func (r *Repl) skillUsage() {
	fmt.Println("usage:")
	fmt.Println("  :skills                 list session and saved Lua skills")
	fmt.Println("  :skill show <name>      show metadata and source")
	fmt.Println("  :skill save <name>      save a session skill")
	fmt.Println("  :skill rm <name>        remove a saved skill")
	fmt.Println("  :skill reload           re-read the skills directory")
}

func (r *Repl) listSkills() int {
	infos := r.skills.List()
	if len(infos) == 0 {
		fmt.Printf("%sno Lua skills loaded (dir: %s)%s\n", cDim, r.skills.Dir(), cNorm)
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

func (r *Repl) showSkill(name string) int {
	src, ok := r.skills.Source(name)
	if !ok {
		fmt.Fprintf(os.Stderr, "%saish: unknown skill %q%s\n", cDim, name, cNorm)
		return 1
	}
	for _, in := range r.skills.List() {
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

func (r *Repl) saveSkill(name string) int {
	path, err := r.skills.Save(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%saish: %v%s\n", cDim, err, cNorm)
		return 1
	}
	fmt.Printf("%ssaved %s to %s%s\n", cDim, name, path, cNorm)
	return 0
}

func (r *Repl) removeSkill(name string) int {
	if err := r.skills.Remove(name); err != nil {
		fmt.Fprintf(os.Stderr, "%saish: %v%s\n", cDim, err, cNorm)
		return 1
	}
	fmt.Printf("%sremoved %s%s\n", cDim, name, cNorm)
	return 0
}

func (r *Repl) reloadSkillsCommand() int {
	errs := r.skills.LoadDir()
	if len(errs) == 0 {
		fmt.Printf("%sskills reloaded from %s%s\n", cDim, r.skills.Dir(), cNorm)
		return 0
	}
	for _, err := range errs {
		fmt.Fprintf(os.Stderr, "%saish: %v%s\n", cDim, err, cNorm)
	}
	return 1
}
