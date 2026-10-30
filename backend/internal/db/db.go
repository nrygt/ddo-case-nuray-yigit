// Package db bağlantı havuzu ve migrasyon çalıştırıcısını içerir.
package db

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	if cfg.MaxConns < 10 {
		cfg.MaxConns = 20
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = "pistkoordinat"
	cfg.ConnConfig.RuntimeParams["timezone"] = "UTC"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return pool, nil
}

// WaitReady veritabanı ayağa kalkana kadar bekler (compose içinde ilk açılış).
func WaitReady(ctx context.Context, pool *pgxpool.Pool, max time.Duration) error {
	deadline := time.Now().Add(max)
	for {
		pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := pool.Ping(pctx)
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("veritabanına bağlanılamadı: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

const migrationLock = 7_310_001

// Migrate gömülü .sql dosyalarını ad sırasıyla, her biri kendi transaction'ında uygular.
// Aynı anda iki süreç çalıştırırsa advisory lock ile sıraya girerler.
func Migrate(ctx context.Context, pool *pgxpool.Pool, files fs.FS) (int, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLock); err != nil {
		return 0, err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLock) //nolint:errcheck

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return 0, err
	}

	names, err := fs.Glob(files, "*.sql")
	if err != nil {
		return 0, err
	}
	sort.Strings(names)

	applied := 0
	for _, name := range names {
		version := strings.TrimSuffix(name, ".sql")
		var exists bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, version).Scan(&exists); err != nil {
			return applied, err
		}
		if exists {
			continue
		}
		body, err := fs.ReadFile(files, name)
		if err != nil {
			return applied, err
		}
		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES ($1)`, version)
			return err
		})
		if err != nil {
			return applied, fmt.Errorf("migrasyon %s: %w", name, err)
		}
		slog.Info("migrasyon uygulandı", "version", version)
		applied++
	}
	return applied, nil
}

// Postgres hata kodları
const (
	CodeExclusion  = "23P01"
	CodeUnique     = "23505"
	CodeCheck      = "23514"
	CodeForeignKey = "23503"
	CodeSerialize  = "40001"
	CodeDeadlock   = "40P01"
)

func ErrCode(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}

func ErrMessage(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Message
	}
	return err.Error()
}

// RetrySerializable SERIALIZABLE transaction'ı serileştirme hatasında yeniden dener.
func RetrySerializable(ctx context.Context, pool *pgxpool.Pool, attempts int, fn func(pgx.Tx) error) error {
	var err error
	for i := 0; i < attempts; i++ {
		err = pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, fn)
		code := ErrCode(err)
		if code != CodeSerialize && code != CodeDeadlock {
			return err
		}
		time.Sleep(time.Duration(5*(i+1)) * time.Millisecond)
	}
	return err
}
