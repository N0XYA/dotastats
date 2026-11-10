package stats

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"dotastats/internal/analysis"
)

// MatchAnalysis is what the match page's analysis tabs show. Text is
// rendered here from stored numbers; nothing stored is prose.
type MatchAnalysis struct {
	Analyzed  bool // the match has insights
	Parsed    bool // and a parsed replay behind them
	Team      []InsightView
	Players   map[int]*PlayerAnalysis
	Timeline  []TimelineItem
	Lanes     []LaneRow
	Vision    []VisionRow
	Chat      []ChatLine
	Map       MapData
	WinChance []float64     // Radiant's approximate chance per minute
	Markers   []ChartMarker // minutes to mark on the win chance chart
}

// InsightView is an insight ready to show.
type InsightView struct {
	Slot     *int
	Kind     string
	Time     *int
	Severity string
	Judgment bool // a heuristic; the UI labels it
	Text     string
}

// TimelineItem is one line of the match timeline, a fact or a finding.
type TimelineItem struct {
	Time     int
	Slots    []int // players the line is about, for filtering
	Kind     string
	Severity string
	Judgment bool
	Text     string
	Team     string // "radiant" or "dire" when the line belongs to a side
}

// PlayerAnalysis is one player's card.
type PlayerAnalysis struct {
	Slot    int
	Rating  string // standout, solid, average, struggled; "" without data
	Score   int
	Lane    string // won, even, lost
	Awards  []string
	Good    []string
	Bad     []string
	Improve string
}

// LaneRow compares a player with their lane opponent at 10:00.
type LaneRow struct {
	Slot, Opponent int
	Lane           int // 1 bottom, 2 middle, 3 top
	Outcome        string
	NW, OppNW      float64
	LH, OppLH      float64
	DN, OppDN      float64
	XP, OppXP      float64
	NWDiff, XPDiff float64
	LHDiff         float64
}

// VisionRow is a player's wards.
type VisionRow struct {
	Slot             int
	Observers        int
	Sentries         int
	ObserversKilled  int
	SentriesKilled   int
	ObserverLifetime float64 // average seconds an observer stood
}

// ChatLine is one chat message.
type ChatLine struct {
	Time  int
	Slot  *int
	Text  string
	Wheel bool
}

// ChartMarker labels a minute on a chart.
type ChartMarker struct {
	Minute int    `json:"minute"`
	Label  string `json:"label"`
}

// MapData feeds the match map and its playback. Coordinates are map cells,
// 64..192 on both axes with Radiant's base at the low corner.
type MapData struct {
	Duration  int           `json:"duration"`
	Kills     []MapKill     `json:"kills"` // every death
	Deaths    []MapDeath    `json:"deaths"`
	Wards     []MapWard     `json:"wards"`
	Buildings []MapBuilding `json:"buildings"`
	Fights    []MapFight    `json:"fights"`
	Roshan    []MapEvent    `json:"roshan"`   // Team killed Roshan
	Aegis     []MapEvent    `json:"aegis"`    // Slot picked it up
	Buybacks  []MapEvent    `json:"buybacks"` // Slot bought back
	Feed      []FeedItem    `json:"feed"`     // the timeline's facts
}

// MapKill is any death.
type MapKill struct {
	T       int    `json:"t"`
	Team    string `json:"team"` // the victim's
	Victim  int    `json:"victim"`
	Killer  *int   `json:"killer"`  // a hero's slot; nil for creeps, towers and the like
	Respawn int    `json:"respawn"` // seconds dead; 0 if not logged, as for a death right before the end
}

// MapDeath is a death in a teamfight; only those have positions. T is the
// death's own time when it matches a logged death, else the fight's start.
type MapDeath struct {
	T     int    `json:"t"`
	X     int    `json:"x"`
	Y     int    `json:"y"`
	Slot  int    `json:"slot"`
	Hero  string `json:"hero"`
	Fight int    `json:"fight"` // index into Fights
}

// MapEvent is a moment with a team or a player.
type MapEvent struct {
	T    int    `json:"t"`
	Team string `json:"team,omitempty"`
	Slot *int   `json:"slot,omitempty"`
}

// FeedItem is a timeline fact for the map's event feed.
type FeedItem struct {
	T    int    `json:"t"`
	Kind string `json:"kind"`
	Team string `json:"team"`
	Text string `json:"text"`
}

// MapWard is a ward from placement to removal.
type MapWard struct {
	T    int     `json:"t"`
	End  int     `json:"end"`
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
	Type string  `json:"type"`
	Slot int     `json:"slot"`
}

// MapBuilding is a building's destruction.
type MapBuilding struct {
	T    int    `json:"t"`
	Key  string `json:"key"` // OpenDota's unit key, e.g. npc_dota_goodguys_tower1_top
	Name string `json:"name"`
	Team string `json:"team"` // the side that lost it
}

// MapFight is a teamfight's span.
type MapFight struct {
	Start  int `json:"start"`
	End    int `json:"end"`
	Deaths int `json:"deaths"`
}

// Ward lifetimes when a removal isn't logged (game rules, 7.3x).
const (
	observerLifetime = analysis.ObserverLifetime
	sentryLifetime   = 420
)

// LoadMatchAnalysis builds the analysis tabs for a loaded match.
func LoadMatchAnalysis(ctx context.Context, db *sql.DB, d *MatchDetail, c *Constants) (MatchAnalysis, error) {
	a := &analyzer{c: c, d: d, players: map[int]PlayerRow{}}
	for _, p := range append(append([]PlayerRow{}, d.Radiant...), d.Dire...) {
		a.players[p.Slot] = p
	}
	out := MatchAnalysis{Parsed: d.IsParsed, Players: map[int]*PlayerAnalysis{}, Map: MapData{Duration: d.Duration}}
	for slot := range a.players {
		out.Players[slot] = &PlayerAnalysis{Slot: slot}
	}

	type stored struct {
		slot     *int
		kind     string
		time     *int
		severity string
		ev       map[string]any
	}
	var insights []stored
	err := each(ctx, db, `SELECT player_slot, kind, time, severity, evidence FROM match_insights WHERE match_id = ? ORDER BY seq`,
		func(r *sql.Rows) error {
			var s stored
			var slot, t sql.NullInt64
			var ev string
			if err := r.Scan(&slot, &s.kind, &t, &s.severity, &ev); err != nil {
				return err
			}
			s.slot, s.time = nullInt(slot), nullInt(t)
			if err := json.Unmarshal([]byte(ev), &s.ev); err != nil {
				return err
			}
			insights = append(insights, s)
			return nil
		}, d.MatchID)
	if err != nil {
		return out, err
	}
	out.Analyzed = len(insights) > 0

	for _, in := range insights {
		view := InsightView{Slot: in.slot, Kind: in.kind, Time: in.time, Severity: in.severity,
			Judgment: analysis.Judgment(in.kind), Text: a.describe(in.kind, in.slot, in.ev)}
		if in.slot == nil {
			out.Team = append(out.Team, view)
		}
		if in.time != nil && timelineKinds[in.kind] {
			item := TimelineItem{Time: *in.time, Kind: in.kind, Severity: in.severity, Judgment: view.Judgment, Text: view.Text}
			if in.slot != nil {
				item.Slots, item.Team = []int{*in.slot}, side(*in.slot)
			} else if team, ok := in.ev["team"].(string); ok {
				item.Team = team
			}
			out.Timeline = append(out.Timeline, item)
		}
		switch in.kind {
		case analysis.KindLane:
			row := LaneRow{Slot: *in.slot, Opponent: int(num(in.ev, "opponent")), Lane: int(num(in.ev, "lane")),
				Outcome: str(in.ev, "outcome"), NW: num(in.ev, "nw"), OppNW: num(in.ev, "opp_nw"),
				LH: num(in.ev, "lh"), OppLH: num(in.ev, "opp_lh"), DN: num(in.ev, "dn"), OppDN: num(in.ev, "opp_dn"),
				XP: num(in.ev, "xp"), OppXP: num(in.ev, "opp_xp")}
			row.NWDiff, row.XPDiff, row.LHDiff = row.NW-row.OppNW, row.XP-row.OppXP, row.LH-row.OppLH
			out.Lanes = append(out.Lanes, row)
		case analysis.KindVision:
			out.Vision = append(out.Vision, VisionRow{Slot: *in.slot, Observers: int(num(in.ev, "observers")),
				Sentries: int(num(in.ev, "sentries")), ObserversKilled: int(num(in.ev, "observers_killed")),
				SentriesKilled: int(num(in.ev, "sentries_killed")), ObserverLifetime: num(in.ev, "observer_lifetime")})
		case analysis.KindTurningPoint:
			out.Markers = append(out.Markers, ChartMarker{Minute: *in.time / 60, Label: "Перелом"})
		case analysis.KindWinSwing:
			out.Markers = append(out.Markers, ChartMarker{Minute: *in.time / 60, Label: "Резкий сдвиг"})
		}
	}
	a.untraded = map[deathKey]bool{}
	for _, in := range insights {
		if in.slot != nil {
			a.card(out.Players[*in.slot], in.kind, in.severity, in.ev)
		}
		switch {
		case in.kind == analysis.KindNoTrade && in.slot != nil && in.time != nil:
			a.untraded[deathKey{*in.slot, *in.time}] = true
		case in.kind == analysis.KindFirstBlood && in.time != nil:
			if v, ok := in.ev["victim"]; ok {
				a.firstBlood = &deathKey{int(toFloat(v)), *in.time}
			}
		}
	}
	for slot, pa := range out.Players {
		a.finishCard(pa, a.players[slot])
	}
	slices.SortFunc(out.Lanes, func(x, y LaneRow) int { return cmp.Or(cmp.Compare(x.Lane, y.Lane), cmp.Compare(x.Slot, y.Slot)) })
	slices.SortFunc(out.Vision, func(x, y VisionRow) int { return cmp.Compare(x.Slot, y.Slot) })

	if err := a.loadEvents(ctx, db, &out); err != nil {
		return out, err
	}
	if err := a.loadWards(ctx, db, &out); err != nil {
		return out, err
	}
	placeDeaths(&out.Map)
	slices.SortStableFunc(out.Timeline, func(x, y TimelineItem) int { return cmp.Compare(x.Time, y.Time) })
	for _, it := range out.Timeline {
		if feedKinds[it.Kind] {
			out.Map.Feed = append(out.Map.Feed, FeedItem{T: it.Time, Kind: it.Kind, Team: it.Team, Text: it.Text})
		}
	}
	if len(d.GoldAdv) > 1 {
		out.WinChance = analysis.WinChance(d.GoldAdv, d.XPAdv)
	}
	return out, nil
}

// timelineKinds are the insights with a moment that belong on the timeline.
// Untraded deaths and first blood are marked on the death lines instead;
// lane outcomes have their own tab.
var timelineKinds = map[string]bool{
	analysis.KindTurningPoint: true, analysis.KindWinSwing: true, analysis.KindDeathStreak: true,
	analysis.KindWastedBuyback: true, analysis.KindDeadWhileLost: true, analysis.KindXPLead10: true,
}

// feedKinds are the timeline facts the map's event feed shows.
var feedKinds = map[string]bool{
	analysis.EventDeath: true, analysis.EventBuilding: true, analysis.EventRoshan: true, analysis.EventAegis: true,
	analysis.EventCourier: true, analysis.EventBuyback: true, analysis.EventTeamfight: true,
}

// placeDeaths gives each positioned teamfight death the time of the
// victim's first logged death inside the fight that isn't taken yet.
func placeDeaths(m *MapData) {
	taken := make([]bool, len(m.Kills))
	for i := range m.Deaths {
		d := &m.Deaths[i]
		f := m.Fights[d.Fight]
		for j, k := range m.Kills {
			if !taken[j] && k.Victim == d.Slot && k.T >= f.Start && k.T <= f.End {
				taken[j], d.T = true, k.T
				break
			}
		}
	}
}

// deathKey identifies a death by victim and time.
type deathKey struct{ slot, time int }

type analyzer struct {
	c          *Constants
	d          *MatchDetail
	players    map[int]PlayerRow
	untraded   map[deathKey]bool
	firstBlood *deathKey
}

// hero names the hero in a slot.
func (a *analyzer) hero(slot int) string {
	if p, ok := a.players[slot]; ok {
		return a.c.HeroName(p.HeroID)
	}
	return "кто-то"
}

func (a *analyzer) loadEvents(ctx context.Context, db *sql.DB, out *MatchAnalysis) error {
	return each(ctx, db, `SELECT time, kind, player_slot, target_slot, coalesce(key, ''), value, coalesce(data, '{}')
		FROM match_events WHERE match_id = ? ORDER BY seq`, func(r *sql.Rows) error {
		var t int
		var kind, key, data string
		var slot, target, value sql.NullInt64
		if err := r.Scan(&t, &kind, &slot, &target, &key, &value, &data); err != nil {
			return err
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return err
		}
		item := TimelineItem{Time: t, Kind: kind, Severity: analysis.Info}
		if slot.Valid {
			item.Slots, item.Team = append(item.Slots, int(slot.Int64)), side(int(slot.Int64))
		}
		switch kind {
		case analysis.EventDeath:
			victim := int(target.Int64)
			out.Map.Kills = append(out.Map.Kills, MapKill{T: t, Team: side(victim), Victim: victim, Killer: nullInt(slot), Respawn: int(value.Int64)})
			item.Slots, item.Team = append(item.Slots, victim), opposite(side(victim))
			if slot.Valid {
				item.Text = fmt.Sprintf("%s убивает %s", a.hero(int(slot.Int64)), a.hero(victim))
			} else {
				item.Text = fmt.Sprintf("%s погибает, убийца: %s", a.hero(victim), a.c.UnitName(key))
			}
			if value.Valid {
				item.Text += fmt.Sprintf(" (возрождение через %d с)", value.Int64)
			}
			if fb := a.firstBlood; fb != nil && *fb == (deathKey{victim, t}) {
				item.Text = "Первая кровь: " + item.Text
			}
			if a.untraded[deathKey{victim, t}] {
				item.Text += ", без размена"
				item.Judgment, item.Severity = true, analysis.Bad
			}
		case analysis.EventBuilding:
			team := str(ev, "team")
			item.Team = opposite(team)
			killer := "крипы"
			if slot.Valid {
				killer = a.hero(int(slot.Int64))
			}
			item.Text = fmt.Sprintf("Разрушено: %s (%s)", BuildingName(key), killer)
			out.Map.Buildings = append(out.Map.Buildings, MapBuilding{T: t, Key: key, Name: BuildingName(key), Team: team})
		case analysis.EventRoshan:
			item.Team = str(ev, "team")
			item.Text = teamNom(item.Team) + " убивают Рошана"
			out.Map.Roshan = append(out.Map.Roshan, MapEvent{T: t, Team: item.Team})
		case analysis.EventAegis:
			item.Text = a.hero(int(slot.Int64)) + " подбирает Аегис"
			out.Map.Aegis = append(out.Map.Aegis, MapEvent{T: t, Slot: nullInt(slot)})
		case analysis.EventCourier:
			item.Team = opposite(str(ev, "team"))
			item.Text = teamNom(str(ev, "team")) + " теряют курьера"
			if slot.Valid {
				item.Text += ", его убивает " + a.hero(int(slot.Int64))
			}
		case analysis.EventBuyback:
			item.Text = a.hero(int(slot.Int64)) + " выкупается"
			out.Map.Buybacks = append(out.Map.Buybacks, MapEvent{T: t, Slot: nullInt(slot)})
		case analysis.EventTeamfight:
			end := int(value.Int64)
			deaths := int(num(ev, "deaths"))
			out.Map.Fights = append(out.Map.Fights, MapFight{Start: t, End: end, Deaths: deaths})
			gold := map[string]float64{}
			players, _ := ev["players"].([]any)
			for _, raw := range players {
				p, _ := raw.(map[string]any)
				s := int(num(p, "slot"))
				gold[side(s)] += num(p, "gold_delta")
				pos, _ := p["deaths_pos"].([]any)
				for _, rp := range pos {
					xy, _ := rp.([]any)
					if len(xy) == 3 {
						for range int(toFloat(xy[2])) {
							out.Map.Deaths = append(out.Map.Deaths, MapDeath{T: t, X: int(toFloat(xy[0])), Y: int(toFloat(xy[1])),
								Slot: s, Hero: a.hero(s), Fight: len(out.Map.Fights) - 1})
						}
					}
				}
			}
			winner := "radiant"
			if gold["dire"] > gold["radiant"] {
				winner = "dire"
			}
			item.Team = winner
			item.Text = fmt.Sprintf("Драка до %s: %s, %s получили на %s золота больше", clock(end),
				Count(deaths, "смерть", "смерти", "смертей"), teamNom(winner), thousands(int(math.Abs(gold["radiant"]-gold["dire"]))))
		case analysis.EventChat:
			wheel, _ := ev["wheel"].(bool)
			text := key
			if wheel {
				text = a.c.ChatWheelText(key)
			}
			out.Chat = append(out.Chat, ChatLine{Time: t, Slot: nullInt(slot), Text: text, Wheel: wheel})
			return nil
		default:
			return nil
		}
		out.Timeline = append(out.Timeline, item)
		return nil
	}, a.d.MatchID)
}

func (a *analyzer) loadWards(ctx context.Context, db *sql.DB, out *MatchAnalysis) error {
	return each(ctx, db, `SELECT player_slot, type, time, x, y, left_time FROM wards WHERE match_id = ? ORDER BY time`,
		func(r *sql.Rows) error {
			var w MapWard
			var left sql.NullInt64
			if err := r.Scan(&w.Slot, &w.Type, &w.T, &w.X, &w.Y, &left); err != nil {
				return err
			}
			life := observerLifetime
			if w.Type == "sen" {
				life = sentryLifetime
			}
			w.End = min(w.T+life, a.d.Duration)
			if left.Valid {
				w.End = int(left.Int64)
			}
			out.Map.Wards = append(out.Map.Wards, w)
			return nil
		}, a.d.MatchID)
}

// describe renders an insight as a Russian sentence.
func (a *analyzer) describe(kind string, slot *int, ev map[string]any) string {
	switch kind {
	case analysis.KindLane:
		return fmt.Sprintf("Линия против %s: %s, %s нетворса к 10:00, добивания %s/%s против %s/%s",
			a.hero(int(num(ev, "opponent"))), laneWord(str(ev, "outcome")), signedK(num(ev, "nw")-num(ev, "opp_nw")),
			thousands(int(num(ev, "lh"))), thousands(int(num(ev, "dn"))), thousands(int(num(ev, "opp_lh"))), thousands(int(num(ev, "opp_dn"))))
	case analysis.KindTurningPoint:
		end := "и победили"
		if held, _ := ev["held"].(bool); !held {
			end = "но всё равно проиграли"
		}
		return fmt.Sprintf("Перелом: с этой минуты %s вели по нетворсу на %.0f%% (%s золота) 3+ минуты подряд %s",
			teamNom(str(ev, "team")), num(ev, "lead_pct")*100, thousands(int(num(ev, "lead"))), end)
	case analysis.KindWinSwing:
		return fmt.Sprintf("Самый резкий сдвиг в пользу %s: шанс победы Сил Света %.0f%% → %.0f%%",
			teamGen(str(ev, "toward")), num(ev, "from")*100, num(ev, "to")*100)
	case analysis.KindNoTrade:
		return fmt.Sprintf("%s: смерть без размена, убийца: %s", a.hero(*slot), a.c.UnitName(str(ev, "killer")))
	case analysis.KindNoTradeTotal:
		return fmt.Sprintf("%d из %s без размена", int(num(ev, "untraded")), Count(int(num(ev, "deaths")), "смерти", "смертей", "смертей"))
	case analysis.KindDeathStreak:
		return fmt.Sprintf("%s: %s подряд до %s без своих убийств", a.hero(*slot),
			Count(int(num(ev, "deaths")), "смерть", "смерти", "смертей"), clock(int(num(ev, "until"))))
	case analysis.KindWastedBuyback:
		return fmt.Sprintf("%s: выкуп впустую, снова смерть в %s без убийств", a.hero(*slot), clock(int(num(ev, "died_at"))))
	case analysis.KindDeadWhileLost:
		var lost []string
		if b, ok := ev["buildings"].([]any); ok {
			for _, k := range b {
				s, _ := k.(string)
				lost = append(lost, BuildingName(s))
			}
		}
		if r, _ := ev["roshan"].(bool); r {
			lost = append(lost, "Рошан у врага")
		}
		return fmt.Sprintf("%s: %d с в смерти, а команда за это время потеряла: %s", a.hero(*slot), int(num(ev, "time_dead")), strings.Join(lost, "; "))
	case analysis.KindTimeDead:
		return fmt.Sprintf("В смерти %s, это %s игры", clock(int(num(ev, "seconds"))), Percent(num(ev, "share")))
	case analysis.KindAward:
		return awardText(str(ev, "award"), num(ev, "value"))
	case analysis.KindWardLeader:
		return fmt.Sprintf("Больше всех обсерверов в команде: %d (и %d сентри)", int(num(ev, "observers")), int(num(ev, "sentries")))
	case analysis.KindDewarder:
		return fmt.Sprintf("Больше всех уничтожено вражеских вардов: %d", int(num(ev, "wards")))
	case analysis.KindXPLead10:
		return fmt.Sprintf("%s вели по опыту на %s к 10:00", teamNom(str(ev, "team")), thousands(int(num(ev, "lead"))))
	case analysis.KindFirstBlood:
		return fmt.Sprintf("Первая кровь: %s убивает %s", a.hero(*slot), a.hero(int(num(ev, "victim"))))
	case analysis.KindRating:
		return fmt.Sprintf("Влияние: %s (%d из 100, оценка)", strings.ToLower(RatingName(str(ev, "label"))), int(num(ev, "score")))
	case analysis.KindVision:
		return fmt.Sprintf("Варды: %d обсерверов, %d сентри; уничтожено вражеских: %d", int(num(ev, "observers")), int(num(ev, "sentries")),
			int(num(ev, "observers_killed"))+int(num(ev, "sentries_killed")))
	}
	return kind
}

// laneWord is the lane outcome in a sentence.
func laneWord(outcome string) string {
	switch outcome {
	case "won":
		return "выиграна"
	case "lost":
		return "проиграна"
	}
	return "ровно"
}

// LaneName is the lane outcome as a short label.
func LaneName(outcome string) string { return laneWord(outcome) }

// RatingName is the impact label in Russian.
func RatingName(label string) string {
	switch label {
	case "standout":
		return "Отлично"
	case "solid":
		return "Хорошо"
	case "average":
		return "Средне"
	case "struggled":
		return "Слабо"
	}
	return label
}

var awardNames = map[string]string{
	analysis.AwardControl: "Мастер контроля", analysis.AwardHealer: "Лекарь", analysis.AwardEyes: "Глаза команды",
	analysis.AwardTank: "Танк", analysis.AwardFarmer: "Фармер", analysis.AwardDestroyer: "Разрушитель",
}

func awardText(award string, v float64) string {
	name := awardNames[award]
	switch award {
	case analysis.AwardControl:
		return fmt.Sprintf("%s: %.0f с оглушений", name, v)
	case analysis.AwardHealer:
		return fmt.Sprintf("%s: %s лечения", name, thousands(int(v)))
	case analysis.AwardEyes:
		return fmt.Sprintf("%s: обсерверы простояли %s", name, clock(int(v)))
	case analysis.AwardTank:
		return fmt.Sprintf("%s: %s урона от героев", name, thousands(int(v)))
	case analysis.AwardFarmer:
		return fmt.Sprintf("%s: %s", name, Count(int(v), "добивание", "добивания", "добиваний"))
	case analysis.AwardDestroyer:
		return fmt.Sprintf("%s: %s урона по строениям", name, thousands(int(v)))
	}
	return name
}

// metricNames are the benchmarks cards mention, as they read in a
// sentence. Healing is left out: heroes without heals all tie at zero,
// which OpenDota ranks as a high percentile.
var metricNames = map[string]string{
	"gold_per_min": "GPM", "xp_per_min": "XPM", "kills_per_min": "убийства в минуту",
	"last_hits_per_min": "добивания в минуту", "hero_damage_per_min": "урон по героям в минуту",
	"tower_damage": "урон по строениям",
}

func capitalize(s string) string {
	for i, r := range s {
		return strings.ToUpper(string(r)) + s[i+len(string(r)):]
	}
	return s
}

// card adds one insight to a player's card.
func (a *analyzer) card(pa *PlayerAnalysis, kind, severity string, ev map[string]any) {
	switch kind {
	case analysis.KindRating:
		pa.Rating, pa.Score = str(ev, "label"), int(num(ev, "score"))
	case analysis.KindLane:
		pa.Lane = str(ev, "outcome")
		diff := num(ev, "nw") - num(ev, "opp_nw")
		switch pa.Lane {
		case "won":
			pa.Good = append(pa.Good, fmt.Sprintf("Линия выиграна против %s (%s нетворса к 10:00)", a.hero(int(num(ev, "opponent"))), signedK(diff)))
		case "lost":
			pa.Bad = append(pa.Bad, fmt.Sprintf("Линия проиграна против %s (%s нетворса к 10:00)", a.hero(int(num(ev, "opponent"))), signedK(diff)))
		}
	case analysis.KindAward:
		pa.Awards = append(pa.Awards, awardText(str(ev, "award"), num(ev, "value")))
	case analysis.KindNoTradeTotal:
		if severity == analysis.Bad {
			u, d := int(num(ev, "untraded")), int(num(ev, "deaths"))
			pa.Bad = append(pa.Bad, fmt.Sprintf("%d из %s без размена", u, Count(d, "смерти", "смертей", "смертей")))
			pa.Improve = fmt.Sprintf("Меньше смертей без пользы: %d из %d не принесли команде ни одного убийства в ответ.", u, d)
		}
	case analysis.KindTimeDead:
		if severity == analysis.Bad {
			pa.Bad = append(pa.Bad, fmt.Sprintf("%s игры в смерти (%s)", Percent(num(ev, "share")), clock(int(num(ev, "seconds")))))
			if pa.Improve == "" {
				pa.Improve = fmt.Sprintf("Дольше оставаться в живых: %s игры прошло в смерти.", Percent(num(ev, "share")))
			}
		}
	case analysis.KindWastedBuyback:
		pa.Bad = append(pa.Bad, "Выкуп впустую в "+clock(int(num(ev, "died_at"))))
		if pa.Improve == "" {
			pa.Improve = "Берегите выкуп для драк, которые можно выиграть: один был потрачен и потерян меньше чем за минуту."
		}
	case analysis.KindWardLeader:
		pa.Good = append(pa.Good, fmt.Sprintf("Больше всех обсерверов в команде (%d)", int(num(ev, "observers"))))
	case analysis.KindDewarder:
		pa.Good = append(pa.Good, fmt.Sprintf("Больше всех уничтожено вражеских вардов (%d)", int(num(ev, "wards"))))
	}
}

// finishCard adds the best and worst benchmarks and picks something to
// improve when the insights didn't.
func (a *analyzer) finishCard(pa *PlayerAnalysis, p PlayerRow) {
	keys := make([]string, 0, len(p.Benchmarks))
	for k := range p.Benchmarks {
		if metricNames[k] != "" {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(x, y string) int { return cmp.Or(cmp.Compare(p.Benchmarks[y], p.Benchmarks[x]), cmp.Compare(x, y)) })
	hero := a.c.HeroName(p.HeroID)
	if len(keys) > 0 {
		if best := keys[0]; p.Benchmarks[best] >= 0.75 {
			pa.Good = append(pa.Good, fmt.Sprintf("%s лучше, чем у %s игроков на %s", capitalize(metricNames[best]), Percent(p.Benchmarks[best]), hero))
		}
		if worst := keys[len(keys)-1]; p.Benchmarks[worst] <= 0.25 {
			pa.Bad = append(pa.Bad, fmt.Sprintf("%s хуже, чем у %s игроков на %s", capitalize(metricNames[worst]), Percent(1-p.Benchmarks[worst]), hero))
			if pa.Improve == "" && pa.Lane != "lost" {
				pa.Improve = fmt.Sprintf("Подтяните %s: сейчас хуже, чем у %s игроков на этом герое.", metricNames[worst], Percent(1-p.Benchmarks[worst]))
			}
		}
	}
	if pa.Improve == "" && pa.Lane == "lost" {
		pa.Improve = "Линия: к 10:00 вы отставали от соперника. Посмотрите на добивания и размены в первые минуты."
	}
	pa.Good = pa.Good[:min(len(pa.Good), 4)]
	pa.Bad = pa.Bad[:min(len(pa.Bad), 4)]
}

func num(m map[string]any, k string) float64 { return toFloat(m[k]) }

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	}
	return 0
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func side(slot int) string {
	if slot < 128 {
		return "radiant"
	}
	return "dire"
}

func opposite(team string) string {
	switch team {
	case "radiant":
		return "dire"
	case "dire":
		return "radiant"
	}
	return ""
}

// clock formats seconds as m:ss, negative before the horn.
func clock(sec int) string {
	sign := ""
	if sec < 0 {
		sign, sec = "-", -sec
	}
	return fmt.Sprintf("%s%d:%02d", sign, sec/60, sec%60)
}

// thousands groups digits with non-breaking spaces: 29 794.
func thousands(n int) string {
	s := strconv.Itoa(abs(n))
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + nbsp + s[i:]
	}
	if n < 0 {
		return "−" + s
	}
	return s
}

// signedK formats a gold difference: "+1 958" or "−926".
func signedK(v float64) string {
	n := roundInt(v)
	if n > 0 {
		return "+" + thousands(n)
	}
	return thousands(n)
}
