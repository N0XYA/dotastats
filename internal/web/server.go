// Package web serves the server-rendered UI from embedded templates and
// static files. Handlers stay thin: queries live in stats, actions in
// collector, and no handler calls OpenDota while rendering a page.
package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"dotastats/internal/collector"
	"dotastats/internal/db"
	"dotastats/internal/stats"
)

var (
	//go:embed templates
	templateFiles embed.FS
	//go:embed static
	staticFiles embed.FS
)

// Options are what the server reads from and acts on.
type Options struct {
	Store *db.DB
	// Collector adds accounts; Scheduler runs cycles and "Prune now". Either
	// may be nil (read-only tests), which disables those actions.
	Collector *collector.Collector
	Scheduler *collector.Scheduler
	Version   string
	DataDir   string
}

// Server renders the pages.
type Server struct {
	opts  Options
	pages map[string]*template.Template
	// asset is a hash of the embedded static files. It goes into their
	// URLs, so browsers fetch new CSS and JS after every build even though
	// static files are cached.
	asset string

	mu            sync.Mutex
	consts        *stats.Constants
	constsVersion string // collector_state constants_synced_at when loaded
	iconsVersion  int    // downloaded icons when loaded
}

// New parses the embedded templates.
func New(opts Options) (*Server, error) {
	s := &Server{opts: opts, pages: map[string]*template.Template{}}
	var err error
	if s.asset, err = staticHash(); err != nil {
		return nil, err
	}
	names, err := fs.Glob(templateFiles, "templates/pages/*.html")
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		page := path.Base(name[:len(name)-len(".html")])
		t, err := template.New(page).Funcs(funcs).ParseFS(templateFiles,
			"templates/base.html", "templates/partials.html", name)
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", page, err)
		}
		s.pages[page] = t
	}
	return s, nil
}

// Handler returns the routes. State-changing requests from other sites are
// refused (Sec-Fetch-Site / Origin checks), since the app has no login.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.withProfile(s.handleOverview))
	mux.HandleFunc("GET /matches", s.withProfile(s.handleMatches))
	mux.HandleFunc("GET /matches/{id}", s.withProfile(s.handleMatch))
	mux.HandleFunc("POST /matches/{id}/parse", s.handleRequestParse)
	mux.HandleFunc("GET /heroes", s.withProfile(s.handleHeroes))
	mux.HandleFunc("GET /heroes/{id}", s.withProfile(s.handleHero))
	mux.HandleFunc("GET /settings", s.withProfile(s.handleSettings))
	mux.HandleFunc("POST /settings", s.withProfile(s.handleSaveSettings))
	mux.HandleFunc("POST /accounts", s.withProfile(s.handleAddAccount))
	mux.HandleFunc("POST /accounts/{id}/{action}", s.handleAccountAction)
	mux.HandleFunc("POST /prune", s.handlePrune)
	mux.HandleFunc("GET /status", s.withProfile(s.handleStatus))
	mux.HandleFunc("POST /refresh", s.handleRefresh)
	mux.HandleFunc("GET /setup", s.handleSetup)
	mux.HandleFunc("POST /setup", s.handleSetup)
	// Requests for /static/x map to static/x inside the embedded FS.
	mux.Handle("GET /static/", cacheFor(24*time.Hour, http.FileServerFS(staticFiles)))
	mux.Handle("GET /assets/", cacheFor(24*time.Hour, http.StripPrefix("/assets/",
		http.FileServer(http.Dir(filepath.Join(s.opts.DataDir, "assets"))))))
	return http.NewCrossOriginProtection().Handler(mux)
}

func cacheFor(d time.Duration, h http.Handler) http.Handler {
	v := "public, max-age=" + strconv.Itoa(int(d.Seconds()))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", v)
		h.ServeHTTP(w, r)
	})
}

// staticHash hashes every embedded static file.
func staticHash() (string, error) {
	h := sha256.New()
	err := fs.WalkDir(staticFiles, "static", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := staticFiles.ReadFile(name)
		h.Write([]byte(name))
		h.Write(b)
		return err
	})
	return hex.EncodeToString(h.Sum(nil))[:12], err
}

// page is what every page template gets.
type page struct {
	Title    string
	Nav      string // highlighted nav item
	Path     string
	Version  string
	Asset    string // static files' hash for cache busting
	Accounts []collector.Account
	Profile  collector.Account // the active account
	C        *stats.Constants
	Query    url.Values
	Data     any
}

// withProfile loads the tracked accounts and the active profile for a
// page, sending first-time visitors to Setup.
func (s *Server) withProfile(h func(http.ResponseWriter, *http.Request, *page)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		accounts, err := collector.ListAccounts(ctx, s.opts.Store.Read)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if len(accounts) == 0 {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		consts, err := s.constants(ctx)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		p := &page{
			Path: r.URL.Path, Version: s.opts.Version, Asset: s.asset, Accounts: accounts,
			Profile: s.profile(w, r, accounts), C: consts, Query: r.URL.Query(),
		}
		h(w, r, p)
	}
}

// profileCookie remembers the active profile between pages.
const profileCookie = "profile"

// profile picks the active account: ?account= (remembered in a cookie),
// then the cookie, then the owner.
func (s *Server) profile(w http.ResponseWriter, r *http.Request, accounts []collector.Account) collector.Account {
	find := func(v string) (collector.Account, bool) {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return collector.Account{}, false
		}
		for _, a := range accounts {
			if a.ID == id {
				return a, true
			}
		}
		return collector.Account{}, false
	}
	if a, ok := find(r.URL.Query().Get("account")); ok {
		http.SetCookie(w, &http.Cookie{
			Name: profileCookie, Value: strconv.FormatInt(a.ID, 10), Path: "/",
			MaxAge: 365 * 24 * 3600, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		})
		return a
	}
	if c, err := r.Cookie(profileCookie); err == nil {
		if a, ok := find(c.Value); ok {
			return a
		}
	}
	return accounts[0] // ListAccounts puts the owner first
}

// constants returns the cached lookup tables, reloading them when the
// collector has refreshed constants or downloaded more icons since.
func (s *Server) constants(ctx context.Context) (*stats.Constants, error) {
	var version string
	var icons int
	if err := s.opts.Store.Read.QueryRowContext(ctx, `SELECT
			coalesce((SELECT value FROM collector_state WHERE key = 'constants_synced_at'), ''),
			(SELECT count(img_path) FROM items) + (SELECT count(img_path) FROM heroes)`).Scan(&version, &icons); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.consts != nil && version == s.constsVersion && icons == s.iconsVersion {
		return s.consts, nil
	}
	c, err := stats.LoadConstants(ctx, s.opts.Store.Read)
	if err != nil {
		return nil, err
	}
	s.consts, s.constsVersion, s.iconsVersion = c, version, icons
	return c, nil
}

// render executes a page, or only its named block for an htmx request
// when block is set. Output is buffered so a template error becomes a 500
// instead of half a page.
func (s *Server) render(w http.ResponseWriter, r *http.Request, name, block string, status int, p *page) {
	t, ok := s.pages[name]
	if !ok {
		s.fail(w, r, fmt.Errorf("no template %q", name))
		return
	}
	entry := "base"
	if block != "" && r.Header.Get("HX-Request") == "true" {
		entry = block
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, entry, p); err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

// fail logs err and shows the error page.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	msg := "Что-то пошло не так. Подробности в журнале программы (окно терминала)."
	var he httpError
	if errors.As(err, &he) {
		status, msg = he.status, he.msg
	} else {
		slog.Error("request failed", "path", r.URL.Path, "err", err)
	}
	t := s.pages["error"]
	var buf bytes.Buffer
	if t == nil || t.ExecuteTemplate(&buf, "base", &page{Title: statusTitle(status), Version: s.opts.Version, Asset: s.asset, Data: msg}) != nil {
		http.Error(w, msg, status)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

// httpError is an error shown to the user as is, with its status.
type httpError struct {
	status int
	msg    string
}

func (e httpError) Error() string { return e.msg }

// notFound is a 404 with a Russian message such as "Матч не найден.".
func notFound(msg string) error { return httpError{http.StatusNotFound, msg} }

func badRequest(msg string) error { return httpError{http.StatusBadRequest, msg} }

func statusTitle(status int) string {
	switch status {
	case http.StatusNotFound:
		return "Не найдено"
	case http.StatusBadRequest:
		return "Неверный запрос"
	}
	return "Ошибка"
}

// portTries is how many consecutive ports listen tries when addr is busy.
const portTries = 10

// Serve listens on addr (or the next free port) and serves h until ctx is
// cancelled, then shuts down gracefully. onReady is called with the page
// URL once the listener is bound, so the browser is never opened before
// the server can answer.
func Serve(ctx context.Context, addr string, h http.Handler, onReady func(url string)) error {
	ln, err := listen(addr, portTries)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	if onReady != nil {
		onReady("http://" + ln.Addr().String() + "/")
	}

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// listen binds addr, moving up one port at a time while the port is taken.
// Any listen error counts as "taken": the reason codes differ per OS, and
// if the host itself is wrong every try fails and the first error is
// returned anyway.
func listen(addr string, tries int) (net.Listener, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("invalid port in %q", addr)
	}
	ln, firstErr := net.Listen("tcp", addr)
	if firstErr == nil || port == 0 {
		return ln, firstErr
	}
	for p := port + 1; p < port+tries && p <= 65535; p++ {
		if ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(p))); err == nil {
			slog.Warn("port busy, using the next free one", "wanted", port, "using", p)
			return ln, nil
		}
	}
	return nil, firstErr
}

// OpenBrowser opens url in the user's default browser without waiting for it.
func OpenBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reap the launcher in the background so it doesn't linger as a zombie.
	go cmd.Wait()
	return nil
}
