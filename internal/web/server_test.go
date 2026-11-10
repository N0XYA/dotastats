package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"dotastats/internal/collector"
	"dotastats/internal/opendota"
	"dotastats/internal/testseed"
)

func newServer(t testing.TB, e testseed.Env) *Server {
	t.Helper()
	s, err := New(Options{Store: e.Store, Collector: e.Collector, Version: "v-test", DataDir: e.DataDir})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// get requests path and returns the recorder; cookies are sent as given.
func get(t testing.TB, h http.Handler, path string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func post(t testing.TB, h http.Handler, path string, form url.Values, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestEmptyDatabaseGoesToSetup(t *testing.T) {
	e := testseed.Empty(t)
	h := newServer(t, e).Handler()

	for _, path := range []string{"/", "/matches", "/heroes/44", "/settings", "/status"} {
		if rec := get(t, h, path); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/setup" {
			t.Errorf("GET %s on an empty database: %d -> %q, want a redirect to /setup", path, rec.Code, rec.Header().Get("Location"))
		}
	}
	if rec := get(t, h, "/setup"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Добро пожаловать в DotaStats") {
		t.Fatalf("GET /setup: %d", rec.Code)
	}

	rec := post(t, h, "/setup", url.Values{"account": {"4294967290"}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Expose Public Match Data") {
		t.Errorf("unknown account: %d, want 422 with the hint", rec.Code)
	}
	rec = post(t, h, "/setup", url.Values{"account": {"https://www.dotabuff.com/players/337895673"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("setup with a valid account: %d -> %q", rec.Code, rec.Header().Get("Location"))
	}
	accounts, err := collector.ListAccounts(context.Background(), e.Store.Read)
	if err != nil || len(accounts) != 1 || accounts[0].Role != collector.RoleOwner || accounts[0].ID != testseed.Owner {
		t.Fatalf("after setup: %+v, %v", accounts, err)
	}
	if rec := get(t, h, "/setup"); rec.Code != http.StatusSeeOther {
		t.Errorf("setup after the owner exists: %d, want a redirect", rec.Code)
	}
	if rec := get(t, h, "/"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Матчей пока нет") {
		t.Errorf("overview before the first collection: %d", rec.Code)
	}
}

func TestPagesRender(t *testing.T) {
	h := newServer(t, testseed.Seed(t)).Handler()
	tests := []struct {
		path string
		want []string
	}{
		{"/", []string{"MayFeelThis", "Легенда 1", "Последние 3 матча", "Самые частые герои", "Привычки и повторяющиеся ошибки", "Смерти до 10:00"}},
		{"/matches", []string{"Phantom Assassin", "21 / 2 / 16", "запросить разбор", "разобран"}},
		{"/matches?hero=44&result=win", []string{"21 / 2 / 16", "Сбросить фильтры"}},
		{"/matches?page=2", []string{"Матчей нет."}},
		{"/matches/9017652399", []string{"16 / 6 / 6", "778", "29\u00a0794", "Силы Света", "advantage", "Золото и добивания игроков", "все покупки",
			"Ключевые моменты", "Первая кровь: Wraith King убивает Luna", "Линия проиграна против", "оценка", "Мастер контроля",
			`data-panel="timeline"`, `data-panel="map"`, "Линии к 10:00", "Драка до", "win_chance", `"kills":[`}},
		{"/matches/9056907605", []string{"21 / 2 / 16", "не разобран", "Zzz", "Реплей не разобран", "Для хронологии нужен разобранный реплей"}},
		{"/matches/2324742574", []string{"18 / 13 / 22", "не разобран"}},
		{"/heroes", []string{"Phantom Assassin", "Винрейт"}},
		{"/heroes?sort=wr", []string{`class="num sorted"`}},
		{"/heroes/48", []string{"Luna", "Тайминги ключевых предметов", "hero-data", "Линии к 10:00"}},
		{"/heroes/44", []string{"Phantom Assassin", "С союзниками"}},
		{"/settings", []string{"Отслеживаемые аккаунты", "Zzz", "Дневной лимит запросов", "Очистить сейчас"}},
		{"/status", []string{"Сбор данных", "Запросы к API сегодня", "Очередь"}},
		{"/static/app.js", []string{"builders"}},
		{"/static/vendor/htmx-2.0.11.min.js", []string{"htmx"}},
		{"/static/vendor/chart-4.5.1.umd.min.js", []string{"Chart"}},
	}
	for _, tt := range tests {
		rec := get(t, h, tt.path)
		if rec.Code != 200 {
			t.Errorf("GET %s: status %d\n%s", tt.path, rec.Code, rec.Body.String())
			continue
		}
		for _, w := range tt.want {
			if !strings.Contains(rec.Body.String(), w) {
				t.Errorf("GET %s: body lacks %q", tt.path, w)
			}
		}
	}
	for _, path := range []string{"/matches/1", "/heroes/9999", "/matches/abc"} {
		if rec := get(t, h, path); rec.Code != 404 {
			t.Errorf("GET %s: %d, want 404", path, rec.Code)
		}
	}
}

// Every scoreboard row must show what OpenDota returned for the match:
// K/D/A, GPM/XPM, last hits and the six item slots, for all ten players.
func TestScoreboardMatchesRecordedData(t *testing.T) {
	h := newServer(t, testseed.Seed(t)).Handler()
	for _, file := range []string{"match_9017652399_parsed.json", "match_9056907605_unparsed.json"} {
		raw, err := os.ReadFile(filepath.Join(testseed.Dir(), file))
		if err != nil {
			t.Fatal(err)
		}
		var m opendota.Match
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		page := get(t, h, "/matches/"+strconv.FormatInt(m.MatchID, 10)).Body.String()
		body := strings.Join(regexp.MustCompile(`(?s)<section class="card scoreboard.*?</section>`).FindAllString(page, -1), "")
		rows := regexp.MustCompile(`(?s)<tr( class="tracked")?>\s*<td>\s*<span class="hero">.*?</tr>`).FindAllString(body, -1)
		if len(rows) != 10 {
			t.Fatalf("%s: %d scoreboard rows, want 10", file, len(rows))
		}
		for _, p := range m.Players {
			want := []string{
				fmt.Sprintf("%d / %d / %d", p.Kills, p.Deaths, p.Assists),
				fmt.Sprintf("%s / %s", thousands(p.GoldPerMin), thousands(p.XPPerMin)),
				fmt.Sprintf("%s / %d", thousands(p.LastHits), p.Denies),
			}
			for _, item := range []int{p.Item0, p.Item1, p.Item2, p.Item3, p.Item4, p.Item5} {
				if item != 0 {
					want = append(want, fmt.Sprintf("/assets/items/%d.png", item))
				}
			}
			found := false
			for _, row := range rows {
				ok := strings.Contains(row, fmt.Sprintf("/assets/heroes/%d.png", p.HeroID))
				for _, w := range want {
					ok = ok && strings.Contains(row, w)
				}
				found = found || ok
			}
			if !found {
				t.Errorf("%s: no scoreboard row for hero %d with %v", file, p.HeroID, want)
			}
		}
	}
}

func TestProfileSwitch(t *testing.T) {
	h := newServer(t, testseed.Seed(t)).Handler()
	rec := get(t, h, "/matches?account=171913321")
	cookie := rec.Result().Cookies()
	if len(cookie) != 1 || cookie[0].Value != "171913321" {
		t.Fatalf("profile cookie = %v", cookie)
	}
	friend := rec.Body.String()
	if strings.Count(friend, `class="num kda"`) != 1 || !strings.Contains(friend, "Outworld Devourer") {
		t.Errorf("friend's matches page should list their one match (Outworld Devourer)")
	}
	// The cookie carries the choice to every page.
	for _, path := range []string{"/", "/heroes", "/matches"} {
		body := get(t, h, path, "Cookie", "profile=171913321").Body.String()
		if strings.Contains(body, "Phantom Assassin") {
			t.Errorf("%s with the friend's profile shows the owner's hero", path)
		}
	}
	if body := get(t, h, "/heroes").Body.String(); !strings.Contains(body, "Phantom Assassin") {
		t.Error("without a cookie the owner's heroes are shown")
	}
}

func TestHTMXPartials(t *testing.T) {
	h := newServer(t, testseed.Seed(t)).Handler()
	for _, path := range []string{"/matches?result=win", "/heroes?sort=kda"} {
		body := get(t, h, path, "HX-Request", "true").Body.String()
		if !strings.HasPrefix(strings.TrimSpace(body), `<div id="results">`) || strings.Contains(body, "<html") {
			t.Errorf("htmx GET %s returned more than the results block", path)
		}
	}
}

func TestActions(t *testing.T) {
	e := testseed.Seed(t)
	h := newServer(t, e).Handler()

	rec := post(t, h, "/matches/2324742574/parse", nil, "HX-Request", "true")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "pending") {
		t.Errorf("request parse: %d %s", rec.Code, rec.Body.String())
	}
	var state string
	e.Store.Read.QueryRow("SELECT parse_state FROM matches WHERE match_id = 2324742574").Scan(&state)
	if state != "pending" {
		t.Errorf("parse state after the button = %q", state)
	}

	form := url.Values{}
	for k, v := range map[string]string{
		"daily_budget": "2000", "collect_interval_minutes": "20", "raw_retention_days": "30",
		"detail_retention_days": "-1", "match_retention_days": "-1", "recent_days": "14", "backfill_budget_pct": "40",
	} {
		form.Set(k, v)
	}
	if rec := post(t, h, "/settings", form); rec.Code != http.StatusSeeOther {
		t.Fatalf("save settings: %d %s", rec.Code, rec.Body.String())
	}
	s, _ := collector.LoadSettings(context.Background(), e.Store.Read)
	if s.DailyBudget != 2000 || s.RecentDays != 14 || s.RawRetentionDays != 30 {
		t.Errorf("settings after save: %+v", s)
	}
	form.Set("backfill_budget_pct", "150")
	if rec := post(t, h, "/settings", form); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "допустимо от 0 до 100") {
		t.Errorf("invalid setting: %d", rec.Code)
	}

	if rec := post(t, h, "/accounts/171913321/pause", nil); rec.Code != http.StatusSeeOther {
		t.Errorf("pause: %d", rec.Code)
	}
	if rec := post(t, h, "/accounts/337895673/remove", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("removing the owner with friends: %d, want 400", rec.Code)
	}
	if rec := post(t, h, "/prune", nil); rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "done=pruned") {
		t.Errorf("prune now: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if rec := post(t, h, "/accounts/171913321/remove", nil); rec.Code != http.StatusSeeOther {
		t.Errorf("remove friend: %d", rec.Code)
	}
}

func TestCrossSitePostsAreRefused(t *testing.T) {
	h := newServer(t, testseed.Seed(t)).Handler()
	rec := post(t, h, "/accounts/171913321/remove", nil, "Sec-Fetch-Site", "cross-site")
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-site POST: %d, want 403", rec.Code)
	}
}

func TestListenSkipsBusyPort(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	busyPort := busy.Addr().(*net.TCPAddr).Port

	ln, err := listen(busy.Addr().String(), portTries)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := ln.Addr().(*net.TCPAddr).Port
	if got <= busyPort || got >= busyPort+portTries {
		t.Errorf("listened on port %d, want one in (%d, %d)", got, busyPort, busyPort+portTries)
	}
}

func TestListenGivesUpWithFirstError(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	if ln, err := listen(busy.Addr().String(), 1); err == nil {
		ln.Close()
		t.Fatal("listen with one try on a busy port succeeded, want error")
	}
}

func TestServeShutsDownOnCancel(t *testing.T) {
	h := newServer(t, testseed.Seed(t)).Handler()
	ctx, cancel := context.WithCancel(context.Background())
	urls := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, "127.0.0.1:0", h, func(url string) { urls <- url })
	}()

	url := <-urls
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "MayFeelThis") {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v, want nil after cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
}
