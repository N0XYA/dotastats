package stats

import (
	"context"
	"database/sql"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"dotastats/internal/testseed"
)

func TestFilterFromQuery(t *testing.T) {
	q := url.Values{
		"from": {"2026-10-01"}, "to": {"2026-10-22"}, "patch": {"0"}, "mode": {"22"},
		"ranked": {"ranked"}, "hero": {"44"}, "role": {"1"}, "result": {"win"}, "party": {"3"},
	}
	f := FilterFromQuery(q, 7)
	if f.AccountID != 7 || f.Patch == nil || *f.Patch != 0 || f.GameMode == nil || *f.GameMode != 22 ||
		f.Ranked != "ranked" || f.HeroID != 44 || f.LaneRole != 1 || f.Result != "win" || f.PartySize != 3 {
		t.Errorf("parsed filter = %+v", f)
	}
	if want := time.Date(2026, 10, 23, 0, 0, 0, 0, time.Local); !f.To.Equal(want) {
		t.Errorf("To = %v, want the day after the last date (%v)", f.To, want)
	}
	if !f.Active() {
		t.Error("filter with values reports inactive")
	}

	bad := FilterFromQuery(url.Values{"from": {"yesterday"}, "patch": {"x"}, "ranked": {"maybe"}, "role": {"9"}, "party": {"6"}, "result": {"draw"}}, 7)
	if bad.Active() {
		t.Errorf("invalid values should be ignored: %+v", bad)
	}
}

func TestRankName(t *testing.T) {
	for tier, want := range map[int]string{0: "Без ранга", 11: "Рекрут 1", 51: "Легенда 1", 75: "Божество 5", 80: "Титан", 60: "Властелин", 95: "Ранг 95"} {
		if got := RankName(&tier); got != want {
			t.Errorf("RankName(%d) = %q, want %q", tier, got, want)
		}
	}
	if RankName(nil) != "Без ранга" {
		t.Error("nil rank")
	}
}

func TestPrettyConstant(t *testing.T) {
	if got := prettyConstant("game_mode_all_draft", "game_mode_"); got != "All Draft" {
		t.Errorf("got %q", got)
	}
	if got := prettyConstant("lobby_type_ranked", "lobby_type_"); got != "Ranked" {
		t.Errorf("got %q", got)
	}
}

// The seeded owner played 3 recorded matches: won 9056907605 (Dire, hero
// 44), lost 9017652399 (Dire, hero 48; Radiant won) and lost the 2016 one.
func TestQueriesOnRecordedMatches(t *testing.T) {
	ctx := context.Background()
	e := testseed.Seed(t)
	db := e.Store.Read
	consts, err := LoadConstants(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	all, err := record(ctx, db, Filter{AccountID: testseed.Owner})
	if err != nil || all.Games != 3 || all.Wins != 1 {
		t.Fatalf("record = %+v, %v; want 1-2", all, err)
	}
	rows, total, err := Matches(ctx, db, Filter{AccountID: testseed.Owner, Result: "win"}, 1, 50)
	if err != nil || total != 1 || len(rows) != 1 || rows[0].MatchID != testseed.PendingMatch {
		t.Errorf("won matches = %d rows (total %d), %v", len(rows), total, err)
	}
	heroes, err := Heroes(ctx, db, Filter{AccountID: testseed.Owner}, "kda")
	if err != nil || len(heroes) != 3 || heroes[0].HeroID != 44 {
		t.Errorf("heroes by KDA = %+v, %v; want Phantom Assassin (21/2/16) first", heroes, err)
	}

	m, err := LoadMatch(ctx, db, testseed.ParsedMatch)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Radiant) != 5 || len(m.Dire) != 5 || len(m.GoldAdv) != 45 || len(m.Series) != 10 || !m.Benchmarks {
		t.Errorf("match: %d/%d players, %d gold points, %d series", len(m.Radiant), len(m.Dire), len(m.GoldAdv), len(m.Series))
	}
	var me PlayerRow
	for _, p := range m.Dire {
		if p.Tracked {
			me = p
		}
	}
	if me.Items[0].ID != 172 || me.Items[0].BoughtAt == nil || len(me.Purchases) != 40 || consts.Item(172).Name != "Mask of Madness" {
		t.Errorf("owner's items: %+v, purchases %d", me.Items, len(me.Purchases))
	}

	d, err := LoadHeroDetail(ctx, db, Filter{AccountID: testseed.Owner, HeroID: 48}, consts)
	if err != nil {
		t.Fatal(err)
	}
	if d.Summary.Record.Games != 1 || d.ParsedGames != 1 || len(d.CoreItems) == 0 || len(d.With) != 4 || len(d.Against) != 5 || len(d.Benchmarks) != 1 {
		t.Errorf("hero detail: %+v games, %d parsed, %d core items, %d with, %d against, %d benchmark points",
			d.Summary.Record, d.ParsedGames, len(d.CoreItems), len(d.With), len(d.Against), len(d.Benchmarks))
	}
}

func TestMatchAnalysis(t *testing.T) {
	ctx := context.Background()
	e := testseed.Seed(t)
	db := e.Store.Read
	consts, err := LoadConstants(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	m, err := LoadMatch(ctx, db, testseed.ParsedMatch)
	if err != nil {
		t.Fatal(err)
	}
	a, err := LoadMatchAnalysis(ctx, db, &m, consts)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Analyzed || !a.Parsed || len(a.Lanes) != 10 || len(a.Vision) != 10 || len(a.Chat) != 22 {
		t.Errorf("analysis: analyzed=%v parsed=%v lanes=%d vision=%d chat=%d", a.Analyzed, a.Parsed, len(a.Lanes), len(a.Vision), len(a.Chat))
	}
	// 75 deaths, 50 of them inside teamfights with positions, 116 wards.
	if len(a.Map.Kills) != 75 || len(a.Map.Deaths) != 50 || len(a.Map.Wards) != 116 || len(a.Map.Fights) != 12 {
		t.Errorf("map: %d kills, %d positioned deaths, %d wards, %d fights", len(a.Map.Kills), len(a.Map.Deaths), len(a.Map.Wards), len(a.Map.Fights))
	}
	// Every positioned death takes the time of a logged death of the same
	// player inside its fight, each logged death at most once.
	type death struct{ slot, t int }
	logged := map[death]int{}
	for _, k := range a.Map.Kills {
		logged[death{k.Victim, k.T}]++
		if (k.Respawn == 0 && k.T < m.Duration-120) || k.Respawn < 0 || k.Team != side(k.Victim) {
			t.Errorf("kill %+v lacks a respawn time or has the wrong team", k)
		}
	}
	for _, d := range a.Map.Deaths {
		f := a.Map.Fights[d.Fight]
		if d.T < f.Start || d.T > f.End || logged[death{d.Slot, d.T}] == 0 {
			t.Errorf("positioned death %+v isn't a logged death inside fight %+v", d, f)
		}
		logged[death{d.Slot, d.T}]--
	}
	for _, b := range a.Map.Buildings {
		if BuildingName(b.Key) != b.Name || !strings.HasPrefix(b.Key, "npc_dota_") {
			t.Errorf("building %+v", b)
		}
	}
	if len(a.Map.Feed) == 0 || len(a.Map.Roshan) != len(a.Map.Aegis) {
		t.Errorf("feed %d items, %d Roshan kills, %d Aegis pickups", len(a.Map.Feed), len(a.Map.Roshan), len(a.Map.Aegis))
	}
	if len(a.WinChance) != 45 || len(a.Markers) != 1 || a.Markers[0].Label != "Резкий сдвиг" {
		t.Errorf("win chance: %d points, markers %v", len(a.WinChance), a.Markers)
	}
	owner := a.Players[130] // Luna, won her lane in the reference
	if owner.Lane != "won" || owner.Rating == "" || len(owner.Good) == 0 {
		t.Errorf("owner's card = %+v", owner)
	}
	judgments, facts := 0, 0
	for _, item := range a.Timeline {
		if item.Text == "" {
			t.Errorf("timeline item without text: %+v", item)
		}
		if item.Judgment {
			judgments++
		} else {
			facts++
		}
	}
	if judgments == 0 || facts == 0 || !slices.IsSortedFunc(a.Timeline, func(x, y TimelineItem) int { return x.Time - y.Time }) {
		t.Errorf("timeline: %d facts, %d findings, sorted=%v", facts, judgments, slices.IsSortedFunc(a.Timeline, func(x, y TimelineItem) int { return x.Time - y.Time }))
	}
}

func TestRecurringPatterns(t *testing.T) {
	ctx := context.Background()
	e := testseed.Seed(t)
	w := e.Store.Write
	// Four more analyzed matches: lane lost in three, an early death in
	// three. The seed already has one (Luna: lane won, first blood victim).
	for i := range 4 {
		id := 7_000_000_000 + int64(i)
		mustExec(t, w, `INSERT INTO matches (match_id, start_time, duration, game_mode, lobby_type, radiant_win, is_parsed)
			VALUES (?, ?, 2400, 22, 7, 1, 1)`, id, 2_000_000_000+i)
		mustExec(t, w, `INSERT INTO match_players (match_id, player_slot, account_id, hero_id, kills, deaths, assists)
			VALUES (?, 0, ?, 44, 5, 5, 5)`, id, testseed.Owner)
		outcome := "lost"
		if i == 3 {
			outcome = "won"
		}
		mustExec(t, w, `INSERT INTO match_insights (match_id, seq, player_slot, kind, severity, evidence, detector_version)
			VALUES (?, 0, 0, 'lane', 'bad', ?, 1), (?, 1, 0, 'time_dead', 'info', '{"seconds":200,"share":0.08}', 1)`,
			id, `{"outcome":"`+outcome+`"}`, id)
		if i < 3 {
			mustExec(t, w, `INSERT INTO match_events (match_id, seq, time, kind, target_slot) VALUES (?, 0, 300, 'death', 0)`, id)
		}
	}
	consts, err := LoadConstants(ctx, e.Store.Read)
	if err != nil {
		t.Fatal(err)
	}
	h, err := LoadHabits(ctx, e.Store.Read, testseed.Owner, consts)
	if err != nil {
		t.Fatal(err)
	}
	if h.Games != 5 {
		t.Errorf("looked at %d matches, want the 5 analyzed ones", h.Games)
	}
	var texts []string
	for _, p := range h.Patterns {
		texts = append(texts, p.Text)
	}
	rows := map[string]Habit{}
	for _, r := range h.Rows {
		rows[r.Label] = r
	}
	if r := rows["Линии к 10:00"]; r.Value != "2 выиграно · 0 ровно · 3 проиграно" {
		t.Errorf("lane habit = %+v", r)
	}
	if r := rows["Смерти до 10:00"]; r.Value != "в 4 из 5 игр" || r.Severity != "bad" {
		t.Errorf("early death habit = %+v", r)
	}
	for _, want := range []string{"Линия проиграна в 3 из 5 игр", "На Phantom Assassin линия проиграна в 3 из 4 игр", "Смерти до 10:00 в 4 из 5 игр"} {
		if !slices.Contains(texts, want) {
			t.Errorf("patterns %q lack %q", texts, want)
		}
	}
}

func mustExec(t *testing.T, db interface {
	Exec(string, ...any) (sql.Result, error)
}, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}
