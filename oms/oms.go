package oms

import (
    "bytes"
    "compress/flate"
    "compress/gzip"
    "compress/zlib"
    "encoding/base64"
    "encoding/binary"
    "crypto/sha1"
    _ "golang.org/x/image/webp"
    "image"
    _ "image/gif"
    "image/jpeg"
    "image/png"
    "io"
    "io/fs"
    "net/http"
    "net/url"
    "path/filepath"
    "sort"
    "strings"

    "golang.org/x/net/html"
    "log"
    "os"
    "strconv"
    "sync"
    "time"
)

// ---------------------- Minimal CSS support ----------------------

type cssRule struct {
    selType int    // 0=tag,1=class,2=id
    sel     string
    props   map[string]string
    order   int
    spec    int    // specificity weight
    // optional single ancestor requirement for descendant selector A B or child selector A > B
    ancType   int    // -1 = none; 0=tag,1=class,2=id
    anc       string
    ancSpec   int
    ancDirect bool
}

// isWhiteHex returns true if the color equals #ffffff (case-insensitive).
func isWhiteHex(hex string) bool {
    return strings.EqualFold(strings.TrimSpace(hex), "#ffffff")
}

// cssToHex normalizes common CSS color syntaxes into #rrggbb.
// Supports: #rgb/#rrggbb, black/white, rgb()/rgba() with 0-255 or % values.
func cssToHex(v string) string {
    s := strings.ToLower(strings.TrimSpace(v))
    if s == "" { return "" }
    if strings.HasPrefix(s, "#") {
        if len(s) == 4 { // #rgb -> #rrggbb
            r := string([]byte{s[1], s[1]})
            g := string([]byte{s[2], s[2]})
            b := string([]byte{s[3], s[3]})
            return "#" + r + g + b
        }
        if len(s) >= 7 { return s[:7] }
        return ""
    }
    switch s {
    case "black":
        return "#000000"
    case "white":
        return "#ffffff"
    case "transparent":
        return ""
    }
    if strings.HasPrefix(s, "rgb(") || strings.HasPrefix(s, "rgba(") {
        open := strings.IndexByte(s, '(')
        close := strings.IndexByte(s, ')')
        if open != -1 && close != -1 && close > open+1 {
            inner := s[open+1 : close]
            parts := strings.Split(inner, ",")
            if len(parts) < 3 { parts = strings.Fields(inner) }
            if len(parts) >= 3 {
                toByte := func(x string) int {
                    x = strings.TrimSpace(x)
                    if strings.HasSuffix(x, "%") {
                        x = strings.TrimSuffix(x, "%")
                        if p, err := strconv.Atoi(x); err == nil {
                            if p < 0 { p = 0 } else if p > 100 { p = 100 }
                            return int(float64(p) * 255.0 / 100.0)
                        }
                        return 0
                    }
                    if n, err := strconv.Atoi(x); err == nil {
                        if n < 0 { n = 0 } else if n > 255 { n = 255 }
                        return n
                    }
                    return 0
                }
                r := toByte(parts[0])
                g := toByte(parts[1])
                b := toByte(parts[2])
                hexd := "0123456789abcdef"
                out := make([]byte, 7)
                out[0] = '#'
                out[1] = hexd[r>>4]; out[2] = hexd[r&0xF]
                out[3] = hexd[g>>4]; out[4] = hexd[g&0xF]
                out[5] = hexd[b>>4]; out[6] = hexd[b&0xF]
                return string(out)
            }
        }
    }
    return ""
}

type Stylesheet struct { rules []cssRule }

func parseCSSText(txt string, startOrder int) ([]cssRule, int) {
    rules := make([]cssRule, 0, 16)
    s := txt
    i := 0
    order := startOrder

    for i < len(s) {
        // find selector
        bs := strings.IndexByte(s[i:], '{')
        if bs == -1 { break }
        sel := strings.TrimSpace(s[i : i+bs])
        i += bs + 1

        be := strings.IndexByte(s[i:], '}')
        if be == -1 { break }
        body := s[i : i+be]
        i += be + 1

        // split selectors by comma
        sels := strings.Split(sel, ",")

        // parse body into props
        props := map[string]string{}
        for _, part := range strings.Split(body, ";") {
            kv := strings.SplitN(part, ":", 2)
            if len(kv) != 2 { continue }
            k := strings.TrimSpace(strings.ToLower(kv[0]))
            v := strings.TrimSpace(strings.ToLower(kv[1]))
            if k != "" && v != "" { props[k] = v }
        }

        for _, one := range sels {
            t := strings.TrimSpace(one)

            // ancestor helpers
            __SET_ANCESTOR__ := ""
            __ANC_DIRECT__ := false // зарезервировано на случай поддержки '>' позже

            // strip simple pseudo-classes like :link, :visited, :hover, :active, :focus
            if idx := strings.IndexByte(t, ':'); idx != -1 {
                t = strings.TrimSpace(t[:idx])
            }

            // detect simple descendant selector "A B" (single space, no >)
            if strings.Contains(t, " ") && !strings.Contains(t, ">") {
                sp := strings.Fields(t)
                if len(sp) == 2 {
                    ancTok := strings.TrimSpace(sp[0])
                    t = strings.TrimSpace(sp[1])
                    // normalize anc token tag.class -> .class
                    if strings.Contains(ancTok, ".") && !strings.HasPrefix(ancTok, ".") && !strings.HasPrefix(ancTok, "#") {
                        parts := strings.Split(ancTok, ".")
                        if len(parts) > 1 { ancTok = "." + parts[len(parts)-1] }
                    }
                    __SET_ANCESTOR__ = ancTok
                }
            }

            // reduce tag.class to .class when possible
            if strings.Contains(t, ".") && !strings.HasPrefix(t, ".") && !strings.HasPrefix(t, "#") {
                parts := strings.Split(t, ".")
                if len(parts) > 1 { t = "." + parts[len(parts)-1] }
            }

            if t == "" || len(props) == 0 { continue }

            r := cssRule{
                props:     map[string]string{},
                order:     order,
                ancType:   -1,
                ancDirect: false,
            }
            for k, v := range props { r.props[k] = v }

            if __SET_ANCESTOR__ != "" {
                if strings.HasPrefix(__SET_ANCESTOR__, ".") {
                    r.ancType = 1
                    r.anc = strings.TrimPrefix(__SET_ANCESTOR__, ".")
                    r.ancSpec = 10
                } else if strings.HasPrefix(__SET_ANCESTOR__, "#") {
                    r.ancType = 2
                    r.anc = strings.TrimPrefix(__SET_ANCESTOR__, "#")
                    r.ancSpec = 100
                } else {
                    r.ancType = 0
                    r.anc = strings.ToLower(__SET_ANCESTOR__)
                    r.ancSpec = 1
                }
                r.ancDirect = __ANC_DIRECT__
            }

            switch {
            case strings.HasPrefix(t, "."):
                r.selType = 1; r.sel = strings.TrimPrefix(t, "."); r.spec = 10
            case strings.HasPrefix(t, "#"):
                r.selType = 2; r.sel = strings.TrimPrefix(t, "#"); r.spec = 100
            default:
                r.selType = 0; r.sel = strings.ToLower(t); r.spec = 1
            }

            rules = append(rules, r)
            order++
        }
    }
    return rules, order
}

func buildStylesheet(doc *html.Node, base string, hdr http.Header, jar http.CookieJar) *Stylesheet {
    ss := &Stylesheet{}
    order := 0
    // inline <style>
    var walk func(*html.Node)
    walk = func(n *html.Node) {
        if n.Type == html.ElementNode && strings.EqualFold(n.Data, "style") {
            if n.FirstChild != nil && n.FirstChild.Type == html.TextNode {
                if rs, ord := parseCSSText(n.FirstChild.Data, order); len(rs) > 0 { ss.rules = append(ss.rules, rs...); order = ord }
            }
        }
        for c := n.FirstChild; c != nil; c = c.NextSibling { walk(c) }
    }
    walk(doc)
    // linked <link rel=stylesheet>
    var collect func(*html.Node)
    links := []string{}
    collect = func(n *html.Node) {
        if n.Type == html.ElementNode && strings.EqualFold(n.Data, "link") {
            rel := strings.ToLower(strings.TrimSpace(getAttr(n, "rel")))
            if rel == "stylesheet" {
                if href := strings.TrimSpace(getAttr(n, "href")); href != "" {
                    abs := resolveAbsURL(base, href)
                    if abs != "" { links = append(links, abs) }
                }
            }
        }
        for c := n.FirstChild; c != nil; c = c.NextSibling { collect(c) }
    }
    collect(doc)
    // fetch a few small css files
    maxFiles := 5
    for i, u := range links {
        if i >= maxFiles { break }
        if b, ok := fetchText(u, hdr, jar, "text/css"); ok {
            if rs, ord := parseCSSText(string(b), order); len(rs) > 0 { ss.rules = append(ss.rules, rs...); order = ord }
        }
    }
    return ss
}

func resolveAbsURL(base, href string) string {
    bu, err := url.Parse(base)
    if err != nil { return "" }
    hu, err := url.Parse(href)
    if err != nil { return "" }
    return bu.ResolveReference(hu).String()
}

func fetchText(absURL string, hdr http.Header, jar http.CookieJar, accept string) ([]byte, bool) {
    req, err := http.NewRequest(http.MethodGet, absURL, nil)
    if err != nil { return nil, false }
    if accept == "" { accept = "text/*" }
    req.Header.Set("Accept", accept)
    if hdr != nil {
        if ua := hdr.Get("User-Agent"); ua != "" { req.Header.Set("User-Agent", ua) }
        if al := hdr.Get("Accept-Language"); al != "" { req.Header.Set("Accept-Language", al) }
        if ck := hdr.Get("Cookie"); ck != "" { req.Header.Set("Cookie", ck) }
        if ref := hdr.Get("Referer"); ref != "" { req.Header.Set("Referer", ref) }
    }
    hc := &http.Client{Timeout: 8 * time.Second}
    if jar != nil { hc.Jar = jar }
    resp, err := hc.Do(req)
    if err != nil { return nil, false }
    defer resp.Body.Close()
    var rc io.ReadCloser = resp.Body
    switch strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))) {
    case "gzip":
        if gr, e := gzip.NewReader(resp.Body); e == nil { rc = gr; defer gr.Close() }
    case "deflate":
        if zr, e := zlib.NewReader(resp.Body); e == nil { rc = zr; defer zr.Close() } else if fr := flate.NewReader(resp.Body); fr != nil { rc = io.NopCloser(fr); defer fr.Close() }
    }
    b, err := io.ReadAll(rc)
    if err != nil { return nil, false }
    return b, true
}

func shouldSkipWhiteFill(prop, val string) bool {
    p := strings.ToLower(strings.TrimSpace(prop))
    switch p {
    case "fill", "background-color":
        if isWhiteHex(cssToHex(val)) { return true }
    case "background":
        if isWhiteHex(cssToHex(val)) { return true }
    }
    return false
}

func computeStyleFor(n *html.Node, ss *Stylesheet) map[string]string {
    if ss == nil || n == nil || n.Type != html.ElementNode { return nil }
    type pv struct{ spec, order int; val string }
    props := map[string]pv{}

    tag := strings.ToLower(n.Data)
    classes := strings.Fields(strings.ToLower(strings.TrimSpace(getAttr(n, "class"))))
    id := strings.ToLower(strings.TrimSpace(getAttr(n, "id")))

    // match helper for node against simple selector
    matchNode := func(nd *html.Node, st int, sel string) bool {
        if nd == nil || nd.Type != html.ElementNode || st < 0 { return false }
        switch st {
        case 0: // tag
            return strings.ToLower(nd.Data) == sel
        case 1: // class
            cls := strings.Fields(strings.ToLower(strings.TrimSpace(getAttr(nd, "class"))))
            for _, c := range cls { if c == sel { return true } }
            return false
        case 2: // id
            return strings.ToLower(strings.TrimSpace(getAttr(nd, "id"))) == sel
        }
        return false
    }

    apply := func(k, v string, spec, ord int) {
        if k == "" || v == "" { return }
        // NEW: если это заливка/фон и цвет белый — пропускаем
        if shouldSkipWhiteFill(k, v) { return }

        if cur, ok := props[k]; !ok || spec > cur.spec || (spec == cur.spec && ord >= cur.order) {
            props[k] = pv{spec: spec, order: ord, val: v}
        }
    }

    for _, r := range ss.rules {
        needAnc := r.ancType >= 0 && r.anc != ""
        switch r.selType {
        case 0:
            if r.sel == tag {
                if !needAnc { for k, v := range r.props { apply(k, v, r.spec, r.order) } } else {
                    if r.ancDirect {
                        if matchNode(n.Parent, r.ancType, r.anc) { for k, v := range r.props { apply(k, v, r.spec+r.ancSpec, r.order) } }
                    } else {
                        for a := n.Parent; a != nil; a = a.Parent {
                            if matchNode(a, r.ancType, r.anc) { for k, v := range r.props { apply(k, v, r.spec+r.ancSpec, r.order) }; break }
                        }
                    }
                }
            }
        case 1:
            for _, c := range classes {
                if r.sel == c {
                    if !needAnc { for k, v := range r.props { apply(k, v, r.spec, r.order) } } else {
                        for a := n.Parent; a != nil; a = a.Parent {
                            if matchNode(a, r.ancType, r.anc) { for k, v := range r.props { apply(k, v, r.spec+r.ancSpec, r.order) }; break }
                        }
                    }
                }
            }
        case 2:
            if id != "" && r.sel == id {
                if !needAnc { for k, v := range r.props { apply(k, v, r.spec, r.order) } } else {
                    if r.ancDirect {
                        if matchNode(n.Parent, r.ancType, r.anc) { for k, v := range r.props { apply(k, v, r.spec+r.ancSpec, r.order) } }
                    } else {
                        for a := n.Parent; a != nil; a = a.Parent {
                            if matchNode(a, r.ancType, r.anc) { for k, v := range r.props { apply(k, v, r.spec+r.ancSpec, r.order) }; break }
                        }
                    }
                }
            }
        }
    }

    // inline style overrides all
    if st := strings.TrimSpace(getAttr(n, "style")); st != "" {
        for _, part := range strings.Split(st, ";") {
            kv := strings.SplitN(part, ":", 2)
            if len(kv) != 2 { continue }
            k := strings.TrimSpace(strings.ToLower(kv[0]))
            v := strings.TrimSpace(strings.ToLower(kv[1]))
            if k == "" || v == "" { continue }

            // NEW: пропускаем белую заливку и для inline
            if shouldSkipWhiteFill(k, v) { continue }

            props[k] = pv{spec: 1000, order: 1<<30, val: v}
        }
    }

    if len(props) == 0 { return nil }
    out := map[string]string{}
    for k, vv := range props { out[k] = vv.val }
    return out
}

// ---------------------- Disk image cache ----------------------

var (
    diskCacheOnce sync.Once
    diskCacheDir  string
    diskCacheMax  int64
    diskCacheMu   sync.Mutex
)

func initDiskCache() {
    diskCacheDir = os.Getenv("OMS_IMG_CACHE_DIR")
    if diskCacheDir == "" { diskCacheDir = filepath.Join("cache", "img") }
    if err := os.MkdirAll(diskCacheDir, 0o755); err != nil { /* ignore */ }
    mb := 200
    if s := os.Getenv("OMS_IMG_CACHE_MB"); s != "" {
        if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && v >= 0 { mb = v }
    }
    diskCacheMax = int64(mb) * 1024 * 1024
}

func diskKey(format string, quality int, url string) (string, string) {
    h := sha1.Sum([]byte(format + "|q=" + strconv.Itoa(quality) + "|" + url))
    hex := make([]byte, 40)
    const hexd = "0123456789abcdef"
    for i, b := range h[:] { hex[i*2] = hexd[b>>4]; hex[i*2+1] = hexd[b&0xF] }
    dir := filepath.Join(diskCacheDir, string(hex[0]), string(hex[1]))
    return dir, filepath.Join(dir, string(hex)+".bin")
}

func diskCacheGet(format string, quality int, url string) ([]byte, int, int, bool) {
    diskCacheOnce.Do(initDiskCache)
    dir, path := diskKey(format, quality, url)
    f, err := os.Open(path)
    if err != nil { return nil, 0, 0, false }
    defer f.Close()
    header := make([]byte, 4)
    if _, err := io.ReadFull(f, header); err != nil { return nil, 0, 0, false }
    w := int(binary.BigEndian.Uint16(header[0:2]))
    h := int(binary.BigEndian.Uint16(header[2:4]))
    b, err := io.ReadAll(f)
    if err != nil { return nil, 0, 0, false }
    // touch mtime to approximate LRU
    _ = os.Chtimes(path, time.Now(), time.Now())
    _ = os.MkdirAll(dir, 0o755)
    return b, w, h, true
}

func diskCachePut(format string, quality int, url string, data []byte, w, h int) {
    diskCacheOnce.Do(initDiskCache)
    dir, path := diskKey(format, quality, url)
    if err := os.MkdirAll(dir, 0o755); err != nil { return }
    tmp := path + ".tmp"
    f, err := os.Create(tmp)
    if err != nil { return }
    var hdr [4]byte
    binary.BigEndian.PutUint16(hdr[0:2], uint16(w))
    binary.BigEndian.PutUint16(hdr[2:4], uint16(h))
    _, _ = f.Write(hdr[:])
    _, _ = f.Write(data)
    _ = f.Close()
    _ = os.Rename(tmp, path)
    // async prune
    go pruneDiskCache()
}

func pruneDiskCache() {
    diskCacheMu.Lock()
    defer diskCacheMu.Unlock()
    // scan total size
    var files []struct{ p string; sz int64; mt time.Time }
    var total int64
    filepath.WalkDir(diskCacheDir, func(p string, d fs.DirEntry, err error) error {
        if err != nil { return nil }
        if d.IsDir() { return nil }
        if !strings.HasSuffix(strings.ToLower(p), ".bin") { return nil }
        if info, e := d.Info(); e == nil {
            files = append(files, struct{ p string; sz int64; mt time.Time }{p, info.Size(), info.ModTime()})
            total += info.Size()
        }
        return nil
    })
    if total <= diskCacheMax || diskCacheMax <= 0 { return }
    // sort by mtime asc
    sort.Slice(files, func(i, j int) bool { return files[i].mt.Before(files[j].mt) })
    for _, f := range files {
        if total <= diskCacheMax { break }
        _ = os.Remove(f.p)
        total -= f.sz
    }
}

// Version is the magic stored in the OMS header.
const Version = 0x3218

// Heuristic default text style for OM2 streams (no bold by default)
const (
	styleDefault   uint32 = 0x00000000
	styleBoldBit   uint32 = 0x00000001
	styleItalicBit uint32 = 0x00000004
	styleUnderBit  uint32 = 0x00000008
	styleCenterBit uint32 = 0x00000010
	styleRightBit  uint32 = 0x00000020
)

// StyleDefault is an exported alias for external callers.
const StyleDefault = styleDefault

// Page represents loaded page data.
// Data holds the final binary OMS representation.
type Page struct {
    Data       []byte
    CachePacked []byte
    tagCount   int
    strCount   int
    SetCookies []string
    partCur    int
    partCnt    int
}

// NewPage allocates an empty page.
func NewPage() *Page { return &Page{} }

func (p *Page) addData(b []byte) { p.Data = append(p.Data, b...) }

func (p *Page) addTag(tag byte) {
	p.addData([]byte{tag})
	p.tagCount++
}

// AddString stores a string prefixed with its big-endian length.
func (p *Page) AddString(s string) {
	var lenBuf [2]byte
	binary.BigEndian.PutUint16(lenBuf[:], uint16(len(s)))
	p.addData(lenBuf[:])
	p.addData([]byte(s))
	p.strCount++
}

func (p *Page) addAuthdata(data string, typ byte) {
	p.addTag('k')
	p.addData([]byte{typ})
	p.AddString(data)
}

// AddAuthprefix adds an auth prefix tag.
func (p *Page) AddAuthprefix(prefix string) { p.addAuthdata(prefix, 0) }

// AddAuthcode adds an auth code tag.
func (p *Page) AddAuthcode(code string) { p.addAuthdata(code, 1) }

func ch(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	}
	return 0
}

func calcColor(color string) uint16 {
	if len(color) < 7 || color[0] != '#' {
		return 0
	}
	r := (ch(color[1])<<4 | ch(color[2])) >> 3
	g := (ch(color[3])<<4 | ch(color[4])) >> 2
	b := (ch(color[5])<<4 | ch(color[6])) >> 3
	return uint16(r) | uint16(g)<<5 | uint16(b)<<11
}

// AddTextcolor applies text color via style tag.
func (p *Page) AddTextcolor(color string) { p.AddStyle(uint32(calcColor(color)) << 8) }

// AddStyle appends style information.
func (p *Page) AddStyle(style uint32) {
	p.addTag('S')
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], style)
	p.addData(buf[:])
}

// AddText appends text content.
func (p *Page) AddText(text string) {
	text = strings.TrimLeft(text, "\r\n")
	if text == "" {
		return
	}
	p.addTag('T')
	p.AddString(text)
}

// AddBreak adds a line break tag.
func (p *Page) AddBreak() { p.addTag('B') }

// AddLink adds a hyperlink with accompanying text.
func (p *Page) AddLink(url, text string) {
	p.addTag('L')
	p.AddString(url)
	p.AddText(text)
	p.AddBreak()
	p.addTag('E')
}

// AddBgcolor sets the page background color.
func (p *Page) AddBgcolor(color string) {
	if color == "" {
		return
	}
	p.addTag('D')
	var buf [2]byte
	binary.BigEndian.PutUint16(buf[:], calcColor(color))
	p.addData(buf[:])
}

// AddHr writes a horizontal line (tag 'R') with optional color.
// If color is empty, defaults to black (0).
func (p *Page) AddHr(color string) {
	p.addTag('R')
	var c uint16
	if color == "" {
		c = 0
	} else {
		c = calcColor(color)
	}
	var buf [2]byte
	binary.BigEndian.PutUint16(buf[:], c)
	p.addData(buf[:])
}

// AddImagePlaceholder writes tag 'J' with width and height (uint16 BE).
func (p *Page) AddImagePlaceholder(width, height int) {
	if width < 0 {
		width = 0
	}
	if height < 0 {
		height = 0
	}
	p.addTag('J')
	var buf [4]byte
	binary.BigEndian.PutUint16(buf[0:2], uint16(width))
	binary.BigEndian.PutUint16(buf[2:4], uint16(height))
	p.addData(buf[:])
}

// AddImageInline writes tag 'I' (PNG/JPEG bytes) with dimensions.
// The actual codec is implied by the client request (k=image/jpeg/png).
func (p *Page) AddImageInline(width, height int, data []byte) {
	if width < 0 {
		width = 0
	}
	if height < 0 {
		height = 0
	}
	p.addTag('I')
	var hdr [8]byte
	binary.BigEndian.PutUint16(hdr[0:2], uint16(width))
	binary.BigEndian.PutUint16(hdr[2:4], uint16(height))
	binary.BigEndian.PutUint16(hdr[4:6], uint16(len(data)))
	// rsrvd
	binary.BigEndian.PutUint16(hdr[6:8], 0)
	p.addData(hdr[:])
	p.addData(data)
}

// AddPlus inserts a block separator tag.
func (p *Page) AddPlus() { p.addTag('+') }

// AddParagraph inserts a paragraph tag.
func (p *Page) AddParagraph() { p.addTag('V') }

// AddForm begins a form description.
func (p *Page) AddForm(action string) {
	p.addTag('h')
	if action == "" {
		p.AddString("1")
	} else {
		p.AddString(action)
	}
	p.AddString("1")
}

// AddTextInput adds a text input field.
func (p *Page) AddTextInput(name, value string) {
	p.addTag('x')
	// Config byte (protocol extension); keep 0 by default
	p.addData([]byte{0})
	p.AddString(name)
	p.AddString(value)
}

// AddPassInput adds a password input field.
func (p *Page) AddPassInput(name, value string) {
	p.addTag('p')
	p.AddString(name)
	p.AddString(value)
}

// AddCheckbox adds a checkbox control: tag 'c' + name + value + 1-byte checked.
func (p *Page) AddCheckbox(name, value string, checked bool) {
	p.addTag('c')
	p.AddString(name)
	p.AddString(value)
	if checked {
		p.addData([]byte{1})
	} else {
		p.addData([]byte{0})
	}
}

func (p *Page) AddSubmit(name, value string) {
	p.addTag('u')
	p.AddString(name)
	p.AddString(value)
}

// AddRadio adds a radio button field (name, value, checked).
func (p *Page) AddRadio(name, value string, checked bool) {
	p.addTag('r')
	p.AddString(name)
	p.AddString(value)
	if checked {
		p.addData([]byte{1})
	} else {
		p.addData([]byte{0})
	}
}

// AddHidden adds a hidden input field.
func (p *Page) AddHidden(name, value string) {
	p.addTag('i')
	p.AddString(name)
	p.AddString(value)
}

// BeginSelect starts a select control (name, multiple, option count).
func (p *Page) BeginSelect(name string, multiple bool, count int) {
	p.addTag('s')
	p.AddString(name)
	if multiple {
		p.addData([]byte{1})
	} else {
		p.addData([]byte{0})
	}
	var buf [2]byte
	binary.BigEndian.PutUint16(buf[:], uint16(count))
	p.addData(buf[:])
}

// AddOption appends an option to the current select control.
// Order per client reader: value first, then label, then selected flag.
func (p *Page) AddOption(value, label string, selected bool) {
	p.addTag('o')
	p.AddString(value)
	p.AddString(label)
	if selected {
		p.addData([]byte{1})
	} else {
		p.addData([]byte{0})
	}
}

// EndSelect closes a select block.
func (p *Page) EndSelect() { p.addTag('l') }

// AddButton adds a generic button control.
func (p *Page) AddButton(name, value string) {
	p.addTag('b')
	p.AddString(name)
	p.AddString(value)
}

// AddReset adds a reset button control.
func (p *Page) AddReset(name, value string) {
	p.addTag('e')
	p.AddString(name)
	p.AddString(value)
}

type v2Header struct {
	Res1        [9]uint16
	TagCount    uint16
	PartCurrent uint16
	PartCount   uint16
	Res2        uint16
	StagCount   uint16
	Res3        uint16
	Res4        uint8
	Cachable    uint16
	Res5        uint16
}

func (p *Page) finalize() {
	// Append end-of-page marker like the C implementation
	p.addTag('Q')
	// Derive TagCount by scanning payload to avoid mismatches
	base := computeTagCount(p.Data)
	cnt := adjustTagCount(base)
	// If no override set, bump by +1 to avoid OM2 AIOOBE on some pages
	if os.Getenv("OMS_TAGCOUNT_MODE") == "" && os.Getenv("OMS_TAGCOUNT_DELTA") == "" {
		cnt++
	}

	// Derive string count and use swapped value for robust client handling
	stag := p.strCount + 1

	// РџРѕРјРѕС‰РЅРёРє РґР»СЏ "СЃРІРѕРїР°" РєР°Рє Сѓ TagCount:
	swap16 := func(v int) uint16 {
		x := uint16(v & 0xFFFF)
		return (x<<8)&0xFF00 | (x>>8)&0x00FF
	}

    pc := 1
    pt := 1
    if p.partCur > 0 { pc = p.partCur }
    if p.partCnt > 0 { pt = p.partCnt }

    v2 := v2Header{
        // TagCount matches legacy C implementation and includes the final 'Q'
        TagCount:    swap16(cnt),
        PartCurrent: swap16(pc),
        PartCount:   swap16(pt),
        StagCount:   swap16(stag),
        Cachable:    0xFFFF,
    }

	var comp bytes.Buffer
	w, _ := flate.NewWriter(&comp, flate.DefaultCompression)
	_ = binary.Write(w, binary.LittleEndian, &v2)
	w.Write(p.Data)
	w.Close()
	size := 6 + comp.Len()
	header := make([]byte, 6)
	binary.LittleEndian.PutUint16(header[:2], Version)
	binary.BigEndian.PutUint32(header[2:], uint32(size))
	p.Data = append(header, comp.Bytes()...)
}

// Normalize ensures the final OMS payload strictly follows conservative
// compatibility rules used by test pages: last tag 'Q', tag_count = parsed+1,
// stag_count = 0x0400. It rewrites the V2 header if necessary.
func (p *Page) Normalize() {
	nb, err := NormalizeOMS(p.Data)
	if err == nil && nb != nil {
		p.Data = nb
	}
}

// NormalizeOMS adjusts an OMS response bytes.
func NormalizeOMS(b []byte) ([]byte, error) {
	if len(b) < 6 {
		return b, nil
	}
	if binary.LittleEndian.Uint16(b[:2]) != Version {
		return b, nil
	}
	// Inflate body
	fr := flate.NewReader(bytes.NewReader(b[6:]))
	dec, err := io.ReadAll(fr)
	fr.Close()
	if err != nil {
		return b, nil
	}
	if len(dec) < 35 {
		return b, nil
	}

	// Ensure payload ends with 'Q'
	if dec[len(dec)-1] != 'Q' {
		dec = append(dec, 'Q')
	}

	// Compute tag count from dec stream
	parsed := parseTagCountFromDec(dec)
	if parsed < 1 {
		parsed = 1
	}
	wantCnt := parsed + 1 // conservative +1
	// Overwrite tag_count (byte-swapped) at V2 offset 18:20 (LE field)
	swap := func(v uint16) uint16 { return (v<<8)&0xFF00 | (v>>8)&0x00FF }
	binary.LittleEndian.PutUint16(dec[18:20], swap(uint16(wantCnt)))
    // Preserve stag_count as authored during finalize (do not override)

	// Repack: deflate + write common header
	var comp bytes.Buffer
	w, _ := flate.NewWriter(&comp, flate.DefaultCompression)
	_, _ = w.Write(dec)
	_ = w.Close()
	size := 6 + comp.Len()
	header := make([]byte, 6)
	binary.LittleEndian.PutUint16(header[:2], Version)
	binary.BigEndian.PutUint32(header[2:], uint32(size))
	out := append(header, comp.Bytes()...)
	return out, nil
}

// parseTagCountFromDec walks the inflated stream (starting with V2 header)
// and returns number of tags in the payload.
func parseTagCountFromDec(dec []byte) int {
	if len(dec) < 35 {
		return 0
	}
	p := 35
	// Skip initial page URL string (len + bytes)
	if p+2 <= len(dec) {
		l := int(binary.BigEndian.Uint16(dec[p : p+2]))
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
			if p+2 > limit {
				return n
			}
			l := int(binary.BigEndian.Uint16(dec[p : p+2]))
			p += 2 + l
		case 'L':
			if p+2 > limit {
				return n
			}
			l := int(binary.BigEndian.Uint16(dec[p : p+2]))
			p += 2 + l
		case 'E', 'B', '+', 'V', 'Q', 'l':
		case 'D':
			p += 2
		case 'R':
			p += 2
		case 'S':
			p += 4
		case 'J':
			p += 4
		case 'I':
			if p+8 > limit {
				return n
			}
			dl := int(binary.BigEndian.Uint16(dec[p+4 : p+6]))
			p += 8 + dl
		case 'k':
			p += 1
			if p+2 > limit {
				return n
			}
			l := int(binary.BigEndian.Uint16(dec[p : p+2]))
			p += 2 + l
		case 'h':
			for i := 0; i < 2; i++ {
				if p+2 > limit {
					return n
				}
				l := int(binary.BigEndian.Uint16(dec[p : p+2]))
				p += 2 + l
			}
		case 'x':
			p += 1
			for i := 0; i < 2; i++ {
				if p+2 > limit {
					return n
				}
				l := int(binary.BigEndian.Uint16(dec[p : p+2]))
				p += 2 + l
			}
		case 'p', 'u', 'i', 'b', 'e':
			for i := 0; i < 2; i++ {
				if p+2 > limit {
					return n
				}
				l := int(binary.BigEndian.Uint16(dec[p : p+2]))
				p += 2 + l
			}
		case 'c', 'r':
			for i := 0; i < 2; i++ {
				if p+2 > limit {
					return n
				}
				l := int(binary.BigEndian.Uint16(dec[p : p+2]))
				p += 2 + l
			}
			p += 1
		case 's':
			if p+2 > limit {
				return n
			}
			l := int(binary.BigEndian.Uint16(dec[p : p+2]))
			p += 2 + l
			if p+1 > limit {
				return n
			}
			p += 1
			if p+2 > limit {
				return n
			}
			_ = int(binary.BigEndian.Uint16(dec[p : p+2]))
			p += 2
		case 'o':
			for i := 0; i < 2; i++ {
				if p+2 > limit {
					return n
				}
				l := int(binary.BigEndian.Uint16(dec[p : p+2]))
				p += 2 + l
			}
			p += 1
		default:
			return n
		}
	}
	return n
}

// computeTagCount scans the payload (p.Data) and counts tags conservatively.
// It skips the initial OMS_STRING with page URL and then walks tagged payload.
func computeTagCount(b []byte) int {
	if len(b) < 2 {
		return 0
	}
	p := 0
	// Skip initial URL string
	l := int(binary.BigEndian.Uint16(b[p : p+2]))
	p += 2 + l
	n := 0
	limit := len(b)
	for p < limit {
		tag := b[p]
		n++
		p++
		switch tag {
		case 'T':
			if p+2 > limit {
				return n
			}
			l := int(binary.BigEndian.Uint16(b[p : p+2]))
			p += 2 + l
		case 'L':
			if p+2 > limit {
				return n
			}
			l := int(binary.BigEndian.Uint16(b[p : p+2]))
			p += 2 + l
		case 'E', 'B', '+', 'V', 'Q', 'l':
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
			if p+8 > limit {
				return n
			}
			dl := int(binary.BigEndian.Uint16(b[p+4 : p+6]))
			p += 8 + dl
		case 'k':
			p += 1 // type
			if p+2 > limit {
				return n
			}
			l := int(binary.BigEndian.Uint16(b[p : p+2]))
			p += 2 + l
		case 'h':
			// two strings
			for i := 0; i < 2; i++ {
				if p+2 > limit {
					return n
				}
				l := int(binary.BigEndian.Uint16(b[p : p+2]))
				p += 2 + l
			}
		case 'x':
			p += 1 // cfg
			for i := 0; i < 2; i++ {
				if p+2 > limit {
					return n
				}
				l := int(binary.BigEndian.Uint16(b[p : p+2]))
				p += 2 + l
			}
		case 'p', 'u', 'i', 'b', 'e':
			for i := 0; i < 2; i++ {
				if p+2 > limit {
					return n
				}
				l := int(binary.BigEndian.Uint16(b[p : p+2]))
				p += 2 + l
			}
		case 'c', 'r':
			for i := 0; i < 2; i++ {
				if p+2 > limit {
					return n
				}
				l := int(binary.BigEndian.Uint16(b[p : p+2]))
				p += 2 + l
			}
			p += 1 // checked
		case 's':
			if p+2 > limit {
				return n
			}
			l := int(binary.BigEndian.Uint16(b[p : p+2]))
			p += 2 + l // name
			if p+1 > limit {
				return n
			}
			p += 1 // multiple
			if p+2 > limit {
				return n
			}
			_ = int(binary.BigEndian.Uint16(b[p : p+2]))
			p += 2
		case 'o':
			// value, label, selected
			for i := 0; i < 2; i++ {
				if p+2 > limit {
					return n
				}
				l := int(binary.BigEndian.Uint16(b[p : p+2]))
				p += 2 + l
			}
			p += 1
		default:
			// Unknown tag: stop counting
			return n
		}
	}
	return n
}

func adjustTagCount(base int) int {
	// Strategy selection via env OMS_TAGCOUNT_MODE: exact|exclude_q|plus1|plus2
	// Or numeric delta via OMS_TAGCOUNT_DELTA (>=0)
	if m := os.Getenv("OMS_TAGCOUNT_MODE"); m != "" {
		switch m {
		case "exact":
			return base
		case "exclude_q":
			if base > 0 {
				return base - 1
			}
			return base
		case "plus1":
			return base + 1
		case "plus2":
			return base + 2
		}
	}
	if s := os.Getenv("OMS_TAGCOUNT_DELTA"); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v >= 0 {
			return base + v
		}
	}
	// Р”Р»СЏ Opera Mini 2.06 РЅСѓР¶РµРЅ С‚РѕС‡РЅС‹Р№ СЃС‡С‘С‚С‡РёРє (РІРєР»СЋС‡Р°СЏ С„РёРЅР°Р»СЊРЅС‹Р№ 'Q')
	return base
}

// Finalize exposes page finalization for external callers.
// It wraps the internal finalize to build the complete OMS payload.
func (p *Page) Finalize() { p.finalize() }

// ---------------------- Pagination helpers ----------------------

// splitByTags splits a raw payload (without V2 header) into parts with at most
// maxTags tags each. Each part starts with the original leading OMS string (URL).
func splitByTags(b []byte, maxTags int) [][]byte {
    if maxTags <= 0 || len(b) < 2 {
        return [][]byte{b}
    }
    // Prefix is initial page URL string (len + bytes)
    if len(b) < 2 { return [][]byte{b} }
    l := int(binary.BigEndian.Uint16(b[0:2]))
    if 2+l > len(b) { return [][]byte{b} }
    prefix := make([]byte, 2+l)
    copy(prefix, b[:2+l])
    p := 2 + l
    start := p
    tags := 0
    limit := len(b)
    parts := make([][]byte, 0, 2)
    for p < limit {
        tag := b[p]
        p++
        switch tag {
        case 'T', 'L':
            if p+2 > limit { p = limit; break }
            l := int(binary.BigEndian.Uint16(b[p : p+2]))
            p += 2 + l
        case 'E', 'B', '+', 'V', 'Q', 'l':
            // no payload
        case 'D', 'R':
            p += 2
        case 'S', 'J':
            p += 4
        case 'I':
            if p+8 > limit { p = limit; break }
            dl := int(binary.BigEndian.Uint16(b[p+4 : p+6]))
            p += 8 + dl
        case 'k':
            p += 1
            if p+2 > limit { p = limit; break }
            l := int(binary.BigEndian.Uint16(b[p : p+2]))
            p += 2 + l
        case 'h':
            for i := 0; i < 2; i++ {
                if p+2 > limit { p = limit; break }
                l := int(binary.BigEndian.Uint16(b[p : p+2]))
                p += 2 + l
            }
        case 'x':
            p += 1
            for i := 0; i < 2; i++ {
                if p+2 > limit { p = limit; break }
                l := int(binary.BigEndian.Uint16(b[p : p+2]))
                p += 2 + l
            }
        case 'p', 'u', 'i', 'b', 'e':
            for i := 0; i < 2; i++ {
                if p+2 > limit { p = limit; break }
                l := int(binary.BigEndian.Uint16(b[p : p+2]))
                p += 2 + l
            }
        case 'c', 'r':
            for i := 0; i < 2; i++ {
                if p+2 > limit { p = limit; break }
                l := int(binary.BigEndian.Uint16(b[p : p+2]))
                p += 2 + l
            }
            p += 1
        case 's':
            if p+2 > limit { p = limit; break }
            l := int(binary.BigEndian.Uint16(b[p : p+2]))
            p += 2 + l
            if p+1 > limit { p = limit; break }
            p += 1
            if p+2 > limit { p = limit; break }
            p += 2 // count (ignored here)
        case 'o':
            for i := 0; i < 2; i++ {
                if p+2 > limit { p = limit; break }
                l := int(binary.BigEndian.Uint16(b[p : p+2]))
                p += 2 + l
            }
            p += 1
        default:
            // Unknown: stop further splitting
            p = limit
        }
        tags++
        if tags >= maxTags {
            // Cut part [start:p)
            chunk := append([]byte(nil), b[start:p]...)
            part := append(append([]byte(nil), prefix...), chunk...)
            parts = append(parts, part)
            start = p
            tags = 0
        }
    }
    if start < limit {
        part := append(append([]byte(nil), prefix...), b[start:limit]...)
        parts = append(parts, part)
    }
    if len(parts) == 0 { return [][]byte{b} }
    return parts
}

// ---------------------- Image cache (LRU by bytes) ----------------------

type imgEntry struct {
    key      string
    data     []byte
    w, h     int
    prev,next *imgEntry
}

type imgLRU struct {
    mu   sync.Mutex
    max  int64
    size int64
    m    map[string]*imgEntry
    head *imgEntry
    tail *imgEntry
}

func newImgLRU(max int64) *imgLRU {
    return &imgLRU{max: max, m: map[string]*imgEntry{}}
}

func (c *imgLRU) moveFront(e *imgEntry) {
    if c.head == e { return }
    if e.prev != nil { e.prev.next = e.next }
    if e.next != nil { e.next.prev = e.prev }
    if c.tail == e { c.tail = e.prev }
    e.prev = nil
    e.next = c.head
    if c.head != nil { c.head.prev = e }
    c.head = e
    if c.tail == nil { c.tail = e }
}

func (c *imgLRU) get(key string) ([]byte, int, int, bool) {
    if c == nil { return nil, 0, 0, false }
    c.mu.Lock(); defer c.mu.Unlock()
    if e, ok := c.m[key]; ok {
        c.moveFront(e)
        return append([]byte(nil), e.data...), e.w, e.h, true
    }
    return nil, 0, 0, false
}

func (c *imgLRU) put(key string, data []byte, w, h int) {
    if c == nil || c.max <= 0 { return }
    c.mu.Lock(); defer c.mu.Unlock()
    if e, ok := c.m[key]; ok {
        c.size -= int64(len(e.data))
        e.data = append([]byte(nil), data...)
        e.w, e.h = w, h
        c.size += int64(len(e.data))
        c.moveFront(e)
    } else {
        e := &imgEntry{key: key, data: append([]byte(nil), data...), w: w, h: h}
        e.next = c.head
        if c.head != nil { c.head.prev = e }
        c.head = e
        if c.tail == nil { c.tail = e }
        c.m[key] = e
        c.size += int64(len(e.data))
    }
    for c.size > c.max && c.tail != nil {
        old := c.tail
        delete(c.m, old.key)
        c.size -= int64(len(old.data))
        c.tail = old.prev
        if c.tail != nil { c.tail.next = nil } else { c.head = nil }
    }
}

var globalImgCache = func() *imgLRU {
    mb := 200
    if s := os.Getenv("OMS_IMG_CACHE_MB"); s != "" {
        if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && v >= 0 { mb = v }
    }
    if mb <= 0 { return nil }
    return newImgLRU(int64(mb) * 1024 * 1024)
}()

func imgCacheKey(format string, quality int, url string) string {
    return format + "|q=" + strconv.Itoa(quality) + "|" + url
}

func imgCacheGet(format string, quality int, url string) ([]byte, int, int, bool) {
    if globalImgCache == nil { return nil, 0, 0, false }
    return globalImgCache.get(imgCacheKey(format, quality, url))
}

func imgCachePut(format string, quality int, url string, data []byte, w, h int) {
    if globalImgCache == nil { return }
    globalImgCache.put(imgCacheKey(format, quality, url), data, w, h)
}

func addHeader(p *Page) {
	p.AddString("1/internal:error")
	p.AddAuthcode("c37c206d2c235978d086b64c39a2fc17df68dbdd5dc04dd8b199177f95be6181")
	p.AddAuthprefix("t19-12")
	p.AddStyle(styleDefault)
	p.AddPlus()
	p.AddText("Internal server error")
}

func errorPage(url, reason string) *Page {
	p := NewPage()
	addHeader(p)
	p.AddText(reason)
	p.AddBreak()
	p.AddText(url)
	p.finalize()
	return p
}

// LoadPage loads the given URL and converts it into OMS format.
// On network or parse errors an error page is returned instead.
func LoadPage(oURL string) (*Page, error) {
	return LoadPageWithHeaders(oURL, nil)
}

// ---------------------- Charset helpers ----------------------

// decodeLegacyToUTF8 converts known legacy encodings to UTF-8 based on header or meta hints.
// Currently supports windows-1251; others fall back to raw bytes.
func decodeLegacyToUTF8(body []byte, contentType string) []byte {
    cs := sniffCharset(body, contentType)
    lcs := strings.ToLower(strings.TrimSpace(cs))
    switch lcs {
    case "windows-1251", "cp1251", "win-1251", "charset=windows-1251":
        return cp1251ToUTF8(body)
    case "koi8-r", "koi8r", "charset=koi8-r":
        return koi8rToUTF8(body)
    default:
        return body
    }
}

func sniffCharset(body []byte, contentType string) string {
    s := strings.ToLower(contentType)
    if i := strings.Index(s, "charset="); i != -1 {
        v := s[i+8:]
        // trim separators and quotes
        for j, c := range v {
            if c == ';' || c == ' ' || c == '"' || c == '\'' { v = v[:j]; break }
        }
        return strings.TrimSpace(v)
    }
    // Search first 2KB of body for meta charset
    n := len(body)
    if n > 2048 { n = 2048 }
    low := strings.ToLower(string(body[:n]))
    if i := strings.Index(low, "charset="); i != -1 {
        v := low[i+8:]
        for j, c := range v {
            if c == ';' || c == ' ' || c == '"' || c == '\'' || c == '>' { v = v[:j]; break }
        }
        return strings.TrimSpace(v)
    }
    return "utf-8"
}

// cp1251ToUTF8 decodes Windows-1251 bytes into UTF-8 (subset sufficient for Cyrillic text).
func cp1251ToUTF8(b []byte) []byte {
    var out bytes.Buffer
    for _, c := range b {
        switch {
        case c < 0x80:
            out.WriteByte(c)
        case c >= 0xC0 && c <= 0xDF: // Рђ..РЇ
            ci := int(c); r := rune(0x0410 + (ci - 0xC0))
            out.WriteRune(r)
        case c >= 0xE0 && c <= 0xFF: // Р°..СЏ
            ci := int(c); r := rune(0x0430 + (ci - 0xE0))
            out.WriteRune(r)
        case c == 0xA8: // РЃ
            out.WriteRune('\u0401')
        case c == 0xB8: // С‘
            out.WriteRune('\u0451')
        case c == 0xA0: // NBSP -> space
            out.WriteByte(' ')
        default:
            // Best-effort: replace with '?'
            out.WriteByte('?')
        }
    }
    return out.Bytes()
}

// koi8rToUTF8 decodes KOI8-R to UTF-8 covering common Cyrillic letters.
func koi8rToUTF8(b []byte) []byte {
    // Accurate KOI8-R mapping table for bytes 0x80..0xFF.
    // See https://en.wikipedia.org/wiki/KOI8-R
    var table = [128]rune{
        0x2500, 0x2502, 0x250C, 0x2510, 0x2514, 0x2518, 0x251C, 0x2524, // 0x80..0x87
        0x252C, 0x2534, 0x253C, 0x2580, 0x2584, 0x2588, 0x258C, 0x2590, // 0x88..0x8F
        0x2591, 0x2592, 0x2593, 0x2320, 0x25A0, 0x2219, 0x221A, 0x2248, // 0x90..0x97
        0x2264, 0x2265, 0x00A0, 0x2321, 0x00B0, 0x00B2, 0x00B7, 0x00F7, // 0x98..0x9F
        0x2550, 0x2551, 0x2552, 0x0451, 0x2553, 0x2554, 0x2555, 0x2556, // 0xA0..0xA7
        0x2557, 0x2558, 0x2559, 0x255A, 0x255B, 0x255C, 0x255D, 0x255E, // 0xA8..0xAF
        0x255F, 0x2560, 0x2561, 0x0401, 0x2562, 0x2563, 0x2564, 0x2565, // 0xB0..0xB7
        0x2566, 0x2567, 0x2568, 0x2569, 0x256A, 0x256B, 0x256C, 0x00A9, // 0xB8..0xBF
        0x044E, 0x0430, 0x0431, 0x0446, 0x0434, 0x0435, 0x0444, 0x0433, // 0xC0..0xC7
        0x0445, 0x0438, 0x0439, 0x043A, 0x043B, 0x043C, 0x043D, 0x043E, // 0xC8..0xCF
        0x043F, 0x044F, 0x0440, 0x0441, 0x0442, 0x0443, 0x0436, 0x0432, // 0xD0..0xD7
        0x044C, 0x044B, 0x0437, 0x0448, 0x044D, 0x0449, 0x0447, 0x044A, // 0xD8..0xDF
        0x042E, 0x0410, 0x0411, 0x0426, 0x0414, 0x0415, 0x0424, 0x0413, // 0xE0..0xE7
        0x0425, 0x0418, 0x0419, 0x041A, 0x041B, 0x041C, 0x041D, 0x041E, // 0xE8..0xEF
        0x041F, 0x042F, 0x0420, 0x0421, 0x0422, 0x0423, 0x0416, 0x0412, // 0xF0..0xF7
        0x042C, 0x042B, 0x0417, 0x0428, 0x042D, 0x0429, 0x0427, 0x042A, // 0xF8..0xFF
    }
    var out bytes.Buffer
    for _, c := range b {
        if c < 0x80 {
            out.WriteByte(c)
        } else {
            out.WriteRune(table[int(c-0x80)])
        }
    }
    return out.Bytes()
}

// LoadPageWithHeaders performs HTTP GET with optional headers and converts the HTML into OMS.
// Unlike the legacy C code, non-200 statuses are still parsed when a body is present.
func LoadPageWithHeaders(oURL string, hdr http.Header) (*Page, error) {
	req, err := http.NewRequest(http.MethodGet, oURL, nil)

	// РџСЂРѕСЃС‚Р°РІРёРј РґРµС„РѕР»С‚РЅС‹Рµ Р·Р°РіРѕР»РѕРІРєРё, РµСЃР»Рё РЅРµ РїРµСЂРµРґР°Р»Рё
	if hdr == nil {
		hdr = http.Header{}
	}
	if hdr.Get("User-Agent") == "" {
		hdr.Set("User-Agent",
			"Mozilla/5.0 (Linux; Android 9; OMS Test) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120 Safari/537.36")
	}
	if hdr.Get("Accept") == "" {
		hdr.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
	}
	if hdr.Get("Accept-Language") == "" {
		hdr.Set("Accept-Language", "ru,en;q=0.8")
	}
	// РР·Р±РµРіР°РµРј brotli: Р»РёР±Рѕ СЏРІРЅРѕ gzip, Р»РёР±Рѕ identity.
	// Go СЃР°Рј СЂР°Р·РѕР¶РјРµС‚ gzip, РµСЃР»Рё СЃРµСЂРІРµСЂ РµРіРѕ РїСЂРёС€Р»С‘С‚.
	if hdr.Get("Accept-Encoding") == "" {
		hdr.Set("Accept-Encoding", "gzip")
	}
	// РџСЂРёРјРµРЅСЏРµРј РІСЃРµ Р·Р°РіРѕР»РѕРІРєРё Рє Р·Р°РїСЂРѕСЃСѓ
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return errorPage(oURL, "Timeout loading page"), nil
	}
	defer resp.Body.Close()
	// Decode body if Content-Encoding present; net/http auto-decodes only when
	// Accept-Encoding wasn't set explicitly by the caller.
	var reader io.ReadCloser = resp.Body
	switch strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))) {
	case "gzip":
		if gr, gerr := gzip.NewReader(resp.Body); gerr == nil {
			reader = gr
			defer gr.Close()
		}
	case "deflate":
		if zr, zerr := zlib.NewReader(resp.Body); zerr == nil {
			reader = zr
			defer zr.Close()
		} else if fr := flate.NewReader(resp.Body); fr != nil {
			reader = io.NopCloser(fr)
			defer fr.Close()
		}
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		return errorPage(oURL, "Internal server error"), nil
	}
	// Pass-through if the response already looks like an OMS payload
	if looksLikeOMS(body) {
		return &Page{Data: body}, nil
	}
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return errorPage(oURL, "Internal server error while parsing"), nil
	}
	p := NewPage()
	// Prefix page URL with "1/" as in legacy streams for better client compatibility
	p.AddString("1/" + oURL)
	p.AddAuthcode("c37c206d2c235978d086b64c39a2fc17df68dbdd5dc04dd8b199177f95be6181")
	p.AddAuthprefix("t19-12")
	p.AddStyle(styleDefault)
	base := oURL
	if i := strings.Index(base, "?"); i != -1 {
		base = base[:i]
	}
	// Respect <base href> if present
	base = findBaseURL(doc, base)
	walk(doc, base, p)
	p.finalize()
	return p, nil
}

// looksLikeOMS heuristically checks if b is a complete OMS response
// framed with 6-byte common header followed by raw DEFLATE body that
// inflates to a tag stream ending with 'Q'.
func looksLikeOMS(b []byte) bool {
	if len(b) < 8 {
		return false
	}
	if binary.LittleEndian.Uint16(b[:2]) != Version {
		return false
	}
	// Optional size sanity: match length if reasonable
	// Some producers may not set it exactly; ignore mismatch.
	// Try to inflate body and verify last tag is 'Q'.
	fr := flate.NewReader(bytes.NewReader(b[6:]))
	dec, err := io.ReadAll(fr)
	fr.Close()
	if err != nil || len(dec) == 0 {
		return false
	}
	if dec[len(dec)-1] != 'Q' {
		return false
	}
	return true
}

func getAttr(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, name) {
			return a.Val
		}
	}
	return ""
}

func findTextNode(n *html.Node, visited map[*html.Node]bool) *html.Node {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode && !visited[c] {
			return c
		}
		if t := findTextNode(c, visited); t != nil {
			return t
		}
	}
	return nil
}

func resolveLink(base, href string) string {
	if href == "" {
		return "error:link"
	}
	bu, err := url.Parse(base)
	if err != nil {
		return "error:link"
	}
	hu, err := url.Parse(href)
	if err != nil {
		return "error:link"
	}
	if !hu.IsAbs() {
		href = bu.ResolveReference(hu).String()
	} else {
		href = hu.String()
	}
	return "0/" + href
}

func walk(n *html.Node, base string, p *Page) {
	visited := map[*html.Node]bool{}
	// Apply a default style once to stabilize rendering
	p.AddStyle(styleDefault)
	st := walkState{curStyle: styleDefault}
	prefs := defaultRenderPrefs()
	walkRich(n, base, p, visited, &st, prefs)
}

// pickSrcFromSrcset returns the first URL from a srcset string.
func pickSrcFromSrcset(srcset string) string {
	s := strings.TrimSpace(srcset)
	if s == "" {
		return ""
	}
	// srcset: comma-separated candidates; each candidate: URL [descriptor]
	parts := strings.Split(s, ",")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// URL is first token until space
		sp := strings.SplitN(p, " ", 2)
		url := strings.TrimSpace(sp[0])
		if url != "" {
			return url
		}
	}
	return ""
}

// ---------- Enhanced HTML4/XHTML в†’ OBML transformer ----------

type listCtx struct {
	kind    string
	counter int
	bullet  string
}
type walkState struct {
    pre        bool
    lists      []listCtx
    styleStack []uint32
    curStyle   uint32
    inLink     bool
    css        *Stylesheet
    colorStack []string
    curColor   string
}

// RenderOptions define client rendering preferences relevant to OBML generation.
type RenderOptions struct {
    ImagesOn      bool
    HighQuality   bool
    ImageMIME     string         // e.g. "image/jpeg", "image/png"
    MaxInlineKB   int            // max kilobytes for inline image ('I') before falling back to placeholder
    ReqHeaders    http.Header    // copy of page request headers (UA, Lang, Cookies)
    Referrer      string         // page URL for Referer
    OriginCookies string         // cookies set by origin page (name=value; ...)
    Jar           http.CookieJar // optional cookie jar for origin requests
    // Opera Mini auth echo: include these as 'k' tags ('authcode' and 'authprefix')
    AuthCode      string
    AuthPrefix    string
        ScreenW      int
    ScreenH      int
    NumColors    int
    HeapKB       int
    AlphaLevels  int
    FormBody     string
    // Pagination: 1-based page index and max tags per page (0=disabled)
    Page          int
    MaxTagsPerPage int
    // Optional absolute base (scheme://host) for building navigation links
    ServerBase    string
    Styles        *Stylesheet
    WantFullCache bool
}

func defaultRenderPrefs() RenderOptions {
    // Default pagination disabled; can be overridden via env in loader
    return RenderOptions{ImagesOn: false, HighQuality: false, ImageMIME: "image/jpeg", MaxInlineKB: 96}
}
func (s *walkState) pushList(kind string) { s.lists = append(s.lists, listCtx{kind: kind}) }
func (s *walkState) popList() {
	if len(s.lists) > 0 {
		s.lists = s.lists[:len(s.lists)-1]
	}
}
func (s *walkState) currentList() *listCtx {
	if len(s.lists) == 0 {
		return nil
	}
	return &s.lists[len(s.lists)-1]
}
func (s *walkState) pushStyle(p *Page, style uint32) {
	s.styleStack = append(s.styleStack, s.curStyle)
	s.curStyle = style
	p.AddStyle(style)
}
func (s *walkState) popStyle(p *Page) {
	if l := len(s.styleStack); l > 0 {
		prev := s.styleStack[l-1]
		s.styleStack = s.styleStack[:l-1]
		s.curStyle = prev
		p.AddStyle(prev)
	}
}


func (s *walkState) pushColor(p *Page, hex string) {
    s.colorStack = append(s.colorStack, s.curColor)
    s.curColor = hex
    if hex != "" { p.AddTextcolor(hex) }
}
func (s *walkState) popColor(p *Page) {
    if l := len(s.colorStack); l > 0 {
        prev := s.colorStack[l-1]
        s.colorStack = s.colorStack[:l-1]
        s.curColor = prev
        if prev != "" { p.AddTextcolor(prev) }
    }
}

func isDisplayNone(style string) bool {
	s := strings.ToLower(style)
	return strings.Contains(s, "display:none")
}

func parseCssColor(style, prop string) string {
	if style == "" {
		return ""
	}
	s := strings.ToLower(style)
	prop = strings.ToLower(prop)
	parts := strings.Split(s, ";")
	for _, part := range parts {
		kv := strings.SplitN(strings.TrimSpace(part), ":", 2)
		if len(kv) != 2 {
			continue
		}
		k := strings.TrimSpace(kv[0])
		v := strings.TrimSpace(kv[1])
		if k == prop {
			if strings.HasPrefix(v, "#") {
				if len(v) == 4 {
					r := string([]byte{v[1], v[1]})
					g := string([]byte{v[2], v[2]})
					b := string([]byte{v[3], v[3]})
					return "#" + r + g + b
				}
				if len(v) >= 7 {
					return v[:7]
				}
			}
		}
	}
	return ""
}

func parseCssTextAlign(style string) string {
	if style == "" {
		return ""
	}
	s := strings.ToLower(style)
	parts := strings.Split(s, ";")
	for _, part := range parts {
		kv := strings.SplitN(strings.TrimSpace(part), ":", 2)
		if len(kv) != 2 {
			continue
		}
		k := strings.TrimSpace(kv[0])
		v := strings.TrimSpace(kv[1])
		if k == "text-align" {
			switch v {
			case "center", "right", "left":
				return v
			}
		}
	}
	return ""
}

func condenseSpaces(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", " ")
	if !strings.Contains(s, "  ") {
		return strings.TrimSpace(s)
	}
	var b strings.Builder
	prev := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\t' || c == ' ' {
			if !prev {
				b.WriteByte(' ')
				prev = true
			}
			continue
		}
		prev = false
		b.WriteByte(c)
	}
	return strings.TrimSpace(b.String())
}

func walkRich(cur *html.Node, base string, p *Page, visited map[*html.Node]bool, st *walkState, prefs RenderOptions) {
	for c := cur; c != nil; c = c.NextSibling {
		recurse := true
        if c.Type == html.ElementNode {
            // Skip hidden elements
            if stAttr := getAttr(c, "style"); stAttr != "" && isDisplayNone(stAttr) { continue }
            // Apply computed CSS conservatively: support display:none and background-color
            if st.css != nil {
                if props := computeStyleFor(c, st.css); props != nil {
                    if strings.Contains(props["display"], "none") { continue }
                    
                }
            }
            switch strings.ToLower(c.Data) {
			case "html", "head":
			case "title":
				if t := findTextNode(c, visited); t != nil {
					visited[t] = true
					p.AddPlus()
					p.AddText(strings.TrimSpace(t.Data))
					p.AddBreak()
				}
			case "body":
				if l := getAttr(c, "bgcolor"); l != "" {
					p.AddBgcolor(l)
				}
				if l := getAttr(c, "text"); l != "" {
					p.AddTextcolor(l)
				}
				if stl := getAttr(c, "style"); stl != "" {
					if col := parseCssColor(stl, "background-color"); col != "" {
						p.AddBgcolor(col)
					}
					if col := parseCssColor(stl, "color"); col != "" {
						p.AddTextcolor(col)
					}
				}
			case "br":
				p.AddBreak()
			case "hr":
				p.AddHr(getAttr(c, "color"))
			case "p":
				p.AddParagraph()
			case "h1", "h2", "h3", "h4", "h5", "h6":
				// Prefer full collected text; fallback to first text node
				txt := strings.TrimSpace(collectText(c))
				if txt == "" {
					if t := findTextNode(c, visited); t != nil {
						visited[t] = true
						txt = strings.TrimSpace(t.Data)
					}
				}
				if txt != "" {
					p.AddPlus()
					// Emphasize headings
					st.pushStyle(p, st.curStyle|styleBoldBit)
					p.AddText(txt)
					st.popStyle(p)
					p.AddBreak()
				}
				// Do not recurse into heading children to avoid duplicate text
				recurse = false
            case "div", "section", "article", "header", "footer", "main", "nav", "aside":
                p.AddParagraph()
			case "b", "strong":
				st.pushStyle(p, st.curStyle|styleBoldBit)
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				st.popStyle(p)
				recurse = false
			case "i", "em":
				st.pushStyle(p, st.curStyle|styleItalicBit)
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				st.popStyle(p)
				recurse = false
			case "u":
				st.pushStyle(p, st.curStyle|styleUnderBit)
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				st.popStyle(p)
				recurse = false
			case "small":
				st.pushStyle(p, st.curStyle|styleItalicBit)
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				st.popStyle(p)
				recurse = false
			case "center":
				st.pushStyle(p, st.curStyle|styleCenterBit)
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				st.popStyle(p)
				recurse = false
			case "big":
				st.pushStyle(p, st.curStyle|styleBoldBit)
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				st.popStyle(p)
				recurse = false
			case "sup":
				p.AddText("^")
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				p.AddText("^")
				recurse = false
			case "sub":
				p.AddText("_")
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				p.AddText("_")
				recurse = false
			case "span":
				if sp := getAttr(c, "style"); sp != "" {
					aligned := false
					if ta := parseCssTextAlign(sp); ta == "center" {
						st.pushStyle(p, st.curStyle|styleCenterBit)
						aligned = true
					}
					if ta := parseCssTextAlign(sp); ta == "right" {
						st.pushStyle(p, st.curStyle|styleRightBit)
						aligned = true
					}
					pushed := 0
					if parseCssHas(sp, "font-weight", "bold") { st.pushStyle(p, st.curStyle|styleBoldBit); pushed++ }
					if parseCssHas(sp, "font-style", "italic") { st.pushStyle(p, st.curStyle|styleItalicBit); pushed++ }
					if parseCssHas(sp, "text-decoration", "underline") { st.pushStyle(p, st.curStyle|styleUnderBit); pushed++ }
					colorPushed := false
					if col := parseCssColor(sp, "color"); col != "" { st.pushColor(p, col); colorPushed = true }
					if c.FirstChild != nil { walkRich(c.FirstChild, base, p, visited, st, prefs) }
					for pushed > 0 { pushed--; st.popStyle(p) }
					if colorPushed { st.popColor(p) }
					if aligned { st.popStyle(p) }
					recurse = false
				}
			case "font":
				if col := strings.TrimSpace(getAttr(c, "color")); col != "" {
					st.pushStyle(p, st.curStyle)
					p.AddTextcolor(col)
					if c.FirstChild != nil {
						walkRich(c.FirstChild, base, p, visited, st, prefs)
					}
					st.popStyle(p)
					recurse = false
				}
			case "a":
				href := getAttr(c, "href")
				link := resolveLink(base, href)
				name := "Link"
				if t := findTextNode(c, visited); t != nil {
					visited[t] = true
					if txt := strings.TrimSpace(t.Data); txt != "" {
						name = txt
					}
				}
				if name == "Link" {
					if alt := findFirstImgAlt(c); alt != "" {
						name = alt
					}
				}
				// Render link with children as content
				p.addTag('L')
				p.AddString(link)
				before := p.tagCount
				prevIn := st.inLink
				st.inLink = true
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				st.inLink = prevIn
				if p.tagCount == before {
					p.AddText(name)
				}
				p.addTag('E')
				p.AddBreak()
				recurse = false
        case "img":
				// Images handling based on client prefs
				src := strings.TrimSpace(getAttr(c, "src"))
				if src == "" {
					if ss := strings.TrimSpace(getAttr(c, "srcset")); ss != "" {
						src = pickSrcFromSrcset(ss)
					}
				}
				if src == "" {
					// Common lazy-loading attributes
					src = strings.TrimSpace(getAttr(c, "data-src"))
					if src == "" {
						src = strings.TrimSpace(getAttr(c, "data-original"))
					}
					if src == "" {
						src = strings.TrimSpace(getAttr(c, "data-lazy-src"))
					}
				}
				alt := strings.TrimSpace(getAttr(c, "alt"))
				if alt == "" {
					alt = "Image"
				}
				if !prefs.ImagesOn || src == "" {
					p.AddText("[" + alt + "]")
					recurse = false
					break
				}
				abs := resolveLink(base, src)
				// Try to inline small images; otherwise use a link+placeholder
				if ib, w, h, ok := fetchAndEncodeImage(abs[2:], prefs); ok { // abs has leading "0/"
					if len(ib) <= prefs.MaxInlineKB*1024 {
						p.AddImageInline(w, h, ib)
					} else {
						if st.inLink {
							p.AddImagePlaceholder(w, h)
						} else {
							p.addTag('L')
							p.AddString(abs)
							p.AddImagePlaceholder(w, h)
							p.addTag('E')
						}
					}
				} else {
					// Fallback: clickable image link with placeholder (unknown size)
					if st.inLink {
						p.AddImagePlaceholder(0, 0)
					} else {
						p.addTag('L')
						p.AddString(abs)
						p.AddImagePlaceholder(0, 0)
						p.addTag('E')
					}
				}
				recurse = false
			case "caption":
				if t := findTextNode(c, visited); t != nil {
					visited[t] = true
					p.AddPlus()
					p.AddText(strings.TrimSpace(t.Data))
					p.AddBreak()
				}
				recurse = false
			case "ul":
				bul := "• "
				if st.css != nil {
					if props := computeStyleFor(c, st.css); props != nil {
						if v := props["list-style-type"]; v != "" {
							switch v {
							case "circle": bul = "○ "
							case "square": bul = "■ "
							case "disc": bul = "• "
							case "none": bul = ""
							}
						}
					}
				}
				st.pushList("ul")
				if top := st.currentList(); top != nil { top.bullet = bul }
			case "ol":
				st.pushList("ol")
			case "li":
				if top := st.currentList(); top != nil {
					if top.kind == "ol" {
						top.counter++
						p.AddText(strconv.Itoa(top.counter) + ". ")
					} else {
						p.AddText("- ")
					}
				} else {
					p.AddText("- ")
				}
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				p.AddBreak()
				recurse = false
			case "dl":
				st.pushList("dl")
			case "dt":
				p.AddPlus()
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				p.AddBreak()
				recurse = false
			case "dd":
                p.AddText(": ")
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				p.AddBreak()
				recurse = false
			case "pre", "code":
				child := *st
				child.pre = true
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, &child, prefs)
				}
				recurse = false
			case "blockquote":
				p.AddParagraph()
				p.AddText("> ")
			case "label":
				if t := findTextNode(c, visited); t != nil {
					visited[t] = true
					if txt := condenseSpaces(t.Data); txt != "" {
						p.AddText(txt + ": ")
					}
				}
				recurse = false
			case "fieldset":
				p.AddParagraph()
			case "legend":
				if t := findTextNode(c, visited); t != nil {
					visited[t] = true
					p.AddPlus()
					p.AddText(strings.TrimSpace(t.Data))
					p.AddBreak()
				}
				recurse = false
			case "q":
				p.AddText("\"")
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				p.AddText("\"")
				recurse = false
			case "tt", "kbd", "samp", "var", "cite", "address":
				st.pushStyle(p, st.curStyle|styleItalicBit)
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				st.popStyle(p)
				recurse = false
			case "iframe":
				if src := strings.TrimSpace(getAttr(c, "src")); src != "" {
					p.AddLink(resolveLink(base, src), "[Frame]")
				}
				recurse = false
			case "object", "embed":
				data := strings.TrimSpace(getAttr(c, "data"))
				if data == "" {
					data = strings.TrimSpace(getAttr(c, "src"))
				}
				if data != "" {
					p.AddLink(resolveLink(base, data), "[Object]")
				}
				recurse = false
			case "s", "strike", "del":
				p.AddText("~")
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				p.AddText("~")
				recurse = false
			case "ins":
				p.AddText("+")
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				p.AddText("+")
				recurse = false
			case "table":
				if hasFormControls(c) {
					// let recursion process inputs/selects
				} else {
					// Traverse sections and rows: thead/tbody/tfoot/tr
					for sec := c.FirstChild; sec != nil; sec = sec.NextSibling {
						if sec.Type != html.ElementNode { continue }
						if strings.EqualFold(sec.Data, "tr") {
							row := make([]string, 0, 8)
							for cell := sec.FirstChild; cell != nil; cell = cell.NextSibling {
								if cell.Type == html.ElementNode && (strings.EqualFold(cell.Data, "td") || strings.EqualFold(cell.Data, "th")) {
									txt := strings.TrimSpace(collectText(cell))
									if txt != "" { row = append(row, txt) }
								}
							}
							if len(row) > 0 { p.AddText(strings.Join(row, " | ")); p.AddBreak() }
							continue
						}
						if strings.EqualFold(sec.Data, "thead") || strings.EqualFold(sec.Data, "tbody") || strings.EqualFold(sec.Data, "tfoot") {
							for r := sec.FirstChild; r != nil; r = r.NextSibling {
								if r.Type == html.ElementNode && strings.EqualFold(r.Data, "tr") {
									row := make([]string, 0, 8)
									for cell := r.FirstChild; cell != nil; cell = cell.NextSibling {
										if cell.Type == html.ElementNode && (strings.EqualFold(cell.Data, "td") || strings.EqualFold(cell.Data, "th")) {
											txt := strings.TrimSpace(collectText(cell))
											if txt != "" { row = append(row, txt) }
										}
									}
									if len(row) > 0 { p.AddText(strings.Join(row, " | ")); p.AddBreak() }
								}
							}
						}
					}
					recurse = false
				}
			            case "details":
                // Render expanded content inline: summary processed elsewhere
                if c.FirstChild != nil { walkRich(c.FirstChild, base, p, visited, st, prefs) }
                recurse = false
            case "audio", "video":
                if src := strings.TrimSpace(getAttr(c, "src")); src != "" {
                    p.AddLink(resolveLink(base, src), "[Media]")
                }
                for s := c.FirstChild; s != nil; s = s.NextSibling {
                    if s.Type == html.ElementNode && strings.EqualFold(s.Data, "source") {
                        if ss := strings.TrimSpace(getAttr(s, "src")); ss != "" {
                            p.AddLink(resolveLink(base, ss), "[Media]")
                        }
                    }
                }
                recurse = false
            case "picture":
                chosen := ""
                for s := c.FirstChild; s != nil && chosen == ""; s = s.NextSibling {
                    if s.Type == html.ElementNode && strings.EqualFold(s.Data, "source") {
                        if ss := strings.TrimSpace(getAttr(s, "srcset")); ss != "" { chosen = pickSrcFromSrcset(ss) }
                        if chosen == "" { chosen = strings.TrimSpace(getAttr(s, "src")) }
                    }
                }
                if chosen == "" {
                    if img := findFirstChild(c, "img"); img != nil {
                        chosen = strings.TrimSpace(getAttr(img, "src"))
                    }
                }
                if chosen != "" { p.AddLink(resolveLink(base, chosen), "[Image]") }
                recurse = false
            case "form":
				action := getAttr(c, "action")
				p.AddForm(action)
			case "button":
				typ := strings.ToLower(getAttr(c, "type"))
				if typ == "" {
					typ = "submit"
				}
				name := getAttr(c, "name")
				if name == "" {
					name = "dname"
				}
				value := getAttr(c, "value")
				label := strings.TrimSpace(collectText(c))
				if value == "" {
					value = label
				}
				markTextNodes(c, visited)
				switch typ {
				case "submit":
					p.AddSubmit(name, value)
				case "reset":
					p.AddReset(name, value)
				default:
					p.AddButton(name, value)
				}
				recurse = false
			case "textarea":
				stl := getAttr(c, "style")
				if stl == "" || !strings.Contains(stl, "display:none") {
					name := getAttr(c, "name")
					if name == "" {
						name = "dname"
					}
					value := getAttr(c, "value")
					p.AddTextInput(name, value)
				}
			case "input":
				typ := strings.ToLower(getAttr(c, "type"))
				if typ == "" {
					typ = "text"
				}
				name := getAttr(c, "name")
				if name == "" {
					name = "dname"
				}
				value := getAttr(c, "value")
				switch typ {
				case "text":
					p.AddTextInput(name, value)
				case "password":
					p.AddPassInput(name, value)
				case "submit":
					p.AddSubmit(name, value)
				case "checkbox":
					checked := strings.EqualFold(getAttr(c, "checked"), "true")
					if value == "" {
						value = "on"
					}
					p.AddCheckbox(name, value, checked)
				case "radio":
					checked := strings.EqualFold(getAttr(c, "checked"), "true")
					if value == "" {
						value = "on"
					}
					p.AddRadio(name, value, checked)
				case "hidden":
					p.AddHidden(name, value)
				case "button":
					p.AddButton(name, value)
				case "reset":
					p.AddReset(name, value)
				}
			case "select":
				name := getAttr(c, "name")
				if name == "" {
					name = "dname"
				}
				multiple := strings.EqualFold(getAttr(c, "multiple"), "true") || getAttr(c, "multiple") != ""
				type option struct {
					label, value string
					selected     bool
					textNode     *html.Node
				}
				opts := make([]option, 0, 8)
				for oc := c.FirstChild; oc != nil; oc = oc.NextSibling {
					if oc.Type == html.ElementNode && strings.EqualFold(oc.Data, "option") {
						txt := findTextNode(oc, visited)
						label := ""
						if txt != nil {
							label = strings.TrimSpace(txt.Data)
						}
						val := getAttr(oc, "value")
						if val == "" {
							val = label
						}
						sel := strings.EqualFold(getAttr(oc, "selected"), "true") || getAttr(oc, "selected") != ""
						opts = append(opts, option{label: label, value: val, selected: sel, textNode: txt})
					}
				}
				p.BeginSelect(name, multiple, len(opts))
				for _, opt := range opts {
					if opt.textNode != nil {
						visited[opt.textNode] = true
					}
					p.AddOption(opt.value, opt.label, opt.selected)
				}
				p.EndSelect()
			}
		} else if c.Type == html.TextNode {
			if !visited[c] {
				// Skip any stray text nodes under head/style/script/meta/link/noscript
				skip := false
				if par := c.Parent; par != nil && par.Type == html.ElementNode {
					t := strings.ToLower(par.Data)
					if t == "style" || t == "script" || t == "noscript" || t == "link" || t == "meta" || t == "head" {
						skip = true
					}
				}
				if !skip {
					var txt string
					if st.pre {
						txt = c.Data
					} else {
						txt = condenseSpaces(c.Data)
					}
					if txt != "" {
						p.AddText(txt)
					}
				}
			}
		}
		if recurse && c.FirstChild != nil {
			walkRich(c.FirstChild, base, p, visited, st, prefs)
		}
		if c.Type == html.ElementNode && (strings.EqualFold(c.Data, "ul") || strings.EqualFold(c.Data, "ol") || strings.EqualFold(c.Data, "dl")) {
			st.popList()
			p.AddParagraph()
		}
	}
}

func collectText(n *html.Node) string {
	var b strings.Builder
	var rec func(*html.Node)
	rec = func(x *html.Node) {
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.TextNode {
				b.WriteString(strings.TrimSpace(c.Data))
				b.WriteString(" ")
			}
			if c.Type == html.ElementNode {
				t := strings.ToLower(c.Data)
				if t == "style" || t == "script" || t == "noscript" || t == "link" || t == "meta" {
					continue
				}
				if t == "a" {
					b.WriteString(" ")
				}
				if c.FirstChild != nil {
					rec(c)
				}
			}
		}
	}
	rec(n)
	return strings.TrimSpace(b.String())
}

func markTextNodes(n *html.Node, visited map[*html.Node]bool) {
	var rec func(*html.Node)
	rec = func(x *html.Node) {
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.TextNode {
				visited[c] = true
			}
			if c.FirstChild != nil {
				rec(c)
			}
		}
	}
	rec(n)
}

// hasFormControls returns true if the node subtree contains any form control
func hasFormControls(n *html.Node) bool {
	found := false
	var rec func(*html.Node)
	rec = func(x *html.Node) {
		if x == nil || found {
			return
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode {
				t := strings.ToLower(c.Data)
				if t == "input" || t == "select" || t == "textarea" || t == "button" || t == "label" || t == "form" {
					found = true
					return
				}
				if c.FirstChild != nil {
					rec(c)
				}
			}
		}
	}
	rec(n)
	return found
}

// findFirstImgAlt returns alt text for first <img> under a node
func findFirstImgAlt(n *html.Node) string {
	var rec func(*html.Node) string
	rec = func(x *html.Node) string {
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && strings.EqualFold(c.Data, "img") {
				alt := strings.TrimSpace(getAttr(c, "alt"))
				if alt == "" {
					alt = "Image"
				}
				return alt
			}
			if c.FirstChild != nil {
				if a := rec(c); a != "" {
					return a
				}
			}
		}
		return ""
	}
	return rec(n)
}

// findBaseURL scans <head> for <base href> and returns absolute base
func findBaseURL(doc *html.Node, cur string) string {
	if doc == nil {
		return cur
	}
	var head *html.Node
	var find func(*html.Node)
	find = func(n *html.Node) {
		for c := n; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && strings.EqualFold(c.Data, "head") {
				head = c
				return
			}
			if c.FirstChild != nil {
				find(c.FirstChild)
				if head != nil {
					return
				}
			}
		}
	}
	find(doc)
	if head == nil {
		return cur
	}
	for c := head.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && strings.EqualFold(c.Data, "base") {
			href := getAttr(c, "href")
			if href == "" {
				continue
			}
			bu, err := url.Parse(cur)
			if err != nil {
				continue
			}
			hu, err := url.Parse(href)
			if err != nil {
				continue
			}
			if !hu.IsAbs() {
				href = bu.ResolveReference(hu).String()
			} else {
				href = hu.String()
			}
			return href
		}
	}
	return cur
}

// Build a compact, test-like OMS page from a URL and optional title
func buildCompactPage(oURL, title string) *Page {
	if title == "" {
		title = "Open page"
	}
	p := NewPage()
	p.AddString("1/" + oURL)
	p.AddAuthcode("c37c206d2c235978d086b64c39a2fc17df68dbdd5dc04dd8b199177f95be6181")
	p.AddAuthprefix("t19-12")
	p.AddStyle(styleDefault)
	p.AddPlus()
	p.AddText(title)
	p.AddBreak()
	p.AddLink("0/"+oURL, title)
	p.finalize()
	return p
}

// extractTitle returns the first <title> text or empty string
func extractTitle(n *html.Node) string {
	var dfs func(*html.Node) string
	dfs = func(x *html.Node) string {
		if x.Type == html.ElementNode && strings.EqualFold(x.Data, "title") {
			if t := findTextNode(x, map[*html.Node]bool{}); t != nil {
				return strings.TrimSpace(t.Data)
			}
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			if s := dfs(c); s != "" {
				return s
			}
		}
		return ""
	}
	return dfs(n)
}
// findFirstChild returns the first direct child element with the given tag name.
func findFirstChild(n *html.Node, name string) *html.Node {
    low := strings.ToLower(name)
    for c := n.FirstChild; c != nil; c = c.NextSibling {
        if c.Type == html.ElementNode && strings.EqualFold(c.Data, low) {
            return c
        }
    }
    return nil
}

// findFirstByTag performs DFS to find the first element with the given tag name.
func findFirstByTag(n *html.Node, name string) *html.Node {
    low := strings.ToLower(name)
    var dfs func(*html.Node) *html.Node
    dfs = func(x *html.Node) *html.Node {
        if x.Type == html.ElementNode && strings.EqualFold(x.Data, low) {
            return x
        }
        for c := x.FirstChild; c != nil; c = c.NextSibling {
            if r := dfs(c); r != nil { return r }
        }
        return nil
    }
    return dfs(n)
}

// isDarkHex returns true if the given #rgb or #rrggbb color is visually dark.
func isDarkHex(hex string) bool {
    if len(hex) == 0 || hex[0] != '#' { return false }
    var r8, g8, b8 int
    if len(hex) == 4 { // #rgb
        r := ch(hex[1])
        g := ch(hex[2])
        b := ch(hex[3])
        r8 = int(r*16 + r)
        g8 = int(g*16 + g)
        b8 = int(b*16 + b)
    } else if len(hex) == 7 { // #rrggbb
        r8 = int((ch(hex[1])<<4 | ch(hex[2])))
        g8 = int((ch(hex[3])<<4 | ch(hex[4])))
        b8 = int((ch(hex[5])<<4 | ch(hex[6])))
    } else {
        return false
    }
    // relative luminance (simple sRGB approximation)
    y := 0.2126*float64(r8)/255.0 + 0.7152*float64(g8)/255.0 + 0.0722*float64(b8)/255.0
    return y < 0.4
}

// parseCssHas returns true if inline style contains prop with a value including val substring.
func parseCssHas(style, prop, val string) bool {
    if style == "" { return false }
    s := strings.ToLower(style)
    prop = strings.ToLower(prop)
    val = strings.ToLower(val)
    parts := strings.Split(s, ";")
    for _, part := range parts {
        kv := strings.SplitN(strings.TrimSpace(part), ":", 2)
        if len(kv) != 2 { continue }
        k := strings.TrimSpace(kv[0])
        v := strings.TrimSpace(kv[1])
        if k == prop && strings.Contains(v, val) { return true }
    }
    return false
}

// LoadCompactPageWithHeaders fetches the URL (with headers) and returns a small
// OBML page similar to /oms/test: title + link to the original URL.
func LoadCompactPageWithHeaders(oURL string, hdr http.Header) (*Page, error) {
	req, err := http.NewRequest(http.MethodGet, oURL, nil)
	if err != nil {
		return errorPage(oURL, "Internal server error"), nil
	}
	if hdr != nil {
		for k, vs := range hdr {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
	}
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return errorPage(oURL, "Timeout loading page"), nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	title := ""
	if len(body) > 0 {
		if doc, err := html.Parse(bytes.NewReader(body)); err == nil {
			title = extractTitle(doc)
		}
	}
	return buildCompactPage(oURL, title), nil
}

// BuildGoogleFront creates a small Google front page suitable for OM2
/* func BuildGoogleFront(oURL string, lang string) *Page {
	title := "Google"
	if strings.HasPrefix(strings.ToLower(lang), "ru") {
		title = "РџРѕРёСЃРє Google"
	}
	p := NewPage()
	p.AddString("1/" + oURL)
	p.AddAuthcode("c37c206d2c235978d086b64c39a2fc17df68dbdd5dc04dd8b199177f95be6181")
	p.AddAuthprefix("t19-12")
	p.AddStyle(styleDefault)
	p.AddPlus()
	p.AddText(title)
	p.AddBreak()
	// Simple search form
	p.AddForm("https://www.google.com/search")
	p.AddTextInput("q", "")
	p.AddSubmit("btnG", "Search")
	p.finalize()
	return p
} */

// ---------------------- Images: fetch and encode helpers ----------------------

// fetchAndEncodeImage downloads the image at absURL and encodes it into the
// client-requested format, returning bytes and dimensions.
// absURL should be absolute (no 0/ prefix).
func fetchAndEncodeImage(absURL string, prefs RenderOptions) ([]byte, int, int, bool) {
	debug := os.Getenv("OMS_IMG_DEBUG") == "1"
	// data: URIs
	if strings.HasPrefix(absURL, "data:") {
		if b, w, h, ok := decodeDataURI(absURL, prefs); ok {
			return b, w, h, true
		}
		if debug {
			log.Printf("IMG decode data: failed url=%s", absURL)
		}
		return nil, 0, 0, false
	}
	req, err := http.NewRequest(http.MethodGet, absURL, nil)
	if err != nil {
		if debug {
			log.Printf("IMG newrequest: %v", err)
		}
		return nil, 0, 0, false
	}
	req.Header.Set("Accept", "image/*")
	// Carry over useful headers from the page request
	if prefs.ReqHeaders != nil {
		if ua := prefs.ReqHeaders.Get("User-Agent"); ua != "" {
			req.Header.Set("User-Agent", ua)
		}
		if al := prefs.ReqHeaders.Get("Accept-Language"); al != "" {
			req.Header.Set("Accept-Language", al)
		}
		// Merge cookies from client and origin page
		var cookieParts []string
		if ck := prefs.ReqHeaders.Get("Cookie"); ck != "" {
			cookieParts = append(cookieParts, ck)
		}
		if oc := prefs.OriginCookies; oc != "" {
			cookieParts = append(cookieParts, oc)
		}
		if len(cookieParts) > 0 {
			req.Header.Set("Cookie", strings.Join(cookieParts, "; "))
		}
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "OMS-ImageFetcher/1.0")
	}
	if prefs.Referrer != "" {
		req.Header.Set("Referer", prefs.Referrer)
	}
	ihc := &http.Client{Timeout: 8 * time.Second}
	if prefs.Jar != nil {
		ihc.Jar = prefs.Jar
	}
    client := ihc
    resp, err := client.Do(req)
	if err != nil {
		if debug {
			log.Printf("IMG fetch: %v", err)
		}
		return nil, 0, 0, false
	}
	defer resp.Body.Close()
	var rc io.ReadCloser = resp.Body
	switch strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))) {
	case "gzip":
		if gr, e := gzip.NewReader(resp.Body); e == nil {
			rc = gr
			defer gr.Close()
		}
	case "deflate":
		if zr, e := zlib.NewReader(resp.Body); e == nil {
			rc = zr
			defer zr.Close()
		} else if fr := flate.NewReader(resp.Body); fr != nil {
			rc = io.NopCloser(fr)
			defer fr.Close()
		}
	}
	raw, err := io.ReadAll(rc)
	if err != nil || len(raw) == 0 {
		if debug {
			log.Printf("IMG read: err=%v len=%d", err, len(raw))
		}
		return nil, 0, 0, false
	}
	// Decode
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		if debug {
			log.Printf("IMG decode: %v (ct=%s)", err, resp.Header.Get("Content-Type"))
		}
		return nil, 0, 0, false
	}
	b := img.Bounds()
	w := b.Dx()
	h := b.Dy()
    // Encode to requested MIME; check disk cache first with final format/quality key
    var out bytes.Buffer
    want := strings.ToLower(prefs.ImageMIME)
    if want == "image/jpeg" && imageHasAlpha(img) {
        want = "image/png"
    }
    // Derive quality only for JPEG
    q := 0
    if want == "image/jpeg" {
        if prefs.HighQuality { q = 85 } else { q = 40 }
    }
    if data, cw, ch, ok := diskCacheGet(want, q, absURL); ok {
        return data, cw, ch, true
    }
    switch want {
    case "image/png":
        enc := png.Encoder{CompressionLevel: png.BestCompression}
        if prefs.HighQuality {
            enc.CompressionLevel = png.BestCompression
        } else {
            enc.CompressionLevel = png.DefaultCompression
        }
        if err := enc.Encode(&out, img); err != nil {
            if debug {
                log.Printf("IMG encode png: %v", err)
            }
            return nil, 0, 0, false
        }
    default:
        if q == 0 { q = 60 }
        if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: q}); err != nil {
            if debug {
                log.Printf("IMG encode jpeg: %v", err)
            }
            return nil, 0, 0, false
        }
    }
    enc := append([]byte(nil), out.Bytes()...)
    diskCachePut(want, q, absURL, enc, w, h)
    return enc, w, h, true
}

// imageHasAlpha returns true if any sampled pixel has alpha != 0xff.
func imageHasAlpha(img image.Image) bool {
	b := img.Bounds()
	dx, dy := b.Dx(), b.Dy()
	if dx <= 0 || dy <= 0 {
		return false
	}
	// Sample grid up to ~64x64 points to avoid heavy scans on big images
	stepX := dx / 64
	if stepX < 1 {
		stepX = 1
	}
	stepY := dy / 64
	if stepY < 1 {
		stepY = 1
	}
	for y := b.Min.Y; y < b.Max.Y; y += stepY {
		for x := b.Min.X; x < b.Max.X; x += stepX {
			_, _, _, a := img.At(x, y).RGBA()
			if a < 0xFFFF {
				return true
			}
		}
	}
	return false
}

func decodeDataURI(uri string, prefs RenderOptions) ([]byte, int, int, bool) {
	// data:[<mediatype>][;base64],<data>
	comma := strings.IndexByte(uri, ',')
	if !strings.HasPrefix(uri, "data:") || comma == -1 {
		return nil, 0, 0, false
	}
	meta := uri[len("data:"):comma]
	data := uri[comma+1:]
	var raw []byte
	if strings.Contains(meta, ";base64") {
		b, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return nil, 0, 0, false
		}
		raw = b
	} else {
		raw = []byte(data)
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, 0, 0, false
	}
	b := img.Bounds()
	w := b.Dx()
	h := b.Dy()
	var out bytes.Buffer
	switch strings.ToLower(prefs.ImageMIME) {
	case "image/png":
		if err := png.Encode(&out, img); err != nil {
			return nil, 0, 0, false
		}
	default:
		q := 60
		if prefs.HighQuality {
			q = 85
		} else {
			q = 40
		}
		if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: q}); err != nil {
			return nil, 0, 0, false
		}
	}
	return out.Bytes(), w, h, true
}

// ---------------------- Public API with options ----------------------

// LoadPageWithHeadersAndOptions performs HTTP GET with optional headers and rendering options.
func LoadPageWithHeadersAndOptions(oURL string, hdr http.Header, opts *RenderOptions) (*Page, error) {
	req, err := http.NewRequest(http.MethodGet, oURL, nil)
	if hdr == nil {
		hdr = http.Header{}
	}
	if hdr.Get("User-Agent") == "" {
		hdr.Set("User-Agent", "Mozilla/5.0 (Linux; Android 9; OMS Test) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120 Safari/537.36")
	}
	if hdr.Get("Accept") == "" {
		hdr.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
	}
	if hdr.Get("Accept-Language") == "" {
		hdr.Set("Accept-Language", "ru,en;q=0.8")
	}
	if hdr.Get("Accept-Encoding") == "" {
		hdr.Set("Accept-Encoding", "gzip")
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	hc := &http.Client{Timeout: 15 * time.Second}
	if opts != nil && opts.Jar != nil {
		hc.Jar = opts.Jar
	}
	client := hc
	resp, err := client.Do(req)
	if err != nil {
		return errorPage(oURL, "Timeout loading page"), nil
	}
	defer resp.Body.Close()
	var reader io.ReadCloser = resp.Body
	switch strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))) {
	case "gzip":
		if gr, gerr := gzip.NewReader(resp.Body); gerr == nil {
			reader = gr
			defer gr.Close()
		}
	case "deflate":
		if zr, zerr := zlib.NewReader(resp.Body); zerr == nil {
			reader = zr
			defer zr.Close()
		} else if fr := flate.NewReader(resp.Body); fr != nil {
			reader = io.NopCloser(fr)
			defer fr.Close()
		}
	}
    body, err := io.ReadAll(reader)
    if err != nil {
        return errorPage(oURL, "Internal server error"), nil
    }
    if looksLikeOMS(body) {
        return &Page{Data: body, SetCookies: resp.Header["Set-Cookie"]}, nil
    }
    // Decode to UTF-8 to support legacy charsets (e.g. windows-1251, koi8-r)
    utf8Body := decodeLegacyToUTF8(body, resp.Header.Get("Content-Type"))
    doc, err := html.Parse(bytes.NewReader(utf8Body))
    if err != nil {
        return errorPage(oURL, "Internal server error while parsing"), nil
    }
	p := NewPage()
	p.AddString("1/" + oURL)
	p.AddAuthcode("c37c206d2c235978d086b64c39a2fc17df68dbdd5dc04dd8b199177f95be6181")
	p.AddAuthprefix("t19-12")
	p.AddStyle(styleDefault)
	base := oURL
	if i := strings.Index(base, "?"); i != -1 {
		base = base[:i]
	}
	base = findBaseURL(doc, base)
    // Select prefs or fallback
    rp := defaultRenderPrefs()
    if opts != nil {
        rp = *opts
    }
    rp.ReqHeaders = hdr
    rp.Referrer = oURL
    // Build minimal stylesheet (inline + linked)
    rp.Styles = buildStylesheet(doc, base, hdr, opts.Jar)
    // Apply body defaults (background/text) with contrast if necessary
    chosenCol := ""
    if body := findFirstByTag(doc, "body"); body != nil {
        var bgHex, fgHex string
        if rp.Styles != nil {
            if props := computeStyleFor(body, rp.Styles); props != nil {
                if v := props["background-color"]; v != "" { bgHex = v }
                if v := props["color"]; v != "" { fgHex = v }
            }
        }
        if fgHex == "" { if v := getAttr(body, "text"); v != "" { fgHex = v } }
        if bgHex == "" { if v := getAttr(body, "bgcolor"); v != "" { bgHex = v } }
        if bgHex == "" { if v := getAttr(body, "bgcolor"); v != "" { bgHex = v } }
        if stl := getAttr(body, "style"); stl != "" {
            if v := parseCssColor(stl, "background-color"); v != "" { bgHex = v }
            if v := parseCssColor(stl, "color"); v != "" { fgHex = v }
        }
        if bgHex != "" { p.AddBgcolor(bgHex) }
        chosenCol = ""
        if fgHex != "" { p.AddTextcolor(fgHex); chosenCol = fgHex } else if bgHex != "" && isDarkHex(bgHex) { p.AddTextcolor("#eeeeee"); chosenCol = "#eeeeee" }
    }
    visited := map[*html.Node]bool{}
    st := walkState{curStyle: styleDefault}
    if chosenCol != "" { st.curColor = chosenCol }
    st.css = rp.Styles
    p.AddStyle(styleDefault)
    walkRich(doc, base, p, visited, &st, rp)
	// Build lightweight Cookie header for same-request image fetches
	if len(p.SetCookies) > 0 {
		var pairs []string
		for _, sc := range p.SetCookies {
			// take name=value before first ';'
			i := strings.IndexByte(sc, ';')
			kv := sc
			if i != -1 {
				kv = sc[:i]
			}
			kv = strings.TrimSpace(kv)
			if kv != "" {
				pairs = append(pairs, kv)
			}
		}
		if len(pairs) > 0 {
			rp.OriginCookies = strings.Join(pairs, "; ")
		}
	}
    // Optional pagination: split by tags, page index 1-based
    // Determine effective pagination from options or env
    pageIdx := 1
    maxTags := 0
    if opts != nil {
        if opts.Page > 0 { pageIdx = opts.Page }
        if opts.MaxTagsPerPage > 0 { maxTags = opts.MaxTagsPerPage }
    }
    if maxTags == 0 {
        if s := os.Getenv("OMS_PAGINATE_TAGS"); s != "" {
            if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && v > 0 {
                maxTags = v
            }
        }
        if maxTags == 0 {
            // Sensible default for old clients; adjust as needed
            maxTags = 1200
        }
    }
    if pageIdx < 1 { pageIdx = 1 }

    // Split raw payload (without V2 header) into parts by tag boundaries
    parts := splitByTags(p.Data, maxTags)
    if len(parts) == 0 {
        // Fallback: no tags recognized; finalize as-is
        p.finalize()
        return p, nil
    }
    if pageIdx > len(parts) { pageIdx = len(parts) }
    sel := parts[pageIdx-1]
    // If we have multiple parts, append Prev/Next navigation (only when ServerBase is known)
    serverBase := ""
    if opts != nil { serverBase = opts.ServerBase }
    if len(parts) > 1 && serverBase != "" {
        // Build small OBML fragment with navigation links
        nav := NewPage()
        nav.AddHr("")
        // Build base query
        qpp := strconv.Itoa(maxTags)
        // Prev
        if pageIdx > 1 {
            prevURL := serverBase + "/fetch?url=" + url.QueryEscape(oURL) + "&pp=" + qpp + "&page=" + strconv.Itoa(pageIdx-1)
            nav.AddLink("0/"+prevURL, "Назад")
        }
        // Next
        if pageIdx < len(parts) {
            nextURL := serverBase + "/fetch?url=" + url.QueryEscape(oURL) + "&pp=" + qpp + "&page=" + strconv.Itoa(pageIdx+1)
            nav.AddLink("0/"+nextURL, "Далее")
        }
        nav.AddHr("")
        sel = append(sel, nav.Data...)
    }
    // Replace payload with selected part and set part fields
    p.Data = sel
    p.partCur = pageIdx
    p.partCnt = len(parts)
    p.finalize()
    return p, nil
}








// SetPart allows external callers to set pagination metadata on the page.
func (p *Page) SetPart(cur, cnt int) {
    if cur < 0 { cur = 0 }
    if cnt < 0 { cnt = 0 }
    p.partCur = cur
    p.partCnt = cnt
}

// NormalizeOMSWithStag adjusts an OMS response bytes and sets stag_count to the provided value.
// stag is written as a swapped little-endian field.
func NormalizeOMSWithStag(b []byte, stag int) ([]byte, error) {
    if len(b) < 6 {
        return b, nil
    }
    if binary.LittleEndian.Uint16(b[:2]) != Version {
        return b, nil
    }
    fr := flate.NewReader(bytes.NewReader(b[6:]))
    dec, err := io.ReadAll(fr)
    fr.Close()
    if err != nil { return b, nil }
    if len(dec) < 35 { return b, nil }
    if dec[len(dec)-1] != 'Q' { dec = append(dec, 'Q') }
    parsed := parseTagCountFromDec(dec)
    if parsed < 1 { parsed = 1 }
    wantCnt := parsed + 1
    swap := func(v uint16) uint16 { return (v<<8)&0xFF00 | (v>>8)&0x00FF }
    binary.LittleEndian.PutUint16(dec[18:20], swap(uint16(wantCnt)))
    if stag < 0 { stag = 0 }
    binary.LittleEndian.PutUint16(dec[26:28], swap(uint16(stag)))
    var comp bytes.Buffer
    w, _ := flate.NewWriter(&comp, flate.DefaultCompression)
    _, _ = w.Write(dec)
    _ = w.Close()
    size := 6 + comp.Len()
    header := make([]byte, 6)
    binary.LittleEndian.PutUint16(header[:2], Version)
    binary.BigEndian.PutUint32(header[2:], uint32(size))
    out := append(header, comp.Bytes()...)
    return out, nil
}

// SelectOMSPartFromPacked returns a selected part from a packed OMS payload. Stub: returns whole payload.
func SelectOMSPartFromPacked(data []byte, page, maxTags int) ([]byte, int, int, error) {
    if page <= 0 { page = 1 }
    if maxTags <= 0 { return data, 1, 1, nil }
    return data, 1, 1, nil
}

