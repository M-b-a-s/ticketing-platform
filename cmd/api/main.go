package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/M-b-a-s/ticketing-platform/internal/auth"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/health"
	pmw "github.com/M-b-a-s/ticketing-platform/internal/platform/middleware"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := run(); err != nil {
		logger.Error("api exited with error", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// NEW: database pool
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return err
	}

	// NEW: wire auth
	jwtSecret := os.Getenv("JWT_SECRET")
	tokens, err := auth.NewTokenManager(jwtSecret, 15*time.Minute)
	if err != nil {
		return err
	}

	queries := db.New(pool)
	authSvc, err := auth.NewService(queries, queries, tokens, auth.DefaultParams, auth.Config{
		RefreshTTL:          30 * 24 * time.Hour,
		MaxConcurrentHashes: 4,
	})
	if err != nil {
		return err
	}
	// NEW (rate limit): limiters
	trustProxy := os.Getenv("TRUST_PROXY") == "true" // set only when running behind Nginx
	ipKey := pmw.KeyByIP(trustProxy)

	globalLimit := pmw.NewLimiter(100*time.Millisecond, 30) // ~10 req/s per IP, bursts of 30
	registerLimit := pmw.NewLimiter(20*time.Second, 5)      // ~3 per minute per IP
	loginLimit := pmw.NewLimiter(6*time.Second, 10)         // ~10 per minute per IP
	loginEmailLimit := pmw.NewLimiter(30*time.Second, 10)   // per account, from any IP

	// NEW (rate limit): evict idle keys until shutdown so memory stays bounded
	for _, l := range []*pmw.Limiter{globalLimit, registerLimit, loginLimit, loginEmailLimit} {
		go l.Run(ctx, time.Minute)
	}

	// CHANGED (rate limit): handler now receives its limits
	authHandler := auth.NewHandler(authSvc, slog.Default()).WithLimits(auth.Limits{
		IPKey:        ipKey,
		Register:     registerLimit,
		Login:        loginLimit,
		LoginByEmail: loginEmailLimit,
	})

	// router
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)

	r.Method(http.MethodGet, "/healthz", health.Liveness()) // outside the limiter: probes are never throttled
	r.Route("/v1", func(r chi.Router) {
		r.Use(pmw.RateLimit(globalLimit, ipKey)) // NEW (rate limit)
		authHandler.Routes(r)
	})

	srv := &http.Server{
		Addr:              getenv("HTTP_ADDR", ":8080"),
		Handler:           r, // CHANGED
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("api listening", "addr", srv.Addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		slog.Info("shutdown signal received, draining")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	slog.Info("api stopped cleanly")
	return nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}