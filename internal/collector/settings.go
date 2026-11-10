package collector

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"

	"dotastats/internal/db"
	"dotastats/internal/opendota"
)

// Settings are the collector's tunables. They live in the settings table,
// are edited on the Settings page and are read at the start of each run;
// unset keys use DefaultSettings. Negative retention days mean "off".
type Settings struct {
	DailyBudget     int // OpenDota calls per UTC day
	IntervalMinutes int // between scheduled runs
	// RawRetentionDays drops the raw JSON of older matches (0 drops all).
	RawRetentionDays int
	// DetailRetentionDays drops time series, purchases and wards of older
	// matches, keeping the scoreboard.
	DetailRetentionDays int
	// MatchRetentionDays deletes older matches entirely.
	MatchRetentionDays int
	// RecentDays: matches this recent get details first and parse
	// requests; older ones are backfilled slowly and never parsed.
	RecentDays int
	// BackfillBudgetPct pauses fetching older matches' details once
	// today's usage reaches this share of the daily budget, so new
	// matches always find calls left.
	BackfillBudgetPct int
}

// DefaultSettings apply to keys missing from the settings table.
var DefaultSettings = Settings{
	DailyBudget:         opendota.DefaultDailyBudget,
	IntervalMinutes:     15,
	RawRetentionDays:    90,
	DetailRetentionDays: -1,
	MatchRetentionDays:  -1,
	RecentDays:          30,
	BackfillBudgetPct:   50,
}

// Settings keys.
const (
	KeyDailyBudget         = "daily_budget"
	KeyIntervalMinutes     = "collect_interval_minutes"
	KeyRawRetentionDays    = "raw_retention_days"
	KeyDetailRetentionDays = "detail_retention_days"
	KeyMatchRetentionDays  = "match_retention_days"
	KeyRecentDays          = "recent_days"
	KeyBackfillBudgetPct   = "backfill_budget_pct"
)

// settingRange is the accepted range of each setting.
var settingRange = map[string][2]int{
	KeyDailyBudget:         {1, 100000},
	KeyIntervalMinutes:     {5, 1440},
	KeyRawRetentionDays:    {-1, 36500},
	KeyDetailRetentionDays: {-1, 36500},
	KeyMatchRetentionDays:  {-1, 36500},
	KeyRecentDays:          {0, 365},
	KeyBackfillBudgetPct:   {0, 100},
}

func (s *Settings) fields() []struct {
	key string
	val *int
} {
	return []struct {
		key string
		val *int
	}{
		{KeyDailyBudget, &s.DailyBudget},
		{KeyIntervalMinutes, &s.IntervalMinutes},
		{KeyRawRetentionDays, &s.RawRetentionDays},
		{KeyDetailRetentionDays, &s.DetailRetentionDays},
		{KeyMatchRetentionDays, &s.MatchRetentionDays},
		{KeyRecentDays, &s.RecentDays},
		{KeyBackfillBudgetPct, &s.BackfillBudgetPct},
	}
}

// RangeError is a setting outside its accepted range.
type RangeError struct {
	Key      string
	Min, Max int
}

func (e *RangeError) Error() string {
	return fmt.Sprintf("%s must be between %d and %d", e.Key, e.Min, e.Max)
}

// Validate reports the first setting outside its range.
func (s Settings) Validate() error {
	for _, f := range s.fields() {
		r := settingRange[f.key]
		if *f.val < r[0] || *f.val > r[1] {
			return &RangeError{Key: f.key, Min: r[0], Max: r[1]}
		}
	}
	return nil
}

// SaveSettings validates s and writes every key.
func SaveSettings(ctx context.Context, q *sql.DB, s Settings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	tx, err := q.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, f := range s.fields() {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value`, f.key, strconv.Itoa(*f.val)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LoadSettings reads the settings table over DefaultSettings.
func LoadSettings(ctx context.Context, q *sql.DB) (Settings, error) {
	s := DefaultSettings
	for _, f := range s.fields() {
		v, err := db.SettingInt(ctx, q, f.key, *f.val)
		if err != nil {
			return Settings{}, err
		}
		*f.val = v
	}
	return s, nil
}
