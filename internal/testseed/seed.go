// Package testseed builds test databases from the recorded OpenDota
// fixtures by running the real collector against a fake OpenDota server.
// Only tests import it; nothing here touches the network.
package testseed

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"golang.org/x/time/rate"

	"dotastats/internal/collector"
	"dotastats/internal/db"
	"dotastats/internal/opendota"
)

// Accounts and matches in the fixtures.
const (
	Owner        = 337895673 // MayFeelThis
	Friend       = 171913321 // Zzz, in PendingMatch with the owner
	PendingMatch = 9056907605
	ParsedMatch  = 9017652399
	OldMatch     = 2324742574 // 2016, no damage totals
)

// Env is a seeded database with a collector wired to the fake API.
type Env struct {
	Store     *db.DB
	Collector *collector.Collector
	DataDir   string
	API       *FakeAPI
}

// Empty returns a migrated database with no accounts.
func Empty(t testing.TB) Env {
	t.Helper()
	dataDir := t.TempDir()
	store, err := db.Open(filepath.Join(dataDir, "dota.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	if _, _, err := db.Migrate(ctx, store.Write); err != nil {
		t.Fatal(err)
	}
	// Fetch every match's details now, request no parses, keep raw JSON.
	if err := collector.SaveSettings(ctx, store.Write, collector.Settings{
		DailyBudget: 10000, IntervalMinutes: 15, RawRetentionDays: -1, DetailRetentionDays: -1,
		MatchRetentionDays: -1, RecentDays: 0, BackfillBudgetPct: 100,
	}); err != nil {
		t.Fatal(err)
	}

	api := newFakeAPI(t)
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	budget := opendota.NewBudget(store.Write, 10000)
	client := opendota.New(opendota.Options{BaseURL: srv.URL, Budget: budget, Limiter: rate.NewLimiter(rate.Inf, 1)})
	col := collector.New(store, client, budget, collector.Config{
		AssetsDir: filepath.Join(dataDir, "assets"), IconBaseURL: srv.URL,
	})
	return Env{Store: store, Collector: col, DataDir: dataDir, API: api}
}

// Seed returns a database tracking the owner and one friend, with the
// three recorded matches stored in full.
func Seed(t testing.TB) Env {
	t.Helper()
	e := Empty(t)
	ctx := context.Background()
	for _, id := range []int64{Owner, Friend} {
		if _, err := e.Collector.AddAccount(ctx, strconv.FormatInt(id, 10), ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.Collector.Run(ctx, collector.RunOptions{Full: true}); err != nil {
		t.Fatal(err)
	}
	return e
}

// Dir is the recorded fixtures directory.
func Dir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "opendota")
}

// FakeAPI serves recorded OpenDota responses.
type FakeAPI struct {
	t       testing.TB
	details map[int64]string // match ID -> fixture

	mu       sync.Mutex
	Requests []string
}

func newFakeAPI(t testing.TB) *FakeAPI {
	return &FakeAPI{t: t, details: map[int64]string{
		PendingMatch: "match_9056907605_unparsed.json",
		ParsedMatch:  "match_9017652399_parsed.json",
		OldMatch:     "match_2324742574_old.json",
	}}
}

// pngPixel is a 1x1 PNG served for every icon.
var pngPixel = func() []byte {
	var buf bytes.Buffer
	png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1)))
	return buf.Bytes()
}()

func (f *FakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.Requests = append(f.Requests, r.Method+" "+r.URL.Path)
	f.mu.Unlock()

	segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	id, _ := strconv.ParseInt(segs[min(1, len(segs)-1)], 10, 64)
	switch {
	case segs[0] == "players" && len(segs) == 2 && id == Owner:
		w.Write(f.fixture("player_337895673.json"))
	case segs[0] == "players" && len(segs) == 2 && id == Friend:
		writeJSON(w, map[string]any{"profile": map[string]any{"account_id": Friend, "personaname": "Zzz"}, "rank_tier": 54})
	case segs[0] == "players" && len(segs) == 3:
		writeJSON(w, f.playerMatches(id))
	case segs[0] == "matches" && f.details[id] != "":
		w.Write(f.fixture(f.details[id]))
	case segs[0] == "request" && r.Method == http.MethodPost:
		w.Write(f.fixture("request_post_9056907605.json"))
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
		w.Write(pngPixel)
	default:
		w.WriteHeader(http.StatusNotFound)
		w.Write(f.fixture("player_not_found.json"))
	}
}

// playerMatches builds an account's projected match list from the
// recorded match details it appears in, newest first.
func (f *FakeAPI) playerMatches(accountID int64) []map[string]any {
	out := []map[string]any{}
	for _, id := range []int64{PendingMatch, ParsedMatch, OldMatch} {
		var m opendota.Match
		if err := json.Unmarshal(f.fixture(f.details[id]), &m); err != nil {
			f.t.Fatal(err)
		}
		for _, p := range m.Players {
			if p.AccountID == nil || *p.AccountID != accountID {
				continue
			}
			out = append(out, map[string]any{
				"match_id": m.MatchID, "player_slot": p.PlayerSlot, "radiant_win": m.RadiantWin,
				"hero_id": p.HeroID, "start_time": m.StartTime, "duration": m.Duration,
				"game_mode": m.GameMode, "lobby_type": m.LobbyType, "version": m.Version,
				"kills": p.Kills, "deaths": p.Deaths, "assists": p.Assists, "average_rank": 51,
				"leaver_status": p.LeaverStatus, "party_size": p.PartySize,
				"gold_per_min": p.GoldPerMin, "xp_per_min": p.XPPerMin, "last_hits": p.LastHits,
				"denies": p.Denies, "hero_damage": p.HeroDamage, "tower_damage": p.TowerDamage,
				"hero_healing": p.HeroHealing, "level": p.Level, "item_0": p.Item0, "item_1": p.Item1,
				"item_2": p.Item2, "item_3": p.Item3, "item_4": p.Item4, "item_5": p.Item5,
				"item_neutral": p.ItemNeutral, "lane": p.Lane, "lane_role": p.LaneRole, "is_roaming": p.IsRoaming,
			})
		}
	}
	return out
}

func (f *FakeAPI) fixture(name string) []byte {
	b, err := os.ReadFile(filepath.Join(Dir(), name))
	if err != nil {
		f.t.Error(err)
	}
	return b
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
