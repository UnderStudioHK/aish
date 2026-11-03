// Package doctor reports install health for aish.
package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/UnderStudioHK/aish/internal/config"
	"github.com/UnderStudioHK/aish/internal/llm"
)

func Run(cfg *config.Config) int {
	fail := 0
	prefix := func(ok bool) string {
		if ok {
			return "ok  "
		}
		fail++
		return "FAIL"
	}
	report := func(ok bool, format string, a ...any) {
		fmt.Printf("[%s] %s\n", prefix(ok), fmt.Sprintf(format, a...))
	}

	fmt.Println("aish doctor")
	fmt.Printf("  binary   : %s\n", os.Args[0])
	fmt.Printf("  state dir: %s\n", cfg.StateDir)

	// Model file
	info, err := os.Stat(cfg.ModelPath)
	modelOK := err == nil && info.Size() > 1_000_000
	if modelOK {
		f, ferr := os.Open(cfg.ModelPath)
		magic := make([]byte, 4)
		if ferr == nil {
			_, _ = f.Read(magic)
			f.Close()
		}
		modelOK = string(magic) == "GGUF"
	}
	if modelOK {
		report(true, "model: %s (%.1f GB)", cfg.ModelPath, float64(info.Size())/1e9)
	} else {
		report(false, "model: %s missing or not GGUF", cfg.ModelPath)
	}

	// llama-server binary
	if _, err := exec.LookPath(cfg.LLMBin); err != nil {
		if _, serr := os.Stat(cfg.LLMBin); serr != nil {
			report(false, "llama-server: not found at %s", cfg.LLMBin)
		} else {
			report(true, "llama-server: %s", cfg.LLMBin)
		}
	} else {
		report(true, "llama-server: %s", cfg.LLMBin)
	}

	// Prompt
	if _, err := os.Stat(cfg.PromptFile); err == nil {
		report(true, "prompt: %s", cfg.PromptFile)
	} else {
		report(true, "prompt: built-in default (no %s)", cfg.PromptFile)
	}

	// Lua skills
	if cfg.LuaEnabled {
		n := 0
		if entries, err := os.ReadDir(cfg.SkillsDir); err == nil {
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".lua") {
					n++
				}
			}
		}
		report(true, "lua: enabled (%s, %d saved skill(s), timeout %s)", cfg.SkillsDir, n, cfg.LuaTimeout)
	} else {
		report(true, "lua: disabled (AISH_LUA=0)")
	}

	// Endpoint
	c := llm.New(cfg.HubURL, "", 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if c.Healthy(ctx) {
		report(true, "server: %s healthy", cfg.HubURL)
	} else {
		report(true, "server: %s down (aish will auto-start it on first intent)", cfg.HubURL)
	}

	fmt.Println("  knobs:")
	fmt.Printf("    AISH_MAX_STEPS=%d  AISH_HISTORY_TURNS=%d  AISH_CONFIRM=%s\n",
		cfg.MaxSteps, cfg.HistoryTurns, cfg.Confirm)
	fmt.Printf("    AISH_LLM_CTX=%d  AISH_LLM_PORT=%d  AISH_KEEP_ALIVE=%v\n",
		cfg.LLMCtx, cfg.LLMPort, cfg.KeepAlive)

	if fail > 0 {
		fmt.Printf("\n%d check(s) failed\n", fail)
		return 1
	}
	fmt.Println("\nall core checks passed")
	return 0
}
