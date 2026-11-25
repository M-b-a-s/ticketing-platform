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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/M-b-a-s/ticketing-platform/internal/auth"
	"github.com/M-b-a-s/ticketing-platform/internal/events"
	"github.com/M-b-a-s/ticketing-platform/internal/httpapi"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/database/db"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/health"
	"github.com/M-b-a-s/ticketing-platform/internal/platform/middleware"
	"github.com/M-b-a-s/ticketing-platform/internal/tickets"
	"github.com/M-b-a-s/ticketing-platform/internal/orders"
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

	// database pool
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
	queries := db.New(pool)

	// auth
	tokens, err := auth.NewTokenManager(os.Getenv("JWT_SECRET"), 15*time.Minute)
	if err != nil {
		return err
	}
	authSvc, err := auth.NewService(queries, queries, tokens, auth.DefaultParams, auth.Config{
		RefreshTTL:          30 * 24 * time.Hour,
		MaxConcurrentHashes: 4,
	})
	if err != nil {
		return err
	}

	// rate limiters
	trustProxy := os.Getenv("TRUST_PROXY") == "true" // set only when running behind Nginx
	ipKey := middleware.KeyByIP(trustProxy)

	globalLimit := middleware.NewLimiter(100*time.Millisecond, 30) // ~10 req/s per IP, bursts of 30
	registerLimit := middleware.NewLimiter(20*time.Second, 5)      // ~3 per minute per IP
	loginLimit := middleware.NewLimiter(6*time.Second, 10)         // ~10 per minute per IP
	loginEmailLimit := middleware.NewLimiter(30*time.Second, 10)   // per account, from any IP
	orderCreateLimit := middleware.NewLimiter(5*time.Second, 6)     // ~12 orders a minute per user, bursts of 6
	for _, l := range []*middleware.Limiter{globalLimit, registerLimit, loginLimit, loginEmailLimit, orderCreateLimit} {
		go l.Run(ctx, time.Minute) // evict idle keys until shutdown
	}

	// handlers
	authHandler := auth.NewHandler(authSvc, slog.Default()).WithLimits(auth.Limits{
		IPKey:        ipKey,
		Register:     registerLimit,
		Login:        loginLimit,
		LoginByEmail: loginEmailLimit,
	})

	eventSvc := events.NewService(queries)
	ticketSvc := tickets.NewService(queries)

	router := httpapi.NewRouter(httpapi.Deps{
		Tokens:       tokens,
		Health:       health.Liveness(),
		Auth:         authHandler,
		Events:       events.NewHandler(eventSvc, slog.Default()),
		Tickets:      tickets.NewHandler(ticketSvc, slog.Default()),
		Orders:       orders.NewHandler(orders.NewService(orders.NewPgStore(pool)), slog.Default()).WithCreateLimit(orderCreateLimit),
		V1Middleware: middleware.RateLimit(globalLimit, ipKey),
	})

	srv := &http.Server{
		Addr:              getenv("HTTP_ADDR", ":8080"),
		Handler:           router,
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