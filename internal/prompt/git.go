// Git state detection for the prompt (fast, cached, never blocks long).
package prompt

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// GitInfo is the branch/dirty state shown in the prompt.
type GitInfo struct {
	Branch   string
	Detached bool
	Dirty    bool
}

type gitCacheEntry struct {
	info GitInfo
	at   time.Time
}

var (
	gitMu    sync.Mutex
	gitCache = map[string]gitCacheEntry{}
)

const gitTTL = 500 * time.Millisecond

// GitFor returns git info for dir (empty when not in a work tree).
// Results are cached briefly so per-prompt cost stays low.
func GitFor(dir string) GitInfo {
	root := repoRoot(dir)
	if root == "" {
		return GitInfo{}
	}
	gitMu.Lock()
	if e, ok := gitCache[root]; ok && time.Since(e.at) < gitTTL {
		gitMu.Unlock()
		return e.info
	}
	gitMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	info := GitInfo{}
	if out, err := run(ctx, root, "git", "symbolic-ref", "--short", "-q", "HEAD"); err == nil {
		info.Branch = strings.TrimSpace(out)
	}
	if info.Branch == "" {
		if out, err := run(ctx, root, "git", "rev-parse", "--short", "HEAD"); err == nil {
			info.Branch = strings.TrimSpace(out)
			info.Detached = info.Branch != ""
		}
	}
	if out, err := run(ctx, root, "git", "status", "--porcelain"); err == nil {
		info.Dirty = strings.TrimSpace(out) != ""
	}

	gitMu.Lock()
	gitCache[root] = gitCacheEntry{info: info, at: time.Now()}
	gitMu.Unlock()
	return info
}

func run(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}

// repoRoot walks up from dir looking for .git; "" when none.
func repoRoot(dir string) string {
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
