package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/M-b-a-s/ticketing-platform/internal/orders"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := run(); err != nil {
		logger.Error("worker exited with error", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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

	var cfg orders.SweepConfig // zero value: the defaults
	if v := os.Getenv("SWEEP_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("SWEEP_INTERVAL: %w", err)
		}
		cfg.Interval = d
	}
	sweeper := orders.NewSweeper(orders.NewPgStore(pool), cfg, slog.Default())

	var wg sync.WaitGroup
	for _, loop := range []func(context.Context){sweeper.Run, sweeper.RunKeyCleanup} {
		loop := loop
		wg.Add(1)
		go func() {
			defer wg.Done()
			loop(ctx)
		}()
	}

	slog.Info("worker started")
	<-ctx.Done()
	slog.Info("shutdown signal received, finishing the current batch")
	wg.Wait() // a batch in progress completes; no new one starts
	slog.Info("worker stopped cleanly")
	return nil
}