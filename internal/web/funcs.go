package web

import (
	"fmt"
	"html/template"
	"math"
	"net/url"
	"strconv"
	"time"

	"dotastats/internal/stats"
)

var funcs = template.FuncMap{
	"duration":   duration,
	"clock":      clock,
	"dclock":     dclock,
	"pct":        func(f float64) string { return stats.Decimal(f*100, 1) + "%" },
	"f1":         func(f float64) string { return stats.Decimal(f, 1) },
	"f2":         func(f float64) string { return stats.Decimal(f, 2) },
	"num":        num,
	"thousands":  thousands,
	"numf":       numf,
	"thousandsf": func(f float64) string { return thousands(int(math.Round(f))) },
	"signedf": func(f float64) string {
		n := int(math.Round(f))
		if n > 0 {
			return "+" + thousands(n)
		}
		return thousands(n)
	},
	"avg":    avg,
	"signed": signed,
	"iso":    func(t any) string { return asTime(t).UTC().Format(time.RFC3339) },
	"utc":    func(t any) string { return asTime(t).UTC().Format("2006-01-02 15:04 UTC") },
	"rank":   stats.RankName,
	"role": func(r *int) string {
		if r == nil {
			return ""
		}
		return stats.LaneRoleName(*r)
	},
	"result": result,
	"side": func(slot int) string {
		if slot < 128 {
			return "radiant"
		}
		return "dire"
	},
	"query":      withQuery,
	"add":        func(a, b int) int { return a + b },
	"sub":        func(a, b int) int { return a - b },
	"ptr":        func(p *int) int { return deref(p) },
	"itoa":       strconv.Itoa,
	"intp":       func(n int) *int { return &n },
	"list":       func(xs ...int) []int { return xs },
	"strings":    func(xs ...string) []string { return xs },
	"concat":     func(a, b []stats.PlayerRow) []stats.PlayerRow { return append(append([]stats.PlayerRow{}, a...), b...) },
	"bytes":      formatBytes,
	"dict":       dict,
	"team":       stats.TeamName,
	"ratingName": stats.RatingName,
	"laneName":   stats.LaneName,
	"count":      stats.Count,
	"kindName":   stats.KindName,
	"divf": func(a, b int) float64 {
		if b == 0 {
			return 0
		}
		return min(float64(a)/float64(b), 1)
	},
	"pctBar": func(f float64) template.CSS { return template.CSS(fmt.Sprintf("width:%.1f%%", f*100)) },
}

// duration formats seconds as m:ss, or h:mm:ss from an hour.
func duration(sec int) string {
	if sec >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", sec/3600, sec/60%60, sec%60)
	}
	return fmt.Sprintf("%d:%02d", sec/60, sec%60)
}

// clock formats a signed game time (purchases before the horn are
// negative) as m:ss. It takes int, *int or float64 (averages).
func clock(v any) string {
	var sec int
	switch t := v.(type) {
	case int:
		sec = t
	case *int:
		if t == nil {
			return "–"
		}
		sec = *t
	case float64:
		sec = int(math.Round(t))
	default:
		return fmt.Sprint(v)
	}
	sign := ""
	if sec < 0 {
		sign, sec = "-", -sec
	}
	return sign + duration(sec)
}

// asTime accepts time.Time or *time.Time; templates pass both.
func asTime(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t
	case *time.Time:
		if t != nil {
			return *t
		}
	}
	return time.Time{}
}

// dclock formats a change in seconds with an explicit sign: "+0:12".
func dclock(sec float64) string {
	n := int(math.Round(sec))
	if n == 0 {
		return "±0:00"
	}
	if n > 0 {
		return "+" + duration(n)
	}
	return "−" + duration(-n)
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// num formats an optional count with thousands separators, or an en dash.
func num(p *int) string {
	if p == nil {
		return "–"
	}
	return thousands(*p)
}

// numf rounds an optional average.
func numf(p *float64) string {
	if p == nil {
		return "–"
	}
	return thousands(int(math.Round(*p)))
}

// avg shows an optional average with one decimal below 100 (kills,
// deaths) and as a whole number above (GPM, damage).
func avg(p *float64) string {
	switch {
	case p == nil:
		return "–"
	case math.Abs(*p) < 100:
		return stats.Decimal(*p, 1)
	}
	return thousands(int(math.Round(*p)))
}

// thousands groups digits with non-breaking spaces, Russian style: 29 794.
func thousands(n int) string {
	s := strconv.Itoa(n)
	neg := n < 0
	if neg {
		s = s[1:]
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "\u00a0" + s[i:]
	}
	if neg {
		s = "−" + s
	}
	return s
}

// signed formats a change with an explicit sign: one decimal below 100,
// whole numbers above.
func signed(f float64) string {
	sign := "+"
	if f < 0 {
		sign = "−"
	}
	switch {
	case math.Abs(f) < 0.05:
		return "±0"
	case math.Abs(f) >= 100:
		return sign + thousands(int(math.Round(math.Abs(f))))
	}
	return sign + stats.Decimal(math.Abs(f), 1)
}

// result is "win", "loss" or "unknown" for an optional outcome; a nil
// pointer would otherwise be truthy in templates.
func result(win *bool) string {
	switch {
	case win == nil:
		return "unknown"
	case *win:
		return "win"
	}
	return "loss"
}

// withQuery returns "?..." with the given keys set (empty value removes
// the key), keeping the rest of q.
func withQuery(q url.Values, kv ...string) template.URL {
	out := url.Values{}
	for k, v := range q {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "" {
			out.Del(kv[i])
		} else {
			out.Set(kv[i], kv[i+1])
		}
	}
	if len(out) == 0 {
		return "?"
	}
	return template.URL("?" + out.Encode())
}

func formatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return stats.Decimal(float64(n)/(1<<30), 1) + " ГБ"
	case n >= 1<<20:
		return stats.Decimal(float64(n)/(1<<20), 1) + " МБ"
	case n >= 1<<10:
		return stats.Decimal(float64(n)/(1<<10), 0) + " КБ"
	}
	return fmt.Sprintf("%d Б", n)
}

// dict builds a map from key/value pairs, for passing several values to a
// partial template.
func dict(kv ...any) (map[string]any, error) {
	if len(kv)%2 != 0 {
		return nil, fmt.Errorf("dict: odd number of arguments")
	}
	m := make(map[string]any, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict: key %v is not a string", kv[i])
		}
		m[k] = kv[i+1]
	}
	return m, nil
}
