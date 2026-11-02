package repl

import "testing"

func TestFastNative(t *testing.T) {
	cases := map[string]bool{
		"ls":                     true,
		"ls -la /home/lvturner":  true,
		"git status":             true,
		"git -C /tmp status":     true, // -C tail
		"cat /etc/os-release":    true, // path tail
		"echo hi > out":          true, // redirect
		"grep -n main . | wc -l": true, // pipe
		"find . -name '*.go'":    true,
		"grep all my huge files": false, // free-form tail
		"python all large ones":  false,
		"xyzzy --flag":           false, // unknown binary
		"explain the kernel log": false, // not a binary at all
	}
	for line, want := range cases {
		if got := fastNative(line); got != want {
			t.Errorf("fastNative(%q) = %v, want %v", line, got, want)
		}
	}
}
