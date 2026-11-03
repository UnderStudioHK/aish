// Package llm talks to the llama.cpp OpenAI-compatible server and
// resolves the system prompt.
package llm

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

//go:embed default-prompt.md
var defaultPrompt string

// Sanitize hides special-token scaffolding leaked into model content
// (e.g. <|tool_call_start|> when a model tool-calls outside tool mode).
func Sanitize(c string) string {
	r := strings.NewReplacer("<|tool_call_start|>", "", "<|tool_call_end|>", "",
		"<|im_start|>", "", "<|im_end|>", "", "<|startoftext|>", "", "<|endoftext|>", "")
	return strings.TrimSpace(r.Replace(c))
}

// ResolvePrompt returns the byte-identical prompt shown to the model.
// A user file replaces the whole persona (safety stays client-side).
func ResolvePrompt(path string) string {
	b, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(b)) == "" {
		return strings.TrimSpace(defaultPrompt)
	}
	return strings.TrimSpace(string(b))
}

// Client is a thin chat-completions client.
type Client struct {
	BaseURL string
	Token   string
	Prompt  string
	http    *http.Client
}

func New(baseURL, systemPrompt string, timeout time.Duration) *Client {
	return &Client{
		BaseURL: baseURL,
		Prompt:  systemPrompt,
		http:    &http.Client{Timeout: timeout},
	}
}

// ToolCall describes one requested function invocation.
type ToolCall struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Func struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

// Message is a chat message (role/content/tool_calls/tool ids).
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// Option passed to run_command tool.
type CommandArg struct {
	Command string `json:"command"`
}

const toolDescription = "Run a command through /bin/sh on this machine. " +
	"The full output (truncated) and exit code come back to you next turn."

var findPackageTool = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name": "find_package",
		"description": "Look up which local package ecosystem provides a piece of " +
			"software (checks dnf and flatpak). Call this BEFORE any install " +
			"command so you know whether it is a system package, a flatpak, or " +
			"something else.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string"},
			},
			"required": []string{"name"},
		},
	},
}

var runCommandTool = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name":        "run_command",
		"description": toolDescription,
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string"},
			},
			"required": []string{"command"},
		},
	},
}

var proposeLuaTool = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name": "propose_lua",
		"description": "Define a temporary Lua tool for this session. Use this only " +
			"when a small reusable helper makes a task more reliable than one-off " +
			"commands. The code is a Lua function body; it receives one table " +
			"argument named `args` whose keys match your parameters schema (use " +
			"args.file, not positional args[1]). Return a string or a table. " +
			"Example for a word-count tool: parameters = " +
			`{"type":"object","properties":{"file":{"type":"string"}},"required":["file"]}` +
			"; code = \"local r = aish.exec({'wc','-w',args.file}); return r.stdout\". " +
			"Host API: aish.exec({'wc','-w',args.file}) -> {code,stdout,stderr}; " +
			"the first argv element is the program itself, never 'cmd' or 'sh'. " +
			"aish.print, aish.log, aish.cwd, aish.env_get, aish.json_encode, " +
			"aish.json_decode. There is no io/os/require; use aish.exec for file " +
			"and system access. The tool is registered as lua_<name> for this " +
			"session and can be called on your next turn. To use it immediately, " +
			"pass `args` with the exact shape your parameters schema declares; " +
			"aish runs it and returns the result in this tool message. Never run " +
			"`aish.exec(...)` through run_command; `aish.exec` exists only inside " +
			"Lua. The skill is not saved unless the user runs :skill save <name>.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":        map[string]any{"type": "string", "description": "lowercase identifier, max 32 chars"},
				"description": map[string]any{"type": "string", "description": "what the tool does"},
				"parameters":  map[string]any{"type": "object", "description": "JSON schema for args"},
				"code":        map[string]any{"type": "string", "description": "Lua function body over args"},
				"args":        map[string]any{"type": "object", "description": "optional arguments to run immediately after registering"},
				"persist":     map[string]any{"type": "boolean", "description": "suggest saving after use; user must approve"},
			},
			"required": []string{"name", "description", "parameters", "code"},
		},
	},
}

var runLuaTool = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name": "run_lua",
		"description": "Run a registered Lua skill by name. After propose_lua " +
			"registers a skill, call this with {\"name\":\"<skill>\",\"args\":{...}}. " +
			"Use this instead of run_command for skills; aish.exec is a Lua API, " +
			"not a shell command.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "description": "registered skill name"},
				"args": map[string]any{"type": "object", "description": "arguments matching the skill schema"},
			},
			"required": []string{"name"},
		},
	},
}

// RunLuaTool is the stable entry point for using a registered Lua skill.
func RunLuaTool() map[string]any { return runLuaTool }

// BaseTools returns the tools that are always available. Callers append
// session-specific skills and the Lua proposal tool when enabled.
func BaseTools() []map[string]any {
	return []map[string]any{runCommandTool, findPackageTool}
}

// ProposeLuaTool is the meta-tool that lets the model author a session skill.
func ProposeLuaTool() map[string]any { return proposeLuaTool }

type chatRequest struct {
	Model         string           `json:"model"`
	Messages      []Message        `json:"messages"`
	Tools         []map[string]any `json:"tools"`
	ToolChoice    string           `json:"tool_choice,omitempty"`
	Temperature   float64          `json:"temperature,omitempty"`
	Stream        bool             `json:"stream"`
	ParallelTools bool             `json:"parallel_tool_calls,omitempty"`
}

type chatChoice struct {
	Message       Message `json:"message"`
	FinishRreason string  `json:"finish_reason"`
}

type chatResponse struct {
	Choices []chatChoice    `json:"choices"`
	Error   json.RawMessage `json:"error,omitempty"`
}

// Turn calls the server once. An empty tools slice omits the tool schema
// (answer in prose only). sysExtra, when non-empty, is appended as a second
// system message after the static prompt (keeps the cached prompt prefix
// intact).
func (c *Client) Turn(ctx context.Context, history []Message, tools []map[string]any, sysExtra string) (Message, error) {
	msgs := make([]Message, 0, len(history)+2)
	msgs = append(msgs, Message{Role: "system", Content: c.Prompt})
	if sysExtra != "" {
		msgs = append(msgs, Message{Role: "system", Content: sysExtra})
	}
	msgs = append(msgs, history...)

	req := chatRequest{
		Model:       "lfm2.5-24b-a2b",
		Messages:    msgs,
		Temperature: 0.1,
	}
	if len(tools) > 0 {
		req.Tools = tools
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Message{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Message{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return Message{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Message{}, fmt.Errorf("llm http %d: %s", resp.StatusCode, b)
	}
	var cr chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return Message{}, err
	}
	if len(cr.Error) > 0 {
		return Message{}, fmt.Errorf("llm error: %s", cr.Error)
	}
	if len(cr.Choices) == 0 {
		return Message{}, fmt.Errorf("llm: no choices")
	}
	return cr.Choices[0].Message, nil
}

// DecodeArgs decodes tool arguments, tolerating models that return a
// double-encoded JSON string instead of an object.
func DecodeArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return fmt.Errorf("empty arguments")
	}
	if err := json.Unmarshal(raw, v); err == nil {
		return nil
	}
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err == nil {
		if err := json.Unmarshal([]byte(encoded), v); err == nil {
			return nil
		}
	}
	// Last resort: strip backslashes from escaped-but-unquoted JSON.
	clean := strings.ReplaceAll(string(raw), `\"`, `"`)
	return json.Unmarshal([]byte(clean), v)
}

// PackageOf extracts the package name from a find_package tool call,
// tolerating JSON, escaped JSON, and pythonic `name="x"` forms.
func PackageOf(tc ToolCall) (string, bool) {
	var a struct {
		Name string `json:"name"`
	}
	raw := strings.Map(func(r rune) rune {
		if r == '\\' {
			return -1
		}
		return r
	}, string(tc.Func.Arguments))
	if err := json.Unmarshal([]byte(raw), &a); err == nil && a.Name != "" {
		return a.Name, true
	}
	// pythonic / malformed: name="ripgrep" or name: ripgrep
	if m := rePackageName.FindStringSubmatch(raw); len(m) == 2 && m[1] != "" {
		return m[1], true
	}
	if t := strings.Trim(raw, `"' {}`); t != "" && !strings.ContainsAny(t, "{}:") {
		return strings.TrimPrefix(t, "name="), true
	}
	return "", false
}

var rePackageName = regexp.MustCompile(`name["']?\s*[:=]\s*["']?([A-Za-z0-9][A-Za-z0-9._+-]*)`)

// CommandOf extracts the command from a run_command tool call, tolerating
// the mildly malformed arguments small/quantized models emit.
func CommandOf(tc ToolCall) (string, bool) {
	raw := []byte(tc.Func.Arguments)
	var a CommandArg
	// 1) well-formed arguments object
	if err := json.Unmarshal(raw, &a); err == nil && a.Command != "" {
		return a.Command, true
	}
	// 2) arguments double-encoded (object as a JSON string)
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" {
		if err := json.Unmarshal([]byte(s), &a); err == nil && a.Command != "" {
			return a.Command, true
		}
	}
	// 3) escape artifacts: strip backslashes, then retry
	cleaned := strings.Map(func(r rune) rune {
		if r == '\\' {
			return -1
		}
		return r
	}, string(raw))
	cleaned = strings.Trim(cleaned, `"`)
	if err := json.Unmarshal([]byte(cleaned), &a); err == nil && a.Command != "" {
		return a.Command, true
	}
	// 4) bare pythonic tail: `command="ls -la"` or a plain string
	if t := strings.TrimSpace(string(raw)); t != "" && !strings.ContainsAny(t, "{}") {
		if v, ok := strings.CutPrefix(t, `command=`); ok {
			return strings.Trim(v, `"'`), true
		}
		return strings.Trim(t, `"'`), true
	}
	return "", false
}

// Healthy reports whether the server answers.
func (c *Client) Healthy(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/health", nil)
	if err != nil {
		return false
	}
	quick := &http.Client{Timeout: 3 * time.Second}
	resp, err := quick.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}
