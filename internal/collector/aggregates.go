package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

const (
	aggregatesInterval    = 24 * time.Hour
	stateAggregatesSynced = "aggregates_synced_at"
	// benchmarkHeroDays: heroes played this recently get /benchmarks.
	benchmarkHeroDays = 30
)

// heroStatCounts selects the /heroStats fields kept in the daily snapshot:
// pick and win counts per rank bracket (1_pick .. 8_win) and for pubs,
// turbo and pro games. Static hero attributes come from constants.
var heroStatCounts = regexp.MustCompile(`^(\d+_(pick|win)|pub_(pick|win)|turbo_(picks|wins)|pro_(pick|win|ban))$`)

// syncAggregates takes the daily /heroStats snapshot and refreshes
// /benchmarks for heroes a tracked account played lately.
// Between daily runs it only fetches benchmarks for newly played heroes.
// Without active accounts nothing would use them, so it waits.
func (c *Collector) syncAggregates(ctx context.Context, haveAccounts bool) error {
	if !haveAccounts {
		return nil
	}
	due, err := c.isDue(ctx, stateAggregatesSynced, aggregatesInterval)
	if err != nil {
		return err
	}
	if !due {
		return c.syncBenchmarks(ctx, true)
	}
	now := c.now()

	raw, err := c.api.Get(ctx, "/heroStats", nil)
	if err != nil {
		return fmt.Errorf("heroStats: %w", err)
	}
	var heroes []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &heroes); err != nil {
		return fmt.Errorf("heroStats: %w", err)
	}
	day := now.UTC().Format(time.DateOnly)
	tx, err := c.db.Write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ex := &execer{ctx: ctx, tx: tx}
	for _, h := range heroes {
		var id int
		if err := json.Unmarshal(h["id"], &id); err != nil {
			return fmt.Errorf("heroStats: hero id: %w", err)
		}
		counts := map[string]json.RawMessage{}
		for k, v := range h {
			if heroStatCounts.MatchString(k) {
				counts[k] = v
			}
		}
		data, err := json.Marshal(counts) // map keys marshal sorted
		if err != nil {
			return err
		}
		ex.exec(`INSERT INTO hero_stats_cache (hero_id, fetched_on, data) VALUES (?, ?, ?)
			ON CONFLICT (hero_id, fetched_on) DO UPDATE SET data = excluded.data`, id, day, string(data))
	}
	if ex.err != nil {
		return ex.err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	if err := c.syncBenchmarks(ctx, false); err != nil {
		return err
	}
	slog.Info("aggregates updated", "heroes", len(heroes))
	return c.setState(ctx, stateAggregatesSynced, strconv.FormatInt(now.Unix(), 10))
}

// syncBenchmarks fetches /benchmarks for heroes an active account played
// within benchmarkHeroDays; with onlyMissing, just those not fetched yet.
func (c *Collector) syncBenchmarks(ctx context.Context, onlyMissing bool) error {
	now := c.now()
	heroIDs, err := queryIDs(ctx, c.db.Read, `
		SELECT DISTINCT mp.hero_id
		FROM match_players mp
		JOIN account_matches am ON am.match_id = mp.match_id AND am.account_id = mp.account_id
		JOIN accounts a ON a.account_id = am.account_id AND a.is_active = 1
		JOIN matches m ON m.match_id = mp.match_id
		WHERE m.start_time >= ?1
			AND (?2 = 0 OR NOT EXISTS (SELECT 1 FROM hero_benchmarks hb WHERE hb.hero_id = mp.hero_id))
		ORDER BY mp.hero_id`, now.AddDate(0, 0, -benchmarkHeroDays).Unix(), onlyMissing)
	if err != nil {
		return err
	}
	for _, id := range heroIDs {
		raw, err := c.api.Get(ctx, "/benchmarks", url.Values{"hero_id": {strconv.FormatInt(id, 10)}})
		if err != nil {
			return fmt.Errorf("benchmarks for hero %d: %w", id, err)
		}
		var b struct {
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(raw, &b); err != nil || len(b.Result) == 0 {
			return fmt.Errorf("benchmarks for hero %d: no result (%v)", id, err)
		}
		if _, err := c.db.Write.ExecContext(ctx, `
			INSERT INTO hero_benchmarks (hero_id, fetched_at, data) VALUES (?, ?, ?)
			ON CONFLICT (hero_id) DO UPDATE SET fetched_at = excluded.fetched_at, data = excluded.data`,
			id, now.Unix(), compactJSON(b.Result)); err != nil {
			return err
		}
	}
	return nil
}

func (c *Collector) setState(ctx context.Context, key, value string) error {
	_, err := c.db.Write.ExecContext(ctx, `INSERT INTO collector_state (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
