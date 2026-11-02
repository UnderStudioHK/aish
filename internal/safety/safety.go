// Package safety classifies LLM-proposed commands and gates execution.
package safety

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"golang.org/x/term"
)

// Level of risk per command.
type Level int

const (
	Safe Level = iota
	Confirm      // y/enter-style confirmation
	Never        // destructive: must type "yes"
)

// Patterns are conservative on purpose.
var (
	reNever = regexp.MustCompile(
		`(?i)(?:^|\s)(?:sudo\s+rm|dd\s|mkfs|shred\b|wipefs|reboot\b|shutdown\b|halt\b|poweroff\b)` +
			`|(?i)(?:^|\s)rm\s+(?:-[a-z]*[rp]|-[a-z]*f)` +
			`|(?i)\bgit\s+(?:push\s+(?:--force|-\S*f\b|-\S*\s*--)|reset\s+--hard|clean\s+-[a-z]*[df])` +
			`|(?i)\btruncate\s+-s\s*0\b|(?i)>\s*/dev/|(?i)\bchmod\s+-r?\s*7[67]{2}\b|(?i)\bchown\s+-r\b`)
	reConfirm = regexp.MustCompile(
		`(?i)(?:^|\s)(?:sudo|doas)\b` +
			`|(?i)(?:^|\s)rm\s` + // plain rm: confirm, not silent auto-run
			`|(?i)\b(?:dnf|yum|zypper|pacman)\s+(?:install|remove|erase|downgrade|reinstall|upgrade|-y)` +
			`|(?i)\bapt(?:-get)?\s+(?:install|remove|purge|upgrade|full-upgrade|-y)` +
			`|(?i)\b(?:flatpak|snap)\s+(?:install|uninstall|remove)` +
			`|(?i)\b(?:kill|pkill|killall)\b|(?i)\bchmod\b|(?i)\bchown\b|(?i)\bchattr\b|(?i)\bsetfacl\b` +
			`|(?i)\bgit\s+(?:push|reset|clean|revert|restore|checkout)\b` +
			`|(?i)\b(?:podman|docker)\s+(?:rm|rmi|stop|kill|prune|system\s+prune)` +
			`|(?i)\bsystemctl\s+(?:stop|restart|disable|mask|enable)` +
			`|(?i)\b(?:mkfs|fsck|mount|umount|crontab)\b` +
			`|(?i)\buser(?:add|del|mod)\b|(?i)\bgroup(?:add|del|mod)\b` +
			`|(?i)\bnmcli\b`)
)

func (lv Level) String() string {
	switch lv {
	case Confirm:
		return "confirm"
	case Never:
		return "destructive"
	default:
		return "safe"
	}
}

type Classifier struct{}

func (Classifier) Classify(cmd string) Level {
	lo := strings.ToLower(cmd)
	switch {
	case reNever.MatchString(lo):
		return Never
	case reConfirm.MatchString(lo):
		return Confirm
	}
	return Safe
}

// Policy combines the classifier with the configured confirm mode.
type Policy struct {
	Mode  string // "safe-auto" | "always" | "never"
	Class Classifier
}

func (p Policy) Classify(cmd string) Level { return p.Class.Classify(cmd) }

func NewPolicy(mode string) Policy {
	if mode == "" {
		mode = "safe-auto"
	}
	return Policy{Mode: mode}
}

// Ask prompts on stderr and reads one line. Returns the approved
// command (possibly edited) and whether to run.
func Ask(cmd string, lv Level) (string, bool) {
	desc, _ := term.GetState(int(os.Stdin.Fd()))
	defer term.Restore(int(os.Stdin.Fd()), desc)

	diff := "[y/N]"
	if lv == Never {
		diff = "type 'yes'"
	}
	fmt.Fprintf(os.Stderr, "\n\x1b[33m%s\x1b[0m\n  run (%s, n=cancel, e=edit)? ", cmd, diff)
	sc := bufio.NewReader(os.Stdin)
	line, err := sc.ReadString('\n')
	if err != nil {
		return "", false
	}
	ans := strings.TrimSpace(line)
	switch {
	case lv != Never && strings.EqualFold(ans, "y"):
		return cmd, true
	case strings.EqualFold(ans, "yes"):
		return cmd, true
	case strings.EqualFold(ans, "edit") || strings.EqualFold(ans, "e"):
		return Edited(cmd)
	case strings.EqualFold(ans, "no") || strings.EqualFold(ans, "n"):
		return "", false
	default:
		return "", false
	}
}

// Edited opens $EDITOR on the command; returns new command, ok.
func Edited(cmd string) (string, bool) {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	f, err := os.CreateTemp("", "aish-edit-*")
	if err != nil {
		return "", false
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err := f.WriteString(cmd + "\n"); err != nil {
		f.Close()
		return "", false
	}
	f.Close()

	c := exec.Command(editor, name)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stderr, os.Stderr
	if err := c.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "edit: %v\n", err)
		return "", false
	}
	b, err := os.ReadFile(name)
	if err != nil {
		return "", false
	}
	out := strings.TrimSpace(string(b))
	if out == "" || out == cmd {
		return "", false
	}
	return out, true
}
