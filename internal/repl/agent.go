// Agent side of the REPL: the sync first turn, the eager background
// follow-up, queued proposals, and their lifecycle.
package repl

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/UnderStudioHK/aish/internal/llm"
	"github.com/UnderStudioHK/aish/internal/pkgfind"
)

// ---- state helpers (all fields guarded by r.mu) ----

// beginNewInput cancels any in-flight follow-up, invalidates queued
// proposals with synthetic tool results (keeping the conversation valid),
// and resets the step counter for a fresh user request.
func (r *Repl) beginNewInput() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.epoch++
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	r.inFlight = false
	if len(r.pending) > 0 {
		for _, tc := range r.pending {
			r.talk = append(r.talk, llm.Message{Role: "tool", ToolCallID: tc.ID,
				Content: "not run: user moved on"})
		}
		r.pending = nil
	}
	r.steps = 0
	r.lastCmds = nil
	r.lastOK = false
}

// appendTalkLocked appends messages and trims to the configured depth.
func (r *Repl) appendTalkLocked(msgs ...llm.Message) {
	r.talk = append(r.talk, msgs...)
	max := r.cfg.HistoryTurns
	if max <= 0 {
		max = 1
	}
	var users []int
	for i, m := range r.talk {
		if m.Role == "user" {
			users = append(users, i)
		}
	}
	if len(users) > max+1 {
		r.talk = r.talk[users[len(users)-(max+1)]:]
	}
}

// talkCopy returns a snapshot for a model request.
func (r *Repl) talkCopy() []llm.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]llm.Message(nil), r.talk...)
}

// llmTurn is the single-flight model call (one request at a time).
// extra is optional per-request context appended after the live cwd block.
func (r *Repl) llmTurn(ctx context.Context, msgs []llm.Message, tools bool, extra string) (llm.Message, error) {
	sys := r.contextLine()
	if extra != "" {
		sys += "\n\n" + extra
	}
	r.llmMu.Lock()
	defer r.llmMu.Unlock()
	return r.llm.Turn(ctx, msgs, tools, sys)
}

// ---- prompt-safe hint rendering ----

// showHint prints a dim line above the active prompt (or plainly when no
// prompt is active). It is the only background-driven screen update.
func (r *Repl) showHint(text string) {
	r.uiMu.Lock()
	defer r.uiMu.Unlock()
	if r.promptActive && r.rl != nil {
		fmt.Fprintf(r.rl.Stdout(), "\r\x1b[K%s%s%s\n", cDim, text, cNorm)
		// repaint so state markers (pending ⏎) appear in the prompt
		r.rl.SetPrompt(r.promptString())
		r.rl.Operation.Refresh()
		return
	}
	fmt.Printf("%s%s%s\n", cDim, text, cNorm)
}

// ---- sync first turn ----

// handleIntent runs the blocking part of a request: one model turn and,
// if it proposes commands, their execution. Then it either finishes or
// arms the follow-up mode. Returns the exit code.
func (r *Repl) handleIntent(request string) int {
	if !r.readyLLM() {
		return r.dir.RunNative(request)
	}
	r.mu.Lock()
	r.currentReq = request
	r.mu.Unlock()

	// Exact repeat of a request that previously ran commands: replay those
	// commands (gated as always) instead of asking the model to reconstruct
	// an answer from stale transcript content.
	if cmds := r.replayFor(request); len(cmds) > 0 {
		r.mu.Lock()
		r.steps = 1
		r.replayCmds = nil
		r.appendTalkLocked(llm.Message{Role: "user", Content: request})
		r.mu.Unlock()
		r.debugLog("replay: %d command(s)", len(cmds))
		var summary strings.Builder
		for _, cmd := range cmds {
			r.scopeWarn(cmd)
			lv := r.policy.Classify(cmd)
			fmt.Printf("%s%s%s%s\n", colorFor(lv), cBold, cmd, cNorm)
			if !r.mayRun(cmd, lv) {
				fmt.Fprintf(os.Stderr, "%saish: cancelled%s\n", cDim, cNorm)
				return r.lastCode
			}
			result := r.runCommand(cmd)
			fmt.Fprintf(&summary, "re-ran `%s`:\n%s\n", cmd, result)
			r.mu.Lock()
			r.replayCmds = append(r.replayCmds, cmd)
			r.mu.Unlock()
		}
		if summary.Len() > 4000 {
			summary.Reset() // do not dump huge output back into history
		}
		r.mu.Lock()
		if summary.Len() > 0 {
			r.appendTalkLocked(llm.Message{Role: "assistant", Content: summary.String()})
		}
		r.mu.Unlock()
		r.saveReplay(request)
		return r.lastCode
	}

	r.mu.Lock()
	r.steps = 1
	r.replayCmds = nil
	r.appendTalkLocked(llm.Message{Role: "user", Content: request})
	msgs := append([]llm.Message(nil), r.talk...)
	r.mu.Unlock()

	start := time.Now()
	defer r.debugTime("model", start)

	debugDump("intent", msgs)
	tctx, cancel := context.WithTimeout(context.Background(), r.cfg.LLMTimeout)
	asMsg, err := r.llmTurn(tctx, msgs, true, r.typoHint(request))
	cancel()
	if err != nil {
		fmt.Printf("%saish: %v%s\n", cRed, err, cNorm)
		return -1
	}
	r.debugLog("intent: calls=%d content=%d", len(asMsg.ToolCalls), len(asMsg.Content))

	if content := llm.Sanitize(asMsg.Content); content != "" {
		// Commentary accompanying tool calls is worth showing; a final
		// answer to a request that needed no commands likewise.
		fmt.Printf("%s%s%s\n", cYell, content, cNorm)
	}
	r.mu.Lock()
	r.appendTalkLocked(asMsg)
	r.mu.Unlock()

	if len(asMsg.ToolCalls) == 0 {
		// The model answered without acting: it is done. Scheduling a
		// follow-up here would burn a turn and invent further work.
		return r.lastCode
	}
	if !r.executeCalls(asMsg.ToolCalls) {
		// declined: turn ends here
		return r.lastCode
	}
	r.saveReplay(request)
	r.armFollowUp()
	return r.lastCode
}

// replayFor returns the commands recorded for an identical earlier request.
func (r *Repl) replayFor(request string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	cmds := r.replay[strings.TrimSpace(request)]
	return append([]string(nil), cmds...)
}

// saveReplay records the commands just run under the request text.
func (r *Repl) saveReplay(request string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.replayCmds) == 0 {
		return
	}
	if r.replay == nil {
		r.replay = map[string][]string{}
	}
	if len(r.replay) > 128 { // bounded, crude but safe
		r.replay = map[string][]string{}
	}
	r.replay[strings.TrimSpace(request)] = append([]string(nil), r.replayCmds...)
	r.replayCmds = nil
}

// executeCalls gates and runs a batch of tool calls, appending results to
// the conversation. Returns false if the user declined.
func (r *Repl) executeCalls(calls []llm.ToolCall) bool {
	// Guard against the model re-proposing the identical command inside a
	// chain (common when a command legitimately has no output).
	if r.isDuplicateBatch(calls) {
		r.mu.Lock()
		for _, tc := range calls {
			r.appendTalkLocked(llm.Message{Role: "tool", ToolCallID: tc.ID,
				Content: "not run: identical to the command just executed (see its output)"})
		}
		r.mu.Unlock()
		fmt.Printf("%s↳ same command as the previous step; stopping%s\n", cDim, cNorm)
		return false
	}

	allOK := true
	defer func() {
		r.mu.Lock()
		r.lastCmds = batchCommands(calls)
		r.lastOK = allOK
		r.mu.Unlock()
	}()

	for _, tc := range calls {
		// find_package is read-only: resolve locally, no gate.
		if tc.Func.Name == "find_package" {
			r.findPackage(tc)
			continue
		}
		if tc.Func.Name != "" && tc.Func.Name != "run_command" {
			r.mu.Lock()
			r.appendTalkLocked(llm.Message{Role: "tool", ToolCallID: tc.ID,
				Content: "error: unknown tool " + tc.Func.Name})
			r.mu.Unlock()
			continue
		}
		cmd, ok := llm.CommandOf(tc)
		if !ok {
			r.mu.Lock()
			r.appendTalkLocked(llm.Message{Role: "tool", ToolCallID: tc.ID,
				Content: "error: unreadable tool arguments"})
			r.mu.Unlock()
			continue
		}
		r.scopeWarn(cmd)
		lv := r.policy.Classify(cmd)
		fmt.Printf("%s%s%s%s\n", colorFor(lv), cBold, cmd, cNorm)
		if !r.mayRun(cmd, lv) {
			r.mu.Lock()
			for _, rest := range calls {
				content := "not run: batch cancelled"
				if rest.ID == tc.ID {
					content = "user declined the command"
				}
				r.appendTalkLocked(llm.Message{Role: "tool", ToolCallID: rest.ID, Content: content})
			}
			r.mu.Unlock()
			fmt.Fprintf(os.Stderr, "%saish: cancelled%s\n", cDim, cNorm)
			return false
		}
		result := r.runCommand(cmd)
		if r.lastCode != 0 {
			allOK = false
		}
		r.mu.Lock()
		r.appendTalkLocked(llm.Message{Role: "tool", ToolCallID: tc.ID, Content: result})
		r.replayCmds = append(r.replayCmds, cmd)
		r.mu.Unlock()
	}
	return true
}

// findPackage resolves a package name against local ecosystems and feeds
// the answer back to the model.
func (r *Repl) findPackage(tc llm.ToolCall) {
	name, ok := llm.PackageOf(tc)
	if !ok {
		r.mu.Lock()
		r.appendTalkLocked(llm.Message{Role: "tool", ToolCallID: tc.ID,
			Content: "error: missing package name"})
		r.mu.Unlock()
		return
	}
	fmt.Printf("%s? find_package %s%s\n", cDim, name, cNorm)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	out := pkgfind.Resolve(ctx, name)
	cancel()
	r.mu.Lock()
	r.appendTalkLocked(llm.Message{Role: "tool", ToolCallID: tc.ID, Content: out})
	r.mu.Unlock()
}

// runCommand executes one command (builtin-aware), returning model feedback.
func (r *Repl) runCommand(cmd string) string {
	if fb, handled := r.dir.ApplyIfBuiltin(cmd); handled {
		r.audit(cmd, 0)
		return fb
	}
	cctx, cancel := context.WithTimeout(context.Background(), r.cfg.CmdTimeout)
	out, code, _ := r.dir.RunTee(cctx, cmd)
	cancel()
	if strings.HasPrefix(cmd, "cd ") || cmd == "cd" {
		out += "\nnote: cd inside a compound command runs in a subshell; " +
			"the persistent working directory is unchanged. Emit a plain `cd <dir>` to change it."
	}
	r.audit(cmd, code)
	r.lastCode = code
	r.lastShown = code

	// Make silent outcomes visible to the user: an empty success and a
	// failure with no message both look like "nothing happened" otherwise.
	empty := strings.TrimSpace(out) == ""
	r.lastEmpty = empty
	if code != 0 {
		fmt.Fprintf(os.Stderr, "%s✗ exit %d%s\n", cDim, code, cNorm)
	} else if empty {
		fmt.Printf("%s(no output, exit 0)%s\n", cDim, cNorm)
	}
	feedback := fmt.Sprintf("exit_code=%d\n%s", code, out)
	if empty {
		feedback += "(no output — the command succeeded silently)\n"
	}
	return feedback
}

// audit appends executed commands to <state>/commands.log for diagnosis.
func (r *Repl) audit(cmd string, code int) {
	if err := os.MkdirAll(r.cfg.StateDir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(r.cfg.StateDir, "commands.log"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	wd, _ := os.Getwd()
	fmt.Fprintf(f, "%s\t%s\t%d\t%s\n", time.Now().Format(time.RFC3339), wd, code, cmd)
}

// batchCommands extracts run_command strings from a batch.
func batchCommands(calls []llm.ToolCall) []string {
	var out []string
	for _, tc := range calls {
		if tc.Func.Name == "find_package" {
			continue
		}
		if cmd, ok := llm.CommandOf(tc); ok {
			out = append(out, cmd)
		}
	}
	return out
}

// normalizeCmd strips output-redirection / fallback noise so cosmetic
// rewrites of the same command still count as duplicates.
func normalizeCmd(cmd string) string {
	c := strings.TrimSpace(cmd)
	for _, cut := range []string{"||", "&&", ";"} {
		if i := strings.Index(c, cut); i >= 0 {
			c = c[:i]
		}
	}
	c = strings.TrimSpace(c)
	for _, suffix := range []string{"2>&1", "2>/dev/null", "2> /dev/null", "1>/dev/null"} {
		c = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(c), suffix))
	}
	return c
}

// dupBatchLocked: caller holds r.mu.
func (r *Repl) dupBatchLocked(calls []llm.ToolCall) bool {
	if !r.lastOK || len(r.lastCmds) == 0 {
		return false
	}
	cmds := batchCommands(calls)
	if len(cmds) != len(r.lastCmds) {
		return false
	}
	for i := range cmds {
		if normalizeCmd(cmds[i]) != normalizeCmd(r.lastCmds[i]) {
			return false
		}
	}
	return true
}

// isDuplicateBatch reports whether this chain is re-proposing the same
// command batch that just ran successfully.
func (r *Repl) isDuplicateBatch(calls []llm.ToolCall) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dupBatchLocked(calls)
}

// scopeWarn prints an advisory when a proposal leaves the current directory
// for $HOME without the request asking for it.
func (r *Repl) scopeWarn(cmd string) {
	wd, _ := os.Getwd()
	home, _ := os.UserHomeDir()
	if home == "" || wd == home {
		return
	}
	req := strings.ToLower(r.currentReq)
	reqAsksHome := strings.Contains(req, "home") || strings.Contains(req, "~") ||
		strings.Contains(req, home)
	if reqAsksHome {
		return
	}
	lo := strings.ToLower(cmd)
	if strings.Contains(lo, strings.ToLower(home)) || strings.Contains(cmd, "$HOME") ||
		strings.Contains(cmd, "~/") {
		fmt.Printf("%s⚠ scope: searches %s while cwd is %s%s\n", cDim, home, wd, cNorm)
	}
}

// ---- follow-up modes ----

// armFollowUp decides what happens after the last tool batch ran.
func (r *Repl) armFollowUp() {
	switch r.cfg.FollowUp {
	case "eager":
		r.mu.Lock()
		exhausted := r.steps >= r.cfg.MaxSteps
		steps := r.steps
		r.mu.Unlock()
		if exhausted {
			r.showHint(fmt.Sprintf("aish: stopped after %d steps", steps))
			return
		}
		r.startFollowUp()
	case "lazy":
		r.showHint("↳ press ⏎ to continue")
	}
}

// startFollowUp runs one model turn in the background. Its only outputs
// are: transcript updates, a queued proposal, or a narration hint.
func (r *Repl) startFollowUp() {
	r.mu.Lock()
	if r.inFlight || len(r.pending) > 0 || !r.cfg.Enabled() || r.cfg.FollowUp != "eager" {
		r.mu.Unlock()
		return
	}
	r.epoch++
	ep := r.epoch
	ctx, cancel := context.WithTimeout(context.Background(), r.cfg.LLMTimeout)
	r.inFlight = true
	r.cancel = cancel
	msgs := append([]llm.Message(nil), r.talk...)
	r.mu.Unlock()

	go func() {
		defer cancel()
		start := time.Now()
		msg, err := r.llmTurn(ctx, msgs, true, "")
		r.debugTime("follow-up", start)
		if err == nil {
			r.debugLog("follow-up: calls=%d content=%d", len(msg.ToolCalls), len(msg.Content))
		}

		if err != nil {
			r.mu.Lock()
			stale := r.epoch != ep
			if !stale {
				r.inFlight = false
				r.cancel = nil
			}
			r.mu.Unlock()
			if !stale && ctx.Err() == nil {
				r.showHint(fmt.Sprintf("aish: follow-up failed: %v", err))
			}
			return
		}

		// Check freshness and commit in one critical section, so a user
		// input can never interleave between the check and the append.
		r.mu.Lock()
		if r.epoch != ep {
			r.mu.Unlock()
			return
		}
		r.inFlight = false
		r.cancel = nil
		r.appendTalkLocked(msg)
		var calls []llm.ToolCall
		dup := len(msg.ToolCalls) > 0 && r.dupBatchLocked(msg.ToolCalls)
		if len(msg.ToolCalls) > 0 {
			calls = msg.ToolCalls
			if dup {
				// Close the tool-call pair without running anything.
				for _, tc := range calls {
					r.appendTalkLocked(llm.Message{Role: "tool", ToolCallID: tc.ID,
						Content: "not run: identical to the command just executed"})
				}
			} else {
				r.pending = append([]llm.ToolCall(nil), calls...)
			}
		}
		emptyOut := r.lastEmpty
		r.mu.Unlock()

		if dup {
			fmt.Printf("%s↳ repeated command ignored%s\n", cDim, cNorm)
			return
		}

		switch {
		case len(calls) > 0:
			r.showHint("↳ follow-up: " + describeCalls(calls) + "   (⏎ run · any input dismisses)")
		case strings.TrimSpace(msg.Content) != "" && (r.cfg.Narrate || emptyOut):
			// A silent-success command benefits from the model's one-line
			// explanation even when narration is otherwise off.
			r.showHint(llm.Sanitize(msg.Content))
		}
	}()
}

// continueSync runs one follow-up turn in the foreground (lazy mode,
// triggered by Enter on an empty prompt).
func (r *Repl) continueSync() {
	if !r.readyLLM() {
		return
	}
	r.mu.Lock()
	r.steps++
	steps := r.steps
	r.mu.Unlock()
	if steps > r.cfg.MaxSteps {
		r.showHint(fmt.Sprintf("aish: stopped after %d steps", r.cfg.MaxSteps))
		return
	}
	msgs := r.talkCopy()
	start := time.Now()
	tctx, cancel := context.WithTimeout(context.Background(), r.cfg.LLMTimeout)
	msg, err := r.llmTurn(tctx, msgs, true, "")
	cancel()
	r.debugTime("continue", start)
	if err != nil {
		fmt.Printf("%saish: %v%s\n", cRed, err, cNorm)
		return
	}
	if content := llm.Sanitize(msg.Content); content != "" {
		fmt.Printf("%s%s%s\n", cYell, content, cNorm)
	}
	r.mu.Lock()
	r.appendTalkLocked(msg)
	r.mu.Unlock()
	if len(msg.ToolCalls) > 0 && r.executeCalls(msg.ToolCalls) {
		r.armFollowUp()
	}
}

// acceptPending runs the queued follow-up proposal (Enter on empty prompt).
// Returns true when it consumed the Enter.
func (r *Repl) acceptPending() bool {
	r.mu.Lock()
	calls := r.pending
	r.pending = nil
	r.steps++
	steps := r.steps
	r.mu.Unlock()
	if len(calls) == 0 {
		return false
	}
	if steps > r.cfg.MaxSteps {
		r.showHint(fmt.Sprintf("aish: stopped after %d steps", r.cfg.MaxSteps))
		return true
	}
	if !r.executeCalls(calls) {
		return true
	}
	r.armFollowUp()
	return true
}

// hasPending reports whether a proposal is queued.
func (r *Repl) hasPending() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending) > 0
}

func describeCalls(calls []llm.ToolCall) string {
	if len(calls) == 0 {
		return ""
	}
	cmd := "?"
	if calls[0].Func.Name == "find_package" {
		if name, ok := llm.PackageOf(calls[0]); ok {
			cmd = "find_package " + name
		}
	} else if c, ok := llm.CommandOf(calls[0]); ok {
		cmd = c
	}
	if len(calls) > 1 {
		return fmt.Sprintf("%s (+%d more)", cmd, len(calls)-1)
	}
	return cmd
}

func (r *Repl) debugLog(format string, a ...any) {
	if os.Getenv("AISH_DEBUG") == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "%sdebug: "+format+"%s\n", append([]any{cDim}, append(a, cNorm)...)...)
}

func (r *Repl) debugTime(label string, start time.Time) {
	if os.Getenv("AISH_DEBUG") == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "%s[%s %s]%s\n", cDim, label,
		time.Since(start).Round(100*time.Millisecond), cNorm)
}

// debugDump writes the request messages for inspection when AISH_DEBUG is on.
func debugDump(stage string, msgs []llm.Message) {
	if os.Getenv("AISH_DEBUG") == "" {
		return
	}
	b, _ := json.MarshalIndent(msgs, "", " ")
	dir := filepath.Join(os.TempDir(), "aish-dbg")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "aish_dbg_"+stage+".json"), b, 0o600)
}
