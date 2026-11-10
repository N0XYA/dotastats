package stats

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"

	"dotastats/internal/analysis"
)

// Habits sum up the account's recent analyzed matches: a row per habit,
// always shown, and the notable patterns among them.
type Habits struct {
	Games    int // analyzed matches looked at
	Rows     []Habit
	Patterns []Pattern
}

// Habit is one measured habit, judged good, bad or neither (info).
type Habit struct {
	Label     string
	Value     string
	Severity  string
	Heuristic bool
}

// Pattern is a notable habit, e.g. losing the lane on one hero.
type Pattern struct {
	Text     string
	Severity string // good or bad
	Count    int
	Of       int
}

// Share is Count/Of.
func (p Pattern) Share() float64 { return float64(p.Count) / float64(max(p.Of, 1)) }

// patternGames is how many recent analyzed matches patterns look at.
const patternGames = 20

// LoadHabits measures habits over the account's last patternGames parsed
// matches: lane outcomes (overall and per hero), untraded deaths, early
// deaths, death streaks, wasted buybacks, time dead and the impact rating.
// Patterns are the notable ones, worst first.
func LoadHabits(ctx context.Context, db *sql.DB, accountID int64, c *Constants) (Habits, error) {
	type game struct {
		match, hero int64
		slot        int
	}
	var games []game
	err := each(ctx, db, `SELECT mp.match_id, mp.hero_id, mp.player_slot
		FROM match_players mp JOIN matches m ON m.match_id = mp.match_id
		WHERE mp.account_id = ? AND m.is_parsed = 1
			AND EXISTS (SELECT 1 FROM match_insights i WHERE i.match_id = m.match_id AND i.kind = ?)
		ORDER BY m.start_time DESC LIMIT ?`, func(r *sql.Rows) error {
		var g game
		if err := r.Scan(&g.match, &g.hero, &g.slot); err != nil {
			return err
		}
		games = append(games, g)
		return nil
	}, accountID, analysis.KindTimeDead, patternGames)
	if err != nil || len(games) == 0 {
		return Habits{}, err
	}

	var (
		lanes, lost, won      int
		heroLanes, heroLost   = map[int64]int{}, map[int64]int{}
		untraded, deaths      int
		streakGames, buybacks int
		earlyDeathGames       int
		deadShare             float64
		deadGames             int
		ratingSum, ratings    float64
	)
	for _, g := range games {
		seenStreak := false
		err := each(ctx, db, `SELECT kind, evidence FROM match_insights WHERE match_id = ? AND player_slot = ?`,
			func(r *sql.Rows) error {
				var kind, raw string
				if err := r.Scan(&kind, &raw); err != nil {
					return err
				}
				var ev map[string]any
				if err := json.Unmarshal([]byte(raw), &ev); err != nil {
					return err
				}
				switch kind {
				case analysis.KindLane:
					lanes++
					heroLanes[g.hero]++
					switch str(ev, "outcome") {
					case "lost":
						lost++
						heroLost[g.hero]++
					case "won":
						won++
					}
				case analysis.KindNoTradeTotal:
					untraded += int(num(ev, "untraded"))
					deaths += int(num(ev, "deaths"))
				case analysis.KindDeathStreak:
					if !seenStreak {
						streakGames++
						seenStreak = true
					}
				case analysis.KindWastedBuyback:
					buybacks++
				case analysis.KindTimeDead:
					deadShare += num(ev, "share")
					deadGames++
				case analysis.KindRating:
					ratingSum += num(ev, "score")
					ratings++
				}
				return nil
			}, g.match, g.slot)
		if err != nil {
			return Habits{}, err
		}
		var early int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM match_events
			WHERE match_id = ? AND kind = ? AND target_slot = ? AND time < 600`,
			g.match, analysis.EventDeath, g.slot).Scan(&early); err != nil {
			return Habits{}, err
		}
		if early > 0 {
			earlyDeathGames++
		}
	}

	n := len(games)
	h := Habits{Games: n}
	pct := func(a, b int) float64 { return float64(a) / float64(max(b, 1)) }
	judge := func(share, goodUpTo, badFrom float64) string {
		switch {
		case share >= badFrom:
			return analysis.Bad
		case share <= goodUpTo:
			return analysis.Good
		}
		return analysis.Info
	}
	if lanes > 0 {
		sev := analysis.Info
		switch {
		case lost >= 3 && float64(lost) >= 1.5*float64(won):
			sev = analysis.Bad
		case won >= 3 && float64(won) >= 1.5*float64(lost):
			sev = analysis.Good
		}
		h.Rows = append(h.Rows, Habit{"Линии к 10:00", fmt.Sprintf("%d выиграно · %d ровно · %d проиграно", won, lanes-won-lost, lost), sev, true})
	}
	if deaths > 0 {
		h.Rows = append(h.Rows, Habit{"Смерти без размена", fmt.Sprintf("%d из %d (%s)", untraded, deaths, Percent(pct(untraded, deaths))),
			judge(pct(untraded, deaths), 0.3, 0.5), true})
	}
	h.Rows = append(h.Rows, Habit{"Смерти до 10:00", fmt.Sprintf("в %d из %s", earlyDeathGames, Count(n, "игры", "игр", "игр")), judge(pct(earlyDeathGames, n), 0.25, 0.5), false})
	if deadGames > 0 {
		avg := deadShare / float64(deadGames)
		h.Rows = append(h.Rows, Habit{"Время в смерти", Decimal(avg*100, 1) + "% игрового времени", judge(avg, 0.07, 0.12), false})
	}
	h.Rows = append(h.Rows, Habit{"Серии смертей (3+ без убийств)", fmt.Sprintf("в %d из %s", streakGames, Count(n, "игры", "игр", "игр")), judge(pct(streakGames, n), 0.1, 0.3), true})
	bbSev := analysis.Info
	if buybacks >= 2 {
		bbSev = analysis.Bad
	}
	h.Rows = append(h.Rows, Habit{"Выкупы впустую", fmt.Sprint(buybacks), bbSev, true})
	if ratings > 0 {
		avg := ratingSum / ratings
		sev := analysis.Info
		switch {
		case avg >= analysis.RatingSolid:
			sev = analysis.Good
		case avg < analysis.RatingAverage:
			sev = analysis.Bad
		}
		h.Rows = append(h.Rows, Habit{"Среднее влияние", fmt.Sprintf("%.0f из 100", avg), sev, true})
	}

	var out []Pattern
	add := func(ok bool, p Pattern) {
		if ok {
			out = append(out, p)
		}
	}
	add(lanes >= 3 && lost*10 >= lanes*4, Pattern{fmt.Sprintf("Линия проиграна в %d из %s", lost, Count(lanes, "игры", "игр", "игр")), analysis.Bad, lost, lanes})
	add(lanes >= 3 && won*2 >= lanes, Pattern{fmt.Sprintf("Линия выиграна в %d из %s", won, Count(lanes, "игры", "игр", "игр")), analysis.Good, won, lanes})
	for hero, total := range heroLanes {
		add(total >= 3 && heroLost[hero]*10 >= total*6,
			Pattern{fmt.Sprintf("На %s линия проиграна в %d из %s", c.HeroName(int(hero)), heroLost[hero], Count(total, "игры", "игр", "игр")), analysis.Bad, heroLost[hero], total})
	}
	add(deaths >= 10 && untraded*2 >= deaths, Pattern{fmt.Sprintf("%d из %s без размена", untraded, Count(deaths, "смерти", "смертей", "смертей")), analysis.Bad, untraded, deaths})
	add(n >= 3 && earlyDeathGames*2 >= n, Pattern{fmt.Sprintf("Смерти до 10:00 в %d из %s", earlyDeathGames, Count(n, "игры", "игр", "игр")), analysis.Bad, earlyDeathGames, n})
	add(n >= 3 && streakGames*10 >= n*3, Pattern{fmt.Sprintf("3+ смерти подряд без убийств в %d из %s", streakGames, Count(n, "игры", "игр", "игр")), analysis.Bad, streakGames, n})
	add(buybacks >= 2, Pattern{fmt.Sprintf("%s впустую за %s", Count(buybacks, "выкуп", "выкупа", "выкупов"), Count(n, "игру", "игры", "игр")), analysis.Bad, buybacks, n})
	if deadGames > 0 {
		avg := deadShare / float64(deadGames)
		add(avg >= 0.12, Pattern{fmt.Sprintf("В среднем %s игрового времени в смерти", Percent(avg)), analysis.Bad, int(avg * 100), 100})
	}
	slices.SortStableFunc(out, func(a, b Pattern) int {
		if a.Severity != b.Severity {
			if a.Severity == analysis.Bad {
				return -1
			}
			return 1
		}
		switch {
		case a.Share() > b.Share():
			return -1
		case a.Share() < b.Share():
			return 1
		}
		return 0
	})
	h.Patterns = out
	return h, nil
}

// LaneRecord counts the account's lane outcomes on one hero.
type LaneRecord struct{ Won, Even, Lost int }

// Total is the number of reported lanes.
func (l LaneRecord) Total() int { return l.Won + l.Even + l.Lost }

// HeroLaneRecord counts lane outcomes on a hero over analyzed matches.
func HeroLaneRecord(ctx context.Context, db *sql.DB, accountID int64, heroID int) (LaneRecord, error) {
	var r LaneRecord
	err := each(ctx, db, `SELECT json_extract(i.evidence, '$.outcome')
		FROM match_players mp JOIN match_insights i ON i.match_id = mp.match_id AND i.player_slot = mp.player_slot
		WHERE mp.account_id = ? AND mp.hero_id = ? AND i.kind = ?`, func(rows *sql.Rows) error {
		var o sql.NullString
		if err := rows.Scan(&o); err != nil {
			return err
		}
		switch o.String {
		case "won":
			r.Won++
		case "lost":
			r.Lost++
		default:
			r.Even++
		}
		return nil
	}, accountID, heroID, analysis.KindLane)
	return r, err
}
