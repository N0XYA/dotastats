package collector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dotastats/internal/opendota"
)

const (
	constantsInterval    = 24 * time.Hour
	stateConstantsSynced = "constants_synced_at"
	iconWorkers          = 4
	maxIconBytes         = 2 << 20
)

// syncConstants refreshes the constants tables at most once per
// constantsInterval, then downloads any icons that are missing on disk.
func (c *Collector) syncConstants(ctx context.Context) error {
	due, err := c.isDue(ctx, stateConstantsSynced, constantsInterval)
	if err != nil {
		return err
	}
	if due {
		if err := c.fetchConstants(ctx); err != nil {
			return fmt.Errorf("constants: %w", err)
		}
	}
	return c.downloadIcons(ctx)
}

func (c *Collector) fetchConstants(ctx context.Context) error {
	var (
		heroes  map[string]opendota.Hero
		items   map[string]opendota.Item
		modes   map[string]opendota.GameMode
		lobbies map[string]opendota.GameMode
		patches []opendota.Patch
		wheel   map[string]opendota.ChatWheel
	)
	for _, r := range []struct {
		name string
		v    any
	}{
		{"heroes", &heroes}, {"items", &items}, {"game_mode", &modes},
		{"lobby_type", &lobbies}, {"patch", &patches}, {"chat_wheel", &wheel},
	} {
		if err := c.api.Constants(ctx, r.name, r.v); err != nil {
			return err
		}
	}

	tx, err := c.db.Write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	exec := func(query string, args ...any) {
		if err == nil {
			_, err = tx.ExecContext(ctx, query, args...)
		}
	}
	// A changed image source clears the local path, so the icon is
	// downloaded again.
	for _, h := range heroes {
		exec(`INSERT INTO heroes (id, name, localized_name, img_src, icon_src) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET name = excluded.name, localized_name = excluded.localized_name,
				img_path = iif(img_src IS excluded.img_src, img_path, NULL),
				icon_path = iif(icon_src IS excluded.icon_src, icon_path, NULL),
				img_src = excluded.img_src, icon_src = excluded.icon_src`,
			h.ID, h.Name, h.LocalizedName, nullString(h.Img), nullString(h.Icon))
	}
	for name, it := range items {
		exec(`INSERT INTO items (id, name, dname, img_src, cost) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET name = excluded.name, dname = excluded.dname,
				img_path = iif(img_src IS excluded.img_src, img_path, NULL),
				img_src = excluded.img_src, cost = excluded.cost`,
			it.ID, name, nullString(it.DName), nullString(it.Img), it.Cost)
	}
	for _, m := range modes {
		exec(`INSERT INTO game_modes (id, name) VALUES (?, ?)
			ON CONFLICT (id) DO UPDATE SET name = excluded.name`, m.ID, m.Name)
	}
	for _, l := range lobbies {
		exec(`INSERT INTO lobby_types (id, name) VALUES (?, ?)
			ON CONFLICT (id) DO UPDATE SET name = excluded.name`, l.ID, l.Name)
	}
	for _, p := range patches {
		exec(`INSERT INTO patches (id, name, released_at) VALUES (?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET name = excluded.name, released_at = excluded.released_at`,
			p.ID, p.Name, p.Date.Unix())
	}
	for _, w := range wheel {
		exec(`INSERT INTO chat_wheel (id, name, message) VALUES (?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET name = excluded.name, message = excluded.message`, w.ID, w.Name, w.Message)
	}
	// Matches discovered before the patch list was known.
	exec(`UPDATE matches SET patch =
			(SELECT id FROM patches WHERE released_at <= matches.start_time ORDER BY released_at DESC LIMIT 1)
		WHERE patch IS NULL`)
	exec(`INSERT INTO collector_state (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		stateConstantsSynced, strconv.FormatInt(c.now().Unix(), 10))
	if err != nil {
		return err
	}
	slog.Info("constants updated", "heroes", len(heroes), "items", len(items), "patches", len(patches))
	return tx.Commit()
}

// isDue reports whether the step whose last run is stored under key
// hasn't run within every.
func (c *Collector) isDue(ctx context.Context, key string, every time.Duration) (bool, error) {
	var v string
	err := c.db.Read.QueryRowContext(ctx, "SELECT value FROM collector_state WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	last, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return true, nil
	}
	return c.now().Sub(time.Unix(last, 0)) >= every, nil
}

// iconJob is one image to download. rel is the local path relative to the
// assets dir, slash-separated as stored in the database.
type iconJob struct {
	table, column string
	id            int
	src, rel      string
}

// downloadIcons fetches every hero and item image that has no local path
// yet or whose file is gone. Failures are logged and retried next run;
// they don't fail the cycle.
func (c *Collector) downloadIcons(ctx context.Context) error {
	jobs, err := c.missingIcons(ctx)
	if err != nil || len(jobs) == 0 {
		return err
	}
	slog.Info("downloading icons", "count", len(jobs))

	work := make(chan iconJob)
	done := make(chan iconJob)
	var failed atomic.Int32
	var wg sync.WaitGroup
	for range iconWorkers {
		wg.Go(func() {
			for j := range work {
				if err := c.fetchIcon(ctx, j); err != nil {
					failed.Add(1)
					slog.Debug("icon download failed", "src", j.src, "err", err)
					continue
				}
				done <- j
			}
		})
	}
	go func() {
		defer close(work)
		for _, j := range jobs {
			select {
			case work <- j:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(done)
	}()
	var saved []iconJob
	for j := range done {
		saved = append(saved, j)
	}

	if err := c.recordIcons(ctx, saved); err != nil {
		return err
	}
	if n := failed.Load(); n > 0 {
		slog.Warn("some icons failed to download; retrying next run", "failed", n, "saved", len(saved))
	}
	return ctx.Err()
}

func (c *Collector) missingIcons(ctx context.Context) ([]iconJob, error) {
	rows, err := c.db.Read.QueryContext(ctx, `
		SELECT 'heroes', 'img_path', id, img_src, img_path, 'heroes/' || id || '.png' FROM heroes
		UNION ALL
		SELECT 'heroes', 'icon_path', id, icon_src, icon_path, 'heroes/' || id || '_icon.png' FROM heroes
		UNION ALL
		SELECT 'items', 'img_path', id, img_src, img_path, 'items/' || id || '.png' FROM items`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []iconJob
	for rows.Next() {
		var j iconJob
		var src, cur sql.NullString
		if err := rows.Scan(&j.table, &j.column, &j.id, &src, &cur, &j.rel); err != nil {
			return nil, err
		}
		if !src.Valid {
			continue
		}
		j.src = src.String
		if cur.Valid && fileExists(filepath.Join(c.assetsDir, filepath.FromSlash(cur.String))) {
			continue
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func (c *Collector) fetchIcon(ctx context.Context, j iconJob) error {
	// The source must be a path on the CDN, never a different host.
	if !strings.HasPrefix(j.src, "/") || strings.HasPrefix(j.src, "//") {
		return fmt.Errorf("unexpected image path %q", j.src)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.iconBase+j.src, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "dotastats")
	resp, err := c.icons.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxIconBytes+1))
	if err != nil {
		return err
	}
	if len(b) > maxIconBytes {
		return fmt.Errorf("larger than %d bytes", maxIconBytes)
	}
	return writeFileAtomic(filepath.Join(c.assetsDir, filepath.FromSlash(j.rel)), b)
}

func (c *Collector) recordIcons(ctx context.Context, saved []iconJob) error {
	if len(saved) == 0 {
		return nil
	}
	// Fresh context: icons already on disk are recorded even when the run
	// was cancelled mid-download.
	ctx = context.WithoutCancel(ctx)
	tx, err := c.db.Write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, j := range saved {
		// table and column come from missingIcons, never from input.
		q := fmt.Sprintf("UPDATE %s SET %s = ? WHERE id = ?", j.table, j.column)
		if _, err := tx.ExecContext(ctx, q, j.rel, j.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// writeFileAtomic writes via a temp file and rename, so an interrupted
// download never leaves a truncated icon behind.
func writeFileAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".download-*")
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(f.Name())
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		os.Remove(f.Name())
		return err
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
