package opendota

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"time"
)

// ErrBudgetExhausted means today's call budget is spent. Callers stop
// cleanly and finish the work on the next run.
var ErrBudgetExhausted = errors.New("opendota: daily call budget exhausted")

// DefaultDailyBudget leaves headroom below the free tier's 3,000 calls per
// day (reported by the x-rate-limit-remaining-day header).
const DefaultDailyBudget = 2500

// Budget is the persistent daily call budget kept in the api_usage table.
// Days are UTC dates. Calls are attributed to the account set on the
// context with WithAccount, or to account 0.
type Budget struct {
	db    *sql.DB
	limit atomic.Int64
	now   func() time.Time
}

type accountKey struct{}

// WithAccount attributes the API calls made with ctx to accountID.
func WithAccount(ctx context.Context, accountID int64) context.Context {
	return context.WithValue(ctx, accountKey{}, accountID)
}

func accountOf(ctx context.Context) int64 {
	id, _ := ctx.Value(accountKey{}).(int64)
	return id
}

// NewBudget returns a budget of limit calls per day recorded in db, which
// must be the write connection.
func NewBudget(db *sql.DB, limit int) *Budget {
	b := &Budget{db: db, now: time.Now}
	b.SetLimit(limit)
	return b
}

// SetLimit changes the daily limit, e.g. after the setting was edited.
func (b *Budget) SetLimit(limit int) { b.limit.Store(int64(limit)) }

// Limit returns the daily limit.
func (b *Budget) Limit() int { return int(b.limit.Load()) }

// Spend records n calls for today, or returns ErrBudgetExhausted without
// recording anything if they don't fit.
func (b *Budget) Spend(ctx context.Context, n int) error {
	// One statement, so the check and the increment are atomic even when
	// the server and a CLI command spend at the same time.
	res, err := b.db.ExecContext(ctx, `
		INSERT INTO api_usage (day, account_id, calls)
		SELECT ?1, ?2, ?3
		WHERE (SELECT coalesce(sum(calls), 0) FROM api_usage WHERE day = ?1) + ?3 <= ?4
		ON CONFLICT (day, account_id) DO UPDATE SET calls = calls + excluded.calls`,
		b.day(), accountOf(ctx), n, b.Limit())
	if err != nil {
		return err
	}
	if affected, err := res.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		return ErrBudgetExhausted
	}
	return nil
}

// Used returns the calls spent today across all accounts.
func (b *Budget) Used(ctx context.Context) (int, error) {
	var n int
	err := b.db.QueryRowContext(ctx, "SELECT coalesce(sum(calls), 0) FROM api_usage WHERE day = ?", b.day()).Scan(&n)
	return n, err
}

// UsedByAccount returns today's calls per account; key 0 holds calls not
// made for one account.
func (b *Budget) UsedByAccount(ctx context.Context) (map[int64]int, error) {
	rows, err := b.db.QueryContext(ctx, "SELECT account_id, calls FROM api_usage WHERE day = ?", b.day())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	used := map[int64]int{}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		used[id] = n
	}
	return used, rows.Err()
}

func (b *Budget) day() string {
	return b.now().UTC().Format(time.DateOnly)
}
