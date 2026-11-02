// Package repl runs the interactive aish loop.
package repl

import (
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

	"github.com/chzyer/readline"

	"aish/internal/config"
	"aish/internal/execx"
	"aish/internal/history"
	"aish/internal/llm"
	"aish/internal/prompt"
	"aish/internal/safety"
	"aish/internal/servermgmt"
	"golang.org/x/term"
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
	srvErr  error

	// agent state (r.mu)
	talk     []llm.Message // full conversation incl. tool exchanges
	steps    int
	epoch    uint64
	inFlight bool
	cancel   context.CancelFunc
	pending  []llm.ToolCall

	replay     map[string][]string // request -> commands run for it
	replayCmds []string            // commands run in the current request
	currentReq string
	lastCmds   []string // last executed batch (duplicate guard)
	lastOK     bool     // last batch fully succeeded
	lastEmpty  bool     // last command produced no output

	// typo assistance cache
	cmdOnce  sync.Once
	cmdNames []string
	dict     map[string]bool

	// ui state
	mu           sync.Mutex // guards the fields above
	llmMu        sync.Mutex // single-flight model calls
	uiMu         sync.Mutex // guards prompt-active printing
	promptActive bool
	rl           *readline.Instance

	confirmOnce sync.Once
	confirmRL   *readline.Instance
	confirmErr  error
}

func New(cfg *config.Config, dir *execx.Director, h *history.Store,
	c *llm.Client, s *servermgmt.Server) *Repl {
	return &Repl{cfg: cfg, dir: dir, hist: h, llm: c, srv: s,
		policy: safety.NewPolicy(cfg.Confirm), dict: map[string]bool{},
		replay: map[string][]string{}}
}

// promptString renders the interactive prompt from live state.
func (r *Repl) promptString() string {
	wd, _ := os.Getwd()
	home, _ := os.UserHomeDir()
	user := os.Getenv("USER")
	host, _ := os.Hostname()

	r.mu.Lock()
	pending := len(r.pending) > 0
	busy := r.inFlight
	r.mu.Unlock()

	git := prompt.GitFor(wd)
	color := os.Getenv("NO_COLOR") == "" && term.IsTerminal(int(os.Stdout.Fd()))

	var tmpl string
	if r.cfg.PromptTmpl != "" {
		if b, err := os.ReadFile(r.cfg.PromptTmpl); err == nil {
			tmpl = strings.TrimRight(string(b), "\n")
		}
	}

	return prompt.Render(prompt.State{
		Cwd:      wd,
		Home:     home,
		Exit:     r.lastShown,
		Pending:  pending,
		Busy:     busy,
		Root:     os.Geteuid() == 0,
		Color:    color,
		Branch:   git.Branch,
		Detached: git.Detached,
		Dirty:    git.Dirty,
		Style:    r.cfg.PromptStyle,
		Template: tmpl,
		User:     user,
		Host:     host,
	})
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
	add("!")
	add("??")
	add("export")
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
		go func() { _ = r.ensureLLM() }()
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
	r.rl = rl
	defer func() {
		if r.confirmRL != nil {
			_ = r.confirmRL.Close()
		}
		_ = rl.Close()
	}()

	for {
		rl.SetPrompt(r.promptString())
		r.uiMu.Lock()
		r.promptActive = true
		r.uiMu.Unlock()
		line, err := rl.Readline()
		r.uiMu.Lock()
		r.promptActive = false
		r.uiMu.Unlock()
		switch {
		case err == nil:
		case errors.Is(err, readline.ErrEsc):
			// Esc: dismiss any pending follow-up without running anything.
			if r.hasPending() {
				fmt.Printf("%sdismissed%s\n", cDim, cNorm)
			}
			r.beginNewInput()
			continue
		case errors.Is(err, readline.ErrInterrupt):
			r.beginNewInput()
			continue
		case errors.Is(err, io.EOF):
			fmt.Println()
			return r.lastCode, nil
		default:
			return 1, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			// Empty Enter: accept a queued follow-up, else "continue" in lazy mode.
			if r.hasPending() {
				r.acceptPending()
				continue
			}
			if r.cfg.FollowUp == "lazy" && r.cfg.Enabled() {
				r.continueSync()
			}
			continue
		}
		// Any real input wins over queued/background work.
		r.beginNewInput()
		// history persistence is owned by readline's HistoryFile (plain lines);
		// Store is the shared reader for the history builtin + model context.

		switch {
		case line == "exit" || line == "quit":
			r.beginNewInput()
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
	return r.handleIntent(line)
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
	// Some binaries are also common English verbs: "install figlet" or
	// "make the file readable" is intent, while "install -m 644 x y" or
	// "make all" is shell. Route to the model once enough free words pile
	// up for these.
	if threshold, ok := ambiguousVerbs[w0]; ok && freeWords >= threshold {
		return false
	}
	return freeWords < 3
}

// ambiguousVerbs maps verb-like binaries to how many free-form words turn
// the line into natural language.
var ambiguousVerbs = map[string]int{
	"install": 1,
	"test":    1,
	"yes":     1,
	"make":    2,
}

func looksShellToken(w string) bool {
	if len(w) == 1 || isAllDigits(w) {
		return true
	}
	return strings.HasPrefix(w, "-") || strings.ContainsAny(w, "~/.$*{}=\"(")
}

func isAllDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

// contextLine tells the model exactly where and on what it is; appended per
// request so the static prompt prefix stays cacheable.
func (r *Repl) contextLine() string {
	wd, _ := os.Getwd()
	home, _ := os.UserHomeDir()
	osName := "unknown"
	if b, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
				osName = strings.Trim(v, `"`)
			}
		}
	}
	pm := "unknown"
	switch {
	case exists("/usr/bin/dnf") || exists("/bin/dnf"):
		pm = "dnf (Fedora/RHEL)"
	case exists("/usr/bin/apt"):
		pm = "apt (Debian/Ubuntu)"
	case exists("/usr/bin/pacman"):
		pm = "pacman (Arch)"
	}
	python := "python3"
	if exists("/usr/bin/python3.14") {
		python = "python3.14"
	}
	extra := ""
	if exists("/usr/bin/pipx") {
		extra += " pipx available for Python CLI tools."
	}
	if exists("/usr/bin/uv") {
		extra += " uv available."
	}
	return fmt.Sprintf(
		"Search scope: the current working directory unless the user names another path.\n"+
			"Current working directory: %s\nHome directory: %s\nOS: %s\n"+
			"System package manager: %s. Use it for system packages; never "+
			"use pip for system packages (use pipx/uv only for Python CLI tools).\n"+
			"Python: %s.%s\nShell: /bin/sh. `cd` and `export` are applied "+
			"in-process and persist across commands. Prefer `--help`/`man` "+
			"over guessing when unsure about a program's flags.",
		wd, home, osName, pm, python, extra)
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// askOnly sends a question with no tools; the answer is printed in yellow.
func (r *Repl) askOnly(q string) {
	if !r.readyLLM() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.cfg.LLMTimeout)
	defer cancel()
	msg, err := r.llmTurn(ctx, []llm.Message{{Role: "user", Content: q}}, false, "")
	if err != nil {
		fmt.Printf("%saish: %v%s\n", cRed, err, cNorm)
		return
	}
	fmt.Printf("%s%s%s\n", cYell, llm.Sanitize(msg.Content), cNorm)
}

// ensureLLM starts/checks the model server exactly once per process.
func (r *Repl) ensureLLM() error {
	r.srvOnce.Do(func() {
		r.srvStop, r.srvErr = r.srv.EnsureUp(context.Background(), r.llm)
	})
	return r.srvErr
}

// readyLLM makes sure the server is up (blocking, first use only).
func (r *Repl) readyLLM() bool {
	if err := r.ensureLLM(); err != nil {
		fmt.Printf("%saish: %v%s\n", cRed, err, cNorm)
		return false
	}
	return true
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

// ask reads a confirmation through readline itself. This keeps a single
// owner of the terminal (no stray newlines or raw/cooked mismatches between
// separate stdin readers) and gives proper line editing/echo.
func (r *Repl) ask(cmd string, lv safety.Level) (string, bool) {
	r.confirmOnce.Do(func() {
		r.confirmRL, r.confirmErr = readline.NewEx(&readline.Config{
			Prompt:          "",
			InterruptPrompt: "^C",
			HistoryLimit:    0,
		})
	})
	if r.confirmErr != nil || r.confirmRL == nil {
		return "", false
	}
	hint := "[y/N]"
	if lv == safety.Never {
		hint = "type 'yes'"
	}
	r.confirmRL.SetPrompt(fmt.Sprintf("  run (%s, n=cancel, e=edit)? ", hint))
	ans, err := r.confirmRL.Readline()
	if err != nil {
		// Ctrl-C / Esc / EOF all mean "do not run".
		return "", false
	}
	switch strings.ToLower(strings.TrimSpace(ans)) {
	case "y":
		if lv == safety.Never {
			return "", false
		}
		return cmd, true
	case "yes":
		return cmd, true
	case "e", "edit":
		return safety.Edited(cmd)
	}
	return "", false
}

// mayRun gates a proposed command through policy + interactive confirm.
func (r *Repl) mayRun(cmd string, lv safety.Level) bool {
	switch {
	case lv != safety.Safe:
		_, ok := r.ask(cmd, lv)
		return ok
	case r.policy.Mode == "always":
		_, ok := r.ask(cmd, lv)
		return ok
	}
	return true // safe + safe-auto/never
}
