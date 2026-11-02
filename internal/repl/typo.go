// Typo assistance: when the first word of a request is not an installed
// binary, tell the model the nearest real commands. Deterministic, local,
// no extra model turn.
package repl

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// ensureIndex lazily loads the dictionary and the PATH command list once.
func (r *Repl) ensureIndex() {
	r.cmdOnce.Do(func() {
		if r.dict == nil {
			r.dict = map[string]bool{}
		}
		// Real English words are never typos of commands ("list" ≠ "last").
		if b, err := os.ReadFile("/usr/share/dict/words"); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if w := strings.TrimSpace(line); w != "" {
					r.dict[strings.ToLower(w)] = true
				}
			}
		}
		seen := map[string]bool{}
		for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
			ents, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, e := range ents {
				name := e.Name()
				if e.IsDir() || seen[name] {
					continue
				}
				seen[name] = true
				r.cmdNames = append(r.cmdNames, name)
			}
		}
		sort.Strings(r.cmdNames)
	})
}

// nearestCommands returns up to n PATH entries within a small edit distance.
func (r *Repl) nearestCommands(word string) []string {
	r.ensureIndex()

	threshold := 1
	if len([]rune(word)) >= 6 {
		threshold = 2
	}
	type cand struct {
		name string
		dist int
	}
	var out []cand
	for _, name := range r.cmdNames {
		if diff := abs(len(name) - len(word)); diff > threshold {
			continue
		}
		if d := damerau(word, name); d <= threshold {
			out = append(out, cand{name, d})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].dist != out[j].dist {
			return out[i].dist < out[j].dist
		}
		return out[i].name < out[j].name
	})
	names := make([]string, 0, 3)
	for _, c := range out {
		names = append(names, c.name)
		if len(names) == 3 {
			break
		}
	}
	return names
}

// typoHint inspects a short, shell-shaped request whose first word is not a
// real command and returns a context note suggesting the closest matches.
func (r *Repl) typoHint(request string) string {
	fields := strings.Fields(request)
	if len(fields) == 0 || len(fields) > 4 {
		return ""
	}
	word := fields[0]
	if strings.ContainsAny(word, "/.-_=") || !isAlphaWord(word) {
		return ""
	}
	if _, err := exec.LookPath(word); err == nil {
		return "" // real command: not a typo
	}
	r.ensureIndex()
	if r.dict[strings.ToLower(word)] {
		return "" // an English word, not a misspelled command
	}
	matches := r.nearestCommands(word)
	if len(matches) == 0 {
		return ""
	}
	return "IMPORTANT: `" + word + "` is not an installed command — the user " +
		"most likely mistyped it. Closest installed commands: " +
		strings.Join(matches, ", ") + ". Unless the request clearly means " +
		"something else, resolve the typo to the closest command instead of " +
		"running `" + word + "`."
}

func isAlphaWord(w string) bool {
	if w == "" {
		return false
	}
	for _, c := range w {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// damerau computes optimal string alignment distance (adjacent transpositions
// count as one edit).
func damerau(a, b string) int {
	ar, br := []rune(a), []rune(b)
	la, lb := len(ar), len(br)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	d := make([][]int, la+1)
	for i := range d {
		d[i] = make([]int, lb+1)
		d[i][0] = i
	}
	for j := 0; j <= lb; j++ {
		d[0][j] = j
	}
	for i := 1; i <= la; i++ {
		for j := 1; j <= lb; j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			d[i][j] = min3(d[i-1][j]+1, d[i][j-1]+1, d[i-1][j-1]+cost)
			if i > 1 && j > 1 && ar[i-1] == br[j-2] && ar[i-2] == br[j-1] {
				if v := d[i-2][j-2] + 1; v < d[i][j] {
					d[i][j] = v
				}
			}
		}
	}
	return d[la][lb]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}
