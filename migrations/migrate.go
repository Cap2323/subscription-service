package migrations

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed *.sql
var Files embed.FS

func Apply(ctx context.Context, pool *pgxpool.Pool) error {
	names, err := fs.Glob(Files, "*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		if err := applyOne(ctx, pool, name); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}
	return nil
}
func applyOne(ctx context.Context, pool *pgxpool.Pool, name string) error {
	data, err := Files.ReadFile(name)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	checksum := hex.EncodeToString(sum[:])
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(764302198)`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS public.schema_migrations(name TEXT PRIMARY KEY,checksum TEXT NOT NULL,applied_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp())`); err != nil {
		return err
	}
	var existing string
	rows, err := tx.Query(ctx, `SELECT checksum FROM public.schema_migrations WHERE name=$1`, name)
	if err != nil {
		return err
	}
	if rows.Next() {
		if err := rows.Scan(&existing); err != nil {
			rows.Close()
			return err
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if existing != "" {
		if existing != checksum {
			return fmt.Errorf("applied migration checksum changed; create a new migration")
		}
		return tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, string(data)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO public.schema_migrations(name,checksum) VALUES($1,$2)`, name, checksum); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
