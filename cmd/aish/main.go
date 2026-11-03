// Command aish is the AI shell REPL.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/UnderStudioHK/aish/internal/config"
	"github.com/UnderStudioHK/aish/internal/doctor"
	"github.com/UnderStudioHK/aish/internal/execx"
	"github.com/UnderStudioHK/aish/internal/history"
	"github.com/UnderStudioHK/aish/internal/llm"
	"github.com/UnderStudioHK/aish/internal/repl"
	"github.com/UnderStudioHK/aish/internal/servermgmt"
)

var version = "0.1.0"

func main() {
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}

	// Flags override env/defaults.
	fs := flag.NewFlagSet("aish", flag.ExitOnError)
	fs.IntVar(&cfg.MaxSteps, "max-steps", cfg.MaxSteps, "agentic tool-call steps per request (0 disables the model)")
	fs.IntVar(&cfg.LLMPort, "port", cfg.LLMPort, "llama-server port")
	fs.IntVar(&cfg.LLMCtx, "ctx", cfg.LLMCtx, "llama-server context length")
	fs.StringVar(&cfg.ModelPath, "model", cfg.ModelPath, "GGUF model path")
	fs.StringVar(&cfg.Confirm, "confirm", cfg.Confirm, "safe-auto|always|never")
	showVersion := fs.Bool("version", false, "print version")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: aish [flags]\n       aish doctor\n\nflags:\n")
		fs.PrintDefaults()
	}
	if len(os.Args) > 1 && os.Args[1] == "doctor" {
		os.Exit(doctor.Run(cfg))
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		fatal(err)
	}
	if *showVersion {
		fmt.Println("aish " + version)
		return
	}
	// Re-derive dependent values from overridden fields.
	cfg.HubURL = fmt.Sprintf("http://%s:%d", cfg.LLMHost, cfg.LLMPort)

	h, err := history.New(cfg.HistoryPath)
	if err != nil {
		fatal(err)
	}
	client := llm.New(cfg.HubURL, llm.ResolvePrompt(cfg.PromptFile), cfg.LLMTimeout)
	srv := servermgmt.New(cfg)
	r := repl.New(cfg, execx.New(), h, client, srv)

	code, err := r.Run()
	srv.Stop()
	if err != nil {
		fatal(err)
	}
	os.Exit(code)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "aish: %v\n", err)
	os.Exit(1)
}
