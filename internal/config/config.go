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

	// Prompt (display)
	PromptStyle string // "full" | "minimal"
	PromptTmpl  string // optional template file

	// State
	PromptFile  string
	StateDir    string
	HistoryPath string

	// Base URL for the OpenAI-compatible API
	HubURL string
}

// dataHome is where models/llama.cpp live: AISH_HOME, else XDG_DATA_HOME/aish.
func dataHome() string {
	if v := os.Getenv("AISH_HOME"); v != "" {
		return home(v)
	}
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "aish")
	}
	return home(filepath.Join("~", ".local", "share", "aish"))
}

func stateHome() string {
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return v
	}
	return home(filepath.Join("~", ".local", "state"))
}

func dataPath(parts ...string) string {
	return filepath.Join(append([]string{dataHome()}, parts...)...)
}

// envPath returns the env override if set, else the first existing legacy
// path among candidates, else the primary default.
func envPath(key, primary string, legacy ...string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return home(v)
	}
	if _, err := os.Stat(primary); err == nil {
		return primary
	}
	for _, p := range legacy {
		if _, err := os.Stat(home(p)); err == nil {
			return home(p)
		}
	}
	return primary
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
		LLMBin:       envPath("AISH_LLM_BIN", dataPath("llama.cpp", "llama-server"), "~/tools/llama-b12034/llama-server"),
		ModelPath:    envPath("AISH_MODEL", dataPath("models", "LFM3-1.7B-QAD-Q4_0.gguf"), "~/models/LFM3-1.7B-QAD-Q4_0.gguf", "~/models/LFM2.5-2.6B-Q4_0.gguf"),
		KeepAlive:    envBool("AISH_KEEP_ALIVE", false),
		PromptFile:   home(envStr("AISH_PROMPT_FILE", "~/.config/aish/prompt.md")),
		PromptStyle:  envStr("AISH_PROMPT_STYLE", "full"),
		PromptTmpl:   home(envStr("AISH_PROMPT_TMPL", "~/.config/aish/prompt.tmpl")),
		StateDir:     home(envStr("AISH_STATE_DIR", filepath.Join(stateHome(), "aish"))),
	}
	c.HistoryPath = filepath.Join(c.StateDir, "history")
	c.ServerLogPath = filepath.Join(c.StateDir, "llama-server.log")
	c.HubURL = fmt.Sprintf("http://%s:%d", c.LLMHost, c.LLMPort)

	switch c.Confirm {
	case "safe-auto", "always", "never":
	default:
		return nil, fmt.Errorf("AISH_CONFIRM=%q: want safe-auto|always|never", c.Confirm)
	}
	switch c.PromptStyle {
	case "full", "minimal":
	default:
		return nil, fmt.Errorf("AISH_PROMPT_STYLE=%q: want full|minimal", c.PromptStyle)
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
