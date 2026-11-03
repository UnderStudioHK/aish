package repl

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/chzyer/readline"
)

// shellCompleter completes command names at the first word and file/directory
// paths everywhere else. Path completion is relative to the current working
// directory unless the token itself is absolute or starts with "~".
type shellCompleter struct {
	commands *readline.PrefixCompleter
}

func newShellCompleter() *shellCompleter {
	items := make([]readline.PrefixCompleterInterface, 0, 128)
	seen := map[string]bool{}
	add := func(w string) {
		if !seen[w] {
			items = append(items, readline.PcItem(w))
			seen[w] = true
		}
	}
	add("!")
	add("??")
	for _, b := range []string{"cd", "pwd", "export", "exit", "help", "history", "env"} {
		add(b)
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			add(e.Name())
		}
	}
	return &shellCompleter{commands: readline.NewPrefixCompleter(items...)}
}

// Do implements readline.AutoCompleter.
func (c *shellCompleter) Do(line []rune, pos int) ([][]rune, int) {
	// readline only appends at the cursor, so completing in the middle of a
	// line would insert text instead of replacing it. Stay at the end.
	if pos < 0 || pos > len(line) || pos != len(line) {
		return nil, 0
	}

	start := tokenStart(line, pos)
	token := string(line[start:pos])
	first := strings.TrimSpace(string(line[:start])) == ""

	if first && !looksLikePath(token) {
		return c.commands.Do(line, pos)
	}
	return completePathToken(token)
}

// tokenStart returns the index where the token containing pos begins. It
// understands quotes, backslash escapes and common shell separators.
func tokenStart(line []rune, pos int) int {
	start := 0
	var quote rune
	escaped := false
	for i := 0; i < pos; i++ {
		r := line[i]
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			}
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
		case ' ', '\t', '\n', '|', '&', ';', '(', ')', '<', '>':
			start = i + 1
		}
	}
	return start
}

func looksLikePath(token string) bool {
	return strings.ContainsAny(token, "/") ||
		strings.HasPrefix(token, ".") ||
		strings.HasPrefix(token, "~")
}

// completePathToken completes the value of a token, including values after
// "--flag=". The returned offset covers the whole token so readline can show
// the typed prefix above the candidate list.
func completePathToken(token string) ([][]rune, int) {
	value := token
	if i := strings.LastIndex(token, "="); i >= 0 && strings.HasPrefix(token, "-") {
		value = token[i+1:]
	}
	return pathSuffixes(value), len([]rune(token))
}

// pathSuffixes returns completion suffixes for a single path fragment. The
// suffix is what readline appends after the already-typed token.
func pathSuffixes(value string) [][]rune {
	if value == "" {
		return listPath(".", "", 0, false)
	}

	var quote rune
	raw := value
	if raw[0] == '\'' || raw[0] == '"' {
		quote = rune(raw[0])
		raw = raw[1:]
		// A closed quote means the token is finished; do not reopen it.
		if len(raw) > 0 && rune(raw[len(raw)-1]) == quote {
			return nil
		}
	}
	if quote == 0 {
		raw = unescapePath(raw)
	}

	// Split "dir/base" at the last slash.
	dir, base := "", raw
	if i := strings.LastIndex(raw, "/"); i >= 0 {
		dir, base = raw[:i+1], raw[i+1:]
	}

	// "~" alone needs a slash inserted before the candidate; "~/x" already
	// has one in the typed token.
	tilde := false
	if strings.HasPrefix(raw, "~") {
		switch {
		case raw == "~":
			dir, base, tilde = "~/", "", true
		case strings.HasPrefix(raw, "~/"):
			// dir already contains the "~/..." prefix
		default:
			return nil // ~user is not expanded
		}
	}

	return listPath(dir, base, quote, tilde)
}

func listPath(dir, base string, quote rune, tilde bool) [][]rune {
	search := dir
	if search == "" {
		search = "."
	}
	search = expandHome(search)

	entries, err := os.ReadDir(search)
	if err != nil {
		return nil
	}

	out := make([][]rune, 0, len(entries)+1)
	// "." and ".." are not returned by ReadDir, but are useful completions.
	if dir == "" && (base == "." || base == "..") {
		out = append(out, []rune("/"))
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, base) {
			continue
		}
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(base, ".") {
			continue
		}

		isDir := e.IsDir()
		if e.Type()&os.ModeSymlink != 0 {
			if fi, err := os.Stat(filepath.Join(search, name)); err == nil {
				isDir = fi.IsDir()
			}
		}

		cand := name
		if isDir {
			cand += "/"
		}
		suffix := cand[len(base):]
		if tilde {
			suffix = "/" + suffix
		}
		suffix = escapeCompletion(suffix, quote)
		if !isDir && quote == 0 {
			suffix += " " // a completed file argument is ready for the next one
		}
		out = append(out, []rune(suffix))
	}
	return out
}

func expandHome(p string) string {
	if p == "~" {
		if h, err := os.UserHomeDir(); err == nil {
			return h
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

// unescapePath turns "my\ file" into "my file" for filesystem lookup.
func unescapePath(s string) string {
	var b strings.Builder
	escaped := false
	for _, r := range s {
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		b.WriteRune(r)
	}
	if escaped {
		b.WriteRune('\\')
	}
	return b.String()
}

// escapeCompletion escapes a candidate for insertion into the current token.
func escapeCompletion(s string, quote rune) string {
	switch quote {
	case '\'':
		return strings.ReplaceAll(s, "'", `'\''`)
	case '"':
		var b strings.Builder
		for _, r := range s {
			if r == '\\' || r == '"' || r == '$' || r == '`' {
				b.WriteRune('\\')
			}
			b.WriteRune(r)
		}
		return b.String()
	}

	var b strings.Builder
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\\', '\'', '"', '$', '`', '&', '|', ';',
			'<', '>', '(', ')', '[', ']', '{', '}', '*', '?', '!', '#', '~':
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
