package opendota

import (
	"encoding/json"
	"time"
)

// Field names and nullability below are checked against the recorded
// responses in testdata/opendota (see its README), not just the spec.
// Pointers mark fields that are null or absent in real responses.

// Player is GET /players/{account_id}.
type Player struct {
	Profile         *Profile `json:"profile"`
	RankTier        *int     `json:"rank_tier"`
	LeaderboardRank *int     `json:"leaderboard_rank"`
}

// Profile is the Steam profile part of Player.
type Profile struct {
	AccountID    int64   `json:"account_id"`
	PersonaName  string  `json:"personaname"`
	Name         *string `json:"name"`
	SteamID      string  `json:"steamid"`
	Avatar       string  `json:"avatar"`
	AvatarMedium string  `json:"avatarmedium"`
	AvatarFull   string  `json:"avatarfull"`
}

// PlayerMatch is one entry of GET /players/{account_id}/matches. Request
// it with project= set to PlayerMatchFields; the fields after PartySize
// only come back when projected.
type PlayerMatch struct {
	MatchID    int64 `json:"match_id"`
	PlayerSlot *int  `json:"player_slot"`
	RadiantWin *bool `json:"radiant_win"`
	Duration   int   `json:"duration"`
	GameMode   int   `json:"game_mode"`
	LobbyType  int   `json:"lobby_type"`
	HeroID     int   `json:"hero_id"`
	StartTime  int64 `json:"start_time"`
	// Version is the parser version; null while the match is unparsed.
	Version      *int `json:"version"`
	Kills        int  `json:"kills"`
	Deaths       int  `json:"deaths"`
	Assists      int  `json:"assists"`
	AverageRank  *int `json:"average_rank"`
	LeaverStatus *int `json:"leaver_status"`
	PartySize    *int `json:"party_size"`

	GoldPerMin  *int  `json:"gold_per_min"`
	XPPerMin    *int  `json:"xp_per_min"`
	LastHits    *int  `json:"last_hits"`
	Denies      *int  `json:"denies"`
	HeroDamage  *int  `json:"hero_damage"`
	TowerDamage *int  `json:"tower_damage"`
	HeroHealing *int  `json:"hero_healing"`
	Level       *int  `json:"level"`
	Item0       *int  `json:"item_0"`
	Item1       *int  `json:"item_1"`
	Item2       *int  `json:"item_2"`
	Item3       *int  `json:"item_3"`
	Item4       *int  `json:"item_4"`
	Item5       *int  `json:"item_5"`
	ItemNeutral *int  `json:"item_neutral"`
	Lane        *int  `json:"lane"`
	LaneRole    *int  `json:"lane_role"`
	IsRoaming   *bool `json:"is_roaming"`
}

// PlayerMatchFields is the project= list for PlayerMatch. Projection
// replaces the default field set, so every field is listed; match_id,
// player_slot and radiant_win always come back. backpack_*, item_neutral2,
// net_worth and rank_tier are silently not projectable.
var PlayerMatchFields = []string{
	"hero_id", "start_time", "duration", "game_mode", "lobby_type", "version",
	"kills", "deaths", "assists", "average_rank", "leaver_status", "party_size",
	"gold_per_min", "xp_per_min", "last_hits", "denies", "hero_damage",
	"tower_damage", "hero_healing", "level", "item_0", "item_1", "item_2",
	"item_3", "item_4", "item_5", "item_neutral", "lane", "lane_role", "is_roaming",
}

// Match is GET /matches/{match_id}: the scoreboard plus, once parsed, the
// time series and logs the extractor stores.
type Match struct {
	MatchID    int64 `json:"match_id"`
	StartTime  int64 `json:"start_time"`
	Duration   int   `json:"duration"`
	GameMode   int   `json:"game_mode"`
	LobbyType  int   `json:"lobby_type"`
	RadiantWin *bool `json:"radiant_win"`
	Patch      *int  `json:"patch"`
	Region     *int  `json:"region"`
	// Version is the parser version: null while unparsed, and absent in
	// old matches.
	Version *int `json:"version"`
	// Team advantage per minute, kept as the raw JSON arrays.
	RadiantGoldAdv json.RawMessage `json:"radiant_gold_adv"`
	RadiantXPAdv   json.RawMessage `json:"radiant_xp_adv"`
	Players        []MatchPlayer   `json:"players"`

	// Parsed only.
	Objectives []Objective `json:"objectives"`
	Teamfights []Teamfight `json:"teamfights"`
	Chat       []Chat      `json:"chat"`
}

// Objective is one entry of Match.Objectives. Which fields are set depends
// on Type: building_kill (Key is the building, Unit the killer),
// CHAT_MESSAGE_FIRSTBLOOD (PlayerSlot killed VictimSlot),
// CHAT_MESSAGE_ROSHAN_KILL (Team), CHAT_MESSAGE_AEGIS (PlayerSlot),
// CHAT_MESSAGE_COURIER_LOST (Team, Killer). Team 2 is Radiant, 3 Dire.
type Objective struct {
	Time       int    `json:"time"`
	Type       string `json:"type"`
	Key        string `json:"key"`
	Unit       string `json:"unit"`
	PlayerSlot *int   `json:"player_slot"`
	VictimSlot *int   `json:"victim_player_slot"`
	Team       *int   `json:"team"`
	Killer     *int   `json:"killer"`
}

// Teamfight is one entry of Match.Teamfights. Players are in the same
// order as Match.Players (checked against the death logs).
type Teamfight struct {
	Start     int               `json:"start"`
	End       int               `json:"end"`
	LastDeath int               `json:"last_death"`
	Deaths    int               `json:"deaths"`
	Players   []TeamfightPlayer `json:"players"`
}

// TeamfightPlayer is one player's part in a teamfight. DeathsPos maps x to
// y to a count of deaths there, in the same map cells as ward positions
// (64..192 on both axes).
type TeamfightPlayer struct {
	Deaths    int                       `json:"deaths"`
	Buybacks  int                       `json:"buybacks"`
	Damage    int                       `json:"damage"`
	Healing   int                       `json:"healing"`
	GoldDelta int                       `json:"gold_delta"`
	XPDelta   int                       `json:"xp_delta"`
	DeathsPos map[string]map[string]int `json:"deaths_pos"`
}

// Chat is one message of Match.Chat: Type "chat" with the text in Key, or
// "chatwheel" with a chat wheel ID in Key.
type Chat struct {
	Time       int    `json:"time"`
	Type       string `json:"type"`
	Key        string `json:"key"`
	PlayerSlot *int   `json:"player_slot"`
}

// Parsed reports whether OpenDota has parsed the replay.
func (m Match) Parsed() bool { return m.Version != nil }

// MatchPlayer is one of the ten entries in Match.Players.
type MatchPlayer struct {
	// AccountID is null for anonymous players. Never join on it then.
	AccountID    *int64 `json:"account_id"`
	PlayerSlot   int    `json:"player_slot"`
	PersonaName  string `json:"personaname"`
	HeroID       int    `json:"hero_id"`
	Kills        int    `json:"kills"`
	Deaths       int    `json:"deaths"`
	Assists      int    `json:"assists"`
	LeaverStatus *int   `json:"leaver_status"`
	PartySize    *int   `json:"party_size"`
	LastHits     int    `json:"last_hits"`
	Denies       int    `json:"denies"`
	GoldPerMin   int    `json:"gold_per_min"`
	XPPerMin     int    `json:"xp_per_min"`
	// NetWorth and ItemNeutral2 are missing from the spec but present in
	// responses. ItemNeutral2 is the neutral enhancement.
	NetWorth *int `json:"net_worth"`
	// The damage and healing totals are absent in old (2016) matches.
	HeroDamage   *int `json:"hero_damage"`
	TowerDamage  *int `json:"tower_damage"`
	HeroHealing  *int `json:"hero_healing"`
	Level        int  `json:"level"`
	Item0        int  `json:"item_0"`
	Item1        int  `json:"item_1"`
	Item2        int  `json:"item_2"`
	Item3        int  `json:"item_3"`
	Item4        int  `json:"item_4"`
	Item5        int  `json:"item_5"`
	Backpack0    int  `json:"backpack_0"`
	Backpack1    int  `json:"backpack_1"`
	Backpack2    int  `json:"backpack_2"`
	ItemNeutral  int  `json:"item_neutral"`
	ItemNeutral2 *int `json:"item_neutral2"`
	// Lane, LaneRole and IsRoaming are absent until the match is parsed.
	Lane      *int  `json:"lane"`
	LaneRole  *int  `json:"lane_role"`
	IsRoaming *bool `json:"is_roaming"`
	RankTier  *int  `json:"rank_tier"`

	// Parsed only. Per-minute values, kept as the raw JSON arrays.
	// NetworthT is missing from the spec but present in responses.
	GoldT     json.RawMessage `json:"gold_t"`
	XPT       json.RawMessage `json:"xp_t"`
	LHT       json.RawMessage `json:"lh_t"`
	DNT       json.RawMessage `json:"dn_t"`
	NetworthT json.RawMessage `json:"networth_t"`
	// Parsed only.
	PurchaseLog []Purchase  `json:"purchase_log"`
	ObsLog      []WardEvent `json:"obs_log"`
	SenLog      []WardEvent `json:"sen_log"`
	ObsLeftLog  []WardEvent `json:"obs_left_log"`
	SenLeftLog  []WardEvent `json:"sen_left_log"`
	KillsLog    []LogEntry  `json:"kills_log"` // Key: the victim hero
	// DeathsLog is missing from the spec but present in responses and,
	// unlike kills_log, complete. Key is the killer unit.
	DeathsLog  []Death    `json:"deaths_log"`
	BuybackLog []LogEntry `json:"buyback_log"`

	// Parsed only.
	Stuns         *float64           `json:"stuns"` // seconds of stun dealt
	DamageTaken   map[string]float64 `json:"damage_taken"`
	LifeStateDead *int               `json:"life_state_dead"` // seconds dead
	ObserverKills *int               `json:"observer_kills"`
	SentryKills   *int               `json:"sentry_kills"`
	CampsStacked  *int               `json:"camps_stacked"`
	TeamfightPart *float64           `json:"teamfight_participation"`

	// Benchmarks maps a metric (gold_per_min, ...) to the player's value
	// and percentile among players of the hero. Present in unparsed
	// matches too.
	Benchmarks map[string]Benchmark `json:"benchmarks"`
}

// LogEntry is a timed entry of kills_log or buyback_log.
type LogEntry struct {
	Time int    `json:"time"`
	Key  string `json:"key"`
}

// Death is one deaths_log entry. TimeDead is missing for some deaths, e.g.
// one right before the game ends.
type Death struct {
	Time     int    `json:"time"`
	Key      string `json:"key"` // killer unit, e.g. npc_dota_hero_luna
	GoldLost int    `json:"gold_lost"`
	TimeDead *int   `json:"time_dead"`
}

// Purchase is one purchase_log entry.
type Purchase struct {
	Time int    `json:"time"` // seconds from the horn; negative before it
	Key  string `json:"key"`  // item name, as in /constants/items
}

// WardEvent is one entry of a ward placement or *_left_log. A ward's
// placement and removal share EHandle.
type WardEvent struct {
	Time    int     `json:"time"`
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	EHandle int64   `json:"ehandle"`
}

// Benchmark is one metric of MatchPlayer.Benchmarks.
type Benchmark struct {
	Raw *float64 `json:"raw"`
	Pct *float64 `json:"pct"`
}

// Hero is one value of GET /constants/heroes, an object keyed by hero ID.
type Hero struct {
	ID            int    `json:"id"`
	Name          string `json:"name"`           // npc_dota_hero_antimage
	LocalizedName string `json:"localized_name"` // Anti-Mage
	// Img and Icon are paths on the Steam CDN (IconBaseURL).
	Img  string `json:"img"`
	Icon string `json:"icon"`
}

// Item is one value of GET /constants/items, an object keyed by item name
// (blink).
type Item struct {
	ID    int    `json:"id"`
	DName string `json:"dname"` // missing for a few items
	Img   string `json:"img"`
	Cost  *int   `json:"cost"`
}

// GameMode is one value of GET /constants/game_mode, keyed by ID. Lobby
// types from GET /constants/lobby_type have the same shape.
type GameMode struct {
	ID   int    `json:"id"`
	Name string `json:"name"` // game_mode_all_pick
}

// ChatWheel is one value of GET /constants/chat_wheel, keyed by ID. Some
// messages are localization keys (dota_chatwheel_message_...).
type ChatWheel struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Message string `json:"message"`
}

// Patch is one element of GET /constants/patch, an array.
type Patch struct {
	ID   int       `json:"id"`
	Name string    `json:"name"` // 7.41
	Date time.Time `json:"date"`
}
