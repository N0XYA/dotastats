// Package stats holds the read-only queries behind the web pages. Every
// function takes the read pool and filters by one tracked account: "our"
// rows are match_players rows with that account_id.
package stats

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// winExpr is 1 for a win, 0 for a loss and NULL when the result is
// unknown, for the player row mp of match m.
const winExpr = "(m.radiant_win = (mp.player_slot < 128))"

// Record counts games with a known result.
type Record struct {
	Games int
	Wins  int
}

// Losses is Games minus Wins.
func (r Record) Losses() int { return r.Games - r.Wins }

// WinRate is between 0 and 1; 0 without games.
func (r Record) WinRate() float64 {
	if r.Games == 0 {
		return 0
	}
	return float64(r.Wins) / float64(r.Games)
}

// Filter is the shared filter bar. Zero values mean "any".
type Filter struct {
	AccountID int64
	From, To  time.Time // From inclusive, To exclusive
	Patch     *int      // patch IDs start at 0
	GameMode  *int
	Ranked    string // "", "ranked" or "unranked"
	HeroID    int
	LaneRole  int
	Result    string // "", "win" or "loss"
	PartySize int    // 1..5
}

// rankedLobby is lobby_type_ranked in /constants/lobby_type.
const rankedLobby = 7

// FilterFromQuery reads the filter bar's query parameters; invalid values
// are ignored. Dates are local calendar days.
func FilterFromQuery(q url.Values, accountID int64) Filter {
	f := Filter{AccountID: accountID}
	day := func(key string) time.Time {
		t, err := time.ParseInLocation(time.DateOnly, q.Get(key), time.Local)
		if err != nil {
			return time.Time{}
		}
		return t
	}
	f.From = day("from")
	if to := day("to"); !to.IsZero() {
		f.To = to.AddDate(0, 0, 1)
	}
	optInt := func(key string) *int {
		n, err := strconv.Atoi(q.Get(key))
		if err != nil || n < 0 {
			return nil
		}
		return &n
	}
	f.Patch = optInt("patch")
	f.GameMode = optInt("mode")
	if v := q.Get("ranked"); v == "ranked" || v == "unranked" {
		f.Ranked = v
	}
	if p := optInt("hero"); p != nil {
		f.HeroID = *p
	}
	if p := optInt("role"); p != nil && *p <= 4 {
		f.LaneRole = *p
	}
	if v := q.Get("result"); v == "win" || v == "loss" {
		f.Result = v
	}
	if p := optInt("party"); p != nil && *p >= 1 && *p <= 5 {
		f.PartySize = *p
	}
	return f
}

// Active reports whether any filter beyond the account is set.
func (f Filter) Active() bool {
	return !f.From.IsZero() || !f.To.IsZero() || f.Patch != nil || f.GameMode != nil || f.Ranked != "" ||
		f.HeroID != 0 || f.LaneRole != 0 || f.Result != "" || f.PartySize != 0
}

// where returns the SQL condition for f over matches m and match_players
// mp, and its arguments.
func (f Filter) where() (string, []any) {
	conds := []string{"mp.account_id = ?"}
	args := []any{f.AccountID}
	add := func(cond string, arg ...any) {
		conds = append(conds, cond)
		args = append(args, arg...)
	}
	if !f.From.IsZero() {
		add("m.start_time >= ?", f.From.Unix())
	}
	if !f.To.IsZero() {
		add("m.start_time < ?", f.To.Unix())
	}
	if f.Patch != nil {
		add("m.patch = ?", *f.Patch)
	}
	if f.GameMode != nil {
		add("m.game_mode = ?", *f.GameMode)
	}
	switch f.Ranked {
	case "ranked":
		add("m.lobby_type = ?", rankedLobby)
	case "unranked":
		add("m.lobby_type != ?", rankedLobby)
	}
	if f.HeroID != 0 {
		add("mp.hero_id = ?", f.HeroID)
	}
	if f.LaneRole != 0 {
		add("mp.lane_role = ?", f.LaneRole)
	}
	switch f.Result {
	case "win":
		add(winExpr + " = 1")
	case "loss":
		add(winExpr + " = 0")
	}
	if f.PartySize != 0 {
		add("mp.party_size = ?", f.PartySize)
	}
	return strings.Join(conds, " AND "), args
}

// RankName turns a rank tier (tens: medal, ones: stars) into the
// Russian client's name, "Легенда 1".
func RankName(tier *int) string {
	if tier == nil || *tier <= 0 {
		return "Без ранга"
	}
	medals := []string{"", "Рекрут", "Страж", "Рыцарь", "Герой", "Легенда", "Властелин", "Божество", "Титан"}
	medal, stars := *tier/10, *tier%10
	if medal < 1 || medal >= len(medals) {
		return fmt.Sprintf("Ранг %d", *tier)
	}
	if medal == 8 || stars == 0 {
		return medals[medal]
	}
	return fmt.Sprintf("%s %d", medals[medal], stars)
}

// LaneRoleName names OpenDota's lane_role. 1-3 were checked against
// recorded matches (safe lane is bottom for Radiant, top for Dire); 4 is
// OpenDota's jungle and wasn't in the recorded data.
func LaneRoleName(role int) string {
	switch role {
	case 1:
		return "Лёгкая линия"
	case 2:
		return "Мид"
	case 3:
		return "Сложная линия"
	case 4:
		return "Лес"
	}
	return ""
}

// FilterOptions are the filter bar's choices for one account: only game
// modes, patches and heroes it has matches in.
type FilterOptions struct {
	GameModes []int
	Patches   []int // newest first
	Heroes    []int
}

// LoadFilterOptions lists what the account's matches contain.
func LoadFilterOptions(ctx context.Context, db *sql.DB, accountID int64) (FilterOptions, error) {
	var o FilterOptions
	for _, q := range []struct {
		query string
		dst   *[]int
	}{
		{`SELECT DISTINCT m.game_mode FROM match_players mp JOIN matches m ON m.match_id = mp.match_id
			WHERE mp.account_id = ? ORDER BY m.game_mode`, &o.GameModes},
		{`SELECT DISTINCT m.patch FROM match_players mp JOIN matches m ON m.match_id = mp.match_id
			WHERE mp.account_id = ? AND m.patch IS NOT NULL ORDER BY m.patch DESC`, &o.Patches},
		{`SELECT DISTINCT hero_id FROM match_players WHERE account_id = ?`, &o.Heroes},
	} {
		if err := each(ctx, db, q.query, func(r *sql.Rows) error {
			var n int
			if err := r.Scan(&n); err != nil {
				return err
			}
			*q.dst = append(*q.dst, n)
			return nil
		}, accountID); err != nil {
			return o, err
		}
	}
	return o, nil
}
