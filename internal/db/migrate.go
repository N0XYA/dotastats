package db

import (
	"cmp"
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type migration struct {
	version int
	name    string
	sql     string
}

// Migrate applies every embedded migration newer than the database's
// PRAGMA user_version, each in its own transaction, and returns the schema
// version before and after.
func Migrate(ctx context.Context, db *sql.DB) (from, to int, err error) {
	migs, err := loadMigrations(migrationFiles)
	if err != nil {
		return 0, 0, err
	}
	return apply(ctx, db, migs)
}

// UserVersion returns the schema version stored in PRAGMA user_version.
func UserVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v int
	err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	return v, err
}

// loadMigrations reads files named like 001_init.sql and checks that their
// versions run 1, 2, 3, ... without gaps or duplicates.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	names, err := fs.Glob(fsys, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	var migs []migration
	for _, name := range names {
		base := path.Base(name)
		prefix, _, ok := strings.Cut(base, "_")
		v, err := strconv.Atoi(prefix)
		if !ok || err != nil || v < 1 {
			return nil, fmt.Errorf("migration %s: name must look like 001_description.sql", base)
		}
		b, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		migs = append(migs, migration{version: v, name: base, sql: string(b)})
	}
	slices.SortFunc(migs, func(a, b migration) int { return cmp.Compare(a.version, b.version) })
	for i, m := range migs {
		if m.version != i+1 {
			return nil, fmt.Errorf("migration %s: expected version %d", m.name, i+1)
		}
	}
	return migs, nil
}

func apply(ctx context.Context, db *sql.DB, migs []migration) (from, to int, err error) {
	from, err = UserVersion(ctx, db)
	if err != nil {
		return 0, 0, err
	}
	if from > len(migs) {
		return from, from, fmt.Errorf("database schema version %d is newer than this binary supports (%d); use a newer dotastats", from, len(migs))
	}
	for _, m := range migs[from:] {
		if err := applyOne(ctx, db, m); err != nil {
			return from, 0, err
		}
	}
	to, err = UserVersion(ctx, db)
	return from, to, err
}

func applyOne(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Re-check under the write lock: another dotastats process may have
	// applied this migration since the version was first read.
	var cur int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&cur); err != nil {
		return err
	}
	if cur >= m.version {
		return nil
	}
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("migration %s: %w", m.name, err)
	}
	// user_version lives in the database header, so it commits atomically
	// with the schema change.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
		return fmt.Errorf("migration %s: %w", m.name, err)
	}
	return tx.Commit()
}
