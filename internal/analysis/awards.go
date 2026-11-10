package analysis

import "math"

// Award names.
const (
	AwardControl   = "control"   // most stun time dealt
	AwardHealer    = "healer"    // most hero healing
	AwardEyes      = "eyes"      // longest-living observer wards in total
	AwardTank      = "tank"      // most damage taken from heroes
	AwardFarmer    = "farmer"    // most last hits
	AwardDestroyer = "destroyer" // most building damage
)

// awards hands out each award to the best player at it, the lowest slot
// on ties. An award needs a value worth mentioning, and the data: stuns,
// damage taken and ward logs exist in parsed matches only.
func (mm *match) awards() []Insight {
	var out []Insight
	give := func(award string, min float64, value func(*player) (float64, bool)) {
		var best *player
		var bestV float64
		for _, pl := range mm.players {
			v, ok := value(pl)
			if ok && v >= min && (best == nil || v > bestV) {
				best, bestV = pl, v
			}
		}
		if best != nil && bestV > 0 {
			out = append(out, Insight{Slot: ptr(best.slot), Kind: KindAward, Severity: Good,
				Evidence: map[string]any{"award": award, "value": math.Round(bestV*10) / 10}})
		}
	}
	give(AwardControl, MinControlStuns, func(pl *player) (float64, bool) {
		if pl.p.Stuns == nil {
			return 0, false
		}
		return *pl.p.Stuns, true
	})
	give(AwardHealer, MinHealing, func(pl *player) (float64, bool) {
		if pl.p.HeroHealing == nil {
			return 0, false
		}
		return float64(*pl.p.HeroHealing), true
	})
	if mm.parsed {
		give(AwardEyes, 1, func(pl *player) (float64, bool) { return observerLifetime(pl, mm.m.Duration), true })
		give(AwardTank, 1, func(pl *player) (float64, bool) {
			total := 0.0
			for unit, dmg := range pl.p.DamageTaken {
				if isHero(unit) {
					total += dmg
				}
			}
			return total, pl.p.DamageTaken != nil
		})
	}
	give(AwardFarmer, 1, func(pl *player) (float64, bool) { return float64(pl.p.LastHits), true })
	give(AwardDestroyer, 1, func(pl *player) (float64, bool) {
		if pl.p.TowerDamage == nil {
			return 0, false
		}
		return float64(*pl.p.TowerDamage), true
	})
	return out
}

// observerLifetime totals how long a player's observer wards stood: until
// their logged removal, else ObserverLifetime or the end of the game.
func observerLifetime(pl *player, duration int) float64 {
	removed := map[int64][]int{}
	for _, e := range pl.p.ObsLeftLog {
		removed[e.EHandle] = append(removed[e.EHandle], e.Time)
	}
	total := 0
	for _, e := range pl.p.ObsLog {
		end := min(e.Time+ObserverLifetime, duration)
		times := removed[e.EHandle]
		for i, t := range times {
			if t >= e.Time {
				end = t
				removed[e.EHandle] = append(times[:i:i], times[i+1:]...)
				break
			}
		}
		total += min(max(end-e.Time, 0), ObserverLifetime)
	}
	return float64(total)
}
