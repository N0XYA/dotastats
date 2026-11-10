package collector

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"time"

	"dotastats/internal/opendota"
)

// discoveryMarginDays re-checks extra days, so matches that reach OpenDota
// late are still found. Already stored matches are skipped.
const discoveryMarginDays = 2

// profileInterval is how often names, avatars and ranks are refreshed.
const profileInterval = 6 * time.Hour

// syncProfile refreshes an account's name, avatar and rank when they are
// older than profileInterval, and updates a with them.
func (c *Collector) syncProfile(ctx context.Context, a *Account) error {
	if a.ProfileSyncedAt != nil && c.now().Sub(*a.ProfileSyncedAt) < profileInterval {
		return nil
	}
	p, err := c.api.Player(opendota.WithAccount(ctx, a.ID), a.ID)
	if err != nil {
		return fmt.Errorf("account %d: profile: %w", a.ID, err)
	}
	if p.Profile == nil {
		return fmt.Errorf("account %d: profile: OpenDota returned none", a.ID)
	}
	_, err = c.db.Write.ExecContext(ctx, `
		UPDATE accounts SET name = ?, avatar_url = ?, rank_tier = ?, profile_synced_at = ?
		WHERE account_id = ?`,
		p.Profile.PersonaName, p.Profile.AvatarMedium, p.RankTier, c.now().Unix(), a.ID)
	if err != nil {
		return err
	}
	now := c.now()
	a.Name, a.AvatarURL, a.RankTier, a.ProfileSyncedAt = p.Profile.PersonaName, p.Profile.AvatarMedium, p.RankTier, &now
	return nil
}

// discover stores the account's matches that aren't stored or linked to
// it yet. Without full, it only asks for the days since the last sync; an
// account never synced gets its whole history.
func (c *Collector) discover(ctx context.Context, a Account, full bool) (added, linked int, err error) {
	q := url.Values{
		"significant": {"0"}, // the default, 1, hides some matches
		"project":     slices.Clone(opendota.PlayerMatchFields),
	}
	incremental := !full && a.LastSyncedAt != nil
	if incremental {
		days := int(c.now().Sub(*a.LastSyncedAt)/(24*time.Hour)) + discoveryMarginDays
		q.Set("date", strconv.Itoa(days))
	}
	started := c.now()
	ms, err := c.api.PlayerMatches(opendota.WithAccount(ctx, a.ID), a.ID, q)
	if err != nil {
		return 0, 0, fmt.Errorf("account %d: matches: %w", a.ID, err)
	}
	// An empty full history usually means it is hidden (the profile's
	// fh_unavailable is true). Leave the account unsynced so the whole
	// history is fetched once the player exposes it, not just recent days.
	var syncedAt time.Time
	if incremental || len(ms) > 0 {
		syncedAt = started
	}
	added, linked, err = storeDiscovered(ctx, c.db.Write, a.ID, ms, syncedAt)
	if err != nil {
		return 0, 0, fmt.Errorf("account %d: store matches: %w", a.ID, err)
	}
	return added, linked, nil
}

// storeDiscovered inserts unknown matches, links every match to the account
// and stores the account's own player row, in one transaction. Existing
// rows are left alone: match details overwrite them later. A zero syncedAt
// leaves the account's last sync time unchanged.
func storeDiscovered(ctx context.Context, db *sql.DB, accountID int64, ms []opendota.PlayerMatch, syncedAt time.Time) (added, linked int, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	// Until details arrive, the patch is the last one released before the
	// match started.
	insMatch, err := tx.PrepareContext(ctx, `
		INSERT INTO matches (match_id, start_time, duration, game_mode, lobby_type,
			radiant_win, avg_rank_tier, is_parsed, patch)
		VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8,
			(SELECT id FROM patches WHERE released_at <= ?2 ORDER BY released_at DESC LIMIT 1))
		ON CONFLICT (match_id) DO NOTHING`)
	if err != nil {
		return 0, 0, err
	}
	insLink, err := tx.PrepareContext(ctx, `
		INSERT INTO account_matches (account_id, match_id) VALUES (?, ?)
		ON CONFLICT DO NOTHING`)
	if err != nil {
		return 0, 0, err
	}
	insPlayer, err := tx.PrepareContext(ctx, `
		INSERT INTO match_players (match_id, player_slot, account_id, hero_id,
			kills, deaths, assists, leaver_status, party_size,
			last_hits, denies, gold_per_min, xp_per_min, hero_damage, tower_damage,
			hero_healing, level, item_0, item_1, item_2, item_3, item_4, item_5,
			item_neutral, lane, lane_role, is_roaming)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (match_id, player_slot) DO NOTHING`)
	if err != nil {
		return 0, 0, err
	}

	for _, m := range ms {
		res, err := insMatch.ExecContext(ctx, m.MatchID, m.StartTime, m.Duration, m.GameMode, m.LobbyType,
			m.RadiantWin, m.AverageRank, m.Version != nil)
		if err != nil {
			return 0, 0, fmt.Errorf("match %d: %w", m.MatchID, err)
		}
		added += affected(res)
		res, err = insLink.ExecContext(ctx, accountID, m.MatchID)
		if err != nil {
			return 0, 0, fmt.Errorf("match %d: %w", m.MatchID, err)
		}
		linked += affected(res)
		if m.PlayerSlot == nil {
			continue
		}
		_, err = insPlayer.ExecContext(ctx, m.MatchID, *m.PlayerSlot, accountID, m.HeroID,
			m.Kills, m.Deaths, m.Assists, m.LeaverStatus, m.PartySize,
			m.LastHits, m.Denies, m.GoldPerMin, m.XPPerMin, m.HeroDamage, m.TowerDamage,
			m.HeroHealing, m.Level, m.Item0, m.Item1, m.Item2, m.Item3, m.Item4, m.Item5,
			m.ItemNeutral, m.Lane, m.LaneRole, m.IsRoaming)
		if err != nil {
			return 0, 0, fmt.Errorf("match %d: %w", m.MatchID, err)
		}
	}

	if !syncedAt.IsZero() {
		if _, err := tx.ExecContext(ctx, "UPDATE accounts SET last_synced_at = ? WHERE account_id = ?",
			syncedAt.Unix(), accountID); err != nil {
			return 0, 0, err
		}
	}
	return added, linked, tx.Commit()
}

func affected(res sql.Result) int {
	n, _ := res.RowsAffected()
	return int(n)
}
