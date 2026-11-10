package analysis

import "math"

// teamNetWorth sums the teams' net worth per minute over the minutes all
// players have.
func (mm *match) teamNetWorth() (radiant, dire []float64) {
	n := math.MaxInt
	for _, pl := range mm.players {
		n = min(n, len(pl.nw))
	}
	if n == math.MaxInt || n == 0 {
		return nil, nil
	}
	radiant, dire = make([]float64, n), make([]float64, n)
	for _, pl := range mm.players {
		dst := radiant
		if pl.team == "dire" {
			dst = dire
		}
		for t := range n {
			dst[t] += pl.nw[t]
		}
	}
	return radiant, dire
}

// turningPoint finds the first minute from which one team led net worth by
// TurningLead for TurningMinutes in a row.
func (mm *match) turningPoint() []Insight {
	if !mm.parsed {
		return nil
	}
	radiant, dire := mm.teamNetWorth()
	lead := func(t int) (string, float64) {
		if radiant[t] >= dire[t] {
			return "radiant", (radiant[t] - dire[t]) / max(dire[t], 1)
		}
		return "dire", (dire[t] - radiant[t]) / max(radiant[t], 1)
	}
	for t := 0; t+TurningMinutes <= len(radiant); t++ {
		side, ratio := lead(t)
		ok := true
		for k := 1; k < TurningMinutes && ok; k++ {
			s, r := lead(t + k)
			ok = s == side && r >= TurningLead
		}
		if ratio >= TurningLead && ok {
			gold := math.Abs(radiant[t] - dire[t])
			return []Insight{{
				Kind: KindTurningPoint, Time: ptr(t * 60), Severity: Info,
				Evidence: map[string]any{"team": side, "lead_pct": ratio, "lead": gold, "held": side == mm.winnerTeam()},
			}}
		}
	}
	return nil
}

// WinChance is an approximate Radiant win chance per minute from the gold
// and XP leads: a logistic heuristic, not a model fitted to data.
func WinChance(goldAdv, xpAdv []float64) []float64 {
	out := make([]float64, len(goldAdv))
	for t, g := range goldAdv {
		x := 0.0
		if t < len(xpAdv) {
			x = xpAdv[t]
		}
		z := (g + WinXPWeight*x) / (WinScale + WinScalePerMinute*float64(t))
		out[t] = 1 / (1 + math.Exp(-z))
	}
	return out
}

// winSwing marks the minute where the approximate win chance moved most.
func (mm *match) winSwing() []Insight {
	gold, xp := series(mm.m.RadiantGoldAdv), series(mm.m.RadiantXPAdv)
	if !mm.parsed || len(gold) < 2 {
		return nil
	}
	p := WinChance(gold, xp)
	best := 1
	for t := 2; t < len(p); t++ {
		if math.Abs(p[t]-p[t-1]) > math.Abs(p[best]-p[best-1]) {
			best = t
		}
	}
	toward := "radiant"
	if p[best] < p[best-1] {
		toward = "dire"
	}
	return []Insight{{
		Kind: KindWinSwing, Time: ptr(best * 60), Severity: Info,
		Evidence: map[string]any{"from": p[best-1], "to": p[best], "toward": toward},
	}}
}
