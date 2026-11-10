package collector

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"dotastats/internal/analysis"
	"dotastats/internal/db"
	"dotastats/internal/opendota"
)

const fixtureDir = "../../testdata/opendota"

// Synthetic accounts for discovery tests: the owner and two friends all
// played match 100; 101 is the owner's alone, 102 friend A's.
const (
	owner   = 1
	friendA = 2
	friendB = 3
)

// Real recorded data for the details and parsing tests.
const (
	realOwner     = 337895673
	pendingMatch  = 9056907605 // unparsed, then parsed after a request
	parsedMatch   = 9017652399 // parsed from the start
	oldMatch      = 2324742574 // 2016; replay long gone
	recordedJobID = 602520851
)

var t0 = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

// fakeAPI serves OpenDota: synthetic players and match lists, recorded
// match details, parse jobs, constants and aggregates, placeholder icons.
type fakeAPI struct {
	t       *testing.T
	players map[int64]string           // persona names; others 404
	matches map[int64][]map[string]any // match list per account
	details map[int64]string           // match ID -> fixture file

	mu        sync.Mutex
	requests  []string
	jobQueued bool // GET /request/{job} answers with the job, else null
	// onRequest, when set, runs before each response (outside mu).
	onRequest func(path string)
}

func newFakeAPI(t *testing.T) *fakeAPI {
	return &fakeAPI{
		t:       t,
		players: map[int64]string{owner: "Owner", friendA: "Friend A", friendB: "Friend B"},
		matches: map[int64][]map[string]any{
			// Newest first, like OpenDota. Match 100 started during 7.41.
			owner:   {pm(101, 0, 1, t0.Add(-time.Hour)), pm(100, 1, 44, t0.Add(-2*time.Hour))},
			friendA: {pm(102, 3, 6, t0.Add(-30*time.Minute)), pm(100, 2, 48, t0.Add(-2*time.Hour))},
			friendB: {pm(100, 130, 6, t0.Add(-2*time.Hour))},
		},
		details: map[int64]string{},
	}
}

// pm is a projected /players/{id}/matches entry.
func pm(matchID int64, slot, hero int, start time.Time) map[string]any {
	return map[string]any{
		"match_id": matchID, "player_slot": slot, "radiant_win": true, "hero_id": hero,
		"start_time": start.Unix(), "duration": 2000, "game_mode": 22, "lobby_type": 7,
		"version": nil, "kills": 5, "deaths": 3, "assists": 7, "average_rank": 51,
		"leaver_status": 0, "party_size": 3, "gold_per_min": 600, "xp_per_min": 700,
		"last_hits": 200, "denies": 10, "hero_damage": 20000, "tower_damage": 3000,
		"hero_healing": 0, "level": 25, "item_0": 1, "item_1": 0, "item_2": 0,
		"item_3": 0, "item_4": 0, "item_5": 0, "item_neutral": 1605,
		"lane": nil, "lane_role": nil, "is_roaming": nil,
	}
}

// useRealOwner serves the recorded owner and three recorded matches.
func (f *fakeAPI) useRealOwner() {
	f.players[realOwner] = "MayFeelThis"
	f.details[pendingMatch] = "match_9056907605_unparsed.json"
	f.details[parsedMatch] = "match_9017652399_parsed.json"
	f.details[oldMatch] = "match_2324742574_old.json"
	for _, id := range []int64{pendingMatch, parsedMatch, oldMatch} {
		var m opendota.Match
		if err := json.Unmarshal(f.fixture(f.details[id]), &m); err != nil {
			f.t.Fatal(err)
		}
		for _, p := range m.Players {
			if p.AccountID != nil && *p.AccountID == realOwner {
				f.matches[realOwner] = append(f.matches[realOwner], pm(id, p.PlayerSlot, p.HeroID, time.Unix(m.StartTime, 0)))
			}
		}
	}
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	queued, hook := f.jobQueued, f.onRequest
	f.mu.Unlock()
	if hook != nil {
		hook(r.URL.Path)
	}

	segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	id, _ := strconv.ParseInt(segs[len(segs)-1], 10, 64)
	switch {
	case segs[0] == "players" && len(segs) == 2:
		name, ok := f.players[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			w.Write(f.fixture("player_not_found.json"))
			return
		}
		writeJSON(w, map[string]any{
			"profile":   map[string]any{"account_id": id, "personaname": name, "avatarmedium": "https://example.invalid/a.jpg"},
			"rank_tier": 50,
		})
	case segs[0] == "players" && len(segs) == 3 && segs[2] == "matches":
		id, _ := strconv.ParseInt(segs[1], 10, 64)
		ms := f.matches[id]
		if r.URL.Query().Get("limit") == "1" && len(ms) > 1 {
			ms = ms[:1]
		}
		if ms == nil {
			ms = []map[string]any{}
		}
		writeJSON(w, ms)
	case segs[0] == "matches":
		name, ok := f.details[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(f.fixture(name))
	case segs[0] == "request" && r.Method == http.MethodPost:
		w.Write(f.fixture("request_post_9056907605.json"))
	case segs[0] == "request" && queued:
		w.Write(f.fixture("request_job_queued.json"))
	case segs[0] == "request":
		w.Write(f.fixture("request_job_not_queued.json"))
	case segs[0] == "constants":
		w.Write(f.fixture("constants_" + segs[1] + ".json"))
	case segs[0] == "heroStats":
		w.Write(f.fixture("hero_stats.json"))
	case segs[0] == "benchmarks":
		w.Write(f.fixture("benchmarks_hero_44.json"))
	case segs[0] == "apps":
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("\x89PNG fake " + r.URL.Path))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeAPI) fixture(name string) []byte {
	b, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		f.t.Error(err)
	}
	return b
}

func (f *fakeAPI) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

// seen counts requests (as "METHOD path?query") containing every part.
func (f *fakeAPI) seen(parts ...string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
outer:
	for _, r := range f.requests {
		for _, p := range parts {
			if !strings.Contains(r, p) {
				continue outer
			}
		}
		n++
	}
	return n
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

type env struct {
	c      *Collector
	api    *fakeAPI
	store  *db.DB
	now    *time.Time
	assets string
}

// discoveryOnly keeps runs to constants, profiles and discovery: no match
// is recent and older matches get no budget.
var discoveryOnly = map[string]int{KeyRecentDays: 0, KeyBackfillBudgetPct: 0}

func newEnv(t *testing.T, budgetLimit int, settings map[string]int) env {
	t.Helper()
	api := newFakeAPI(t)
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	store, err := db.Open(filepath.Join(t.TempDir(), "dota.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if _, _, err := db.Migrate(context.Background(), store.Write); err != nil {
		t.Fatal(err)
	}

	budget := opendota.NewBudget(store.Write, budgetLimit)
	client := opendota.New(opendota.Options{BaseURL: srv.URL, Budget: budget, Limiter: rate.NewLimiter(rate.Inf, 1)})
	assets := t.TempDir()
	c := New(store, client, budget, Config{AssetsDir: assets, IconBaseURL: srv.URL})
	now := t0
	c.now = func() time.Time { return now }
	e := env{c: c, api: api, store: store, now: &now, assets: assets}
	e.setting(t, KeyDailyBudget, budgetLimit)
	for k, v := range settings {
		e.setting(t, k, v)
	}
	return e
}

func (e env) setting(t *testing.T, key string, value int) {
	t.Helper()
	if _, err := e.store.Write.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, strconv.Itoa(value)); err != nil {
		t.Fatal(err)
	}
}

func (e env) add(t *testing.T, input string) Account {
	t.Helper()
	res, err := e.c.AddAccount(context.Background(), input, "")
	if err != nil {
		t.Fatalf("AddAccount(%q): %v", input, err)
	}
	return res.Account
}

func (e env) run(t *testing.T, opts RunOptions) Report {
	t.Helper()
	rep, err := e.c.Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return rep
}

func (e env) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := e.store.Read.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func (e env) str(t *testing.T, query string, args ...any) string {
	t.Helper()
	var s sql.NullString
	if err := e.store.Read.QueryRow(query, args...).Scan(&s); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return s.String
}

func (e env) parseState(t *testing.T, matchID int64) string {
	return e.str(t, "SELECT parse_state FROM matches WHERE match_id = ?", matchID)
}

// rowCounts snapshots every table that discovery writes.
func (e env) rowCounts(t *testing.T) map[string]int {
	counts := map[string]int{}
	for _, table := range []string{"accounts", "matches", "account_matches", "match_players", "heroes", "items", "game_modes", "lobby_types", "patches"} {
		counts[table] = e.count(t, "SELECT count(*) FROM "+table)
	}
	return counts
}

func TestBackfillOwnerAndFriends(t *testing.T) {
	e := newEnv(t, 1000, discoveryOnly)
	// Three accepted input forms: account ID, profile URL, SteamID64.
	if a := e.add(t, "1"); a.Role != RoleOwner || a.Name != "Owner" {
		t.Errorf("first account = %+v, want the owner", a)
	}
	if a := e.add(t, "https://www.dotabuff.com/players/2"); a.Role != RoleFriend {
		t.Errorf("second account role = %s, want friend", a.Role)
	}
	e.add(t, "76561197960265731")

	rep := e.run(t, RunOptions{Full: true})

	if got := e.count(t, "SELECT count(*) FROM matches"); got != 3 {
		t.Errorf("matches = %d, want 3", got)
	}
	if got := e.count(t, "SELECT count(*) FROM account_matches WHERE match_id = 100"); got != 3 {
		t.Errorf("match 100 linked to %d accounts, want 3", got)
	}
	if got := e.count(t, "SELECT count(*) FROM match_players WHERE match_id = 100"); got != 3 {
		t.Errorf("match 100 has %d player rows, want one per tracked account", got)
	}
	if got := e.count(t, "SELECT count(*) FROM match_players WHERE match_id = 100 AND account_id = 3 AND player_slot = 130 AND gold_per_min = 600 AND item_neutral = 1605"); got != 1 {
		t.Error("friend B's row of match 100 is missing or wrong")
	}
	// 7.41 (id 60) was released before t0.
	if got := e.count(t, "SELECT count(*) FROM matches WHERE patch = 60 AND is_parsed = 0 AND radiant_win = 1 AND avg_rank_tier = 51"); got != 3 {
		t.Errorf("%d matches with patch 60 and basic fields, want 3", got)
	}

	// Profiles were fetched by AddAccount just now, so only match lists.
	want := map[int64][3]int{ // new matches, new links, calls
		owner:   {2, 2, 1},
		friendA: {1, 2, 1},
		friendB: {0, 1, 1},
	}
	for _, r := range rep.Accounts {
		if got := [3]int{r.NewMatches, r.NewLinks, r.Calls}; got != want[r.Account.ID] {
			t.Errorf("account %d: report %v, want %v", r.Account.ID, got, want[r.Account.ID])
		}
	}
	if rep.Accounts[0].Account.ID != owner {
		t.Errorf("first account in run = %d, want the owner", rep.Accounts[0].Account.ID)
	}
	if rep.OtherCalls != 11 {
		t.Errorf("other calls = %d, want 6 constants + heroStats + benchmarks for 4 heroes", rep.OtherCalls)
	}

	// Re-running stores nothing new and skips the daily steps.
	before := e.rowCounts(t)
	rep = e.run(t, RunOptions{Full: true})
	if after := e.rowCounts(t); !equalCounts(before, after) {
		t.Errorf("second backfill changed row counts: %v -> %v", before, after)
	}
	for _, r := range rep.Accounts {
		if r.NewMatches != 0 || r.NewLinks != 0 {
			t.Errorf("second backfill, account %d: %+v", r.Account.ID, r)
		}
	}
	if rep.OtherCalls != 0 {
		t.Errorf("second backfill made %d daily calls, want 0", rep.OtherCalls)
	}
}

func equalCounts(a, b map[string]int) bool {
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return len(a) == len(b)
}

func TestIncrementalDiscovery(t *testing.T) {
	e := newEnv(t, 1000, discoveryOnly)
	e.add(t, "1")
	e.run(t, RunOptions{})
	if e.api.seen("/players/1/matches", "date=") != 0 {
		t.Error("first sync of a new account used a date filter, want full history")
	}
	e.add(t, "2")

	*e.now = t0.Add(3*24*time.Hour + time.Hour)
	e.run(t, RunOptions{})
	if e.api.seen("/players/1/matches", "date=5", "significant=0", "project=gold_per_min") != 1 {
		t.Errorf("incremental sync didn't ask for 3+2 days: %v", e.api.requests)
	}
	if e.api.seen("/players/2/matches", "date=") != 0 {
		t.Error("a never-synced account got a date filter")
	}
}

func TestHiddenHistoryIsRefetchedInFull(t *testing.T) {
	e := newEnv(t, 1000, discoveryOnly)
	e.api.players[4] = "Hidden"
	e.add(t, "4")
	e.run(t, RunOptions{})

	*e.now = t0.Add(time.Hour)
	e.api.set(func() { e.api.matches[4] = []map[string]any{pm(200, 0, 1, t0.Add(-400*24*time.Hour))} })
	rep := e.run(t, RunOptions{})
	if e.api.seen("/players/4/matches", "date=") != 0 {
		t.Error("account with an empty history got a date filter; old matches would be missed")
	}
	if rep.Accounts[0].NewMatches != 1 {
		t.Errorf("new matches = %d, want the year-old match once the history is visible", rep.Accounts[0].NewMatches)
	}
	if e.count(t, "SELECT count(*) FROM accounts WHERE account_id = 4 AND last_synced_at IS NOT NULL") != 1 {
		t.Error("account not marked synced after its history arrived")
	}
}

func TestProfilesRefreshEverySixHours(t *testing.T) {
	e := newEnv(t, 1000, discoveryOnly)
	e.add(t, "1") // fetches the profile once
	*e.now = t0.Add(time.Hour)
	e.run(t, RunOptions{})
	if n := e.api.seen("GET /players/1?"); n != 1 {
		t.Errorf("profile fetched %d times within 6 h, want 1 (by AddAccount)", n)
	}
	*e.now = t0.Add(7 * time.Hour)
	e.run(t, RunOptions{})
	if n := e.api.seen("GET /players/1?"); n != 2 {
		t.Errorf("profile fetched %d times after 7 h, want 2", n)
	}
}

func TestAddAccountErrors(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 1000, discoveryOnly)

	if _, err := e.c.AddAccount(ctx, "999", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown account: err = %v, want ErrNotFound", err)
	}
	e.add(t, "1")
	if _, err := e.c.AddAccount(ctx, "https://www.opendota.com/players/1", ""); !errors.Is(err, ErrAccountExists) {
		t.Errorf("duplicate: err = %v, want ErrAccountExists", err)
	}

	e.api.players[4] = "No Matches"
	res, err := e.c.AddAccount(ctx, "4", "Label")
	if err != nil {
		t.Fatal(err)
	}
	if !res.NoMatches || res.Account.DisplayName() != "Label" {
		t.Errorf("account without matches: %+v", res)
	}
}

func TestRemoveAccount(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 1000, discoveryOnly)
	e.add(t, "1")
	e.add(t, "2")
	e.add(t, "3")
	e.run(t, RunOptions{Full: true})

	if _, err := RemoveAccount(ctx, e.store.Write, owner); !errors.Is(err, ErrOwnerHasFriends) {
		t.Errorf("removing the owner: err = %v, want ErrOwnerHasFriends", err)
	}
	deleted, err := RemoveAccount(ctx, e.store.Write, friendA)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Errorf("deleted %d matches, want 1 (102, played only by friend A)", deleted)
	}
	if n := e.count(t, "SELECT count(*) FROM match_players WHERE match_id = 102"); n != 0 {
		t.Errorf("match 102 still has %d player rows", n)
	}
	if n := e.count(t, "SELECT count(*) FROM account_matches WHERE match_id = 100"); n != 2 {
		t.Errorf("match 100 has %d links, want 2", n)
	}
	// Friend A stays on the scoreboard of the shared match.
	if n := e.count(t, "SELECT count(*) FROM match_players WHERE match_id = 100"); n != 3 {
		t.Errorf("match 100 has %d player rows, want 3", n)
	}
	if _, err := RemoveAccount(ctx, e.store.Write, friendA); !errors.Is(err, ErrNoSuchAccount) {
		t.Errorf("removing twice: err = %v, want ErrNoSuchAccount", err)
	}
}

func TestPausedAccountIsSkipped(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 1000, discoveryOnly)
	e.add(t, "1")
	e.add(t, "3")
	if err := SetAccountActive(ctx, e.store.Write, friendB, false); err != nil {
		t.Fatal(err)
	}
	rep := e.run(t, RunOptions{Full: true})
	if len(rep.Accounts) != 1 || e.api.seen("/players/3/matches", "project=") != 0 {
		t.Errorf("paused account was collected: %+v", rep.Accounts)
	}
	if _, err := e.c.Run(ctx, RunOptions{AccountID: friendB}); !errors.Is(err, ErrNoSuchAccount) {
		t.Errorf("run for a paused account: err = %v, want ErrNoSuchAccount", err)
	}

	accounts, err := ListAccounts(ctx, e.store.Read)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 || accounts[0].ID != owner || accounts[0].Matches != 2 || accounts[1].Active {
		t.Errorf("ListAccounts = %+v", accounts)
	}
}

func TestConstantsAndIcons(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 1000, discoveryOnly)
	e.add(t, "1") // aggregates wait until an account is tracked
	e.run(t, RunOptions{})

	for table, want := range map[string]int{"heroes": 25, "items": 166, "game_modes": 26, "lobby_types": 16, "patches": 61} {
		if got := e.count(t, "SELECT count(*) FROM "+table); got != want {
			t.Errorf("%s: %d rows, want %d", table, got, want)
		}
	}
	if name := e.str(t, "SELECT localized_name FROM heroes WHERE id = 44"); name != "Phantom Assassin" {
		t.Errorf("hero 44 = %q", name)
	}
	if dname := e.str(t, "SELECT dname FROM items WHERE name = 'blink'"); dname != "Blink Dagger" {
		t.Errorf("blink = %q", dname)
	}

	// 25 hero images + 25 hero icons + 166 items, all recorded in the database.
	icons := e.api.seen("/apps/")
	if icons != 216 {
		t.Errorf("downloaded %d icons, want 216", icons)
	}
	if n := e.count(t, "SELECT count(*) FROM heroes WHERE img_path = 'heroes/' || id || '.png' AND icon_path IS NOT NULL"); n != 25 {
		t.Errorf("%d heroes with icon paths, want 25", n)
	}
	icon := filepath.Join(e.assets, "items", "1.png")
	if b, err := os.ReadFile(icon); err != nil || !strings.Contains(string(b), "blink.png") {
		t.Errorf("blink icon: %q, %v", b, err)
	}

	// Nothing missing: no downloads. A deleted file comes back alone.
	e.run(t, RunOptions{})
	if e.api.seen("/apps/") != icons {
		t.Error("second run downloaded icons again")
	}
	os.Remove(icon)
	e.run(t, RunOptions{})
	if e.api.seen("/apps/") != icons+1 || !fileExists(icon) {
		t.Error("deleted icon was not downloaded again (alone)")
	}

	// Constants and aggregates refresh after a day, not before.
	if e.api.seen("/constants/heroes") != 1 || e.api.seen("/heroStats") != 1 {
		t.Error("daily steps ran more than once within a day")
	}
	*e.now = t0.Add(25 * time.Hour)
	if _, err := e.c.Run(ctx, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if e.api.seen("/constants/heroes") != 2 || e.api.seen("/heroStats") != 2 {
		t.Error("daily steps not repeated after 25 h")
	}
	if n := e.count(t, "SELECT count(*) FROM hero_stats_cache"); n != 4 {
		t.Errorf("hero_stats_cache has %d rows, want 2 heroes x 2 UTC days", n)
	}
}

func TestBudgetExhaustedStopsCleanly(t *testing.T) {
	e := newEnv(t, 8, discoveryOnly) // add: 2 calls; run: 6 constants, then out
	e.add(t, "1")
	rep, err := e.c.Run(context.Background(), RunOptions{Full: true})
	if err != nil {
		t.Fatalf("Run returned %v, want a clean stop", err)
	}
	if !rep.BudgetExhausted {
		t.Error("report doesn't say the budget ran out")
	}
	if e.api.seen("/players/1/matches", "project=") != 0 {
		t.Error("discovery ran past the budget")
	}
	if e.count(t, "SELECT count(*) FROM accounts WHERE account_id = 1 AND last_synced_at IS NULL") != 1 {
		t.Error("account marked synced although discovery didn't run")
	}
}

// realEnv tracks the recorded owner with three recorded matches: one
// pending parse (2 days old), one parsed (4 weeks), one from 2016.
func realEnv(t *testing.T, budget int) env {
	e := newEnv(t, budget, map[string]int{KeyRecentDays: 30, KeyBackfillBudgetPct: 100})
	e.api.useRealOwner()
	e.add(t, strconv.Itoa(realOwner))
	return e
}

func TestParseLifecycle(t *testing.T) {
	// Budget: add 2, constants 6, discovery 1, two recent details 2 = 11.
	// The parse request (10) doesn't fit yet.
	e := realEnv(t, 15)
	rep := e.run(t, RunOptions{})
	if !rep.BudgetExhausted || rep.Details != 2 {
		t.Fatalf("cycle 1: %+v, want 2 details then out of budget", rep)
	}
	if s := e.parseState(t, pendingMatch); s != ParseUnparsed {
		t.Fatalf("cycle 1: %s, want unparsed", s)
	}
	if s := e.parseState(t, parsedMatch); s != ParseParsed {
		t.Errorf("already parsed match: %s", s)
	}

	e.setting(t, KeyDailyBudget, 1000)
	*e.now = t0.Add(time.Minute)
	rep = e.run(t, RunOptions{})
	if rep.ParseRequests != 1 || e.parseState(t, pendingMatch) != ParsePending {
		t.Fatalf("cycle 2: %+v, state %s; want one request and pending", rep, e.parseState(t, pendingMatch))
	}
	if e.api.seen("POST /request/9056907605") != 1 {
		t.Error("no POST /request for the unparsed match")
	}
	// The old match got details through backfill but no parse request.
	if s := e.parseState(t, oldMatch); s != ParseUnparsed || e.api.seen("POST /request/2324742574") != 0 {
		t.Errorf("old match: state %s, requests %d", s, e.api.seen("POST /request/2324742574"))
	}
	if at, ok, _ := nextPollAt(context.Background(), e.store.Read); !ok || !at.Equal(t0.Add(time.Minute+pollDelay(0))) {
		t.Errorf("next poll at %v, want 2 min after the request", at)
	}

	e.api.set(func() { e.api.details[pendingMatch] = "match_9056907605_parsed.json" })
	*e.now = t0.Add(4 * time.Minute)
	rep = e.run(t, RunOptions{PollOnly: true})
	if rep.Parsed != 1 || e.parseState(t, pendingMatch) != ParseParsed {
		t.Fatalf("cycle 3: %+v, state %s; want parsed", rep, e.parseState(t, pendingMatch))
	}
	if s := e.str(t, "SELECT status FROM parse_jobs WHERE match_id = ?", pendingMatch); s != "done" {
		t.Errorf("job status %s, want done", s)
	}
	if n := e.count(t, "SELECT count(*) FROM player_timeseries WHERE match_id = ?", pendingMatch); n != 10 {
		t.Errorf("%d time series rows after parsing, want 10", n)
	}
	if n := e.count(t, "SELECT count(*) FROM match_raw WHERE match_id = ? AND parser_version = 22", pendingMatch); n != 1 {
		t.Error("raw JSON not replaced by the parsed version")
	}
}

func TestParseExpires(t *testing.T) {
	e := realEnv(t, 1000)
	e.run(t, RunOptions{})
	if e.parseState(t, pendingMatch) != ParsePending {
		t.Fatal("parse not requested")
	}
	// The job left the queue but the match never gets parsed.
	*e.now = t0.Add(3 * time.Minute)
	e.run(t, RunOptions{PollOnly: true})
	if s := e.parseState(t, pendingMatch); s != ParsePending {
		t.Fatalf("after the first 'gone' poll: %s, want still pending", s)
	}
	*e.now = t0.Add(10 * time.Minute)
	e.run(t, RunOptions{PollOnly: true})
	if s := e.parseState(t, pendingMatch); s != ParseExpired {
		t.Errorf("after the second 'gone' poll: %s, want expired", s)
	}
	if e.str(t, "SELECT last_error FROM parse_jobs WHERE match_id = ?", pendingMatch) == "" {
		t.Error("expired job has no explanation")
	}
}

func TestParseFailsWhileStillQueued(t *testing.T) {
	e := realEnv(t, 1000)
	e.api.set(func() { e.api.jobQueued = true })
	e.run(t, RunOptions{})
	for i := range maxPolls {
		*e.now = t0.Add(time.Duration(i+1) * 2 * time.Hour)
		e.run(t, RunOptions{PollOnly: true})
	}
	if s := e.parseState(t, pendingMatch); s != ParseFailed {
		t.Errorf("after %d polls: %s, want failed", maxPolls, s)
	}
	if n := e.count(t, "SELECT attempts FROM parse_jobs WHERE match_id = ?", pendingMatch); n != maxPolls {
		t.Errorf("attempts = %d, want %d", n, maxPolls)
	}
}

func TestExtractRecordedMatches(t *testing.T) {
	e := realEnv(t, 1000)
	e.run(t, RunOptions{})

	// Expected values computed from the fixtures independently.
	for _, c := range []struct {
		match                                     int64
		purchases, wards, wardsLeft, bench, times int
	}{
		{parsedMatch, 475, 116, 106, 100, 10},
		{pendingMatch, 0, 0, 0, 100, 0}, // still unparsed here
		{oldMatch, 0, 0, 0, 100, 0},
	} {
		got := []int{
			e.count(t, "SELECT count(*) FROM item_purchases WHERE match_id = ?", c.match),
			e.count(t, "SELECT count(*) FROM wards WHERE match_id = ?", c.match),
			e.count(t, "SELECT count(*) FROM wards WHERE match_id = ? AND left_time IS NOT NULL", c.match),
			e.count(t, "SELECT count(*) FROM player_benchmarks WHERE match_id = ?", c.match),
			e.count(t, "SELECT count(*) FROM player_timeseries WHERE match_id = ?", c.match),
		}
		want := []int{c.purchases, c.wards, c.wardsLeft, c.bench, c.times}
		if !slices.Equal(got, want) {
			t.Errorf("match %d: purchases, wards, wards with removal, benchmarks, time series = %v, want %v", c.match, got, want)
		}
		if n := e.count(t, "SELECT count(*) FROM match_players WHERE match_id = ?", c.match); n != 10 {
			t.Errorf("match %d: %d players, want 10", c.match, n)
		}
	}

	row := e.str(t, `SELECT printf('%d %d %d/%d/%d %d %d %d %d %d', player_slot, hero_id, kills, deaths, assists,
			gold_per_min, net_worth, item_neutral, item_neutral2, lane)
		FROM match_players WHERE match_id = ? AND account_id = ?`, parsedMatch, realOwner)
	if row != "130 48 16/6/6 778 29794 2190 1584 3" {
		t.Errorf("owner's row in %d = %q", parsedMatch, row)
	}
	if g := e.str(t, "SELECT gold_t FROM player_timeseries WHERE match_id = ? AND player_slot = 130", parsedMatch); !strings.HasPrefix(g, "[0,167,503,800,") {
		t.Errorf("gold_t = %.40s", g)
	}
	if b := e.str(t, "SELECT item FROM item_purchases WHERE match_id = ? AND player_slot = 130 AND seq = 0", parsedMatch); b != "magic_wand" {
		t.Errorf("first purchase = %q, want magic_wand", b)
	}
	if n := e.count(t, "SELECT count(*) FROM match_timeseries WHERE match_id = ? AND radiant_gold_adv LIKE '[0,489,583,695,%'", parsedMatch); n != 1 {
		t.Error("radiant_gold_adv not stored")
	}
	if n := e.count(t, "SELECT count(*) FROM match_players WHERE match_id = ? AND hero_damage IS NULL AND net_worth IS NOT NULL", oldMatch); n != 10 {
		t.Errorf("2016 match: %d rows with NULL hero damage, want 10", n)
	}
	if n := e.count(t, "SELECT count(*) FROM matches WHERE match_id = ? AND patch = 17 AND region = 3", oldMatch); n != 1 {
		t.Error("2016 match: patch and region not taken from details")
	}
	// Past raw retention (90 days) when fetched: no raw JSON kept.
	if n := e.count(t, "SELECT count(*) FROM matches m WHERE match_id = ? AND raw_pruned_at IS NOT NULL AND NOT EXISTS (SELECT 1 FROM match_raw r WHERE r.match_id = m.match_id)", oldMatch); n != 1 {
		t.Error("2016 match: raw JSON stored although past retention")
	}
}

// dump returns the rows of every table extraction writes, sorted.
func dump(t *testing.T, e env) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, table := range []string{"matches", "match_players", "match_timeseries", "player_timeseries", "item_purchases",
		"wards", "player_benchmarks", "match_events", "match_insights"} {
		rows, err := e.store.Read.Query("SELECT * FROM " + table)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			out[table] = append(out[table], fmt.Sprint(vals...))
		}
		rows.Close()
		slices.Sort(out[table])
	}
	return out
}

func TestReextractRebuildsIdenticalTables(t *testing.T) {
	ctx := context.Background()
	e := realEnv(t, 1000)
	e.run(t, RunOptions{})
	before := dump(t, e)

	// Wipe derived rows so reextract has to rebuild them, not just keep them.
	for _, table := range []string{"player_timeseries", "item_purchases", "wards", "player_benchmarks", "match_timeseries",
		"match_events", "match_insights"} {
		if _, err := e.store.Write.Exec("DELETE FROM "+table+" WHERE match_id != ?", oldMatch); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := Reextract(ctx, e.store)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Matches != 2 || rep.Skipped != 1 {
		t.Errorf("reextract report %+v, want 2 rebuilt and the 2016 match skipped", rep)
	}
	after := dump(t, e)
	for table, rows := range before {
		if !slices.Equal(rows, after[table]) {
			t.Errorf("%s differs after reextract: %d rows before, %d after", table, len(rows), len(after[table]))
		}
	}
}

func TestPrune(t *testing.T) {
	ctx := context.Background()
	e := realEnv(t, 1000)
	e.run(t, RunOptions{})
	players := e.count(t, "SELECT count(*) FROM match_players")

	s := DefaultSettings
	s.RawRetentionDays = 0
	rep, err := Prune(ctx, e.store.Write, s, e.c.now())
	if err != nil {
		t.Fatal(err)
	}
	if rep.RawDeleted != 2 || e.count(t, "SELECT count(*) FROM match_raw") != 0 {
		t.Errorf("raw retention 0: %+v, %d blobs left", rep, e.count(t, "SELECT count(*) FROM match_raw"))
	}
	if rep.FreedBytes <= 0 {
		t.Errorf("freed %d bytes, want the file to shrink", rep.FreedBytes)
	}
	if n := e.count(t, "SELECT count(*) FROM matches WHERE raw_pruned_at IS NOT NULL"); n != 3 {
		t.Errorf("%d matches marked pruned, want 3", n)
	}
	if n := e.count(t, "SELECT count(*) FROM match_players"); n != players {
		t.Errorf("scoreboard rows %d -> %d; pruning raw JSON must keep them", players, n)
	}
	if rr, err := Reextract(ctx, e.store); err != nil || rr.Matches != 0 || rr.Skipped != 3 {
		t.Errorf("reextract after pruning: %+v, %v", rr, err)
	}

	s.DetailRetentionDays = 0
	if _, err := Prune(ctx, e.store.Write, s, e.c.now()); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"player_timeseries", "item_purchases", "wards", "match_timeseries"} {
		if n := e.count(t, "SELECT count(*) FROM "+table); n != 0 {
			t.Errorf("detail retention 0: %s has %d rows", table, n)
		}
	}
	if n := e.count(t, "SELECT count(*) FROM match_players"); n != players {
		t.Error("detail retention removed scoreboard rows")
	}

	s.MatchRetentionDays = 0
	rep, err = Prune(ctx, e.store.Write, s, e.c.now())
	if err != nil {
		t.Fatal(err)
	}
	if rep.MatchesDeleted != 3 || e.count(t, "SELECT count(*) FROM match_players") != 0 || e.count(t, "SELECT count(*) FROM account_matches") != 0 {
		t.Errorf("match retention 0: %+v", rep)
	}
}

func TestBackfillBudgetShare(t *testing.T) {
	e := realEnv(t, 1000)
	e.setting(t, KeyBackfillBudgetPct, 1) // 10 calls: already spent before backfill
	e.run(t, RunOptions{})
	if n := e.count(t, "SELECT count(*) FROM matches WHERE match_id = ? AND details_at IS NULL", oldMatch); n != 1 {
		t.Error("old match details fetched beyond the backfill share")
	}
	if n := e.count(t, "SELECT count(*) FROM matches WHERE details_at IS NOT NULL"); n != 2 {
		t.Errorf("%d matches with details, want the 2 recent ones regardless of the share", n)
	}
}

func TestWorkQueueOrder(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 1000, discoveryOnly)
	at := func(id int64) time.Time { return t0.Add(time.Duration(id) * time.Minute) } // higher ID = newer
	lists := map[int64][]int64{owner: {11, 10}, friendA: {21, 20, 11}, friendB: {31, 30}}
	var accounts []Account
	for _, id := range []int64{owner, friendA, friendB} {
		e.add(t, strconv.FormatInt(id, 10))
		var ms []opendota.PlayerMatch
		for i, m := range lists[id] {
			slot := i
			ms = append(ms, opendota.PlayerMatch{MatchID: m, PlayerSlot: &slot, StartTime: at(m).Unix()})
		}
		if _, _, err := storeDiscovered(ctx, e.store.Write, id, ms, t0); err != nil {
			t.Fatal(err)
		}
	}
	accounts, err := activeAccounts(ctx, e.store.Read, 0)
	if err != nil {
		t.Fatal(err)
	}
	queue, err := workQueue(ctx, e.store.Read, accounts, "m.details_at IS NULL")
	if err != nil {
		t.Fatal(err)
	}
	var got []int64
	for _, q := range queue {
		got = append(got, q.matchID)
	}
	// Owner first; the shared 11 only once; then friends take turns.
	if want := []int64{11, 10, 21, 31, 20, 30}; !slices.Equal(got, want) {
		t.Errorf("queue = %v, want %v", got, want)
	}
}

func TestLock(t *testing.T) {
	dir := t.TempDir()
	l, err := AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireLock(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock: err = %v, want ErrLocked", err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	l, err = AcquireLock(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	l.Release()
}

func TestScheduler(t *testing.T) {
	e := realEnv(t, 1000)
	e.c.now = time.Now
	cycles := make(chan bool, 10)
	s := NewScheduler(e.c)
	s.onCycle = func(pollOnly bool) { cycles <- pollOnly }

	// A parse job due now wakes the scheduler before its first full cycle.
	if _, err := e.store.Write.Exec(`INSERT INTO matches (match_id, start_time, duration, game_mode, lobby_type) VALUES (?, 0, 0, 0, 0)`, pendingMatch); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Write.Exec(`INSERT INTO account_matches (account_id, match_id) VALUES (?, ?)`, realOwner, pendingMatch); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Write.Exec(`INSERT INTO parse_jobs (match_id, job_id, requested_at, next_check_at, status)
		VALUES (?, ?, 0, 0, 'pending')`, pendingMatch, recordedJobID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()
	wait := func(wantPollOnly bool) {
		t.Helper()
		select {
		case pollOnly := <-cycles:
			if pollOnly != wantPollOnly {
				t.Errorf("cycle pollOnly = %v, want %v", pollOnly, wantPollOnly)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("no cycle")
		}
	}
	wait(true)  // the due parse job
	wait(false) // the first full cycle, after startDelay
	s.Trigger()
	wait(false) // the manual trigger
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler did not stop")
	}
}

func TestParseAccountID(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"337895673", 337895673},
		{" 337895673 ", 337895673},
		{"76561198298161401", 337895673},
		{"https://www.dotabuff.com/players/337895673", 337895673},
		{"dotabuff.com/players/337895673/matches", 337895673},
		{"https://www.opendota.com/players/337895673", 337895673},
		{"https://stratz.com/players/337895673", 337895673},
		{"https://steamcommunity.com/profiles/76561198298161401/", 337895673},
		{"4294967295", 4294967295},
	}
	for _, tt := range tests {
		got, err := ParseAccountID(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("ParseAccountID(%q) = %d, %v; want %d", tt.in, got, err, tt.want)
		}
	}
	for _, in := range []string{
		"", "0", "abc", "4294967296", "76561197960265727",
		"https://steamcommunity.com/id/somename",
		"https://example.com/players/1",
		"https://www.dotabuff.com/heroes/axe",
	} {
		if got, err := ParseAccountID(in); err == nil {
			t.Errorf("ParseAccountID(%q) = %d, want error", in, got)
		}
	}
}

func TestQueueParseForOldMatch(t *testing.T) {
	ctx := context.Background()
	e := realEnv(t, 1000)
	e.run(t, RunOptions{})
	if e.parseState(t, oldMatch) != ParseUnparsed {
		t.Fatalf("old match: %s", e.parseState(t, oldMatch))
	}
	// The button: queue it although it is years old.
	if err := QueueParse(ctx, e.store.Write, oldMatch, e.c.now()); err != nil {
		t.Fatal(err)
	}
	if e.parseState(t, oldMatch) != ParsePending {
		t.Error("queued match not shown as pending")
	}
	*e.now = t0.Add(time.Minute)
	rep := e.run(t, RunOptions{})
	if rep.ParseRequests != 1 || e.api.seen("POST /request/2324742574") != 1 {
		t.Errorf("queued parse not requested: %+v", rep)
	}
	if n := e.count(t, "SELECT count(*) FROM parse_jobs WHERE match_id = ? AND job_id IS NOT NULL AND next_check_at IS NOT NULL", oldMatch); n != 1 {
		t.Error("job not updated with the request")
	}
	// Parsed matches are left alone.
	if err := QueueParse(ctx, e.store.Write, parsedMatch, e.c.now()); err != nil {
		t.Fatal(err)
	}
	if e.parseState(t, parsedMatch) != ParseParsed {
		t.Error("queueing a parsed match changed it")
	}
}

func TestPauseStopsWorkMidRun(t *testing.T) {
	ctx := context.Background()
	e := realEnv(t, 1000)
	// The owner is paused while the run is under way, right after their
	// match list arrives: the details, parse requests and polls that
	// follow must skip them.
	e.api.set(func() {
		e.api.onRequest = func(path string) {
			if path == "/players/337895673/matches" {
				if err := SetAccountActive(ctx, e.store.Write, realOwner, false); err != nil {
					t.Error(err)
				}
			}
		}
	})
	rep := e.run(t, RunOptions{})
	if rep.Accounts[0].NewMatches != 3 {
		t.Fatalf("discovery before the pause: %+v", rep.Accounts[0])
	}
	if rep.Details != 0 || rep.ParseRequests != 0 || e.api.seen("/matches/") != 0 {
		t.Errorf("paused owner's matches were still worked on: %+v", rep)
	}

	// A pending parse of a paused account is neither polled nor wakes the
	// scheduler.
	e.api.set(func() { e.api.onRequest = nil })
	if err := SetAccountActive(ctx, e.store.Write, realOwner, true); err != nil {
		t.Fatal(err)
	}
	e.run(t, RunOptions{})
	if e.parseState(t, pendingMatch) != ParsePending {
		t.Fatal("setup: parse not requested")
	}
	if err := SetAccountActive(ctx, e.store.Write, realOwner, false); err != nil {
		t.Fatal(err)
	}
	*e.now = t0.Add(time.Hour)
	before := e.api.seen("/matches/9056907605")
	e.run(t, RunOptions{PollOnly: true})
	if e.api.seen("/matches/9056907605") != before {
		t.Error("parse of a paused account was polled")
	}
	if _, ok, _ := nextPollAt(ctx, e.store.Read); ok {
		t.Error("a paused account's job would wake the scheduler")
	}
}

func TestInsightsStoredWithVersionAndUpgraded(t *testing.T) {
	e := realEnv(t, 1000)
	e.run(t, RunOptions{})
	if n := e.count(t, "SELECT count(*) FROM match_insights WHERE match_id = ? AND kind = 'lane'", parsedMatch); n != 10 {
		t.Errorf("%d lane insights for the parsed match, want 10", n)
	}
	if n := e.count(t, "SELECT count(*) FROM match_insights WHERE detector_version != ?", analysis.Version); n != 0 {
		t.Errorf("%d insights without the current detector version", n)
	}
	if n := e.count(t, "SELECT count(*) FROM match_events WHERE match_id = ? AND kind = 'death'", parsedMatch); n != 75 {
		t.Errorf("%d death events, want 75", n)
	}

	// A new analysis version re-analyzes stored matches from raw JSON.
	if _, err := e.store.Write.Exec("DELETE FROM match_insights"); err != nil {
		t.Fatal(err)
	}
	if err := e.c.setState(context.Background(), stateAnalysisVersion, "0"); err != nil {
		t.Fatal(err)
	}
	before := len(e.api.requests)
	e.run(t, RunOptions{})
	if n := e.count(t, "SELECT count(*) FROM match_insights WHERE match_id = ?", parsedMatch); n == 0 {
		t.Error("insights not rebuilt after an analysis version change")
	}
	for _, r := range e.api.requests[before:] {
		if strings.Contains(r, "/matches/") {
			t.Errorf("re-analysis fetched %s; it must use stored raw JSON", r)
		}
	}
}
