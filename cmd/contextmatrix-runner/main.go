// Command contextmatrix-runner receives webhooks from ContextMatrix and
// spawns disposable Docker containers to execute autonomous tasks.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mhersson/contextmatrix-runner/internal/callback"
	"github.com/mhersson/contextmatrix-runner/internal/config"
	"github.com/mhersson/contextmatrix-runner/internal/container"
	"github.com/mhersson/contextmatrix-runner/internal/github"
	"github.com/mhersson/contextmatrix-runner/internal/tracker"
	"github.com/mhersson/contextmatrix-runner/internal/webhook"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to configuration file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: cfg.LogLevelSlog(),
	}))

	// Docker client.
	docker, err := container.NewRealDockerClient()
	if err != nil {
		logger.Error("failed to create Docker client", "error", err)
		os.Exit(1)
	}
	defer func() { _ = docker.Close() }()

	// GitHub App token provider.
	tokenProvider, err := github.NewTokenProvider(
		cfg.GitHubApp.AppID,
		cfg.GitHubApp.InstallationID,
		cfg.GitHubApp.PrivateKeyPath,
	)
	if err != nil {
		logger.Error("failed to create GitHub token provider", "error", err)
		os.Exit(1)
	}

	// Core components.
	trk := tracker.New()
	cb := callback.NewClient(cfg.ContextMatrixURL, cfg.APIKey, logger)
	mgr := container.NewManager(docker, trk, cb, tokenProvider, cfg, logger)

	// Clean up any orphan containers from a previous crash.
	if err := mgr.CleanupOrphans(context.Background()); err != nil {
		logger.Warn("orphan cleanup failed", "error", err)
	}

	// Webhook handler.
	wh := webhook.NewHandler(mgr, trk, cfg.APIKey, cfg.MaxConcurrent, logger)
	mux := http.NewServeMux()
	wh.Register(mux)

	// HTTP server.
	addr := fmt.Sprintf(":%d", cfg.Port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Start server in a goroutine.
	go func() {
		logger.Info("runner started", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	// Wait for shutdown signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	logger.Info("received signal, shutting down", "signal", sig)

	// Kill all running containers.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	for _, info := range trk.All() {
		logger.Info("killing container on shutdown", "card_id", info.CardID, "project", info.Project)
		if err := mgr.Kill(info.Project, info.CardID); err != nil {
			logger.Warn("failed to kill container", "card_id", info.CardID, "error", err)
		}
		if err := cb.ReportStatus(shutdownCtx, info.CardID, info.Project, "failed", "runner shutting down"); err != nil {
			logger.Warn("failed to report shutdown status", "card_id", info.CardID, "error", err)
		}
	}

	// Wait for container goroutines to finish.
	mgr.Wait()

	// Graceful HTTP shutdown.
	httpCtx, httpCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer httpCancel()
	if err := srv.Shutdown(httpCtx); err != nil {
		logger.Error("server shutdown error", "error", err)
	}

	logger.Info("runner stopped")
}
