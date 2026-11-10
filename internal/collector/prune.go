package collector

import (
	"context"
	"database/sql"
	"strconv"
	"time"
)

const (
	retentionInterval = 24 * time.Hour
	stateRetentionRan = "retention_ran_at"
)

// PruneReport summarizes a Prune run.
type PruneReport struct {
	RawDeleted     int64 // raw JSON blobs
	DetailsDeleted int64 // matches whose time series, purchases and wards were dropped
	MatchesDeleted int64
	FreedBytes     int64 // file space returned by incremental vacuum
}

// Prune applies the retention settings: raw JSON, then parsed detail, then
// whole matches older than their limits (negative days: off). Normalized
// scoreboard rows outlive the raw JSON, so pages keep working. Afterwards
// it vacuums incrementally so the file actually shrinks.
func Prune(ctx context.Context, db *sql.DB, s Settings, now time.Time) (PruneReport, error) {
	var rep PruneReport
	cutoff := func(days int) int64 { return now.AddDate(0, 0, -days).Unix() }

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return rep, err
	}
	defer tx.Rollback()
	run := func(query string, args ...any) (int64, error) {
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	}

	if s.RawRetentionDays >= 0 {
		c := cutoff(s.RawRetentionDays)
		if _, err := run(`UPDATE matches SET raw_pruned_at = ?
			WHERE start_time < ? AND match_id IN (SELECT match_id FROM match_raw)`, now.Unix(), c); err != nil {
			return rep, err
		}
		if rep.RawDeleted, err = run(`DELETE FROM match_raw
			WHERE match_id IN (SELECT match_id FROM matches WHERE start_time < ?)`, c); err != nil {
			return rep, err
		}
	}
	if s.DetailRetentionDays >= 0 {
		old := "match_id IN (SELECT match_id FROM matches WHERE start_time < ?)"
		c := cutoff(s.DetailRetentionDays)
		if rep.DetailsDeleted, err = run("DELETE FROM match_timeseries WHERE "+old, c); err != nil {
			return rep, err
		}
		for _, table := range []string{"player_timeseries", "item_purchases", "wards", "match_events"} {
			if _, err := run("DELETE FROM "+table+" WHERE "+old, c); err != nil {
				return rep, err
			}
		}
	}
	if s.MatchRetentionDays >= 0 {
		// Players, links, raw and detail rows go with the match (ON DELETE CASCADE).
		if rep.MatchesDeleted, err = run("DELETE FROM matches WHERE start_time < ?", cutoff(s.MatchRetentionDays)); err != nil {
			return rep, err
		}
	}
	if _, err := run(`INSERT INTO collector_state (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		stateRetentionRan, strconv.FormatInt(now.Unix(), 10)); err != nil {
		return rep, err
	}
	if err := tx.Commit(); err != nil {
		return rep, err
	}

	rep.FreedBytes, err = vacuum(ctx, db)
	return rep, err
}

// vacuum returns free pages to the file system (auto_vacuum is
// INCREMENTAL) and checkpoints the WAL so the main file shrinks now.
func vacuum(ctx context.Context, db *sql.DB) (int64, error) {
	pages := func() (int64, error) {
		var n int64
		err := db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&n)
		return n, err
	}
	var pageSize int64
	if err := db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return 0, err
	}
	before, err := pages()
	if err != nil {
		return 0, err
	}
	// incremental_vacuum frees pages while it is stepped, so read it to the end.
	rows, err := db.QueryContext(ctx, "PRAGMA incremental_vacuum")
	if err != nil {
		return 0, err
	}
	for rows.Next() {
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if _, err := db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return 0, err
	}
	after, err := pages()
	if err != nil {
		return 0, err
	}
	return (before - after) * pageSize, nil
}

// syncRetention runs Prune at most once per retentionInterval.
func (c *Collector) syncRetention(ctx context.Context, s Settings) error {
	due, err := c.isDue(ctx, stateRetentionRan, retentionInterval)
	if err != nil || !due {
		return err
	}
	_, err = Prune(ctx, c.db.Write, s, c.now())
	return err
}
