package analysis

import (
	"cmp"
	"slices"
	"strconv"
)

// events lists the match's facts in time order: deaths, buildings,
// Roshan, aegis, couriers, buybacks, teamfights and chat.
func (mm *match) events() []Event {
	var out []Event
	for _, pl := range mm.players {
		for _, d := range pl.deaths {
			e := Event{Time: d.Time, Kind: EventDeath, Target: ptr(pl.slot), Key: d.Key, Value: d.TimeDead,
				Data: map[string]any{"gold_lost": d.GoldLost}}
			if killer, ok := mm.byHero[d.Key]; ok {
				e.Slot = ptr(killer.slot)
			}
			out = append(out, e)
		}
		for _, b := range pl.p.BuybackLog {
			out = append(out, Event{Time: b.Time, Kind: EventBuyback, Slot: ptr(pl.slot)})
		}
	}
	for _, o := range mm.m.Objectives {
		switch o.Type {
		case "building_kill":
			e := Event{Time: o.Time, Kind: EventBuilding, Key: o.Key,
				Data: map[string]any{"team": buildingTeam(o.Key), "killer": o.Unit}}
			if killer, ok := mm.byHero[o.Unit]; ok {
				e.Slot = ptr(killer.slot)
			}
			out = append(out, e)
		case "CHAT_MESSAGE_ROSHAN_KILL":
			out = append(out, Event{Time: o.Time, Kind: EventRoshan, Data: map[string]any{"team": teamName(o.Team)}})
		case "CHAT_MESSAGE_AEGIS":
			out = append(out, Event{Time: o.Time, Kind: EventAegis, Slot: o.PlayerSlot})
		case "CHAT_MESSAGE_COURIER_LOST":
			e := Event{Time: o.Time, Kind: EventCourier, Data: map[string]any{"team": teamName(o.Team)}}
			if o.Killer != nil && mm.bySlot[*o.Killer] != nil {
				e.Slot = o.Killer
			}
			out = append(out, e)
		}
	}
	for _, tf := range mm.m.Teamfights {
		var players []map[string]any
		for i, tp := range tf.Players {
			if i >= len(mm.players) {
				break
			}
			var pos [][3]int
			for x, row := range tp.DeathsPos {
				for y, n := range row {
					xi, err1 := strconv.Atoi(x)
					yi, err2 := strconv.Atoi(y)
					if err1 == nil && err2 == nil {
						pos = append(pos, [3]int{xi, yi, n})
					}
				}
			}
			slices.SortFunc(pos, func(a, b [3]int) int { return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[1], b[1])) })
			players = append(players, map[string]any{
				"slot": mm.players[i].slot, "deaths": tp.Deaths, "buybacks": tp.Buybacks, "damage": tp.Damage,
				"healing": tp.Healing, "gold_delta": tp.GoldDelta, "xp_delta": tp.XPDelta, "deaths_pos": pos,
			})
		}
		out = append(out, Event{Time: tf.Start, Kind: EventTeamfight, Value: ptr(tf.End),
			Data: map[string]any{"deaths": tf.Deaths, "last_death": tf.LastDeath, "players": players}})
	}
	for _, c := range mm.m.Chat {
		e := Event{Time: c.Time, Kind: EventChat, Slot: c.PlayerSlot, Key: c.Key}
		if c.Type == "chatwheel" {
			e.Data = map[string]any{"wheel": true}
		}
		out = append(out, e)
	}
	// Stable, so same-second events keep the order above.
	slices.SortStableFunc(out, func(a, b Event) int { return cmp.Compare(a.Time, b.Time) })
	return out
}
