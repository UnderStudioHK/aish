//go:build linux

// aish patch: let a bare ESC be detected without consuming the next key.
package readline

import (
	"os"
	"syscall"
	"time"
)

// readableWithin reports whether f has data ready within d.
func readableWithin(f *os.File, d time.Duration) bool {
	fd := int(f.Fd())
	var rfds syscall.FdSet
	wordBits := 64
	idx := fd / wordBits
	if idx >= len(rfds.Bits) {
		return true // out of range: be conservative, let the read block
	}
	rfds.Bits[idx] |= 1 << (uint(fd) % uint(wordBits))
	tv := syscall.NsecToTimeval(d.Nanoseconds())
	n, err := syscall.Select(fd+1, &rfds, nil, nil, &tv)
	return err != nil || n > 0
}
