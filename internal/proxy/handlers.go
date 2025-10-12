package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"operetta/oms"
)

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		params := parseNullKV(body)
		if raw := params["u"]; raw != "" {
			target := normalizeObmlURL(raw)
			hdr := s.headersFromParams(r, params)
			opt := s.renderOptionsFromParams(r, params, hdr)
			if s.isInternalAboutRequest(raw, target) {
				page := s.renderAboutPage(params)
				s.writeOMS(w, page.Data, page.SetCookies)
				return
			}
			if s.shouldServeLocalBookmarks() && looksLikeBookmarksPortal(target) {
				if page := s.renderLocalBookmarks(params["c"], params["h"], opt); page != nil {
					page.Normalize()
					s.writeOMS(w, page.Data, page.SetCookies)
					return
				}
			}
			s.renderPrefs.Remember(s.renderPrefKey(r, params["u"]), opt)
			cacheHit := s.serveFromCache(w, target, opt)
			if cacheHit {
				return
			}
			page, err := s.loadPage(target, hdr, opt)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			for _, sc := range page.SetCookies {
				w.Header().Add("Set-Cookie", sc)
			}
			page.Normalize()
			s.cache.Store(target, opt, hdr, page)
			s.writeOMS(w, page.Data, page.SetCookies)
			return
		}
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.Header().Set("Connection", "close")
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(s.cfg.IndexHTML)))
	w.Header().Set("Connection", "close")
	io.WriteString(w, s.cfg.IndexHTML)
}

func (s *Server) handleFetch(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	base := firstNonEmpty(r.FormValue("url"), r.URL.Query().Get("url"))
	if base != "" {
		base = urlDecode(urlDecode(base))
	}
	if base == "" {
		http.Error(w, "missing url", http.StatusBadRequest)
		return
	}
	action := firstNonEmpty(r.FormValue("action"), r.URL.Query().Get("action"))
	get := firstNonEmpty(r.FormValue("get"), r.URL.Query().Get("get"))
	finalURL := buildURL(base, action, get)
	s.logger.Printf("IN %s %s from %s | action=%q get=%q -> final=%s", r.Method, r.URL.String(), r.RemoteAddr, action, get, finalURL)

	hdr := s.headersFromQuery(r)
	opt := s.renderOptionsFromQuery(r, hdr)
	s.renderPrefs.Remember(s.renderPrefKey(r, finalURL), opt)
	if s.serveFromCache(w, finalURL, opt) {
		return
	}
	page, err := s.loadPage(finalURL, hdr, opt)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	for _, sc := range page.SetCookies {
		w.Header().Add("Set-Cookie", sc)
	}
	page.Normalize()
	s.cache.Store(finalURL, opt, hdr, page)
	s.writeOMS(w, page.Data, page.SetCookies)
}

func (s *Server) handleValidate(w http.ResponseWriter, r *http.Request) {
	u := r.URL.Query().Get("url")
	if u == "" {
		http.Error(w, "missing url", http.StatusBadRequest)
		return
	}
	hdr := http.Header{}
	if ua := r.URL.Query().Get("ua"); ua != "" {
		hdr.Set("User-Agent", ua)
	}
	if lang := r.URL.Query().Get("lang"); lang != "" {
		hdr.Set("Accept-Language", lang)
	}
	pageFull, err := oms.LoadPageWithHeaders(u, hdr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	pageFull.Normalize()
	aFull := analyzeOMS(pageFull.Data)
	pageCompact, err := oms.LoadCompactPageWithHeaders(u, hdr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	aCompact := analyzeOMS(pageCompact.Data)
	res := validateResult{URL: u, Full: aFull, Compact: aCompact}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(res)
}

func (s *Server) handlePing(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Connection", "close")
	io.WriteString(w, "pong\n")
}

func (s *Server) headersFromParams(r *http.Request, params map[string]string) http.Header {
	hdr := http.Header{}
	if ua := params["i"]; ua != "" {
		hdr.Set("User-Agent", ua)
	} else if v := params["v"]; v != "" {
		hdr.Set("User-Agent", v)
	}
	if lang := firstNonEmpty(firstNonEmpty(params["q"], params["y"]), params["D"]); lang != "" {
		hdr.Set("Accept-Language", lang)
	}
	if ck := r.Header.Get("Cookie"); ck != "" {
		hdr.Set("Cookie", ck)
	}
	if ref := params["f"]; ref != "" {
		hdr.Set("Referer", ref)
	}
	hdr.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
	return hdr
}

func (s *Server) headersFromQuery(r *http.Request) http.Header {
	hdr := http.Header{}
	if ua := r.URL.Query().Get("ua"); ua != "" {
		hdr.Set("User-Agent", ua)
	}
	if lang := r.URL.Query().Get("lang"); lang != "" {
		hdr.Set("Accept-Language", lang)
	}
	if ck := r.Header.Get("Cookie"); ck != "" {
		hdr.Set("Cookie", ck)
	}
	return hdr
}

func (s *Server) renderOptionsFromParams(r *http.Request, params map[string]string, hdr http.Header) *oms.RenderOptions {
	opt := defaultRenderOptions()
	if km := params["k"]; strings.HasPrefix(strings.ToLower(km), "image/") {
		opt.ImageMIME = km
	}
	opt.Compression = oms.CompressionFromParam(params["e"])
	if dv := params["d"]; dv != "" {
		for _, part := range strings.Split(dv, ";") {
			kv := strings.SplitN(strings.TrimSpace(part), ":", 2)
			if len(kv) != 2 {
				continue
			}
			key := strings.TrimSpace(kv[0])
			val := strings.TrimSpace(kv[1])
			switch key {
			case "i":
				opt.ImagesOn = val == "1"
			case "q":
				opt.HighQuality = val == "1"
			case "w":
				if n, err := strconv.Atoi(val); err == nil && n > 0 {
					opt.ScreenW = n
				}
			case "h":
				if n, err := strconv.Atoi(val); err == nil && n > 0 {
					opt.ScreenH = n
				}
			case "c":
				if n, err := strconv.Atoi(val); err == nil && n > 0 {
					opt.NumColors = n
				}
			case "m":
				if n, err := strconv.Atoi(val); err == nil && n >= 0 {
					opt.HeapBytes = n
				}
			case "l":
				if n, err := strconv.Atoi(val); err == nil && n >= 0 {
					opt.AlphaLevels = n
				}
			}
		}
	}
	opt.AuthCode = params["c"]
	opt.AuthPrefix = params["h"]
	if form := strings.TrimSpace(params["j"]); form != "" {
		opt.FormBody = form
	}
	if gv := strings.TrimSpace(params["o"]); gv != "" {
		if n, err := strconv.Atoi(gv); err == nil && n > 0 {
			opt.GatewayVersion = n
		}
	}
	opt.ClientVersion = oms.ClientVersionFromGateway(opt.GatewayVersion)
	if v := strings.TrimSpace(params["version"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			switch n {
			case 1:
				opt.ClientVersion = oms.ClientVersion1
			case 3:
				opt.ClientVersion = oms.ClientVersion3
			case 2:
				opt.ClientVersion = oms.ClientVersion2
			}
		}
	}
	if wv := strings.TrimSpace(params["w"]); wv != "" {
		seg := strings.SplitN(wv, ";", 2)
		if len(seg) >= 1 {
			if n, err := strconv.Atoi(strings.TrimSpace(seg[0])); err == nil && n > 0 {
				opt.Page = n
			}
		}
	}
	opt.ServerBase = serverBase(r)
	opt.ReqHeaders = hdr
	opt.Referrer = params["u"]
	opt.Jar = s.cookieJars.Get(deriveClientKey(r))
	opt.WantFullCache = true
	return opt
}

func (s *Server) renderOptionsFromQuery(r *http.Request, hdr http.Header) *oms.RenderOptions {
	q := r.URL.Query()
	opt := defaultRenderOptions()
	if q.Get("img") == "1" {
		opt.ImagesOn = true
	}
	if q.Get("hq") == "1" {
		opt.HighQuality = true
	}
	if v := strings.TrimSpace(q.Get("mime")); strings.HasPrefix(strings.ToLower(v), "image/") {
		opt.ImageMIME = v
	}
	if v := strings.TrimSpace(q.Get("e")); v != "" {
		opt.Compression = oms.CompressionFromParam(v)
	}
	if v := strings.TrimSpace(q.Get("maxkb")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			opt.MaxInlineKB = n
		}
	}
	if v := strings.TrimSpace(q.Get("page")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			opt.Page = n
		}
	}
	if v := strings.TrimSpace(q.Get("pp")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			opt.MaxTagsPerPage = n
		}
	}
	if v := strings.TrimSpace(q.Get("o")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			opt.GatewayVersion = n
		}
	}
	opt.ClientVersion = oms.ClientVersionFromGateway(opt.GatewayVersion)
	if v := strings.TrimSpace(q.Get("version")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			switch n {
			case 1:
				opt.ClientVersion = oms.ClientVersion1
			case 3:
				opt.ClientVersion = oms.ClientVersion3
			case 2:
				opt.ClientVersion = oms.ClientVersion2
			}
		}
	}
	if v := strings.TrimSpace(q.Get("c")); v != "" {
		opt.AuthCode = v
	}
	if v := strings.TrimSpace(q.Get("h")); v != "" {
		opt.AuthPrefix = v
	}
	opt.ServerBase = serverBase(r)
	opt.ReqHeaders = hdr
	opt.Referrer = q.Get("ref")
	opt.Jar = s.cookieJars.Get(deriveClientKey(r))
	opt.WantFullCache = true
	key := s.renderPrefKey(r, q.Get("url"))
	s.renderPrefs.Apply(key, opt, q)
	return opt
}

func defaultRenderOptions() *oms.RenderOptions {
	return &oms.RenderOptions{
		ImagesOn:      false,
		HighQuality:   false,
		ImageMIME:     "image/jpeg",
		MaxInlineKB:   96,
		Compression:   oms.CompressionDeflate,
		ClientVersion: oms.ClientVersion2,
	}
}

func (s *Server) serveFromCache(w http.ResponseWriter, target string, opt *oms.RenderOptions) bool {
	if raw, cookies, cur, cnt, ok := s.cache.Select(target, opt); ok {
		if cur > 0 || cnt > 0 {
			w.Header().Set("X-Operetta-Page", strconv.Itoa(cur))
			w.Header().Set("X-Operetta-Pages", strconv.Itoa(cnt))
		}
		for _, sc := range cookies {
			w.Header().Add("Set-Cookie", sc)
		}
		s.writeOMS(w, raw, cookies)
		return true
	}
	return false
}

func (s *Server) renderPrefKey(r *http.Request, target string) string {
	return deriveClientKey(r) + "|" + target
}

func serverBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) loadPage(target string, hdr http.Header, opt *oms.RenderOptions) (*oms.Page, error) {
	cfg := s.sites.Find(target)
	header := http.Header{}
	copyHeader(header, hdr)
	if cfg != nil {
		for k, v := range cfg.Headers {
			header.Set(k, v)
		}
		switch strings.ToLower(cfg.Mode) {
		case "compact":
			return oms.LoadCompactPageWithHeaders(target, header)
		}
	}
	return oms.LoadPageWithHeadersAndOptions(target, header, opt)
}

func (s *Server) writeOMS(w http.ResponseWriter, data []byte, _ []string) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Connection", "close")
	dumpOMS(s.logger, data)
	_, _ = w.Write(data)
}

func (s *Server) isInternalAboutRequest(raw, normalized string) bool {
	lowerRaw := strings.ToLower(raw)
	lowerNorm := strings.ToLower(normalized)
	if strings.HasPrefix(lowerRaw, "/obml/server:") || strings.HasPrefix(lowerRaw, "server:") {
		return true
	}
	if strings.HasPrefix(lowerNorm, "http://server:") || strings.HasPrefix(lowerNorm, "https://server:") {
		return true
	}
	return false
}

func (s *Server) renderAboutPage(params map[string]string) *oms.Page {
	page := oms.NewPage()
	page.AddString("1/internal:about")
	if c := strings.TrimSpace(params["c"]); c != "" {
		page.AddAuthcode(c)
	}
	if h := strings.TrimSpace(params["h"]); h != "" {
		page.AddAuthprefix(h)
	}
	page.AddStyle(oms.StyleDefault)
	page.AddPlus()
	page.AddText("About / License")
	page.AddBreak()
	page.AddText("This proxy is open-source and not affiliated with Opera.")
	page.AddBreak()
	page.AddLink("0/https://github.com/openai/codex-cli", "Project Homepage")
	page.AddLink("0/https://www.gnu.org/licenses/", "Licenses")
	page.AddBreak()
	page.Finalize()
	page.Normalize()
	return page
}

func looksLikeBookmarksPortal(target string) bool {
	lt := strings.ToLower(target)
	return strings.Contains(lt, "opera-mini.ru/bndex.php") || strings.Contains(lt, "opera-mini.ru/index.php")
}
