package stats

import (
	"context"
	"database/sql"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Hero is a hero from the constants tables. Paths are relative to the
// assets directory and empty until the icon is downloaded.
type Hero struct {
	ID       int
	Key      string // npc_dota_hero_antimage, as in logs
	Name     string // Anti-Mage
	ImgPath  string
	IconPath string
}

// Item is an item from the constants tables.
type Item struct {
	ID      int
	Key     string // blink
	Name    string // Blink Dagger
	ImgPath string
	Cost    int
}

// Patch is a game patch.
type Patch struct {
	ID       int
	Name     string
	Released time.Time
}

// Constants are the lookup tables pages render names and icons from.
type Constants struct {
	Heroes     map[int]Hero
	Items      map[int]Item
	ItemsByKey map[string]Item
	GameModes  map[int]string
	LobbyTypes map[int]string
	Patches    []Patch // newest first
	// HeroesByKey and ChatWheel name things in match logs.
	HeroesByKey map[string]Hero
	ChatWheel   map[int]string
}

// LoadConstants reads every constants table.
func LoadConstants(ctx context.Context, db *sql.DB) (*Constants, error) {
	c := &Constants{
		Heroes: map[int]Hero{}, Items: map[int]Item{}, ItemsByKey: map[string]Item{},
		GameModes: map[int]string{}, LobbyTypes: map[int]string{},
		HeroesByKey: map[string]Hero{}, ChatWheel: map[int]string{},
	}
	if err := each(ctx, db, `SELECT id, name, localized_name, coalesce(img_path, ''), coalesce(icon_path, '') FROM heroes`,
		func(r *sql.Rows) error {
			var h Hero
			if err := r.Scan(&h.ID, &h.Key, &h.Name, &h.ImgPath, &h.IconPath); err != nil {
				return err
			}
			c.Heroes[h.ID] = h
			c.HeroesByKey[h.Key] = h
			return nil
		}); err != nil {
		return nil, err
	}
	if err := each(ctx, db, `SELECT id, name, message FROM chat_wheel`, func(r *sql.Rows) error {
		var id int
		var name, msg string
		if err := r.Scan(&id, &name, &msg); err != nil {
			return err
		}
		// Some messages are untranslated localization keys; the name
		// ("MissingHero") reads better then.
		if strings.HasPrefix(msg, "dota_chatwheel_") {
			msg = splitWords(name)
		}
		c.ChatWheel[id] = msg
		return nil
	}); err != nil {
		return nil, err
	}
	if err := each(ctx, db, `SELECT id, name, coalesce(dname, name), coalesce(img_path, ''), coalesce(cost, 0) FROM items`,
		func(r *sql.Rows) error {
			var it Item
			if err := r.Scan(&it.ID, &it.Key, &it.Name, &it.ImgPath, &it.Cost); err != nil {
				return err
			}
			c.Items[it.ID] = it
			c.ItemsByKey[it.Key] = it
			return nil
		}); err != nil {
		return nil, err
	}
	for _, t := range []struct {
		query, prefix string
		dst           map[int]string
		ru            map[string]string
	}{
		{"SELECT id, name FROM game_modes", "game_mode_", c.GameModes, gameModesRU},
		{"SELECT id, name FROM lobby_types", "lobby_type_", c.LobbyTypes, lobbyTypesRU},
	} {
		if err := each(ctx, db, t.query, func(r *sql.Rows) error {
			var id int
			var name string
			if err := r.Scan(&id, &name); err != nil {
				return err
			}
			if ru, ok := t.ru[name]; ok {
				t.dst[id] = ru
			} else {
				t.dst[id] = prettyConstant(name, t.prefix)
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	if err := each(ctx, db, `SELECT id, name, released_at FROM patches ORDER BY released_at DESC`,
		func(r *sql.Rows) error {
			var p Patch
			var released int64
			if err := r.Scan(&p.ID, &p.Name, &released); err != nil {
				return err
			}
			p.Released = time.Unix(released, 0)
			c.Patches = append(c.Patches, p)
			return nil
		}); err != nil {
		return nil, err
	}
	return c, nil
}

// Game modes keep their in-game names; OpenDota's "All Draft" is what the
// client calls All Pick. Lobby types are translated.
var gameModesRU = map[string]string{
	"game_mode_all_draft": "All Pick", "game_mode_all_pick": "All Pick (старый)", "game_mode_turbo": "Турбо",
	"game_mode_unknown": "Неизвестный режим", "game_mode_1v1_mid": "1 на 1 (мид)", "game_mode_custom": "Пользовательская игра",
	"game_mode_event": "Событие", "game_mode_tutorial": "Обучение",
}

var lobbyTypesRU = map[string]string{
	"lobby_type_normal": "Обычная", "lobby_type_practice": "Лобби", "lobby_type_tournament": "Турнир",
	"lobby_type_tutorial": "Обучение", "lobby_type_coop_bots": "С ботами", "lobby_type_ranked_team_mm": "Рейтинговая (команда)",
	"lobby_type_ranked_solo_mm": "Рейтинговая (соло)", "lobby_type_ranked": "Рейтинговая", "lobby_type_1v1_mid": "1 на 1 (мид)",
	"lobby_type_battle_cup": "Боевой кубок", "lobby_type_local_bots": "С ботами (локально)", "lobby_type_spectator": "Зритель",
	"lobby_type_event": "Событие", "lobby_type_gauntlet": "Испытание", "lobby_type_new_player": "Для новичков",
	"lobby_type_featured": "Особый режим",
}

// prettyConstant turns "game_mode_all_draft" into "All Draft".
func prettyConstant(name, prefix string) string {
	words := strings.Fields(strings.ReplaceAll(strings.TrimPrefix(name, prefix), "_", " "))
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

// HeroName returns the hero's name, or "Hero <id>" before constants load.
func (c *Constants) HeroName(id int) string {
	if h, ok := c.Heroes[id]; ok {
		return h.Name
	}
	return "Герой " + strconv.Itoa(id)
}

// HeroIcon returns the small hero icon's asset path, or "".
func (c *Constants) HeroIcon(id int) string { return c.Heroes[id].IconPath }

// HeroImage returns the wide hero portrait's asset path, or "".
func (c *Constants) HeroImage(id int) string { return c.Heroes[id].ImgPath }

// Item returns the item with id. Items missing from constants (a new
// patch before the daily refresh) get a placeholder name.
func (c *Constants) Item(id int) Item {
	if it, ok := c.Items[id]; ok {
		return it
	}
	return Item{ID: id, Name: "Предмет " + strconv.Itoa(id)}
}

// ItemByKey returns the item named key (as in purchase logs).
func (c *Constants) ItemByKey(key string) Item {
	if it, ok := c.ItemsByKey[key]; ok {
		return it
	}
	return Item{Key: key, Name: key}
}

// GameMode names a game mode ID.
func (c *Constants) GameMode(id int) string {
	if n, ok := c.GameModes[id]; ok {
		return n
	}
	return "Режим " + strconv.Itoa(id)
}

// LobbyType names a lobby type ID.
func (c *Constants) LobbyType(id int) string {
	if n, ok := c.LobbyTypes[id]; ok {
		return n
	}
	return "Лобби " + strconv.Itoa(id)
}

// PatchName names a patch ID, or "" if unknown.
func (c *Constants) PatchName(id *int) string {
	if id == nil {
		return ""
	}
	for _, p := range c.Patches {
		if p.ID == *id {
			return p.Name
		}
	}
	return ""
}

// CurrentPatch is the newest patch, if constants are loaded.
func (c *Constants) CurrentPatch() (Patch, bool) {
	if len(c.Patches) == 0 {
		return Patch{}, false
	}
	return c.Patches[0], true
}

func each(ctx context.Context, db *sql.DB, query string, fn func(*sql.Rows) error, args ...any) error {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// HeroesByName returns the given hero IDs sorted by hero name.
func (c *Constants) HeroesByName(ids []int) []Hero {
	out := make([]Hero, 0, len(ids))
	for _, id := range ids {
		h, ok := c.Heroes[id]
		if !ok {
			h = Hero{ID: id, Name: c.HeroName(id)}
		}
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// splitWords turns "MissingHero" or "Missing_Mid" into "Missing hero" and
// "Missing mid".
func splitWords(name string) string {
	var b strings.Builder
	for i, r := range strings.ReplaceAll(name, "_", " ") {
		if i > 0 && r >= 'A' && r <= 'Z' && !strings.HasSuffix(b.String(), " ") {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	s := strings.ToLower(b.String())
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// UnitName names a unit from a log: a hero, a building, creeps, neutrals
// or Roshan.
func (c *Constants) UnitName(key string) string {
	if h, ok := c.HeroesByKey[key]; ok {
		return h.Name
	}
	switch {
	case strings.Contains(key, "_tower") || strings.Contains(key, "_rax_") || strings.HasSuffix(key, "_fort"):
		return BuildingName(key)
	case strings.Contains(key, "roshan"):
		return "Рошан"
	case strings.HasPrefix(key, "npc_dota_creep"):
		return "крипы"
	case strings.HasPrefix(key, "npc_dota_neutral"):
		return "нейтральные крипы"
	case key == "":
		return "неизвестно"
	}
	return splitWords(strings.TrimPrefix(key, "npc_dota_"))
}

var laneRU = map[string]string{"top": "верх", "mid": "мид", "bot": "низ"}

// BuildingName turns npc_dota_badguys_tower1_mid into
// "башня T1, мид (Силы Тьмы)".
func BuildingName(key string) string {
	team := ""
	switch {
	case strings.Contains(key, "goodguys"):
		team, key = " (Силы Света)", key[strings.Index(key, "goodguys")+len("goodguys_"):]
	case strings.Contains(key, "badguys"):
		team, key = " (Силы Тьмы)", key[strings.Index(key, "badguys")+len("badguys_"):]
	}
	lane := func(l string) string {
		if ru, ok := laneRU[l]; ok {
			return ru
		}
		return l
	}
	parts := strings.Split(key, "_")
	switch {
	case strings.HasPrefix(key, "tower") && len(parts) == 2:
		return "башня T" + strings.TrimPrefix(parts[0], "tower") + ", " + lane(parts[1]) + team
	case strings.HasPrefix(key, "tower"):
		return "башня T" + strings.TrimPrefix(key, "tower") + team
	case strings.HasPrefix(key, "melee_rax_"):
		return "казармы ближнего боя, " + lane(strings.TrimPrefix(key, "melee_rax_")) + team
	case strings.HasPrefix(key, "range_rax_"):
		return "казармы дальнего боя, " + lane(strings.TrimPrefix(key, "range_rax_")) + team
	case key == "fort":
		return "Древний" + team
	}
	return strings.ReplaceAll(key, "_", " ") + team
}

// ChatWheelText is a chat wheel phrase (as OpenDota has it, in English),
// or a placeholder for IDs missing from the constants (hero and sound
// lines).
func (c *Constants) ChatWheelText(id string) string {
	n, err := strconv.Atoi(id)
	if err != nil {
		return id
	}
	if msg, ok := c.ChatWheel[n]; ok {
		return msg
	}
	return "фраза колеса чата №" + id
}
