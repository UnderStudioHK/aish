package repl

import (
	"strings"
	"testing"
)

func TestFastNative(t *testing.T) {
	cases := map[string]bool{
		"ls":                     true,
		"ls -la /home/user":      true,
		"git status":             true,
		"git -C /tmp status":     true, // -C tail
		"cat /etc/os-release":    true, // path tail
		"echo hi > out":          true, // redirect
		"grep -n main . | wc -l": true, // pipe
		"find . -name '*.go'":    true,
		"install figlet":         false, // verb-like binary + object → model
		"install -m 644 a b":     true,  // explicit flags → shell
		"make all":               true,
		"make the file readable": false,
		"npm run build":          true,
		"time ls":                true,
		"list":                   false, // bare non-binary word → model
		"free":                   true,  // bare real binary
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

func TestDamerau(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"gti", "git", 1}, // adjacent transposition
		{"git", "git", 0},
		{"list", "less", 2}, // must NOT be suggested at threshold 1
		{"", "ls", 2},
	}
	for _, c := range cases {
		if got := damerau(c.a, c.b); got != c.want {
			t.Errorf("damerau(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestTypoHint(t *testing.T) {
	r := &Repl{}
	if h := r.typoHint("gti status"); !strings.Contains(h, "git") {
		t.Errorf("typoHint(gti status) = %q, want git suggestion", h)
	}
	if h := r.typoHint("list"); h != "" {
		t.Errorf("typoHint(list) = %q, want empty (no close match)", h)
	}
	if h := r.typoHint("what kernel version is running on this machine"); h != "" {
		t.Errorf("typoHint(natural sentence) = %q, want empty", h)
	}
}

func TestReplayCache(t *testing.T) {
	r := &Repl{replay: map[string][]string{}}
	r.replayCmds = []string{"ls -la", "df -h /"}
	r.saveReplay("list")
	if got := r.replayFor("list"); len(got) != 2 || got[0] != "ls -la" {
		t.Errorf("replayFor(list) = %v", got)
	}
	if got := r.replayFor("list "); len(got) != 2 {
		t.Errorf("replayFor should trim: %v", got)
	}
	if got := r.replayFor("other"); len(got) != 0 {
		t.Errorf("unknown request should not replay: %v", got)
	}
	got := r.replayFor("list")
	got[0] = "mutated"
	if r.replayFor("list")[0] == "mutated" {
		t.Error("replayFor leaked its slice")
	}
}
