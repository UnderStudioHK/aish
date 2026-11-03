package repl

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/chzyer/readline"
)

func completionStrings(cands [][]rune) []string {
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = string(c)
	}
	return out
}

func setupCompletionDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "beta"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha.txt", "my file.txt", ".hidden", "beta/inner.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
	return dir
}

func TestPathCompletion(t *testing.T) {
	setupCompletionDir(t)

	tests := []struct {
		token string
		want  []string
	}{
		{"al", []string{"pha.txt "}},
		{"be", []string{"ta/"}},
		{"beta/in", []string{"ner.txt "}},
		{"my\\ f", []string{"ile.txt "}},
		{`"my f`, []string{"ile.txt"}},
		{".h", []string{"idden "}},
		{".", []string{"/", "hidden "}},
		{"..", []string{"/"}},
		{"", []string{"alpha.txt ", "beta/", "my\\ file.txt "}},
	}
	for _, tc := range tests {
		got := completionStrings(pathSuffixes(tc.token))
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("pathSuffixes(%q) = %#v, want %#v", tc.token, got, tc.want)
		}
	}
}

func TestPathCompletionDirectoriesAndFlags(t *testing.T) {
	setupCompletionDir(t)

	if got := completionStrings(pathSuffixes("beta/")); !reflect.DeepEqual(got, []string{"inner.txt "}) {
		t.Errorf("beta/ = %#v", got)
	}

	got, off := completePathToken("--out=al")
	if !reflect.DeepEqual(completionStrings(got), []string{"pha.txt "}) {
		t.Errorf("--out=al candidates = %#v", completionStrings(got))
	}
	if off != len([]rune("--out=al")) {
		t.Errorf("--out=al offset = %d, want %d", off, len([]rune("--out=al")))
	}
}

func TestPathCompletionHome(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, "Documents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "notes.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	if got := completionStrings(pathSuffixes("~")); !reflect.DeepEqual(got, []string{"/Documents/", "/notes.txt "}) {
		t.Errorf("~ = %#v", got)
	}
	if got := completionStrings(pathSuffixes("~/Doc")); !reflect.DeepEqual(got, []string{"uments/"}) {
		t.Errorf("~/Doc = %#v", got)
	}
}

func TestTokenStart(t *testing.T) {
	tests := []struct {
		line string
		want int
	}{
		{"ls -la /et", 7},
		{"cd src/fo", 3},
		{"echo 'a b", 5},
		{"echo a\\ b", 5},
		{"echo x | gr", 9},
	}
	for _, tc := range tests {
		if got := tokenStart([]rune(tc.line), len([]rune(tc.line))); got != tc.want {
			t.Errorf("tokenStart(%q) = %d, want %d", tc.line, got, tc.want)
		}
	}
}

func TestShellCompleterRoutesCommandsAndPaths(t *testing.T) {
	setupCompletionDir(t)
	c := &shellCompleter{commands: readline.NewPrefixCompleter(
		readline.PcItem("git"), readline.PcItem("grep"))}

	got, _ := c.Do([]rune("gi"), 2)
	if !reflect.DeepEqual(completionStrings(got), []string{"t "}) {
		t.Errorf("command completion = %#v", completionStrings(got))
	}

	got, _ = c.Do([]rune("cat al"), 6)
	if !reflect.DeepEqual(completionStrings(got), []string{"pha.txt "}) {
		t.Errorf("path completion = %#v", completionStrings(got))
	}

	got, _ = c.Do([]rune("./al"), 4)
	if !reflect.DeepEqual(completionStrings(got), []string{"pha.txt "}) {
		t.Errorf("first-token path completion = %#v", completionStrings(got))
	}

	if got, _ := c.Do([]rune("cat al"), 3); got != nil {
		t.Errorf("mid-line completion should be ignored, got %#v", completionStrings(got))
	}
}
