// Package testdb gives integration tests a fresh, fully migrated database.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

const envURL = "TEST_DATABASE_URL"

// New creates a throwaway database, applies every migration, and returns a
// pool connected to it. TEST_DATABASE_URL must point at an existing database
// (usually "postgres") whose user may CREATE DATABASE; the test database is
// dropped when the test ends. The test is skipped if the variable is unset.
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()

	adminURL := os.Getenv(envURL)
	if adminURL == "" {
		t.Skipf("%s is not set; skipping integration test", envURL)
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect to admin database: %v", err)
	}
	t.Cleanup(admin.Close) // registered first, so it runs last

	name := "itest_" + randomHex(6) // hex only, so safe to use as an identifier
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("drop database %s: %v", name, err)
		}
	})

	cfg, err := pgxpool.ParseConfig(adminURL)
	if err != nil {
		t.Fatalf("parse %s: %v", envURL, err)
	}
	cfg.ConnConfig.Database = name

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect to %s: %v", name, err)
	}
	t.Cleanup(pool.Close) // registered last, so it runs first: the pool closes before the drop

	migrate(t, pool)
	return pool
}

func migrate(t testing.TB, pool *pgxpool.Pool) {
	t.Helper()

	sqlDB := stdlib.OpenDBFromPool(pool)
	defer sqlDB.Close()

	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("goose dialect: %v", err)
	}
	if err := goose.Up(sqlDB, filepath.Join(repoRoot(t), "migrations")); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
}

func repoRoot(t testing.TB) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for dir := wd; ; {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found above %s", wd)
		}
		dir = parent
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}