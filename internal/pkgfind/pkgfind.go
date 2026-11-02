// Package pkgfind answers "what is this package and how would I install it
// on this machine?" using local package managers as the oracle.
package pkgfind

import (
	"context"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

// Resolve probes the local ecosystem for name and returns a short report
// suitable for feeding back to the model.
func Resolve(ctx context.Context, name string) string {
	name = strings.TrimSpace(name)
	if !nameRe.MatchString(name) {
		return "error: invalid package name " + "`" + name + "`"
	}
	if found, detail := dnf(ctx, name); found {
		return "`" + name + "`: found in dnf — " + detail
	}
	if found, detail := flatpak(ctx, name); found {
		return "`" + name + "`: found in flatpak — " + detail
	}
	return "`" + name + "`: not found in dnf or flatpak. " +
		"It may be a Python CLI (try `pipx install " + name + "`), a Rust crate " +
		"(`cargo install " + name + "`), or an npm package. Do not assume pip for " +
		"system packages; if this is a system tool, tell the user it was not found."
}

// dnf checks installed + available packages (metadata cache makes this fast).
func dnf(ctx context.Context, name string) (bool, string) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "dnf", "-q", "info", name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, ""
	}
	text := string(out)
	fields := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			key := strings.TrimSpace(k)
			val := strings.TrimSpace(v)
			if key != "" && val != "" {
				fields[key] = val
			}
		}
	}
	pkg := fields["Name"]
	ver := fields["Version"]
	if pkg == "" {
		return false, ""
	}
	state := "available"
	if strings.Contains(text, "Installed Packages") {
		state = "installed"
	}
	detail := pkg
	if ver != "" {
		detail += " " + ver
	}
	if repo := fields["Repository"]; repo != "" {
		detail += " (" + repo + ")"
	}
	detail += ", " + state + ". Install with: sudo dnf install " + pkg
	return true, detail
}

// flatpak searches the appstream cache; skipped if flatpak is absent.
func flatpak(ctx context.Context, name string) (bool, string) {
	if _, err := exec.LookPath("flatpak"); err != nil {
		return false, ""
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "flatpak", "search", "--columns=application,summary", name).
		CombinedOutput()
	if err != nil {
		return false, ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 1 && (lines[0] == "" || !strings.Contains(strings.ToLower(lines[0]), strings.ToLower(name))) {
		return false, ""
	}
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return false, ""
	}
	detail := describe(lines[0])
	if detail == "" {
		return false, ""
	}
	return true, detail + ", install with: flatpak install " + detail
}

func describe(line string) string {
	f := strings.Fields(line)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}
