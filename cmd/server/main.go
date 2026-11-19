package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/crypto/bcrypt"

	"cloute/internal/auth"
	"cloute/internal/config"
	"cloute/internal/doc"
	"cloute/internal/httpapi"
	"cloute/internal/storage/postgres"
	"cloute/internal/transport/ws"
	"cloute/web"
)

const sessionTTL = 30 * 24 * time.Hour

var hubConfig = doc.Config{
	QueueSize:     256,
	IdleTimeout:   time.Minute,
	SnapshotEvery: 1000,
	MaxCatchUp:    1000,
	StoreTimeout:  10 * time.Second,
}

func main() {
	if err := start(); err != nil {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}

func start() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger := newLogger(os.Stdout, cfg.LogFormat, cfg.LogLevel)
	slog.SetDefault(logger)

	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.HTTPAddr, err)
	}

	return run(ctx, cfg, logger, ln)
}

func newLogger(w io.Writer, format string, level slog.Level) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if format == config.LogFormatJSON {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

func run(ctx context.Context, cfg config.Config, logger *slog.Logger, ln net.Listener) error {
	defer func() { _ = ln.Close() }()

	pool, err := postgres.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	applied, err := postgres.Migrate(ctx, pool)
	if err != nil {
		return err
	}
	for _, m := range applied {
		logger.Info("migration applied", "version", m.Source.Version, "file", m.Source.Path)
	}

	accounts, err := auth.NewService(postgres.NewAuthStore(pool), auth.Config{
		BcryptCost: bcrypt.DefaultCost,
		SessionTTL: sessionTTL,
	})
	if err != nil {
		return err
	}

	registry := doc.NewRegistry(postgres.NewStore(pool), hubConfig, logger)
	defer registry.Close()

	wsHandler := ws.NewHandler(registry, accounts, logger)
	api := httpapi.NewHandler(accounts, registry, httpapi.Options{CookieSecure: cfg.CookieSecure, SessionTTL: sessionTTL}, logger)
	frontend, embedded := web.Dist()
	if !embedded {
		logger.Warn("built without the frontend (-tags embedweb); use the Vite dev server")
	}

	mux := http.NewServeMux()
	mux.Handle("GET /ws", wsHandler)
	mux.Handle("/api/", api)
	mux.Handle("GET /healthz", api)
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.Handle("/", httpapi.Static(frontend))

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(ln)
	}()
	logger.Info("http server started", "addr", ln.Addr().String())

	select {
	case err := <-serveErr:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	logger.Info("shutting down", "timeout", cfg.ShutdownTimeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	registry.Close()
	if err := wsHandler.Shutdown(shutdownCtx); err != nil {
		logger.Warn("websocket connections closed without handshake", "err", err)
	}

	logger.Info("server stopped")
	return nil
}
