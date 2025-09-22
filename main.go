package main

import (
    "bytes"
    "compress/flate"
    "encoding/binary"
    "encoding/json"
    "flag"
    "io"
    "log"
    "net"
    "net/url"
    "net/http/cookiejar"
    "sync"
    "net/http"
    "os"
    "path/filepath"
    "strconv"
    "operetta/oms"
    "strings"
    "time"
)

const indexHTML = `<!DOCTYPE html>
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

// Test HTML and handlers removed for production cleanliness

func rootHandler(w http.ResponseWriter, r *http.Request) {
    if r.Method == http.MethodPost {
        body, _ := io.ReadAll(r.Body)
        r.Body.Close()
        params := parseNullKV(body)
        if u := params["u"]; u != "" {
            url := normalizeObmlURL(u)
            // Handle internal pseudo-links like server:license to avoid proxying invalid hosts
            if strings.HasPrefix(strings.ToLower(u), "/obml/server:") || strings.HasPrefix(strings.ToLower(u), "server:") || strings.HasPrefix(strings.ToLower(url), "http://server:") || strings.HasPrefix(strings.ToLower(url), "https://server:") {
                page := oms.NewPage()
                page.AddString("1/internal:about")
                if c := strings.TrimSpace(params["c"]); c != "" { page.AddAuthcode(c) }
                if h := strings.TrimSpace(params["h"]); h != "" { page.AddAuthprefix(h) }
                page.AddStyle(oms.StyleDefault)
                page.AddPlus(); page.AddText("About / License"); page.AddBreak()
                page.AddText("This proxy is open-source and not affiliated with Opera.")
                page.AddBreak()
                page.AddLink("0/https://github.com/openai/codex-cli", "Project Homepage")
                page.AddLink("0/https://www.gnu.org/licenses/", "Licenses")
                page.AddBreak()
                page.Finalize()
                page.Normalize()
                w.Header().Set("Content-Type", "application/octet-stream")
                w.Header().Set("Content-Length", strconv.Itoa(len(page.Data)))
                w.Header().Set("Connection", "close")
                log.Printf("PROXY POST / u=%s -> [internal about page]", u)
                dumpOMS(page.Data)
                w.Write(page.Data)
                return
            }
            // Opera Mini mod 2.xx: some bookmark UIs hit opera-mini.ru/bndex.php first.
            // If reachable upstream is unavailable, optionally rewrite to a fallback target.
            if strings.Contains(url, "opera-mini.ru/bndex.php") || strings.Contains(url, "opera-mini.ru/index.php") {
                mode := strings.ToLower(strings.TrimSpace(os.Getenv("OMS_BOOKMARKS_MODE")))
                // passthrough to remote portal when explicitly requested
                if mode == "remote" || mode == "pass" || mode == "passthrough" {
                    // keep original url
                } else {
                    // Serve a small local OBML bookmarks page so user can pick sites
                    // Build options (image/auth) first
                    o := oms.RenderOptions{ImagesOn: false, HighQuality: false, ImageMIME: "image/jpeg", MaxInlineKB: 96}
                    o.AuthCode = params["c"]
                    o.AuthPrefix = params["h"]
                    page := oms.NewPage()
                    // Mimic original portal page URL as page context
                    page.AddString("1/http://opera-mini.ru/bndex.php")
                    if o.AuthCode != "" { page.AddAuthcode(o.AuthCode) }
                    if o.AuthPrefix != "" { page.AddAuthprefix(o.AuthPrefix) }
                    page.AddStyle(oms.StyleDefault)
                    page.AddPlus()
                    page.AddText("Р—Р°РєР»Р°РґРєРё")
                    page.AddBreak()
                    // Default bookmarks (override with OMS_BOOKMARKS=name|url,...)
                    bspec := strings.TrimSpace(os.Getenv("OMS_BOOKMARKS"))
                    bookmarks := [][2]string{}
                    if bspec == "" {
                        // No local list configured: passthrough to remote portal
                        log.Printf("PROXY POST / u=%s -> %s (passthrough)", u, url)
                        hdr2 := http.Header{}
                        if ua := params["i"]; ua != "" { hdr2.Set("User-Agent", ua) } else if v := params["v"]; v != "" { hdr2.Set("User-Agent", v) }
                        if lang := firstNonEmpty(params["q"], params["D"]); lang != "" { hdr2.Set("Accept-Language", lang) }
                        if ck := r.Header.Get("Cookie"); ck != "" { hdr2.Set("Cookie", ck) }
                        if ref := params["f"]; ref != "" { hdr2.Set("Referer", ref) }
                        hdr2.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
                        page, err := loadWithSiteConfig(url, hdr2, &o)
                        if err != nil {
                            http.Error(w, err.Error(), http.StatusBadGateway)
                            return
                        }
                        for _, sc := range page.SetCookies { w.Header().Add("Set-Cookie", sc) }
                        page.Normalize()
                        w.Header().Set("Content-Type", "application/octet-stream")
                        w.Header().Set("Content-Length", strconv.Itoa(len(page.Data)))
                        w.Header().Set("Connection", "close")
                        dumpOMS(page.Data)
                        w.Write(page.Data)
                        return
                    }
                    if bspec != "" {
                        parts := strings.Split(bspec, ",")
                        for _, p := range parts {
                            kv := strings.SplitN(strings.TrimSpace(p), "|", 2)
                            if len(kv) == 2 {
                                name := strings.TrimSpace(kv[0]); link := strings.TrimSpace(kv[1])
                                if name != "" && link != "" { bookmarks = append(bookmarks, [2]string{name, link}) }
                            }
                        }
                    } else {
                    // Use a static Google Mobile link; client-side bookmarks control locale
                    google := "http://www.google.com/xhtml?client=ms-opera_mb_no&channel=bm"
                    bookmarks = append(bookmarks,
                        [2]string{"Р›РµРЅС‚Р°.СЂСѓ", "http://lenta.ru/"},
                        [2]string{"Р“Р°Р·РµС‚Р°.Ru", "http://www.gazeta.ru/"},
                        [2]string{"РЇРЅРґРµРєСЃ", "http://yandex.ru/"},
                        [2]string{"Google", google},
                    )
                }
                    for _, it := range bookmarks {
                        page.AddLink("0/"+it[1], it[0])
                    }
                    page.Finalize()
                    // Force stag_count to 0x0400 specifically for this internal page
                    if nb, err := oms.NormalizeOMSWithStag(page.Data, 4); err == nil && nb != nil { page.Data = nb }
                    w.Header().Set("Content-Type", "application/octet-stream")
                    w.Header().Set("Content-Length", strconv.Itoa(len(page.Data)))
                    w.Header().Set("Connection", "close")
                    log.Printf("PROXY POST / u=%s -> [local bookmarks page]", u)
                    dumpOMS(page.Data)
                    w.Write(page.Data)
                    return
                }
            }
            // Opera Mini internal: mapping removed to avoid test routes in production
            log.Printf("PROXY POST / u=%s -> %s", u, url)
            // Build request headers based on client hints
            hdr := http.Header{}
            if ua := params["i"]; ua != "" {
                hdr.Set("User-Agent", ua)
            } else if v := params["v"]; v != "" {
                hdr.Set("User-Agent", v)
            }
            if lang := firstNonEmpty(firstNonEmpty(params["q"], params["y"]), params["D"]); lang != "" {
                hdr.Set("Accept-Language", lang)
            }
            // Forward Cookie header from client HTTP request
            if ck := r.Header.Get("Cookie"); ck != "" { hdr.Set("Cookie", ck) }
            // Forward Referer from client payload (Opera Mini protocol uses 'f=')
            if ref := params["f"]; ref != "" { hdr.Set("Referer", ref) }
            hdr.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
            // Client rendering + auth options
            o := oms.RenderOptions{ImagesOn: false, HighQuality: false, ImageMIME: "image/jpeg", MaxInlineKB: 96}
            if km := params["k"]; strings.HasPrefix(strings.ToLower(km), "image/") { o.ImageMIME = km }
            if dv := params["d"]; dv != "" {
                parts := strings.Split(dv, ";")
                for _, p := range parts {
                    kv := strings.SplitN(strings.TrimSpace(p), ":", 2)
                    if len(kv) != 2 { continue }
                    key := strings.TrimSpace(kv[0])
                    val := strings.TrimSpace(kv[1])
                    switch key {
                    case "i":
                        // Enable only on explicit Yes (1)
                        o.ImagesOn = (val == "1")
                    case "q":
                        o.HighQuality = (val == "1")
                    case "w":
                        if n, err := strconv.Atoi(val); err == nil && n > 0 { o.ScreenW = n }
                    case "h":
                        if n, err := strconv.Atoi(val); err == nil && n > 0 { o.ScreenH = n }
                    case "c":
                        if n, err := strconv.Atoi(val); err == nil && n > 0 { o.NumColors = n }
                    case "m":
                        if n, err := strconv.Atoi(val); err == nil && n >= 0 { o.HeapKB = n }
                    case "l":
                        if n, err := strconv.Atoi(val); err == nil && n >= 0 { o.AlphaLevels = n }
                    }
                }
            }
            // Echo Opera Mini auth tokens so OM 2.x accepts the response
            o.AuthCode = params["c"]
            o.AuthPrefix = params["h"]
            // Client form submission payload ('j=...'), urlencoded
            if j := strings.TrimSpace(params["j"]); j != "" { o.FormBody = j }
            // Pagination from client
            if ov := strings.TrimSpace(params["o"]); ov != "" {
                if n, err := strconv.Atoi(ov); err == nil && n > 0 { o.MaxTagsPerPage = n }
            }
            if wv := strings.TrimSpace(params["w"]); wv != "" {
                seg := strings.SplitN(wv, ";", 2)
                if len(seg) >= 1 {
                    if n, err := strconv.Atoi(strings.TrimSpace(seg[0])); err == nil && n > 0 { o.Page = n }
                }
            }
            // Server base for paging links
            scheme := "http"
            if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") { scheme = "https" }
            o.ServerBase = scheme + "://" + r.Host
            // Provide headers and referrer for sub-requests (images/CSS)
            o.ReqHeaders = hdr
            o.Referrer = url
            // Attach per-client cookie jar so origin cookies persist across requests
            o.Jar = getClientJar(r)
            opt := &o
            // Apply per-site config when rendering proxied requests
            page, err := loadWithSiteConfig(url, hdr, opt)
            if err != nil {
                http.Error(w, err.Error(), http.StatusBadGateway)
                return
            }
            // Pass back origin Set-Cookie headers to the client
            for _, sc := range page.SetCookies { w.Header().Add("Set-Cookie", sc) }
            // Ask loader to prepare full packed copy for pagination cache
            o.WantFullCache = true
            page.Normalize()
            cacheStore(url, &o, hdr, page)
            w.Header().Set("Content-Type", "application/octet-stream")
            w.Header().Set("Content-Length", strconv.Itoa(len(page.Data)))
            w.Header().Set("Connection", "close")
            dumpOMS(page.Data)
            w.Write(page.Data)
            return
        }
        // Fallback: empty OK
        w.Header().Set("Content-Type", "application/xml; charset=utf-8")
        w.Header().Set("Connection", "close")
        w.Header().Set("Content-Length", "0")
        w.WriteHeader(http.StatusOK)
        return
    }
    w.Header().Set("Content-Type", "text/html; charset=utf-8")
    w.Header().Set("Connection", "close")
    w.Header().Set("Content-Length", strconv.Itoa(len(indexHTML)))
    io.WriteString(w, indexHTML)
}

func fetchHandler(w http.ResponseWriter, r *http.Request) {
    _ = r.ParseForm()
    base := firstNonEmpty(r.FormValue("url"), r.URL.Query().Get("url"))
    if base != "" {
        // Some clients double-encode this param; decode twice for safety
        base = urlDecode(urlDecode(base))
    }
    if base == "" {
        http.Error(w, "missing url", http.StatusBadRequest)
        return
    }
    action := firstNonEmpty(r.FormValue("action"), r.URL.Query().Get("action"))
    get := firstNonEmpty(r.FormValue("get"), r.URL.Query().Get("get"))
    finalURL := buildURL(base, action, get)
    log.Printf("IN %s %s from %s | action=%q get=%q -> final=%s", r.Method, r.URL.String(), r.RemoteAddr, action, get, finalURL)
	hdr := http.Header{}
	if ua := r.URL.Query().Get("ua"); ua != "" { hdr.Set("User-Agent", ua) }
	if lang := r.URL.Query().Get("lang"); lang != "" { hdr.Set("Accept-Language", lang) }
    // Optional image prefs via query: img=1|0, hq=1|0, mime=image/jpeg|image/png, maxkb=96
    var opt *oms.RenderOptions
    if true {
        // Always allocate options; apply defaults and read optional params
        o := oms.RenderOptions{ImagesOn: false, HighQuality: false, ImageMIME: "image/jpeg", MaxInlineKB: 96}
        if v := strings.TrimSpace(r.URL.Query().Get("img")); v == "1" { o.ImagesOn = true }
        if v := strings.TrimSpace(r.URL.Query().Get("hq")); v == "1" { o.HighQuality = true }
        if v := strings.TrimSpace(r.URL.Query().Get("mime")); strings.HasPrefix(strings.ToLower(v), "image/") { o.ImageMIME = v }
        if v := strings.TrimSpace(r.URL.Query().Get("maxkb")); v != "" { if n, err := strconv.Atoi(v); err == nil && n > 0 { o.MaxInlineKB = n } }
        if v := strings.TrimSpace(r.URL.Query().Get("page")); v != "" { if n, err := strconv.Atoi(v); err == nil && n > 0 { o.Page = n } }
        if v := strings.TrimSpace(r.URL.Query().Get("pp")); v != "" { if n, err := strconv.Atoi(v); err == nil && n > 0 { o.MaxTagsPerPage = n } }
        // Server base for paging links
        scheme := "http"
        if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") { scheme = "https" }
        o.ServerBase = scheme + "://" + r.Host
        o.Jar = getClientJar(r)
        opt = &o
    }
    // Forward HTTP Cookie and Referer from the client request
    if ck := r.Header.Get("Cookie"); ck != "" { hdr.Set("Cookie", ck) }
    if ref := r.Referer(); ref != "" { hdr.Set("Referer", ref) }
    // Try to serve from cache for pages > 1
    if opt != nil && opt.Page > 1 && opt.MaxTagsPerPage > 0 {
        if raw, cur, cnt, ok := cacheSelect(finalURL, opt); ok {
            p := oms.NewPage()
            p.Data = raw
            // Build simple navigation
            if opt.ServerBase != "" && cnt > 1 {
                nav := oms.NewPage()
                nav.AddHr("")
                qpp := strconv.Itoa(opt.MaxTagsPerPage)
                if opt.Page > 1 {
                    prevURL := opt.ServerBase + "/fetch?url=" + url.QueryEscape(base) + "&pp=" + qpp + "&page=" + strconv.Itoa(opt.Page-1)
                    nav.AddLink("0/"+prevURL, "РќР°Р·Р°Рґ")
                }
                if opt.Page < cnt {
                    nextURL := opt.ServerBase + "/fetch?url=" + url.QueryEscape(base) + "&pp=" + qpp + "&page=" + strconv.Itoa(opt.Page+1)
                    nav.AddLink("0/"+nextURL, "Р”Р°Р»РµРµ")
                }
                nav.AddHr("")
                p.Data = append(p.Data, nav.Data...)
            }
            p.SetPart(cur, cnt)
            p.Finalize()
            p.Normalize()
            w.Header().Set("Content-Type", "application/octet-stream")
            w.Header().Set("Content-Length", strconv.Itoa(len(p.Data)))
            w.Header().Set("Connection", "close")
            dumpOMS(p.Data)
            w.Write(p.Data)
            return
        }
    }
    if opt != nil && opt.Page <= 1 { opt.WantFullCache = true }
    page, err := loadWithSiteConfig(finalURL, hdr, opt)
    if err != nil {
        http.Error(w, err.Error(), http.StatusBadGateway)
        return
    }
    for _, sc := range page.SetCookies { w.Header().Add("Set-Cookie", sc) }
    page.Normalize()
    cacheStore(finalURL, opt, hdr, page)
    w.Header().Set("Content-Type", "application/octet-stream")
    // Some legacy clients require explicit Content-Length and non-chunked body
    w.Header().Set("Content-Length", strconv.Itoa(len(page.Data)))
    // Encourage connection close to avoid client-side keep-alive issues
    w.Header().Set("Connection", "close")
    // Debug-dump OMS header + head/tail bytes
    dumpOMS(page.Data)
    w.Write(page.Data)
}

func pingHandler(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "text/plain; charset=utf-8")
    w.Header().Set("Connection", "close")
    io.WriteString(w, "pong\n")
}


func newServer() http.Handler {
    mux := http.NewServeMux()
    mux.HandleFunc("/", rootHandler)
    mux.HandleFunc("/fetch", fetchHandler)
    mux.HandleFunc("/validate", validateHandler)
    mux.HandleFunc("/ping", pingHandler)
    return withLogging(mux)
}

func main() {
    addrFlag := flag.String("addr", ":8080", "listen address, e.g. :80 or 0.0.0.0:8080")
    flag.Parse()
    addr := *addrFlag
    if env := os.Getenv("PORT"); env != "" {
        addr = ":" + env
    }
    log.SetFlags(log.LstdFlags | log.Lmicroseconds)
    log.SetOutput(os.Stdout)

    handler := newServer()
    srv := &http.Server{
        Addr:         addr,
        Handler:      handler,
        ReadTimeout:  0,
        WriteTimeout: 0,
        ErrorLog:     log.New(os.Stdout, "HTTPERR ", log.LstdFlags|log.Lmicroseconds),
        ConnState: func(c net.Conn, s http.ConnState) {
            log.Printf("CONN %s %s", s.String(), c.RemoteAddr())
        },
    }
    ln, err := net.Listen("tcp", addr)
    if err != nil {
        log.Fatalf("Listen error on %s: %v", addr, err)
    }
    log.Println("Listening on", addr)
    log.Fatal(srv.Serve(ln))
}

// withLogging logs basic request/response info for all handlers.
func withLogging(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        log.Printf("REQ %s %s Host=%s UA=%q From=%s", r.Method, r.URL.String(), r.Host, r.UserAgent(), r.RemoteAddr)
        // Log selected headers that matter to J2ME proxies
        if hv := r.Header.Get("X-Online-Host"); hv != "" { log.Printf("HDR X-Online-Host: %s", hv) }
        if hv := r.Header.Get("Proxy-Connection"); hv != "" { log.Printf("HDR Proxy-Connection: %s", hv) }
        if hv := r.Header.Get("Connection"); hv != "" { log.Printf("HDR Connection: %s", hv) }
        if hv := r.Header.Get("Content-Type"); hv != "" { log.Printf("HDR Content-Type: %s", hv) }
        if hv := r.Header.Get("Content-Length"); hv != "" { log.Printf("HDR Content-Length: %s", hv) }
        // Read and log request body (restore for handler)
        if r.ContentLength != 0 && (r.Method == http.MethodPost || r.Method == http.MethodPut) {
            var limit int64 = 1 << 20 // 1 MiB safety cap
            if r.ContentLength > 0 && r.ContentLength < limit { limit = r.ContentLength }
            data, _ := io.ReadAll(io.LimitReader(r.Body, limit))
            if r.ContentLength < 0 || int64(len(data)) < r.ContentLength {
                _, _ = io.Copy(io.Discard, r.Body)
            }
            r.Body.Close()
            r.Body = io.NopCloser(bytes.NewReader(data))
            if len(data) > 0 {
                if len(data) > 512 {
                    log.Printf("BODY (first 512): %s", hexBlock(data, 0, 512))
                } else {
                    log.Printf("BODY: %s", hexBlock(data, 0, len(data)))
                }
            }
        }
        next.ServeHTTP(w, r)
    })
}

func keysOf(m map[string][]string) []string {
    ks := make([]string, 0, len(m))
    for k := range m { ks = append(ks, k) }
    return ks
}

func normalizeObmlURL(u string) string {
    s := u
    if strings.HasPrefix(s, "/obml/") {
        s = s[len("/obml/"):]
        // optional numeric version and slash
        i := 0
        for i < len(s) && s[i] >= '0' && s[i] <= '9' { i++ }
        if i < len(s) && s[i] == '/' { s = s[i+1:] } else if i > 0 { s = s[i:] }
    }
    if strings.HasPrefix(s, "0/") { s = s[2:] }
    // Prepend http:// if no scheme present
    if !(strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")) {
        s = "http://" + s
    }
    return s
}

func parseNullKV(b []byte) map[string]string {
    res := make(map[string]string)
    if len(b) == 0 { return res }
    parts := bytes.Split(b, []byte{0})
    for _, p := range parts {
        if len(p) == 0 { continue }
        kv := string(p)
        if i := strings.IndexByte(kv, '='); i != -1 {
            k := kv[:i]
            v := kv[i+1:]
            res[k] = v
        }
    }
    return res
}

func firstNonEmpty(a, b string) string { if a != "" { return a }; return b }

func dumpOMS(b []byte) {
    if len(b) < 6 {
        log.Printf("OUT OMS: too short (%d)", len(b))
        return
    }
    magic := binary.LittleEndian.Uint16(b[:2])
    size := binary.BigEndian.Uint32(b[2:6])
    head := hexBlock(b, 0, min(64, len(b)))
    tail := hexBlock(b, max(0, len(b)-64), len(b))
    log.Printf("OUT OMS: magic=0x%04x size=%d len=%d", magic, size, len(b))
    log.Printf("OUT head: %s", head)
    if len(b) > 64 {
        log.Printf("OUT tail: %s", tail)
    }
    // Attempt to inflate and report last tag
    fr := flate.NewReader(bytes.NewReader(b[6:]))
    dec, err := io.ReadAll(fr)
    fr.Close()
    if err != nil {
        log.Printf("OUT inflate error: %v", err)
        return
    }
    last := byte(0)
    if len(dec) > 0 {
        last = dec[len(dec)-1]
    }
    // Parse V2 header (LE) and estimate tag count
    if len(dec) >= 35 {
        tcSw := binary.LittleEndian.Uint16(dec[18:20])
        tcUnswapped := uint16((tcSw&0xFF)<<8 | (tcSw>>8))
		scSw := binary.LittleEndian.Uint16(dec[26:28])
		scUnswapped := uint16((scSw&0xFF)<<8 | (scSw>>8))
        parsed, tags, counts := parseTags(dec)
        log.Printf("OUT inflate ok: decLen=%d last=0x%02x(%q) V2.tag_count(sw=0x%04x,cnt=%d) stag_count(sw=0x%04x,cnt=%d) parsedTags=%d",
			len(dec), last, printable(last), tcSw, tcUnswapped, scSw, scUnswapped, parsed)
        // Log V2 as the client likely reads it (big-endian shorts, 1-byte res4)
        beU16 := func(off int) uint16 { return binary.BigEndian.Uint16(dec[off : off+2]) }
        v2_d := beU16(0)      // res1[0]
        v2_f := beU16(2)      // res1[1]
        v2_g := beU16(4)      // res1[2]
        v2_res1 := [9]uint16{beU16(0),beU16(2),beU16(4),beU16(6),beU16(8),beU16(10),beU16(12),beU16(14),beU16(16)}
        v2_tag := beU16(18)
        v2_partCur := beU16(20)
        v2_partCnt := beU16(22)
        v2_res2 := beU16(24)
        v2_stag := beU16(26)
        v2_res3 := beU16(28)
        v2_res4 := dec[30]
        v2_cachable := beU16(31)
        v2_res5 := beU16(33)
        _ = v2_d; _ = v2_f; _ = v2_g; _ = v2_res1
        log.Printf("OUT V2(be): tag=%d part=%d/%d res2=%d stag=%d res3=%d res4=%d cach=0x%04x res5=%d",
            v2_tag, v2_partCur, v2_partCnt, v2_res2, v2_stag, v2_res3, v2_res4, v2_cachable, v2_res5)
        // Print first 64 tags as a string for quick visual
        maxShow := 64
        if len(tags) < maxShow { maxShow = len(tags) }
        buf := make([]rune, maxShow)
        for i:=0;i<maxShow;i++{ t:=tags[i]; if t>=32 && t<=126 { buf[i]=rune(t) } else { buf[i]='.' } }
        log.Printf("OUT tags[0:%d]: %s", maxShow, string(buf))
        log.Printf("OUT tag counts: T=%d L=%d E=%d B=%d +=%d V=%d D=%d S=%d R=%d k=%d h=%d x=%d p=%d u=%d i=%d b=%d e=%d c=%d r=%d s=%d o=%d Q=%d",
            counts['T'],counts['L'],counts['E'],counts['B'],counts['+'],counts['V'],counts['D'],counts['S'],counts['R'],counts['k'],counts['h'],counts['x'],counts['p'],counts['u'],counts['i'],counts['b'],counts['e'],counts['c'],counts['r'],counts['s'],counts['o'],counts['Q'])
    } else {
        log.Printf("OUT inflate ok: decLen=%d last=0x%02x(%q)", len(dec), last, printable(last))
    }
}

func hexBlock(b []byte, start, end int) string {
    if start < 0 { start = 0 }
    if end > len(b) { end = len(b) }
    sb := make([]byte, 0, (end-start)*3)
    for i := start; i < end; i++ {
        hi := "0123456789abcdef"[b[i]>>4]
        lo := "0123456789abcdef"[b[i]&0x0f]
        sb = append(sb, byte(hi), byte(lo))
        if i+1 < end {
            sb = append(sb, ' ')
        }
    }
    return string(sb)
}

func printable(b byte) rune {
    if b >= 32 && b <= 126 { return rune(b) }
    return '.'
}

func min(a, b int) int { if a < b { return a }; return b }
func max(a, b int) int { if a > b { return a }; return b }

// ---------------------- Validation helpers ----------------------
type omsAnalysis struct {
    Magic      uint16            `json:"magic"`
    Size       uint32            `json:"size"`
    DecLen     int               `json:"decLen"`
    LastTag    string            `json:"lastTag"`
    V2BE       map[string]uint32 `json:"v2be"`
    TagCountSw string            `json:"tagCountSw"`
    ParsedTags int               `json:"parsedTags"`
    TagsHead   string            `json:"tagsHead"`
    Counts     map[string]int    `json:"counts"`
}

func hexU16(v uint16) string {
    const hexd = "0123456789abcdef"
    return string([]byte{hexd[v>>12&0xF], hexd[v>>8&0xF], hexd[v>>4&0xF], hexd[v&0xF]})
}

func analyzeOMS(b []byte) omsAnalysis {
    out := omsAnalysis{Counts: map[string]int{}, V2BE: map[string]uint32{}}
    if len(b) < 6 { return out }
    out.Magic = binary.LittleEndian.Uint16(b[:2])
    out.Size = binary.BigEndian.Uint32(b[2:6])
    fr := flate.NewReader(bytes.NewReader(b[6:]))
    dec, err := io.ReadAll(fr)
    fr.Close()
    if err != nil { return out }
    out.DecLen = len(dec)
    last := byte(0)
    if len(dec) > 0 { last = dec[len(dec)-1] }
    if last >= 32 && last <= 126 { out.LastTag = string([]byte{last}) } else { out.LastTag = "." }
    if len(dec) >= 35 {
        beU16 := func(off int) uint16 { return binary.BigEndian.Uint16(dec[off : off+2]) }
        out.V2BE["tag"] = uint32(beU16(18))
        out.V2BE["partCur"] = uint32(beU16(20))
        out.V2BE["partCnt"] = uint32(beU16(22))
        out.V2BE["res2"] = uint32(beU16(24))
        out.V2BE["stag"] = uint32(beU16(26))
        out.V2BE["res3"] = uint32(beU16(28))
        out.V2BE["res4"] = uint32(dec[30])
        out.V2BE["cachable"] = uint32(beU16(31))
        out.V2BE["res5"] = uint32(beU16(33))
        tcSw := binary.LittleEndian.Uint16(dec[18:20])
        out.TagCountSw = "0x" + hexU16(tcSw)
        parsed, tags, counts := parseTags(dec)
        out.ParsedTags = parsed
        maxShow := 64
        if len(tags) < maxShow { maxShow = len(tags) }
        buf := make([]rune, maxShow)
        for i:=0;i<maxShow;i++{ t:=tags[i]; if t>=32 && t<=126 { buf[i]=rune(t) } else { buf[i]='.' } }
        out.TagsHead = string(buf)
        // Fill selected counts
        keys := []byte{'T','L','E','B','+','V','D','S','R','k','h','x','p','u','i','b','e','c','r','s','o','Q'}
        for _, k := range keys { if v, ok := counts[k]; ok && v>0 { out.Counts[string([]byte{k})]=v } }
    }
    return out
}

type validateResult struct {
    URL     string      `json:"url"`
    Full    omsAnalysis `json:"full"`
    Compact omsAnalysis `json:"compact"`
}

func validateHandler(w http.ResponseWriter, r *http.Request) {
    u := r.URL.Query().Get("url")
    if u == "" { http.Error(w, "missing url", http.StatusBadRequest); return }
    hdr := http.Header{}
    if ua := r.URL.Query().Get("ua"); ua != "" { hdr.Set("User-Agent", ua) }
    if lang := r.URL.Query().Get("lang"); lang != "" { hdr.Set("Accept-Language", lang) }
    pageFull, err := oms.LoadPageWithHeaders(u, hdr)
    if err != nil { http.Error(w, err.Error(), http.StatusBadGateway); return }
    pageFull.Normalize()
    aFull := analyzeOMS(pageFull.Data)
    pageCompact, err := oms.LoadCompactPageWithHeaders(u, hdr)
    if err != nil { http.Error(w, err.Error(), http.StatusBadGateway); return }
    aCompact := analyzeOMS(pageCompact.Data)
    res := validateResult{URL: u, Full: aFull, Compact: aCompact}
    w.Header().Set("Content-Type", "application/json; charset=utf-8")
    enc := json.NewEncoder(w)
    enc.SetIndent("", "  ")
    _ = enc.Encode(res)
}

// ---------------------- Per-site config ----------------------
type SiteConfig struct {
    Mode    string            `json:"mode"`              // "full" or "compact"
    Headers map[string]string `json:"headers,omitempty"` // extra/override request headers
}

func sitesConfigDir() string {
    if d := os.Getenv("OMS_SITES_DIR"); d != "" { return d }
    return filepath.Join("config", "sites")
}

func getSiteConfig(target string) *SiteConfig {
    u, err := url.Parse(target)
    if err != nil || u.Host == "" { return nil }
    dir := sitesConfigDir()
    host := u.Host
    // Try candidates: exact host, then trim leftmost label progressively (a.b.c -> b.c -> c)
    labels := strings.Split(host, ".")
    for i := 0; i < len(labels); i++ {
        cand := strings.Join(labels[i:], ".")
        if cand == "" { continue }
        p := filepath.Join(dir, cand+".json")
        if b, err := os.ReadFile(p); err == nil {
            var cfg SiteConfig
            if err := json.Unmarshal(b, &cfg); err == nil {
                return &cfg
            }
        }
    }
    return nil
}

func loadWithSiteConfig(target string, base http.Header, opt *oms.RenderOptions) (*oms.Page, error) {
    hdr := http.Header{}
    for k, vs := range base { for _, v := range vs { hdr.Add(k, v) } }
    if cfg := getSiteConfig(target); cfg != nil {
        for k, v := range cfg.Headers { if v != "" { hdr.Set(k, v) } }
        switch strings.ToLower(cfg.Mode) {
        case "compact":
            return oms.LoadCompactPageWithHeaders(target, hdr)
        }
    }
    return oms.LoadPageWithHeadersAndOptions(target, hdr, opt)
}

// parseTagCount walks the OMS payload (after V2 header) and counts tags conservatively.
// It understands string and fixed-size fields to avoid false positives.
func parseTagCount(dec []byte) int {
    if len(dec) < 35 { return 0 }
    p := 35
    // Skip initial page URL string (len + bytes)
    if p+2 <= len(dec) {
        l := int(binary.BigEndian.Uint16(dec[p:p+2]))
        p += 2 + l
    }
    n := 0
    limit := len(dec)
    for p < limit {
        tag := dec[p]
        n++
        p++
        switch tag {
        case 'T':
            if p+2 > limit { return n }
            l := int(binary.BigEndian.Uint16(dec[p : p+2])); p += 2 + l
        case 'L':
            if p+2 > limit { return n }
            l := int(binary.BigEndian.Uint16(dec[p : p+2])); p += 2 + l
        case 'E','B','+','V','Q','l':
            // no payload
        case 'D':
            p += 2
        case 'R':
            p += 2
        case 'S':
            p += 4
        case 'J':
            p += 4
        case 'I':
            if p+8 > limit { return n }
            dl := int(binary.BigEndian.Uint16(dec[p+4 : p+6]))
            p += 8 + dl
        case 'k':
            p += 1 // type
            if p+2 > limit { return n }
            l := int(binary.BigEndian.Uint16(dec[p : p+2])); p += 2 + l
        case 'h':
            // two strings
            for i:=0;i<2;i++{ if p+2>limit{return n}; l:=int(binary.BigEndian.Uint16(dec[p:p+2])); p+=2+l }
        case 'x':
            p += 1 // cfg
            for i:=0;i<2;i++{ if p+2>limit{return n}; l:=int(binary.BigEndian.Uint16(dec[p:p+2])); p+=2+l }
        case 'p','u','i','b','e':
            for i:=0;i<2;i++{ if p+2>limit{return n}; l:=int(binary.BigEndian.Uint16(dec[p:p+2])); p+=2+l }
        case 'c','r':
            for i:=0;i<2;i++{ if p+2>limit{return n}; l:=int(binary.BigEndian.Uint16(dec[p:p+2])); p+=2+l }
            p += 1 // checked
        case 's':
            if p+2>limit { return n }; l:=int(binary.BigEndian.Uint16(dec[p:p+2])); p+=2+l // name
            if p+1>limit { return n }; p+=1 // multiple
            if p+2>limit { return n }; cnt:=int(binary.BigEndian.Uint16(dec[p:p+2])); p+=2
            // we don't consume options here; they will be seen as 'o' tags in loop
            _ = cnt
        case 'o':
            // value, label, selected
            for i:=0;i<2;i++{ if p+2>limit{return n}; l:=int(binary.BigEndian.Uint16(dec[p:p+2])); p+=2+l }
            p+=1
        default:
            // Unknown tag: bail out
            return n
        }
    }
    return n
}

// parseTags returns (count, tags list, tag frequency map)
func parseTags(dec []byte) (int, []byte, map[byte]int) {
    if len(dec) < 35 { return 0, nil, map[byte]int{} }
    p := 35
    if p+2 <= len(dec) {
        l := int(binary.BigEndian.Uint16(dec[p:p+2]))
        p += 2 + l
    }
    tags := make([]byte, 0, 256)
    counts := map[byte]int{}
    limit := len(dec)
    for p < limit {
        tag := dec[p]
        tags = append(tags, tag)
        counts[tag]++
        p++
        switch tag {
        case 'T':
            if p+2>limit { return len(tags), tags, counts }
            l := int(binary.BigEndian.Uint16(dec[p:p+2])); p+=2+l
        case 'L':
            if p+2>limit { return len(tags), tags, counts }
            l := int(binary.BigEndian.Uint16(dec[p:p+2])); p+=2+l
        case 'E','B','+','V','Q','l':
        case 'D': p+=2
        case 'R': p+=2
        case 'S': p+=4
        case 'J': p+=4
        case 'I': if p+8>limit { return len(tags), tags, counts }; dl := int(binary.BigEndian.Uint16(dec[p+4:p+6])); p+=8+dl
        case 'k':
            p+=1; if p+2>limit { return len(tags), tags, counts }
            l := int(binary.BigEndian.Uint16(dec[p:p+2])); p+=2+l
        case 'h':
            for i:=0;i<2;i++{ if p+2>limit { return len(tags), tags, counts }; l:=int(binary.BigEndian.Uint16(dec[p:p+2])); p+=2+l }
        case 'x':
            p+=1; for i:=0;i<2;i++{ if p+2>limit { return len(tags), tags, counts }; l:=int(binary.BigEndian.Uint16(dec[p:p+2])); p+=2+l }
        case 'p','u','i','b','e':
            for i:=0;i<2;i++{ if p+2>limit { return len(tags), tags, counts }; l:=int(binary.BigEndian.Uint16(dec[p:p+2])); p+=2+l }
        case 'c','r':
            for i:=0;i<2;i++{ if p+2>limit { return len(tags), tags, counts }; l:=int(binary.BigEndian.Uint16(dec[p:p+2])); p+=2+l }
            p+=1
        case 's':
            if p+2>limit { return len(tags), tags, counts }; l:=int(binary.BigEndian.Uint16(dec[p:p+2])); p+=2+l
            if p+1>limit { return len(tags), tags, counts }; p+=1
            if p+2>limit { return len(tags), tags, counts }; _=int(binary.BigEndian.Uint16(dec[p:p+2])); p+=2
        case 'o':
            for i:=0;i<2;i++{ if p+2>limit { return len(tags), tags, counts }; l:=int(binary.BigEndian.Uint16(dec[p:p+2])); p+=2+l }
            p+=1
        default:
            return len(tags), tags, counts
        }
    }
    return len(tags), tags, counts
}
var clientCookieJars sync.Map // key -> http.CookieJar

func jarKey(r *http.Request) string {
    // Use remote address without port + UA to group per device
    host, _, _ := net.SplitHostPort(r.RemoteAddr)
    if host == "" { host = r.RemoteAddr }
    return host + "|" + r.UserAgent()
}

func getClientJar(r *http.Request) http.CookieJar {
    key := jarKey(r)
    if v, ok := clientCookieJars.Load(key); ok {
        if j, ok2 := v.(http.CookieJar); ok2 { return j }
    }
    j, _ := cookiejar.New(nil)
    clientCookieJars.Store(key, j)
    return j
}

// ---------------------- Pagination cache ----------------------
type cacheEntry struct {
    Data       []byte
    SetCookies []string
    When       time.Time
}

var pageCache sync.Map

func cacheKey(target string, opt *oms.RenderOptions, hdr http.Header) string {
    if opt == nil { return target }
    ua := hdr.Get("User-Agent")
    lang := hdr.Get("Accept-Language")
    img := opt.ImageMIME + ":i=" + strconv.Itoa(btoi(opt.ImagesOn)) + ":q=" + strconv.Itoa(btoi(opt.HighQuality)) + ":w=" + strconv.Itoa(opt.ScreenW)
    return target + "|" + ua + "|" + lang + "|" + img
}

func btoi(b bool) int { if b { return 1 }; return 0 }

func cacheStore(target string, opt *oms.RenderOptions, hdr http.Header, page *oms.Page) {
    if opt == nil || page == nil || len(page.Data) == 0 { return }
    if opt.Page <= 1 { // only (re)init on first page
        k := cacheKey(target, opt, hdr)
        data := page.Data
        if len(page.CachePacked) > 0 { data = page.CachePacked }
        pageCache.Store(k, cacheEntry{Data: append([]byte(nil), data...), SetCookies: append([]string(nil), page.SetCookies...), When: time.Now()})
    }
}

func cacheSelect(target string, opt *oms.RenderOptions) ([]byte, int, int, bool) {
    if opt == nil || opt.Page <= 1 || opt.MaxTagsPerPage <= 0 { return nil, 0, 0, false }
    // Do not refresh; only serve if present
    k := cacheKey(target, opt, http.Header{})
    // Note: UA/lang were part of key on store; to make selection robust across handlers,
    // drop them by using empty header when checking. So try exact first, then relaxed.
    if v, ok := pageCache.Load(k); ok {
        ce := v.(cacheEntry)
        if raw, cur, cnt, err := oms.SelectOMSPartFromPacked(ce.Data, opt.Page, opt.MaxTagsPerPage); err == nil {
            return raw, cur, cnt, true
        }
    }
    // Relaxed: try without UA/lang
    k2 := target + "|" + opt.ImageMIME + ":i=" + strconv.Itoa(btoi(opt.ImagesOn)) + ":q=" + strconv.Itoa(btoi(opt.HighQuality)) + ":w=" + strconv.Itoa(opt.ScreenW)
    if v, ok := pageCache.Load(k2); ok {
        ce := v.(cacheEntry)
        if raw, cur, cnt, err := oms.SelectOMSPartFromPacked(ce.Data, opt.Page, opt.MaxTagsPerPage); err == nil {
            return raw, cur, cnt, true
        }
    }
    return nil, 0, 0, false
}



