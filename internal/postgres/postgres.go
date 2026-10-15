package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var embedded embed.FS

func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return pool, nil
}

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return withProvider(pool, func(p *goose.Provider) error {
		if _, err := p.Up(ctx); err != nil {
			return fmt.Errorf("postgres: migrate up: %w", err)
		}
		return nil
	})
}

func Rollback(ctx context.Context, pool *pgxpool.Pool, version int64) error {
	return withProvider(pool, func(p *goose.Provider) error {
		if _, err := p.DownTo(ctx, version); err != nil {
			return fmt.Errorf("postgres: migrate down to %d: %w", version, err)
		}
		return nil
	})
}

func Migrations() (fs.FS, error) {
	return fs.Sub(embedded, "migrations")
}

func withProvider(pool *pgxpool.Pool, fn func(*goose.Provider) error) (err error) {
	fsys, err := Migrations()
	if err != nil {
		return err
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("postgres: migration locker: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, stdlib.OpenDBFromPool(pool), fsys, goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("postgres: migration provider: %w", err)
	}
	defer func() {
		if cerr := p.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("postgres: close migration provider: %w", cerr))
		}
	}()
	return fn(p)
}
