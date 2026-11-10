package analysis

import "math"

// ratingMetrics are the benchmark percentiles averaged into the rating.
var ratingMetrics = []string{"gold_per_min", "xp_per_min", "kills_per_min", "last_hits_per_min", "hero_damage_per_min", "tower_damage"}

// ratings scores each player's impact from 0 to 100: the average of their
// benchmark percentiles against players of the same hero, nudged by the
// lane outcome, untraded deaths and teamfight participation. A heuristic,
// labeled as such; it can't see what a support enabled.
func (mm *match) ratings(found []Insight) []Insight {
	lane := map[int]string{}
	untraded := map[int][2]int{}
	for _, in := range found {
		if in.Slot == nil {
			continue
		}
		switch in.Kind {
		case KindLane:
			lane[*in.Slot], _ = in.Evidence["outcome"].(string)
		case KindNoTradeTotal:
			u, _ := in.Evidence["untraded"].(int)
			d, _ := in.Evidence["deaths"].(int)
			untraded[*in.Slot] = [2]int{u, d}
		}
	}

	var out []Insight
	for _, pl := range mm.players {
		var sum float64
		n := 0
		for _, k := range ratingMetrics {
			if b, ok := pl.p.Benchmarks[k]; ok && b.Pct != nil {
				sum += *b.Pct
				n++
			}
		}
		if n == 0 {
			continue
		}
		base := sum / float64(n) * 100
		score := base
		switch lane[pl.slot] {
		case "won":
			score += 8
		case "lost":
			score -= 8
		}
		if u := untraded[pl.slot]; u[1] >= 4 && float64(u[0])/float64(u[1]) >= 0.6 {
			score -= 8
		}
		if tp := pl.p.TeamfightPart; tp != nil {
			switch {
			case *tp >= 0.7:
				score += 5
			case *tp <= 0.3:
				score -= 5
			}
		}
		score = math.Round(min(max(score, 0), 100))
		label, sev := "average", Info
		switch {
		case score >= RatingStandout:
			label, sev = "standout", Good
		case score >= RatingSolid:
			label, sev = "solid", Good
		case score < RatingAverage:
			label, sev = "struggled", Bad
		}
		ev := map[string]any{"score": score, "label": label, "benchmarks": math.Round(base)}
		if l := lane[pl.slot]; l != "" {
			ev["lane"] = l
		}
		out = append(out, Insight{Slot: ptr(pl.slot), Kind: KindRating, Severity: sev, Evidence: ev})
	}
	return out
}
