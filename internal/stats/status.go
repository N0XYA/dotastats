package stats

import (
	"context"
	"database/sql"
	"strconv"
	"time"
)

// Status is the collector's state for the Status page.
type Status struct {
	LastRunStarted, LastRunFinished *time.Time
	LastRunError                    string
	ConstantsSynced                 *time.Time
	AggregatesSynced                *time.Time
	RetentionRan                    *time.Time

	CallsToday     int
	CallsByAccount []AccountCalls // today, largest first

	DetailsBacklog int // matches without details
	ParseQueue     []ParseJob
	ParseCounts    map[string]int // by job status
	Problems       []ParseJob     // recent expired and failed jobs
}

// AccountCalls is one account's API calls today; AccountID 0 holds
// constants and aggregates.
type AccountCalls struct {
	AccountID int64
	Name      string
	Calls     int
}

// ParseJob is a parse request as tracked by the collector.
type ParseJob struct {
	MatchID     int64
	Status      string
	RequestedAt time.Time
	NextCheck   *time.Time
	Attempts    int
	LastError   string
}

const maxListedJobs = 20

// LoadStatus reads the Status page. Days for API usage are UTC.
func LoadStatus(ctx context.Context, db *sql.DB, now time.Time) (Status, error) {
	s := Status{ParseCounts: map[string]int{}}
	state := map[string]string{}
	if err := each(ctx, db, "SELECT key, value FROM collector_state", func(r *sql.Rows) error {
		var k, v string
		if err := r.Scan(&k, &v); err != nil {
			return err
		}
		state[k] = v
		return nil
	}); err != nil {
		return s, err
	}
	unix := func(key string) *time.Time {
		n, err := strconv.ParseInt(state[key], 10, 64)
		if err != nil {
			return nil
		}
		t := time.Unix(n, 0)
		return &t
	}
	// Keys written by the collector package.
	s.LastRunStarted, s.LastRunFinished = unix("last_run_started_at"), unix("last_run_finished_at")
	s.LastRunError = state["last_run_error"]
	s.ConstantsSynced, s.AggregatesSynced, s.RetentionRan = unix("constants_synced_at"), unix("aggregates_synced_at"), unix("retention_ran_at")

	if err := each(ctx, db, `SELECT u.account_id, coalesce(a.label, a.name, ''), u.calls
		FROM api_usage u LEFT JOIN accounts a ON a.account_id = u.account_id
		WHERE u.day = ? ORDER BY u.calls DESC`, func(r *sql.Rows) error {
		var c AccountCalls
		if err := r.Scan(&c.AccountID, &c.Name, &c.Calls); err != nil {
			return err
		}
		s.CallsToday += c.Calls
		s.CallsByAccount = append(s.CallsByAccount, c)
		return nil
	}, now.UTC().Format(time.DateOnly)); err != nil {
		return s, err
	}

	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM matches m
		WHERE m.details_at IS NULL AND EXISTS (SELECT 1 FROM account_matches am WHERE am.match_id = m.match_id)`).Scan(&s.DetailsBacklog); err != nil {
		return s, err
	}
	if err := each(ctx, db, "SELECT status, count(*) FROM parse_jobs GROUP BY status", func(r *sql.Rows) error {
		var st string
		var n int
		if err := r.Scan(&st, &n); err != nil {
			return err
		}
		s.ParseCounts[st] = n
		return nil
	}); err != nil {
		return s, err
	}
	var err error
	if s.ParseQueue, err = parseJobs(ctx, db, "status = 'pending' ORDER BY coalesce(next_check_at, 0), match_id"); err != nil {
		return s, err
	}
	s.Problems, err = parseJobs(ctx, db, "status IN ('expired', 'failed') ORDER BY requested_at DESC")
	return s, err
}

func parseJobs(ctx context.Context, db *sql.DB, whereOrder string) ([]ParseJob, error) {
	var out []ParseJob
	err := each(ctx, db, `SELECT match_id, status, requested_at, next_check_at, attempts, coalesce(last_error, '')
		FROM parse_jobs WHERE `+whereOrder+` LIMIT `+strconv.Itoa(maxListedJobs), func(r *sql.Rows) error {
		var j ParseJob
		var req int64
		var next sql.NullInt64
		if err := r.Scan(&j.MatchID, &j.Status, &req, &next, &j.Attempts, &j.LastError); err != nil {
			return err
		}
		j.RequestedAt = time.Unix(req, 0)
		if next.Valid {
			t := time.Unix(next.Int64, 0)
			j.NextCheck = &t
		}
		out = append(out, j)
		return nil
	})
	return out, err
}
