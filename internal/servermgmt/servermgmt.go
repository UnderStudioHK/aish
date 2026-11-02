// Package servermgmt manages the llama-server process aish serves over.
package servermgmt

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"aish/internal/config"
	"aish/internal/llm"
)

// Server knows how to start/stop the local llama-server.
type Server struct {
	cfg  *config.Config
	proc *exec.Cmd
	log  *os.File
}

func New(cfg *config.Config) *Server { return &Server{cfg: cfg} }

// EnsureUp block-waits until the OpenAI endpoint answers, spawning the
// server if needed. Returns stop func (or nil when KeepAlive).
func (s *Server) EnsureUp(ctx context.Context, client *llm.Client) (func(), error) {
	if client.Healthy(ctx) {
		return nil, nil
	}
	attach, err := s.spawn()
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(180 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			attach()
			return nil, ctx.Err()
		default:
		}
		if s.proc != nil && s.proc.ProcessState != nil {
			attach()
			return nil, fmt.Errorf("llama-server exited early; log: %s", s.cfg.ServerLogPath)
		}
		if client.Healthy(ctx) {
			return attach, nil
		}
		time.Sleep(750 * time.Millisecond)
	}
	attach()
	return nil, fmt.Errorf("llama-server not healthy after 3m; log: %s", s.cfg.ServerLogPath)
}

func (s *Server) spawn() (func(), error) {
	if err := os.MkdirAll(s.cfg.StateDir, 0o755); err != nil {
		return nil, err
	}
	logf, err := os.OpenFile(s.cfg.ServerLogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	s.log = logf

	cmd := exec.Command(s.cfg.LLMBin,
		"-m", s.cfg.ModelPath,
		"-ngl", "99",
		"-c", fmt.Sprint(s.cfg.LLMCtx),
		"--jinja",
		"--host", s.cfg.LLMHost,
		"--port", fmt.Sprint(s.cfg.LLMPort),
	)
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		logf.Close()
		return nil, fmt.Errorf("spawn llama-server: %w", err)
	}
	s.proc = cmd
	fmt.Fprintf(os.Stderr, "aish: starting llama-server (log: %s)…\n", s.cfg.ServerLogPath)

	attached := func() {
		// no-op handle; we keep the child reaped by Start-side
	}
	if s.cfg.KeepAlive {
		return attached, nil
	}
	// PID recorded for atexit-style stop
	go func() {
		_ = cmd.Wait()
	}()
	return func() {
		if s.proc != nil && s.proc.Process != nil {
			_ = s.proc.Process.Signal(syscall.SIGTERM)
		}
		logf.Close()
	}, nil
}

// Stop terminates the spawned server, if any.
func (s *Server) Stop() {
	if s.proc != nil && s.proc.Process != nil {
		_ = s.proc.Process.Signal(syscall.SIGTERM)
		// small grace, then kill
		done := make(chan struct{})
		go func() { _ = s.proc.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = s.proc.Process.Kill()
		}
	}
}
