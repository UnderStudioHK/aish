// Package config resolves all aish knobs.
// Precedence: flag > env > optional TOML (TODO M4) > built-in default.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type Config struct {
	// Loop parameters
	MaxSteps     int
	HistoryTurns int

	// Timeouts
	CmdTimeout time.Duration
	LLMTimeout time.Duration

	// Safety + presentation
	Confirm  string // "safe-auto" | "always" | "never"
	Narrate  bool   // print the model's explanation after tool runs
	FollowUp string // "eager" (background) | "lazy" (Enter to continue) | "off"

	// llama-server
	LLMHost       string
	LLMPort       int
	LLMCtx        int
	LLMBin        string
	ModelPath     string
	KeepAlive     bool
	ServerLogPath string

	// State
	PromptFile  string
	StateDir    string
	HistoryPath string

	// Base URL for the OpenAI-compatible API
	HubURL string
}

func home(p string) string {
	if p == "" {
		return ""
	}
	if p[0] == '~' {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		p = filepath.Join(home, p[1:])
	}
	return p
}

func Load() (*Config, error) {
	c := &Config{
		MaxSteps:     envInt("AISH_MAX_STEPS", 5),
		HistoryTurns: envInt("AISH_HISTORY_TURNS", 5),
		CmdTimeout:   envDur("AISH_CMD_TIMEOUT", 2*time.Hour),
		LLMTimeout:   envDur("AISH_LLM_TIMEOUT", 2*time.Minute),
		Confirm:      envStr("AISH_CONFIRM", "safe-auto"),
		Narrate:      envBool("AISH_NARRATE", false),
		FollowUp:     envStr("AISH_FOLLOWUP", "eager"),
		LLMHost:      envStr("AISH_LLM_HOST", "127.0.0.1"),
		LLMPort:      envInt("AISH_LLM_PORT", 8081),
		LLMCtx:       envInt("AISH_LLM_CTX", 32768),
		LLMBin:       home(envStr("AISH_LLM_BIN", "~/tools/llama-b12034/llama-server")),
		ModelPath:    home(envStr("AISH_MODEL", "~/models/LFM2.5-24B-A2B-QAD-Q4_0.gguf")),
		KeepAlive:    envBool("AISH_KEEP_ALIVE", false),
		PromptFile:   home(envStr("AISH_PROMPT_FILE", "~/.config/aish/prompt.md")),
		StateDir:     home(envStr("AISH_STATE_DIR", "~/.local/state/aish")),
	}
	c.HistoryPath = filepath.Join(c.StateDir, "history")
	c.ServerLogPath = filepath.Join(c.StateDir, "llama-server.log")
	c.HubURL = fmt.Sprintf("http://%s:%d", c.LLMHost, c.LLMPort)

	switch c.Confirm {
	case "safe-auto", "always", "never":
	default:
		return nil, fmt.Errorf("AISH_CONFIRM=%q: want safe-auto|always|never", c.Confirm)
	}
	switch c.FollowUp {
	case "eager", "lazy", "off":
	default:
		return nil, fmt.Errorf("AISH_FOLLOWUP=%q: want eager|lazy|off", c.FollowUp)
	}
	return c, nil
}

func (c *Config) Enabled() bool { return c.MaxSteps > 0 }

func (c *Config) HubBase() string { return c.HubURL }

func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
		fmt.Fprintf(os.Stderr, "aish: warn: %s=%q is not a non-negative int; using default %d\n", key, v, def)
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		switch v {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}
