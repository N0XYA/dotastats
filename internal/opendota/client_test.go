package opendota

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"dotastats/internal/db"
)

const (
	ownerID         = 337895673
	parsedMatchID   = 9017652399
	unparsedMatchID = 9056907605
	fixtureDir      = "../../testdata/opendota"
	testBudgetLimit = 100
)

// fixtures serves the recorded responses by request path.
var fixtures = map[string]string{
	"/players/337895673":         "player_337895673.json",
	"/players/337895673/matches": "player_337895673_matches.json",
	"/matches/9017652399":        "match_9017652399_parsed.json",
	"/matches/9056907605":        "match_9056907605_unparsed.json",
	"/constants/heroes":          "constants_heroes.json",
	"/constants/items":           "constants_items.json",
	"/constants/game_mode":       "constants_game_mode.json",
	"/constants/lobby_type":      "constants_lobby_type.json",
	"/constants/patch":           "constants_patch.json",
}

func fixtureHandler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name, ok := fixtures[r.URL.Path]
		if r.URL.Path == "/players/337895673/matches" && r.URL.Query().Has("project") {
			name = "player_337895673_matches_projected.json"
		}
		if r.URL.Path == "/players/4294967290" {
			w.WriteHeader(http.StatusNotFound)
			name, ok = "player_not_found.json", true
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		b, err := os.ReadFile(filepath.Join(fixtureDir, name))
		if err != nil {
			t.Error(err)
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write(b)
	}
}

type testEnv struct {
	client *Client
	budget *Budget
	calls  *atomic.Int32
	slept  *[]time.Duration
}

// newTestEnv starts h behind httptest and returns a client with a real
// budget on a temp database, no rate limit and a recording sleep.
func newTestEnv(t *testing.T, h http.Handler, o Options) testEnv {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	store, err := db.Open(filepath.Join(t.TempDir(), "dota.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if _, _, err := db.Migrate(context.Background(), store.Write); err != nil {
		t.Fatal(err)
	}

	budget := NewBudget(store.Write, testBudgetLimit)
	o.BaseURL = srv.URL
	o.Budget = budget
	o.Limiter = rate.NewLimiter(rate.Inf, 1)
	c := New(o)
	var slept []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}
	return testEnv{client: c, budget: budget, calls: &calls, slept: &slept}
}

func (e testEnv) used(t *testing.T) int {
	t.Helper()
	n, err := e.budget.Used(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDecodeFixtures(t *testing.T) {
	ctx := context.Background()
	c := newTestEnv(t, fixtureHandler(t), Options{}).client

	p, err := c.Player(ctx, ownerID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Profile == nil || p.Profile.AccountID != ownerID || p.Profile.PersonaName == "" {
		t.Errorf("profile = %+v, want account %d with a persona name", p.Profile, ownerID)
	}
	if p.RankTier == nil || *p.RankTier != 51 {
		t.Errorf("rank_tier = %v, want 51", p.RankTier)
	}

	ms, err := c.PlayerMatches(ctx, ownerID, url.Values{"significant": {"0"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 21 || ms[0].MatchID != unparsedMatchID || ms[0].Version != nil {
		t.Errorf("got %d matches, first %+v; want 21 starting with unparsed %d", len(ms), ms[0], unparsedMatchID)
	}
	if last := ms[len(ms)-1]; last.MatchID != parsedMatchID || last.Version == nil || *last.Version != 22 {
		t.Errorf("last match = %+v, want parsed %d with version 22", last, parsedMatchID)
	}
	if ms[0].PlayerSlot == nil || ms[0].RadiantWin == nil || ms[0].PartySize == nil || *ms[0].PartySize != 3 {
		t.Errorf("basic fields missing from %+v", ms[0])
	}

	parsed, raw, err := c.Match(ctx, parsedMatchID)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.Parsed() || len(parsed.Players) != 10 || len(raw) == 0 {
		t.Fatalf("parsed match: parsed=%v players=%d raw=%dB", parsed.Parsed(), len(parsed.Players), len(raw))
	}
	// Values checked by hand against the recorded JSON.
	me := findPlayer(t, parsed, ownerID)
	want := MatchPlayer{PlayerSlot: 130, HeroID: 48, Kills: 16, Deaths: 6, Assists: 6, GoldPerMin: 778, XPPerMin: 1187, Item0: 172, ItemNeutral: 2190}
	if me.PlayerSlot != want.PlayerSlot || me.HeroID != want.HeroID || me.Kills != want.Kills || me.Deaths != want.Deaths ||
		me.Assists != want.Assists || me.GoldPerMin != want.GoldPerMin || me.XPPerMin != want.XPPerMin ||
		me.Item0 != want.Item0 || me.ItemNeutral != want.ItemNeutral {
		t.Errorf("owner row = %+v", me)
	}
	if me.NetWorth == nil || *me.NetWorth != 29794 || me.ItemNeutral2 == nil || *me.ItemNeutral2 != 1584 || me.HeroDamage == nil {
		t.Errorf("net worth %v, neutral enhancement %v, hero damage %v; want 29794, 1584, set", me.NetWorth, me.ItemNeutral2, me.HeroDamage)
	}
	if len(me.GoldT) == 0 || len(me.PurchaseLog) != 40 || len(me.ObsLog) == 0 || me.Benchmarks["gold_per_min"].Pct == nil {
		t.Errorf("parsed logs missing: gold_t %dB, purchases %d, obs %d", len(me.GoldT), len(me.PurchaseLog), len(me.ObsLog))
	}
	if me.Lane == nil || *me.Lane != 3 || me.LaneRole == nil || *me.LaneRole != 1 || me.IsRoaming == nil {
		t.Errorf("parsed lane fields = %v %v %v, want 3 1 false", me.Lane, me.LaneRole, me.IsRoaming)
	}

	unparsed, _, err := c.Match(ctx, unparsedMatchID)
	if err != nil {
		t.Fatal(err)
	}
	if unparsed.Parsed() {
		t.Error("unparsed match reports Parsed() = true")
	}
	if me := findPlayer(t, unparsed, ownerID); me.Lane != nil || me.LaneRole != nil || me.IsRoaming != nil {
		t.Errorf("unparsed match has lane fields: %+v", me)
	}
	nullRanks := 0
	for _, pl := range unparsed.Players {
		if pl.RankTier == nil {
			nullRanks++
		}
	}
	if nullRanks != 3 {
		t.Errorf("%d players with null rank_tier, want 3", nullRanks)
	}
}

func findPlayer(t *testing.T, m Match, accountID int64) MatchPlayer {
	t.Helper()
	for _, p := range m.Players {
		if p.AccountID != nil && *p.AccountID == accountID {
			return p
		}
	}
	t.Fatalf("account %d not in match %d", accountID, m.MatchID)
	return MatchPlayer{}
}

func TestSendsAPIKeyAndUserAgent(t *testing.T) {
	var gotKey, gotUA, gotLimit string
	env := newTestEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotUA, gotLimit = r.URL.Query().Get("api_key"), r.UserAgent(), r.URL.Query().Get("limit")
		w.Write([]byte("[]"))
	}), Options{APIKey: "secret", UserAgent: "dotastats/v-test"})

	query := url.Values{"limit": {"5"}}
	if _, err := env.client.PlayerMatches(context.Background(), ownerID, query); err != nil {
		t.Fatal(err)
	}
	if gotKey != "secret" || gotUA != "dotastats/v-test" || gotLimit != "5" {
		t.Errorf("api_key=%q User-Agent=%q limit=%q", gotKey, gotUA, gotLimit)
	}
	if query.Has("api_key") {
		t.Error("caller's query was modified")
	}
}

func TestRetry429HonorsRetryAfter(t *testing.T) {
	var n atomic.Int32
	env := newTestEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{}`))
	}), Options{})

	if _, err := env.client.Get(context.Background(), "/x", nil); err != nil {
		t.Fatal(err)
	}
	if got := *env.slept; len(got) != 1 || got[0] != 7*time.Second {
		t.Errorf("slept %v, want [7s]", got)
	}
	if env.calls.Load() != 2 || env.used(t) != 2 {
		t.Errorf("calls=%d budget used=%d, want 2 and 2", env.calls.Load(), env.used(t))
	}
}

func TestLongRetryAfterIsNotSleptThrough(t *testing.T) {
	env := newTestEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}), Options{})

	_, err := env.client.Get(context.Background(), "/x", nil)
	se, ok := errors.AsType[*StatusError](err)
	if !ok || se.StatusCode != 429 || se.RetryAfter != time.Hour {
		t.Fatalf("err = %v, want 429 StatusError with RetryAfter 1h", err)
	}
	if env.calls.Load() != 1 || len(*env.slept) != 0 {
		t.Errorf("calls=%d slept=%v, want one call and no sleep", env.calls.Load(), *env.slept)
	}
}

func TestRetry500ThenSuccess(t *testing.T) {
	var n atomic.Int32
	env := newTestEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) <= 2 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}), Options{})

	b, err := env.client.Get(context.Background(), "/x", nil)
	if err != nil || string(b) != `{"ok":true}` {
		t.Fatalf("got %q, %v", b, err)
	}
	slept := *env.slept
	if len(slept) != 2 {
		t.Fatalf("slept %v, want two backoffs", slept)
	}
	// Attempt k waits in [base*2^(k-1)/2, base*2^(k-1)).
	for i, d := range slept {
		full := baseBackoff << i
		if d < full/2 || d >= full {
			t.Errorf("backoff %d = %v, want in [%v, %v)", i+1, d, full/2, full)
		}
	}
}

func TestRetry500GivesUp(t *testing.T) {
	env := newTestEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}), Options{})

	_, err := env.client.Get(context.Background(), "/x", nil)
	if se, ok := errors.AsType[*StatusError](err); !ok || se.StatusCode != 502 || !strings.Contains(se.Body, "down") {
		t.Fatalf("err = %v, want 502 StatusError", err)
	}
	if env.calls.Load() != maxAttempts {
		t.Errorf("calls = %d, want %d", env.calls.Load(), maxAttempts)
	}
}

func TestNoRetryOn4xx(t *testing.T) {
	env := newTestEnv(t, fixtureHandler(t), Options{})
	_, err := env.client.Get(context.Background(), "/players/1", nil)
	if se, ok := errors.AsType[*StatusError](err); !ok || se.StatusCode != 404 {
		t.Fatalf("err = %v, want 404 StatusError", err)
	}
	if env.calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", env.calls.Load())
	}
}

func TestBudgetExhaustion(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t, fixtureHandler(t), Options{})
	if err := env.budget.Spend(ctx, testBudgetLimit-1); err != nil {
		t.Fatal(err)
	}

	if _, err := env.client.Player(ctx, ownerID); err != nil {
		t.Fatalf("last call within budget: %v", err)
	}
	if _, err := env.client.Player(ctx, ownerID); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted", err)
	}
	if env.calls.Load() != 1 {
		t.Errorf("server saw %d calls, want 1: an exhausted budget must not reach the API", env.calls.Load())
	}
	if env.used(t) != testBudgetLimit {
		t.Errorf("used = %d, want %d", env.used(t), testBudgetLimit)
	}
}

func TestBudgetPersistsAndResetsDaily(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "dota.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, _, err := db.Migrate(ctx, store.Write); err != nil {
		t.Fatal(err)
	}

	day := time.Date(2026, 10, 24, 23, 0, 0, 0, time.UTC)
	b := NewBudget(store.Write, 3)
	b.now = func() time.Time { return day }
	if err := b.Spend(ctx, 2); err != nil {
		t.Fatal(err)
	}

	// A second Budget, as after a restart or from a CLI command, sees it.
	b2 := NewBudget(store.Write, 3)
	b2.now = b.now
	if err := b2.Spend(ctx, 2); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("spend past limit after restart: err = %v", err)
	}
	if err := b2.Spend(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := b2.Spend(ctx, 1); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("spend at limit: err = %v", err)
	}

	day = day.Add(2 * time.Hour) // next UTC day
	if err := b2.Spend(ctx, 3); err != nil {
		t.Fatalf("new day: %v", err)
	}
}

func TestDevCache(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	env := newTestEnv(t, fixtureHandler(t), Options{CacheDir: dir, APIKey: "secret"})

	for range 2 {
		if _, err := env.client.PlayerMatches(ctx, ownerID, url.Values{"significant": {"0"}}); err != nil {
			t.Fatal(err)
		}
	}
	if env.calls.Load() != 1 || env.used(t) != 1 {
		t.Errorf("calls=%d used=%d, want the second request served from cache", env.calls.Load(), env.used(t))
	}
	if _, err := os.Stat(filepath.Join(dir, "players_337895673_matches_significant=0.json")); err != nil {
		t.Errorf("cache file: %v", err)
	}
}

func TestErrorsHideAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // connection refused from now on
	c := New(Options{BaseURL: srv.URL, APIKey: "secret", Limiter: rate.NewLimiter(rate.Inf, 1)})
	c.sleep = func(context.Context, time.Duration) error { return nil }

	_, err := c.Get(context.Background(), "/x", nil)
	if _, ok := errors.AsType[*netError](err); !ok {
		t.Fatalf("err = %v, want netError", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("error leaks the API key: %v", err)
	}
}

func TestCancelStopsRetrying(t *testing.T) {
	env := newTestEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}), Options{})
	ctx, cancel := context.WithCancel(context.Background())
	env.client.sleep = func(ctx context.Context, d time.Duration) error {
		cancel()
		return sleepCtx(ctx, d)
	}
	if _, err := env.client.Get(ctx, "/x", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if env.calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", env.calls.Load())
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	tests := map[string]time.Duration{
		"":                              0,
		"5":                             5 * time.Second,
		"-1":                            0,
		"Sat, 24 Oct 2026 12:00:30 GMT": 30 * time.Second,
		"Sat, 24 Oct 2026 11:00:00 GMT": 0,
		"soon":                          0,
	}
	for in, want := range tests {
		if got := parseRetryAfter(in, now); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestDecodeProjectedMatches(t *testing.T) {
	c := newTestEnv(t, fixtureHandler(t), Options{}).client
	ms, err := c.PlayerMatches(context.Background(), ownerID, url.Values{"project": PlayerMatchFields})
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 3 {
		t.Fatalf("got %d matches, want 3", len(ms))
	}
	// Values checked by hand against the recorded JSON.
	m := ms[2]
	if m.MatchID != 9056752179 || m.HeroID != 1 || m.StartTime != 1792683186 || m.Duration != 2843 ||
		m.Kills != 12 || m.AverageRank == nil || *m.AverageRank != 35 || m.PartySize == nil || *m.PartySize != 3 {
		t.Errorf("default fields: %+v", m)
	}
	if m.GoldPerMin == nil || *m.GoldPerMin != 750 || m.XPPerMin == nil || *m.XPPerMin != 1122 ||
		m.LastHits == nil || *m.LastHits != 524 || m.Item0 == nil || *m.Item0 != 147 ||
		m.ItemNeutral == nil || *m.ItemNeutral != 1605 || m.LeaverStatus == nil {
		t.Errorf("projected fields: %+v", m)
	}
	if m.Lane != nil || m.LaneRole != nil || m.IsRoaming != nil || m.Version != nil {
		t.Errorf("unparsed match has parsed-only fields: %+v", m)
	}
}

func TestDecodeConstants(t *testing.T) {
	ctx := context.Background()
	c := newTestEnv(t, fixtureHandler(t), Options{}).client

	var heroes map[string]Hero
	if err := c.Constants(ctx, "heroes", &heroes); err != nil {
		t.Fatal(err)
	}
	if h := heroes["1"]; h.ID != 1 || h.Name != "npc_dota_hero_antimage" || h.LocalizedName != "Anti-Mage" ||
		!strings.HasPrefix(h.Img, "/apps/") || !strings.HasPrefix(h.Icon, "/apps/") {
		t.Errorf("hero 1 = %+v", h)
	}

	var items map[string]Item
	if err := c.Constants(ctx, "items", &items); err != nil {
		t.Fatal(err)
	}
	if it := items["blink"]; it.ID != 1 || it.DName != "Blink Dagger" || !strings.HasPrefix(it.Img, "/apps/") {
		t.Errorf("blink = %+v", it)
	}
	if it := items["enhancement_alert"]; it.ID != 1584 {
		t.Errorf("enhancement_alert = %+v, want id 1584 (seen as item_neutral2)", it)
	}

	var modes map[string]GameMode
	if err := c.Constants(ctx, "game_mode", &modes); err != nil {
		t.Fatal(err)
	}
	if m := modes["22"]; m.ID != 22 || m.Name != "game_mode_all_draft" {
		t.Errorf("game mode 22 = %+v", m)
	}

	var patches []Patch
	if err := c.Constants(ctx, "patch", &patches); err != nil {
		t.Fatal(err)
	}
	last := patches[len(patches)-1]
	if last.ID != 60 || last.Name != "7.41" || last.Date.Year() != 2026 {
		t.Errorf("last patch = %+v, want 7.41 (id 60) from 2026", last)
	}
}

func TestPlayerNotFound(t *testing.T) {
	c := newTestEnv(t, fixtureHandler(t), Options{}).client
	_, err := c.Player(context.Background(), 4294967290)
	if !IsNotFound(err) {
		t.Fatalf("err = %v, want a 404", err)
	}
}

func TestBudgetPerAccount(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t, fixtureHandler(t), Options{})
	if _, err := env.client.Player(WithAccount(ctx, ownerID), ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.client.Get(ctx, "/constants/heroes", nil); err != nil {
		t.Fatal(err)
	}
	used, err := env.budget.UsedByAccount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if used[ownerID] != 1 || used[0] != 1 || len(used) != 2 {
		t.Errorf("used by account = %v, want owner 1 and unattributed 1", used)
	}
	// The limit applies to the sum over all accounts.
	if err := env.budget.Spend(WithAccount(ctx, 42), testBudgetLimit-1); !errors.Is(err, ErrBudgetExhausted) {
		t.Errorf("spend past the shared limit: err = %v", err)
	}
}

func TestCacheFileLongName(t *testing.T) {
	short := cacheFile("dir", "/players/1/matches", url.Values{"limit": {"20"}})
	if filepath.Base(short) != "players_1_matches_limit=20.json" {
		t.Errorf("short name = %q", short)
	}
	a := cacheFile("dir", "/players/1/matches", url.Values{"project": PlayerMatchFields})
	b := cacheFile("dir", "/players/2/matches", url.Values{"project": PlayerMatchFields})
	if len(filepath.Base(a)) > maxCacheName+len(".json") || a == b {
		t.Errorf("long names not cut and unique: %q, %q", a, b)
	}
}

func TestDecodeOldMatch(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(fixtureDir, "match_2324742574_old.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m Match
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.Parsed() || m.Patch == nil || *m.Patch != 17 || len(m.Players) != 10 {
		t.Fatalf("old match: parsed=%v patch=%v players=%d", m.Parsed(), m.Patch, len(m.Players))
	}
	for _, p := range m.Players {
		if p.HeroDamage != nil || p.TowerDamage != nil || p.HeroHealing != nil {
			t.Errorf("slot %d: damage fields set in a match that has none", p.PlayerSlot)
		}
	}
}

func TestParseRequests(t *testing.T) {
	ctx := context.Background()
	var queued atomic.Bool
	queued.Store(true)
	env := newTestEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/request/9056907605":
			w.Write(mustRead(t, "request_post_9056907605.json"))
		case r.URL.Path == "/request/602520851" && queued.Load():
			w.Write(mustRead(t, "request_job_queued.json"))
		case r.URL.Path == "/request/602520851":
			w.Write(mustRead(t, "request_job_not_queued.json"))
		default:
			http.NotFound(w, r)
		}
	}), Options{})

	job, err := env.client.RequestParse(ctx, unparsedMatchID)
	if err != nil || job != 602520851 {
		t.Fatalf("RequestParse = %d, %v; want job 602520851", job, err)
	}
	if env.used(t) != parseRequestCost {
		t.Errorf("a parse request spent %d calls, want %d", env.used(t), parseRequestCost)
	}
	if q, err := env.client.ParseJobQueued(ctx, job); err != nil || !q {
		t.Errorf("queued job: %v, %v", q, err)
	}
	queued.Store(false)
	if q, err := env.client.ParseJobQueued(ctx, job); err != nil || q {
		t.Errorf("finished job reported queued: %v, %v", q, err)
	}
}

func mustRead(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRetry429WithoutRetryAfterWaitsForTheMinute(t *testing.T) {
	var n atomic.Int32
	env := newTestEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"minute rate limit exceeded"}`))
			return
		}
		w.Write([]byte(`{}`))
	}), Options{})
	if _, err := env.client.Get(context.Background(), "/x", nil); err != nil {
		t.Fatal(err)
	}
	if got := *env.slept; len(got) != 2 || got[0] != minuteWait || got[1] != 2*minuteWait {
		t.Errorf("slept %v, want [%v %v]", got, minuteWait, 2*minuteWait)
	}
}

func TestPausesWhenMinuteAllowanceIsUsedUp(t *testing.T) {
	var left atomic.Int32
	left.Store(1)
	env := newTestEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Rate-Limit-Remaining-Minute", strconv.Itoa(int(left.Add(-1))))
		w.Write([]byte(`{}`))
	}), Options{})
	ctx := context.Background()
	if _, err := env.client.Get(ctx, "/a", nil); err != nil { // leaves 0
		t.Fatal(err)
	}
	if len(*env.slept) != 0 {
		t.Fatalf("paused before the allowance ran out: %v", *env.slept)
	}
	if _, err := env.client.Get(ctx, "/b", nil); err != nil {
		t.Fatal(err)
	}
	if got := *env.slept; len(got) != 1 || got[0] <= 0 || got[0] > minuteWait {
		t.Errorf("slept %v, want one pause of up to %v before the second request", got, minuteWait)
	}
}
