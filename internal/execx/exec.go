// Package execx dispatches lines to builtins or /bin/sh, and tracks
// shell state (cwd, env) so it persists across prompts.
package execx

import (
	"fmt"
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
)

var builtins = map[string]bool{
	"cd": true, "export": true, "exit": true, "quit": true,
	"help": true, "history": true, "env": true, "pwd": true,
}

const captureLimit = 64 * 1024 // cap on output fed back to the LLM

func IsBuiltin(word string) bool { return builtins[word] }

// Hooks wire builtins to REPL services.
type Hooks struct {
	Printer func(format string, a ...any)
	Hist    func() []string
}

// Director owns shell state.
type Director struct {
	LastLine string
}

func New() *Director { return &Director{} }

// Builtin executes an aish builtin. Returns (handled, wantExit).
func (d *Director) Builtin(line string, h Hooks) (bool, bool) {
	words := strings.Fields(line)
	if len(words) == 0 {
		return false, false
	}
	print := h.Printer
	if print == nil {
		print = func(string, ...any) {}
	}
	switch words[0] {
	case "cd":
		dir := ""
		if len(words) > 1 {
			dir = os.ExpandEnv(words[1])
		}
		if dir == "" {
			dir = os.Getenv("HOME")
		}
		if err := os.Chdir(dir); err != nil {
			print("cd: %v", err)
		}
		return true, false
	case "pwd":
		wd, _ := os.Getwd()
		print(wd)
		return true, false
	case "export":
		for _, kv := range words[1:] {
			if key, val, ok := strings.Cut(kv, "="); ok {
				os.Setenv(key, val)
			}
		}
		return true, false
	case "env":
		env := exec.Command("/usr/bin/env")
		env.Stdout, env.Stderr, env.Stdin = os.Stdout, os.Stderr, os.Stdin
		_ = env.Run()
		return true, false
	case "history":
		for i, l := range h.Hist() {
			print("%3d  %s", i+1, l)
		}
		return true, false
	case "help":
		for _, l := range []string{
			"aish — AI shell",
			"",
			"  <line>        run natively if unambiguous, else ask the model",
			"  !!            re-run the last command",
			"  !<cmd>        force native execution (no model)",
			"  ??<question>  ask-only: the model answers without running anything",
			"  cd, export    builtins; state persists across prompts",
			"  help, history this; recent lines",
			"  exit, Ctrl+D  leave aish",
			"",
			"Configuration: see PLAN.md (AISH_* env knobs).",
		} {
			print("%s", l)
		}
		return true, false
	case "exit", "quit":
		return true, true
	}
	return false, false
}

// ApplyIfBuiltin executes cd/export-style commands in-process so their
// effect persists (a subshell would lose it). Returns model feedback and
// whether the command was handled.
func (d *Director) ApplyIfBuiltin(line string) (string, bool) {
	words := strings.Fields(line)
	if len(words) == 0 {
		return "", false
	}
	// Only a bare builtin is applied in-process; compounds ("cd x && y")
	// belong to the shell and cannot persist.
	if strings.ContainsAny(line, "&|;<>`$(){}") || strings.ContainsRune(line, '"') {
		return "", false
	}
	switch words[0] {
	case "cd":
		dir := ""
		if len(words) > 1 {
			dir = os.ExpandEnv(words[1])
		}
		if dir == "" {
			dir = os.Getenv("HOME")
		}
		if err := os.Chdir(dir); err != nil {
			return "error: " + err.Error(), true
		}
		wd, _ := os.Getwd()
		return "changed directory; now in " + wd, true
	case "export":
		applied := 0
		for _, kv := range words[1:] {
			if key, val, ok := strings.Cut(kv, "="); ok {
				os.Setenv(key, val)
				applied++
			}
		}
		if applied == 0 {
			return "error: export needs KEY=VALUE", true
		}
		return fmt.Sprintf("exported %d variable(s)", applied), true
	}
	return "", false
}

// RunNative runs a command through /bin/sh with the terminal attached
// (streams wired directly, so interactive programs and progress bars work).
// Returns the exit code.
func (d *Director) RunNative(line string) int {
	cmd := exec.Command("/bin/sh", "-c", line)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	d.LastLine = line
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	if err != nil {
		return 127
	}
	return 0
}

// RunCaptured runs a command through /bin/sh without touching the main
// terminal streams (output is captured for the model's feedback loop).
// stdin stays attached. Returns (output, exitCode, err).
func (d *Director) RunCaptured(ctx context.Context, line string) (string, int, error) {
	var w io.Writer = newCappedBuffer(captureLimit)
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", line)
	cmd.Stdin, cmd.Stderr = os.Stdin, os.Stdout
	cmd.Stdout = w
	err := cmd.Run()
	d.LastLine = line
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		code = -1
	}
	return w.(*cappedBuffer).String(), code, err
}

// RunTee runs a command with stdout/stderr streamed to the terminal and
// simultaneously captured (capped) so the model can see it too. stdin stays
// attached. Returns (capturedOutput, exitCode, err).
func (d *Director) RunTee(ctx context.Context, line string) (string, int, error) {
	cap := newCappedBuffer(captureLimit)
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", line)
	cmd.Stdin = os.Stdin
	cmd.Stdout = io.MultiWriter(os.Stdout, cap)
	cmd.Stderr = io.MultiWriter(os.Stderr, cap)
	err := cmd.Run()
	d.LastLine = line
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		code = -1
	}
	return cap.String(), code, err
}

type cappedBuffer struct {
	sb   strings.Builder
	used int
	limit int
}

func newCappedBuffer(limit int) *cappedBuffer {
	return &cappedBuffer{limit: limit}
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	room := c.limit - c.used
	if room <= 0 {
		return len(p), nil
	}
	if len(p) > room {
		c.sb.Write(p[:room])
		c.sb.WriteString("\n… (truncated)")
		c.used = c.limit
		return len(p), nil
	}
	c.sb.Write(p)
	c.used += len(p)
	return len(p), nil
}

func (c *cappedBuffer) String() string { return c.sb.String() }
