package web

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"dotastats/internal/stats"
)

type overviewPage struct {
	stats.Overview
	Habits stats.Habits
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request, p *page) {
	ctx := r.Context()
	o, err := stats.LoadOverview(ctx, s.opts.Store.Read, p.Profile.ID, p.C, time.Now())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d := overviewPage{Overview: o}
	if d.Habits, err = stats.LoadHabits(ctx, s.opts.Store.Read, p.Profile.ID, p.C); err != nil {
		s.fail(w, r, err)
		return
	}
	p.Title, p.Nav, p.Data = p.Profile.DisplayName(), "overview", d
	s.render(w, r, "overview", "", http.StatusOK, p)
}

const matchesPerPage = 50

type matchesPage struct {
	Filter  filterBar
	Rows    []stats.MatchRow
	Total   int
	Page    int
	Pages   int
	Showing [2]int // first and last row number on the page
}

func (s *Server) handleMatches(w http.ResponseWriter, r *http.Request, p *page) {
	ctx := r.Context()
	fb, err := s.filterBar(r, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("page"))
	d := matchesPage{Filter: fb, Page: max(n, 1)}
	if d.Rows, d.Total, err = stats.Matches(ctx, s.opts.Store.Read, fb.Filter, d.Page, matchesPerPage); err != nil {
		s.fail(w, r, err)
		return
	}
	d.Pages = max((d.Total+matchesPerPage-1)/matchesPerPage, 1)
	if len(d.Rows) > 0 {
		first := (d.Page-1)*matchesPerPage + 1
		d.Showing = [2]int{first, first + len(d.Rows) - 1}
	}
	p.Title, p.Nav, p.Data = "Матчи", "matches", d
	s.render(w, r, "matches", "match_table", http.StatusOK, p)
}

type matchPage struct {
	stats.MatchDetail
	Mine     *stats.MatchRow  // the active profile's row, if it played
	Me       *stats.PlayerRow // and its scoreboard line
	Analysis stats.MatchAnalysis
	Charts   matchCharts

	MapBuildings []mapBuilding
}

type matchCharts struct {
	Advantage struct {
		Gold []float64 `json:"gold"`
		XP   []float64 `json:"xp"`
	} `json:"advantage"`
	Players   []stats.PlayerSeries `json:"players"`
	WinChance []float64            `json:"win_chance"`
	Markers   []stats.ChartMarker  `json:"markers"`
	Map       stats.MapData        `json:"map"`
}

func (s *Server) handleMatch(w http.ResponseWriter, r *http.Request, p *page) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.fail(w, r, notFound("Матч не найден."))
		return
	}
	m, err := stats.LoadMatch(ctx, s.opts.Store.Read, id)
	if errors.Is(err, stats.ErrNotFound) {
		err = notFound("Матч не найден: его нет среди матчей отслеживаемых аккаунтов.")
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d := matchPage{MatchDetail: m, MapBuildings: mapBuildings}
	if row, ok, err := stats.AccountMatch(ctx, s.opts.Store.Read, p.Profile.ID, id); err != nil {
		s.fail(w, r, err)
		return
	} else if ok {
		d.Mine = &row
		for _, pl := range append(append([]stats.PlayerRow{}, m.Radiant...), m.Dire...) {
			if pl.AccountID != nil && *pl.AccountID == p.Profile.ID {
				d.Me = &pl
			}
		}
	}
	if d.Analysis, err = stats.LoadMatchAnalysis(ctx, s.opts.Store.Read, &m, p.C); err != nil {
		s.fail(w, r, err)
		return
	}
	d.Charts.Advantage.Gold, d.Charts.Advantage.XP = m.GoldAdv, m.XPAdv
	d.Charts.WinChance, d.Charts.Markers, d.Charts.Map = d.Analysis.WinChance, d.Analysis.Markers, d.Analysis.Map
	for _, ps := range m.Series {
		ps.Name = p.C.HeroName(ps.HeroID) + " · " + ps.Name
		d.Charts.Players = append(d.Charts.Players, ps)
	}
	p.Title, p.Nav, p.Data = "Матч "+strconv.FormatInt(id, 10), "matches", d
	s.render(w, r, "match", "", http.StatusOK, p)
}

type heroesPage struct {
	Filter filterBar
	Rows   []stats.HeroRow
	Sort   string
}

func (s *Server) handleHeroes(w http.ResponseWriter, r *http.Request, p *page) {
	fb, err := s.filterBar(r, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sort := r.URL.Query().Get("sort")
	if _, ok := stats.HeroSorts[sort]; !ok {
		sort = "games"
	}
	d := heroesPage{Filter: fb, Sort: sort}
	if d.Rows, err = stats.Heroes(r.Context(), s.opts.Store.Read, fb.Filter, sort); err != nil {
		s.fail(w, r, err)
		return
	}
	p.Title, p.Nav, p.Data = "Герои", "heroes", d
	s.render(w, r, "heroes", "hero_table", http.StatusOK, p)
}

type heroPage struct {
	stats.HeroDetail
	HeroID int
	Lanes  stats.LaneRecord
	Filter filterBar
	Charts heroCharts
}

type heroCharts struct {
	Trend      []stats.TrendPoint `json:"trend"`
	Benchmarks struct {
		Metrics []string               `json:"metrics"`
		Points  []stats.BenchmarkPoint `json:"points"`
	} `json:"benchmarks"`
}

func (s *Server) handleHero(w http.ResponseWriter, r *http.Request, p *page) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if _, ok := p.C.Heroes[id]; err != nil || !ok {
		s.fail(w, r, notFound("Герой не найден."))
		return
	}
	fb, err := s.filterBar(r, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	fb.Filter.HeroID, fb.FixedHero = id, true
	d := heroPage{HeroID: id, Filter: fb}
	if d.HeroDetail, err = stats.LoadHeroDetail(r.Context(), s.opts.Store.Read, fb.Filter, p.C); err != nil {
		s.fail(w, r, err)
		return
	}
	if d.Lanes, err = stats.HeroLaneRecord(r.Context(), s.opts.Store.Read, p.Profile.ID, id); err != nil {
		s.fail(w, r, err)
		return
	}
	d.Charts.Trend = d.Games
	d.Charts.Benchmarks.Metrics, d.Charts.Benchmarks.Points = stats.BenchmarkMetrics, d.Benchmarks
	p.Title, p.Nav, p.Data = p.C.HeroName(id), "heroes", d
	s.render(w, r, "hero", "", http.StatusOK, p)
}

// filterBar is the shared filter bar: the parsed filter and the choices
// offered, limited to what the account actually played.
type filterBar struct {
	stats.Filter
	Options   stats.FilterOptions
	FixedHero bool // the page is about one hero
}

// ShowClear reports whether the user set any filter; a page's own hero
// doesn't count.
func (f filterBar) ShowClear() bool {
	if f.FixedHero {
		f.HeroID = 0
	}
	return f.Active()
}

func (s *Server) filterBar(r *http.Request, p *page) (filterBar, error) {
	opts, err := stats.LoadFilterOptions(r.Context(), s.opts.Store.Read, p.Profile.ID)
	return filterBar{Filter: stats.FilterFromQuery(r.URL.Query(), p.Profile.ID), Options: opts}, err
}
