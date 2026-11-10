// Package opendota is the OpenDota API client: rate limit, persistent
// daily budget, retries with backoff and the optional API key.
package opendota

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// DefaultBaseURL is the public API root.
const DefaultBaseURL = "https://api.opendota.com/api"

// IconBaseURL is the Steam CDN that the img and icon paths in constants
// are relative to.
const IconBaseURL = "https://cdn.cloudflare.steamstatic.com"

const (
	maxAttempts = 4
	baseBackoff = time.Second
	maxBackoff  = 30 * time.Second
	// defaultInterval spaces requests at 50 a minute: OpenDota's free tier
	// allows 60, and running at exactly the limit drew 429s.
	defaultInterval = 1200 * time.Millisecond
	// minuteWait is the wait after a 429 without Retry-After, times the
	// attempt: the limit is per minute, so seconds of backoff don't help.
	minuteWait = 20 * time.Second
	// A 429 asking to wait longer than this is about the daily limit, not
	// the per-minute one, so it is returned instead of slept through.
	maxRetryAfter = time.Minute
	maxBodyBytes  = 32 << 20
)

// Options configure a Client. Zero values pick the defaults.
type Options struct {
	BaseURL   string // default DefaultBaseURL
	APIKey    string // optional; sent as the api_key query parameter
	UserAgent string // default "dotastats"
	// Budget is the daily call budget. Nil means unlimited (tests only).
	Budget *Budget
	// Limiter spaces requests. Default 50 a minute. Use one Client per
	// process so the limit is shared.
	Limiter *rate.Limiter
	// CacheDir, when set, serves GETs from files there and saves new
	// responses to it. Development only: entries never expire and cached
	// responses spend no budget.
	CacheDir   string
	HTTPClient *http.Client
}

// Client calls the OpenDota API.
type Client struct {
	baseURL   string
	apiKey    string
	userAgent string
	cacheDir  string
	budget    *Budget
	limiter   *rate.Limiter
	http      *http.Client
	sleep     func(context.Context, time.Duration) error
	now       func() time.Time
	// coolDownUntil (unix nanoseconds) holds requests back after OpenDota
	// reported this minute's allowance used up.
	coolDownUntil atomic.Int64
}

// New returns a Client for o.
func New(o Options) *Client {
	c := &Client{
		baseURL:   strings.TrimSuffix(o.BaseURL, "/"),
		apiKey:    o.APIKey,
		userAgent: o.UserAgent,
		cacheDir:  o.CacheDir,
		budget:    o.Budget,
		limiter:   o.Limiter,
		http:      o.HTTPClient,
		sleep:     sleepCtx,
		now:       time.Now,
	}
	if c.baseURL == "" {
		c.baseURL = DefaultBaseURL
	}
	if c.userAgent == "" {
		c.userAgent = "dotastats"
	}
	if c.limiter == nil {
		c.limiter = rate.NewLimiter(rate.Every(defaultInterval), 1)
	}
	if c.http == nil {
		c.http = &http.Client{
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
				TLSHandshakeTimeout: 10 * time.Second,
				IdleConnTimeout:     90 * time.Second,
			},
			// 10 s to connect plus 30 s to read the response.
			Timeout: 40 * time.Second,
		}
	}
	return c
}

// StatusError is a response other than 200 OK.
type StatusError struct {
	Method     string
	Path       string
	StatusCode int
	// RetryAfter is the Retry-After header of a 429, or 0 if absent.
	RetryAfter time.Duration
	Body       string // start of the body, for logs
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("opendota: %s %s: status %d: %s", e.Method, e.Path, e.StatusCode, e.Body)
}

// IsNotFound reports whether err is a 404, e.g. an account OpenDota has
// never seen.
func IsNotFound(err error) bool {
	se, ok := errors.AsType[*StatusError](err)
	return ok && se.StatusCode == http.StatusNotFound
}

// netError is a failure to get a complete response. It is retried like a
// 5xx.
type netError struct {
	op  string
	err error
}

func (e *netError) Error() string { return "opendota: " + e.op + ": " + e.err.Error() }
func (e *netError) Unwrap() error { return e.err }

// Player fetches a player's profile and rank.
func (c *Client) Player(ctx context.Context, accountID int64) (Player, error) {
	var p Player
	err := c.getJSON(ctx, fmt.Sprintf("/players/%d", accountID), nil, &p)
	return p, err
}

// PlayerMatches fetches a player's match list. query takes the filters
// documented for the endpoint, e.g. limit, date, significant.
func (c *Client) PlayerMatches(ctx context.Context, accountID int64, query url.Values) ([]PlayerMatch, error) {
	var ms []PlayerMatch
	err := c.getJSON(ctx, fmt.Sprintf("/players/%d/matches", accountID), query, &ms)
	return ms, err
}

// Match fetches a match. It returns the raw body too, which is stored as is.
func (c *Client) Match(ctx context.Context, matchID int64) (Match, []byte, error) {
	return c.match(ctx, matchID, true)
}

// MatchFresh is Match without the dev cache, for checking whether a match
// has been parsed since it was last fetched.
func (c *Client) MatchFresh(ctx context.Context, matchID int64) (Match, []byte, error) {
	return c.match(ctx, matchID, false)
}

func (c *Client) match(ctx context.Context, matchID int64, cached bool) (Match, []byte, error) {
	path := fmt.Sprintf("/matches/%d", matchID)
	raw, err := c.get(ctx, path, nil, cached)
	if err != nil {
		return Match{}, nil, err
	}
	var m Match
	if err := json.Unmarshal(raw, &m); err != nil {
		return Match{}, nil, fmt.Errorf("opendota: decode %s: %w", path, err)
	}
	return m, raw, nil
}

// parseRequestCost is what POST /request counts as against the rate
// limit, per the spec.
const parseRequestCost = 10

// RequestParse asks OpenDota to parse a match's replay and returns the job
// ID. It costs parseRequestCost calls.
func (c *Client) RequestParse(ctx context.Context, matchID int64) (int64, error) {
	path := fmt.Sprintf("/request/%d", matchID)
	b, err := c.do(ctx, http.MethodPost, path, nil, parseRequestCost)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Job *struct {
			JobID int64 `json:"jobId"`
		} `json:"job"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return 0, fmt.Errorf("opendota: decode %s: %w", path, err)
	}
	if resp.Job == nil {
		return 0, fmt.Errorf("opendota: %s: no job in response %s", path, truncate(string(b), 200))
	}
	return resp.Job.JobID, nil
}

// ParseJobQueued reports whether a parse job is still in OpenDota's queue.
// The endpoint answers null once the job is gone, whether it succeeded or
// not, so the match itself must be checked to tell which.
func (c *Client) ParseJobQueued(ctx context.Context, jobID int64) (bool, error) {
	b, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/request/%d", jobID), nil, 1)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(b)) != "null", nil
}

// Constants decodes GET /constants/{resource} into v.
func (c *Client) Constants(ctx context.Context, resource string, v any) error {
	return c.getJSON(ctx, "/constants/"+resource, nil, v)
}

func (c *Client) getJSON(ctx context.Context, path string, query url.Values, v any) error {
	b, err := c.Get(ctx, path, query)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("opendota: decode %s: %w", path, err)
	}
	return nil
}

// Get fetches path, relative to the base URL, and returns the body.
func (c *Client) Get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	return c.get(ctx, path, query, true)
}

// get is Get; readCache false skips reading the dev cache but still
// updates it.
func (c *Client) get(ctx context.Context, path string, query url.Values, readCache bool) ([]byte, error) {
	var cached string
	if c.cacheDir != "" {
		cached = cacheFile(c.cacheDir, path, query)
		if b, err := os.ReadFile(cached); err == nil && readCache {
			return b, nil
		}
	}
	b, err := c.do(ctx, http.MethodGet, path, query, 1)
	if err == nil && cached != "" {
		if err := writeCache(cached, b); err != nil {
			slog.Warn("opendota: write dev cache", "err", err)
		}
	}
	return b, err
}

// do sends the request, retrying per retryWait. cost is how many calls the
// request counts as against the budget and the rate limit.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, cost int) ([]byte, error) {
	for attempt := 1; ; attempt++ {
		body, err := c.once(ctx, method, path, query, cost)
		if err == nil {
			return body, nil
		}
		wait, retry := retryWait(err, attempt)
		if !retry || attempt == maxAttempts || ctx.Err() != nil {
			return nil, err
		}
		slog.Info("opendota: retrying", "method", method, "path", path, "attempt", attempt, "wait", wait.Round(time.Millisecond), "err", err)
		if err := c.sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

func (c *Client) once(ctx context.Context, method, path string, query url.Values, cost int) ([]byte, error) {
	if c.budget != nil {
		if err := c.budget.Spend(ctx, cost); err != nil {
			return nil, err
		}
	}
	for range cost {
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, err
		}
	}
	if wait := time.Unix(0, c.coolDownUntil.Load()).Sub(c.now()); wait > 0 {
		slog.Info("opendota: minute allowance used up, pausing", "wait", wait.Round(time.Second))
		if err := c.sleep(ctx, wait); err != nil {
			return nil, err
		}
	}

	q := maps.Clone(query)
	if c.apiKey != "" {
		if q == nil {
			q = url.Values{}
		}
		q.Set("api_key", c.apiKey)
	}
	u := c.baseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)

	op := method + " " + path
	resp, err := c.http.Do(req)
	if err != nil {
		// *url.Error repeats the full URL, which may contain the API key.
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err
		}
		return nil, &netError{op: op, err: err}
	}
	defer resp.Body.Close()
	// OpenDota reports what is left of the per-minute allowance; at zero,
	// wait before the next request instead of drawing a 429.
	if left, err := strconv.Atoi(resp.Header.Get("X-Rate-Limit-Remaining-Minute")); err == nil && left <= 0 {
		c.coolDownUntil.Store(c.now().Add(minuteWait).UnixNano())
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, &netError{op: op, err: err}
	}
	if len(body) > maxBodyBytes {
		return nil, fmt.Errorf("opendota: %s: response larger than %d bytes", op, maxBodyBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{
			Method:     method,
			Path:       path,
			StatusCode: resp.StatusCode,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
			Body:       truncate(string(body), 200),
		}
	}
	return body, nil
}

// retryWait decides whether err is worth another attempt and how long to
// wait first: 429 and 5xx responses and network failures are retried,
// other 4xx responses and budget exhaustion are not.
func retryWait(err error, attempt int) (time.Duration, bool) {
	if se, ok := errors.AsType[*StatusError](err); ok {
		switch {
		case se.StatusCode == http.StatusTooManyRequests:
			if se.RetryAfter > maxRetryAfter {
				return 0, false
			}
			if se.RetryAfter > 0 {
				return se.RetryAfter, true
			}
			return time.Duration(attempt) * minuteWait, true
		case se.StatusCode >= 500:
			return backoff(attempt), true
		}
		return 0, false
	}
	if _, ok := errors.AsType[*netError](err); ok {
		return backoff(attempt), true
	}
	return 0, false
}

// backoff doubles from baseBackoff up to maxBackoff, with the upper half
// randomized so retries from several runs don't line up.
func backoff(attempt int) time.Duration {
	d := min(baseBackoff<<(attempt-1), maxBackoff)
	return d/2 + rand.N(d/2)
}

// parseRetryAfter reads delay-seconds or an HTTP date.
func parseRetryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// cacheFile maps a request to a readable file name, e.g.
// players_1_matches_limit=20.json. Long names (a project= list) are cut
// and suffixed with a hash of the full request to stay unique and within
// file name limits. The API key never reaches it.
func cacheFile(dir, path string, query url.Values) string {
	key := strings.Trim(path, "/")
	if len(query) > 0 {
		key += "_" + query.Encode()
	}
	name := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '=' || r == '.' {
			return r
		}
		return '_'
	}, key)
	if len(name) > maxCacheName {
		sum := sha256.Sum256([]byte(key))
		name = name[:maxCacheName-17] + "-" + hex.EncodeToString(sum[:8])
	}
	return filepath.Join(dir, name+".json")
}

const maxCacheName = 100

func writeCache(file string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	return os.WriteFile(file, b, 0o644)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
