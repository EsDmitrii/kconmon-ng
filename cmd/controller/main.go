package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/controller"
)

func main() {
	configPath := os.Getenv("KCONMON_NG_CONFIG")
	if configPath == "" {
		configPath = "/etc/kconmon-ng/config.yaml"
	}

	loader := config.NewLoader(configPath)
	if err := loader.Load(); err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	cfg := loader.Get()
	config.SetupLogger(cfg.LogLevel, cfg.LogFormat)

	slog.Info("kconmon-ng controller starting",
		"version", config.Version,
		"commit", config.Commit,
		"buildDate", config.BuildDate,
	)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	ctrl := controller.New(cfg)
	// Subscribed before the watch starts, so no reload lands between New and the subscription.
	loader.OnChange(ctrl.ApplyConfig)
	if err := loader.WatchForChanges(); err != nil {
		slog.Warn("config hot-reload not available", "error", err)
	}

	if err := ctrl.Run(ctx); err != nil {
		slog.Error("controller exited with error", "error", err)
		cancel()
		_ = loader.Close()
		os.Exit(1)
	}
	cancel()
	_ = loader.Close()
}
