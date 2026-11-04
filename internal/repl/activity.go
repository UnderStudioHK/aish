package repl

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/term"
)

// activityFrames is a pulsing block used while a model turn is in flight.
// A block reads as "working" without looking like a progress bar that could
// be mistaken for command output.
var activityFrames = []string{"▁", "▂", "▃", "▄", "▅", "▆", "▇", "█", "▇", "▆", "▅", "▄", "▃", "▂"}

const activityInterval = 120 * time.Millisecond

// startActivity animates a small status indicator until the returned stop
// function is called. With an active readline prompt the indicator is drawn
// inside the prompt; otherwise it is a transient line at the bottom of the
// terminal. Non-interactive output is left alone.
func (r *Repl) startActivity(label string) func() {
	r.actMu.Lock()
	if r.actStop != nil {
		close(r.actStop)
		r.actStop = nil
	}
	gen := r.actGen.Add(1)
	stop := make(chan struct{})
	r.actStop = stop
	r.mu.Lock()
	r.busyAnim = true
	r.busyFrame = activityFrames[0]
	r.mu.Unlock()
	r.actMu.Unlock()

	if !term.IsTerminal(int(os.Stdout.Fd())) {
		return func() { r.stopActivity(gen, stop) }
	}
	go r.animateActivity(gen, stop, label)
	return func() { r.stopActivity(gen, stop) }
}

// stopActivity ends one animation. A newer activity may already have
// superseded it, in which case this is a no-op.
func (r *Repl) stopActivity(gen uint64, stop chan struct{}) {
	r.actMu.Lock()
	defer r.actMu.Unlock()
	if r.actGen.Load() != gen {
		return
	}
	r.actGen.Add(1) // invalidate the animation goroutine
	if r.actStop == stop {
		r.actStop = nil
	}
	close(stop)
	r.clearActivityLocked()
}

// stopActivityNow ends whichever animation is running, if any. Used when a
// new user input takes over from background work.
func (r *Repl) stopActivityNow() {
	r.actMu.Lock()
	defer r.actMu.Unlock()
	if r.actStop != nil {
		close(r.actStop)
		r.actStop = nil
	}
	r.actGen.Add(1)
	r.clearActivityLocked()
}

func (r *Repl) animateActivity(gen uint64, stop <-chan struct{}, label string) {
	start := time.Now()
	ticker := time.NewTicker(activityInterval)
	defer ticker.Stop()

	i := 0
	r.renderActivity(gen, label, activityFrames[i], start)
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			i = (i + 1) % len(activityFrames)
			r.renderActivity(gen, label, activityFrames[i], start)
		}
	}
}

func (r *Repl) renderActivity(gen uint64, label, frame string, start time.Time) {
	if r.actGen.Load() != gen {
		return
	}
	r.uiMu.Lock()
	defer r.uiMu.Unlock()
	if r.actGen.Load() != gen {
		return
	}

	if r.promptActive && r.rl != nil {
		// Repaint the prompt in place; readline preserves the typed line.
		r.mu.Lock()
		r.busyAnim = true
		r.busyFrame = frame
		r.mu.Unlock()
		r.rl.SetPrompt(r.promptString())
		r.rl.Operation.Refresh()
		return
	}

	// A background follow-up may start a tick before readline paints the
	// prompt. Wait for it rather than drawing a line the prompt would join.
	r.mu.Lock()
	background := r.inFlight
	r.mu.Unlock()
	if background {
		return
	}

	// No prompt is active (a foreground model turn): animate a single line.
	elapsed := time.Since(start).Seconds()
	fmt.Fprintf(os.Stdout, "\r\x1b[K%s%s %s  %.1fs%s", cDim, frame, label, elapsed, cNorm)
}

func (r *Repl) clearActivityLocked() {
	r.uiMu.Lock()
	defer r.uiMu.Unlock()

	r.mu.Lock()
	r.busyAnim = false
	r.busyFrame = ""
	r.mu.Unlock()

	if r.promptActive && r.rl != nil {
		r.rl.SetPrompt(r.promptString())
		r.rl.Operation.Refresh()
		return
	}
	if term.IsTerminal(int(os.Stdout.Fd())) {
		fmt.Fprint(os.Stdout, "\r\x1b[K")
	}
}
