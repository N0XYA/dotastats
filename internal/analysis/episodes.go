package analysis

import (
	"slices"
	"strings"
)

// deathsWithoutTrade marks each death after which, and shortly before
// which, no enemy hero died: it bought the team nothing. Uses the enemies'
// death logs, which are complete (kills_log misses some kills).
func (mm *match) deathsWithoutTrade() []Insight {
	if !mm.parsed {
		return nil
	}
	var out []Insight
	for _, pl := range mm.players {
		var enemyDeaths []int
		for _, e := range mm.enemies(pl) {
			for _, d := range e.deaths {
				enemyDeaths = append(enemyDeaths, d.Time)
			}
		}
		untraded := 0
		for _, d := range pl.deaths {
			if slices.ContainsFunc(enemyDeaths, func(t int) bool { return abs(t-d.Time) <= TradeWindow }) {
				continue
			}
			untraded++
			out = append(out, Insight{
				Slot: ptr(pl.slot), Kind: KindNoTrade, Time: ptr(d.Time), Severity: Bad,
				Evidence: map[string]any{"killer": d.Key, "killer_slot": mm.slotOf(d.Key)},
			})
		}
		if len(pl.deaths) > 0 {
			sev := Info
			if untraded*2 > len(pl.deaths) && untraded >= 3 {
				sev = Bad
			}
			out = append(out, Insight{
				Slot: ptr(pl.slot), Kind: KindNoTradeTotal, Severity: sev,
				Evidence: map[string]any{"untraded": untraded, "deaths": len(pl.deaths)},
			})
		}
	}
	return out
}

// deathStreaks finds StreakDeaths or more deaths within StreakWindow with
// no kill of the player's own in between.
func (mm *match) deathStreaks() []Insight {
	if !mm.parsed {
		return nil
	}
	var out []Insight
	for _, pl := range mm.players {
		ds := deathTimes(pl)
		for i := 0; i < len(ds); {
			j := i
			for j+1 < len(ds) && ds[j+1]-ds[i] <= StreakWindow && !pl.killedBetween(ds[i], ds[j+1]) {
				j++
			}
			if n := j - i + 1; n >= StreakDeaths {
				out = append(out, Insight{
					Slot: ptr(pl.slot), Kind: KindDeathStreak, Time: ptr(ds[i]), Severity: Bad,
					Evidence: map[string]any{"deaths": n, "until": ds[j]},
				})
				i = j + 1
				continue
			}
			i++
		}
	}
	return out
}

// wastedBuybacks marks buybacks followed within BuybackWindow by another
// death without a kill of the player's own.
func (mm *match) wastedBuybacks() []Insight {
	if !mm.parsed {
		return nil
	}
	var out []Insight
	for _, pl := range mm.players {
		ds := deathTimes(pl)
		for _, b := range pl.p.BuybackLog {
			i := slices.IndexFunc(ds, func(t int) bool { return t > b.Time && t <= b.Time+BuybackWindow })
			if i < 0 || pl.killedWithin(b.Time, ds[i]) {
				continue
			}
			out = append(out, Insight{
				Slot: ptr(pl.slot), Kind: KindWastedBuyback, Time: ptr(b.Time), Severity: Bad,
				Evidence: map[string]any{"died_at": ds[i]},
			})
		}
	}
	return out
}

// deadWhileLost marks long deaths during which the player's team lost
// buildings or the enemy took Roshan.
func (mm *match) deadWhileLost() []Insight {
	if !mm.parsed {
		return nil
	}
	var out []Insight
	for _, pl := range mm.players {
		enemyTeam := "dire"
		if pl.team == "dire" {
			enemyTeam = "radiant"
		}
		for _, d := range pl.deaths {
			if d.TimeDead == nil || *d.TimeDead < LongDeath {
				continue
			}
			from, to := d.Time, d.Time+*d.TimeDead
			var lost []string
			roshan := false
			for _, o := range mm.m.Objectives {
				if o.Time < from || o.Time > to {
					continue
				}
				switch {
				case o.Type == "building_kill" && buildingTeam(o.Key) == pl.team:
					lost = append(lost, o.Key)
				case o.Type == "CHAT_MESSAGE_ROSHAN_KILL" && teamName(o.Team) == enemyTeam:
					roshan = true
				}
			}
			if len(lost) == 0 && !roshan {
				continue
			}
			out = append(out, Insight{
				Slot: ptr(pl.slot), Kind: KindDeadWhileLost, Time: ptr(d.Time), Severity: Bad,
				Evidence: map[string]any{"time_dead": *d.TimeDead, "buildings": lost, "roshan": roshan},
			})
		}
	}
	return out
}

// timeDead totals each player's time dead, from the death log where it
// has durations, else life_state_dead.
func (mm *match) timeDead() []Insight {
	if !mm.parsed {
		return nil
	}
	var out []Insight
	for _, pl := range mm.players {
		total := 0
		for _, d := range pl.deaths {
			if d.TimeDead != nil {
				total += *d.TimeDead
			}
		}
		if pl.p.LifeStateDead != nil && *pl.p.LifeStateDead > total {
			total = *pl.p.LifeStateDead
		}
		if total == 0 {
			continue
		}
		share := float64(total) / float64(max(mm.m.Duration, 1))
		sev := Info
		if share >= 0.15 {
			sev = Bad
		}
		out = append(out, Insight{
			Slot: ptr(pl.slot), Kind: KindTimeDead, Severity: sev,
			Evidence: map[string]any{"seconds": total, "share": share},
		})
	}
	return out
}

// vision records each player's wards placed and destroyed, and names each
// team's top observer placer and dewarder.
func (mm *match) vision() []Insight {
	if !mm.parsed {
		return nil
	}
	var out []Insight
	for _, pl := range mm.players {
		ev := map[string]any{
			"observers": len(pl.p.ObsLog), "sentries": len(pl.p.SenLog),
			"observers_killed": deref(pl.p.ObserverKills), "sentries_killed": deref(pl.p.SentryKills),
		}
		if n := len(pl.p.ObsLog); n > 0 {
			ev["observer_lifetime"] = observerLifetime(pl, mm.m.Duration) / float64(n)
		}
		out = append(out, Insight{Slot: ptr(pl.slot), Kind: KindVision, Severity: Info, Evidence: ev})
	}
	for _, team := range []string{"radiant", "dire"} {
		var wards, dewards *player
		for _, pl := range mm.players {
			if pl.team != team {
				continue
			}
			if wards == nil || len(pl.p.ObsLog) > len(wards.p.ObsLog) {
				wards = pl
			}
			if dewards == nil || dewardCount(pl) > dewardCount(dewards) {
				dewards = pl
			}
		}
		if wards != nil && len(wards.p.ObsLog) > 0 {
			out = append(out, Insight{Slot: ptr(wards.slot), Kind: KindWardLeader, Severity: Good,
				Evidence: map[string]any{"observers": len(wards.p.ObsLog), "sentries": len(wards.p.SenLog), "team": team}})
		}
		if dewards != nil && dewardCount(dewards) > 0 {
			out = append(out, Insight{Slot: ptr(dewards.slot), Kind: KindDewarder, Severity: Good,
				Evidence: map[string]any{"wards": dewardCount(dewards), "team": team}})
		}
	}
	return out
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func dewardCount(pl *player) int {
	n := 0
	if pl.p.ObserverKills != nil {
		n += *pl.p.ObserverKills
	}
	if pl.p.SentryKills != nil {
		n += *pl.p.SentryKills
	}
	return n
}

// xpLead reports the team experience lead at LaneMinute.
func (mm *match) xpLead() []Insight {
	xp := series(mm.m.RadiantXPAdv)
	if !mm.parsed || len(xp) <= LaneMinute {
		return nil
	}
	lead := xp[LaneMinute]
	team := "radiant"
	if lead < 0 {
		team, lead = "dire", -lead
	}
	return []Insight{{Kind: KindXPLead10, Time: ptr(LaneMinute * 60), Severity: Info,
		Evidence: map[string]any{"team": team, "lead": lead}}}
}

func (mm *match) firstBlood() []Insight {
	for _, o := range mm.m.Objectives {
		if o.Type == "CHAT_MESSAGE_FIRSTBLOOD" && o.PlayerSlot != nil {
			ev := map[string]any{}
			if o.VictimSlot != nil {
				ev["victim"] = *o.VictimSlot
			}
			return []Insight{{Slot: o.PlayerSlot, Kind: KindFirstBlood, Time: ptr(o.Time), Severity: Info, Evidence: ev}}
		}
	}
	return nil
}

// slotOf returns the player slot of a hero unit name, or -1.
func (mm *match) slotOf(unit string) int {
	if pl, ok := mm.byHero[unit]; ok {
		return pl.slot
	}
	return -1
}

func deathTimes(pl *player) []int {
	out := make([]int, len(pl.deaths))
	for i, d := range pl.deaths {
		out[i] = d.Time
	}
	slices.Sort(out)
	return out
}

// killedBetween reports a kill strictly between two moments.
func (pl *player) killedBetween(from, to int) bool {
	return slices.ContainsFunc(pl.kills, func(t int) bool { return t > from && t < to })
}

// killedWithin reports a kill in [from, to].
func (pl *player) killedWithin(from, to int) bool {
	return slices.ContainsFunc(pl.kills, func(t int) bool { return t >= from && t <= to })
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// isHero reports whether a unit name is a hero.
func isHero(unit string) bool { return strings.HasPrefix(unit, "npc_dota_hero_") }
