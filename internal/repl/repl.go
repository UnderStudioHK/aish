// Package repl runs the interactive aish loop.
package repl

import (
	"encoding/json"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chzyer/readline"

	"aish/internal/config"
	"aish/internal/execx"
	"aish/internal/history"
	"aish/internal/llm"
	"aish/internal/safety"
	"aish/internal/servermgmt"
)

const (
	cCyan = "\x1b[36m"
	cYell = "\x1b[33m"
	cRed  = "\x1b[31m"
	cDim  = "\x1b[2m"
	cBold = "\x1b[1m"
	cNorm = "\x1b[0m"
)

// Repl holds the interactive shell.
type Repl struct {
	cfg    *config.Config
	dir    *execx.Director
	hist   *history.Store
	llm    *llm.Client
	srv    *servermgmt.Server
	policy safety.Policy

	lastCode  int
	lastShown int

	srvOnce sync.Once
	srvStop func()

	talk []llm.Message // session transcript (user/assistant pairs)
}

func New(cfg *config.Config, dir *execx.Director, h *history.Store,
	c *llm.Client, s *servermgmt.Server) *Repl {
	return &Repl{cfg: cfg, dir: dir, hist: h, llm: c, srv: s, policy: safety.NewPolicy(cfg.Confirm)}
}

func shortCwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "?"
	}
	if home, _ := os.UserHomeDir(); wd == home {
		return "~"
	}
	return filepath.Base(wd)
}

func (r *Repl) promptString() string {
	if r.lastShown == 0 {
		return fmt.Sprintf("%s%s (aish)%s ", cCyan, shortCwd(), cNorm)
	}
	color := cDim
	if r.lastShown < 0 {
		color = cRed
	}
	return fmt.Sprintf("%s%s (aish)%s %s[%d]%s ", cCyan, shortCwd(), cNorm, color, r.lastShown, cNorm)
}

func (r *Repl) completer() *readline.PrefixCompleter {
	items := make([]readline.PrefixCompleterInterface, 0, 128)
	seen := map[string]bool{}
	add := func(w string) {
		if !seen[w] {
			items = append(items, readline.PcItem(w))
			seen[w] = true
		}
	}
	add("!"); add("??"); add("export")
	for _, b := range []string{"cd", "pwd", "export", "exit", "help", "history", "env"} {
		add(b)
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			add(e.Name())
		}
	}
	return readline.NewPrefixCompleter(items...)
}

// Run is the main loop. Returns the shell exit code.
func (r *Repl) Run() (int, error) {
	if err := os.MkdirAll(r.cfg.StateDir, 0o755); err != nil {
		return 1, err
	}

	// Swallow SIGINT/SIGTERM: tty Ctrl-C goes to the whole process group,
	// which is how running children get interrupted — aish must survive it.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for range sig {
		}
	}()

	// Warm the model server in the background so first intent line is fast.
	if r.cfg.Enabled() {
		go func() {
			stop, _ := r.srv.EnsureUp(context.Background(), r.llm)
			r.srvOnce.Do(func() { r.srvStop = stop })
		}()
	}

	rl, err := readline.NewEx(&readline.Config{
		Prompt:          r.promptString(),
		HistoryFile:     r.cfg.HistoryPath,
		HistoryLimit:    10000,
		AutoComplete:    r.completer(),
		InterruptPrompt: cYell + "^C" + cNorm,
	})
	if err != nil {
		return 1, err
	}
	defer rl.Close()

	for {
		rl.SetPrompt(r.promptString())
		line, err := rl.Readline()
		switch {
		case err == nil:
		case errors.Is(err, readline.ErrInterrupt):
			continue
		case errors.Is(err, io.EOF):
			fmt.Println()
			return r.lastCode, nil
		default:
			return 1, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// history persistence is owned by readline's HistoryFile (plain lines);
		// Store is the shared reader for the history builtin + model context.

		switch {
		case line == "exit" || line == "quit":
			return r.lastCode, nil
		case line == "!!":
			if r.dir.LastLine == "" {
				fmt.Println("no previous command")
				continue
			}
			r.lastCode = r.dir.RunNative(r.dir.LastLine)
			r.lastShown = r.lastCode
		case strings.HasPrefix(line, "!"):
			if raw := strings.TrimSpace(line[1:]); raw != "" {
				r.lastCode = r.dir.RunNative(raw)
				r.lastShown = r.lastCode
			}
		case strings.HasPrefix(line, "??"):
			if q := strings.TrimSpace(strings.TrimPrefix(line, "??")); q != "" {
				r.askOnly(q)
			}
		default:
			var handled, wantExit bool
			handled, wantExit = r.dir.Builtin(line, execx.Hooks{
				Printer: func(f string, a ...any) { fmt.Printf(f+"\n", a...) },
				Hist:    func() []string { return r.hist.Last(50) },
			})
			if wantExit {
				return r.lastCode, nil
			}
			if handled {
				continue
			}
			r.lastCode = r.dispatch(line)
			r.lastShown = r.lastCode
		}
	}
}

// dispatch routes one line: native fast path or the model's intent loop.
func (r *Repl) dispatch(line string) int {
	if fastNative(line) {
		return r.dir.RunNative(line)
	}
	if !r.cfg.Enabled() {
		return r.dir.RunNative(line)
	}
	return r.intentLoop(line)
}

// fastNative decides whether a line is unambiguous plain shell.
func fastNative(line string) bool {
	if strings.ContainsAny(line, "|;&>") {
		return true // shell plumbing: cheap to run as-is
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return true
	}
	w0 := fields[0]
	if execx.IsBuiltin(w0) {
		return true
	}
	if _, err := exec.LookPath(w0); err != nil {
		return false // unknown first word → model may resolve intent
	}
	if len(fields) == 1 {
		return true // single token that IS a real command
	}
	// Known binary with a tail: if every tail token looks like a shell
	// argument (flags, paths, numbers, globs) it's native; a pile of
	// free-form English words is intent.
	freeWords := 0
	for _, w := range fields[1:] {
		if looksShellToken(w) {
			continue
		}
		freeWords++
	}
	return freeWords < 3
}

func looksShellToken(w string) bool {
	if len(w) == 1 {
		return true
	}
	return strings.HasPrefix(w, "-") || strings.ContainsAny(w, "~/.$*{}=\"(")
}

// askOnly sends a question with no tools; the answer is printed in yellow.
func (r *Repl) askOnly(q string) {
	if !r.readyLLM() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.cfg.LLMTimeout)
	defer cancel()
	msg, err := r.llm.Turn(ctx, []llm.Message{{Role: "user", Content: q}}, false)
	if err != nil {
		fmt.Printf("%saish: %v%s\n", cRed, err, cNorm)
		return
	}
	fmt.Printf("%s%s%s\n", cYell, llm.Sanitize(msg.Content), cNorm)
}

// readyLLM makes sure the server is up (blocking, first use only).
func (r *Repl) readyLLM() bool {
	var err error
	r.srvOnce.Do(func() {
		r.srvStop, err = r.srv.EnsureUp(context.Background(), r.llm)
	})
	if err != nil {
		fmt.Printf("%saish: %v%s\n", cRed, err, cNorm)
		return false
	}
	return true
}

// intentLoop drives the run_command tool loop; returns last exit code.
func (r *Repl) intentLoop(request string) int {
	if !r.readyLLM() {
		return r.dir.RunNative(request)
	}

	var msgs []llm.Message
	// Prior turns as real user/assistant pairs (bounded by AISH_HISTORY_TURNS).
	msgs = append(msgs, r.talk...)
	msgs = append(msgs, llm.Message{Role: "user", Content: request})

	start := time.Now()
	var lastContent string
	debugDump := os.Getenv("AISH_DEBUG") != ""
	dump := func(stage string, msgs []llm.Message) {
		if !debugDump {
			return
		}
		b, _ := json.MarshalIndent(msgs, "", " ")
		_ = os.WriteFile("/tmp/opencode/aish_dbg_"+stage+".json", b, 0o644)
	}
	defer func() {
		fmt.Fprintf(os.Stderr, "%s[%s model time]%s\n", cDim, time.Since(start).Round(100*time.Millisecond), cNorm)
	}()

	for step := 0; step < r.cfg.MaxSteps; step++ {
		dump(fmt.Sprintf("req_%d", step), msgs)
		tctx, cancel := context.WithTimeout(context.Background(), r.cfg.LLMTimeout)
		asMsg, err := r.llm.Turn(tctx, msgs, true)
		cancel()
		if err != nil {
			fmt.Printf("%saish: %v%s\n", cRed, err, cNorm)
			return -1
		}
		if strings.TrimSpace(asMsg.Content) != "" {
			lastContent = llm.Sanitize(asMsg.Content)
			fmt.Printf("%s%s%s\n", cYell, lastContent, cNorm)
			if len(asMsg.ToolCalls) == 0 {
				r.remember(request, lastContent)
				return r.lastCode
			}
		}
		if len(asMsg.ToolCalls) == 0 {
			r.remember(request, lastContent)
			return r.lastCode
		}
		msgs = append(msgs, asMsg)

		declined := false
		for _, tc := range asMsg.ToolCalls {
			cmd, ok := llm.CommandOf(tc)
			if !ok {
				msgs = append(msgs, llm.Message{Role: "tool", ToolCallID: tc.ID,
					Content: "error: unreadable tool arguments"})
				continue
			}
			lv := r.policy.Classify(cmd)
			fmt.Printf("%s%s%s%s\n", colorFor(lv), cBold, cmd, cNorm)
			if !r.mayRun(cmd, lv) {
				declined = true
				break
			}
			cctx, ccancel := context.WithTimeout(context.Background(), r.cfg.CmdTimeout)
			out, code, _ := r.dir.RunCaptured(cctx, cmd)
			ccancel()
			msgs = append(msgs, llm.Message{Role: "tool", ToolCallID: tc.ID,
				Content: fmt.Sprintf("exit_code=%d\n%s", code, out)})
			r.lastCode = code
			r.lastShown = code
			dump(fmt.Sprintf("post_tool_%d", step), msgs)
		}
		if declined {
			fmt.Fprintf(os.Stderr, "%saish: cancelled%s\n", cDim, cNorm)
			if lastContent != "" {
				r.remember(request, lastContent)
			}
			return r.lastCode
		}
	}
	fmt.Fprintf(os.Stderr, "%saish: stopped after %d steps%s\n", cDim, r.cfg.MaxSteps, cNorm)
	if lastContent != "" {
		r.remember(request, lastContent)
	}
	return r.lastCode
}

// remember stores one completed user exchange for next-turn context.
func (r *Repl) remember(request string, final string) {
	r.talk = append(r.talk,
		llm.Message{Role: "user", Content: request},
		llm.Message{Role: "assistant", Content: final})
	// keep at most HistoryTurns pairs
	max := 2 * r.cfg.HistoryTurns
	if len(r.talk) > max {
		r.talk = r.talk[len(r.talk)-max:]
	}
}

func colorFor(lv safety.Level) string {
	switch lv {
	case safety.Never:
		return cRed
	case safety.Confirm:
		return cYell
	}
	return ""
}

// mayRun gates a proposed command through policy + interactive confirm.
func (r *Repl) mayRun(cmd string, lv safety.Level) bool {
	switch {
	case lv != safety.Safe:
		_, ok := safety.Ask(cmd, lv)
		return ok
	case r.policy.Mode == "always":
		_, ok := safety.Ask(cmd, lv)
		return ok
	}
	return true // safe + safe-auto/never
}
