package proxy

import (
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

const defaultIndexHTML = `<!DOCTYPE html>
<html><body>
<h1>Operetta Server</h1>
<form action="/fetch" method="get">
<h3>Fetch URL as OMS</h3>
URL: <input name="url" size="60"><br>
Action: <input name="action"><br>
Get: <input name="get"><br>
<button type="submit">Fetch</button>
</form>
</body></html>`

const defaultSitesDir = "config/sites"

// Bookmark represents a quick link shown in the local bookmarks portal.
type Bookmark struct {
	Title string
	URL   string
}

// BookmarkMode controls how the proxy handles portal requests.
type BookmarkMode int

const (
	BookmarkModeAuto BookmarkMode = iota
	BookmarkModeRemote
	BookmarkModeLocal
)

// Config describes server wiring and runtime behaviour.
type Config struct {
	IndexHTML    string
	Bookmarks    []Bookmark
	BookmarkMode BookmarkMode
	SitesDir     string
	Logger       *log.Logger
	Clock        func() time.Time
}

// DefaultConfig populates configuration from environment variables.
func DefaultConfig() Config {
	cfg := Config{
		IndexHTML: defaultIndexHTML,
		Logger:    log.Default(),
		Clock:     time.Now,
		SitesDir:  strings.TrimSpace(os.Getenv("OMS_SITES_DIR")),
	}
	if cfg.SitesDir == "" {
		cfg.SitesDir = defaultSitesDir
	}
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("OMS_BOOKMARKS_MODE")))
	switch mode {
	case "remote", "pass", "passthrough":
		cfg.BookmarkMode = BookmarkModeRemote
	case "local", "inline":
		cfg.BookmarkMode = BookmarkModeLocal
	default:
		cfg.BookmarkMode = BookmarkModeAuto
	}
	if raw := strings.TrimSpace(os.Getenv("OMS_BOOKMARKS")); raw != "" {
		cfg.Bookmarks = parseBookmarks(raw)
		if len(cfg.Bookmarks) > 0 && cfg.BookmarkMode == BookmarkModeRemote {
			// honour explicit remote request; ignore list.
			cfg.Bookmarks = nil
		}
	} else if cfg.BookmarkMode == BookmarkModeLocal {
		cfg.Bookmarks = parseBookmarks(defaultBookmarksSpec)
	}
	return cfg
}

const defaultBookmarksSpec = "Bookmarks|http://www.google.com/xhtml?client=ms-opera_mb_no&channel=bm"

func parseBookmarks(raw string) []Bookmark {
	items := []Bookmark{}
	parts := strings.Split(raw, ",")
	for _, part := range parts {
		kv := strings.SplitN(strings.TrimSpace(part), "|", 2)
		if len(kv) != 2 {
			continue
		}
		title := strings.TrimSpace(kv[0])
		url := strings.TrimSpace(kv[1])
		if title == "" || url == "" {
			continue
		}
		items = append(items, Bookmark{Title: title, URL: url})
	}
	return items
}

// Server exposes the HTTP handlers implementing the proxy behaviour.
type Server struct {
    cfg         Config
    mux         *http.ServeMux
    handler     http.Handler
    logger      *log.Logger
    renderPrefs *renderPrefStore
    cookieJars  *cookieJarStore
    auth        *authStore
    cache       *pageCache
    sites       *siteConfigStore
    clock       func() time.Time
}

// New wires a new proxy server with the provided configuration.
func New(cfg Config) *Server {
	if cfg.IndexHTML == "" {
		cfg.IndexHTML = defaultIndexHTML
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.SitesDir == "" {
		cfg.SitesDir = defaultSitesDir
	}
    s := &Server{
        cfg:         cfg,
        mux:         http.NewServeMux(),
        logger:      cfg.Logger,
        renderPrefs: newRenderPrefStore(),
        cookieJars:  newCookieJarStore(),
        auth:        newAuthStore(cfg.Clock),
        cache:       newPageCache(cfg.Clock),
        sites:       newSiteConfigStore(cfg.SitesDir),
        clock:       cfg.Clock,
    }
	s.registerRoutes()
	s.handler = withLogging(s.logger, s.mux)
	return s
}

// NewServer keeps backwards compatibility with the old factory signature.
func NewServer() http.Handler {
	return New(DefaultConfig())
}

// Handler exposes the HTTP handler with middleware applied.
func (s *Server) Handler() http.Handler { return s }

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("/", s.handleRoot)
	s.mux.HandleFunc("/fetch", s.handleFetch)
	s.mux.HandleFunc("/validate", s.handleValidate)
	s.mux.HandleFunc("/ping", s.handlePing)
}
