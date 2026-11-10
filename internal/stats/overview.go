package stats

import (
	"context"
	"database/sql"
	"time"
)

// Overview is the account's home page.
type Overview struct {
	All, Last30Days, Patch Record
	PatchName              string
	Recent                 []MatchRow // last 20, newest first
	TopHeroes              []HeroRow  // most played
	Averages               []Comparison
	DetailsPending         int // matches still waiting for details
}

// Comparison is an average over the last 30 days against the 30 days
// before.
type Comparison struct {
	Label            string
	Recent, Previous *float64 // nil without games in the window
	HigherIsBetter   bool
}

// Delta is Recent - Previous, or 0 when either is missing.
func (c Comparison) Delta() float64 {
	if c.Recent == nil || c.Previous == nil {
		return 0
	}
	return *c.Recent - *c.Previous
}

// Improved reports whether the change goes the good way.
func (c Comparison) Improved() bool { return (c.Delta() > 0) == c.HigherIsBetter }

// LoadOverview gathers the Overview page for an account.
func LoadOverview(ctx context.Context, db *sql.DB, accountID int64, consts *Constants, now time.Time) (Overview, error) {
	var o Overview
	f := Filter{AccountID: accountID}
	var err error
	if o.All, err = record(ctx, db, f); err != nil {
		return o, err
	}
	f30 := f
	f30.From = now.AddDate(0, 0, -30)
	if o.Last30Days, err = record(ctx, db, f30); err != nil {
		return o, err
	}
	if p, ok := consts.CurrentPatch(); ok {
		fp := f
		fp.Patch = &p.ID
		o.PatchName = p.Name
		if o.Patch, err = record(ctx, db, fp); err != nil {
			return o, err
		}
	}
	if o.Recent, err = matchRows(ctx, db, "mp.account_id = ? ORDER BY m.start_time DESC, m.match_id DESC LIMIT 20", accountID); err != nil {
		return o, err
	}
	heroes, err := Heroes(ctx, db, f, "games")
	if err != nil {
		return o, err
	}
	o.TopHeroes = heroes[:min(len(heroes), 8)]
	if o.Averages, err = averages(ctx, db, accountID, now); err != nil {
		return o, err
	}
	err = db.QueryRowContext(ctx, `SELECT count(*) FROM match_players mp JOIN matches m ON m.match_id = mp.match_id
		WHERE mp.account_id = ? AND m.details_at IS NULL`, accountID).Scan(&o.DetailsPending)
	return o, err
}

func record(ctx context.Context, db *sql.DB, f Filter) (Record, error) {
	where, args := f.where()
	var r Record
	err := db.QueryRowContext(ctx, `SELECT count(m.radiant_win), coalesce(sum(`+winExpr+`), 0)
		FROM match_players mp JOIN matches m ON m.match_id = mp.match_id WHERE `+where, args...).Scan(&r.Games, &r.Wins)
	return r, err
}

func averages(ctx context.Context, db *sql.DB, accountID int64, now time.Time) ([]Comparison, error) {
	metrics := []Comparison{
		{Label: "Убийства", HigherIsBetter: true},
		{Label: "Смерти"},
		{Label: "Помощь", HigherIsBetter: true},
		{Label: "GPM", HigherIsBetter: true},
		{Label: "XPM", HigherIsBetter: true},
		{Label: "Добивания", HigherIsBetter: true},
		{Label: "Урон по героям", HigherIsBetter: true},
	}
	for _, w := range []struct {
		from, to time.Time
		recent   bool
	}{
		{now.AddDate(0, 0, -30), now, true},
		{now.AddDate(0, 0, -60), now.AddDate(0, 0, -30), false},
	} {
		vals := make([]sql.NullFloat64, len(metrics))
		if err := db.QueryRowContext(ctx, `SELECT avg(mp.kills), avg(mp.deaths), avg(mp.assists), avg(mp.gold_per_min),
				avg(mp.xp_per_min), avg(mp.last_hits), avg(mp.hero_damage)
			FROM match_players mp JOIN matches m ON m.match_id = mp.match_id
			WHERE mp.account_id = ? AND m.start_time >= ? AND m.start_time < ?`,
			accountID, w.from.Unix(), w.to.Unix()).Scan(&vals[0], &vals[1], &vals[2], &vals[3], &vals[4], &vals[5], &vals[6]); err != nil {
			return nil, err
		}
		for i, v := range vals {
			if !v.Valid {
				continue
			}
			f := v.Float64
			if w.recent {
				metrics[i].Recent = &f
			} else {
				metrics[i].Previous = &f
			}
		}
	}
	return metrics, nil
}
