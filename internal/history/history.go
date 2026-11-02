// Package history persists and loads REPL history.
package history

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const maxLines = 10000

type Store struct {
	path string
}

func New(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("history: %w", err)
	}
	return &Store{path: path}, nil
}

// Append records one line, ignoring multi-line input.
func (s *Store) Append(line string) {
	line = strings.TrimSpace(line)
	if line == "" || strings.ContainsRune(line, '\n') {
		return
	}
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}

// Last returns up to n most recent entries, oldest first.
func (s *Store) Last(n int) []string {
	f, err := os.Open(s.path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var all []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		all = append(all, sc.Text())
	}
	if n <= 0 || n >= len(all) {
		return all
	}
	return all[len(all)-n:]
}
