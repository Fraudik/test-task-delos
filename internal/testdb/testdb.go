package testdb

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"test-task-dellos/internal/postgres"
)

const (
	EnvURL     = "TEST_DATABASE_URL"
	DefaultURL = "postgres://dellos:dellos@localhost:5432/dellos?sslmode=disable"

	setupTimeout   = 30 * time.Second
	templateLockID = 7_061_955_502
)

var template struct {
	once sync.Once
	name string
	err  error
}

func URL() string {
	if v := os.Getenv(EnvURL); v != "" {
		return v
	}
	return DefaultURL
}

func ident(name string) string {
	return pgx.Identifier{name}.Sanitize()
}

func randomHex() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func adminExec(ctx context.Context, t testing.TB, adminURL, sql string) error {
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return err
	}
	defer closeConn(ctx, t, conn)
	_, err = conn.Exec(ctx, sql)
	return err
}

func closeConn(ctx context.Context, t testing.TB, conn *pgx.Conn) {
	if err := conn.Close(context.WithoutCancel(ctx)); err != nil {
		t.Logf("testdb: close admin connection: %v", err)
	}
}

func connect(ctx context.Context, adminURL, database string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(adminURL)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", EnvURL, err)
	}
	cfg.ConnConfig.Database = database
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", database, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping %s: %w", database, err)
	}
	return pool, nil
}

func New(t testing.TB) *pgxpool.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("testdb: database tests are skipped in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()
	adminURL := URL()

	// Migrations are run once against a database, and each test clones it using `CREATE DATABASE … TEMPLATE`
	// Template creates once
	template.once.Do(func() {
		template.name, template.err = ensureTemplate(ctx, t, adminURL)
	})
	if template.err != nil {
		t.Fatalf("testdb: prepare template failed (is PostgreSQL up? set %s if not on default URL): %v", EnvURL, template.err)
	}

	name := "testdb_" + randomHex()
	if err := adminExec(ctx, t, adminURL, "CREATE DATABASE "+ident(name)+" TEMPLATE "+ident(template.name)); err != nil {
		t.Fatalf("testdb: create database failed: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
		defer cancel()
		if err := adminExec(ctx, t, adminURL, "DROP DATABASE IF EXISTS "+ident(name)+" WITH (FORCE)"); err != nil {
			t.Errorf("testdb: drop database %s failed: %v", name, err)
		}
	})

	pool, err := connect(ctx, adminURL, name)
	if err != nil {
		t.Fatalf("testdb: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func ensureTemplate(ctx context.Context, t testing.TB, adminURL string) (string, error) {
	hash, err := migrationsHash()
	if err != nil {
		return "", err
	}
	name := "testdb_template_" + hash[:16]

	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return "", fmt.Errorf("connect: %w", err)
	}
	defer closeConn(ctx, t, conn)

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, templateLockID); err != nil {
		return "", fmt.Errorf("lock: %w", err)
	}

	var ready bool
	err = conn.QueryRow(ctx, `SELECT datistemplate FROM pg_database WHERE datname = $1`, name).Scan(&ready)
	switch {
	case err == nil && ready:
		return name, nil
	case err == nil:
		if _, err := conn.Exec(ctx, "DROP DATABASE "+ident(name)+" WITH (FORCE)"); err != nil {
			return "", fmt.Errorf("drop incomplete template: %w", err)
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return "", fmt.Errorf("look up template: %w", err)
	}

	if _, err := conn.Exec(ctx, "CREATE DATABASE "+ident(name)); err != nil {
		return "", fmt.Errorf("create template: %w", err)
	}
	pool, err := connect(ctx, adminURL, name)
	if err != nil {
		return "", err
	}
	err = postgres.Migrate(ctx, pool)
	pool.Close()
	if err != nil {
		return "", err
	}
	if _, err := conn.Exec(ctx, "ALTER DATABASE "+ident(name)+" WITH IS_TEMPLATE true ALLOW_CONNECTIONS false"); err != nil {
		return "", fmt.Errorf("mark template: %w", err)
	}
	return name, nil
}

func migrationsHash() (string, error) {
	fsys, err := postgres.Migrations()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	err = fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", path, len(data))
		h.Write(data)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("hash migrations: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
