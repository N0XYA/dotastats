package web

import (
	"strings"

	"dotastats/internal/stats"
)

// mapBuilding is a building drawn on the match map. PX and PY are SVG
// units: map cell x-64 and 192-y, so Radiant's base is bottom left.
//
// OpenDota doesn't report building positions. These sit on the drawn
// lanes, which were lined up with the laning positions (lane_pos) of
// recorded parsed matches; the towers' spacing along a lane is
// approximate. Dire's side mirrors Radiant's through the map's centre,
// like the real map, which turns Radiant's top lane into Dire's bottom.
type mapBuilding struct {
	Key, Kind, Team, Name string
	PX, PY                float64
}

var mapBuildings = func() []mapBuilding {
	radiant := []mapBuilding{
		{Key: "tower1_top", Kind: "tower", PX: 15.5, PY: 49},
		{Key: "tower2_top", Kind: "tower", PX: 15.5, PY: 70},
		{Key: "tower3_top", Kind: "tower", PX: 15.5, PY: 90},
		{Key: "tower1_mid", Kind: "tower", PX: 52, PY: 76},
		{Key: "tower2_mid", Kind: "tower", PX: 39, PY: 89},
		{Key: "tower3_mid", Kind: "tower", PX: 30, PY: 98},
		{Key: "tower1_bot", Kind: "tower", PX: 101, PY: 112.5},
		{Key: "tower2_bot", Kind: "tower", PX: 63, PY: 112.5},
		{Key: "tower3_bot", Kind: "tower", PX: 38, PY: 112.5},
		{Key: "melee_rax_top", Kind: "rax melee", PX: 13, PY: 94.5},
		{Key: "range_rax_top", Kind: "rax", PX: 18, PY: 94.5},
		{Key: "melee_rax_mid", Kind: "rax melee", PX: 25, PY: 99},
		{Key: "range_rax_mid", Kind: "rax", PX: 29, PY: 103},
		{Key: "melee_rax_bot", Kind: "rax melee", PX: 33.5, PY: 115},
		{Key: "range_rax_bot", Kind: "rax", PX: 33.5, PY: 110},
		{Key: "tower4", Kind: "tower t4", PX: 21.5, PY: 103},
		{Key: "tower4", Kind: "tower t4", PX: 25, PY: 106.5},
		{Key: "fort", Kind: "ancient", PX: 18, PY: 110},
	}
	lane := strings.NewReplacer("_top", "_bot", "_bot", "_top")
	var out []mapBuilding
	for _, b := range radiant {
		r, d := b, b
		r.Key, r.Team = "npc_dota_goodguys_"+b.Key, "radiant"
		d.Key, d.Team = "npc_dota_badguys_"+lane.Replace(b.Key), "dire"
		d.PX, d.PY = 128-b.PX, 128-b.PY
		r.Name, d.Name = stats.BuildingName(r.Key), stats.BuildingName(d.Key)
		out = append(out, r, d)
	}
	return out
}()
