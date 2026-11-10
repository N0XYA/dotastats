package web

import (
	"context"
	"math/rand/v2"
	"net/http"
	"testing"
	"time"

	"dotastats/internal/testseed"
)

// pageBudget is the roadmap's limit for rendering any page on 5,000
// matches.
const pageBudget = 300 * time.Millisecond

func TestPagesFastOnFiveThousandMatches(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: builds a 5,000-match database")
	}
	e := testseed.Seed(t)
	ctx := context.Background()
	tx, err := e.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewPCG(1, 2))
	heroes := []int{1, 6, 44, 48, 76, 8, 11, 18, 36, 74}
	start := time.Now().AddDate(-3, 0, 0).Unix()
	exec := func(q string, args ...any) {
		if _, err := tx.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 5000 {
		id := int64(8_000_000_000 + i)
		detailed := i%10 < 3 // 1,500 matches with all ten players
		exec(`INSERT INTO matches (match_id, start_time, duration, game_mode, lobby_type, radiant_win, patch, is_parsed, details_at, parse_state)
			VALUES (?, ?, ?, 22, 7, ?, 60, ?, ?, ?)`,
			id, start+int64(i)*18000, 1500+r.IntN(2400), r.IntN(2) == 0, detailed, map[bool]any{true: start, false: nil}[detailed],
			map[bool]any{true: "parsed", false: nil}[detailed])
		exec("INSERT INTO account_matches (account_id, match_id) VALUES (?, ?)", testseed.Owner, id)
		mySlot := []int{0, 1, 2, 3, 4, 128, 129, 130, 131, 132}[r.IntN(10)]
		for _, slot := range []int{0, 1, 2, 3, 4, 128, 129, 130, 131, 132} {
			if slot != mySlot && !detailed {
				continue
			}
			var account any
			if slot == mySlot {
				account = testseed.Owner
			}
			exec(`INSERT INTO match_players (match_id, player_slot, account_id, hero_id, kills, deaths, assists, party_size,
					last_hits, denies, gold_per_min, xp_per_min, net_worth, hero_damage, level, item_0, item_1, lane_role)
				VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, 25, 172, 1, ?)`,
				id, slot, account, heroes[r.IntN(len(heroes))], r.IntN(20), r.IntN(12), r.IntN(25),
				r.IntN(500), r.IntN(30), 300+r.IntN(500), 400+r.IntN(600), 10000+r.IntN(20000), r.IntN(50000), 1+r.IntN(3))
			if detailed && slot == mySlot {
				for seq, item := range []string{"tango", "magic_wand", "power_treads", "mask_of_madness", "black_king_bar", "butterfly"} {
					exec("INSERT INTO item_purchases (match_id, player_slot, seq, time, item) VALUES (?, ?, ?, ?, ?)",
						id, slot, seq, seq*400-90+r.IntN(120), item)
				}
				for _, m := range []string{"gold_per_min", "xp_per_min", "last_hits_per_min", "hero_damage_per_min"} {
					exec("INSERT INTO player_benchmarks (match_id, player_slot, metric, raw, pct) VALUES (?, ?, ?, 1, ?)", id, slot, m, r.Float64())
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	limit := pageBudget
	if raceEnabled {
		limit *= 10
	}
	h := newServer(t, e).Handler()
	for _, path := range []string{
		"/", "/matches", "/matches?page=50", "/matches?hero=44&result=win&ranked=ranked",
		"/heroes", "/heroes?sort=kda", "/heroes/44", "/matches/8000000002", "/settings", "/status",
	} {
		var slowest time.Duration
		for range 3 {
			began := time.Now()
			rec := get(t, h, path)
			slowest = max(slowest, time.Since(began))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s: %d", path, rec.Code)
			}
		}
		t.Logf("%-45s %v", path, slowest.Round(time.Millisecond))
		if slowest > limit {
			t.Errorf("GET %s took %v, over %v", path, slowest, limit)
		}
	}
}
