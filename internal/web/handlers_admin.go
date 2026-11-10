package web

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"dotastats/internal/collector"
	"dotastats/internal/config"
	"dotastats/internal/stats"
)

type setupPage struct {
	Input string
	Error string
	Hint  bool // show how to expose match data
}

// handleSetup is the first-run page: the account entered becomes the
// owner. It is only reachable while no account is tracked.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accounts, err := collector.ListAccounts(ctx, s.opts.Store.Read)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(accounts) > 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	p := &page{Title: "Настройка", Version: s.opts.Version, Asset: s.asset, Path: r.URL.Path}
	d := setupPage{}
	if r.Method == http.MethodPost {
		d.Input = strings.TrimSpace(r.FormValue("account"))
		res, err := s.addAccount(r, d.Input, "")
		if err == nil {
			s.trigger()
			dest := "/"
			if res.NoMatches {
				dest = "/settings?done=added&nomatches=1"
			}
			http.Redirect(w, r, dest, http.StatusSeeOther)
			return
		}
		d.Error, d.Hint = userError(err), errors.Is(err, collector.ErrNotFound)
	}
	p.Data = d
	status := http.StatusOK
	if d.Error != "" {
		status = http.StatusUnprocessableEntity
	}
	s.render(w, r, "setup", "", status, p)
}

func (s *Server) addAccount(r *http.Request, input, label string) (collector.AddResult, error) {
	if s.opts.Collector == nil {
		return collector.AddResult{}, errors.New("в этом режиме добавлять аккаунты нельзя")
	}
	if input == "" {
		return collector.AddResult{}, errEmptyAccount
	}
	return s.opts.Collector.AddAccount(r.Context(), input, label)
}

var errEmptyAccount = errors.New("empty account")

// userError explains an account or settings error in Russian.
func userError(err error) string {
	var re *collector.RangeError
	switch {
	case errors.As(err, &re):
		return fmt.Sprintf("«%s»: допустимо от %s до %s.", settingLabels[re.Key], thousands(re.Min), thousands(re.Max))
	case errors.Is(err, errEmptyAccount):
		return "Введите ID аккаунта, SteamID64 или ссылку на профиль."
	case errors.Is(err, collector.ErrNotFound):
		return "Такого аккаунта нет на OpenDota."
	case errors.Is(err, collector.ErrAccountExists):
		return "Этот аккаунт уже отслеживается."
	case errors.Is(err, collector.ErrSteamCustomURL):
		return "Ссылки Steam с ником (steamcommunity.com/id/…) пока не поддерживаются. Используйте Friend ID из Dota 2 или ссылку вида steamcommunity.com/profiles/<число>."
	case errors.Is(err, collector.ErrUnsupportedSite):
		return "Нужна ссылка на профиль OpenDota, Dotabuff, Stratz или Steam."
	case errors.Is(err, collector.ErrNoIDInLink):
		return "В ссылке не нашлось ID аккаунта."
	case errors.Is(err, collector.ErrNotAccountOrLink):
		return "Это не похоже на ID аккаунта, SteamID64 или ссылку на профиль."
	case errors.Is(err, collector.ErrOwnerHasFriends):
		return "Владельца нельзя удалить, пока отслеживаются друзья: сначала удалите их."
	}
	return "Не получилось: " + err.Error()
}

// settingLabels name the settings as the form does.
var settingLabels = map[string]string{
	collector.KeyDailyBudget:         "Дневной лимит запросов",
	collector.KeyIntervalMinutes:     "Сбор каждые (минут)",
	collector.KeyRecentDays:          "Свежие матчи (дней)",
	collector.KeyBackfillBudgetPct:   "Доля лимита на старые матчи (%)",
	collector.KeyRawRetentionDays:    "Хранить исходные данные (дней)",
	collector.KeyDetailRetentionDays: "Хранить графики, покупки и варды (дней)",
	collector.KeyMatchRetentionDays:  "Хранить матчи (дней)",
}

func (s *Server) trigger() {
	if s.opts.Scheduler != nil {
		s.opts.Scheduler.Trigger()
	}
}

type settingsPage struct {
	Settings   collector.Settings
	Defaults   collector.Settings
	Error      string
	AddInput   string
	AddLabel   string
	AddError   string
	AddHint    bool
	Done       string // last action, from the redirect
	DoneCount  string
	NoMatches  bool
	DBSize     int64
	CallsToday int
	Calls      []stats.AccountCalls
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request, p *page) {
	s.renderSettings(w, r, p, settingsPage{}, http.StatusOK)
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, p *page, d settingsPage, status int) {
	ctx := r.Context()
	if d.Settings == (collector.Settings{}) {
		var err error
		if d.Settings, err = collector.LoadSettings(ctx, s.opts.Store.Read); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	st, err := stats.LoadStatus(ctx, s.opts.Store.Read, time.Now())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	d.Defaults, d.CallsToday, d.Calls = collector.DefaultSettings, st.CallsToday, st.CallsByAccount
	if d.Done == "" {
		d.Done, d.DoneCount, d.NoMatches = q.Get("done"), q.Get("n"), q.Get("nomatches") == "1"
		if n, err := strconv.ParseInt(d.DoneCount, 10, 64); err == nil && d.Done == "pruned" {
			d.DoneCount = formatBytes(n)
		}
	}
	d.DBSize = s.dbSize()
	p.Title, p.Nav, p.Data = "Аккаунты и настройки", "settings", d
	s.render(w, r, "settings", "", status, p)
}

func (s *Server) handleSaveSettings(w http.ResponseWriter, r *http.Request, p *page) {
	set := collector.Settings{}
	fields := []struct {
		key string
		dst *int
	}{
		{collector.KeyDailyBudget, &set.DailyBudget},
		{collector.KeyIntervalMinutes, &set.IntervalMinutes},
		{collector.KeyRawRetentionDays, &set.RawRetentionDays},
		{collector.KeyDetailRetentionDays, &set.DetailRetentionDays},
		{collector.KeyMatchRetentionDays, &set.MatchRetentionDays},
		{collector.KeyRecentDays, &set.RecentDays},
		{collector.KeyBackfillBudgetPct, &set.BackfillBudgetPct},
	}
	for _, f := range fields {
		n, err := strconv.Atoi(strings.TrimSpace(r.FormValue(f.key)))
		if err != nil {
			s.renderSettings(w, r, p, settingsPage{Settings: set, Error: "«" + settingLabels[f.key] + "»: нужно целое число."}, http.StatusUnprocessableEntity)
			return
		}
		*f.dst = n
	}
	if err := collector.SaveSettings(r.Context(), s.opts.Store.Write, set); err != nil {
		s.renderSettings(w, r, p, settingsPage{Settings: set, Error: userError(err)}, http.StatusUnprocessableEntity)
		return
	}
	http.Redirect(w, r, "/settings?done=saved", http.StatusSeeOther)
}

func (s *Server) handleAddAccount(w http.ResponseWriter, r *http.Request, p *page) {
	input, label := strings.TrimSpace(r.FormValue("account")), strings.TrimSpace(r.FormValue("label"))
	res, err := s.addAccount(r, input, label)
	if err != nil {
		s.renderSettings(w, r, p, settingsPage{
			AddInput: input, AddLabel: label, AddError: userError(err), AddHint: errors.Is(err, collector.ErrNotFound),
		}, http.StatusUnprocessableEntity)
		return
	}
	s.trigger()
	dest := "/settings?done=added"
	if res.NoMatches {
		dest += "&nomatches=1"
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// handleAccountAction pauses, resumes or removes an account.
func (s *Server) handleAccountAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.fail(w, r, notFound("Аккаунт не найден."))
		return
	}
	dest := "/settings?done=" + r.PathValue("action")
	switch r.PathValue("action") {
	case "pause", "resume":
		err = collector.SetAccountActive(ctx, s.opts.Store.Write, id, r.PathValue("action") == "resume")
	case "remove":
		var deleted int64
		err = s.exclusive(func() error {
			var err error
			deleted, err = collector.RemoveAccount(ctx, s.opts.Store.Write, id)
			return err
		})
		dest += "&n=" + strconv.FormatInt(deleted, 10)
	default:
		err = notFound("Такого действия нет.")
	}
	switch {
	case errors.Is(err, collector.ErrNoSuchAccount):
		err = notFound("Аккаунт не найден.")
	case errors.Is(err, collector.ErrOwnerHasFriends):
		err = badRequest(userError(err))
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// exclusive runs fn between collector cycles when the scheduler runs.
func (s *Server) exclusive(fn func() error) error {
	if s.opts.Scheduler == nil {
		return fn()
	}
	return s.opts.Scheduler.Exclusive(fn)
}

func (s *Server) handlePrune(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var rep collector.PruneReport
	err := s.exclusive(func() error {
		set, err := collector.LoadSettings(ctx, s.opts.Store.Read)
		if err != nil {
			return err
		}
		rep, err = collector.Prune(ctx, s.opts.Store.Write, set, time.Now())
		return err
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings?done=pruned&n="+strconv.FormatInt(rep.FreedBytes, 10), http.StatusSeeOther)
}

type statusPage struct {
	stats.Status
	Running     bool
	Scheduler   bool
	DailyBudget int
	DBSize      int64
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request, p *page) {
	ctx := r.Context()
	st, err := stats.LoadStatus(ctx, s.opts.Store.Read, time.Now())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	set, err := collector.LoadSettings(ctx, s.opts.Store.Read)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d := statusPage{Status: st, Scheduler: s.opts.Scheduler != nil, DailyBudget: set.DailyBudget, DBSize: s.dbSize()}
	if s.opts.Scheduler != nil {
		d.Running = s.opts.Scheduler.Running()
	}
	p.Title, p.Nav, p.Data = "Статус", "status", d
	s.render(w, r, "status", "", http.StatusOK, p)
}

// handleRefresh is the "Refresh now" button: start a cycle soon.
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	s.trigger()
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<button class="refresh" disabled>Обновление запущено</button>`))
		return
	}
	http.Redirect(w, r, "/status", http.StatusSeeOther)
}

// handleRequestParse queues a parse of one match and starts a cycle; the
// response replaces the match's parse badge.
func (s *Server) handleRequestParse(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.fail(w, r, notFound("Матч не найден."))
		return
	}
	if err := collector.QueueParse(r.Context(), s.opts.Store.Write, id, time.Now()); err != nil {
		s.fail(w, r, badRequest(err.Error()))
		return
	}
	s.trigger()
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<span class="badge pending" title="Разбор запрошен">в очереди</span>`))
		return
	}
	http.Redirect(w, r, "/matches/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// dbSize is the database file plus its write-ahead log.
func (s *Server) dbSize() int64 {
	var n int64
	for _, suffix := range []string{"", "-wal"} {
		if fi, err := os.Stat(config.DBPath(s.opts.DataDir) + suffix); err == nil {
			n += fi.Size()
		}
	}
	return n
}
