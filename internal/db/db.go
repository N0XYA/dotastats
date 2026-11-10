// Package db opens the SQLite database and applies embedded migrations.
package db

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// readPoolSize bounds concurrent page reads; the target host has one vCPU.
const readPoolSize = 4

// DB holds the single write connection and a small read-only pool. In WAL
// mode readers never block the writer, so pages render while the collector
// writes.
type DB struct {
	Write *sql.DB
	Read  *sql.DB
}

// Open opens the database file at path, creating it if needed.
func Open(path string) (*DB, error) {
	w, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	// Ping opens the first connection so a bad path or pragma fails here,
	// and switches the file to WAL before any reader connects.
	if err := w.Ping(); err != nil {
		w.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	r, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(readPoolSize)
	r.SetConnMaxIdleTime(5 * time.Minute)
	if err := r.Ping(); err != nil {
		r.Close()
		w.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return &DB{Write: w, Read: r}, nil
}

// Close closes the read pool, then the writer, so the last connection to
// close checkpoints the WAL.
func (d *DB) Close() error {
	return errors.Join(d.Read.Close(), d.Write.Close())
}

func dsn(path string, write bool) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	if write {
		// auto_vacuum must be set before switching to WAL: on a new file the
		// WAL switch writes the header, after which auto_vacuum can only be
		// enabled by a full VACUUM. On an existing file this is a no-op.
		// prune runs PRAGMA incremental_vacuum so the file actually shrinks.
		q.Add("_pragma", "auto_vacuum(INCREMENTAL)")
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "synchronous(NORMAL)")
		// Take the write lock at BEGIN, so a transaction never fails halfway
		// when a CLI command and the server write at the same time.
		q.Set("_txlock", "immediate")
	} else {
		q.Add("_pragma", "query_only(1)")
	}
	return path + "?" + q.Encode()
}
