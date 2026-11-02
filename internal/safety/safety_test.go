package safety

import "testing"

func TestClassify(t *testing.T) {
	cases := map[string]Level{
		"ls -la /home":                  Safe,
		"grep -rn TODO .":               Safe,
		"echo hi > out.txt":             Safe,
		"git status":                    Safe,
		"dnf list installed | grep k":   Safe,
		"cat /proc/cpuinfo":             Safe,
		"sudo dnf install htop":         Confirm,
		"chmod 755 build.sh":            Confirm,
		"kill -9 12345":                 Confirm,
		"git push origin main":          Confirm,
		"podman rm myctr":               Confirm,
		"systemctl restart bluetooth":   Confirm,
		"rm notes.txt":                  Confirm,
		"rm -rf /tmp/x":                 Never,
		"sudo rm -rf /opt":              Never,
		"dd if=/dev/zero of=x bs=1M":    Never,
		"git push --force origin main":  Never,
		"shutdown now":                  Never,
		"truncate -s 0 bigfile":         Never,
	}
	for cmd, want := range cases {
		if got := (Classifier{}).Classify(cmd); got != want {
			t.Errorf("Classify(%q) = %s, want %s", cmd, got, want)
		}
	}
}

func TestPolicyMayAutoRun(t *testing.T) {
	p := NewPolicy("safe-auto")
	if !p.MayAutoRun("ls -la") {
		t.Error("safe should auto-run")
	}
	if p.MayAutoRun("kill -9 1") {
		t.Error("confirm-level must prompt")
	}
	if p.MayAutoRun("rm -rf /") {
		t.Error("destructive must always prompt")
	}
	if !NewPolicy("never").MayAutoRun("ls -la") {
		t.Error("never mode auto-runs safe")
	}
	if NewPolicy("never").MayAutoRun("rm -rf /") {
		t.Error("never mode still prompts destructive")
	}
}
