package stats

import (
	"cmp"
	"context"
	"database/sql"
	"slices"
	"strings"
	"time"
)

// HeroRow aggregates the account's games on one hero.
type HeroRow struct {
	HeroID        int
	Record        Record
	Kills, Deaths float64 // averages
	Assists       float64
	GPM, XPM      *float64 // nil when no game has them
	LastPlayed    time.Time

	killSum, deathSum, assistSum int
}

// KDA is (kills + assists) / deaths over all games, deaths at least 1.
func (h HeroRow) KDA() float64 { return float64(h.killSum+h.assistSum) / float64(max(h.deathSum, 1)) }

// HeroSorts are the Heroes page's sort keys, mapped to SQL.
var HeroSorts = map[string]string{
	"games": "count(*) DESC",
	"wr":    "coalesce(sum(" + winExpr + ") * 1.0 / nullif(count(m.radiant_win), 0), 0) DESC, count(*) DESC",
	"kda":   "(sum(mp.kills) + sum(mp.assists)) * 1.0 / max(sum(mp.deaths), 1) DESC",
	"gpm":   "avg(mp.gold_per_min) DESC",
	"xpm":   "avg(mp.xp_per_min) DESC",
	"last":  "max(m.start_time) DESC",
}

// Heroes aggregates the account's games under f per hero, sorted by sort
// (a HeroSorts key; unknown keys sort by games).
func Heroes(ctx context.Context, db *sql.DB, f Filter, sort string) ([]HeroRow, error) {
	order, ok := HeroSorts[sort]
	if !ok {
		order = HeroSorts["games"]
	}
	where, args := f.where()
	var out []HeroRow
	err := each(ctx, db, `SELECT mp.hero_id, count(m.radiant_win), coalesce(sum(`+winExpr+`), 0),
			sum(mp.kills), sum(mp.deaths), sum(mp.assists), count(*),
			avg(mp.gold_per_min), avg(mp.xp_per_min), max(m.start_time)
		FROM match_players mp JOIN matches m ON m.match_id = mp.match_id
		WHERE `+where+`
		GROUP BY mp.hero_id ORDER BY `+order+`, mp.hero_id`, func(r *sql.Rows) error {
		var h HeroRow
		var n int
		var gpm, xpm sql.NullFloat64
		var last int64
		if err := r.Scan(&h.HeroID, &h.Record.Games, &h.Record.Wins, &h.killSum, &h.deathSum, &h.assistSum, &n, &gpm, &xpm, &last); err != nil {
			return err
		}
		h.Kills, h.Deaths, h.Assists = float64(h.killSum)/float64(n), float64(h.deathSum)/float64(n), float64(h.assistSum)/float64(n)
		if gpm.Valid {
			h.GPM = &gpm.Float64
		}
		if xpm.Valid {
			h.XPM = &xpm.Float64
		}
		h.LastPlayed = time.Unix(last, 0)
		out = append(out, h)
		return nil
	}, args...)
	return out, err
}

// HeroDetail is the Hero detail page for one account and hero.
type HeroDetail struct {
	Summary     HeroRow
	Games       []TrendPoint // oldest first, for the trend chart
	ParsedGames int          // parsed and stored in full, so purchase logs exist
	CoreItems   []ItemTiming
	With        []Matchup // allies on the account's team
	Against     []Matchup
	Benchmarks  []BenchmarkPoint // oldest first
}

// TrendPoint is one game on the hero.
type TrendPoint struct {
	MatchID int64     `json:"match_id"`
	Time    time.Time `json:"time"`
	Win     bool      `json:"win"`
	GPM     *int      `json:"gpm"`
	KDA     float64   `json:"kda"`
}

// ItemTiming is a core item: bought in at least coreItemShare of parsed
// games, compared against the account's own average on this hero.
type ItemTiming struct {
	Item       Item
	Games      int
	Share      float64 // of parsed games on the hero
	AvgTime    float64 // seconds, first purchase, all games
	RecentTime float64 // the same over the last recentItemGames games with it
	RecentN    int
}

// Delta is RecentTime - AvgTime in seconds; negative means faster lately.
func (t ItemTiming) Delta() float64 { return t.RecentTime - t.AvgTime }

// Matchup is the account's record with or against a hero.
type Matchup struct {
	HeroID int
	Record Record
}

// BenchmarkPoint holds a game's benchmark percentiles (0..1) by metric.
type BenchmarkPoint struct {
	MatchID int64              `json:"match_id"`
	Time    time.Time          `json:"time"`
	Pct     map[string]float64 `json:"pct"`
}

// BenchmarkMetrics are charted on the Hero detail page.
var BenchmarkMetrics = []string{"gold_per_min", "xp_per_min", "last_hits_per_min", "hero_damage_per_min"}

const (
	coreItemShare   = 0.25
	coreItemMinCost = 1200
	recentItemGames = 5
	maxMatchups     = 15
)

// LoadHeroDetail gathers the Hero detail page; f must set HeroID.
func LoadHeroDetail(ctx context.Context, db *sql.DB, f Filter, consts *Constants) (HeroDetail, error) {
	var d HeroDetail
	rows, err := Heroes(ctx, db, f, "games")
	if err != nil || len(rows) == 0 {
		return d, err
	}
	d.Summary = rows[0]
	where, args := f.where()

	games, err := matchRows(ctx, db, where+" ORDER BY m.start_time, m.match_id", args...)
	if err != nil {
		return d, err
	}
	for _, g := range games {
		if g.Win == nil {
			continue
		}
		d.Games = append(d.Games, TrendPoint{MatchID: g.MatchID, Time: g.StartTime, Win: *g.Win, GPM: g.GPM, KDA: g.KDA()})
		if g.IsParsed && g.HasDetails {
			d.ParsedGames++
		}
	}

	if d.CoreItems, err = coreItems(ctx, db, where, args, consts, d.ParsedGames); err != nil {
		return d, err
	}
	if d.With, d.Against, err = matchups(ctx, db, where, args); err != nil {
		return d, err
	}
	d.Benchmarks, err = benchmarks(ctx, db, where, args)
	return d, err
}

func coreItems(ctx context.Context, db *sql.DB, where string, args []any, consts *Constants, parsedGames int) ([]ItemTiming, error) {
	if parsedGames == 0 {
		return nil, nil
	}
	// First purchase of each item per game, newest game first.
	type first struct {
		item string
		time int
	}
	byItem := map[string][]int{}
	err := each(ctx, db, `SELECT ip.item, min(ip.time)
		FROM match_players mp JOIN matches m ON m.match_id = mp.match_id
		JOIN item_purchases ip ON ip.match_id = mp.match_id AND ip.player_slot = mp.player_slot
		WHERE `+where+`
		GROUP BY mp.match_id, ip.item
		ORDER BY m.start_time DESC`, func(r *sql.Rows) error {
		var f first
		if err := r.Scan(&f.item, &f.time); err != nil {
			return err
		}
		byItem[f.item] = append(byItem[f.item], f.time)
		return nil
	}, args...)
	if err != nil {
		return nil, err
	}
	var out []ItemTiming
	for key, times := range byItem {
		it := consts.ItemByKey(key)
		share := float64(len(times)) / float64(parsedGames)
		if it.Cost < coreItemMinCost || strings.HasPrefix(key, "recipe_") || share < coreItemShare {
			continue
		}
		recent := times[:min(len(times), recentItemGames)]
		out = append(out, ItemTiming{
			Item: it, Games: len(times), Share: share,
			AvgTime: mean(times), RecentTime: mean(recent), RecentN: len(recent),
		})
	}
	slices.SortFunc(out, func(a, b ItemTiming) int { return cmp.Compare(a.AvgTime, b.AvgTime) })
	return out, nil
}

func mean(xs []int) float64 {
	sum := 0
	for _, x := range xs {
		sum += x
	}
	return float64(sum) / float64(max(len(xs), 1))
}

func matchups(ctx context.Context, db *sql.DB, where string, args []any) (with, against []Matchup, err error) {
	err = each(ctx, db, `SELECT o.hero_id, (o.player_slot < 128) = (mp.player_slot < 128),
			count(m.radiant_win), coalesce(sum(`+winExpr+`), 0)
		FROM match_players mp JOIN matches m ON m.match_id = mp.match_id
		JOIN match_players o ON o.match_id = mp.match_id AND o.player_slot != mp.player_slot
		WHERE `+where+`
		GROUP BY o.hero_id, 2
		ORDER BY 3 DESC, o.hero_id`, func(r *sql.Rows) error {
		var m Matchup
		var ally bool
		if err := r.Scan(&m.HeroID, &ally, &m.Record.Games, &m.Record.Wins); err != nil {
			return err
		}
		if ally && len(with) < maxMatchups {
			with = append(with, m)
		} else if !ally && len(against) < maxMatchups {
			against = append(against, m)
		}
		return nil
	}, args...)
	return with, against, err
}

func benchmarks(ctx context.Context, db *sql.DB, where string, args []any) ([]BenchmarkPoint, error) {
	var out []BenchmarkPoint
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(BenchmarkMetrics)), ",")
	qargs := append([]any{}, args...)
	for _, m := range BenchmarkMetrics {
		qargs = append(qargs, m)
	}
	err := each(ctx, db, `SELECT m.match_id, m.start_time, pb.metric, pb.pct
		FROM match_players mp JOIN matches m ON m.match_id = mp.match_id
		JOIN player_benchmarks pb ON pb.match_id = mp.match_id AND pb.player_slot = mp.player_slot
		WHERE `+where+` AND pb.pct IS NOT NULL AND pb.metric IN (`+placeholders+`)
		ORDER BY m.start_time, m.match_id`, func(r *sql.Rows) error {
		var id, start int64
		var metric string
		var pct float64
		if err := r.Scan(&id, &start, &metric, &pct); err != nil {
			return err
		}
		if len(out) == 0 || out[len(out)-1].MatchID != id {
			out = append(out, BenchmarkPoint{MatchID: id, Time: time.Unix(start, 0), Pct: map[string]float64{}})
		}
		out[len(out)-1].Pct[metric] = pct
		return nil
	}, qargs...)
	return out, err
}
