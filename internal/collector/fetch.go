package collector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"dotastats/internal/opendota"
)

// Values of matches.parse_state once details are stored.
const (
	ParseUnparsed = "unparsed" // not parsed and not requested
	ParsePending  = "pending"  // parse requested, polling
	ParseParsed   = "parsed"
	ParseExpired  = "expired" // the parse never landed; the replay is likely gone
	ParseFailed   = "failed"  // still queued at OpenDota after every poll
)

// queued is a match waiting for work, attributed to one tracked account
// for budget accounting.
type queued struct {
	matchID   int64
	accountID int64
}

// workQueue orders matches selected by where (on matches m), newest first,
// so the owner's come first and the friends' take turns: when the budget
// runs out the friends have had equal shares. A shared match is queued
// once, under the first account in that order.
func workQueue(ctx context.Context, q *sql.DB, accounts []Account, where string, args ...any) ([]queued, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT am.account_id, m.match_id
		FROM matches m JOIN account_matches am ON am.match_id = m.match_id
		WHERE `+where+`
		ORDER BY m.start_time DESC, m.match_id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	perAccount := map[int64][]int64{}
	for rows.Next() {
		var account, match int64
		if err := rows.Scan(&account, &match); err != nil {
			return nil, err
		}
		perAccount[account] = append(perAccount[account], match)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	seen := map[int64]bool{}
	lists := make([][]queued, len(accounts))
	for i, a := range accounts {
		for _, id := range perAccount[a.ID] {
			if !seen[id] {
				seen[id] = true
				lists[i] = append(lists[i], queued{matchID: id, accountID: a.ID})
			}
		}
	}
	var out []queued
	friends := lists
	if len(accounts) > 0 && accounts[0].Role == RoleOwner {
		out, friends = lists[0], lists[1:]
	}
	for i := 0; ; i++ {
		added := false
		for _, l := range friends {
			if i < len(l) {
				out = append(out, l[i])
				added = true
			}
		}
		if !added {
			return out, nil
		}
	}
}

// fetchDetails stores details for matches that have none yet. Recent ones
// come first and may use the whole budget; older ones (backfill) stop once
// today's usage reaches BackfillBudgetPct.
func (c *Collector) fetchDetails(ctx context.Context, s Settings, accounts []Account, recent bool) (int, error) {
	cutoff := c.now().AddDate(0, 0, -s.RecentDays).Unix()
	where := "m.details_at IS NULL AND m.start_time >= ?"
	if !recent {
		where = "m.details_at IS NULL AND m.start_time < ?"
	}
	queue, err := workQueue(ctx, c.db.Read, accounts, where, cutoff)
	if err != nil {
		return 0, err
	}
	backfillCap := c.budget.Limit() * s.BackfillBudgetPct / 100
	var fetched int
	var errs []error
	for _, q := range queue {
		if !c.stillActive(ctx, q.accountID) {
			continue
		}
		if !recent {
			used, err := c.budget.Used(ctx)
			if err != nil {
				return fetched, err
			}
			if used >= backfillCap {
				slog.Info("older match details paused for today", "used", used, "cap", backfillCap, "left", len(queue)-fetched)
				break
			}
		}
		m, raw, err := c.api.Match(opendota.WithAccount(ctx, q.accountID), q.matchID)
		if err == nil {
			state := ParseUnparsed
			if m.Parsed() {
				state = ParseParsed
			}
			err = c.storeDetails(ctx, m, raw, s, state)
		}
		if err != nil {
			if errors.Is(err, opendota.ErrBudgetExhausted) || ctx.Err() != nil {
				return fetched, err
			}
			// Leave it for the next run; one bad match must not block the rest.
			errs = append(errs, fmt.Errorf("match %d: details: %w", q.matchID, err))
			continue
		}
		fetched++
	}
	return fetched, errors.Join(errs...)
}

// storeDetails writes a fetched match in one transaction: normalized
// tables, the raw JSON (unless the match is already past raw retention)
// and pipeline state.
func (c *Collector) storeDetails(ctx context.Context, m opendota.Match, raw []byte, s Settings, parseState string) error {
	now := c.now()
	tx, err := c.db.Write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := extract(ctx, tx, m); err != nil {
		return err
	}
	ex := &execer{ctx: ctx, tx: tx}
	if s.RawRetentionDays < 0 || m.StartTime >= now.AddDate(0, 0, -s.RawRetentionDays).Unix() {
		ex.exec(`
			INSERT INTO match_raw (match_id, fetched_at, parser_version, data) VALUES (?, ?, ?, ?)
			ON CONFLICT (match_id) DO UPDATE SET fetched_at = excluded.fetched_at,
				parser_version = excluded.parser_version, data = excluded.data`,
			m.MatchID, now.Unix(), m.Version, compress(raw))
		ex.exec("UPDATE matches SET raw_pruned_at = NULL WHERE match_id = ?", m.MatchID)
	} else {
		// Retention would delete it on its next run anyway.
		ex.exec("DELETE FROM match_raw WHERE match_id = ?", m.MatchID)
		ex.exec("UPDATE matches SET raw_pruned_at = ? WHERE match_id = ?", now.Unix(), m.MatchID)
	}
	ex.exec("UPDATE matches SET details_at = ?, parse_state = ? WHERE match_id = ?", now.Unix(), parseState, m.MatchID)
	if ex.err != nil {
		return ex.err
	}
	return tx.Commit()
}

// pollBackoff is the wait before each parse check; the last value repeats.
var pollBackoff = []time.Duration{2 * time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}

// maxPolls is how many checks a parse job gets before it is given up.
const maxPolls = 8

func pollDelay(attempt int) time.Duration {
	return pollBackoff[min(attempt, len(pollBackoff)-1)]
}

// attributedAccount picks the account a match's API calls count against:
// the owner if they played it, else the lowest friend ID.
const attributedAccount = `coalesce((SELECT am.account_id FROM account_matches am
	JOIN accounts a ON a.account_id = am.account_id AND a.is_active = 1
	WHERE am.match_id = j.match_id ORDER BY a.role = 'owner' DESC, a.account_id LIMIT 1), 0)`

// activeJob limits parse jobs (alias j) to matches an active account
// played: pausing an account also stops polling its parses.
const activeJob = `EXISTS (SELECT 1 FROM account_matches am
	JOIN accounts a ON a.account_id = am.account_id AND a.is_active = 1
	WHERE am.match_id = j.match_id)`

// QueueParse asks for one match to be parsed on the next run, whatever its
// age (the "request parse" button). A finished, expired or failed job
// starts over. Parsed matches are left alone.
func QueueParse(ctx context.Context, db *sql.DB, matchID int64, now time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var parsed bool
	err = tx.QueryRowContext(ctx, "SELECT is_parsed FROM matches WHERE match_id = ?", matchID).Scan(&parsed)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("match %d is not stored", matchID)
	}
	if err != nil || parsed {
		return err
	}
	ex := &execer{ctx: ctx, tx: tx}
	ex.exec(`INSERT INTO parse_jobs (match_id, job_id, requested_at, status) VALUES (?, NULL, ?, 'pending')
		ON CONFLICT (match_id) DO UPDATE SET job_id = NULL, requested_at = excluded.requested_at,
			attempts = 0, next_check_at = NULL, left_queue_at = NULL, last_error = NULL, status = 'pending'`,
		matchID, now.Unix())
	ex.exec("UPDATE matches SET parse_state = ? WHERE match_id = ?", ParsePending, matchID)
	if ex.err != nil {
		return ex.err
	}
	return tx.Commit()
}

// requestParses sends parse requests: first for matches queued by
// QueueParse, then for recent unparsed matches that have no job yet.
func (c *Collector) requestParses(ctx context.Context, s Settings, accounts []Account) (int, error) {
	queue, err := c.queuedParses(ctx)
	if err != nil {
		return 0, err
	}
	cutoff := c.now().AddDate(0, 0, -s.RecentDays).Unix()
	recent, err := workQueue(ctx, c.db.Read, accounts, `
		m.details_at IS NOT NULL AND m.is_parsed = 0 AND m.start_time >= ?
		AND NOT EXISTS (SELECT 1 FROM parse_jobs j WHERE j.match_id = m.match_id)`, cutoff)
	if err != nil {
		return 0, err
	}
	queue = append(queue, recent...)
	var requested int
	for _, q := range queue {
		if !c.stillActive(ctx, q.accountID) {
			continue
		}
		jobID, err := c.api.RequestParse(opendota.WithAccount(ctx, q.accountID), q.matchID)
		if err != nil {
			if errors.Is(err, opendota.ErrBudgetExhausted) || ctx.Err() != nil {
				return requested, err
			}
			return requested, fmt.Errorf("match %d: request parse: %w", q.matchID, err)
		}
		now := c.now()
		tx, err := c.db.Write.BeginTx(ctx, nil)
		if err != nil {
			return requested, err
		}
		ex := &execer{ctx: ctx, tx: tx}
		ex.exec(`INSERT INTO parse_jobs (match_id, job_id, requested_at, next_check_at, status)
			VALUES (?, ?, ?, ?, 'pending')
			ON CONFLICT (match_id) DO UPDATE SET job_id = excluded.job_id, requested_at = excluded.requested_at,
				next_check_at = excluded.next_check_at`,
			q.matchID, jobID, now.Unix(), now.Add(pollDelay(0)).Unix())
		ex.exec("UPDATE matches SET parse_state = ? WHERE match_id = ?", ParsePending, q.matchID)
		if ex.err == nil {
			ex.err = tx.Commit()
		}
		tx.Rollback()
		if ex.err != nil {
			return requested, ex.err
		}
		requested++
	}
	return requested, nil
}

// queuedParses lists jobs queued by QueueParse that have no request yet,
// oldest first.
func (c *Collector) queuedParses(ctx context.Context) ([]queued, error) {
	rows, err := c.db.Read.QueryContext(ctx, `
		SELECT j.match_id, `+attributedAccount+`
		FROM parse_jobs j WHERE j.status = 'pending' AND j.job_id IS NULL AND `+activeJob+`
		ORDER BY j.requested_at, j.match_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []queued
	for rows.Next() {
		var q queued
		if err := rows.Scan(&q.matchID, &q.accountID); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

type pendingJob struct {
	matchID, jobID, accountID int64
	attempts                  int
	leftQueue                 bool
}

// pollParses re-checks parse jobs that are due. A parsed match is stored
// again. Otherwise the job's queue state decides: still queued means
// wait; gone on two checks in a row means the parse never landed
// (expired). A job still queued after maxPolls checks has failed.
func (c *Collector) pollParses(ctx context.Context, s Settings) (int, error) {
	jobs, err := c.dueJobs(ctx)
	if err != nil {
		return 0, err
	}
	var parsed int
	for _, j := range jobs {
		if !c.stillActive(ctx, j.accountID) {
			continue
		}
		done, err := c.pollOne(ctx, s, j)
		if err != nil {
			if errors.Is(err, opendota.ErrBudgetExhausted) || ctx.Err() != nil {
				return parsed, err
			}
			return parsed, fmt.Errorf("match %d: poll parse: %w", j.matchID, err)
		}
		if done {
			parsed++
		}
	}
	return parsed, nil
}

func (c *Collector) dueJobs(ctx context.Context) ([]pendingJob, error) {
	rows, err := c.db.Read.QueryContext(ctx, `
		SELECT j.match_id, j.job_id, j.attempts, j.left_queue_at IS NOT NULL, `+attributedAccount+`
		FROM parse_jobs j
		WHERE j.status = 'pending' AND j.next_check_at <= ? AND `+activeJob+`
		ORDER BY j.next_check_at`, c.now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []pendingJob
	for rows.Next() {
		var j pendingJob
		if err := rows.Scan(&j.matchID, &j.jobID, &j.attempts, &j.leftQueue, &j.accountID); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func (c *Collector) pollOne(ctx context.Context, s Settings, j pendingJob) (parsed bool, err error) {
	actx := opendota.WithAccount(ctx, j.accountID)
	m, raw, err := c.api.MatchFresh(actx, j.matchID)
	if err != nil {
		return false, err
	}
	if m.Parsed() {
		if err := c.storeDetails(ctx, m, raw, s, ParseParsed); err != nil {
			return false, err
		}
		return true, c.setJob(ctx, j.matchID, "done", "", j.attempts+1, nil, false)
	}

	queuedAtOpenDota, err := c.api.ParseJobQueued(actx, j.jobID)
	if err != nil {
		return false, err
	}
	attempts := j.attempts + 1
	next := c.now().Add(pollDelay(attempts)).Unix()
	switch {
	case !queuedAtOpenDota && j.leftQueue:
		return false, c.setJob(ctx, j.matchID, "expired", "the parse job finished without a parsed match; the replay is probably no longer available", attempts, nil, true)
	case !queuedAtOpenDota:
		// Give the parsed result time to show up before concluding.
		return false, c.setJob(ctx, j.matchID, "pending", "", attempts, &next, true)
	case attempts >= maxPolls:
		return false, c.setJob(ctx, j.matchID, "failed", fmt.Sprintf("still queued at OpenDota after %d checks", attempts), attempts, nil, false)
	}
	return false, c.setJob(ctx, j.matchID, "pending", "", attempts, &next, false)
}

// setJob records a poll's outcome and mirrors final states onto the match.
func (c *Collector) setJob(ctx context.Context, matchID int64, status, lastError string, attempts int, nextCheck *int64, leftQueue bool) error {
	now := c.now().Unix()
	tx, err := c.db.Write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ex := &execer{ctx: ctx, tx: tx}
	ex.exec(`UPDATE parse_jobs SET status = ?, last_error = ?, attempts = ?, next_check_at = ?,
			left_queue_at = CASE WHEN ? THEN coalesce(left_queue_at, ?) END
		WHERE match_id = ?`,
		status, nullString(lastError), attempts, nextCheck, leftQueue, now, matchID)
	switch status {
	case "expired":
		ex.exec("UPDATE matches SET parse_state = ? WHERE match_id = ?", ParseExpired, matchID)
	case "failed":
		ex.exec("UPDATE matches SET parse_state = ? WHERE match_id = ?", ParseFailed, matchID)
	}
	if ex.err != nil {
		return ex.err
	}
	return tx.Commit()
}

// stillActive reports whether an account is tracked and not paused. A run
// can take minutes, so long loops check it before every API call to make
// pausing take effect right away.
func (c *Collector) stillActive(ctx context.Context, accountID int64) bool {
	var active bool
	err := c.db.Read.QueryRowContext(ctx, "SELECT is_active FROM accounts WHERE account_id = ?", accountID).Scan(&active)
	return err == nil && active
}

// nextPollAt returns when the earliest pending parse job is due, if any.
func nextPollAt(ctx context.Context, q *sql.DB) (time.Time, bool, error) {
	var at sql.NullInt64
	err := q.QueryRowContext(ctx, "SELECT min(j.next_check_at) FROM parse_jobs j WHERE j.status = 'pending' AND "+activeJob).Scan(&at)
	if err != nil || !at.Valid {
		return time.Time{}, false, err
	}
	return time.Unix(at.Int64, 0), true, nil
}
