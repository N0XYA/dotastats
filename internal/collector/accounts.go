package collector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"dotastats/internal/opendota"
)

// Account roles.
const (
	RoleOwner  = "owner"
	RoleFriend = "friend"
)

// ExposeDataHint tells a player how to make their matches visible.
const ExposeDataHint = `In Dota 2, open Settings > Options > Social and turn on "Expose Public Match Data", then play a match.`

var (
	ErrNotFound        = errors.New("account not found on OpenDota")
	ErrAccountExists   = errors.New("account is already tracked")
	ErrNoSuchAccount   = errors.New("account is not tracked")
	ErrOwnerHasFriends = errors.New("the owner can't be removed while friends are tracked; remove them first")

	// Errors of ParseAccountID.
	ErrNotAccountOrLink = errors.New("not an account ID, SteamID64 or profile URL")
	ErrSteamCustomURL   = errors.New("custom Steam URLs aren't supported yet; use the Friend ID from Dota 2 or a steamcommunity.com/profiles/<number> link")
	ErrUnsupportedSite  = errors.New("use an OpenDota, Dotabuff, Stratz or Steam profile link")
	ErrNoIDInLink       = errors.New("no account ID in the link")
)

// steamID64Base is the SteamID64 of account ID 0 for individual accounts.
const steamID64Base = 76561197960265728

// Account is a tracked account.
type Account struct {
	ID           int64
	Role         string
	Label        string
	Active       bool
	AddedAt      time.Time
	Name         string // Steam persona name
	AvatarURL    string
	RankTier     *int
	LastSyncedAt *time.Time // nil until the full history was fetched
	// ProfileSyncedAt is when name, avatar and rank were last fetched.
	ProfileSyncedAt *time.Time
	Matches         int // filled by ListAccounts only
}

// DisplayName is the label if set, else the Steam name, else the ID.
func (a Account) DisplayName() string {
	switch {
	case a.Label != "":
		return a.Label
	case a.Name != "":
		return a.Name
	}
	return strconv.FormatInt(a.ID, 10)
}

// ParseAccountID accepts a 32-bit account ID, a SteamID64, or an OpenDota,
// Dotabuff, Stratz or Steam profile URL with a numeric ID in it. Steam
// custom URLs (/id/name) need the Steam Web API and are not supported.
func ParseAccountID(input string) (int64, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return 0, ErrNotAccountOrLink
	}
	if isDigits(s) {
		return fromNumber(s)
	}
	return fromURL(s)
}

func fromNumber(s string) (int64, error) {
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", s, ErrNotAccountOrLink)
	}
	switch {
	case n >= steamID64Base && n-steamID64Base <= math.MaxUint32:
		return int64(n - steamID64Base), nil
	case n >= 1 && n <= math.MaxUint32:
		return int64(n), nil
	}
	return 0, fmt.Errorf("%s: %w", s, ErrNotAccountOrLink)
}

func fromURL(s string) (int64, error) {
	raw := s
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return 0, fmt.Errorf("%q: %w", raw, ErrNotAccountOrLink)
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	var want string
	switch strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.") {
	case "opendota.com", "dotabuff.com", "stratz.com":
		want = "players"
	case "steamcommunity.com":
		if segs[0] == "id" {
			return 0, fmt.Errorf("%q: %w", raw, ErrSteamCustomURL)
		}
		want = "profiles"
	default:
		return 0, fmt.Errorf("%q: %w", raw, ErrUnsupportedSite)
	}
	if len(segs) < 2 || segs[0] != want || !isDigits(segs[1]) {
		return 0, fmt.Errorf("%q: %w", raw, ErrNoIDInLink)
	}
	return fromNumber(segs[1])
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// AddResult is the outcome of AddAccount.
type AddResult struct {
	Account Account
	// NoMatches means the profile exists but shows no public matches, so
	// the player probably hasn't enabled Expose Public Match Data.
	NoMatches bool
}

// AddAccount validates input against OpenDota and starts tracking it. The
// first account becomes the owner. Its match history is fetched by the
// next run.
func (c *Collector) AddAccount(ctx context.Context, input, label string) (AddResult, error) {
	id, err := ParseAccountID(input)
	if err != nil {
		return AddResult{}, err
	}
	if _, err := getAccount(ctx, c.db.Read, id); err == nil {
		return AddResult{}, fmt.Errorf("%d: %w", id, ErrAccountExists)
	} else if !errors.Is(err, ErrNoSuchAccount) {
		return AddResult{}, err
	}

	actx := opendota.WithAccount(ctx, id)
	p, err := c.api.Player(actx, id)
	if opendota.IsNotFound(err) || err == nil && p.Profile == nil {
		return AddResult{}, fmt.Errorf("%d: %w", id, ErrNotFound)
	}
	if err != nil {
		return AddResult{}, err
	}
	ms, err := c.api.PlayerMatches(actx, id, url.Values{"limit": {"1"}, "significant": {"0"}})
	if err != nil {
		return AddResult{}, err
	}

	tx, err := c.db.Write.BeginTx(ctx, nil)
	if err != nil {
		return AddResult{}, err
	}
	defer tx.Rollback()
	var owners int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM accounts WHERE role = ?", RoleOwner).Scan(&owners); err != nil {
		return AddResult{}, err
	}
	role := RoleFriend
	if owners == 0 {
		role = RoleOwner
	}
	now := c.now().Unix()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO accounts (account_id, role, label, added_at, name, avatar_url, rank_tier, profile_synced_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, role, nullString(label), now, p.Profile.PersonaName, p.Profile.AvatarMedium, p.RankTier, now)
	if err != nil {
		return AddResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return AddResult{}, err
	}
	a, err := getAccount(ctx, c.db.Read, id)
	return AddResult{Account: a, NoMatches: len(ms) == 0}, err
}

// RemoveAccount stops tracking an account and deletes the matches no other
// tracked account played, with all their data. It returns how many
// matches were deleted.
func RemoveAccount(ctx context.Context, db *sql.DB, id int64) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var role string
	err = tx.QueryRowContext(ctx, "SELECT role FROM accounts WHERE account_id = ?", id).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%d: %w", id, ErrNoSuchAccount)
	}
	if err != nil {
		return 0, err
	}
	if role == RoleOwner {
		var others int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM accounts WHERE account_id != ?", id).Scan(&others); err != nil {
			return 0, err
		}
		if others > 0 {
			return 0, ErrOwnerHasFriends
		}
	}

	// Links go with the account (ON DELETE CASCADE); players of deleted
	// matches go with the match.
	if _, err := tx.ExecContext(ctx, "DELETE FROM accounts WHERE account_id = ?", id); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `
		DELETE FROM matches
		WHERE NOT EXISTS (SELECT 1 FROM account_matches am WHERE am.match_id = matches.match_id)`)
	if err != nil {
		return 0, err
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return deleted, tx.Commit()
}

// SetAccountActive pauses (false) or resumes (true) an account. A paused
// account keeps its data but is skipped by the collector.
func SetAccountActive(ctx context.Context, db *sql.DB, id int64, active bool) error {
	res, err := db.ExecContext(ctx, "UPDATE accounts SET is_active = ? WHERE account_id = ?", active, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return fmt.Errorf("%d: %w", id, ErrNoSuchAccount)
	}
	return nil
}

const accountColumns = `account_id, role, coalesce(label, ''), is_active, added_at,
	coalesce(name, ''), coalesce(avatar_url, ''), rank_tier, last_synced_at, profile_synced_at`

// ListAccounts returns every tracked account, owner first, with its match
// count.
func ListAccounts(ctx context.Context, db *sql.DB) ([]Account, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT `+accountColumns+`,
			(SELECT count(*) FROM account_matches am WHERE am.account_id = a.account_id)
		FROM accounts a
		ORDER BY role = 'owner' DESC, added_at, account_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		if err := scanAccount(rows, &a, &a.Matches); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// activeAccounts returns the accounts a run works on: the owner first,
// then friends least recently synced first, so a run that runs out of
// budget serves the others first next time. onlyID limits it to one.
func activeAccounts(ctx context.Context, db *sql.DB, onlyID int64) ([]Account, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT `+accountColumns+`
		FROM accounts
		WHERE is_active = 1 AND (?1 = 0 OR account_id = ?1)
		ORDER BY role = 'owner' DESC, last_synced_at, account_id`, onlyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		if err := scanAccount(rows, &a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if onlyID != 0 && len(out) == 0 {
		return nil, fmt.Errorf("%d: %w or is paused", onlyID, ErrNoSuchAccount)
	}
	return out, nil
}

func getAccount(ctx context.Context, db *sql.DB, id int64) (Account, error) {
	var a Account
	row := db.QueryRowContext(ctx, "SELECT "+accountColumns+" FROM accounts WHERE account_id = ?", id)
	err := scanAccount(row, &a)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, fmt.Errorf("%d: %w", id, ErrNoSuchAccount)
	}
	return a, err
}

func scanAccount(row interface{ Scan(...any) error }, a *Account, extra ...any) error {
	var addedAt int64
	var rank, synced, profiled sql.NullInt64
	dest := append([]any{&a.ID, &a.Role, &a.Label, &a.Active, &addedAt, &a.Name, &a.AvatarURL, &rank, &synced, &profiled}, extra...)
	if err := row.Scan(dest...); err != nil {
		return err
	}
	a.AddedAt = time.Unix(addedAt, 0)
	if rank.Valid {
		r := int(rank.Int64)
		a.RankTier = &r
	}
	if synced.Valid {
		t := time.Unix(synced.Int64, 0)
		a.LastSyncedAt = &t
	}
	if profiled.Valid {
		t := time.Unix(profiled.Int64, 0)
		a.ProfileSyncedAt = &t
	}
	return nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
