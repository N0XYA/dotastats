package stats

import (
	"context"
	"database/sql"
	"time"
)

// MatchRow is one match of the account, as listed on the Matches page.
type MatchRow struct {
	MatchID    int64
	StartTime  time.Time
	Duration   int
	GameMode   int
	LobbyType  int
	Patch      *int
	Win        *bool
	HeroID     int
	Kills      int
	Deaths     int
	Assists    int
	GPM        *int
	XPM        *int
	LastHits   *int
	LaneRole   *int
	PartySize  *int
	HasDetails bool
	IsParsed   bool
	ParseState string // "" until details are fetched
}

// KDA is (kills + assists) / deaths, with deaths at least 1.
func (r MatchRow) KDA() float64 { return kda(r.Kills, r.Deaths, r.Assists) }

func kda(k, d, a int) float64 { return float64(k+a) / float64(max(d, 1)) }

const matchRowColumns = `m.match_id, m.start_time, m.duration, m.game_mode, m.lobby_type, m.patch, ` + winExpr + `,
	mp.hero_id, mp.kills, mp.deaths, mp.assists, mp.gold_per_min, mp.xp_per_min, mp.last_hits,
	mp.lane_role, mp.party_size, m.details_at IS NOT NULL, m.is_parsed, coalesce(m.parse_state, '')`

func scanMatchRow(r interface{ Scan(...any) error }) (MatchRow, error) {
	var row MatchRow
	var start int64
	var win sql.NullBool
	var gpm, xpm, lh, role, party, patch sql.NullInt64
	err := r.Scan(&row.MatchID, &start, &row.Duration, &row.GameMode, &row.LobbyType, &patch, &win,
		&row.HeroID, &row.Kills, &row.Deaths, &row.Assists, &gpm, &xpm, &lh,
		&role, &party, &row.HasDetails, &row.IsParsed, &row.ParseState)
	row.StartTime = time.Unix(start, 0)
	row.Win = nullBool(win)
	row.GPM, row.XPM, row.LastHits = nullInt(gpm), nullInt(xpm), nullInt(lh)
	row.LaneRole, row.PartySize, row.Patch = nullInt(role), nullInt(party), nullInt(patch)
	return row, err
}

// Matches returns one page of the account's matches under f, newest
// first, and how many there are in total.
func Matches(ctx context.Context, db *sql.DB, f Filter, page, perPage int) ([]MatchRow, int, error) {
	where, args := f.where()
	var total int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM match_players mp
		JOIN matches m ON m.match_id = mp.match_id WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := matchRows(ctx, db, where+" ORDER BY m.start_time DESC, m.match_id DESC LIMIT ? OFFSET ?",
		append(args, perPage, max(page-1, 0)*perPage)...)
	return rows, total, err
}

func matchRows(ctx context.Context, db *sql.DB, whereOrder string, args ...any) ([]MatchRow, error) {
	var out []MatchRow
	err := each(ctx, db, `SELECT `+matchRowColumns+` FROM match_players mp
		JOIN matches m ON m.match_id = mp.match_id WHERE `+whereOrder, func(r *sql.Rows) error {
		row, err := scanMatchRow(r)
		out = append(out, row)
		return err
	}, args...)
	return out, err
}

// AccountMatch returns the account's row in one match, if it played it.
func AccountMatch(ctx context.Context, db *sql.DB, accountID, matchID int64) (MatchRow, bool, error) {
	rows, err := matchRows(ctx, db, "mp.account_id = ? AND m.match_id = ?", accountID, matchID)
	if err != nil || len(rows) == 0 {
		return MatchRow{}, false, err
	}
	return rows[0], true, nil
}

func nullInt(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}

func nullBool(b sql.NullBool) *bool {
	if !b.Valid {
		return nil
	}
	return &b.Bool
}
