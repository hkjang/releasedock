package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hkjang/releasedock/backend/internal/config"
	"github.com/hkjang/releasedock/backend/internal/secure"
	"github.com/hkjang/releasedock/backend/internal/server"
	"github.com/hkjang/releasedock/backend/internal/store"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

func main() {
	showVersion := flag.Bool("version", false, "print server version")
	flag.Parse()
	if *showVersion {
		fmt.Printf("releasedock-server %s (%s, %s)\n", version, commit, buildTime)
		return
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := config.Load()
	if err != nil {
		logger.Error("invalid runtime configuration", "error", err)
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	st, err := store.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		logger.Error("database initialization failed", "error", err)
		os.Exit(1)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		logger.Error("database migration failed", "error", err)
		os.Exit(1)
	}
	if err := st.BootstrapAdmin(ctx, cfg.BootstrapAdmin, cfg.BootstrapAdminPassword); err != nil {
		logger.Error("bootstrap administrator failed", "error", err)
		os.Exit(1)
	}
	vault, err := secure.NewVault(cfg.EncryptionKey)
	if err != nil {
		logger.Error("secret vault initialization failed", "error", err)
		os.Exit(1)
	}
	webRoot := server.FindWebRoot()
	if webRoot == "" {
		logger.Warn("web assets not found; API-only mode enabled")
	} else {
		logger.Info("serving web assets", "root", webRoot)
	}
	app := server.New(st, vault, logger, server.BuildInfo{Version: version, Commit: commit, BuildTime: buildTime}, webRoot)
	httpServer := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
	go cleanupExpired(ctx, st, logger)
	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("HTTP shutdown failed", "error", err)
		}
	}()
	logger.Info("ReleaseDock server starting", "address", httpServer.Addr, "version", version)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("HTTP server stopped unexpectedly", "error", err)
		os.Exit(1)
	}
}

func cleanupExpired(ctx context.Context, st *store.Store, logger *slog.Logger) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			_, sessionErr := st.Pool.Exec(cleanupCtx, `DELETE FROM sessions WHERE expires_at<now()`)
			_, oidcErr := st.Pool.Exec(cleanupCtx, `DELETE FROM oidc_states WHERE expires_at<now()`)
			if err := errors.Join(sessionErr, oidcErr); err != nil {
				logger.Warn("expired credential cleanup failed", "error", err)
			}
			cancel()
		}
	}
}
