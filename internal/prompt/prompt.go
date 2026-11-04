// Package prompt renders the interactive aish prompt line.
package prompt

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

// State is everything the prompt may display.
type State struct {
	Cwd       string // absolute working directory
	Home      string
	Exit      int    // last exit code (0 hides the segment)
	Pending   bool   // a follow-up proposal is queued
	Busy      bool   // a model turn is in flight
	BusyFrame string // animated frame for the busy segment
	Root      bool   // running as root
	Color     bool   // emit ANSI colors

	// Filled by Git when inside a work tree.
	Branch   string
	Detached bool
	Dirty    bool

	Style    string // "full" (default) | "minimal"
	Template string // optional template with {placeholders}
	User     string
	Host     string
}

const (
	cReset  = "\x1b[0m"
	cBold   = "\x1b[1m"
	cDim    = "\x1b[2m"
	cCyan   = "\x1b[36m"
	cMag    = "\x1b[35m"
	cYellow = "\x1b[33m"
	cGreen  = "\x1b[32m"
	cRed    = "\x1b[31m"
)

// Shorten renders a path compactly: ~ at home, relative under home, and all
// but the last two components abbreviated to their first letter.
func Shorten(cwd, home string) string {
	if cwd == "" {
		return "?"
	}
	sep := string(os.PathSeparator)
	if home != "" {
		if cwd == home {
			return "~"
		}
		if strings.HasPrefix(cwd, home+sep) {
			rel := strings.TrimPrefix(cwd, home+sep)
			return "~/" + abbreviate(strings.Split(rel, sep))
		}
	}
	if strings.HasPrefix(cwd, sep) {
		parts := strings.Split(strings.TrimPrefix(cwd, sep), sep)
		return "/" + abbreviate(parts)
	}
	return abbreviate(strings.Split(cwd, sep))
}

func abbreviate(parts []string) string {
	if len(parts) <= 2 {
		return strings.Join(parts, "/")
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts[:len(parts)-1] {
		out = append(out, initial(p))
	}
	out = append(out, parts[len(parts)-1])
	return strings.Join(out, "/")
}

func initial(p string) string {
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, ".") {
		// keep dotfiles readable: .config -> .c
		r, size := utf8.DecodeRuneInString(p)
		rest := p[size:]
		if rest == "" {
			return p
		}
		r2, _ := utf8.DecodeRuneInString(rest)
		return string(r) + string(r2)
	}
	_, size := utf8.DecodeRuneInString(p)
	return p[:size]
}

// Render builds the prompt string. The returned string always ends in a space.
func Render(s State) string {
	if s.Template != "" {
		return renderTemplate(s)
	}
	if s.Style == "minimal" {
		out := color(s, segment(s, "path"), cCyan, cBold)
		if s.Busy {
			out += " " + color(s, busyGlyph(s), cDim)
		}
		return out + " " + glyph(s) + " "
	}
	segs := []string{color(s, Shorten(s.Cwd, s.Home), cCyan, cBold)}
	if g := gitSegment(s); g != "" {
		segs = append(segs, g)
	}
	if s.Exit != 0 {
		segs = append(segs, color(s, fmt.Sprintf("✗%d", s.Exit), cRed))
	}
	if s.Pending {
		segs = append(segs, color(s, "⏎", cYellow))
	}
	if s.Busy {
		segs = append(segs, color(s, busyGlyph(s), cDim))
	}
	return strings.Join(segs, "  ") + " " + glyph(s) + " "
}

func segment(s State, name string) string {
	switch name {
	case "path":
		return Shorten(s.Cwd, s.Home)
	case "git":
		return stripANSI(gitSegment(s))
	case "branch":
		return s.Branch
	case "dirty":
		if s.Dirty {
			return "*"
		}
	case "detached":
		if s.Detached {
			return "@" + s.Branch
		}
	case "exit":
		if s.Exit != 0 {
			return fmt.Sprintf("✗%d", s.Exit)
		}
	case "pending":
		if s.Pending {
			return "⏎"
		}
	case "busy":
		if s.Busy {
			return busyGlyph(s)
		}
	case "user":
		return s.User
	case "host":
		return s.Host
	case "glyph":
		return string(glyphRune(s))
	}
	return ""
}

func gitSegment(s State) string {
	if s.Branch == "" {
		return ""
	}
	name := s.Branch
	if s.Detached {
		name = "@" + s.Branch
	}
	out := color(s, name, cMag)
	if s.Dirty {
		out += color(s, "*", cYellow)
	}
	return out
}

// busyGlyph returns the animated block for an in-flight model turn, with a
// static fallback for callers that only set Busy.
func busyGlyph(s State) string {
	if s.BusyFrame != "" {
		return s.BusyFrame
	}
	return "⋯"
}

func glyphRune(s State) rune {
	if s.Root {
		return '#'
	}
	if !s.Color {
		return '>'
	}
	return '❯'
}

func glyph(s State) string {
	r := glyphRune(s)
	if s.Root {
		return color(s, string(r), cRed, cBold)
	}
	if s.Exit != 0 {
		return color(s, string(r), cRed)
	}
	return color(s, string(r), cGreen)
}

func color(s State, text string, codes ...string) string {
	if !s.Color || text == "" {
		return text
	}
	return strings.Join(codes, "") + text + cReset
}

func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		if r == '\x1b' {
			inEsc = true
			continue
		}
		if inEsc {
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// renderTemplate expands {placeholders}; users may include their own ANSI.
func renderTemplate(s State) string {
	repl := map[string]string{
		"{cwd}":      Shorten(s.Cwd, s.Home),
		"{branch}":   s.Branch,
		"{dirty}":    segment(s, "dirty"),
		"{detached}": segment(s, "detached"),
		"{git}":      segment(s, "git"),
		"{exit}":     segment(s, "exit"),
		"{pending}":  segment(s, "pending"),
		"{busy}":     segment(s, "busy"),
		"{user}":     s.User,
		"{host}":     s.Host,
		"{glyph}":    string(glyphRune(s)),
	}
	out := s.Template
	for k, v := range repl {
		out = strings.ReplaceAll(out, k, v)
	}
	if !strings.HasSuffix(out, " ") {
		out += " "
	}
	return out
}
