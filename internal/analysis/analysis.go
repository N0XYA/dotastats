// Package analysis turns a match into timestamped facts (events) and
// rule-based findings (insights), in the spirit of replay-analysis sites.
// It runs at extraction time; the results are stored, so they survive
// raw-JSON pruning and are rebuilt by reextract. Insights hold numbers,
// never prose: pages render text from templates.
package analysis

import (
	"encoding/json"
	"strings"

	"dotastats/internal/opendota"
)

// Version is stored with every insight. Bump it when a detector or a
// threshold changes; the collector then re-analyzes stored matches.
const Version = 1

// Severities.
const (
	Good = "good"
	Bad  = "bad"
	Info = "info"
)

// Insight kinds. Judgments (heuristics) are marked in judgments below and
// labeled as such in the UI; the rest are facts.
const (
	KindLane          = "lane"            // lane outcome against the lane opponent at 10:00
	KindTurningPoint  = "turning_point"   // first lasting 25% net worth lead
	KindWinSwing      = "win_swing"       // biggest change in the approximate win chance
	KindNoTrade       = "death_no_trade"  // a death the team didn't trade back
	KindNoTradeTotal  = "deaths_no_trade" // a player's untraded deaths out of all
	KindDeathStreak   = "death_streak"    // several deaths close together, no kill between
	KindWastedBuyback = "wasted_buyback"  // bought back and died again without a kill
	KindDeadWhileLost = "dead_while_lost" // buildings or Roshan lost while dead
	KindTimeDead      = "time_dead"       // total time dead
	KindAward         = "award"           // best in the match at something
	KindVision        = "vision"          // a player's wards placed and destroyed
	KindWardLeader    = "ward_leader"     // most observer wards placed on a team
	KindDewarder      = "dewarder"        // most enemy wards destroyed on a team
	KindXPLead10      = "xp_lead_10"      // team experience lead at 10:00
	KindFirstBlood    = "first_blood"
	KindRating        = "rating" // approximate overall impact
)

var judgments = map[string]bool{
	KindLane: true, KindWinSwing: true, KindNoTrade: true, KindNoTradeTotal: true,
	KindDeathStreak: true, KindWastedBuyback: true, KindDeadWhileLost: true, KindRating: true,
}

// Judgment reports whether insights of kind are heuristic opinions rather
// than plain facts.
func Judgment(kind string) bool { return judgments[kind] }

// Insight is one finding about a match or one player in it.
type Insight struct {
	Slot     *int // nil for the whole match or a team
	Kind     string
	Time     *int // seconds from the horn, if the finding has a moment
	Severity string
	Evidence map[string]any
}

// Event kinds: facts for the timeline and map.
const (
	EventDeath     = "death"     // Target died; Slot killed them if a hero did; Value seconds dead
	EventBuilding  = "building"  // Key destroyed; Slot the killer if a hero
	EventRoshan    = "roshan"    // Data team
	EventAegis     = "aegis"     // Slot picked it up
	EventCourier   = "courier"   // Data team lost a courier
	EventBuyback   = "buyback"   // Slot bought back
	EventTeamfight = "teamfight" // Time to Value; Data deaths and players
	EventChat      = "chat"      // Slot said Key; Data wheel for chat wheel IDs
)

// Event is a fact at a moment of the match.
type Event struct {
	Time   int
	Kind   string
	Slot   *int
	Target *int
	Key    string
	Value  *int
	Data   map[string]any
}

// Heroes maps internal hero names (npc_dota_hero_luna) to hero IDs, from
// the constants.
type Heroes map[string]int

// Result is everything Analyze finds.
type Result struct {
	Events   []Event
	Insights []Insight
}

// player gathers what detectors need about one player.
type player struct {
	slot   int
	team   string
	p      *opendota.MatchPlayer
	nw, lh []float64 // per minute, parsed only
	dn, xp []float64
	deaths []opendota.Death
	kills  []int // times of the player's kills
}

type match struct {
	m       opendota.Match
	players []*player
	bySlot  map[int]*player
	byHero  map[string]*player // internal hero name -> player
	parsed  bool
}

// Analyze runs every detector. Detectors check what data they need, so an
// unparsed match still gets awards and ratings from its totals.
func Analyze(m opendota.Match, heroes Heroes) Result {
	mm := &match{m: m, bySlot: map[int]*player{}, byHero: map[string]*player{}, parsed: m.Parsed()}
	names := map[int]string{}
	for name, id := range heroes {
		names[id] = name
	}
	for i := range m.Players {
		p := &m.Players[i]
		pl := &player{
			slot: p.PlayerSlot, team: teamOf(p.PlayerSlot), p: p,
			nw: series(p.NetworthT), lh: series(p.LHT), dn: series(p.DNT), xp: series(p.XPT),
			deaths: p.DeathsLog,
		}
		if len(pl.nw) == 0 {
			pl.nw = series(p.GoldT)
		}
		for _, k := range p.KillsLog {
			pl.kills = append(pl.kills, k.Time)
		}
		mm.players = append(mm.players, pl)
		mm.bySlot[pl.slot] = pl
		if name, ok := names[p.HeroID]; ok {
			mm.byHero[name] = pl
		}
	}

	var r Result
	r.Events = mm.events()
	for _, detect := range []func() []Insight{
		mm.lanes, mm.turningPoint, mm.winSwing, mm.deathsWithoutTrade, mm.deathStreaks,
		mm.wastedBuybacks, mm.deadWhileLost, mm.timeDead, mm.awards, mm.vision, mm.xpLead,
		mm.firstBlood,
	} {
		r.Insights = append(r.Insights, detect()...)
	}
	r.Insights = append(r.Insights, mm.ratings(r.Insights)...)
	return r
}

func teamOf(slot int) string {
	if slot < 128 {
		return "radiant"
	}
	return "dire"
}

// teamName maps the objectives' team numbers: 2 Radiant, 3 Dire.
func teamName(team *int) string {
	switch {
	case team == nil:
		return ""
	case *team == 2:
		return "radiant"
	case *team == 3:
		return "dire"
	}
	return ""
}

// buildingTeam is the owner of a building from its unit name.
func buildingTeam(key string) string {
	switch {
	case strings.Contains(key, "goodguys"):
		return "radiant"
	case strings.Contains(key, "badguys"):
		return "dire"
	}
	return ""
}

func series(raw json.RawMessage) []float64 {
	var out []float64
	if len(raw) == 0 || json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

func ptr[T any](v T) *T { return &v }

// winnerTeam is "radiant", "dire" or "".
func (mm *match) winnerTeam() string {
	if mm.m.RadiantWin == nil {
		return ""
	}
	if *mm.m.RadiantWin {
		return "radiant"
	}
	return "dire"
}

func (mm *match) enemies(pl *player) []*player {
	var out []*player
	for _, q := range mm.players {
		if q.team != pl.team {
			out = append(out, q)
		}
	}
	return out
}
