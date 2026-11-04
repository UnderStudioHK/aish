package repl

import (
	"testing"
	"time"

	"github.com/UnderStudioHK/aish/internal/config"
)

func TestActivityLifecycle(t *testing.T) {
	r := &Repl{cfg: &config.Config{}}

	// Tests are not a TTY, so the animation goroutine is skipped but the
	// start/stop bookkeeping and prompt state must still work.
	stop := r.startActivity("thinking")
	r.mu.Lock()
	if !r.busyAnim {
		r.mu.Unlock()
		t.Fatal("activity should mark the prompt busy")
	}
	r.mu.Unlock()

	stop()
	r.mu.Lock()
	if r.busyAnim || r.busyFrame != "" {
		r.mu.Unlock()
		t.Fatalf("activity not cleared: busy=%v frame=%q", r.busyAnim, r.busyFrame)
	}
	r.mu.Unlock()

	// Superseding an activity must not let the old stop clear the new one.
	stopOld := r.startActivity("old")
	stopNew := r.startActivity("new")
	stopOld()
	r.mu.Lock()
	if !r.busyAnim {
		r.mu.Unlock()
		t.Fatal("old stop cleared the new activity")
	}
	r.mu.Unlock()
	stopNew()
}

func TestStopActivityNow(t *testing.T) {
	r := &Repl{cfg: &config.Config{}}
	stop := r.startActivity("thinking")
	r.stopActivityNow()
	time.Sleep(10 * time.Millisecond)
	stop() // must be a harmless no-op
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.busyAnim {
		t.Error("stopActivityNow left the prompt busy")
	}
}
