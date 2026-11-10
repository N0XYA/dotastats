package analysis

import (
	"cmp"
	"slices"
)

// lanes compares each laner with their lane opponent at LaneMinute: same
// physical lane, enemies paired by net worth rank (core against core,
// support against support). Players without an opponent are skipped.
func (mm *match) lanes() []Insight {
	if !mm.parsed {
		return nil
	}
	at := func(s []float64) float64 { return s[LaneMinute] }
	hasMinute := func(pl *player) bool {
		return len(pl.nw) > LaneMinute && len(pl.lh) > LaneMinute && len(pl.dn) > LaneMinute && len(pl.xp) > LaneMinute
	}
	laners := func(team string, lane int) []*player {
		var out []*player
		for _, pl := range mm.players {
			if pl.team == team && pl.p.Lane != nil && *pl.p.Lane == lane && hasMinute(pl) {
				out = append(out, pl)
			}
		}
		slices.SortStableFunc(out, func(a, b *player) int { return cmp.Or(cmp.Compare(at(b.nw), at(a.nw)), cmp.Compare(a.slot, b.slot)) })
		return out
	}

	var out []Insight
	for _, pl := range mm.players {
		if pl.p.Lane == nil || *pl.p.Lane < 1 || *pl.p.Lane > 3 || !hasMinute(pl) {
			continue
		}
		enemy := "dire"
		if pl.team == "dire" {
			enemy = "radiant"
		}
		allies, opps := laners(pl.team, *pl.p.Lane), laners(enemy, *pl.p.Lane)
		rank := slices.Index(allies, pl)
		if rank < 0 || rank >= len(opps) {
			continue
		}
		o := opps[rank]
		diff := at(pl.nw) - at(o.nw)
		ratio := diff / max(at(o.nw), 1)
		outcome, sev := "even", Info
		switch {
		case diff >= LaneWinGold && ratio >= LaneWinRatio:
			outcome, sev = "won", Good
		case diff <= -LaneWinGold && ratio <= -LaneWinRatio:
			outcome, sev = "lost", Bad
		}
		out = append(out, Insight{
			Slot: ptr(pl.slot), Kind: KindLane, Time: ptr(LaneMinute * 60), Severity: sev,
			Evidence: map[string]any{
				"outcome": outcome, "opponent": o.slot, "lane": *pl.p.Lane,
				"nw": at(pl.nw), "opp_nw": at(o.nw), "lh": at(pl.lh), "opp_lh": at(o.lh),
				"dn": at(pl.dn), "opp_dn": at(o.dn), "xp": at(pl.xp), "opp_xp": at(o.xp),
			},
		})
	}
	return out
}
