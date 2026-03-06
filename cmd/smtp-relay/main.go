package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/neosenth/gmail-smtp-relay/internal/config"
	"github.com/neosenth/gmail-smtp-relay/internal/gmail"
	"github.com/neosenth/gmail-smtp-relay/internal/obs"
	"github.com/neosenth/gmail-smtp-relay/internal/queue"
	"github.com/neosenth/gmail-smtp-relay/internal/smtp"
	"github.com/neosenth/gmail-smtp-relay/internal/worker"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.LogLevel,
	}))
	slog.SetDefault(logger)

	queueStore, err := queue.NewSQLiteStore(cfg.QueueDBPath)
	if err != nil {
		logger.Error("failed to initialize queue store", "error", err, "path", cfg.QueueDBPath)
		os.Exit(1)
	}
	defer func() {
		if closeErr := queueStore.Close(); closeErr != nil {
			logger.Error("failed to close queue store", "error", closeErr)
		}
	}()
	recovered, err := queueStore.RecoverStuckProcessing(
		context.Background(),
		time.Now().UTC().Add(-time.Duration(cfg.ProcessingStuckTimeoutSeconds)*time.Second),
		time.Now().UTC(),
	)
	if err != nil {
		logger.Error("failed to recover stuck queue rows", "error", err)
		os.Exit(1)
	}
	if recovered > 0 {
		logger.Warn("recovered stale processing queue rows", "count", recovered)
	}

	sender, err := gmail.NewAPISender(context.Background(), gmail.Config{
		ClientID:     cfg.GmailClientID,
		ClientSecret: cfg.GmailClientSecret,
		RefreshToken: cfg.GmailRefreshToken,
		Mailbox:      cfg.GmailMailbox,
	}, time.Duration(cfg.SendAsRefreshSeconds)*time.Second)
	if err != nil {
		logger.Error("failed to initialize gmail sender", "error", err)
		os.Exit(1)
	}
	server, err := smtp.NewServer(cfg, logger, queueStore, sender)
	if err != nil {
		logger.Error("failed to initialize smtp server", "error", err)
		os.Exit(1)
	}
	workerRunner, err := worker.NewRunner(worker.Config{
		PollInterval:     time.Duration(cfg.WorkerPollIntervalSeconds) * time.Second,
		Concurrency:      cfg.WorkerConcurrency,
		MaxAttempts:      cfg.WorkerMaxAttempts,
		RetryBaseBackoff: time.Duration(cfg.WorkerRetryBaseSeconds) * time.Second,
		RetryMaxBackoff:  time.Duration(cfg.WorkerRetryMaxSeconds) * time.Second,
	}, logger, queueStore, sender)
	if err != nil {
		logger.Error("failed to initialize delivery worker", "error", err)
		os.Exit(1)
	}
	obsServer, err := obs.NewServer(obs.Config{
		Addr:             cfg.ObsHTTPAddr,
		AliasMaxStaleFor: time.Duration(cfg.ReadinessAliasMaxStaleSeconds) * time.Second,
	}, logger, queueStore, workerRunner, sender)
	if err != nil {
		logger.Error("failed to initialize observability server", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if shutdownErr := server.Shutdown(shutdownCtx); shutdownErr != nil {
			logger.Error("smtp server shutdown error", "error", shutdownErr)
		}
		if shutdownErr := obsServer.Shutdown(shutdownCtx); shutdownErr != nil {
			logger.Error("observability server shutdown error", "error", shutdownErr)
		}
	}()
	go func() {
		sender.RunAliasRefresh(ctx)
	}()
	go func() {
		if runErr := obsServer.Run(ctx); runErr != nil {
			logger.Error("observability server stopped with error", "error", runErr)
		}
	}()
	go func() {
		if runErr := workerRunner.Run(ctx); runErr != nil {
			logger.Error("delivery worker stopped with error", "error", runErr)
		}
	}()

	logger.Info("starting smtp relay", "addr_465", cfg.SMTPBindAddr465, "addr_587", cfg.SMTPBindAddr587)
	if err := server.Run(ctx); err != nil {
		logger.Error("smtp server stopped with error", "error", err)
		os.Exit(1)
	}

	logger.Info("smtp server stopped")
}
