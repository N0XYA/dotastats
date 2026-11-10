package analysis

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"dotastats/internal/opendota"
)

const fixtureDir = "../../testdata/opendota"

func load(t *testing.T, name string) opendota.Match {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatal(err)
	}
	var m opendota.Match
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func heroes(t *testing.T) Heroes {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureDir, "constants_heroes.json"))
	if err != nil {
		t.Fatal(err)
	}
	var hs map[string]opendota.Hero
	if err := json.Unmarshal(b, &hs); err != nil {
		t.Fatal(err)
	}
	out := Heroes{}
	for _, h := range hs {
		out[h.Name] = h.ID
	}
	return out
}

func of(r Result, kind string) []Insight {
	var out []Insight
	for _, in := range r.Insights {
		if in.Kind == kind {
			out = append(out, in)
		}
	}
	return out
}

func bySlot(ins []Insight) map[int][]Insight {
	out := map[int][]Insight{}
	for _, in := range ins {
		out[*in.Slot] = append(out[*in.Slot], in)
	}
	return out
}

// Expected values below were computed independently from the fixtures
// with a separate reference implementation of the same rules.
type expected struct {
	file        string
	lanes       map[int]string
	opponents   map[int]int
	turning     *[2]any // minute, team
	untraded    map[int][2]int
	streaks     map[int][][2]int // start, deaths
	wasted      [][3]int         // slot, buyback, next death
	deadAndLost int
	awards      map[string]int
	wardLeaders map[string]int
	dewarders   map[string]int
	xpLead      [2]any // team, lead
	firstBlood  [3]int // killer, victim, time
	swingMinute int
	deaths      int
	teamfights  int
}

var cases = []expected{
	{
		file:      "match_9017652399_parsed.json",
		lanes:     map[int]string{0: "won", 1: "lost", 2: "lost", 3: "even", 4: "even", 128: "even", 129: "won", 130: "won", 131: "lost", 132: "even"},
		opponents: map[int]int{0: 131, 1: 130, 2: 129, 3: 132, 4: 128, 128: 4, 129: 2, 130: 1, 131: 0, 132: 3},
		untraded: map[int][2]int{0: {1, 5}, 1: {3, 6}, 2: {4, 7}, 3: {1, 3}, 4: {4, 11},
			128: {9, 11}, 129: {3, 6}, 130: {4, 6}, 131: {4, 9}, 132: {6, 11}},
		streaks:     map[int][][2]int{4: {{1467, 3}}, 128: {{1107, 3}, {2003, 3}, {2382, 3}}},
		wasted:      [][3]int{{128, 2611, 2620}},
		deadAndLost: 14,
		// 692 healing is below MinHealing: no healer award.
		awards:      map[string]int{AwardControl: 4, AwardEyes: 2, AwardTank: 132, AwardFarmer: 130, AwardDestroyer: 3},
		wardLeaders: map[string]int{"radiant": 2, "dire": 128},
		dewarders:   map[string]int{"radiant": 2, "dire": 128},
		xpLead:      [2]any{"dire", 26.0},
		firstBlood:  [3]int{1, 130, 221},
		swingMinute: 19,
		deaths:      75,
		teamfights:  12,
	},
	{
		file:      "match_9056907605_parsed.json",
		lanes:     map[int]string{0: "won", 1: "lost", 2: "lost", 3: "even", 4: "won", 128: "lost", 129: "even", 130: "even", 131: "won", 132: "won"},
		opponents: map[int]int{0: 128, 1: 131, 2: 132, 3: 129, 4: 130, 128: 0, 129: 3, 130: 4, 131: 1, 132: 2},
		turning:   &[2]any{20, "dire"},
		untraded: map[int][2]int{0: {2, 9}, 1: {9, 15}, 2: {4, 10}, 3: {6, 11}, 4: {5, 8},
			128: {2, 8}, 129: {2, 5}, 130: {0, 4}, 131: {1, 2}, 132: {0, 6}},
		streaks:     map[int][][2]int{1: {{289, 3}, {764, 4}, {1150, 4}}, 2: {{522, 3}, {1156, 3}}, 3: {{853, 4}}},
		deadAndLost: 12,
		awards:      map[string]int{AwardControl: 130, AwardHealer: 132, AwardEyes: 132, AwardTank: 1, AwardFarmer: 131, AwardDestroyer: 131},
		wardLeaders: map[string]int{"radiant": 3, "dire": 132},
		dewarders:   map[string]int{"radiant": 3, "dire": 132},
		xpLead:      [2]any{"dire", 982.0},
		firstBlood:  [3]int{132, 1, 107},
		swingMinute: 15,
		deaths:      78,
		teamfights:  9,
	},
}

func TestDetectorsOnRecordedMatches(t *testing.T) {
	hs := heroes(t)
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			r := Analyze(load(t, c.file), hs)

			lanes := bySlot(of(r, KindLane))
			if len(lanes) != len(c.lanes) {
				t.Errorf("lane reports for %d players, want %d", len(lanes), len(c.lanes))
			}
			for slot, want := range c.lanes {
				got := lanes[slot]
				if len(got) != 1 || got[0].Evidence["outcome"] != want || got[0].Evidence["opponent"] != c.opponents[slot] {
					t.Errorf("slot %d lane = %+v, want %s against %d", slot, got, want, c.opponents[slot])
				}
			}

			tp := of(r, KindTurningPoint)
			switch {
			case c.turning == nil && len(tp) != 0:
				t.Errorf("turning point %+v, want none", tp)
			case c.turning != nil && (len(tp) != 1 || *tp[0].Time != c.turning[0].(int)*60 || tp[0].Evidence["team"] != c.turning[1]):
				t.Errorf("turning point %+v, want minute %v for %v", tp, c.turning[0], c.turning[1])
			}

			totals := bySlot(of(r, KindNoTradeTotal))
			for slot, want := range c.untraded {
				got := totals[slot]
				if len(got) != 1 || got[0].Evidence["untraded"] != want[0] || got[0].Evidence["deaths"] != want[1] {
					t.Errorf("slot %d untraded deaths = %+v, want %d of %d", slot, got, want[0], want[1])
				}
				if n := len(bySlot(of(r, KindNoTrade))[slot]); n != want[0] {
					t.Errorf("slot %d: %d untraded death insights, want %d", slot, n, want[0])
				}
			}

			streaks := bySlot(of(r, KindDeathStreak))
			if len(streaks) != len(c.streaks) {
				t.Errorf("streaks for %d players, want %d: %+v", len(streaks), len(c.streaks), streaks)
			}
			for slot, want := range c.streaks {
				var got [][2]int
				for _, in := range streaks[slot] {
					got = append(got, [2]int{*in.Time, in.Evidence["deaths"].(int)})
				}
				if !slices.Equal(got, want) {
					t.Errorf("slot %d streaks = %v, want %v", slot, got, want)
				}
			}

			var wasted [][3]int
			for _, in := range of(r, KindWastedBuyback) {
				wasted = append(wasted, [3]int{*in.Slot, *in.Time, in.Evidence["died_at"].(int)})
			}
			if !slices.Equal(wasted, c.wasted) {
				t.Errorf("wasted buybacks = %v, want %v", wasted, c.wasted)
			}

			if n := len(of(r, KindDeadWhileLost)); n != c.deadAndLost {
				t.Errorf("%d deaths with losses meanwhile, want %d", n, c.deadAndLost)
			}

			awards := map[string]int{}
			for _, in := range of(r, KindAward) {
				awards[in.Evidence["award"].(string)] = *in.Slot
			}
			if len(awards) != len(c.awards) {
				t.Errorf("awards = %v, want %v", awards, c.awards)
			}
			for a, slot := range c.awards {
				if awards[a] != slot {
					t.Errorf("award %s to %d, want %d", a, awards[a], slot)
				}
			}

			for kind, want := range map[string]map[string]int{KindWardLeader: c.wardLeaders, KindDewarder: c.dewarders} {
				got := map[string]int{}
				for _, in := range of(r, kind) {
					got[in.Evidence["team"].(string)] = *in.Slot
				}
				if len(got) != len(want) || got["radiant"] != want["radiant"] || got["dire"] != want["dire"] {
					t.Errorf("%s = %v, want %v", kind, got, want)
				}
			}

			if xp := of(r, KindXPLead10); len(xp) != 1 || xp[0].Evidence["team"] != c.xpLead[0] || xp[0].Evidence["lead"] != c.xpLead[1] {
				t.Errorf("XP lead at 10 = %+v, want %v", xp, c.xpLead)
			}
			if fb := of(r, KindFirstBlood); len(fb) != 1 || *fb[0].Slot != c.firstBlood[0] || fb[0].Evidence["victim"] != c.firstBlood[1] || *fb[0].Time != c.firstBlood[2] {
				t.Errorf("first blood = %+v, want %v", fb, c.firstBlood)
			}
			if sw := of(r, KindWinSwing); len(sw) != 1 || *sw[0].Time != c.swingMinute*60 {
				t.Errorf("win swing = %+v, want minute %d", sw, c.swingMinute)
			}

			count := map[string]int{}
			for _, e := range r.Events {
				count[e.Kind]++
			}
			if count[EventDeath] != c.deaths || count[EventTeamfight] != c.teamfights {
				t.Errorf("events: %v, want %d deaths and %d teamfights", count, c.deaths, c.teamfights)
			}
			if !slices.IsSortedFunc(r.Events, func(a, b Event) int { return a.Time - b.Time }) {
				t.Error("events not in time order")
			}
			if n := len(of(r, KindRating)); n != 10 {
				t.Errorf("%d ratings, want one per player", n)
			}
			if v := bySlot(of(r, KindVision)); len(v) != 10 || v[c.wardLeaders["radiant"]][0].Evidence["observers"] == 0 {
				t.Errorf("vision facts for %d players, want 10", len(v))
			}
		})
	}
}

func TestOwnersStandoutGameIsRatedSo(t *testing.T) {
	// 21/2/16 on Phantom Assassin with 82-96th benchmark percentiles.
	r := Analyze(load(t, "match_9056907605_parsed.json"), heroes(t))
	for _, in := range of(r, KindRating) {
		if *in.Slot == 131 && in.Evidence["label"] != "standout" {
			t.Errorf("owner's rating = %+v, want standout", in.Evidence)
		}
	}
}

func TestUnparsedMatchesGetOnlyTotals(t *testing.T) {
	for _, file := range []string{"match_9056907605_unparsed.json", "match_2324742574_old.json"} {
		r := Analyze(load(t, file), heroes(t))
		for _, in := range r.Insights {
			if in.Kind != KindRating && in.Kind != KindAward {
				t.Errorf("%s: %s insight without a parsed replay", file, in.Kind)
			}
		}
		if len(r.Events) != 0 {
			t.Errorf("%s: %d events without a parsed replay", file, len(r.Events))
		}
		if len(of(r, KindRating)) != 10 {
			t.Errorf("%s: ratings come from benchmarks and should exist", file)
		}
	}
}

func TestWinChance(t *testing.T) {
	ahead := WinChance([]float64{0, 3000}, nil)
	behind := WinChance([]float64{0, -3000}, nil)
	if ahead[0] != 0.5 || ahead[1] <= 0.5 || math.Abs(ahead[1]+behind[1]-1) > 1e-9 {
		t.Errorf("win chance %v / %v: want 0.5 when even and symmetric leads", ahead, behind)
	}
	// The same lead counts for less later in the game.
	if late := WinChance([]float64{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 3000}, nil); late[10] >= ahead[1] {
		t.Errorf("a 3k lead at 10:00 (%v) should count less than at 1:00 (%v)", late[10], ahead[1])
	}
}
