package stats

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// The interface is in Russian. These helpers keep numbers and word forms
// right: thousands separated by a non-breaking space, a decimal comma, and
// the three Russian plural forms.

const nbsp = " "

// Plural picks the form for n: one (1, 21), few (2-4, 22-24) or many
// (0, 5-20, 25...), e.g. Plural(5, "игра", "игры", "игр").
func Plural(n int, one, few, many string) string {
	n = abs(n)
	switch {
	case n%10 == 1 && n%100 != 11:
		return one
	case n%10 >= 2 && n%10 <= 4 && (n%100 < 12 || n%100 > 14):
		return few
	}
	return many
}

// Count is "5 игр": the number and its plural form.
func Count(n int, one, few, many string) string {
	return thousands(n) + " " + Plural(n, one, few, many)
}

// Decimal formats f with digits decimals and a comma.
func Decimal(f float64, digits int) string {
	return strings.Replace(strconv.FormatFloat(f, 'f', digits, 64), ".", ",", 1)
}

// Percent formats a 0..1 share as "47%".
func Percent(share float64) string { return fmt.Sprintf("%.0f%%", share*100) }

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// teamNom and teamGen are the Russian names of the sides: nominative
// ("Силы Света вели") and genitive ("в пользу Сил Света").
func teamNom(team string) string {
	switch team {
	case "radiant":
		return "Силы Света"
	case "dire":
		return "Силы Тьмы"
	}
	return "Команда"
}

func teamGen(team string) string {
	switch team {
	case "radiant":
		return "Сил Света"
	case "dire":
		return "Сил Тьмы"
	}
	return "команды"
}

// TeamName is a side's Russian name, for templates.
func TeamName(team string) string { return teamNom(team) }

// roundInt rounds a float for display.
func roundInt(f float64) int { return int(math.Round(f)) }

// kindNames label timeline lines by what they are.
var kindNames = map[string]string{
	"death": "смерть", "building": "строение", "roshan": "Рошан", "aegis": "Аегис", "courier": "курьер",
	"buyback": "выкуп", "teamfight": "драка", "turning_point": "перелом", "win_swing": "сдвиг",
	"death_streak": "серия смертей", "wasted_buyback": "выкуп впустую", "dead_while_lost": "потери",
	"xp_lead_10": "опыт",
}

// KindName is a timeline line's label.
func KindName(kind string) string { return kindNames[kind] }
