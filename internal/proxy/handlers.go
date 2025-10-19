package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"operetta/oms"
)

// clientJarKey derives a stable per-client key for server-side cookie jars.
// Priority:
// 1) If request provides auth tokens (h/c), bind jar to AUTH|h|c.
// 2) Else, if authStore has tokens for this client (by cookie or host|UA), use AUTH|prefix|code.
// 3) Else, fall back to host|UA key (DeriveClientKey).
func (s *Server) clientJarKey(r *http.Request, params map[string]string) string {
	if params != nil {
		h := strings.TrimSpace(params["h"])
		c := strings.TrimSpace(params["c"])
		if h != "" || c != "" {
			return "AUTH|" + h + "|" + c
		}
	}
	if tok, ok := s.auth.get(clientAuthKeyFromRequest(r)); ok {
		if strings.TrimSpace(tok.Prefix) != "" || strings.TrimSpace(tok.Code) != "" {
			return "AUTH|" + tok.Prefix + "|" + tok.Code
		}
	}
	return DeriveClientKey(r)
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		body, _ := io.ReadAll(r.Body)
		debugHTTP := os.Getenv("OMS_HTTP_DEBUG") == "1"

		if debugHTTP {
			s.logger.Printf("===> Incoming %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
			s.logger.Printf("Headers:")
			for k, v := range r.Header {
				s.logger.Printf("  %s: %s", k, strings.Join(v, "; "))
			}
			s.logger.Printf("Body length: %d", len(body))
			if bytes.Contains(body, []byte("h=")) || bytes.Contains(body, []byte("c=")) {
				reH := regexp.MustCompile(`h=([^\x00]+)`)
				reC := regexp.MustCompile(`c=([^\x00]+)`)
				mH := reH.FindSubmatch(body)
				mC := reC.FindSubmatch(body)
				s.logger.Printf("Auth fields detected in body: prefix=%q code=%q",
					func() string {
						if len(mH) > 1 {
							return string(mH[1])
						}
						return ""
					}(),
					func() string {
						if len(mC) > 1 {
							return string(mC[1])
						}
						return ""
					}(),
				)
			} else {
				s.logger.Printf("No h=/c= fields found in POST body.")
			}
		}

		r.Body.Close()
		params := parseNullKV(body)

		if strings.Contains(params["h"], ".") && params["c"] == "" {
			parts := strings.SplitN(params["h"], ".", 2)
			params["h"] = parts[0]
			if len(parts) > 1 {
				params["c"] = parts[1]
			}
		}

		if debugHTTP {
			s.logger.Printf("Parsed params: c=%q, h=%q, u=%q", params["c"], params["h"], params["u"])
		}

		// Ensure per-client auth tokens are present or create them.
		clientKey := clientAuthKeyFromRequest(r)
		hadCookie := false
		if _, err := r.Cookie(authCookieName); err == nil {
			hadCookie = true
		}

		var tok authTokens
		var ok bool

		if params["h"] != "" && params["c"] != "" {
			tok, ok = s.auth.ensureByCode(params["h"], params["c"])
			if ok {
				s.logger.Printf("AuthStore: restored existing session for h=%q c=%q", params["h"], params["c"])
			} else {
				// создаём или обновляем токен для clientKey
				tok.Prefix = params["h"]
				tok.Code = params["c"]
				s.auth.updateToken(clientKey, tok)
				s.logger.Printf("AuthStore: registered new session for h=%q c=%q (clientKey=%q)",
					params["h"], params["c"], clientKey)
			}
		} else {
			tok = s.auth.ensure(clientKey)
			s.logger.Printf("AuthStore: created new session for %q", clientKey)

			h := strings.TrimSpace(params["h"])
			c := strings.TrimSpace(params["c"])
			if h != "" || c != "" {
				if h != "" {
					tok.Prefix = h
				}
				if c != "" {
					tok.Code = c
				}
				s.auth.updateToken(clientKey, tok)
				s.logger.Printf("AuthStore: updated token for %q with prefix=%q code=%q", clientKey, tok.Prefix, tok.Code)
			} else {
				s.logger.Printf("AuthStore: kept generated token for %q (no prefix/code provided)", clientKey)
			}
		}

		if debugHTTP {
			s.logger.Printf("AuthStore.ensure for clientKey=%q => prefix=%q code=%q",
				clientKey, tok.Prefix, tok.Code)
		}

		if strings.TrimSpace(params["c"]) == "" {
			params["c"] = tok.Code
		}
		if strings.TrimSpace(params["h"]) == "" {
			params["h"] = tok.Prefix
		}
		// If no URL was provided, reply with a minimal valid OMS page so clients don't show an error dialog.
		if raw := params["u"]; raw == "" {
			// set association cookie if needed
			if !hadCookie {
				http.SetCookie(w, s.auth.cookieFor(clientKey))

				if debugHTTP {
					s.logger.Printf("Set auth cookie for %q: %+v", clientKey, s.auth.cookieFor(clientKey))
				}
			}
			page := s.renderBootstrapPage(tok.Code, tok.Prefix)
			s.writeOMS(w, page.Data, page.SetCookies)
			return
		}
		if raw := params["u"]; raw != "" {
			target := normalizeObmlURL(raw)
			jarKey := s.clientJarKey(r, params)
			hdr := s.headersFromParams(r, params)
			if form := strings.TrimSpace(params["j"]); form != "" {
				if augmented, changed := s.forms.Augment(jarKey, target, form); changed {
					params["j"] = augmented
					if debugHTTP {
						s.logger.Printf("Form augment: applied stored hidden fields for %q", target)
					}
				} else if s.prefetchFormHidden(r, params, target, hdr, jarKey, debugHTTP) {
					if augmented, changed := s.forms.Augment(jarKey, target, form); changed {
						params["j"] = augmented
						if debugHTTP {
							s.logger.Printf("Form augment: applied prefetched hidden fields for %q", target)
						}
					}
				}
			}
			opt := s.renderOptionsFromParams(r, params, hdr, jarKey)
			if debugHTTP {
				s.logger.Printf("FETCH target(raw=%q norm=%q) jarKey=%q formLen=%d hdrCookieLen=%d",
					raw, target, jarKey, len(opt.FormBody), len(hdr.Get("Cookie")))
			}
			if s.isInternalAboutRequest(raw, target) {
				page := s.renderAboutPage(params)
				s.writeOMS(w, page.Data, page.SetCookies)
				return
			}
			if s.shouldServeLocalBookmarks() && looksLikeBookmarksPortal(target) {
				if page := s.renderLocalBookmarks(params["c"], params["h"], opt); page != nil {
					page.Normalize()
					if !hadCookie {
						http.SetCookie(w, s.auth.cookieFor(clientKey))

						if debugHTTP {
							s.logger.Printf("Set auth cookie for %q: %+v", clientKey, s.auth.cookieFor(clientKey))
						}
					}
					s.writeOMS(w, page.Data, page.SetCookies)
					return
				}
			}
			s.renderPrefs.Remember(s.renderPrefKeyWithOptions(r, params["u"], opt), opt)
			cacheHit := s.serveFromCache(w, target, opt)
			if cacheHit {
				return
			}
			page, err := s.loadPage(target, hdr, opt)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			if len(page.FormHidden) > 0 && jarKey != "" {
				s.forms.Store(jarKey, page.FormHidden)
			}
			for i, sc := range page.SetCookies {
				w.Header().Add("Set-Cookie", sc)
				if debugHTTP && i < 3 {
					s.logger.Printf("FORWARD Set-Cookie[%d]=%s", i, sc)
				}
			}
			page.Normalize()
			s.cache.Store(target, opt, hdr, page)
			if !hadCookie {
				http.SetCookie(w, s.auth.cookieFor(clientKey))

				if debugHTTP {
					s.logger.Printf("Set auth cookie for %q: %+v", clientKey, s.auth.cookieFor(clientKey))
				}
			}
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
	s.renderPrefs.Remember(s.renderPrefKeyWithOptions(r, finalURL, opt), opt)
	if s.serveFromCache(w, finalURL, opt) {
		return
	}
	page, err := s.loadPage(finalURL, hdr, opt)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if len(page.FormHidden) > 0 {
		params := map[string]string{"h": opt.AuthPrefix, "c": opt.AuthCode}
		jarKey := s.clientJarKey(r, params)
		if jarKey != "" {
			s.forms.Store(jarKey, page.FormHidden)
		}
	}
	for _, sc := range page.SetCookies {
		w.Header().Add("Set-Cookie", sc)
	}
	page.Normalize()
	s.cache.Store(finalURL, opt, hdr, page)
	s.writeOMS(w, page.Data, page.SetCookies)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	target := strings.TrimSpace(r.URL.Query().Get("url"))
	if target == "" {
		http.Error(w, "missing url", http.StatusBadRequest)
		return
	}
	parsed, err := url.Parse(target)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		http.Error(w, "invalid url", http.StatusBadRequest)
		return
	}

	clientKey := s.clientJarKey(r, nil)
	jar := s.cookieJars.Get(clientKey)
	httpClient := &http.Client{
		Timeout: 5 * time.Minute,
	}
	if jar != nil {
		httpClient.Jar = jar
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.Header.Set("User-Agent", oms.DefaultUpstreamUA)
	if ref := strings.TrimSpace(r.URL.Query().Get("ref")); ref != "" {
		req.Header.Set("Referer", ref)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	ct := strings.TrimSpace(r.URL.Query().Get("ct"))
	if ct == "" {
		ct = strings.TrimSpace(resp.Header.Get("Content-Type"))
	}
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}

	filename := strings.TrimSpace(r.URL.Query().Get("name"))
	if filename == "" {
		filename = deriveDownloadFilename(resp.Header.Get("Content-Disposition"), parsed.Path)
	}

	mode := strings.TrimSpace(r.URL.Query().Get("mode"))
	if strings.EqualFold(mode, "stream") {
		if filename != "" {
			w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=\"%s\"", encodeDispositionFilename(filename)))
		} else if disp := resp.Header.Get("Content-Disposition"); disp != "" {
			w.Header().Set("Content-Disposition", disp)
		}
	} else {
		if filename != "" {
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", encodeDispositionFilename(filename)))
		} else if disp := resp.Header.Get("Content-Disposition"); disp != "" {
			w.Header().Set("Content-Disposition", disp)
		}
	}

	if cl := resp.Header.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl)
	}
	if ar := resp.Header.Get("Accept-Ranges"); ar != "" {
		w.Header().Set("Accept-Ranges", ar)
	}
	w.Header().Set("Connection", "close")
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		s.logger.Printf("download stream error for %s: %v", target, err)
	}
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

func deriveDownloadFilename(disposition, pathPart string) string {
	if disp := strings.TrimSpace(disposition); disp != "" {
		if _, params, err := mime.ParseMediaType(disp); err == nil {
			if name := params["filename"]; name != "" {
				if decoded, err := url.QueryUnescape(name); err == nil {
					return decoded
				}
				return name
			}
		}
	}
	if base := path.Base(pathPart); base != "" && base != "." && base != "/" {
		if decoded, err := url.PathUnescape(base); err == nil {
			return decoded
		}
		return base
	}
	return ""
}

func encodeDispositionFilename(name string) string {
	replacer := strings.NewReplacer("\\", "\\\\", "\"", "\\\"")
	return replacer.Replace(name)
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
	// Pass stable client key downstream so oms can pick the right jar
	hdr.Set("X-Operetta-Client-Key", s.clientJarKey(r, params))
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
	// Include client key derived from query auth if present
	params := map[string]string{
		"h": strings.TrimSpace(r.URL.Query().Get("h")),
		"c": strings.TrimSpace(r.URL.Query().Get("c")),
	}
	hdr.Set("X-Operetta-Client-Key", s.clientJarKey(r, params))
	return hdr
}

func (s *Server) renderOptionsFromParams(r *http.Request, params map[string]string, hdr http.Header, jarKey string) *oms.RenderOptions {
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
	if strings.TrimSpace(params["version"]) == "" && opt.ClientVersion == oms.ClientVersion3 {
		opt.ClientVersion = oms.ClientVersion2
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
	if strings.TrimSpace(jarKey) == "" {
		jarKey = s.clientJarKey(r, params)
	}
	opt.Jar = s.cookieJars.Get(jarKey)
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
	// Preserve device characteristics when passed on query to keep cache keys stable
	if v := strings.TrimSpace(q.Get("w")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			opt.ScreenW = n
		}
	}
	if v := strings.TrimSpace(q.Get("h")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			opt.ScreenH = n
		}
	}
	if v := strings.TrimSpace(q.Get("m")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			opt.HeapBytes = n
		}
	}
	if v := strings.TrimSpace(q.Get("l")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			opt.AlphaLevels = n
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
	if strings.TrimSpace(q.Get("version")) == "" && opt.ClientVersion == oms.ClientVersion3 {
		opt.ClientVersion = oms.ClientVersion2
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
	params := map[string]string{"h": strings.TrimSpace(q.Get("h")), "c": strings.TrimSpace(q.Get("c"))}
	opt.Jar = s.cookieJars.Get(s.clientJarKey(r, params))
	opt.WantFullCache = true
	key := s.renderPrefKeyWithOptions(r, q.Get("url"), opt)
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

func (s *Server) renderPrefKeyWithOptions(r *http.Request, target string, opt *oms.RenderOptions) string {
	// Prefer association by our auth cookie; else h/c from options; else host|UA
	params := map[string]string{
		"h": strings.TrimSpace(opt.AuthPrefix),
		"c": strings.TrimSpace(opt.AuthCode),
	}
	return s.clientJarKey(r, params) + "|" + target
}

// prefetchFormHidden performs a lightweight GET for the target page to collect hidden form fields
// before issuing a POST. The fetched tokens are cached under the provided client key and consumed
// by formStore when the client resubmits the request with the same action.
func (s *Server) prefetchFormHidden(r *http.Request, params map[string]string, target string, hdr http.Header, jarKey string, debug bool) bool {
	if jarKey == "" {
		return false
	}
	paramsCopy := make(map[string]string, len(params))
	for k, v := range params {
		paramsCopy[k] = v
	}
	paramsCopy["j"] = ""
	hdrCopy := http.Header{}
	copyHeader(hdrCopy, hdr)
	hdrCopy.Del("Content-Type")
	opt := s.renderOptionsFromParams(r, paramsCopy, hdrCopy, jarKey)
	opt.FormBody = ""
	page, err := s.loadPage(target, hdrCopy, opt)
	if err != nil {
		if debug {
			s.logger.Printf("Form prefetch error for %q: %v", target, err)
		}
		return false
	}
	if len(page.FormHidden) == 0 {
		if debug {
			s.logger.Printf("Form prefetch: no hidden fields detected for %q", target)
		}
		return false
	}
	s.forms.Store(jarKey, page.FormHidden)
	if debug {
		s.logger.Printf("Form prefetch: cached %d hidden fields for %q", len(page.FormHidden), target)
	}
	return true
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

func (s *Server) renderBootstrapPage(authCode, authPrefix string) *oms.Page {
	page := oms.NewPage()
	// Use a canonical HTTP URL string as first field to satisfy legacy clients
	page.AddString("1/http://opera-mini.ru/bndex.php")
	if strings.TrimSpace(authCode) != "" {
		page.AddAuthcode(authCode)
	}
	if strings.TrimSpace(authPrefix) != "" {
		page.AddAuthprefix(authPrefix)
	}
	page.AddStyle(oms.StyleDefault)
	page.AddPlus()
	page.AddText("OK")
	page.Finalize()
	// Conservative normalize with a small stag value, matching simple portal-like pages
	if nb, err := oms.NormalizeOMSWithStag(page.Data, 4); err == nil && nb != nil {
		page.Data = nb
	} else {
		page.Normalize()
	}
	return page
}

func looksLikeBookmarksPortal(target string) bool {
	lt := strings.ToLower(target)
	return strings.Contains(lt, "opera-mini.ru/bndex.php") || strings.Contains(lt, "opera-mini.ru/index.php")
}
