package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeHost struct {
	argv [][]string
	out  string
}

func (f *fakeHost) Exec(_ context.Context, argv []string) (ExecResult, error) {
	f.argv = append(f.argv, append([]string(nil), argv...))
	return ExecResult{Code: 0, Stdout: f.out}, nil
}
func (f *fakeHost) Print(string)      {}
func (f *fakeHost) Log(string)        {}
func (f *fakeHost) Cwd() string       { return "/tmp" }
func (f *fakeHost) Env(string) string { return "" }

func writeSkill(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name+".lua")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadCallAndSchema(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "echoer", `
return {
  name = "echoer",
  description = "echo a value",
  parameters = { type = "object", properties = { x = { type = "string" } }, required = { "x" } },
  run = function(args)
    local r = aish.exec({"echo", args.x})
    return r.stdout
  end,
}`)
	host := &fakeHost{out: "hello\n"}
	m := NewManager(dir, time.Second, 4, 64*1024, host)
	defer m.Close()
	if errs := m.LoadDir(); len(errs) != 0 {
		t.Fatalf("LoadDir: %v", errs)
	}
	list := m.List()
	if len(list) != 1 || list[0].Name != "echoer" || !list[0].Persistent {
		t.Fatalf("List = %+v", list)
	}
	schemas := m.ToolSchemas()
	if len(schemas) != 1 {
		t.Fatalf("schemas = %v", schemas)
	}
	fn := schemas[0]["function"].(map[string]any)
	if fn["name"] != "lua_echoer" {
		t.Errorf("tool name = %v", fn["name"])
	}

	out, err := m.Call(context.Background(), "lua_echoer", map[string]any{"x": "world"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "hello\n" {
		t.Errorf("out = %q", out)
	}
	if len(host.argv) != 1 || strings.Join(host.argv[0], " ") != "echo world" {
		t.Errorf("argv = %v", host.argv)
	}
}

func TestProposeSaveReload(t *testing.T) {
	dir := t.TempDir()
	host := &fakeHost{out: "made\n"}
	m := NewManager(dir, time.Second, 4, 64*1024, host)
	info, err := m.Propose("maker", "make a value", `local r = aish.exec({"printf", "made"})
return r.stdout`, map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "maker" || info.Persistent {
		t.Fatalf("info = %+v", info)
	}
	if _, err := m.Call(context.Background(), "lua_maker", nil); err != nil {
		t.Fatal(err)
	}
	path, err := m.Save("maker")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}

	// A fresh manager must load the saved file.
	m2 := NewManager(dir, time.Second, 4, 64*1024, host)
	defer m2.Close()
	if errs := m2.LoadDir(); len(errs) != 0 {
		t.Fatalf("reload: %v", errs)
	}
	if _, err := m2.Call(context.Background(), "lua_maker", nil); err != nil {
		t.Fatal(err)
	}
}

func TestReloadAndRemove(t *testing.T) {
	dir := t.TempDir()
	path := writeSkill(t, dir, "ver", `
return { name = "ver", description = "v1", run = function(args) return "one" end }`)
	host := &fakeHost{}
	m := NewManager(dir, time.Second, 4, 64*1024, host)
	defer m.Close()
	if errs := m.LoadDir(); len(errs) != 0 {
		t.Fatal(errs)
	}
	out, _ := m.Call(context.Background(), "lua_ver", nil)
	if out != "one" {
		t.Fatalf("out = %q", out)
	}

	if err := os.WriteFile(path, []byte(`
return { name = "ver", description = "v2", run = function(args) return "two" end }`), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if errs := m.LoadDir(); len(errs) != 0 {
		t.Fatal(errs)
	}
	out, _ = m.Call(context.Background(), "lua_ver", nil)
	if out != "two" {
		t.Fatalf("after reload out = %q", out)
	}
	if err := m.Remove("ver"); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Source("ver"); ok {
		t.Error("skill still present after remove")
	}
}

func TestSandboxAndLimits(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "forbidden", `
return { name = "forbidden", run = function(args) return os.execute("true") end }`)
	writeSkill(t, dir, "slow", `
return { name = "slow", run = function(args) while true do end end }`)
	host := &fakeHost{}
	m := NewManager(dir, 100*time.Millisecond, 4, 64*1024, host)
	defer m.Close()
	if errs := m.LoadDir(); len(errs) != 0 {
		t.Fatal(errs)
	}
	if _, err := m.Call(context.Background(), "lua_forbidden", nil); err == nil {
		t.Error("os.execute should be unavailable")
	}
	start := time.Now()
	if _, err := m.Call(context.Background(), "lua_slow", nil); err == nil {
		t.Error("infinite loop should time out")
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("timeout took %v", time.Since(start))
	}
}

func TestNameAndCollisionRules(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, time.Second, 2, 64*1024, &fakeHost{})
	defer m.Close()

	for _, name := range []string{"run_command", "Bad-Name", ""} {
		if _, err := m.Propose(name, "", `return "x"`, nil); err == nil {
			t.Errorf("Propose(%q) should fail", name)
		}
	}
	if _, err := m.Propose("one", "", `return "1"`, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Propose("one", "", `return "2"`, nil); err == nil {
		t.Error("duplicate proposal should fail")
	}
	if _, err := m.Propose("two", "", `return "2"`, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Propose("three", "", `return "3"`, nil); err == nil {
		t.Error("session skill limit should fail")
	}
}

func TestFunctionNormalization(t *testing.T) {
	cases := []struct{ in, want string }{
		{"return function(args) return 1 end", "function(args) return 1 end"},
		{"function(args) return 1 end", "function(args) return 1 end"},
		{"return 1", "function(args)\nreturn 1\nend"},
	}
	for _, c := range cases {
		if got := normalizeFunc(c.in); got != c.want {
			t.Errorf("normalizeFunc(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeParams(t *testing.T) {
	got := normalizeParams(map[string]any{"file": map[string]any{"type": "string"}})
	if got["type"] != "object" {
		t.Errorf("type = %v", got["type"])
	}
	props, ok := got["properties"].(map[string]any)
	if !ok || props["file"] == nil {
		t.Errorf("properties = %#v", got["properties"])
	}
}

func TestPositionalArgsAlias(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "pos", `
return {
  name = "pos",
  description = "positional",
  parameters = { type = "object", properties = { file = { type = "string" } }, required = { "file" } },
  run = function(args) return args[1] end,
}`)
	host := &fakeHost{}
	m := NewManager(dir, time.Second, 4, 64*1024, host)
	defer m.Close()
	if errs := m.LoadDir(); len(errs) != 0 {
		t.Fatal(errs)
	}
	out, err := m.Call(context.Background(), "lua_pos", map[string]any{"file": "README.md"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "README.md" {
		t.Errorf("out = %q", out)
	}
}
