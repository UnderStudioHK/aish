package repl

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/UnderStudioHK/aish/internal/config"
	"github.com/UnderStudioHK/aish/internal/llm"
	"github.com/UnderStudioHK/aish/internal/luatools"
)

func toolCall(name, args string) llm.ToolCall {
	var tc llm.ToolCall
	tc.ID = "call-" + name
	tc.Type = "function"
	tc.Func.Name = name
	tc.Func.Arguments = json.RawMessage(args)
	return tc
}

func TestProposeAndCallLuaTool(t *testing.T) {
	r := &Repl{
		cfg:      &config.Config{LuaTimeout: time.Second},
		luaTools: luatools.NewManager(t.TempDir(), time.Second, 4, 64*1024, nil),
	}
	defer r.luaTools.Close()

	proposal := `{"name":"hello","description":"say hi","code":"return \"hi\"","parameters":{"type":"object","properties":{}}}`
	r.proposeLua(toolCall("propose_lua", proposal))

	list := r.luaTools.List()
	if len(list) != 1 || list[0].Name != "hello" {
		t.Fatalf("tools = %+v", list)
	}
	if got := r.luaTools.ToolSchemas(); len(got) != 1 {
		t.Fatalf("schemas = %v", got)
	}

	r.callLua(toolCall("lua_hello", `{}`))
	if len(r.talk) != 2 {
		t.Fatalf("talk = %+v", r.talk)
	}
	if got := r.talk[1].Content; got != "hi" {
		t.Errorf("tool result = %q, want hi", got)
	}

	r.runLua(toolCall("run_lua", `{"name":"hello","args":{}}`))
	if got := r.talk[len(r.talk)-1].Content; got != "hi" {
		t.Errorf("run_lua result = %q, want hi", got)
	}
}

func TestProposeLuaImmediateArgs(t *testing.T) {
	r := &Repl{
		cfg:      &config.Config{LuaTimeout: time.Second},
		luaTools: luatools.NewManager(t.TempDir(), time.Second, 4, 64*1024, nil),
	}
	defer r.luaTools.Close()

	proposal := `{"name":"echoarg","description":"echo arg","parameters":{"type":"object","properties":{"x":{"type":"string"}}},"code":"return args.x","args":{"x":"now"}}`
	r.proposeLua(toolCall("propose_lua", proposal))
	if len(r.talk) != 1 {
		t.Fatalf("talk = %+v", r.talk)
	}
	if got := r.talk[0].Content; !strings.Contains(got, "immediate result:\nnow") {
		t.Errorf("proposal result = %q", got)
	}
}

func TestBatchCommandsIgnoresLuaTools(t *testing.T) {
	calls := []llm.ToolCall{
		toolCall("run_command", `{"command":"ls"}`),
		toolCall("lua_echo", `{"command":"rm -rf /"}`),
		toolCall("propose_lua", `{"name":"x","code":"return 1","description":"x"}`),
	}
	got := batchCommands(calls)
	if len(got) != 1 || got[0] != "ls" {
		t.Fatalf("batchCommands = %v", got)
	}
	if desc := describeCalls(calls[2:]); desc != "propose_lua x" {
		t.Errorf("describeCalls = %q", desc)
	}
	if desc := describeCalls(calls[1:2]); desc != "lua_echo" {
		t.Errorf("describeCalls = %q", desc)
	}
}
