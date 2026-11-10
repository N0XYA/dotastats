package collector

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/klauspost/compress/zstd"

	"dotastats/internal/analysis"
	"dotastats/internal/db"
	"dotastats/internal/opendota"
)

// anonymousAccountID is the 32-bit "anonymous" sentinel Valve uses for
// hidden players. OpenDota's recorded responses so far never contained it
// (or a null), so both are mapped to NULL defensively.
const anonymousAccountID = 4294967295

// extract replaces everything derived from a match's details: its matches
// row, all player rows and the parsed-only tables. It is deterministic, so
// reextract over the same raw JSON produces identical tables. Pipeline
// state (details_at, parse_state, raw) is the caller's business.
func extract(ctx context.Context, tx *sql.Tx, m opendota.Match) error {
	ex := &execer{ctx: ctx, tx: tx}
	ex.exec(`
		INSERT INTO matches (match_id, start_time, duration, game_mode, lobby_type, radiant_win, patch, region, is_parsed)
		VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9)
		ON CONFLICT (match_id) DO UPDATE SET
			start_time = excluded.start_time, duration = excluded.duration,
			game_mode = excluded.game_mode, lobby_type = excluded.lobby_type,
			radiant_win = excluded.radiant_win, patch = coalesce(excluded.patch, patch),
			region = excluded.region, is_parsed = excluded.is_parsed`,
		m.MatchID, m.StartTime, m.Duration, m.GameMode, m.LobbyType, m.RadiantWin, m.Patch, m.Region, m.Parsed())

	for _, table := range []string{"match_players", "match_timeseries", "player_timeseries", "item_purchases", "wards",
		"player_benchmarks", "match_events", "match_insights"} {
		ex.exec("DELETE FROM "+table+" WHERE match_id = ?", m.MatchID)
	}

	if gold, xp := compactJSON(m.RadiantGoldAdv), compactJSON(m.RadiantXPAdv); gold != nil || xp != nil {
		ex.exec("INSERT INTO match_timeseries (match_id, radiant_gold_adv, radiant_xp_adv) VALUES (?, ?, ?)",
			m.MatchID, gold, xp)
	}

	for _, p := range m.Players {
		var accountID any
		if p.AccountID != nil && *p.AccountID != anonymousAccountID {
			accountID = *p.AccountID
		}
		ex.exec(`
			INSERT INTO match_players (match_id, player_slot, account_id, hero_id, kills, deaths, assists,
				leaver_status, party_size, personaname, last_hits, denies, gold_per_min, xp_per_min,
				net_worth, hero_damage, tower_damage, hero_healing, level,
				item_0, item_1, item_2, item_3, item_4, item_5, backpack_0, backpack_1, backpack_2,
				item_neutral, item_neutral2, lane, lane_role, is_roaming, rank_tier)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.MatchID, p.PlayerSlot, accountID, p.HeroID, p.Kills, p.Deaths, p.Assists,
			p.LeaverStatus, p.PartySize, nullString(p.PersonaName), p.LastHits, p.Denies, p.GoldPerMin, p.XPPerMin,
			p.NetWorth, p.HeroDamage, p.TowerDamage, p.HeroHealing, p.Level,
			p.Item0, p.Item1, p.Item2, p.Item3, p.Item4, p.Item5, p.Backpack0, p.Backpack1, p.Backpack2,
			p.ItemNeutral, p.ItemNeutral2, p.Lane, p.LaneRole, p.IsRoaming, p.RankTier)

		gold, xp, lh, dn := compactJSON(p.GoldT), compactJSON(p.XPT), compactJSON(p.LHT), compactJSON(p.DNT)
		if gold != nil || xp != nil || lh != nil || dn != nil {
			ex.exec("INSERT INTO player_timeseries (match_id, player_slot, gold_t, xp_t, lh_t, dn_t) VALUES (?, ?, ?, ?, ?, ?)",
				m.MatchID, p.PlayerSlot, gold, xp, lh, dn)
		}
		for i, buy := range p.PurchaseLog {
			ex.exec("INSERT INTO item_purchases (match_id, player_slot, seq, time, item) VALUES (?, ?, ?, ?, ?)",
				m.MatchID, p.PlayerSlot, i, buy.Time, buy.Key)
		}
		for _, w := range []struct {
			kind         string
			placed, gone []opendota.WardEvent
		}{{"obs", p.ObsLog, p.ObsLeftLog}, {"sen", p.SenLog, p.SenLeftLog}} {
			leftAt := wardRemovals(w.placed, w.gone)
			for i, e := range w.placed {
				ex.exec("INSERT INTO wards (match_id, player_slot, type, seq, time, x, y, left_time) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
					m.MatchID, p.PlayerSlot, w.kind, i, e.Time, e.X, e.Y, leftAt[i])
			}
		}
		metrics := make([]string, 0, len(p.Benchmarks))
		for k := range p.Benchmarks {
			metrics = append(metrics, k)
		}
		sort.Strings(metrics)
		for _, k := range metrics {
			b := p.Benchmarks[k]
			ex.exec("INSERT INTO player_benchmarks (match_id, player_slot, metric, raw, pct) VALUES (?, ?, ?, ?, ?)",
				m.MatchID, p.PlayerSlot, k, b.Raw, b.Pct)
		}
	}
	if ex.err != nil {
		return fmt.Errorf("extract match %d: %w", m.MatchID, ex.err)
	}
	return analyze(ctx, tx, m)
}

// analyze stores the match's events and insights.
func analyze(ctx context.Context, tx *sql.Tx, m opendota.Match) error {
	heroes := analysis.Heroes{}
	rows, err := tx.QueryContext(ctx, "SELECT name, id FROM heroes")
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		var id int
		if err := rows.Scan(&name, &id); err != nil {
			rows.Close()
			return err
		}
		heroes[name] = id
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	r := analysis.Analyze(m, heroes)
	ex := &execer{ctx: ctx, tx: tx}
	for i, e := range r.Events {
		ex.exec(`INSERT INTO match_events (match_id, seq, time, kind, player_slot, target_slot, key, value, data)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.MatchID, i, e.Time, e.Kind, e.Slot, e.Target, nullString(e.Key), e.Value, jsonOrNil(e.Data))
	}
	for i, in := range r.Insights {
		ex.exec(`INSERT INTO match_insights (match_id, seq, player_slot, kind, time, severity, evidence, detector_version)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			m.MatchID, i, in.Slot, in.Kind, in.Time, in.Severity, jsonOrNil(in.Evidence), analysis.Version)
	}
	if ex.err != nil {
		return fmt.Errorf("analyze match %d: %w", m.MatchID, ex.err)
	}
	return nil
}

// jsonOrNil marshals v; maps marshal with sorted keys, so stored JSON is
// deterministic. An empty value becomes "{}" for evidence and NULL for data.
func jsonOrNil(v map[string]any) any {
	if v == nil {
		return "{}"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// wardRemovals pairs each placement with the first later removal of the
// same entity and returns its time, or nil when none was logged.
func wardRemovals(placed, gone []opendota.WardEvent) []any {
	byHandle := map[int64][]int{}
	for _, e := range gone {
		byHandle[e.EHandle] = append(byHandle[e.EHandle], e.Time)
	}
	out := make([]any, len(placed))
	for i, e := range placed {
		times := byHandle[e.EHandle]
		for j, t := range times {
			if t >= e.Time {
				out[i] = t
				byHandle[e.EHandle] = append(times[:j:j], times[j+1:]...)
				break
			}
		}
	}
	return out
}

// compactJSON returns raw compacted, or nil for a missing or null
// value.
func compactJSON(raw json.RawMessage) any {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil
	}
	return buf.String()
}

// execer runs statements until the first error, so a long extraction reads
// as a list of statements.
type execer struct {
	ctx context.Context
	tx  *sql.Tx
	err error
}

func (e *execer) exec(query string, args ...any) {
	if e.err == nil {
		_, e.err = e.tx.ExecContext(e.ctx, query, args...)
	}
}

// Raw JSON is stored zstd-compressed. The coders are created on first use
// with one worker each to keep memory low.
var (
	encoder = sync.OnceValue(func() *zstd.Encoder {
		e, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
		if err != nil {
			panic(err) // only fails on invalid options
		}
		return e
	})
	decoder = sync.OnceValue(func() *zstd.Decoder {
		d, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
		if err != nil {
			panic(err)
		}
		return d
	})
)

func compress(raw []byte) []byte { return encoder().EncodeAll(raw, nil) }

func decompress(blob []byte) ([]byte, error) { return decoder().DecodeAll(blob, nil) }

// ReextractReport summarizes a Reextract run.
type ReextractReport struct {
	Matches int // rebuilt from raw JSON
	Skipped int // with details but no raw JSON left; their rows were kept
}

// reextractBatch matches share one transaction.
const reextractBatch = 100

// Reextract rebuilds the normalized tables of every match from its stored
// raw JSON, with no API calls. Use it after changing the extractor or the
// schema. Matches whose raw JSON was pruned keep their rows.
func Reextract(ctx context.Context, store *db.DB) (ReextractReport, error) {
	var rep ReextractReport
	ids, err := queryIDs(ctx, store.Read, "SELECT match_id FROM match_raw ORDER BY match_id")
	if err != nil {
		return rep, err
	}
	for len(ids) > 0 {
		n := min(reextractBatch, len(ids))
		if err := reextractBatchTx(ctx, store, ids[:n]); err != nil {
			return rep, err
		}
		rep.Matches += n
		ids = ids[n:]
	}
	err = store.Read.QueryRowContext(ctx, `
		SELECT count(*) FROM matches m
		WHERE m.details_at IS NOT NULL
			AND NOT EXISTS (SELECT 1 FROM match_raw r WHERE r.match_id = m.match_id)`).Scan(&rep.Skipped)
	return rep, err
}

func reextractBatchTx(ctx context.Context, store *db.DB, ids []int64) error {
	tx, err := store.Write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		var blob []byte
		if err := tx.QueryRowContext(ctx, "SELECT data FROM match_raw WHERE match_id = ?", id).Scan(&blob); err != nil {
			return fmt.Errorf("match %d: %w", id, err)
		}
		m, err := decodeRaw(blob)
		if err != nil {
			return fmt.Errorf("match %d: %w", id, err)
		}
		if err := extract(ctx, tx, m); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func decodeRaw(blob []byte) (opendota.Match, error) {
	raw, err := decompress(blob)
	if err != nil {
		return opendota.Match{}, fmt.Errorf("decompress raw: %w", err)
	}
	var m opendota.Match
	if err := json.Unmarshal(raw, &m); err != nil {
		return opendota.Match{}, fmt.Errorf("decode raw: %w", err)
	}
	return m, nil
}

func queryIDs(ctx context.Context, q *sql.DB, query string, args ...any) ([]int64, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
