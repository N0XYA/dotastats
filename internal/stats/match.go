package stats

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// ErrNotFound means the match isn't stored.
var ErrNotFound = errors.New("not found")

// MatchDetail is the Match detail page.
type MatchDetail struct {
	MatchID     int64
	StartTime   time.Time
	Duration    int
	GameMode    int
	LobbyType   int
	Patch       *int
	RadiantWin  *bool
	AvgRankTier *int
	HasDetails  bool
	IsParsed    bool
	ParseState  string
	RawPruned   bool

	Radiant, Dire []PlayerRow
	// Team advantage per minute, positive for Radiant; nil until parsed.
	GoldAdv, XPAdv []float64
	// Per-player curves of parsed matches, by slot.
	Series []PlayerSeries
	// Benchmark percentiles of each player that has them.
	Benchmarks bool
}

// RadiantScore and DireScore sum the teams' kills.
func (d MatchDetail) RadiantScore() int { return teamKills(d.Radiant) }

// DireScore sums Dire's kills.
func (d MatchDetail) DireScore() int { return teamKills(d.Dire) }

func teamKills(ps []PlayerRow) int {
	n := 0
	for _, p := range ps {
		n += p.Kills
	}
	return n
}

// PlayerRow is one scoreboard line.
type PlayerRow struct {
	Slot        int
	AccountID   *int64
	Name        string
	Tracked     bool // one of the instance's accounts
	HeroID      int
	Kills       int
	Deaths      int
	Assists     int
	LastHits    *int
	Denies      *int
	GPM, XPM    *int
	NetWorth    *int
	HeroDamage  *int
	TowerDamage *int
	HeroHealing *int
	Level       *int
	RankTier    *int
	PartySize   *int
	LaneRole    *int
	Items       []ItemSlot // the 6 inventory slots
	Backpack    []ItemSlot
	Neutral     ItemSlot
	Enhancement ItemSlot // second neutral slot
	Benchmarks  map[string]float64
	Purchases   []Purchase // tracked players of parsed matches only
}

// Purchase is one entry of a player's purchase log.
type Purchase struct {
	Time int    // seconds from the horn
	Item string // item key
}

// ItemSlot is an item in a slot and when it was first bought, if known.
type ItemSlot struct {
	ID       int
	BoughtAt *int // seconds
}

// PlayerSeries are a player's per-minute values.
type PlayerSeries struct {
	Slot    int       `json:"slot"`
	HeroID  int       `json:"hero_id"`
	Name    string    `json:"name"`
	Tracked bool      `json:"tracked"`
	Gold    []float64 `json:"gold"`
	LH      []float64 `json:"lh"`
}

// LoadMatch reads one match with all stored players.
func LoadMatch(ctx context.Context, db *sql.DB, matchID int64) (MatchDetail, error) {
	d := MatchDetail{MatchID: matchID}
	var start int64
	var patch, rank sql.NullInt64
	var radiantWin sql.NullBool
	err := db.QueryRowContext(ctx, `SELECT start_time, duration, game_mode, lobby_type, patch, radiant_win,
			avg_rank_tier, details_at IS NOT NULL, is_parsed, coalesce(parse_state, ''),
			raw_pruned_at IS NOT NULL
		FROM matches WHERE match_id = ?`, matchID).Scan(&start, &d.Duration, &d.GameMode, &d.LobbyType,
		&patch, &radiantWin, &rank, &d.HasDetails, &d.IsParsed, &d.ParseState, &d.RawPruned)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	d.StartTime, d.Patch, d.RadiantWin, d.AvgRankTier = time.Unix(start, 0), nullInt(patch), nullBool(radiantWin), nullInt(rank)

	bought, err := firstPurchases(ctx, db, matchID)
	if err != nil {
		return d, err
	}
	bench := map[int]map[string]float64{}
	if err := each(ctx, db, `SELECT player_slot, metric, pct FROM player_benchmarks WHERE match_id = ? AND pct IS NOT NULL`,
		func(r *sql.Rows) error {
			var slot int
			var metric string
			var pct float64
			if err := r.Scan(&slot, &metric, &pct); err != nil {
				return err
			}
			if bench[slot] == nil {
				bench[slot] = map[string]float64{}
			}
			bench[slot][metric] = pct
			d.Benchmarks = true
			return nil
		}, matchID); err != nil {
		return d, err
	}

	err = each(ctx, db, `SELECT mp.player_slot, mp.account_id, coalesce(mp.personaname, ''), a.account_id IS NOT NULL,
			mp.hero_id, mp.kills, mp.deaths, mp.assists, mp.last_hits, mp.denies, mp.gold_per_min, mp.xp_per_min,
			mp.net_worth, mp.hero_damage, mp.tower_damage, mp.hero_healing, mp.level, mp.rank_tier, mp.party_size,
			mp.lane_role, coalesce(mp.item_0, 0), coalesce(mp.item_1, 0), coalesce(mp.item_2, 0), coalesce(mp.item_3, 0),
			coalesce(mp.item_4, 0), coalesce(mp.item_5, 0), coalesce(mp.backpack_0, 0), coalesce(mp.backpack_1, 0),
			coalesce(mp.backpack_2, 0), coalesce(mp.item_neutral, 0), coalesce(mp.item_neutral2, 0)
		FROM match_players mp LEFT JOIN accounts a ON a.account_id = mp.account_id
		WHERE mp.match_id = ? ORDER BY mp.player_slot`, func(r *sql.Rows) error {
		var p PlayerRow
		var account sql.NullInt64
		var lh, dn, gpm, xpm, nw, hd, td, hh, lvl, rt, party, role sql.NullInt64
		items := make([]int, 11)
		if err := r.Scan(&p.Slot, &account, &p.Name, &p.Tracked, &p.HeroID, &p.Kills, &p.Deaths, &p.Assists,
			&lh, &dn, &gpm, &xpm, &nw, &hd, &td, &hh, &lvl, &rt, &party, &role,
			&items[0], &items[1], &items[2], &items[3], &items[4], &items[5], &items[6], &items[7], &items[8],
			&items[9], &items[10]); err != nil {
			return err
		}
		if account.Valid {
			p.AccountID = &account.Int64
		}
		p.LastHits, p.Denies, p.GPM, p.XPM = nullInt(lh), nullInt(dn), nullInt(gpm), nullInt(xpm)
		p.NetWorth, p.HeroDamage, p.TowerDamage, p.HeroHealing = nullInt(nw), nullInt(hd), nullInt(td), nullInt(hh)
		p.Level, p.RankTier, p.PartySize, p.LaneRole = nullInt(lvl), nullInt(rt), nullInt(party), nullInt(role)
		slot := func(id int) ItemSlot { return ItemSlot{ID: id, BoughtAt: bought[p.Slot][id]} }
		for _, id := range items[:6] {
			p.Items = append(p.Items, slot(id))
		}
		for _, id := range items[6:9] {
			p.Backpack = append(p.Backpack, slot(id))
		}
		p.Neutral, p.Enhancement = slot(items[9]), slot(items[10])
		p.Benchmarks = bench[p.Slot]
		if p.Tracked {
			if p.Purchases, err = purchases(ctx, db, matchID, p.Slot); err != nil {
				return err
			}
		}
		if p.Slot < 128 {
			d.Radiant = append(d.Radiant, p)
		} else {
			d.Dire = append(d.Dire, p)
		}
		return nil
	}, matchID)
	if err != nil {
		return d, err
	}

	var gold, xp sql.NullString
	err = db.QueryRowContext(ctx, "SELECT radiant_gold_adv, radiant_xp_adv FROM match_timeseries WHERE match_id = ?", matchID).Scan(&gold, &xp)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return d, err
	}
	d.GoldAdv, d.XPAdv = decodeSeries(gold), decodeSeries(xp)

	names := map[int]PlayerRow{}
	for _, p := range append(append([]PlayerRow{}, d.Radiant...), d.Dire...) {
		names[p.Slot] = p
	}
	err = each(ctx, db, `SELECT player_slot, gold_t, lh_t FROM player_timeseries WHERE match_id = ? ORDER BY player_slot`,
		func(r *sql.Rows) error {
			var s PlayerSeries
			var gold, lh sql.NullString
			if err := r.Scan(&s.Slot, &gold, &lh); err != nil {
				return err
			}
			s.HeroID, s.Name, s.Tracked = names[s.Slot].HeroID, names[s.Slot].Name, names[s.Slot].Tracked
			s.Gold, s.LH = decodeSeries(gold), decodeSeries(lh)
			d.Series = append(d.Series, s)
			return nil
		}, matchID)
	return d, err
}

func purchases(ctx context.Context, db *sql.DB, matchID int64, slot int) ([]Purchase, error) {
	var out []Purchase
	err := each(ctx, db, "SELECT time, item FROM item_purchases WHERE match_id = ? AND player_slot = ? ORDER BY seq",
		func(r *sql.Rows) error {
			var p Purchase
			if err := r.Scan(&p.Time, &p.Item); err != nil {
				return err
			}
			out = append(out, p)
			return nil
		}, matchID, slot)
	return out, err
}

// firstPurchases maps slot -> item ID -> first purchase second.
func firstPurchases(ctx context.Context, db *sql.DB, matchID int64) (map[int]map[int]*int, error) {
	out := map[int]map[int]*int{}
	err := each(ctx, db, `SELECT ip.player_slot, i.id, min(ip.time)
		FROM item_purchases ip JOIN items i ON i.name = ip.item
		WHERE ip.match_id = ? GROUP BY ip.player_slot, i.id`, func(r *sql.Rows) error {
		var slot, id, t int
		if err := r.Scan(&slot, &id, &t); err != nil {
			return err
		}
		if out[slot] == nil {
			out[slot] = map[int]*int{}
		}
		out[slot][id] = &t
		return nil
	}, matchID)
	return out, err
}

func decodeSeries(s sql.NullString) []float64 {
	if !s.Valid {
		return nil
	}
	var out []float64
	if json.Unmarshal([]byte(s.String), &out) != nil {
		return nil
	}
	return out
}

// Player returns the scoreboard row in a slot, or the zero row.
func (d MatchDetail) Player(slot int) PlayerRow {
	for _, p := range append(append([]PlayerRow{}, d.Radiant...), d.Dire...) {
		if p.Slot == slot {
			return p
		}
	}
	return PlayerRow{Slot: slot}
}
