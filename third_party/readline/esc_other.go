//go:build !linux

// aish patch: non-Linux fallback keeps the original blocking behaviour.
package readline

import (
	"os"
	"time"
)

func readableWithin(_ *os.File, _ time.Duration) bool { return true }
