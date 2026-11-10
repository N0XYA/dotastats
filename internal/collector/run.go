// Package collector fetches data from OpenDota into the database. Every
// step is idempotent, so an interrupted run resumes from database state.
package collector

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dotastats/internal/analysis"
	"dotastats/internal/db"
	"dotastats/internal/opendota"
)

// Collector runs the collection pipeline against one database.
type Collector struct {
	db        *db.DB
	api       *opendota.Client
	budget    *opendota.Budget
	assetsDir string
	icons     *http.Client
	iconBase  string
	now       func() time.Time
}

// Config places a Collector's files.
type Config struct {
	AssetsDir   string // receives hero and item icons
	IconBaseURL string // default opendota.IconBaseURL
}

// New returns a Collector.
func New(store *db.DB, api *opendota.Client, budget *opendota.Budget, cfg Config) *Collector {
	if cfg.IconBaseURL == "" {
		cfg.IconBaseURL = opendota.IconBaseURL
	}
	return &Collector{
		db:        store,
		api:       api,
		budget:    budget,
		assetsDir: cfg.AssetsDir,
		icons:     &http.Client{Timeout: 30 * time.Second},
		iconBase:  strings.TrimSuffix(cfg.IconBaseURL, "/"),
		now:       time.Now,
	}
}

// Budget returns the daily API budget the collector spends.
func (c *Collector) Budget() *opendota.Budget { return c.budget }

// RunOptions select what a run does.
type RunOptions struct {
	// Full fetches each account's whole match history instead of only
	// what is new since its last sync.
	Full bool
	// AccountID limits the run to one account; 0 means every active one.
	AccountID int64
	// PollOnly only checks parse jobs that are due.
	PollOnly bool
}

// AccountReport is what a run did for one account.
type AccountReport struct {
	Account    Account
	NewMatches int // matches not stored before
	NewLinks   int // stored matches newly linked to this account
	Calls      int
}

// Report summarizes a run.
type Report struct {
	Accounts      []AccountReport
	Details       int // matches whose details were stored
	ParseRequests int
	Parsed        int // pending matches that came back parsed
	// OtherCalls were not made for one account (constants, aggregates).
	OtherCalls      int
	BudgetExhausted bool
}

// NewMatches totals the accounts' new matches.
func (r Report) NewMatches() int {
	n := 0
	for _, a := range r.Accounts {
		n += a.NewMatches
	}
	return n
}

// Calls totals the API calls of the run.
func (r Report) Calls() int {
	n := r.OtherCalls
	for _, a := range r.Accounts {
		n += a.Calls
	}
	return n
}

// Keys in collector_state describing the last run, for the Status page.
const (
	StateLastRunStarted  = "last_run_started_at"
	StateLastRunFinished = "last_run_finished_at"
	StateLastRunError    = "last_run_error"
)

// Run is one collection cycle, in priority order: constants (daily),
// profiles (every 6 h), new matches, details of recent matches, parse
// requests and polls, aggregates (daily), details of older matches up to
// their budget share, and retention (daily). When the daily budget runs
// out it stops cleanly; the next run carries on. A failure in one step
// doesn't stop the others; all failures are returned joined.
func (c *Collector) Run(ctx context.Context, opts RunOptions) (Report, error) {
	s, err := LoadSettings(ctx, c.db.Read)
	if err != nil {
		return Report{}, err
	}
	c.budget.SetLimit(s.DailyBudget)
	accounts, err := activeAccounts(ctx, c.db.Read, opts.AccountID)
	if err != nil {
		return Report{}, err
	}
	before, err := c.budget.UsedByAccount(ctx)
	if err != nil {
		return Report{}, err
	}
	if !opts.PollOnly {
		if err := c.setState(ctx, StateLastRunStarted, strconv.FormatInt(c.now().Unix(), 10)); err != nil {
			return Report{}, err
		}
	}

	rep := Report{Accounts: make([]AccountReport, len(accounts))}
	for i, a := range accounts {
		rep.Accounts[i].Account = a
	}
	runErr := c.steps(ctx, opts, s, &rep)

	// Bookkeeping must happen even when the run was cancelled.
	bctx := context.WithoutCancel(ctx)
	after, err := c.budget.UsedByAccount(bctx)
	if err != nil {
		return rep, errors.Join(runErr, err)
	}
	for i := range rep.Accounts {
		id := rep.Accounts[i].Account.ID
		rep.Accounts[i].Calls = after[id] - before[id]
	}
	rep.OtherCalls = after[0] - before[0]
	if !opts.PollOnly {
		msg := ""
		if runErr != nil {
			msg = runErr.Error()
		}
		err = errors.Join(
			c.setState(bctx, StateLastRunFinished, strconv.FormatInt(c.now().Unix(), 10)),
			c.setState(bctx, StateLastRunError, msg))
	}
	return rep, errors.Join(runErr, err)
}

func (c *Collector) steps(ctx context.Context, opts RunOptions, s Settings, rep *Report) error {
	var errs []error
	// stop records err and reports whether the run must end here.
	stop := func(err error) bool {
		switch {
		case err == nil:
			return false
		case errors.Is(err, opendota.ErrBudgetExhausted):
			rep.BudgetExhausted = true
			return true
		case ctx.Err() != nil:
			errs = append(errs, ctx.Err())
			return true
		}
		errs = append(errs, err)
		return false
	}
	accounts := make([]Account, len(rep.Accounts))
	for i, r := range rep.Accounts {
		accounts[i] = r.Account
	}

	if opts.PollOnly {
		n, err := c.pollParses(ctx, s)
		rep.Parsed += n
		stop(err)
		return errors.Join(errs...)
	}

	if stop(c.syncConstants(ctx)) {
		return errors.Join(errs...)
	}
	if stop(c.upgradeAnalysis(ctx)) {
		return errors.Join(errs...)
	}
	for i := range rep.Accounts {
		if !c.stillActive(ctx, rep.Accounts[i].Account.ID) {
			continue
		}
		if stop(c.syncProfile(ctx, &rep.Accounts[i].Account)) {
			return errors.Join(errs...)
		}
	}
	for i := range rep.Accounts {
		r := &rep.Accounts[i]
		if !c.stillActive(ctx, r.Account.ID) {
			continue
		}
		var err error
		r.NewMatches, r.NewLinks, err = c.discover(ctx, r.Account, opts.Full)
		if stop(err) {
			return errors.Join(errs...)
		}
	}
	none := new(int)
	for _, step := range []struct {
		count *int
		run   func() (int, error)
	}{
		{&rep.Details, func() (int, error) { return c.fetchDetails(ctx, s, accounts, true) }},
		{&rep.ParseRequests, func() (int, error) { return c.requestParses(ctx, s, accounts) }},
		{&rep.Parsed, func() (int, error) { return c.pollParses(ctx, s) }},
		{none, func() (int, error) { return 0, c.syncAggregates(ctx, len(accounts) > 0) }},
		{&rep.Details, func() (int, error) { return c.fetchDetails(ctx, s, accounts, false) }},
		{none, func() (int, error) { return 0, c.syncRetention(ctx, s) }},
	} {
		n, err := step.run()
		*step.count += n
		if stop(err) {
			break
		}
	}
	return errors.Join(errs...)
}

// stateAnalysisVersion remembers which analysis.Version stored insights
// were computed with.
const stateAnalysisVersion = "analysis_version"

// upgradeAnalysis re-extracts stored matches from their raw JSON when the
// analysis changed since they were stored. No API calls.
func (c *Collector) upgradeAnalysis(ctx context.Context) error {
	var v string
	err := c.db.Read.QueryRowContext(ctx, "SELECT value FROM collector_state WHERE key = ?", stateAnalysisVersion).Scan(&v)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	want := strconv.Itoa(analysis.Version)
	if v == want {
		return nil
	}
	rep, err := Reextract(ctx, c.db)
	if err != nil {
		return err
	}
	if rep.Matches > 0 {
		slog.Info("re-analyzed stored matches", "matches", rep.Matches, "analysis_version", analysis.Version)
	}
	return c.setState(ctx, stateAnalysisVersion, want)
}
