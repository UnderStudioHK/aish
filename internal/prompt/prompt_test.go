package prompt

import (
	"strings"
	"testing"
)

func TestShorten(t *testing.T) {
	home := "/home/u"
	cases := map[string]string{
		"/home/u":                          "~",
		"/home/u/Documents/Code/aish":      "~/D/C/aish",
		"/home/u/Code":                     "~/Code",
		"/home/u/a":                        "~/a",
		"/opt/one/two/three/four":          "/o/o/t/t/four",
		"/tmp":                             "/tmp",
		"/home/userelsewhere/project/deep": "/h/u/p/deep", // not under ~
	}
	for in, want := range cases {
		if got := Shorten(in, home); got != want {
			t.Errorf("Shorten(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderFull(t *testing.T) {
	s := State{
		Cwd: "/home/u/Documents/Code/aish", Home: "/home/u",
		Branch: "master", Dirty: true, Exit: 1, Pending: true, Color: false,
	}
	got := Render(s)
	if !strings.Contains(got, "~/D/C/aish") || !strings.Contains(got, "master*") ||
		!strings.Contains(got, "✗1") || !strings.Contains(got, "⏎") || !strings.Contains(got, ">") {
		t.Errorf("Render(full) = %q", got)
	}
	if !strings.HasSuffix(got, " ") {
		t.Errorf("prompt must end with space: %q", got)
	}
}

func TestRenderMinimal(t *testing.T) {
	s := State{Cwd: "/tmp", Home: "/home/u", Branch: "master", Exit: 2, Style: "minimal"}
	got := Render(s)
	if strings.Contains(got, "master") || strings.Contains(got, "✗") {
		t.Errorf("minimal should only show path+glyph: %q", got)
	}
	if !strings.HasPrefix(got, "/tmp ") {
		t.Errorf("Render(minimal) = %q", got)
	}
}

func TestRenderColor(t *testing.T) {
	s := State{Cwd: "/tmp", Home: "/home/u", Color: true}
	got := Render(s)
	if !strings.Contains(got, "\x1b[36m") || !strings.Contains(got, "❯") {
		t.Errorf("color prompt missing codes/glyph: %q", got)
	}
	if strings.Contains(Render(State{Cwd: "/tmp", Home: "/home/u"}), "\x1b[") {
		t.Error("no-color prompt must not contain ANSI")
	}
}

func TestRenderTemplate(t *testing.T) {
	s := State{
		Cwd: "/tmp", Home: "/home/u", Branch: "dev", Dirty: true, Exit: 3,
		Pending: true, User: "lv", Host: "box", Template: "[{branch}{dirty}] {cwd} {glyph}",
	}
	got := Render(s)
	want := "[dev*] /tmp > "
	if got != want {
		t.Errorf("template = %q, want %q", got, want)
	}
}

func TestGitForNonRepo(t *testing.T) {
	if info := GitFor(t.TempDir()); info.Branch != "" {
		t.Errorf("non-repo should have no branch: %+v", info)
	}
}

func TestRenderBusyBlock(t *testing.T) {
	s := State{Cwd: "/tmp", Home: "/home/u", Busy: true, BusyFrame: "█"}
	if got := Render(s); !strings.Contains(got, "█") {
		t.Errorf("full prompt missing busy frame: %q", got)
	}
	s.Style = "minimal"
	if got := Render(s); !strings.Contains(got, "█") {
		t.Errorf("minimal prompt missing busy frame: %q", got)
	}
	s.Template = "[{busy}] {cwd}"
	if got := Render(s); !strings.Contains(got, "[█]") {
		t.Errorf("template prompt missing busy frame: %q", got)
	}
	// A caller that only sets Busy keeps the static fallback.
	s = State{Cwd: "/tmp", Home: "/home/u", Busy: true}
	if got := Render(s); !strings.Contains(got, "⋯") {
		t.Errorf("static busy fallback missing: %q", got)
	}
}
