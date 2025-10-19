package oms

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"image"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"

	_ "golang.org/x/image/webp"

	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"sync"
	"time"

	"golang.org/x/image/draw"
	"golang.org/x/net/html"
)

var ProxyCookieJarStore interface {
	Get(key string) http.CookieJar
} = nil

var ProxyDeriveClientKey func(r *http.Request) string = nil

const defaultUpstreamUA = "Mozilla/5.0 (Linux; Android 9; OMS Test) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120 Safari/537.36"

// DefaultUpstreamUA exposes the UA string used for origin requests so other packages
// (e.g., proxy download handlers) can mimic the renderer's behaviour.
const DefaultUpstreamUA = defaultUpstreamUA

const defaultPaginationBytes = 32000
const maxInlineBackgroundSize = 128

// ---------------------- Minimal CSS support ----------------------

// isWhiteHex returns true if the color equals #ffffff (case-insensitive).
func isWhiteHex(hex string) bool {
	return strings.EqualFold(strings.TrimSpace(hex), "#ffffff")
}

func hexBrightness(hex string) int {
	hex = strings.TrimPrefix(strings.TrimSpace(hex), "#")
	if len(hex) != 6 {
		return 255
	}
	r, _ := strconv.ParseInt(hex[0:2], 16, 64)
	g, _ := strconv.ParseInt(hex[2:4], 16, 64)
	b, _ := strconv.ParseInt(hex[4:6], 16, 64)
	return int(0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b))
}

func isDarkHex(hex string) bool { return hexBrightness(hex) < 60 }

func relLuma(hex string) float64 {
	hex = strings.TrimPrefix(strings.TrimSpace(hex), "#")
	if len(hex) != 6 {
		return 1.0
	}
	toLin := func(c int64) float64 {
		v := float64(c) / 255.0
		if v <= 0.03928 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	r, _ := strconv.ParseInt(hex[0:2], 16, 64)
	g, _ := strconv.ParseInt(hex[2:4], 16, 64)
	b, _ := strconv.ParseInt(hex[4:6], 16, 64)
	return 0.2126*toLin(r) + 0.7152*toLin(g) + 0.0722*toLin(b)
}

func contrastRatio(a, b string) float64 {
	la := relLuma(a)
	lb := relLuma(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func lightenHex(hex string, percent int) string {
	hex = strings.TrimPrefix(strings.TrimSpace(hex), "#")
	if len(hex) != 6 {
		return "#" + hex
	}
	r, _ := strconv.ParseInt(hex[0:2], 16, 64)
	g, _ := strconv.ParseInt(hex[2:4], 16, 64)
	b, _ := strconv.ParseInt(hex[4:6], 16, 64)
	lighten := func(c int64) int64 {
		c = c + (255-c)*int64(percent)/100
		if c > 255 {
			c = 255
		}
		return c
	}
	r = lighten(r)
	g = lighten(g)
	b = lighten(b)
	return fmt.Sprintf("#%02x%02x%02x", r, g, b)
}

func ensureMinForRGB565(hex string) string {
	hex = strings.TrimPrefix(strings.TrimSpace(hex), "#")
	if len(hex) != 6 {
		return "#" + hex
	}
	r, _ := strconv.ParseInt(hex[0:2], 16, 64)
	g, _ := strconv.ParseInt(hex[2:4], 16, 64)
	b, _ := strconv.ParseInt(hex[4:6], 16, 64)

	const minR = 16
	const minG = 16
	const minB = 16

	if r < minR {
		r = minR
	}
	if g < minG {
		g = minG
	}
	if b < minB {
		b = minB
	}

	return fmt.Sprintf("#%02x%02x%02x", r, g, b)
}

func normalizeBgForBlackText(bg string) string {
	bgHex := cssToHex(bg)
	if bgHex == "" {
		return ""
	}
	const targetCR = 4.5
	const step = 12
	const maxLoops = 8

	if contrastRatio(bgHex, "#000000") >= targetCR {
		return bgHex
	}
	cur := bgHex
	for i := 0; i < maxLoops; i++ {
		cur = lightenHex(cur, step)
		if contrastRatio(cur, "#000000") >= targetCR {
			break
		}
	}
	cur = ensureMinForRGB565(cur)
	return cur
}

func effectiveTextColor(n *html.Node, st *walkState) string {
	a := n.Parent
	for a != nil && a.Type != html.ElementNode {
		a = a.Parent
	}
	for cur := a; cur != nil; cur = cur.Parent {
		if cur.Type != html.ElementNode {
			continue
		}
		if stl := getAttr(cur, "style"); stl != "" {
			if col := parseCssColor(stl, "color"); col != "" {
				return col
			}
		}
		if strings.EqualFold(cur.Data, "font") {
			if col := cssToHex(getAttr(cur, "color")); col != "" {
				return col
			}
		}
		if st.css != nil {
			if props := computeStyleFor(cur, st.css); props != nil {
				if v := cssToHex(props["color"]); v != "" {
					return v
				}
			}
		}
		if strings.EqualFold(cur.Data, "body") {
			if col := cssToHex(getAttr(cur, "text")); col != "" {
				return col
			}
			if stl := getAttr(cur, "style"); stl != "" {
				if col := parseCssColor(stl, "color"); col != "" {
					return col
				}
			}
		}
	}
	return ""
}

func addTextWithColor(p *Page, st *walkState, n *html.Node, text string) {
	if text == "" {
		return
	}

	want := findTextColorFor(n, st)
	if want != "" && want != st.curColor {
		prev := st.curColor
		// Устанавливаем цвет без потери жирного/курсива:
		p.AddStyle(st.curStyle | (uint32(calcColor(want)) << 8))
		p.AddText(text)
		// Восстановить предыдущий цвет (или чёрный как дефолт):
		restore := prev
		if restore == "" {
			restore = "#000000"
		}
		p.AddStyle(st.curStyle | (uint32(calcColor(restore)) << 8))
		return
	}
	p.AddText(text)
}

// cssToHex normalizes common CSS color syntaxes into #rrggbb.
// Supports: #rgb/#rrggbb, black/white, rgb()/rgba() with 0-255 or % values.
func cssToHex(v string) string {
	s := strings.ToLower(strings.TrimSpace(v))
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "#") {
		if len(s) == 4 { // #rgb -> #rrggbb
			r := string([]byte{s[1], s[1]})
			g := string([]byte{s[2], s[2]})
			b := string([]byte{s[3], s[3]})
			return "#" + r + g + b
		}
		if len(s) >= 7 {
			return s[:7]
		}
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
			if len(parts) < 3 {
				parts = strings.Fields(inner)
			}
			if len(parts) >= 3 {
				toByte := func(x string) int {
					x = strings.TrimSpace(x)
					if strings.HasSuffix(x, "%") {
						x = strings.TrimSuffix(x, "%")
						if p, err := strconv.Atoi(x); err == nil {
							if p < 0 {
								p = 0
							} else if p > 100 {
								p = 100
							}
							return int(float64(p) * 255.0 / 100.0)
						}
						return 0
					}
					if n, err := strconv.Atoi(x); err == nil {
						if n < 0 {
							n = 0
						} else if n > 255 {
							n = 255
						}
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
				out[1] = hexd[r>>4]
				out[2] = hexd[r&0xF]
				out[3] = hexd[g>>4]
				out[4] = hexd[g&0xF]
				out[5] = hexd[b>>4]
				out[6] = hexd[b&0xF]
				return string(out)
			}
		}
	}
	return ""
}

// splitByTags splits a raw payload (without V2 header) into parts with at most
// maxTags tags each. Each part starts with the original leading OMS string (URL).
// maxBytesBudget returns the per-part byte budget used for pagination.
// Default is 32KB, optionally overridden by OMS_PAGINATE_BYTES (min 1KB).
func maxBytesBudget() int {
	maxBytes := defaultPaginationBytes
	if s := strings.TrimSpace(os.Getenv("OMS_PAGINATE_BYTES")); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			if v <= 0 {
				maxBytes = 0
			} else {
				maxBytes = v
			}
		}
	}
	if maxBytes > 0 && maxBytes < 1024 {
		maxBytes = 1024
	}
	return maxBytes
}

// shrinkPartToMaxBytes trims a single part (prefix + tagged body) so that its
// total raw size does not exceed limit. Trimming respects tag boundaries.
func shrinkPartToMaxBytes(part []byte, limit int) []byte {
	if limit <= 0 || len(part) <= limit || len(part) < 2 {
		return part
	}
	// Prefix: initial OMS string
	l := int(binary.BigEndian.Uint16(part[0:2]))
	if 2+l > len(part) {
		return part
	}
	// Ensure limit is at least room for prefix
	if limit <= 2+l {
		// Cannot fit any body; return only prefix; finalize() will add 'Q'.
		return append([]byte{}, part[:2+l]...)
	}
	allowedBody := limit - (2 + l)
	start := 2 + l
	p := start
	limitAll := len(part)
	for p < limitAll {
		tag := part[p]
		// Prospective new position after including this tag fully
		np := p + 1
		switch tag {
		case 'T', 'L':
			if np+2 > limitAll {
				np = limitAll
				break
			}
			ln := int(binary.BigEndian.Uint16(part[np : np+2]))
			np += 2 + ln
		case 'E', 'B', '+', 'V', 'Q', 'l':
			// no payload
		case 'D', 'R':
			np += 2
		case 'S', 'J':
			np += 4
		case 'I':
			if np+8 > limitAll {
				np = limitAll
				break
			}
			dl := int(binary.BigEndian.Uint16(part[np+4 : np+6]))
			np += 8 + dl
		case 'k':
			// type + string
			if np+1 > limitAll {
				np = limitAll
				break
			}
			np += 1
			if np+2 > limitAll {
				np = limitAll
				break
			}
			ln := int(binary.BigEndian.Uint16(part[np : np+2]))
			np += 2 + ln
		case 'h':
			for i := 0; i < 2; i++ {
				if np+2 > limitAll {
					np = limitAll
					break
				}
				ln := int(binary.BigEndian.Uint16(part[np : np+2]))
				np += 2 + ln
			}
		case 'x':
			np += 1
			for i := 0; i < 2; i++ {
				if np+2 > limitAll {
					np = limitAll
					break
				}
				ln := int(binary.BigEndian.Uint16(part[np : np+2]))
				np += 2 + ln
			}
		case 'p', 'u', 'i', 'b', 'e':
			for i := 0; i < 2; i++ {
				if np+2 > limitAll {
					np = limitAll
					break
				}
				ln := int(binary.BigEndian.Uint16(part[np : np+2]))
				np += 2 + ln
			}
		case 'c', 'r':
			for i := 0; i < 2; i++ {
				if np+2 > limitAll {
					np = limitAll
					break
				}
				ln := int(binary.BigEndian.Uint16(part[np : np+2]))
				np += 2 + ln
			}
			np += 1
		case 's':
			if np+2 > limitAll {
				np = limitAll
				break
			}
			ln := int(binary.BigEndian.Uint16(part[np : np+2]))
			np += 2 + ln
			if np+1 > limitAll {
				np = limitAll
				break
			}
			np += 1
			if np+2 > limitAll {
				np = limitAll
				break
			}
			np += 2
		case 'o':
			for i := 0; i < 2; i++ {
				if np+2 > limitAll {
					np = limitAll
					break
				}
				ln := int(binary.BigEndian.Uint16(part[np : np+2]))
				np += 2 + ln
			}
			np += 1
		default:
			// Unknown tag: stop
			np = limitAll
		}
		nextBody := (np - start)
		if nextBody > allowedBody {
			break
		}
		p = np
	}
	if p <= start { // nothing fits beyond prefix
		return append([]byte{}, part[:2+l]...)
	}
	return append([]byte{}, part[:p]...)
}

func splitByTags(b []byte, maxTags int) [][]byte {
	if maxTags <= 0 || len(b) < 2 {
		return [][]byte{b}
	}
	maxBytes := maxBytesBudget()
	// Prefix is initial page URL string (len + bytes)
	if len(b) < 2 {
		return [][]byte{b}
	}
	l := int(binary.BigEndian.Uint16(b[0:2]))
	if 2+l > len(b) {
		return [][]byte{b}
	}
	prefix := make([]byte, 2+l)
	copy(prefix, b[:2+l])
	p := 2 + l
	// Capture a short prelude of global tags (style/bg/auth) to prepend to
	// all parts after the first, so style/fg/bg persist across pages for OM2.
	preludeStart := p
	pp := p
	for pp < len(b) {
		tag := b[pp]
		pp++
		switch tag {
		case 'S':
			if pp+4 > len(b) {
				pp = len(b)
				break
			}
			pp += 4
		case 'D':
			if pp+2 > len(b) {
				pp = len(b)
				break
			}
			pp += 2
		case 'k':
			// type + string
			if pp+1 > len(b) {
				pp = len(b)
				break
			}
			pp += 1
			if pp+2 > len(b) {
				pp = len(b)
				break
			}
			ln := int(binary.BigEndian.Uint16(b[pp : pp+2]))
			pp += 2 + ln
		default:
			// stop at first non-global tag
			pp--
			goto PreludeDone
		}
		// continue scanning next tag
	}
PreludeDone:
	preludeRaw := append([]byte(nil), b[preludeStart:pp]...)
	// Normalize prelude so it preserves bg and text color without resetting it
	// back to default. Keep all 'k' tags, keep the last 'D' (bgcolor) and the
	// last 'S' that carries a non-zero color component; drop trailing plain 'S'
	// that would override color on subsequent pages.
	normalizePrelude := func(src []byte) []byte {
		var outK [][]byte
		var haveD bool
		var dTag []byte
		var sColor []byte
		pz := 0
		for pz < len(src) {
			tag := src[pz]
			start := pz
			pz++
			switch tag {
			case 'S':
				if pz+4 > len(src) {
					pz = len(src)
					break
				}
				val := src[pz : pz+4]
				pz += 4
				// style color lives in bits 8..23 (uint32 big-endian)
				sv := binary.BigEndian.Uint32(val)
				if (sv & 0x00FFFF00) != 0 { // has color component
					buf := make([]byte, 1+4)
					buf[0] = 'S'
					copy(buf[1:], val)
					sColor = buf
				}
			case 'D':
				if pz+2 > len(src) {
					pz = len(src)
					break
				}
				dTag = append([]byte(nil), src[start:pz+2]...)
				haveD = true
				pz += 2
			case 'k':
				if pz+1 > len(src) {
					pz = len(src)
					break
				}
				pz += 1 // type
				if pz+2 > len(src) {
					pz = len(src)
					break
				}
				ln := int(binary.BigEndian.Uint16(src[pz : pz+2]))
				pz += 2 + ln
				outK = append(outK, append([]byte(nil), src[start:pz]...))
			default:
				// stop parsing unknowns in prelude area
				pz = len(src)
			}
		}
		var out []byte
		for _, k := range outK {
			out = append(out, k...)
		}
		if haveD {
			out = append(out, dTag...)
		}
		if sColor != nil {
			out = append(out, sColor...)
		}
		return out
	}
	prelude := normalizePrelude(preludeRaw)
	// Keep first part intact (with prelude). Later parts will get prelude inserted.
	start := p
	tags := 0
	limit := len(b)
	parts := make([][]byte, 0, 2)
	partIdx := 0
	for p < limit {
		tag := b[p]
		p++
		switch tag {
		case 'T', 'L':
			if p+2 > limit {
				p = limit
				break
			}
			l := int(binary.BigEndian.Uint16(b[p : p+2]))
			p += 2 + l
		case 'E', 'B', '+', 'V', 'Q', 'l':
			// no payload
		case 'D', 'R':
			p += 2
		case 'S', 'J':
			p += 4
		case 'I':
			if p+8 > limit {
				p = limit
				break
			}
			dl := int(binary.BigEndian.Uint16(b[p+4 : p+6]))
			p += 8 + dl
		case 'k':
			p += 1
			if p+2 > limit {
				p = limit
				break
			}
			l := int(binary.BigEndian.Uint16(b[p : p+2]))
			p += 2 + l
		case 'h':
			for i := 0; i < 2; i++ {
				if p+2 > limit {
					p = limit
					break
				}
				l := int(binary.BigEndian.Uint16(b[p : p+2]))
				p += 2 + l
			}
		case 'x':
			p += 1
			for i := 0; i < 2; i++ {
				if p+2 > limit {
					p = limit
					break
				}
				l := int(binary.BigEndian.Uint16(b[p : p+2]))
				p += 2 + l
			}
		case 'p', 'u', 'i', 'b', 'e':
			for i := 0; i < 2; i++ {
				if p+2 > limit {
					p = limit
					break
				}
				l := int(binary.BigEndian.Uint16(b[p : p+2]))
				p += 2 + l
			}
		case 'c', 'r':
			for i := 0; i < 2; i++ {
				if p+2 > limit {
					p = limit
					break
				}
				l := int(binary.BigEndian.Uint16(b[p : p+2]))
				p += 2 + l
			}
			p += 1
		case 's':
			if p+2 > limit {
				p = limit
				break
			}
			l := int(binary.BigEndian.Uint16(b[p : p+2]))
			p += 2 + l
			if p+1 > limit {
				p = limit
				break
			}
			p += 1
			if p+2 > limit {
				p = limit
				break
			}
			p += 2 // count (ignored here)
		case 'o':
			for i := 0; i < 2; i++ {
				if p+2 > limit {
					p = limit
					break
				}
				l := int(binary.BigEndian.Uint16(b[p : p+2]))
				p += 2 + l
			}
			p += 1
		default:
			// Unknown: stop further splitting
			p = limit
		}
		tags++
		chunkBytes := p - start
		if tags >= maxTags || (maxBytes > 0 && chunkBytes >= maxBytes) {
			// Cut part [start:p)
			chunk := append([]byte(nil), b[start:p]...)
			part := append([]byte(nil), prefix...)
			if partIdx > 0 && len(prelude) > 0 {
				part = append(part, prelude...)
			}
			part = append(part, chunk...)
			parts = append(parts, part)
			start = p
			tags = 0
			partIdx++
		}
	}
	if start < limit {
		part := append([]byte(nil), prefix...)
		if partIdx > 0 && len(prelude) > 0 {
			part = append(part, prelude...)
		}
		part = append(part, b[start:limit]...)
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return [][]byte{b}
	}
	return parts
}

// ---------------------- Image cache (LRU by bytes) ----------------------

type imgEntry struct {
	key        string
	data       []byte
	w, h       int
	prev, next *imgEntry
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
	if c.head == e {
		return
	}
	if e.prev != nil {
		e.prev.next = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	}
	if c.tail == e {
		c.tail = e.prev
	}
	e.prev = nil
	e.next = c.head
	if c.head != nil {
		c.head.prev = e
	}
	c.head = e
	if c.tail == nil {
		c.tail = e
	}
}

func (c *imgLRU) get(key string) ([]byte, int, int, bool) {
	if c == nil {
		return nil, 0, 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[key]; ok {
		c.moveFront(e)
		return append([]byte(nil), e.data...), e.w, e.h, true
	}
	return nil, 0, 0, false
}

func (c *imgLRU) put(key string, data []byte, w, h int) {
	if c == nil || c.max <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[key]; ok {
		c.size -= int64(len(e.data))
		e.data = append([]byte(nil), data...)
		e.w, e.h = w, h
		c.size += int64(len(e.data))
		c.moveFront(e)
	} else {
		e := &imgEntry{key: key, data: append([]byte(nil), data...), w: w, h: h}
		e.next = c.head
		if c.head != nil {
			c.head.prev = e
		}
		c.head = e
		if c.tail == nil {
			c.tail = e
		}
		c.m[key] = e
		c.size += int64(len(e.data))
	}
	for c.size > c.max && c.tail != nil {
		old := c.tail
		delete(c.m, old.key)
		c.size -= int64(len(old.data))
		c.tail = old.prev
		if c.tail != nil {
			c.tail.next = nil
		} else {
			c.head = nil
		}
	}
}

var globalImgCache = func() *imgLRU {
	mb := 100
	if s := os.Getenv("OMS_IMG_CACHE_MB"); s != "" {
		if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && v >= 0 {
			mb = v
		}
	}
	if mb <= 0 {
		return nil
	}
	return newImgLRU(int64(mb) * 1024 * 1024)
}()

func imgCacheKey(format string, quality int, url string) string {
	return format + "|q=" + strconv.Itoa(quality) + "|" + url
}

func imgCacheGet(format string, quality int, url string) ([]byte, int, int, bool) {
	if globalImgCache == nil {
		return nil, 0, 0, false
	}
	return globalImgCache.get(imgCacheKey(format, quality, url))
}

func imgCachePut(format string, quality int, url string, data []byte, w, h int) {
	if globalImgCache == nil {
		return
	}
	globalImgCache.put(imgCacheKey(format, quality, url), data, w, h)
}

func addHeader(p *Page) {
	p.AddString("1/internal:error")
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
			if c == ';' || c == ' ' || c == '"' || c == '\'' {
				v = v[:j]
				break
			}
		}
		return strings.TrimSpace(v)
	}
	// Search first 2KB of body for meta charset
	n := len(body)
	if n > 2048 {
		n = 2048
	}
	low := strings.ToLower(string(body[:n]))
	if i := strings.Index(low, "charset="); i != -1 {
		v := low[i+8:]
		for j, c := range v {
			if c == ';' || c == ' ' || c == '"' || c == '\'' || c == '>' {
				v = v[:j]
				break
			}
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
			ci := int(c)
			r := rune(0x0410 + (ci - 0xC0))
			out.WriteRune(r)
		case c >= 0xE0 && c <= 0xFF: // Р°..СЏ
			ci := int(c)
			r := rune(0x0430 + (ci - 0xE0))
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
	ensureUpstreamUserAgent(hdr)
	// РџСЂРёРјРµРЅСЏРµРј РІСЃРµ Р·Р°РіРѕР»РѕРІРєРё Рє Р·Р°РїСЂРѕСЃСѓ
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	jar := http.CookieJar(nil)
	if ProxyCookieJarStore != nil && ProxyDeriveClientKey != nil {
		jar = ProxyCookieJarStore.Get(ProxyDeriveClientKey(req))
	}

	client := &http.Client{
		Timeout: 15 * time.Second,
		Jar:     jar,
	}

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
	headerWord := binary.LittleEndian.Uint16(b[:2])
	compression := compressionFromHeaderByte(byte(headerWord >> 8))
	dec, err := decompressPayload(compression, b[6:])
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
	bgStack    []string
	curBg      string
	formStack  []string
}

// resolveFormActionURL resolves a form action reference against the base page URL and
// normalises it for use as a cache key when storing hidden form fields.
func resolveFormActionURL(base, action string) string {
	base = strings.TrimSpace(base)
	action = strings.TrimSpace(action)
	var baseURL *url.URL
	if base != "" {
		if bu, err := url.Parse(base); err == nil {
			baseURL = bu
		}
	}
	if action == "" {
		if baseURL != nil {
			clone := *baseURL
			clone.Fragment = ""
			return clone.String()
		}
		return ""
	}
	if action == "1" { // OMS placeholder for same-page action
		if baseURL != nil {
			clone := *baseURL
			clone.Fragment = ""
			return clone.String()
		}
		return ""
	}
	if baseURL == nil {
		return action
	}
	if au, err := url.Parse(action); err == nil {
		if au.IsAbs() {
			return au.String()
		}
		resolved := baseURL.ResolveReference(au)
		resolved.Fragment = ""
		return resolved.String()
	}
	return action
}

// RenderOptions define client rendering preferences relevant to OBML generation.
type RenderOptions struct {
	ImagesOn      bool
	HighQuality   bool
	ImageMIME     string // e.g. "image/jpeg", "image/png"
	MaxInlineKB   int    // max kilobytes for inline image ('I') before falling back to placeholder
	Compression   CompressionMethod
	ReqHeaders    http.Header    // copy of page request headers (UA, Lang, Cookies)
	Referrer      string         // page URL for Referer
	OriginCookies string         // cookies set by origin page (name=value; ...)
	Jar           http.CookieJar // optional cookie jar for origin requests
	// Opera Mini auth echo: include these as 'k' tags ('authcode' and 'authprefix')
	AuthCode       string
	AuthPrefix     string
	GatewayVersion int // OM gateway discriminator (280 for 2.x, 285 for 3.x)
	ScreenW        int
	ScreenH        int
	NumColors      int
	HeapBytes      int // client-reported memory budget (bytes)
	AlphaLevels    int
	FormBody       string
	// Pagination: 1-based page index and max tags per page (0=disabled)
	Page           int
	MaxTagsPerPage int
	// Optional absolute base (scheme://host) for building navigation links
	ServerBase    string
	Styles        *Stylesheet
	WantFullCache bool
	ClientVersion ClientVersion
}

// BuildPaginationQuery encodes paging parameters while preserving render options that affect output quality.
func BuildPaginationQuery(target string, opts *RenderOptions, page, maxTags int) string {
	// For page 1, return a minimal query and strip our internal paging marker
	// from target ("__p=") so the first page opens exactly like the initial
	// load and can be satisfied from client cache.
	if page <= 1 {
		t := target
		if u, err := url.Parse(target); err == nil {
			q := u.Query()
			if q.Has("__p") {
				q.Del("__p")
				u.RawQuery = q.Encode()
				t = u.String()
			}
		}
		vals := url.Values{}
		vals.Set("url", t)
		return vals.Encode()
	}
	vals := url.Values{}
	vals.Set("url", target)
	if maxTags > 0 {
		vals.Set("pp", strconv.Itoa(maxTags))
	}
	vals.Set("page", strconv.Itoa(page))
	if opts != nil {
		if opts.ImagesOn {
			vals.Set("img", "1")
		}
		if opts.HighQuality {
			vals.Set("hq", "1")
		}
		if opts.ImageMIME != "" {
			vals.Set("mime", opts.ImageMIME)
		}
		if opts.MaxInlineKB > 0 {
			vals.Set("maxkb", strconv.Itoa(opts.MaxInlineKB))
		}
		if opts.ScreenW > 0 {
			vals.Set("w", strconv.Itoa(opts.ScreenW))
		}
		if opts.ScreenH > 0 {
			vals.Set("h", strconv.Itoa(opts.ScreenH))
		}
		// Only propagate memory/alpha when client explicitly provided them (>0)
		if opts.HeapBytes > 0 {
			vals.Set("m", strconv.Itoa(opts.HeapBytes))
		}
		if opts.AlphaLevels > 0 {
			vals.Set("l", strconv.Itoa(opts.AlphaLevels))
		}
		// Preserve Opera Mini auth echo and client discriminator so
		// subsequent navigations render with the same context as the first load.
		if strings.TrimSpace(opts.AuthCode) != "" {
			vals.Set("c", opts.AuthCode)
		}
		if strings.TrimSpace(opts.AuthPrefix) != "" {
			vals.Set("h", opts.AuthPrefix)
		}
		if opts.GatewayVersion > 0 {
			vals.Set("o", strconv.Itoa(opts.GatewayVersion))
		}
		// Some clients pass explicit protocol version; keep it if caller set it.
		switch normalizeClientVersion(opts.ClientVersion) {
		case ClientVersion1:
			vals.Set("version", "1")
		case ClientVersion3:
			vals.Set("version", "3")
		}
	}
	return vals.Encode()
}

// GetAttr is an exported helper for debug code paths.
func GetAttr(n *html.Node, name string) string { return getAttr(n, name) }

func ensureUpstreamUserAgent(hdr http.Header) {
	if hdr == nil {
		return
	}
	orig := hdr.Get("User-Agent")
	if orig == "" {
		return
	}
	if isLegacyOperaMiniUA(orig) {
		if hdr.Get("X-OperaMini-UA") == "" {
			hdr.Set("X-OperaMini-UA", orig)
		}
		if hdr.Get("X-OperaMini-Phone-UA") == "" {
			hdr.Set("X-OperaMini-Phone-UA", orig)
		}
		hdr.Set("User-Agent", defaultUpstreamUA)
	}
}

func isLegacyOperaMiniUA(ua string) bool {
	ua = strings.ToLower(strings.TrimSpace(ua))
	if ua == "" {
		return false
	}
	if strings.Contains(ua, "opera mini/") {
		return true
	}
	if strings.HasPrefix(ua, "opera/") && strings.Contains(ua, "midp") {
		return true
	}
	return false
}

func defaultRenderPrefs() RenderOptions {
	// Default pagination disabled; can be overridden via env in loader
	return RenderOptions{
		ImagesOn:      false,
		HighQuality:   false,
		ImageMIME:     "image/jpeg",
		MaxInlineKB:   96,
		Compression:   CompressionDeflate,
		ClientVersion: ClientVersion2,
	}
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
	var colorPart uint32
	if s.curColor != "" {
		colorPart = uint32(calcColor(s.curColor)) << 8
	}
	p.AddStyle(style | colorPart)
}

func (s *walkState) popStyle(p *Page) {
	if len(s.styleStack) == 0 {
		return
	}
	s.curStyle = s.styleStack[len(s.styleStack)-1]
	s.styleStack = s.styleStack[:len(s.styleStack)-1]
	var colorPart uint32
	if s.curColor != "" {
		colorPart = uint32(calcColor(s.curColor)) << 8
	}
	p.AddStyle(s.curStyle | colorPart)
}

func (s *walkState) pushColor(p *Page, hex string) {
	s.colorStack = append(s.colorStack, s.curColor)
	s.curColor = hex
	if hex != "" {
		cur := s.curStyle
		if len(s.styleStack) > 0 {
			cur = s.styleStack[len(s.styleStack)-1]
		}
		p.AddStyle(cur | (uint32(calcColor(hex)) << 8))
	}
}

func (s *walkState) popColor(p *Page) {
	if len(s.colorStack) == 0 {
		return
	}
	prev := s.colorStack[len(s.colorStack)-1]
	s.colorStack = s.colorStack[:len(s.colorStack)-1]
	s.curColor = prev
	cur := s.curStyle
	if len(s.styleStack) > 0 {
		cur = s.styleStack[len(s.styleStack)-1]
	}
	if prev != "" {
		p.AddStyle(cur | (uint32(calcColor(prev)) << 8))
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

func hasClass(n *html.Node, want string) bool {
	want = strings.ToLower(strings.TrimSpace(want))
	if want == "" {
		return false
	}
	classes := strings.Fields(strings.ToLower(getAttr(n, "class")))
	for _, c := range classes {
		if c == want {
			return true
		}
	}
	return false
}

func hasAnyClass(n *html.Node, classes ...string) bool {
	for _, cls := range classes {
		if hasClass(n, cls) {
			return true
		}
	}
	return false
}

func parseCssValue(style, prop string) string {
	if style == "" {
		return ""
	}
	prop = strings.ToLower(strings.TrimSpace(prop))
	if prop == "" {
		return ""
	}
	for _, part := range strings.Split(style, ";") {
		kv := strings.SplitN(part, ":", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(kv[0]))
		if key != prop {
			continue
		}
		return strings.TrimSpace(kv[1])
	}
	return ""
}

func stripCssImportant(val string) string {
	trimmed := strings.TrimSpace(val)
	lower := strings.ToLower(trimmed)
	if strings.HasSuffix(lower, "!important") {
		return strings.TrimSpace(trimmed[:len(trimmed)-len("!important")])
	}
	return trimmed
}

func cssPropValue(props map[string]string, inline, prop string) string {
	if prop == "" {
		return ""
	}
	key := strings.ToLower(prop)
	if props != nil {
		if v := strings.TrimSpace(props[key]); v != "" {
			return stripCssImportant(v)
		}
	}
	if inline != "" {
		if v := parseCssValue(inline, key); v != "" {
			return stripCssImportant(v)
		}
	}
	return ""
}

func extractBackgroundImageURL(value string) string {
	v := strings.TrimSpace(value)
	if v == "" {
		return ""
	}
	lower := strings.ToLower(v)
	searchIdx := 0
	for {
		idx := strings.Index(lower[searchIdx:], "url(")
		if idx == -1 {
			return ""
		}
		idx += searchIdx
		start := idx + 4
		depth := 1
		end := start
		for end < len(v) && depth > 0 {
			switch v[end] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					raw := strings.TrimSpace(v[start:end])
					raw = strings.Trim(raw, "\"'")
					if raw != "" && !strings.EqualFold(raw, "none") {
						return raw
					}
				}
			}
			end++
		}
		if depth > 0 || end >= len(v) {
			return ""
		}
		searchIdx = end
		if searchIdx >= len(v) {
			return ""
		}
	}
}

func cssValueToPx(val string, base int) int {
	val = stripCssImportant(val)
	if val == "" {
		return 0
	}
	px, ok := cssLengthToPx(val, base)
	if !ok || px <= 0 {
		return 0
	}
	return px
}

func hasTextContent(n *html.Node) bool {
	if n == nil {
		return false
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode && strings.TrimSpace(c.Data) != "" {
			return true
		}
		if c.Type == html.ElementNode {
			switch strings.ToLower(c.Data) {
			case "script", "style", "noscript":
				continue
			}
			if hasTextContent(c) {
				return true
			}
		}
	}
	return false
}

func renderBackgroundImage(n *html.Node, props map[string]string, base string, p *Page, prefs RenderOptions) bool {
	if n == nil || p == nil {
		return false
	}
	// Render small decorative backgrounds even when images are globally off
	// (icons, sprites). We keep hard limit by dimensions below.
	allowWhenImagesOff := true
	if !prefs.ImagesOn && !allowWhenImagesOff {
		return false
	}
	// Never draw background sprites directly on form controls to avoid
	// covering native widgets (search button/inputs etc.).
	if n.Type == html.ElementNode && isFormControlTag(n.Data) {
		return false
	}
	// Also avoid background images for containers that include form controls.
	if containsFormControl(n) {
		return false
	}
	inlineStyle := getAttr(n, "style")
	bgVal := cssPropValue(props, inlineStyle, "background-image")
	if bgVal == "" {
		bgVal = cssPropValue(props, inlineStyle, "background")
	}
	if bgVal == "" {
		return false
	}
	urlVal := extractBackgroundImageURL(bgVal)
	if urlVal == "" {
		return false
	}
	// No tag restriction: any element can carry a small decorative background
	if hasTextContent(n) {
		return false
	}
	repeat := strings.ToLower(cssPropValue(props, inlineStyle, "background-repeat"))
	if repeat != "" && repeat != "no-repeat" && repeat != "initial" {
		return false
	}
	widthHint := cssValueToPx(cssPropValue(props, inlineStyle, "width"), prefs.ScreenW)
	heightHint := cssValueToPx(cssPropValue(props, inlineStyle, "height"), prefs.ScreenH)
	if widthHint > maxInlineBackgroundSize || heightHint > maxInlineBackgroundSize {
		return false
	}
	abs := urlVal
	if !strings.HasPrefix(urlVal, "data:") {
		if base != "" {
			if resolved := resolveAbsURL(base, urlVal); resolved != "" {
				abs = resolved
			}
		}
		if !strings.Contains(abs, "://") && !strings.HasPrefix(abs, "data:") {
			return false
		}
	}
	// Parse background-position for sprite cropping if present
	posX, posY, hasPos := parseBackgroundPosition(cssPropValue(props, inlineStyle, "background-position"))

	data, w, h, ok := fetchAndEncodeImage(abs, prefs)
	if !ok {
		return false
	}
	if widthHint <= 0 {
		widthHint = w
	}
	if heightHint <= 0 {
		heightHint = h
	}
	if widthHint <= 0 || heightHint <= 0 {
		return false
	}
	if widthHint > maxInlineBackgroundSize || heightHint > maxInlineBackgroundSize {
		return false
	}
	if prefs.MaxInlineKB > 0 && len(data) > prefs.MaxInlineKB*1024 {
		return false
	}
	if hasPos {
		// CSS background-position offsets shift the image relative to the box.
		// Negative values mean the sprite is shifted left/up, so visible region starts at -pos.
		cropX := -posX
		cropY := -posY
		if cropped, ok := fetchAndEncodeImageRegion(abs, prefs, cropX, cropY, widthHint, heightHint); ok {
			if prefs.MaxInlineKB <= 0 || len(cropped) <= prefs.MaxInlineKB*1024 {
				p.AddImageInline(widthHint, heightHint, cropped)
				return true
			}
		}
		// If cropping fails, fall back to full image rendering.
	}
	p.AddImageInline(widthHint, heightHint, data)
	return true
}

// isBgPaintableTag returns true for structural/container tags where a
// block background-color makes sense to render as a segment. Inline
// controls and phrasing content are excluded to avoid painting over
// buttons/inputs/links.
func isBgPaintableTag(tag string) bool {
	switch strings.ToLower(tag) {
	case "div", "section", "article", "header", "footer", "main", "nav", "aside",
		"ul", "ol", "li", "table", "tbody", "thead", "tr", "td", "th":
		return true
	}
	return false
}

// isFormControlTag returns true for form controls which should not get
// background overlays (neither color nor sprite) applied directly or by
// container heuristics around them.
func isFormControlTag(tag string) bool {
	switch strings.ToLower(tag) {
	case "input", "button", "select", "textarea", "label":
		return true
	}
	return false
}

// containsFormControl reports whether subtree n contains any form controls.
func containsFormControl(n *html.Node) bool {
	if n == nil {
		return false
	}
	var dfs func(*html.Node) bool
	dfs = func(x *html.Node) bool {
		if x.Type == html.ElementNode && isFormControlTag(x.Data) {
			return true
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			if dfs(c) {
				return true
			}
		}
		return false
	}
	return dfs(n)
}

// cssEffectiveProp returns node's CSS property or nearest inherited ancestor value
// for a small set of inheritable properties used by the renderer.
func cssEffectiveProp(n *html.Node, ss *Stylesheet, self map[string]string, prop string) string {
	if self != nil {
		if v := strings.TrimSpace(self[strings.ToLower(prop)]); v != "" {
			return v
		}
	}
	switch strings.ToLower(prop) {
	case "color", "text-align", "font-weight", "font-style", "text-decoration", "list-style-type":
		depth := 0
		for p := n.Parent; p != nil && depth < 12; p = p.Parent {
			if p.Type != html.ElementNode {
				continue
			}
			if props := computeStyleFor(p, ss); props != nil {
				if v := strings.TrimSpace(props[strings.ToLower(prop)]); v != "" {
					return v
				}
			}
			depth++
		}
	}
	return ""
}

func (s *walkState) pushBgcolor(p *Page, hex string) {
	s.bgStack = append(s.bgStack, s.curBg)
	s.curBg = hex
	if hex != "" {
		p.AddBgcolor(hex)
	}
}

func (s *walkState) popBgcolor(p *Page) {
	if len(s.bgStack) == 0 {
		return
	}
	prev := s.bgStack[len(s.bgStack)-1]
	s.bgStack = s.bgStack[:len(s.bgStack)-1]
	s.curBg = prev
	if prev != "" {
		p.AddBgcolor(prev)
	}
}

func resetComputedStyles(st *walkState, p *Page, colorPushed *bool, stylePushed *bool, alignedPushed *bool) {
	if *colorPushed {
		st.popColor(p)
		*colorPushed = false
	}
	if *stylePushed {
		st.popStyle(p)
		*stylePushed = false
	}
	if *alignedPushed {
		st.popStyle(p)
		*alignedPushed = false
	}
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

// parseBackgroundPosition parses simple background-position values like "-24px 0" or "0 0".
// Returns x, y pixel offsets; boolean indicates if any value was parsed.
func parseBackgroundPosition(val string) (int, int, bool) {
	v := strings.TrimSpace(val)
	if v == "" {
		return 0, 0, false
	}
	lower := strings.ToLower(v)
	lower = condenseSpaces(strings.ReplaceAll(lower, ",", " "))
	parts := strings.Fields(lower)
	if len(parts) == 0 {
		return 0, 0, false
	}
	parse := func(s string) (int, bool) {
		s = strings.TrimSpace(s)
		switch s {
		case "left", "top", "center":
			return 0, true
		case "right", "bottom":
			return 0, true
		}
		if strings.HasSuffix(s, "px") {
			s = strings.TrimSpace(s[:len(s)-2])
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			if f >= 0 {
				return int(f + 0.5), true
			}
			return int(f - 0.5), true
		}
		return 0, false
	}
	if len(parts) == 1 {
		if x, ok := parse(parts[0]); ok {
			return x, 0, true
		}
		return 0, 0, false
	}
	x, okx := parse(parts[0])
	y, oky := parse(parts[1])
	if okx || oky {
		return x, y, true
	}
	return 0, 0, false
}

// fetchAndEncodeImageRegion fetches an image, crops the (x,y,w,h) rectangle and encodes it.
// Uses existing caches with a region-specific key.
func fetchAndEncodeImageRegion(absURL string, prefs RenderOptions, x, y, w, h int) ([]byte, bool) {
	if w <= 0 || h <= 0 {
		return nil, false
	}
	// Region cache key
	regionKey := absURL + "#rect=" + strconv.Itoa(x) + "," + strconv.Itoa(y) + "," + strconv.Itoa(w) + "," + strconv.Itoa(h)
	candidates := cacheCandidatesFor(prefs)
	for _, cand := range candidates {
		if data, _, _, ok := imgCacheGet(cand.format, cand.quality, regionKey); ok {
			return data, true
		}
		if data, _, _, ok := diskCacheGet(cand.format, cand.quality, regionKey); ok {
			imgCachePut(cand.format, cand.quality, regionKey, data, w, h)
			return data, true
		}
	}

	// Attempt to reuse cached full image first
	var srcBytes []byte
	var have bool
	for _, cand := range candidates {
		if data, _, _, ok := imgCacheGet(cand.format, cand.quality, absURL); ok {
			srcBytes = data
			have = true
			break
		}
		if data, _, _, ok := diskCacheGet(cand.format, cand.quality, absURL); ok {
			srcBytes = data
			have = true
			break
		}
	}
	if !have {
		// Fallback to fetching from network
		req, err := http.NewRequest(http.MethodGet, absURL, nil)
		if err != nil {
			return nil, false
		}
		req.Header.Set("Accept", "image/*")
		if prefs.ReqHeaders != nil {
			if ua := prefs.ReqHeaders.Get("User-Agent"); ua != "" {
				req.Header.Set("User-Agent", ua)
			}
			if al := prefs.ReqHeaders.Get("Accept-Language"); al != "" {
				req.Header.Set("Accept-Language", al)
			}
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
		client := &http.Client{Timeout: 8 * time.Second}
		if prefs.Jar != nil {
			client.Jar = prefs.Jar
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, false
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
		b, err := io.ReadAll(rc)
		if err != nil || len(b) == 0 {
			return nil, false
		}
		srcBytes = b
	}

	// Decode and crop
	img, _, err := image.Decode(bytes.NewReader(srcBytes))
	if err != nil {
		return nil, false
	}
	b := img.Bounds()
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if x > b.Dx()-1 {
		x = b.Dx() - 1
	}
	if y > b.Dy()-1 {
		y = b.Dy() - 1
	}
	if x+w > b.Dx() {
		w = b.Dx() - x
	}
	if y+h > b.Dy() {
		h = b.Dy() - y
	}
	if w <= 0 || h <= 0 {
		return nil, false
	}
	rect := image.Rect(b.Min.X+x, b.Min.Y+y, b.Min.X+x+w, b.Min.Y+y+h)

	type subImager interface {
		SubImage(r image.Rectangle) image.Image
	}
	var region image.Image
	if si, ok := img.(subImager); ok {
		region = si.SubImage(rect)
	} else {
		dst := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.Draw(dst, dst.Bounds(), img, rect.Min, draw.Src)
		region = dst
	}

	data, _, _, format, quality, err := encodeImage(region, prefs)
	if err != nil {
		return nil, false
	}
	imgCachePut(format, quality, regionKey, data, w, h)
	diskCachePut(format, quality, regionKey, data, w, h)
	return data, true
}

func walkRich(cur *html.Node, base string, p *Page, visited map[*html.Node]bool, st *walkState, prefs RenderOptions) {
	for c := cur; c != nil; c = c.NextSibling {
		recurse := true
		var colorPushed bool
		var stylePushed bool
		var alignedPushed bool
		var bgColorPushed bool
		var bgRendered bool
		if c.Type == html.ElementNode {
			// Skip hidden elements
			if stAttr := getAttr(c, "style"); stAttr != "" && isDisplayNone(stAttr) {
				continue
			}
			// Apply computed CSS: honor display:none, text-align and color (from stylesheet)
			var props map[string]string
			if st.css != nil {
				props = computeStyleFor(c, st.css)
				if props != nil && strings.Contains(strings.ToLower(props["display"]), "none") {
					continue
				}
				if props != nil && strings.Contains(strings.ToLower(props["visibility"]), "hidden") {
					continue
				}
			}
			bgRendered = renderBackgroundImage(c, props, base, p, prefs)
			if props != nil {
				// Block background color support: only for container/structural elements
				// and only when subtree does not contain form controls to avoid flooding
				// input areas with container backgrounds.
				if isBgPaintableTag(strings.ToLower(c.Data)) && !containsFormControl(c) {
					if bgc := strings.TrimSpace(props["background-color"]); bgc != "" {
						if hx := cssToHex(bgc); hx != "" {
							st.pushBgcolor(p, hx)
							bgColorPushed = true
						}
					}
				}
				align := strings.ToLower(strings.TrimSpace(cssEffectiveProp(c, st.css, props, "text-align")))
				switch align {
				case "center":
					st.pushStyle(p, st.curStyle|styleCenterBit)
					alignedPushed = true
				case "right":
					st.pushStyle(p, st.curStyle|styleRightBit)
					alignedPushed = true
				}
				styleOverride := st.curStyle
				styleChanged := false
				if weight := strings.TrimSpace(cssEffectiveProp(c, st.css, props, "font-weight")); weight != "" {
					lw := strings.ToLower(weight)
					switch {
					case strings.Contains(lw, "bold"), strings.Contains(lw, "bolder"):
						if styleOverride&styleBoldBit == 0 {
							styleOverride |= styleBoldBit
							styleChanged = true
						}
					case strings.Contains(lw, "normal"), strings.Contains(lw, "lighter"):
						if styleOverride&styleBoldBit != 0 {
							styleOverride &^= styleBoldBit
							styleChanged = true
						}
					default:
						if n, err := strconv.Atoi(lw); err == nil {
							if n >= 600 {
								if styleOverride&styleBoldBit == 0 {
									styleOverride |= styleBoldBit
									styleChanged = true
								}
							} else if n > 0 && styleOverride&styleBoldBit != 0 {
								styleOverride &^= styleBoldBit
								styleChanged = true
							}
						}
					}
				}
				if fs := strings.ToLower(strings.TrimSpace(cssEffectiveProp(c, st.css, props, "font-style"))); fs != "" {
					if strings.Contains(fs, "italic") || strings.Contains(fs, "oblique") {
						if styleOverride&styleItalicBit == 0 {
							styleOverride |= styleItalicBit
							styleChanged = true
						}
					} else if strings.Contains(fs, "normal") {
						if styleOverride&styleItalicBit != 0 {
							styleOverride &^= styleItalicBit
							styleChanged = true
						}
					}
				}
				if td := strings.ToLower(strings.TrimSpace(cssEffectiveProp(c, st.css, props, "text-decoration"))); td != "" {
					if strings.Contains(td, "underline") {
						if styleOverride&styleUnderBit == 0 {
							styleOverride |= styleUnderBit
							styleChanged = true
						}
					} else if strings.Contains(td, "none") {
						if styleOverride&styleUnderBit != 0 {
							styleOverride &^= styleUnderBit
							styleChanged = true
						}
					}
				}
				if styleChanged && styleOverride != st.curStyle {
					st.pushStyle(p, styleOverride)
					stylePushed = true
				}
				if col := strings.TrimSpace(cssEffectiveProp(c, st.css, props, "color")); col != "" {
					st.pushColor(p, col)
					colorPushed = true
				}
			}
		}
		tag := strings.ToLower(c.Data)
		if handler, ok := extraHTML4Handlers[tag]; ok {
			ctx := elementContext{node: c, base: base, page: p, visited: visited, state: st, prefs: prefs}
			if handler(&ctx) {
				continue
			}
		}
		switch tag {
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
			if l := cssToHex(getAttr(c, "text")); l != "" {
				p.AddTextcolor(l)
				st.curColor = l
			}
			if stl := getAttr(c, "style"); stl != "" {
				if col := parseCssColor(stl, "background-color"); col != "" {
					p.AddBgcolor(col)
				}
				if col := parseCssColor(stl, "color"); col != "" {
					p.AddTextcolor(col)
					st.curColor = col
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
			if hasClass(c, "p") {
				resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
				st.pushColor(p, "#007700")
				p.AddPlus()
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				st.popColor(p)
				p.AddBreak()
				continue
			}
			if hasAnyClass(c, "ts", "tsb", "tso") {
				resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
				st.pushColor(p, "#aaaaaa")
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				st.popColor(p)
				p.AddBreak()
				continue
			}
			if hasClass(c, "center") {
				resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
				st.pushStyle(p, st.curStyle|styleCenterBit)
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				st.popStyle(p)
				p.AddBreak()
				continue
			}
			if hasClass(c, "nw") {
				resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				continue
			}
			if hasClass(c, "bro") {
				resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				p.AddBreak()
				continue
			}
			if hasClass(c, "copy") {
				resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
				st.pushStyle(p, st.curStyle|styleBoldBit)
				st.pushColor(p, "#ffffff")
				p.AddPlus()
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				st.popColor(p)
				st.popStyle(p)
				p.AddBreak()
				continue
			}
			if hasClass(c, "copy2") {
				resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
				st.pushColor(p, "#060")
				p.AddPlus()
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				st.popColor(p)
				p.AddBreak()
				continue
			}
			if hasClass(c, "pr") {
				resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
				st.pushStyle(p, st.curStyle|styleBoldBit|styleCenterBit)
				st.pushColor(p, "#ffffff")
				p.AddPlus()
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				st.popColor(p)
				st.popStyle(p)
				p.AddBreak()
				continue
			}
			if hasClass(c, "sepo") {
				resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
				st.pushStyle(p, st.curStyle|styleBoldBit)
				st.pushColor(p, "#060")
				p.AddPlus()
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				st.popColor(p)
				st.popStyle(p)
				p.AddBreak()
				continue
			}
			if hasClass(c, "bl") {
				resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
				p.AddText("| ")
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				p.AddBreak()
				continue
			}
			if hasAnyClass(c, "str-up", "str-dw") {
				resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
				p.AddPlus()
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				p.AddBreak()
				continue
			}
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
			// Avoid textual placeholders for decorative sprite spans; rely on background renderer.
			if bgRendered {
				resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				continue
			}
			if hasClass(c, "br350") {
				resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
				p.AddBreak()
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				continue
			}
			if hasClass(c, "sepo") {
				resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
				st.pushStyle(p, st.curStyle|styleBoldBit)
				st.pushColor(p, "#060")
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				st.popColor(p)
				st.popStyle(p)
				continue
			}
			if hasClass(c, "grn") {
				resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
				st.pushColor(p, "#aaaaaa")
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				st.popColor(p)
				continue
			}
			if hasClass(c, "red") {
				resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
				st.pushColor(p, "#ff4444")
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				st.popColor(p)
				continue
			}
			if hasClass(c, "zm") {
				resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
				st.pushStyle(p, st.curStyle|styleBoldBit)
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				st.popStyle(p)
				continue
			}
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
				if parseCssHas(sp, "font-weight", "bold") {
					st.pushStyle(p, st.curStyle|styleBoldBit)
					pushed++
				}
				if parseCssHas(sp, "font-style", "italic") {
					st.pushStyle(p, st.curStyle|styleItalicBit)
					pushed++
				}
				if parseCssHas(sp, "text-decoration", "underline") {
					st.pushStyle(p, st.curStyle|styleUnderBit)
					pushed++
				}
				colorPushed := false
				if col := parseCssColor(sp, "color"); col != "" {
					st.pushColor(p, col)
					colorPushed = true
				}
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				for pushed > 0 {
					pushed--
					st.popStyle(p)
				}
				if colorPushed {
					st.popColor(p)
				}
				if aligned {
					st.popStyle(p)
				}
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
			if hasClass(c, "opis") {
				st.pushColor(p, "#151515")
				colorPushed = true
			}
			buttonWrap := hasAnyClass(c, "but", "hud", "tut", "ba", "bmx", "ib-search", "butt")
			blockWrap := hasAnyClass(c, "opis", "str-up", "str-dw")
			iconOnly := isIconOnlyLink(c)
			if blockWrap {
				p.AddPlus()
			}
			if buttonWrap {
				p.AddText("[")
			}
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
			if buttonWrap {
				p.AddText("]")
				p.AddText(" ")
			}
			resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
			shouldBreak := true
			if buttonWrap || blockWrap {
				shouldBreak = true
			} else if iconOnly {
				if ns := nextSignificantSibling(c); ns != nil && ns.Type == html.ElementNode &&
					strings.EqualFold(ns.Data, "a") && isIconOnlyLink(ns) {
					shouldBreak = false
				}
			} else {
				if ns := nextSignificantSibling(c); ns != nil && ns.Type == html.TextNode &&
					hasPrefixAny(strings.TrimLeft(ns.Data, " \t\n"), "]", "|", ")") {
					shouldBreak = false
				}
			}
			if !shouldBreak && iconOnly {
				p.AddText(" ")
			}
			if shouldBreak {
				p.AddBreak()
			}
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
						case "circle":
							bul = "○ "
						case "square":
							bul = "■ "
						case "disc":
							bul = "• "
						case "none":
							bul = ""
						}
					}
				}
			}
			st.pushList("ul")
			if top := st.currentList(); top != nil {
				top.bullet = bul
			}
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
			if hasFormControls(c) || hasAnchorLinks(c) {
				// let recursion process interactive content to preserve links
			} else {
				// Traverse sections and rows: thead/tbody/tfoot/tr
				for sec := c.FirstChild; sec != nil; sec = sec.NextSibling {
					if sec.Type != html.ElementNode {
						continue
					}
					if strings.EqualFold(sec.Data, "tr") {
						row := make([]string, 0, 8)
						for cell := sec.FirstChild; cell != nil; cell = cell.NextSibling {
							if cell.Type == html.ElementNode && (strings.EqualFold(cell.Data, "td") || strings.EqualFold(cell.Data, "th")) {
								txt := strings.TrimSpace(collectText(cell))
								if txt != "" {
									row = append(row, txt)
								}
							}
						}
						if len(row) > 0 {
							p.AddText(strings.Join(row, " | "))
							p.AddBreak()
						}
						continue
					}
					if strings.EqualFold(sec.Data, "thead") || strings.EqualFold(sec.Data, "tbody") || strings.EqualFold(sec.Data, "tfoot") {
						for r := sec.FirstChild; r != nil; r = r.NextSibling {
							if r.Type == html.ElementNode && strings.EqualFold(r.Data, "tr") {
								row := make([]string, 0, 8)
								for cell := r.FirstChild; cell != nil; cell = cell.NextSibling {
									if cell.Type == html.ElementNode && (strings.EqualFold(cell.Data, "td") || strings.EqualFold(cell.Data, "th")) {
										txt := strings.TrimSpace(collectText(cell))
										if txt != "" {
											row = append(row, txt)
										}
									}
								}
								if len(row) > 0 {
									p.AddText(strings.Join(row, " | "))
									p.AddBreak()
								}
							}
						}
					}
				}
				recurse = false
			}
		case "details":
			// Render expanded content inline: summary processed elsewhere
			if c.FirstChild != nil {
				walkRich(c.FirstChild, base, p, visited, st, prefs)
			}
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
					if ss := strings.TrimSpace(getAttr(s, "srcset")); ss != "" {
						chosen = pickSrcFromSrcset(ss)
					}
					if chosen == "" {
						chosen = strings.TrimSpace(getAttr(s, "src"))
					}
				}
			}
			if chosen == "" {
				if img := findFirstChild(c, "img"); img != nil {
					chosen = strings.TrimSpace(getAttr(img, "src"))
				}
			}
			if chosen != "" {
				p.AddLink(resolveLink(base, chosen), "[Image]")
			}
			recurse = false
		case "form":
			action := getAttr(c, "action")
			p.AddForm(action)
			absAction := resolveFormActionURL(base, action)
			st.formStack = append(st.formStack, absAction)
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
				if len(st.formStack) > 0 {
					actionKey := st.formStack[len(st.formStack)-1]
					actionKey = strings.TrimSpace(actionKey)
					if actionKey == "" {
						actionKey = resolveFormActionURL(base, "")
					}
					if actionKey != "" {
						if p.FormHidden[actionKey] == nil {
							p.FormHidden[actionKey] = make(map[string]string)
						}
						if _, exists := p.FormHidden[actionKey][name]; !exists {
							p.FormHidden[actionKey][name] = value
						}
					}
				}
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

		if c.Type == html.TextNode {
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
						if !visited[c] {
							visited[c] = true
							txt := c.Data
							addTextWithColor(p, st, c, txt)
						}
					}
				}
			}
		}
		if recurse && c.FirstChild != nil {
			walkRich(c.FirstChild, base, p, visited, st, prefs)
		}
		if stylePushed {
			st.popStyle(p)
		}
		if colorPushed {
			st.popColor(p)
		}
		if alignedPushed {
			st.popStyle(p)
		}
		if bgColorPushed {
			st.popBgcolor(p)
		}
		if c.Type == html.ElementNode && strings.EqualFold(c.Data, "form") {
			if len(st.formStack) > 0 {
				st.formStack = st.formStack[:len(st.formStack)-1]
			}
		}
		if c.Type == html.ElementNode {
			switch strings.ToLower(c.Data) {
			case "ul", "ol", "dl", "dir", "menu":
				st.popList()
				p.AddParagraph()
			}
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

func hasAnchorLinks(n *html.Node) bool {
	var rec func(*html.Node) bool
	rec = func(x *html.Node) bool {
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode {
				if strings.EqualFold(c.Data, "a") && strings.TrimSpace(getAttr(c, "href")) != "" {
					return true
				}
				if rec(c) {
					return true
				}
			}
		}
		return false
	}
	return rec(n)
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

// isIconOnlyLink reports whether the subtree under n contains at least one image-like
// element and no visible text content. It is used to keep consecutive toolbar icons inline.
func isIconOnlyLink(n *html.Node) bool {
	if n == nil {
		return false
	}
	hasIcon := false
	var walk func(*html.Node) bool
	walk = func(node *html.Node) bool {
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			switch c.Type {
			case html.TextNode:
				text := strings.TrimSpace(c.Data)
				if text == "" {
					continue
				}
				if c.Parent != nil {
					parent := strings.ToLower(c.Parent.Data)
					if parent == "title" || parent == "desc" {
						if c.Parent.Parent != nil && strings.EqualFold(c.Parent.Parent.Data, "svg") {
							// Skip accessibility metadata commonly embedded inside <svg>.
							continue
						}
					}
				}
				return false
			case html.ElementNode:
				tag := strings.ToLower(c.Data)
				switch tag {
				case "img", "svg":
					hasIcon = true
				case "picture":
					// picture wraps <img> / <source>; recurse to find the actual image.
				case "title", "desc":
					// Skip SVG metadata; descendants already handled in text case.
					continue
				}
				if !walk(c) {
					return false
				}
			}
		}
		return true
	}
	if !walk(n) {
		return false
	}
	return hasIcon
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
			if r := dfs(c); r != nil {
				return r
			}
		}
		return nil
	}
	return dfs(n)
}

// parseCssHas returns true if inline style contains prop with a value including val substring.
func parseCssHas(style, prop, val string) bool {
	if style == "" {
		return false
	}
	s := strings.ToLower(style)
	prop = strings.ToLower(prop)
	val = strings.ToLower(val)
	parts := strings.Split(s, ";")
	for _, part := range parts {
		kv := strings.SplitN(strings.TrimSpace(part), ":", 2)
		if len(kv) != 2 {
			continue
		}
		k := strings.TrimSpace(kv[0])
		v := strings.TrimSpace(kv[1])
		if k == prop && strings.Contains(v, val) {
			return true
		}
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
type cacheCandidate struct {
	format  string
	quality int
}

func cacheCandidatesFor(prefs RenderOptions) []cacheCandidate {
	want := strings.ToLower(strings.TrimSpace(prefs.ImageMIME))
	if want == "" {
		want = "image/jpeg"
	}
	var out []cacheCandidate
	seen := make(map[string]struct{})
	add := func(format string, quality int) {
		key := format + "|" + strconv.Itoa(quality)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, cacheCandidate{format: format, quality: quality})
	}
	switch want {
	case "image/png":
		add("image/png", 0)
	default:
		add("image/jpeg", jpegQualityFor(prefs))
		add("image/png", 0)
	}
	return out
}

func jpegQualityFor(prefs RenderOptions) int {
	if prefs.HighQuality {
		return 85
	}
	return 40
}

func fetchAndEncodeImage(absURL string, prefs RenderOptions) ([]byte, int, int, bool) {
	debug := os.Getenv("OMS_IMG_DEBUG") == "1"
	candidates := cacheCandidatesFor(prefs)

	for _, cand := range candidates {
		if data, w, h, ok := imgCacheGet(cand.format, cand.quality, absURL); ok {
			if debug {
				log.Printf("IMG cache hit mem fmt=%s q=%d url=%s", cand.format, cand.quality, absURL)
			}
			return data, w, h, true
		}
		if data, w, h, ok := diskCacheGet(cand.format, cand.quality, absURL); ok {
			imgCachePut(cand.format, cand.quality, absURL, data, w, h)
			if debug {
				log.Printf("IMG cache hit disk fmt=%s q=%d url=%s", cand.format, cand.quality, absURL)
			}
			return data, w, h, true
		}
	}

	if strings.HasPrefix(absURL, "data:") {
		if data, w, h, format, quality, ok := decodeDataURI(absURL, prefs); ok {
			imgCachePut(format, quality, absURL, data, w, h)
			diskCachePut(format, quality, absURL, data, w, h)
			return data, w, h, true
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
	if prefs.ReqHeaders != nil {
		if ua := prefs.ReqHeaders.Get("User-Agent"); ua != "" {
			req.Header.Set("User-Agent", ua)
		}
		if al := prefs.ReqHeaders.Get("Accept-Language"); al != "" {
			req.Header.Set("Accept-Language", al)
		}
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
	client := &http.Client{Timeout: 8 * time.Second}
	if prefs.Jar != nil {
		client.Jar = prefs.Jar
	}
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

	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		if debug {
			log.Printf("IMG decode: %v (ct=%s)", err, resp.Header.Get("Content-Type"))
		}
		return nil, 0, 0, false
	}

	data, w, h, format, quality, err := encodeImage(img, prefs)
	if err != nil {
		if debug {
			log.Printf("IMG encode %s: %v", format, err)
		}
		return nil, 0, 0, false
	}

	imgCachePut(format, quality, absURL, data, w, h)
	diskCachePut(format, quality, absURL, data, w, h)
	return data, w, h, true
}

func clampImageToScreenWidth(img image.Image, maxWidth int) (image.Image, int, int) {
	b := img.Bounds()
	w := b.Dx()
	h := b.Dy()
	if maxWidth <= 0 || w <= 0 || h <= 0 || w <= maxWidth {
		return img, w, h
	}

	scaledH := int(math.Round(float64(h) * float64(maxWidth) / float64(w)))
	if scaledH < 1 {
		scaledH = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, maxWidth, scaledH))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Over, nil)
	return dst, maxWidth, scaledH
}

func encodeImage(img image.Image, prefs RenderOptions) ([]byte, int, int, string, int, error) {
	img, w, h := clampImageToScreenWidth(img, prefs.ScreenW)

	want := strings.ToLower(strings.TrimSpace(prefs.ImageMIME))
	if want == "" {
		want = "image/jpeg"
	}
	if want == "image/jpeg" && imageHasAlpha(img) {
		want = "image/png"
	}

	var out bytes.Buffer
	quality := 0

	switch want {
	case "image/png":
		enc := png.Encoder{CompressionLevel: png.DefaultCompression}
		if prefs.HighQuality {
			enc.CompressionLevel = png.BestCompression
		}
		if err := enc.Encode(&out, img); err != nil {
			return nil, 0, 0, want, quality, err
		}
	default:
		quality = jpegQualityFor(prefs)
		if quality <= 0 {
			quality = 60
		}
		if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: quality}); err != nil {
			return nil, 0, 0, want, quality, err
		}
	}

	return append([]byte(nil), out.Bytes()...), w, h, want, quality, nil
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

func decodeDataURI(uri string, prefs RenderOptions) ([]byte, int, int, string, int, bool) {
	// data:[<mediatype>][;base64],<data>
	comma := strings.IndexByte(uri, ',')
	if !strings.HasPrefix(uri, "data:") || comma == -1 {
		return nil, 0, 0, "", 0, false
	}
	meta := uri[len("data:"):comma]
	data := uri[comma+1:]
	var raw []byte
	if strings.Contains(meta, ";base64") {
		b, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return nil, 0, 0, "", 0, false
		}
		raw = b
	} else {
		raw = []byte(data)
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, 0, 0, "", 0, false
	}
	enc, w, h, format, quality, err := encodeImage(img, prefs)
	if err != nil {
		return nil, 0, 0, format, quality, false
	}
	return enc, w, h, format, quality, true
}

// ---------------------- Public API with options ----------------------

// LoadPageWithHeadersAndOptions performs HTTP GET with optional headers and rendering options.

func LoadPageWithHeadersAndOptions(oURL string, hdr http.Header, opts *RenderOptions) (*Page, error) {
	effectiveURL := oURL
	method := http.MethodGet
	var bodyReader io.Reader
	var contentTypeOverride string
	debugHTTP := os.Getenv("OMS_HTTP_DEBUG") == "1"

	if hdr == nil {
		hdr = http.Header{}
	}

	if opts != nil {
		debugForms := debugHTTP
		if fb := strings.TrimSpace(opts.FormBody); fb != "" && fb != "0" {
			if debugForms {
				if vals, err := url.ParseQuery(fb); err == nil {
					var parts []string
					for k, vs := range vals {
						v := ""
						if len(vs) > 0 {
							v = vs[0]
						}
						masked := v
						lk := strings.ToLower(k)
						if strings.Contains(lk, "pass") || strings.Contains(lk, "pwd") || strings.Contains(lk, "token") {
							masked = "***"
						}
						parts = append(parts, fmt.Sprintf("%s(len=%d)=%s", k, len(v), masked))
					}
					log.Printf("FORM payload keys: %s", strings.Join(parts, ", "))
				} else {
					log.Printf("FORM payload raw len=%d", len(fb))
				}
			}
		}
		if submission := prepareOperaMiniSubmission(oURL, opts.FormBody); submission != nil {
			if debugForms {
				log.Printf("SUBMISSION plan method=%s url=%s body_len=%d ct=%s", submission.Method, submission.URL, len(submission.Body), submission.ContentType)
			}
			if submission.URL != "" {
				effectiveURL = submission.URL
			}
			if submission.Method != "" {
				method = submission.Method
			}
			if submission.Body != "" {
				bodyReader = strings.NewReader(submission.Body)
			}
			if submission.ContentType != "" {
				contentTypeOverride = submission.ContentType
			}
		}
	}

	req, err := http.NewRequest(method, effectiveURL, bodyReader)
	if err != nil {
		return errorPage(effectiveURL, "Internal server error"), nil
	}
	if contentTypeOverride != "" && hdr.Get("Content-Type") == "" {
		hdr.Set("Content-Type", contentTypeOverride)
	}
	if hdr.Get("User-Agent") == "" {
		hdr.Set("User-Agent", defaultUpstreamUA)
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
	// For POST submissions, ensure Referer/Origin are present to mimic browser behavior
	if req.Method == http.MethodPost {
		if req.Header.Get("Referer") == "" {
			if u, err := url.Parse(effectiveURL); err == nil {
				ref := *u
				ref.RawQuery = ""
				req.Header.Set("Referer", ref.String())
				if debugHTTP {
					log.Printf("UPSTREAM add Referer=%s", ref.String())
				}
			}
		}
		if req.Header.Get("Origin") == "" {
			if u, err := url.Parse(effectiveURL); err == nil {
				origin := u.Scheme + "://" + u.Host
				req.Header.Set("Origin", origin)
				if debugHTTP {
					log.Printf("UPSTREAM add Origin=%s", origin)
				}
			}
		}
	}
	hc := &http.Client{Timeout: 15 * time.Second}
	if opts != nil && opts.Jar != nil {
		hc.Jar = opts.Jar
	}
	// Debug logging for upstream request and cookie jar behavior
	if debugHTTP {
		var ck string
		if c := req.Header.Get("Cookie"); c != "" {
			ck = c
		}
		xk := req.Header.Get("X-Operetta-Client-Key")
		var jarInfo string
		if hc.Jar != nil {
			u := req.URL
			if u != nil {
				if cookies := hc.Jar.Cookies(u); len(cookies) > 0 {
					names := make([]string, 0, len(cookies))
					for _, c := range cookies {
						names = append(names, c.Name)
					}
					jarInfo = "cookies=" + strings.Join(names, ",")
				} else {
					jarInfo = "cookies=0"
				}
			}
		} else {
			jarInfo = "jar=nil"
		}
		ct := req.Header.Get("Content-Type")
		log.Printf("UPSTREAM req url=%s method=%s xkey=%q cookie_hdr_len=%d jar=%s ct=%q body=%t", effectiveURL, req.Method, xk, len(ck), jarInfo, ct, bodyReader != nil)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return errorPage(effectiveURL, "Timeout loading page"), nil
	}
	defer resp.Body.Close()
	if shouldOfferDownload(resp) {
		page := renderDownloadPage(effectiveURL, resp, opts)
		return page, nil
	}
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
		return errorPage(effectiveURL, "Internal server error"), nil
	}
	// Log response status and set-cookies (after potential redirects)
	if debugHTTP {
		sc := resp.Header["Set-Cookie"]
		nsc := 0
		if sc != nil {
			nsc = len(sc)
		}
		finalURL := effectiveURL
		if resp.Request != nil && resp.Request.URL != nil {
			finalURL = resp.Request.URL.String()
		}
		log.Printf("UPSTREAM resp status=%d final=%s set-cookie=%d", resp.StatusCode, finalURL, nsc)
		if nsc > 0 {
			for i, v := range sc {
				if i >= 3 {
					break
				}
				log.Printf("UPSTREAM set-cookie[%d]=%s", i, v)
			}
		}
	}
	if looksLikeOMS(body) {
		return &Page{Data: body, SetCookies: resp.Header["Set-Cookie"]}, nil
	}
	utf8Body := decodeLegacyToUTF8(body, resp.Header.Get("Content-Type"))
	doc, err := html.Parse(bytes.NewReader(utf8Body))
	if err != nil {
		return errorPage(effectiveURL, "Internal server error while parsing"), nil
	}
	rp := defaultRenderPrefs()
	var jar http.CookieJar
	if opts != nil {
		rp = *opts
		jar = opts.Jar
	}
	p := NewPage()
	p.AddString("1/" + effectiveURL)
	if rp.AuthCode != "" {
		p.AddAuthcode(rp.AuthCode)
	}
	if rp.AuthPrefix != "" {
		p.AddAuthprefix(rp.AuthPrefix)
	}
	p.AddStyle(styleDefault)
	base := effectiveURL
	if i := strings.Index(base, "?"); i != -1 {
		base = base[:i]
	}
	base = findBaseURL(doc, base)
	rp.ReqHeaders = hdr
	rp.Referrer = effectiveURL
	rp.Styles = buildStylesheet(doc, base, hdr, jar)
	chosenCol := ""
	chosenBg := ""
	if body := findFirstByTag(doc, "body"); body != nil {
		var bgHex, fgHex string
		if rp.Styles != nil {
			if props := computeStyleFor(body, rp.Styles); props != nil {
				if v := props["background-color"]; v != "" {
					bgHex = v
				}
				if v := props["color"]; v != "" {
					fgHex = v
				}
			}
		}
		if fgHex == "" {
			if v := getAttr(body, "text"); v != "" {
				fgHex = v
			}
		}
		if bgHex == "" {
			if v := getAttr(body, "bgcolor"); v != "" {
				bgHex = v
			}
		}
		if bgHex == "" {
			if v := getAttr(body, "bgcolor"); v != "" {
				bgHex = v
			}
		}
		if stl := getAttr(body, "style"); stl != "" {
			if v := parseCssColor(stl, "background-color"); v != "" {
				bgHex = v
			}
			if v := parseCssColor(stl, "color"); v != "" {
				fgHex = v
			}
		}
		if bgHex != "" {
			p.AddBgcolor(bgHex)
		}
		chosenBg = bgHex
		chosenCol = ""
		if fgHex != "" {
			p.AddTextcolor(fgHex)
			chosenCol = fgHex
		} else if bgHex != "" && isDarkHex(bgHex) {
			p.AddTextcolor("#eeeeee")
			chosenCol = "#eeeeee"
		}
	}
	visited := map[*html.Node]bool{}
	st := walkState{curStyle: styleDefault}
	if chosenCol != "" {
		st.curColor = chosenCol
	}
	if chosenBg != "" {
		st.curBg = chosenBg
	}
	st.css = rp.Styles
	p.AddStyle(styleDefault)
	walkRich(doc, base, p, visited, &st, rp)
	if len(p.SetCookies) > 0 {
		var pairs []string
		for _, sc := range p.SetCookies {
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
	pageIdx := 1
	maxTags := 0
	if opts != nil {
		if opts.Page > 0 {
			pageIdx = opts.Page
		}
		if opts.MaxTagsPerPage > 0 {
			maxTags = opts.MaxTagsPerPage
		}
	}
	if maxTags == 0 {
		if s := os.Getenv("OMS_PAGINATE_TAGS"); s != "" {
			if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && v > 0 {
				maxTags = v
			}
		}
		if maxTags == 0 {
			maxTags = 1200
		}
	}
	if pageIdx < 1 {
		pageIdx = 1
	}
	// Pack full raw page once for cache so later selections (page>1) operate on
	// the complete document, not on the already-sliced first page with nav.
	// This fixes the issue where clicking page=2 could return page 1 from cache.
	{
		fullRaw := append([]byte(nil), p.Data...)
		packed := NewPage()
		packed.Data = fullRaw
		packed.SetTransport(rp.ClientVersion, rp.Compression)
		packed.finalize()
		p.CachePacked = append([]byte(nil), packed.Data...)
	}
	parts := splitByTags(p.Data, maxTags)
	if len(parts) == 0 {
		p.finalize()
		return p, nil
	}
	if pageIdx > len(parts) {
		pageIdx = len(parts)
	}
	sel := parts[pageIdx-1]
	// Rewrite only for pages >1 so OM2 history treats them as distinct.
	// Do NOT rewrite page 1 to avoid style regressions on return.
	if pageIdx > 1 {
		sel = rewriteInitialURLRaw(sel, pageIdx)
	}
	serverBase := ""
	if opts != nil {
		serverBase = opts.ServerBase
	}
	if len(parts) > 1 && serverBase != "" {
		nav := NewPage()
		nav.AddHr("")
		if pageIdx > 1 {
			var prevURL string
			if pageIdx-1 <= 1 {
				prevURL = effectiveURL
			} else {
				prevURL = serverBase + "/fetch?" + BuildPaginationQuery(effectiveURL, &rp, pageIdx-1, maxTags)
			}
			nav.AddLink("0/"+prevURL, "[<<]")
		} else {
			nav.AddText("[<<]")
		}
		nav.AddText(" ")
		if pageIdx < len(parts) {
			nextURL := serverBase + "/fetch?" + BuildPaginationQuery(effectiveURL, &rp, pageIdx+1, maxTags)
			nav.AddLink("0/"+nextURL, "[>>]")
		} else {
			nav.AddText("[>>]")
		}
		nav.AddBreak()
		pageSet := map[int]struct{}{}
		addCandidate := func(n int) {
			if n >= 1 && n <= len(parts) {
				pageSet[n] = struct{}{}
			}
		}
		for i := 1; i <= 3; i++ {
			addCandidate(i)
		}
		for i := pageIdx - 2; i <= pageIdx+2; i++ {
			addCandidate(i)
		}
		for i := len(parts) - 2; i <= len(parts); i++ {
			addCandidate(i)
		}
		if len(pageSet) == 0 {
			addCandidate(pageIdx)
		}
		var ordered []int
		for n := range pageSet {
			ordered = append(ordered, n)
		}
		sort.Ints(ordered)
		lastShown := 0
		for _, n := range ordered {
			if lastShown != 0 && n-lastShown > 1 {
				nav.AddText("…")
			}
			label := fmt.Sprintf("[%d]", n)
			if n == pageIdx {
				nav.AddText("•" + label)
			} else {
				pageURL := effectiveURL
				if n > 1 {
					pageURL = serverBase + "/fetch?" + BuildPaginationQuery(effectiveURL, &rp, n, maxTags)
				}
				nav.AddLink("0/"+pageURL, label)
			}
			lastShown = n
		}
		nav.AddBreak()
		/*
			////nav.AddText(fmt.Sprintf("Выбор страницы (1…%d)", len(parts)))
			////nav.AddBreak()
			////formAction := "0/" + serverBase + "/fetch"
			////nav.AddForm(formAction)
			////nav.AddHidden("url", effectiveURL)
			////if maxTags > 0 {
			////	nav.AddHidden("pp", strconv.Itoa(maxTags))
			////}
			////if rp.ImagesOn {
			////	nav.AddHidden("img", "1")
			////}
			////if rp.HighQuality {
			////	nav.AddHidden("hq", "1")
			////}
			////if rp.ImageMIME != "" {
			////	nav.AddHidden("mime", rp.ImageMIME)
			////}
			////if rp.MaxInlineKB > 0 {
			////	nav.AddHidden("maxkb", strconv.Itoa(rp.MaxInlineKB))
			////}
			////nav.AddTextInput("page", "")
			////nav.AddText(" ")
			////nav.AddSubmit("go", "OK")
			////nav.AddHr("")
		*/
		// Ensure final raw page (content + nav) does not exceed per-part byte budget.
		// Shrink the selected chunk before appending nav.
		budget := maxBytesBudget()
		allowed := budget - len(nav.Data)
		if allowed < 1024 { // keep a sane minimal room for content
			allowed = 1024
		}
		sel = shrinkPartToMaxBytes(sel, allowed)
		sel = append(sel, nav.Data...)
	}
	p.Data = sel
	p.partCur = pageIdx
	p.partCnt = len(parts)
	p.SetTransport(rp.ClientVersion, rp.Compression)
	p.finalize()
	return p, nil
}

type formSubmission struct {
	Method      string
	URL         string
	Body        string
	ContentType string
}

func prepareOperaMiniSubmission(baseURL, payload string) *formSubmission {
	payload = strings.TrimSpace(payload)
	if payload == "" || payload == "0" {
		return nil
	}
	if !strings.Contains(payload, "=") {
		return nil
	}
	values := url.Values{}
	actionOverride := ""
	method := http.MethodGet
	seenOPF := false
	hasSensitive := false
	// Heuristics: sensitive fields indicate login; absence of opf -> prefer POST
	parts := strings.Split(payload, "&")
	for _, part := range parts {
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		rawKey := kv[0]
		rawVal := ""
		if len(kv) == 2 {
			rawVal = kv[1]
		}
		key, err := url.QueryUnescape(rawKey)
		if err != nil {
			key = rawKey
		}
		val, err := url.QueryUnescape(rawVal)
		if err != nil {
			val = rawVal
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if key == "" {
			continue
		}
		switch strings.ToLower(key) {
		case "opf":
			seenOPF = true
			if val == "" || val == "0" || val == "1" {
				method = http.MethodGet
			} else {
				method = http.MethodPost
			}
			continue
		case "opa", "action":
			if val != "" {
				actionOverride = val
			}
			continue
		}
		if actionOverride == "" && looksLikeActionKey(key) {
			actionOverride = key
		}
		lk := strings.ToLower(key)
		if strings.Contains(lk, "pass") || strings.Contains(lk, "pwd") || strings.Contains(lk, "token") {
			hasSensitive = hasSensitive || (val != "")
		}
		normalizedKey := key
		if looksLikeActionKey(key) {
			if strings.HasPrefix(key, "/") && len(key) > 1 {
				normalizedKey = strings.TrimLeft(key, "/")
			} else if strings.Contains(key, "://") {
				normalizedKey = ""
			}
		}
		if normalizedKey == "" {
			continue
		}
		values.Add(normalizedKey, val)
	}
	if method == http.MethodGet && !seenOPF && hasSensitive {
		if os.Getenv("OMS_HTTP_DEBUG") == "1" {
			log.Printf("SUBMISSION heuristic: forcing POST (has sensitive fields, no opf)")
		}
		method = http.MethodPost
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		base = nil
	}
	var target *url.URL
	if actionOverride != "" {
		override := actionOverride
		if strings.HasPrefix(override, "//") && base != nil {
			override = base.Scheme + ":" + override
		}
		if u, err := url.Parse(override); err == nil {
			if base != nil && !u.IsAbs() {
				target = base.ResolveReference(u)
			} else {
				target = u
			}
		}
	}
	if target == nil && base != nil {
		clone := *base
		target = &clone
	}
	if target == nil {
		targetURL := baseURL
		if method == http.MethodGet {
			if len(values) > 0 {
				sep := "?"
				if strings.Contains(targetURL, "?") {
					sep = "&"
				}
				targetURL += sep + values.Encode()
			}
			return &formSubmission{Method: http.MethodGet, URL: targetURL}
		}
		return &formSubmission{
			Method:      http.MethodPost,
			URL:         targetURL,
			Body:        values.Encode(),
			ContentType: "application/x-www-form-urlencoded",
		}
	}
	if method == http.MethodGet {
		q := target.Query()
		for k, vs := range values {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
		target.RawQuery = q.Encode()
		return &formSubmission{Method: http.MethodGet, URL: target.String()}
	}
	return &formSubmission{
		Method:      http.MethodPost,
		URL:         target.String(),
		Body:        values.Encode(),
		ContentType: "application/x-www-form-urlencoded",
	}
}

func looksLikeActionKey(key string) bool {
	if key == "" {
		return false
	}
	if strings.HasPrefix(key, "http://") || strings.HasPrefix(key, "https://") {
		return true
	}
	if strings.HasPrefix(key, "//") {
		return true
	}
	return strings.HasPrefix(key, "/")
}

func shouldOfferDownload(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	if cd := strings.ToLower(resp.Header.Get("Content-Disposition")); cd != "" && strings.Contains(cd, "attachment") {
		return true
	}
	ct := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if ct == "" {
		return false
	}
	mediaType := strings.ToLower(ct)
	if mt, _, err := mime.ParseMediaType(ct); err == nil {
		mediaType = strings.ToLower(mt)
	}
	if mediaType == "" {
		return false
	}
	if strings.Contains(mediaType, "html") || strings.Contains(mediaType, "xml") {
		return false
	}
	if strings.HasPrefix(mediaType, "text/") {
		return false
	}
	switch mediaType {
	case "application/json", "application/javascript":
		return false
	case "application/octet-stream":
		// Normal OMS responses use application/octet-stream; only treat as download
		// when server explicitly marks it as an attachment.
		if cd := resp.Header.Get("Content-Disposition"); cd == "" {
			return false
		}
	}
	return true
}

func renderDownloadPage(effectiveURL string, resp *http.Response, opts *RenderOptions) *Page {
	page := NewPage()
	page.AddString("1/" + effectiveURL)
	page.AddStyle(styleDefault)

	ct := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if mt, _, err := mime.ParseMediaType(ct); err == nil {
		ct = mt
	}
	filename := fileNameFromResponse(resp, effectiveURL)
	sizeText := humanReadableSize(resp.ContentLength)

	page.AddText("Download file")
	page.AddBreak()
	page.AddBreak()
	if filename != "" {
		page.AddText("Name: " + filename)
		page.AddBreak()
	}
	if sizeText != "" {
		page.AddText("Size: " + sizeText)
		page.AddBreak()
	}
	if ct != "" {
		page.AddText("Type: " + ct)
		page.AddBreak()
	}
	page.AddBreak()

	downloadLink := buildDownloadLink(opts, effectiveURL, resp, filename, false)
	page.AddLink("0/"+downloadLink, "[Download]")

	if strings.HasPrefix(strings.ToLower(ct), "video/3gpp") {
		page.AddBreak()
		streamLink := buildDownloadLink(opts, effectiveURL, resp, filename, true)
		page.AddLink("0/"+streamLink, "[Play]")
		page.AddText(" Opens external player")
	}

	page.AddBreak()
	page.AddLink("0/"+effectiveURL, "[Open original]")

	if resp != nil {
		page.SetCookies = append([]string(nil), resp.Header["Set-Cookie"]...)
	}
	page.NoCache = true
	page.finalize()
	return page
}

func buildDownloadLink(opts *RenderOptions, effectiveURL string, resp *http.Response, filename string, stream bool) string {
	values := url.Values{}
	values.Set("url", effectiveURL)
	if ct := strings.TrimSpace(resp.Header.Get("Content-Type")); ct != "" {
		values.Set("ct", ct)
	}
	if filename != "" {
		values.Set("name", filename)
	}
	values.Set("ref", effectiveURL)
	if stream {
		values.Set("mode", "stream")
	}
	path := "/download?" + values.Encode()
	if opts != nil && strings.TrimSpace(opts.ServerBase) != "" {
		base := strings.TrimRight(opts.ServerBase, "/")
		return base + path
	}
	return path
}

func fileNameFromResponse(resp *http.Response, rawURL string) string {
	if resp != nil {
		if cd := resp.Header.Get("Content-Disposition"); cd != "" {
			if _, params, err := mime.ParseMediaType(cd); err == nil {
				if name := params["filename"]; name != "" {
					if decoded, err := url.QueryUnescape(name); err == nil {
						return decoded
					}
					return name
				}
			}
		}
	}
	if u, err := url.Parse(rawURL); err == nil {
		if base := path.Base(u.Path); base != "" && base != "/" {
			if decoded, err := url.PathUnescape(base); err == nil {
				return decoded
			}
			return base
		}
	}
	return ""
}

func humanReadableSize(n int64) string {
	if n <= 0 {
		return ""
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	size := float64(n)
	idx := 0
	for size >= 1024 && idx < len(units)-1 {
		size /= 1024
		idx++
	}
	if idx == 0 {
		return fmt.Sprintf("%d %s", n, units[idx])
	}
	return fmt.Sprintf("%.1f %s", size, units[idx])
}

func nextSignificantSibling(n *html.Node) *html.Node {
	for s := n.NextSibling; s != nil; s = s.NextSibling {
		if s.Type == html.TextNode {
			if strings.TrimSpace(s.Data) == "" {
				continue
			}
			return s
		}
		if s.Type == html.ElementNode {
			return s
		}
	}
	return nil
}

func hasPrefixAny(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func (s *walkState) currentStyle() uint32 {
	if len(s.styleStack) > 0 {
		return s.styleStack[len(s.styleStack)-1]
	}
	return s.curStyle
}
