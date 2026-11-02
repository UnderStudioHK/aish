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
	"strings"
	"time"
)

//go:embed default-prompt.md
var defaultPrompt string

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

type chatRequest struct {
	Model         string               `json:"model"`
	Messages      []Message            `json:"messages"`
	Tools         []map[string]any     `json:"tools"`
	ToolChoice    string               `json:"tool_choice,omitempty"`
	Temperature   float64              `json:"temperature,omitempty"`
	Stream        bool                 `json:"stream"`
	ParallelTools bool                 `json:"parallel_tool_calls,omitempty"`
}

type chatChoice struct {
	Message      Message  `json:"message"`
	FinishRreason string `json:"finish_reason"`
}

type chatResponse struct {
	Choices []chatChoice    `json:"choices"`
	Error   json.RawMessage `json:"error,omitempty"`
}

// Turn calls the server once. tools=false omits the tool schema (answer
// in prose only). Returns the assistant message.
func (c *Client) Turn(ctx context.Context, history []Message, tools bool) (Message, error) {
	msgs := make([]Message, 0, len(history)+1)
	msgs = append(msgs, Message{Role: "system", Content: c.Prompt})
	msgs = append(msgs, history...)

	req := chatRequest{
		Model:       "lfm2.5-24b-a2b",
		Messages:    msgs,
		Temperature: 0.1,
	}
	if tools {
		req.Tools = []map[string]any{runCommandTool}
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

// CommandOf extracts the command from a run_command tool call.
func CommandOf(tc ToolCall) (string, bool) {
	var a CommandArg
	if err := json.Unmarshal(tc.Func.Arguments, &a); err != nil || a.Command == "" {
		// LFM sometimes emits pythonic args already decoded to a string.
		if s := strings.Trim(string(tc.Func.Arguments), "\""); !json.Valid([]byte(s)) && s != "" {
			return s, true
		}
		return "", false
	}
	return a.Command, true
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
