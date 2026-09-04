package oms

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/binary"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"unicode"

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
	"golang.org/x/net/html/atom"

	"operetta/origin"
	"operetta/presentation"
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
const maxHeapAwareTagsPerPage = 60000
const maxInlineBackgroundSize = 128
const RGB565MIME = "image/x-rgb565"
const RGB565AlphaMIME = "image/x-rgb565a"

func isRGB565PayloadMIME(value string) bool {
	return strings.EqualFold(value, RGB565MIME) ||
		strings.EqualFold(value, RGB565AlphaMIME)
}

func effectiveTextColor(n *html.Node, st *walkState) string {
	a := n
	if a != nil && a.Type != html.ElementNode {
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

func addTextWithColor(p renderTarget, st *walkState, n *html.Node, text string) {
	if text == "" {
		return
	}

	target := cssToHex(effectiveTextColor(n, st))
	current := cssToHex(st.curColor)
	if current == "" {
		current = defaultTextColorHex
	}
	if target == "" || target == current {
		p.AddText(text)
		return
	}
	style := st.curStyle
	target = readableTextColorForOM(target, st.curBg)
	p.AddStyle(style | (uint32(calcColor(target)) << 8))
	p.AddText(text)
	if current == defaultTextColorHex {
		p.AddStyle(style)
		return
	}
	current = readableTextColorForOM(current, st.curBg)
	p.AddStyle(style | (uint32(calcColor(current)) << 8))
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

// PageHeapBudget reserves half of the client-reported heap for the MIDlet,
// parser, display tree and transient allocations. The remaining half is the
// maximum estimated retained cost of one rendered page. A zero heap means the
// client did not report a usable value and keeps the historical byte limits.
func PageHeapBudget(heapBytes int) int {
	if heapBytes <= 0 {
		return 0
	}
	budget := heapBytes / 2
	if budget < 8*1024 {
		budget = 8 * 1024
	}
	if budget > heapBytes {
		budget = heapBytes
	}
	return budget
}

// EffectivePaginationLimits converts negotiated client capabilities into the
// three independent page limits used by the splitter. When a heap is known,
// the conservative fixed 32KB wire cap is replaced by a retained-memory cap;
// this lets OMPD's large virtual heap receive normal pages without pagination
// while still protecting small Java ME heaps. Explicit pp/maxpagekb and server
// environment overrides continue to win.
func EffectivePaginationLimits(opts *RenderOptions) (maxTags, maxWireBytes, maxHeapBytes int) {
	version := ClientVersion2
	if opts != nil {
		version = normalizeClientVersion(opts.ClientVersion)
		maxTags = opts.MaxTagsPerPage
		maxWireBytes = opts.MaxBytesPerPage
		maxHeapBytes = PageHeapBudget(opts.HeapBytes)
	}

	if maxTags <= 0 {
		if raw := strings.TrimSpace(os.Getenv("OMS_PAGINATE_TAGS")); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
				maxTags = parsed
			}
		}
		if maxTags <= 0 {
			if maxHeapBytes > 0 {
				maxTags = maxHeapAwareTagsPerPage
			} else if version == ClientVersion1 {
				maxTags = 2400
			} else {
				maxTags = 1600
			}
		}
	}

	if maxWireBytes <= 0 {
		if strings.TrimSpace(os.Getenv("OMS_PAGINATE_BYTES")) != "" || maxHeapBytes == 0 {
			maxWireBytes = maxBytesBudget()
		}
	}
	return maxTags, maxWireBytes, maxHeapBytes
}

// shrinkPartToMaxBytes trims a single part (prefix + tagged body) so that its
// total raw size does not exceed limit. Trimming respects tag boundaries.
func shrinkPartToMaxBytes(part []byte, limit int, clientVersion ClientVersion) []byte {
	if limit <= 0 || len(part) <= limit || len(part) < 2 {
		return part
	}
	styleDataLen := stylePayloadSize(clientVersion)
	colorDataLen := colorPayloadSize(clientVersion)
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
	lastSafe := start
	linkDepth := 0
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
		case 'E', 'B', '+', 'V', 'Q', 'l', 'C':
			// no payload
		case 'D', 'R':
			np += colorDataLen
		case 'S':
			if np+styleDataLen > limitAll {
				np = limitAll
				break
			}
			np += styleDataLen
		case 'J':
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
		switch tag {
		case 'L':
			linkDepth++
		case 'E':
			if linkDepth > 0 {
				linkDepth--
			}
		}
		if linkDepth == 0 {
			lastSafe = p
		}
	}
	if linkDepth != 0 {
		p = lastSafe
	}
	if p <= start { // nothing fits beyond prefix
		return append([]byte{}, part[:2+l]...)
	}
	return append([]byte{}, part[:p]...)
}

func splitByTags(b []byte, maxTags int, clientVersion ClientVersion) [][]byte {
	return splitByTagsWithBudget(b, maxTags, clientVersion, maxBytesBudget())
}

func splitByTagsWithBudget(b []byte, maxTags int, clientVersion ClientVersion, maxBytes int) [][]byte {
	return splitByTagsWithBudgets(b, maxTags, clientVersion, maxBytes, 0)
}

// splitByTagsWithBudgets limits both wire bytes and estimated retained heap.
// Encoded image bytes alone are not a useful heap estimate on Java ME: after
// decoding, a highly-compressible image can occupy width*height*4 bytes while
// its JPEG/PNG payload is only a few kilobytes.
func splitByTagsWithBudgets(b []byte, maxTags int, clientVersion ClientVersion, maxBytes, maxHeapBytes int) [][]byte {
	if (maxTags <= 0 && maxBytes <= 0 && maxHeapBytes <= 0) || len(b) < 2 {
		return [][]byte{b}
	}
	styleDataLen := stylePayloadSize(clientVersion)
	colorDataLen := colorPayloadSize(clientVersion)
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
			if pp+styleDataLen > len(b) {
				pp = len(b)
				break
			}
			pp += styleDataLen
		case 'D':
			if pp+colorDataLen > len(b) {
				pp = len(b)
				break
			}
			pp += colorDataLen
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
				if pz+styleDataLen > len(src) {
					pz = len(src)
					break
				}
				data := src[pz : pz+styleDataLen]
				pz += styleDataLen
				hasColor := false
				if styleDataLen == 6 {
					// 24-bit RGB color encoded in big-endian
					if binary.BigEndian.Uint32(data[1:5])&0x00FFFFFF != 0 {
						hasColor = true
					}
				} else if binary.BigEndian.Uint16(data[1:3]) != 0 {
					hasColor = true
				}
				if hasColor {
					buf := make([]byte, 1+styleDataLen)
					buf[0] = 'S'
					copy(buf[1:], data)
					sColor = buf
				}
			case 'D':
				if pz+colorDataLen > len(src) {
					pz = len(src)
					break
				}
				dTag = append([]byte(nil), src[start:pz+colorDataLen]...)
				haveD = true
				pz += colorDataLen
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
	linkDepth := 0
	limit := len(b)
	parts := make([][]byte, 0, 2)
	partIdx := 0
	partBaseHeap := 2048 + len(prefix)*2 + len(prelude)*2
	chunkHeap := partBaseHeap
	atomStart := start
	atomTagsBefore := 0
	atomHeapBefore := chunkHeap
	for p < limit {
		if linkDepth == 0 {
			atomStart = p
			atomTagsBefore = tags
			atomHeapBefore = chunkHeap
		}
		tag := b[p]
		p++
		tagHeap := 64 // display-list node/object bookkeeping
		switch tag {
		case 'T', 'L':
			if p+2 > limit {
				p = limit
				break
			}
			l := int(binary.BigEndian.Uint16(b[p : p+2]))
			p += 2 + l
			tagHeap += l * 2 // Java strings are retained as UTF-16
		case 'E', 'B', '+', 'V', 'Q', 'l', 'C':
			// no payload
		case 'D', 'R':
			p += colorDataLen
		case 'S':
			if p+styleDataLen > limit {
				p = limit
				break
			}
			p += styleDataLen
		case 'J':
			if p+4 > limit {
				p = limit
				break
			}
			p += 4
		case 'I':
			if p+8 > limit {
				p = limit
				break
			}
			width := int(binary.BigEndian.Uint16(b[p : p+2]))
			height := int(binary.BigEndian.Uint16(b[p+2 : p+4]))
			dl := int(binary.BigEndian.Uint16(b[p+4 : p+6]))
			p += 8 + dl
			// Count both the encoded byte[] and a conservative ARGB decode.
			// Some 16-bit phones use RGB565 internally, but Java ME
			// implementations are allowed to retain a 32-bit surface.
			tagHeap += dl
			pixels := int64(width) * int64(height) * 4
			if pixels > int64(^uint(0)>>1)-int64(tagHeap) {
				tagHeap = int(^uint(0) >> 1)
			} else {
				tagHeap += int(pixels)
			}
		case 'k':
			p += 1
			if p+2 > limit {
				p = limit
				break
			}
			l := int(binary.BigEndian.Uint16(b[p : p+2]))
			p += 2 + l
			tagHeap += l * 2
		case 'h':
			for i := 0; i < 2; i++ {
				if p+2 > limit {
					p = limit
					break
				}
				l := int(binary.BigEndian.Uint16(b[p : p+2]))
				p += 2 + l
				tagHeap += l * 2
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
				tagHeap += l * 2
			}
		case 'p', 'u', 'i', 'b', 'e':
			for i := 0; i < 2; i++ {
				if p+2 > limit {
					p = limit
					break
				}
				l := int(binary.BigEndian.Uint16(b[p : p+2]))
				p += 2 + l
				tagHeap += l * 2
			}
		case 'c', 'r':
			for i := 0; i < 2; i++ {
				if p+2 > limit {
					p = limit
					break
				}
				l := int(binary.BigEndian.Uint16(b[p : p+2]))
				p += 2 + l
				tagHeap += l * 2
			}
			p += 1
		case 's':
			if p+2 > limit {
				p = limit
				break
			}
			l := int(binary.BigEndian.Uint16(b[p : p+2]))
			p += 2 + l
			tagHeap += l * 2
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
				tagHeap += l * 2
			}
			p += 1
		default:
			// Unknown: stop further splitting
			p = limit
		}
		tags++
		if chunkHeap > int(^uint(0)>>1)-tagHeap {
			chunkHeap = int(^uint(0) >> 1)
		} else {
			chunkHeap += tagHeap
		}
		switch tag {
		case 'L':
			linkDepth++
		case 'E':
			if linkDepth > 0 {
				linkDepth--
			}
		}
		chunkBytes := p - start
		heapExceeded := maxHeapBytes > 0 && chunkHeap >= maxHeapBytes
		if linkDepth == 0 && ((maxTags > 0 && tags >= maxTags) ||
			(maxBytes > 0 && chunkBytes >= maxBytes) || heapExceeded) {
			cutAt := p
			remainingTags := 0
			remainingHeap := partBaseHeap
			// A decoded image can be much larger than its wire tag. When the
			// newest atomic item (a tag or complete link) crosses the heap cap,
			// place it on the next part instead of letting one extra image make
			// the previous part exceed the negotiated heap substantially.
			if heapExceeded && atomStart > start && atomTagsBefore > 0 {
				cutAt = atomStart
				remainingTags = tags - atomTagsBefore
				remainingHeap = partBaseHeap + (chunkHeap - atomHeapBefore)
			}
			chunk := append([]byte(nil), b[start:cutAt]...)
			part := append([]byte(nil), prefix...)
			if partIdx > 0 && len(prelude) > 0 {
				part = append(part, prelude...)
			}
			part = append(part, chunk...)
			parts = append(parts, part)
			start = cutAt
			tags = remainingTags
			chunkHeap = remainingHeap
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
	if globalImgCache == nil || url == "" {
		return nil, 0, 0, false
	}
	return globalImgCache.get(imgCacheKey(format, quality, url))
}

func imgCachePut(format string, quality int, url string, data []byte, w, h int) {
	if globalImgCache == nil || url == "" {
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

func encodeErrorPage(url, reason string, opts *RenderOptions) (*Page, error) {
	target := newContentTarget("internal:error")
	target.AddStyle(styleDefault)
	target.AddPlus()
	target.AddText("Internal server error")
	target.AddText(reason)
	target.AddBreak()
	target.AddText(url)
	model := target.Document()
	model.NoCache = true
	return EncodeDocument(model, opts)
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
	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return errorPage(oURL, "Internal server error"), nil
	}
	transferBytes := len(rawBody)
	body := rawBody
	if encoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))); encoding != "" {
		switch encoding {
		case "gzip":
			if gr, gerr := gzip.NewReader(bytes.NewReader(rawBody)); gerr == nil {
				if decoded, derr := io.ReadAll(gr); derr == nil {
					body = decoded
				}
				_ = gr.Close()
			}
		case "deflate":
			if zr, zerr := zlib.NewReader(bytes.NewReader(rawBody)); zerr == nil {
				if decoded, derr := io.ReadAll(zr); derr == nil {
					body = decoded
				}
				_ = zr.Close()
			} else if fr := flate.NewReader(bytes.NewReader(rawBody)); fr != nil {
				if decoded, derr := io.ReadAll(fr); derr == nil {
					body = decoded
				}
				_ = fr.Close()
			}
		}
	}
	if len(body) == 0 {
		body = rawBody
	}
	// Pass-through if the response already looks like an OMS payload
	if looksLikeOMS(body) {
		return &Page{
			Data: body,
			Stats: TrafficStats{
				OriginTransferBytes: transferBytes,
				OriginDecodedBytes:  len(body),
				EncodedBytes:        len(body),
			},
		}, nil
	}
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return errorPage(oURL, "Internal server error while parsing"), nil
	}
	p := NewPage()
	p.Stats.OriginTransferBytes = transferBytes
	p.Stats.OriginDecodedBytes = len(body)
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
	versionByte := byte(headerWord & 0xFF)
	compression := compressionFromHeaderByte(byte(headerWord >> 8))
	dec, err := decompressPayload(compression, b[6:])
	if err != nil || len(dec) == 0 {
		return false
	}
	// Validate version byte: must be a known OMS version marker
	switch versionByte {
	case 0x0d, 0x18, 0x1a:
		// valid version markers
	default:
		return false
	}
	// Ensure minimum header length for the detected version
	minHeaderLen := 35
	if versionByte == 0x0d {
		minHeaderLen = 33
	}
	if len(dec) < minHeaderLen {
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

func boolAttr(n *html.Node, name string) bool {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, name) {
			val := strings.TrimSpace(strings.ToLower(a.Val))
			if val == "" {
				return true
			}
			switch val {
			case "0", "false", "no", "off":
				return false
			default:
				return true
			}
		}
	}
	return false
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

func resolveNavigableLink(base, href string) (string, bool) {
	href = strings.TrimSpace(href)
	if href == "" {
		return "", false
	}
	lower := strings.ToLower(href)
	switch {
	case lower == "#":
		return "", false
	case strings.HasPrefix(lower, "javascript:"):
		if strings.TrimPrefix(lower, "javascript:") == "" || strings.Contains(lower, "void") {
			return "", false
		}
	case strings.HasPrefix(lower, "mailto:"), strings.HasPrefix(lower, "tel:"):
		return "", false
	}
	link := resolveLink(base, href)
	if link == "" || link == "error:link" || link == "0/error:link" {
		return "", false
	}
	return link, true
}

func renderImageFromURL(p renderTarget, st *walkState, base, src, alt string, prefs RenderOptions) {
	src = strings.TrimSpace(src)
	alt = strings.TrimSpace(alt)
	if alt == "" {
		alt = "Image"
	}
	if !prefs.ImagesOn || src == "" {
		p.AddText("[" + alt + "]")
		return
	}
	abs := resolveLink(base, src)
	if ib, w, h, ok := fetchAndEncodeImage(abs[2:], prefs); ok {
		if len(ib) <= prefs.MaxInlineKB*1024 {
			p.AddImageInline(w, h, ib)
		} else {
			if st != nil && st.inLink {
				p.AddImagePlaceholder(w, h)
			} else {
				p.BeginLink(abs)
				p.AddImagePlaceholder(w, h)
				p.EndLink()
			}
		}
		return
	}
	if st != nil && st.inLink {
		p.AddImagePlaceholder(0, 0)
		return
	}
	p.BeginLink(abs)
	p.AddImagePlaceholder(0, 0)
	p.EndLink()
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
	n := len(s)
	i := 0
	for i < n {
		// Skip delimiters and whitespace
		for i < n && (s[i] == ',' || s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
			i++
		}
		if i >= n {
			break
		}
		start := i
		sawSpace := false
		for i < n {
			ch := s[i]
			if ch == '\'' || ch == '"' {
				quote := ch
				i++
				for i < n && s[i] != quote {
					i++
				}
				if i < n {
					i++
				}
				continue
			}
			if ch == ',' {
				if sawSpace {
					break
				}
				i++
				continue
			}
			if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' {
				sawSpace = true
				break
			}
			i++
		}
		url := strings.Trim(s[start:i], " \t\r\n\"'")
		if url != "" {
			return url
		}
		for i < n && s[i] != ',' {
			i++
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
	linkColor  string
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

func ensureHiddenFieldOverrides(actionKey string, fields map[string]string) {
	if len(fields) == 0 || strings.TrimSpace(actionKey) == "" {
		return
	}
	u, err := url.Parse(actionKey)
	if err != nil {
		return
	}
	host := strings.ToLower(u.Host)
	if host == "" || !strings.Contains(host, "google.") {
		return
	}
	path := strings.Trim(strings.ToLower(u.Path), "/")
	if path != "search" {
		return
	}
	if _, exists := fields["udm"]; !exists {
		fields["udm"] = "2"
	}
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
	// MaxBytesPerPage is negotiated by clients independently from image size.
	// Zero retains the legacy OMS_PAGINATE_BYTES/default budget.
	MaxBytesPerPage int
	// Optional absolute base (scheme://host) for building navigation links
	ServerBase    string
	Styles        *Stylesheet
	WantFullCache bool
	ClientVersion ClientVersion
	DialectID     string
	// LegacyBasicOM2 enables layout workarounds for the original Opera Mini
	// 2.0 Basic renderer. Unlike later clients, it forces images larger than
	// 38 pixels in either dimension onto their own display-list row.
	LegacyBasicOM2 bool
	// CachePartition isolates temporary packed-page artifacts by gateway
	// session. It is not sent to origins or encoded into the document.
	CachePartition string
	// Optional JavaScript baking configuration (nil = auto/off).
	JS *JSBakingOptions
}

// JSExecutionMode controls whether JS baking should be applied.
type JSExecutionMode int

const (
	JSExecutionModeAuto JSExecutionMode = iota
	JSExecutionModeDisabled
	JSExecutionModeEnabled
	JSExecutionModeRequired
)

// JSBakingOptions capture additional tuning knobs for JS-enabled fetching.
type JSBakingOptions struct {
	Mode              JSExecutionMode
	WaitAfterLoadMS   int
	WaitNetworkIdleMS int
	WaitDOMIdleMS     int
	MaxSettleMS       int
	WaitSelector      string
	TimeoutMS         int
	RasterizeEmoji    bool
	Scripts           []string
}

// UpstreamDocument is kept as a source-compatible alias. New code should use
// origin.Response so acquisition does not depend on the Opera Mini codec.
type UpstreamDocument = origin.Response

func cloneHeader(h http.Header) http.Header {
	if h == nil {
		return http.Header{}
	}
	out := http.Header{}
	for k, vs := range h {
		for _, v := range vs {
			out.Add(k, v)
		}
	}
	return out
}

func shouldOfferDownload(header http.Header) bool {
	if header == nil {
		return false
	}
	if cd := strings.ToLower(header.Get("Content-Disposition")); cd != "" && strings.Contains(cd, "attachment") {
		return true
	}
	ct := strings.TrimSpace(header.Get("Content-Type"))
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
		if cd := header.Get("Content-Disposition"); cd == "" {
			return false
		}
	}
	return true
}

func renderDownloadPageFromDocument(doc *UpstreamDocument, opts *RenderOptions) *Page {
	effectiveURL := doc.URL
	if effectiveURL == "" {
		effectiveURL = "about:blank"
	}
	page := NewPage()
	page.AddString("1/" + effectiveURL)
	page.AddStyle(styleDefault)

	ct := strings.TrimSpace(doc.Header.Get("Content-Type"))
	if mt, _, err := mime.ParseMediaType(ct); err == nil {
		ct = mt
	}
	filename := fileNameFromDocument(doc)
	sizeText := humanReadableSize(doc.ContentLength)

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

	downloadLink := buildDownloadLinkFromDocument(doc, opts, filename, false)
	page.AddLink("0/"+downloadLink, "[Download]")

	if strings.HasPrefix(strings.ToLower(ct), "video/3gpp") {
		page.AddBreak()
		streamLink := buildDownloadLinkFromDocument(doc, opts, filename, true)
		page.AddLink("0/"+streamLink, "[Play]")
		page.AddText(" Opens external player")
	}

	page.AddBreak()
	page.AddLink("0/"+effectiveURL, "[Open original]")

	if len(doc.SetCookies) > 0 {
		page.SetCookies = append([]string(nil), doc.SetCookies...)
	}
	page.NoCache = true
	page.finalize()
	return page
}

func transformDownloadDocument(doc *UpstreamDocument, opts *RenderOptions) *presentation.Document {
	effectiveURL := doc.URL
	if effectiveURL == "" {
		effectiveURL = "about:blank"
	}
	target := newContentTarget(effectiveURL)
	target.AddStyle(styleDefault)

	ct := strings.TrimSpace(doc.Header.Get("Content-Type"))
	if mt, _, err := mime.ParseMediaType(ct); err == nil {
		ct = mt
	}
	filename := fileNameFromDocument(doc)
	sizeText := humanReadableSize(doc.ContentLength)

	target.AddText("Download file")
	target.AddBreak()
	target.AddBreak()
	if filename != "" {
		target.AddText("Name: " + filename)
		target.AddBreak()
	}
	if sizeText != "" {
		target.AddText("Size: " + sizeText)
		target.AddBreak()
	}
	if ct != "" {
		target.AddText("Type: " + ct)
		target.AddBreak()
	}
	target.AddBreak()
	target.AddLink("0/"+buildDownloadLinkFromDocument(doc, opts, filename, false), "[Download]")
	if strings.HasPrefix(strings.ToLower(ct), "video/3gpp") {
		target.AddBreak()
		target.AddLink("0/"+buildDownloadLinkFromDocument(doc, opts, filename, true), "[Play]")
		target.AddText(" Opens external player")
	}
	target.AddBreak()
	target.AddLink("0/"+effectiveURL, "[Open original]")

	model := target.Document()
	model.SetCookies = append([]string(nil), doc.SetCookies...)
	model.Metrics.OriginTransferBytes = doc.TransferBytes
	model.Metrics.OriginDecodedBytes = len(doc.Body)
	model.NoCache = true
	model.DownloadOnly = true
	return model
}

func buildDownloadLinkFromDocument(doc *UpstreamDocument, opts *RenderOptions, filename string, stream bool) string {
	values := url.Values{}
	effectiveURL := doc.URL
	if effectiveURL == "" {
		effectiveURL = "about:blank"
	}
	values.Set("url", effectiveURL)
	if ct := strings.TrimSpace(doc.Header.Get("Content-Type")); ct != "" {
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

func fileNameFromDocument(doc *UpstreamDocument) string {
	if doc == nil {
		return ""
	}
	if cd := doc.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil {
			if name := params["filename"]; name != "" {
				if decoded, err := url.QueryUnescape(name); err == nil {
					return decoded
				}
				return name
			}
		}
	}
	if u, err := url.Parse(doc.URL); err == nil {
		if base := path.Base(u.Path); base != "" && base != "/" {
			if decoded, err := url.PathUnescape(base); err == nil {
				return decoded
			}
			return base
		}
	}
	return ""
}

// BuildPaginationLink returns a target URL with internal pagination options.
// Use a query parameter instead of a fragment because browser extension viewers
// wrap these links in their own query strings, where raw fragments are fragile.
func BuildPaginationLink(target string, opts *RenderOptions, page, maxTags int) string {
	u, err := url.Parse(target)
	if err != nil {
		if page <= 1 {
			return target
		}
		u = &url.URL{Path: target}
	}
	q := u.Query()
	q.Del("__p")
	q.Del("__om")
	u.RawQuery = q.Encode()
	u.Fragment = ""
	if page <= 1 {
		return u.String()
	}
	om := url.Values{}
	om.Set("page", strconv.Itoa(page))
	if maxTags > 0 {
		om.Set("pp", strconv.Itoa(maxTags))
	}
	if opts != nil {
		if opts.ImagesOn {
			om.Set("img", "1")
		} else {
			om.Set("img", "2")
		}
		if opts.HighQuality {
			om.Set("hq", "1")
		}
		if opts.ImageMIME != "" {
			om.Set("mime", opts.ImageMIME)
		}
		if opts.MaxInlineKB > 0 {
			om.Set("maxkb", strconv.Itoa(opts.MaxInlineKB))
		}
		if opts.MaxBytesPerPage > 0 {
			om.Set("maxpagekb", strconv.Itoa((opts.MaxBytesPerPage+1023)/1024))
		}
		if opts.ScreenW > 0 {
			om.Set("w", strconv.Itoa(opts.ScreenW))
		}
		if opts.ScreenH > 0 {
			om.Set("h", strconv.Itoa(opts.ScreenH))
		}
		if opts.HeapBytes > 0 {
			om.Set("m", strconv.Itoa(opts.HeapBytes))
		}
		if opts.AlphaLevels > 0 {
			om.Set("l", strconv.Itoa(opts.AlphaLevels))
		}
		if opts.NumColors > 0 {
			om.Set("c", strconv.Itoa(opts.NumColors))
		}
	}
	encoded := om.Encode()
	if encoded == "" {
		return u.String()
	}
	q = u.Query()
	q.Set("__om", encoded)
	u.RawQuery = q.Encode()
	return u.String()
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
		ImagesOn:      true,
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

func (s *walkState) styleWithTextColor(style uint32, color string) uint32 {
	if color == "" {
		return style
	}
	safe := readableTextColorForOM(color, s.curBg)
	if safe == "" {
		return style
	}
	return style | (uint32(calcColor(safe)) << 8)
}

func (s *walkState) emitCurrentStyle(p renderTarget) {
	style := s.styleWithTextColor(s.curStyle, s.curColor)
	if exact, ok := p.(interface{ AddExactStyle(uint32, string) }); ok {
		exact.AddExactStyle(style, readableTextColorForOM(s.curColor, s.curBg))
		return
	}
	p.AddStyle(style)
}

func (s *walkState) pushStyle(p renderTarget, style uint32) {
	s.styleStack = append(s.styleStack, s.curStyle)
	s.curStyle = style
	s.emitCurrentStyle(p)
}

func (s *walkState) popStyle(p renderTarget) {
	if len(s.styleStack) == 0 {
		return
	}
	s.curStyle = s.styleStack[len(s.styleStack)-1]
	s.styleStack = s.styleStack[:len(s.styleStack)-1]
	s.emitCurrentStyle(p)
}

func (s *walkState) pushColor(p renderTarget, hex string) {
	if normalized := cssToHex(hex); normalized != "" {
		hex = normalized
	} else {
		hex = strings.TrimSpace(hex)
	}
	s.colorStack = append(s.colorStack, s.curColor)
	s.curColor = hex
	if hex == "" {
		return
	}
	s.emitCurrentStyle(p)
}

func (s *walkState) popColor(p renderTarget) {
	if len(s.colorStack) == 0 {
		return
	}
	prev := s.colorStack[len(s.colorStack)-1]
	s.colorStack = s.colorStack[:len(s.colorStack)-1]
	if normalized := cssToHex(prev); normalized != "" {
		prev = normalized
	} else {
		prev = ""
	}
	s.curColor = prev
	if prev == "" {
		p.AddStyle(s.curStyle)
		return
	}
	s.emitCurrentStyle(p)
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

func hasAncestorClass(n *html.Node, class string) bool {
	class = strings.ToLower(strings.TrimSpace(class))
	if class == "" {
		return false
	}
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type != html.ElementNode {
			continue
		}
		if hasClass(p, class) {
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

func renderBackgroundImage(n *html.Node, props map[string]string, base string, p renderTarget, prefs RenderOptions) bool {
	if n == nil || p == nil {
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
	if !prefs.ImagesOn {
		if hasTextContent(n) {
			return false
		}
		p.AddText("*")
		return true
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
		if cropped, croppedW, croppedH, ok := fetchAndEncodeImageRegion(abs, prefs, cropX, cropY, widthHint, heightHint); ok {
			if prefs.MaxInlineKB <= 0 || len(cropped) <= prefs.MaxInlineKB*1024 {
				p.AddImageInline(croppedW, croppedH, cropped)
				return true
			}
		}
		// If cropping fails, fall back to full image rendering.
	}
	stride := 2
	if strings.EqualFold(prefs.ImageMIME, RGB565AlphaMIME) {
		stride = 3
	}
	if isRGB565PayloadMIME(prefs.ImageMIME) && len(data) != widthHint*heightHint*stride {
		p.AddImageInline(w, h, data)
	} else {
		p.AddImageInline(widthHint, heightHint, data)
	}
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

func isInlineCSSBox(n *html.Node, props map[string]string) bool {
	if n == nil || n.Type != html.ElementNode {
		return false
	}
	display := strings.ToLower(cssPropValue(props, getAttr(n, "style"), "display"))
	if strings.Contains(display, "inline") {
		return true
	}
	switch strings.ToLower(n.Data) {
	case "a", "span", "b", "strong", "i", "em", "small", "font", "sup", "sub":
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
	case "color", "text-align", "font-weight", "font-style", "text-decoration", "list-style-type", "text-transform":
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

func (s *walkState) pushBgcolor(p renderTarget, hex string) {
	s.bgStack = append(s.bgStack, s.curBg)
	s.curBg = hex
	if hex != "" {
		p.AddBgcolor(hex)
		s.emitCurrentStyle(p)
	}
}

func (s *walkState) popBgcolor(p renderTarget) {
	if len(s.bgStack) == 0 {
		return
	}
	prev := s.bgStack[len(s.bgStack)-1]
	s.bgStack = s.bgStack[:len(s.bgStack)-1]
	s.curBg = prev
	if prev != "" {
		p.AddBgcolor(prev)
	} else {
		p.AddBgcolor("#ffffff")
	}
	s.emitCurrentStyle(p)
}

func resetComputedStyles(st *walkState, p renderTarget, colorPushed *bool, stylePushed *bool, alignedPushed *bool) {
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
	if s == "" {
		return ""
	}
	// Remember whether original text ended with whitespace so we can restore
	// expected spacing after punctuation (e.g. ", " sequences commonly used
	// between inline links on legacy portals).
	hadTrailingWhitespace := false
	for i := len(s) - 1; i >= 0; i-- {
		c := s[i]
		if c == ' ' || c == '\n' || c == '\r' || c == '\t' {
			hadTrailingWhitespace = true
		} else {
			break
		}
	}
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return ""
	}
	if !strings.Contains(trimmed, "  ") && !strings.Contains(trimmed, "\t") {
		if hadTrailingWhitespace && trimmed != "" {
			last := trimmed[len(trimmed)-1]
			if last == ',' || last == ';' || last == ':' {
				return trimmed + " "
			}
		}
		return trimmed
	}
	var b strings.Builder
	prevSpace := false
	for i := 0; i < len(trimmed); i++ {
		c := trimmed[i]
		if c == ' ' || c == '\t' {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		prevSpace = false
		b.WriteByte(c)
	}
	out := b.String()
	if hadTrailingWhitespace && out != "" {
		last := out[len(out)-1]
		if last == ',' || last == ';' || last == ':' {
			out += " "
		}
	}
	return out
}

func collapseHTMLWhitespace(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	prevSpace := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	return b.String()
}

func textNodeContent(n *html.Node, st *walkState) string {
	if n == nil {
		return ""
	}
	if st != nil && st.pre {
		return n.Data
	}
	collapsed := collapseHTMLWhitespace(n.Data)
	trimmed := strings.TrimSpace(collapsed)
	if trimmed == "" {
		if shouldPreserveInlineWhitespace(n) {
			return " "
		}
		return ""
	}
	if strings.HasPrefix(collapsed, " ") && isInlineContentBoundary(previousSignificantSibling(n)) {
		trimmed = " " + trimmed
	}
	if strings.HasSuffix(collapsed, " ") && isInlineContentBoundary(nextSignificantSibling(n)) {
		trimmed += " "
	}
	return applyNodeTextTransform(n.Parent, st, trimmed)
}

func applyNodeTextTransform(n *html.Node, st *walkState, text string) string {
	if n == nil || st == nil || st.css == nil || text == "" {
		return text
	}
	props := computeStyleFor(n, st.css)
	switch strings.ToLower(strings.TrimSpace(cssEffectiveProp(n, st.css, props, "text-transform"))) {
	case "uppercase":
		return strings.ToUpper(text)
	case "lowercase":
		return strings.ToLower(text)
	case "capitalize":
		var out strings.Builder
		newWord := true
		for _, r := range text {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				if newWord {
					r = unicode.ToUpper(r)
				}
				newWord = false
			} else {
				newWord = true
			}
			out.WriteRune(r)
		}
		return out.String()
	default:
		return text
	}
}

// legacyBasicSafeText removes Unicode shaping records which the original
// CLDC/MIDP Opera Mini 2 text decoder cannot represent. In particular, an
// emoji followed by VS16 can make that client discard the complete OMS T
// record, hiding otherwise ordinary text after it. Later clients keep the
// original Unicode untouched.
func legacyBasicSafeText(text string) string {
	var out strings.Builder
	out.Grow(len(text))
	lastReplacement := false
	for _, r := range text {
		switch {
		case r == '\u200d', // zero-width joiner
			r == '\ufe0e' || r == '\ufe0f', // text/emoji variation selectors
			r >= 0x1f3fb && r <= 0x1f3ff:   // emoji skin-tone modifiers
			continue
		case r > 0xffff:
			if !lastReplacement {
				out.WriteByte('*')
				lastReplacement = true
			}
			continue
		default:
			out.WriteRune(r)
			lastReplacement = false
		}
	}
	return out.String()
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
func fetchAndEncodeImageRegion(absURL string, prefs RenderOptions, x, y, w, h int) ([]byte, int, int, bool) {
	if w <= 0 || h <= 0 {
		return nil, 0, 0, false
	}
	// Region cache key
	fullKey := imageCacheVariantKey(absURL, prefs)
	regionKey := ""
	if fullKey != "" {
		regionKey = fullKey + "#rect=" + strconv.Itoa(x) + "," + strconv.Itoa(y) + "," + strconv.Itoa(w) + "," + strconv.Itoa(h)
	}
	candidates := cacheCandidatesFor(prefs)
	for _, cand := range candidates {
		if data, cachedW, cachedH, ok := imgCacheGet(cand.format, cand.quality, regionKey); ok {
			return data, cachedW, cachedH, true
		}
		if data, cachedW, cachedH, ok := diskCacheGet(cand.format, cand.quality, regionKey); ok {
			imgCachePut(cand.format, cand.quality, regionKey, data, cachedW, cachedH)
			return data, cachedW, cachedH, true
		}
	}

	// Attempt to reuse cached full image first
	var srcBytes []byte
	var have bool
	if !isRGB565PayloadMIME(prefs.ImageMIME) {
		for _, cand := range candidates {
			if data, _, _, ok := imgCacheGet(cand.format, cand.quality, fullKey); ok {
				srcBytes = data
				have = true
				break
			}
			if data, _, _, ok := diskCacheGet(cand.format, cand.quality, fullKey); ok {
				srcBytes = data
				have = true
				break
			}
		}
	}
	if !have {
		// Fallback to fetching from network
		req, err := http.NewRequest(http.MethodGet, absURL, nil)
		if err != nil {
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
		b, err := io.ReadAll(rc)
		if err != nil || len(b) == 0 {
			return nil, 0, 0, false
		}
		srcBytes = b
	}

	// Decode and crop
	img, _, err := image.Decode(bytes.NewReader(srcBytes))
	if err != nil {
		return nil, 0, 0, false
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
		return nil, 0, 0, false
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

	data, encodedW, encodedH, format, quality, err := encodeImage(region, prefs)
	if err != nil {
		return nil, 0, 0, false
	}
	imgCachePut(format, quality, regionKey, data, encodedW, encodedH)
	diskCachePut(format, quality, regionKey, data, encodedW, encodedH)
	return data, encodedW, encodedH, true
}

func walkRich(cur *html.Node, base string, p renderTarget, visited map[*html.Node]bool, st *walkState, prefs RenderOptions) {
	for c := cur; c != nil; c = c.NextSibling {
		recurse := true
		var colorPushed bool
		var stylePushed bool
		var alignedPushed bool
		var bgColorPushed bool
		var bgBoundaryOnFinish bool
		var bgRendered bool
		var props map[string]string
		finishCurrent := func() {
			if stylePushed {
				st.popStyle(p)
				stylePushed = false
			}
			if colorPushed {
				st.popColor(p)
				colorPushed = false
			}
			if alignedPushed {
				st.popStyle(p)
				alignedPushed = false
			}
			if bgColorPushed {
				st.popBgcolor(p)
				bgColorPushed = false
			}
		}
		if c.Type == html.ElementNode {
			// Skip hidden elements
			if stAttr := getAttr(c, "style"); stAttr != "" && isDisplayNone(stAttr) {
				continue
			}
			// Apply computed CSS: honor display:none, text-align and color (from stylesheet)
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
				// Color inheritance is already represented by walkState while the
				// ancestor subtree is active. Applying an ancestor CSS value again on
				// every child would override the browser's legacy link color.
				if col := strings.TrimSpace(props["color"]); col != "" {
					st.pushColor(p, col)
					colorPushed = true
				}
			}
		}
		tag := strings.ToLower(c.Data)
		if bgRendered && isInlineCSSBox(c, props) {
			finishCurrent()
			continue
		}
		if handler, ok := extraHTML4Handlers[tag]; ok {
			ctx := elementContext{node: c, base: base, page: p, visited: visited, state: st, prefs: prefs}
			if handler(&ctx) {
				finishCurrent()
				continue
			}
		}
		switch tag {
		case "html", "head":
		case "title":
			if t := findTextNode(c, visited); t != nil {
				visited[t] = true
			}
		case "body":
			if l := getAttr(c, "bgcolor"); l != "" {
				p.AddBgcolor(l)
				if hx := cssToHex(l); hx != "" {
					st.curBg = hx
					st.emitCurrentStyle(p)
				}
			}
			if l := cssToHex(getAttr(c, "text")); l != "" {
				st.curColor = l
				st.emitCurrentStyle(p)
			}
			if stl := getAttr(c, "style"); stl != "" {
				if col := parseCssColor(stl, "background-color"); col != "" {
					p.AddBgcolor(col)
					st.curBg = col
					st.emitCurrentStyle(p)
				}
				if col := parseCssColor(stl, "color"); col != "" {
					st.curColor = col
					st.emitCurrentStyle(p)
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
				txt = applyNodeTextTransform(c, st, txt)
				if st.curBg == "" {
					p.AddPlus()
				}
				// Keep the HTML heading default, but do not override an author CSS
				// font-weight (including inherit/normal) with synthetic bold.
				if strings.TrimSpace(props["font-weight"]) == "" {
					st.pushStyle(p, st.curStyle|styleBoldBit)
					p.AddText(txt)
					st.popStyle(p)
				} else {
					p.AddText(txt)
				}
				p.AddBreak()
			}
			// Do not recurse into heading children to avoid duplicate text
			recurse = false
		case "div", "section", "article", "header", "footer", "main", "nav", "aside":
			if renderSectionTitle(c, base, p, st, prefs) {
				recurse = false
				finishCurrent()
				continue
			}
			if renderFloatMetadataRow(c, base, p, visited, st, prefs) {
				recurse = false
				finishCurrent()
				continue
			}
			if renderLeadingRightMetadata(c, base, p, visited, st, prefs) {
				recurse = false
				finishCurrent()
				continue
			}
			if renderInlineImageStrip(c, base, p, st, prefs) {
				recurse = false
				finishCurrent()
				continue
			}
			if renderFloatIconGrid(c, base, p, st, prefs) {
				recurse = false
				finishCurrent()
				continue
			}
			if renderMediaObject(c, base, p, visited, st, prefs) {
				recurse = false
				finishCurrent()
				continue
			}
			if hasClass(c, "p") {
				resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
				st.pushColor(p, "#007700")
				if !bgColorPushed {
					p.AddPlus()
				}
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				st.popColor(p)
				p.AddBreak()
				finishCurrent()
				continue
			}
			if hasAnyClass(c, "ts", "tsb", "tso") {
				resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				p.AddBreak()
				finishCurrent()
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
				finishCurrent()
				continue
			}
			if hasClass(c, "nw") {
				resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				finishCurrent()
				continue
			}
			if hasClass(c, "bro") {
				resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				p.AddBreak()
				finishCurrent()
				continue
			}
			if hasClass(c, "copy") {
				resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
				st.pushStyle(p, st.curStyle|styleBoldBit)
				st.pushColor(p, "#ffffff")
				if !bgColorPushed {
					p.AddPlus()
				}
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				st.popColor(p)
				st.popStyle(p)
				p.AddBreak()
				finishCurrent()
				continue
			}
			if hasClass(c, "copy2") {
				resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
				st.pushColor(p, "#060")
				if !bgColorPushed {
					p.AddPlus()
				}
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				st.popColor(p)
				p.AddBreak()
				finishCurrent()
				continue
			}
			if hasClass(c, "pr") {
				resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
				previewStyle := st.curStyle | styleBoldBit
				if !prefs.LegacyBasicOM2 {
					previewStyle |= styleCenterBit
				}
				st.pushStyle(p, previewStyle)
				st.pushColor(p, "#ffffff")
				if !bgColorPushed {
					p.AddPlus()
				}
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				st.popColor(p)
				st.popStyle(p)
				p.AddBreak()
				finishCurrent()
				continue
			}
			if hasClass(c, "sepo") {
				resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
				st.pushStyle(p, st.curStyle|styleBoldBit)
				st.pushColor(p, "#060")
				if !bgColorPushed {
					p.AddPlus()
				}
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				st.popColor(p)
				st.popStyle(p)
				p.AddBreak()
				finishCurrent()
				continue
			}
			if hasClass(c, "bl") {
				resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
				p.AddText("| ")
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				p.AddBreak()
				finishCurrent()
				continue
			}
			if hasAnyClass(c, "str-up", "str-dw") {
				resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
				p.AddPlus()
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				p.AddBreak()
				finishCurrent()
				continue
			}
			if bgColorPushed {
				// OM2 applies D(background) to the current visual segment. Restoring
				// the parent background on the same line lets the following block
				// repaint the tail of this one. Finish coloured generic containers on
				// a line boundary before popBgcolor() restores the parent colour.
				bgBoundaryOnFinish = true
			}
			// A generic block establishes a line boundary, not paragraph
			// spacing. Mapping every nested div to V compounded quarter-em gaps
			// in wrapper-heavy mobile sites. Empty/consecutive B records collapse
			// naturally in legacy clients, while real p/list/quote elements keep
			// their explicit paragraph spacing.
			p.AddBreak()
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
				finishCurrent()
				continue
			}
			if hasClass(c, "br350") {
				resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
				p.AddBreak()
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				finishCurrent()
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
				finishCurrent()
				continue
			}
			if hasClass(c, "grn") {
				resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
				st.pushColor(p, "#aaaaaa")
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				st.popColor(p)
				finishCurrent()
				continue
			}
			if hasClass(c, "red") {
				resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
				st.pushColor(p, "#ff4444")
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				st.popColor(p)
				finishCurrent()
				continue
			}
			if hasClass(c, "zm") {
				resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
				st.pushStyle(p, st.curStyle|styleBoldBit)
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				st.popStyle(p)
				finishCurrent()
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
				st.pushColor(p, col)
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				st.popColor(p)
				recurse = false
			}
		case "a":
			if hasClass(c, "opis") {
				st.pushColor(p, "#151515")
				colorPushed = true
			} else if !colorPushed && st.linkColor != "" {
				st.pushColor(p, st.linkColor)
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
			link, linkOK := resolveNavigableLink(base, href)
			name := ""
			if t := findTextNode(c, visited); t != nil {
				if txt := strings.TrimSpace(t.Data); txt != "" {
					name = txt
				}
			}
			if name == "" {
				name = condenseSpaces(getAttr(c, "aria-label"))
			}
			if name == "" {
				if alt := findFirstImgAlt(c); alt != "" {
					name = alt
				}
			}
			if name == "" {
				name = condenseSpaces(getAttr(c, "title"))
			}
			if !linkOK {
				resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
				before := p.UnitCount()
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				if p.UnitCount() == before && name != "" && !iconOnly {
					p.AddText(name)
				}
				if buttonWrap {
					p.AddText("] ")
				}
				if blockWrap {
					p.AddBreak()
				}
				recurse = false
				finishCurrent()
				continue
			}
			// Render link with children as content
			p.BeginLink(link)
			before := p.UnitCount()
			prevIn := st.inLink
			st.inLink = true
			if c.FirstChild != nil {
				walkRich(c.FirstChild, base, p, visited, st, prefs)
			}
			st.inLink = prevIn
			if p.UnitCount() == before {
				p.AddText(name)
			}
			p.EndLink()
			if buttonWrap {
				p.AddText("]")
				p.AddText(" ")
			}
			resetComputedStyles(st, p, &colorPushed, &stylePushed, &alignedPushed)
			shouldBreak := true
			if buttonWrap || blockWrap {
				shouldBreak = true
			} else if isCompactInlineLinkGroup(c) {
				shouldBreak = false
			} else if iconOnly {
				if ns := nextSignificantSibling(c); ns != nil {
					if ns.Type == html.ElementNode && strings.EqualFold(ns.Data, "a") && isIconOnlyLink(ns) {
						shouldBreak = false
					} else if ns.Type == html.TextNode {
						trimmed := strings.TrimLeft(ns.Data, " \t\n")
						if hasPrefixAny(trimmed, "]", "|", ")", ",", ";", ":", ".", "/", "›", "»", ">") {
							shouldBreak = false
						}
					}
				}
			} else {
				if ns := nextSignificantSibling(c); ns != nil {
					if ns.Type == html.TextNode {
						trimmed := strings.TrimLeft(ns.Data, " \t\n")
						if hasPrefixAny(trimmed, "]", "|", ")", ",", ";", ":", ".", "/", "›", "»", ">") || isPhrasingParent(c.Parent) {
							shouldBreak = false
						}
					} else if ns.Type == html.ElementNode && strings.EqualFold(ns.Data, "a") {
						if hasAncestorClass(c, "bottom") || hasAncestorClass(c, "list_menu") {
							shouldBreak = false
						}
					} else if ns.Type == html.ElementNode && isInlinePhrasingTag(ns.Data) && isPhrasingParent(c.Parent) {
						shouldBreak = false
					}
				}
			}
			if !shouldBreak && shouldInsertInlineGapAfterLink(c) {
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
				if hasAncestorClass(c, "nl") {
					alt = "Icon"
				} else {
					alt = "Image"
				}
			}
			renderImageFromURL(p, st, base, src, alt, prefs)
			recurse = false
		case "caption":
			if txt := strings.TrimSpace(collectText(c)); txt != "" {
				markTextNodes(c, visited)
				p.AddPlus()
				p.AddText(txt)
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
			if txt := strings.TrimSpace(collectText(c)); txt != "" {
				markTextNodes(c, visited)
				p.AddText(txt)
			}
			p.AddBreak()
			recurse = false
		case "dd":
			if txt := strings.TrimSpace(collectText(c)); txt != "" {
				markTextNodes(c, visited)
				p.AddText(": " + txt)
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
		case "noscript":
			// x/net/html may expose noscript markup as one raw text node. Parse
			// that fallback as HTML, but never surface head-only stylesheet/meta
			// declarations as literal angle-bracket text.
			if hasAncestorTag(c, "head") {
				markTextNodes(c, visited)
				recurse = false
				break
			}
			var raw strings.Builder
			rawOnly := true
			for child := c.FirstChild; child != nil; child = child.NextSibling {
				if child.Type != html.TextNode {
					rawOnly = false
					break
				}
				raw.WriteString(child.Data)
			}
			if source := strings.TrimSpace(raw.String()); rawOnly && strings.Contains(source, "<") {
				markTextNodes(c, visited)
				contextNode := &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
				if fragments, err := html.ParseFragment(strings.NewReader(source), contextNode); err == nil {
					wrapper := &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div}
					for _, fragment := range fragments {
						wrapper.AppendChild(fragment)
					}
					if wrapper.FirstChild != nil {
						walkRich(wrapper.FirstChild, base, p, visited, st, prefs)
					}
				}
				recurse = false
			}
		case "blockquote":
			txt := strings.TrimSpace(collectText(c))
			p.AddParagraph()
			if txt != "" {
				markTextNodes(c, visited)
				p.AddText("> " + txt)
			} else {
				p.AddText("> ")
			}
			p.AddBreak()
			recurse = false
		case "label":
			if t := findTextNode(c, visited); t != nil {
				visited[t] = true
				if txt := condenseSpaces(t.Data); txt != "" {
					p.AddText(txt + ": ")
				}
			}
			// A label commonly wraps its control.  Walking the children after
			// marking the label text preserves that input instead of silently
			// dropping it.
			if c.FirstChild != nil {
				walkRich(c.FirstChild, base, p, visited, st, prefs)
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
		case "tr":
			if hasAncestorClass(c, "tlist") {
				before := p.UnitCount()
				if c.FirstChild != nil {
					walkRich(c.FirstChild, base, p, visited, st, prefs)
				}
				if p.UnitCount() > before {
					p.AddBreak()
				}
				recurse = false
			}
		case "table":
			if renderIconToolbar(c, base, p, st, prefs) {
				recurse = false
				finishCurrent()
				continue
			}
			if hasFormControls(c) || hasAnchorLinks(c) {
				// let recursion process interactive content to preserve links
			} else {
				if cap := findFirstChild(c, "caption"); cap != nil {
					if txt := strings.TrimSpace(collectText(cap)); txt != "" {
						markTextNodes(cap, visited)
						p.AddPlus()
						p.AddText(txt)
						p.AddBreak()
					}
				}
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
				alt := "Image"
				if img := findFirstChild(c, "img"); img != nil {
					if a := strings.TrimSpace(getAttr(img, "alt")); a != "" {
						alt = a
					}
				}
				renderImageFromURL(p, st, base, chosen, alt, prefs)
			}
			recurse = false
		case "form":
			action := getAttr(c, "action")
			absAction := resolveFormActionURL(base, action)
			formAction := absAction
			if formAction == "" {
				formAction = action
			}
			p.AddFormWithMethod(formAction, getAttr(c, "method"))
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
				if value == "" {
					value = strings.TrimSpace(collectText(c))
				}
				if prompt := inputAccessiblePrompt(c); prompt != "" {
					p.AddText(prompt + ": ")
				}
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
			case "text", "search", "email", "tel", "url", "number", "date", "datetime-local", "time", "month", "week":
				if prompt := inputAccessiblePrompt(c); prompt != "" {
					p.AddText(prompt + ": ")
				}
				p.AddTextInput(name, value)
			case "password":
				if prompt := inputAccessiblePrompt(c); prompt != "" {
					p.AddText(prompt + ": ")
				}
				p.AddPassInput(name, value)
			case "submit":
				p.AddSubmit(name, value)
			case "checkbox":
				checked := boolAttr(c, "checked")
				if value == "" {
					value = "on"
				}
				p.AddCheckbox(name, value, checked)
			case "radio":
				checked := boolAttr(c, "checked")
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
						p.RecordHidden(actionKey, name, value)
						overrides := map[string]string{}
						ensureHiddenFieldOverrides(actionKey, overrides)
						for overrideName, overrideValue := range overrides {
							p.RecordHidden(actionKey, overrideName, overrideValue)
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
			multiple := boolAttr(c, "multiple")
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
					sel := boolAttr(oc, "selected")
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
				// Skip metadata and executable/style content. Noscript is useful
				// fallback content for this deliberately non-JavaScript renderer.
				skip := false
				if par := c.Parent; par != nil && par.Type == html.ElementNode {
					t := strings.ToLower(par.Data)
					if t == "style" || t == "script" || t == "link" || t == "meta" || t == "head" {
						skip = true
					}
				}
				if !skip {
					txt := textNodeContent(c, st)
					if prefs.LegacyBasicOM2 {
						txt = legacyBasicSafeText(txt)
					}
					if txt != "" {
						if !visited[c] {
							visited[c] = true
							addTextWithColor(p, st, c, txt)
						}
					}
				}
			}
		}
		if recurse && c.FirstChild != nil {
			walkRich(c.FirstChild, base, p, visited, st, prefs)
		}
		if bgBoundaryOnFinish {
			p.AddBreak()
		}
		finishCurrent()
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
				if t == "style" || t == "script" || t == "link" || t == "meta" {
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

// inputAccessiblePrompt exposes labels that modern, visually styled forms
// keep only in accessibility attributes or placeholders. An HTML label is
// already emitted by walkRich, so it suppresses the synthetic prompt.
func inputAccessiblePrompt(n *html.Node) string {
	if n == nil || hasHTMLLabel(n) {
		return ""
	}
	if isCompactSearchInput(n) && strings.TrimSpace(getAttr(n, "placeholder")) == "" {
		// Search tables commonly expose aria-label only because their adjacent
		// submit button is a background icon. The OMS submit remains visible, so
		// repeating aria-label as a full line is noisy and breaks compact layout.
		return ""
	}
	for _, attr := range []string{"aria-label", "placeholder", "title"} {
		if value := strings.TrimSpace(condenseSpaces(getAttr(n, attr))); value != "" {
			return strings.TrimRight(value, " :")
		}
	}
	return ""
}

func isCompactSearchInput(n *html.Node) bool {
	if n == nil || !strings.EqualFold(n.Data, "input") {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(getAttr(n, "type")), "search") ||
		hasAnyClass(n, "search", "search-input", "input-txt_search") {
		return true
	}
	for parent := n.Parent; parent != nil; parent = parent.Parent {
		if parent.Type != html.ElementNode {
			continue
		}
		if hasAnyClass(parent, "search", "search-form", "search-wrap", "input-txt_grid", "input-txt_wrapper_search") {
			return true
		}
		if strings.EqualFold(parent.Data, "form") {
			action := strings.ToLower(strings.TrimSpace(getAttr(parent, "action")))
			return strings.Contains(action, "search") || strings.Contains(action, "поиск")
		}
	}
	return false
}

func hasHTMLLabel(n *html.Node) bool {
	for parent := n.Parent; parent != nil; parent = parent.Parent {
		if parent.Type == html.ElementNode && strings.EqualFold(parent.Data, "label") {
			return true
		}
	}
	id := strings.TrimSpace(getAttr(n, "id"))
	if id == "" {
		return false
	}
	root := n
	for root.Parent != nil {
		root = root.Parent
	}
	var find func(*html.Node) bool
	find = func(cur *html.Node) bool {
		if cur.Type == html.ElementNode && strings.EqualFold(cur.Data, "label") && strings.TrimSpace(getAttr(cur, "for")) == id {
			return true
		}
		for child := cur.FirstChild; child != nil; child = child.NextSibling {
			if find(child) {
				return true
			}
		}
		return false
	}
	return find(root)
}

func hasAncestorTag(n *html.Node, tag string) bool {
	for parent := n.Parent; parent != nil; parent = parent.Parent {
		if parent.Type == html.ElementNode && strings.EqualFold(parent.Data, tag) {
			return true
		}
	}
	return false
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
				case "span", "div":
					if hasAnyClass(c, "balls", "icon", "ico", "sprite") || strings.Contains(strings.ToLower(getAttr(c, "style")), "background") {
						hasIcon = true
					}
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
	switch want {
	case RGB565MIME:
		return []cacheCandidate{{format: RGB565MIME, quality: 0}}
	case RGB565AlphaMIME:
		return []cacheCandidate{{format: RGB565AlphaMIME, quality: 0}}
	case "image/png":
		return []cacheCandidate{{format: "image/png", quality: 0}}
	case "image/gif":
		return []cacheCandidate{{format: "image/gif", quality: 0}}
	default:
		return []cacheCandidate{
			{format: "image/jpeg", quality: jpegQualityFor(prefs)},
			{format: "image/png", quality: 0},
		}
	}
}

func jpegQualityFor(prefs RenderOptions) int {
	if usesLowMemoryImagePalette(prefs) {
		if prefs.HighQuality {
			return 76
		}
		return 32
	}
	if prefs.HighQuality {
		return 85
	}
	return 40
}

const lowMemoryImageHeapLimit = 512 << 10

func usesLowMemoryImagePalette(prefs RenderOptions) bool {
	return prefs.HeapBytes > 0 && prefs.HeapBytes <= lowMemoryImageHeapLimit
}

func expandQuantizedChannel(value uint8, bits uint) uint8 {
	levels := uint32((1 << bits) - 1)
	quantized := (uint32(value)*levels + 127) / 255
	return uint8((quantized*255 + levels/2) / levels)
}

// quantizeLowMemoryImage reduces both decoded working-set diversity and
// encoded entropy for very small Java ME heaps. High quality uses RGB444;
// low quality uses RGB332, matching the limited displays of these handsets.
func quantizeLowMemoryImage(img image.Image, highQuality bool) image.Image {
	if img == nil {
		return img
	}
	bounds := img.Bounds()
	out := image.NewNRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	redBits, greenBits, blueBits := uint(3), uint(3), uint(2)
	if highQuality {
		redBits, greenBits, blueBits = 4, 4, 4
	}
	for y := 0; y < bounds.Dy(); y++ {
		for x := 0; x < bounds.Dx(); x++ {
			pixel := color.NRGBAModel.Convert(img.At(bounds.Min.X+x, bounds.Min.Y+y)).(color.NRGBA)
			pixel.R = expandQuantizedChannel(pixel.R, redBits)
			pixel.G = expandQuantizedChannel(pixel.G, greenBits)
			pixel.B = expandQuantizedChannel(pixel.B, blueBits)
			out.SetNRGBA(x, y, pixel)
		}
	}
	return out
}

func fetchAndEncodeImage(absURL string, prefs RenderOptions) ([]byte, int, int, bool) {
	debug := os.Getenv("OMS_IMG_DEBUG") == "1"
	candidates := cacheCandidatesFor(prefs)
	cacheKey := imageCacheVariantKey(absURL, prefs)

	for _, cand := range candidates {
		if data, w, h, ok := imgCacheGet(cand.format, cand.quality, cacheKey); ok {
			if debug {
				log.Printf("IMG cache hit mem fmt=%s q=%d url=%s", cand.format, cand.quality, absURL)
			}
			return data, w, h, true
		}
		if data, w, h, ok := diskCacheGet(cand.format, cand.quality, cacheKey); ok {
			imgCachePut(cand.format, cand.quality, cacheKey, data, w, h)
			if debug {
				log.Printf("IMG cache hit disk fmt=%s q=%d url=%s", cand.format, cand.quality, absURL)
			}
			return data, w, h, true
		}
	}

	if strings.HasPrefix(absURL, "data:") {
		if data, w, h, format, quality, ok := decodeDataURI(absURL, prefs); ok {
			imgCachePut(format, quality, cacheKey, data, w, h)
			diskCachePut(format, quality, cacheKey, data, w, h)
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

	imgCachePut(format, quality, cacheKey, data, w, h)
	diskCachePut(format, quality, cacheKey, data, w, h)
	return data, w, h, true
}

func imageCacheVariantKey(absURL string, prefs RenderOptions) string {
	// Anonymous/private ambiguity is unsafe: without an explicit gateway
	// partition, skip caching rather than share origin-dependent assets.
	if strings.TrimSpace(prefs.CachePartition) == "" {
		return ""
	}
	return fmt.Sprintf("%s|session=%s|viewport=%dx%d|mime=%s|quality=%t|inline=%d|dialect=%s|lowmem=%t|v=4",
		absURL, prefs.CachePartition, prefs.ScreenW, prefs.ScreenH, prefs.ImageMIME,
		prefs.HighQuality, prefs.MaxInlineKB, prefs.DialectID, usesLowMemoryImagePalette(prefs))
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

// clampImageToPixelBudget keeps an uncompressed handset payload within both
// the OM3 uint16 tag limit and the client-advertised per-image memory budget.
func clampImageToPixelBudget(img image.Image, maxBytes, bytesPerPixel int) (image.Image, int, int) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return img, w, h
	}
	if maxBytes <= 0 || maxBytes > 0xfffe {
		maxBytes = 0xfffe
	}
	if bytesPerPixel < 1 {
		bytesPerPixel = 1
	}
	maxPixels := maxBytes / bytesPerPixel
	if maxPixels < 1 {
		maxPixels = 1
	}
	if w <= maxPixels/h {
		return img, w, h
	}
	scale := math.Sqrt(float64(maxPixels) / float64(w*h))
	newW := int(math.Floor(float64(w) * scale))
	newH := int(math.Floor(float64(h) * scale))
	if newW < 1 {
		newW = 1
	}
	if newH < 1 {
		newH = 1
	}
	for newW*newH > maxPixels {
		if newW >= newH && newW > 1 {
			newW--
		} else if newH > 1 {
			newH--
		} else {
			break
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Over, nil)
	return dst, newW, newH
}

func clampImageToRGB565Budget(img image.Image, maxBytes int) (image.Image, int, int) {
	return clampImageToPixelBudget(img, maxBytes, 2)
}

func encodeRGB565BE(img image.Image, maxBytes int) ([]byte, int, int) {
	img, w, h := clampImageToRGB565Budget(img, maxBytes)
	if w <= 0 || h <= 0 {
		return nil, w, h
	}
	out := make([]byte, w*h*2)
	b := img.Bounds()
	position := 0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, bl, a := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			// RGBA values are alpha-premultiplied. Composite transparency over
			// white, matching the normal page background on old handsets.
			r += 0xffff - a
			g += 0xffff - a
			bl += 0xffff - a
			pixel := uint16(((r * 31 / 0xffff) << 11) |
				((g * 63 / 0xffff) << 5) | (bl * 31 / 0xffff))
			out[position] = byte(pixel >> 8)
			out[position+1] = byte(pixel)
			position += 2
		}
	}
	return out, w, h
}

func encodeRGB565AlphaBE(img image.Image, maxBytes int) ([]byte, int, int) {
	img, w, h := clampImageToPixelBudget(img, maxBytes, 3)
	if w <= 0 || h <= 0 {
		return nil, w, h
	}
	out := make([]byte, w*h*3)
	b := img.Bounds()
	position := 0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			pixelColor := color.NRGBAModel.Convert(
				img.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA)
			pixel := uint16((uint32(pixelColor.R)*31/255)<<11 |
				(uint32(pixelColor.G)*63/255)<<5 |
				uint32(pixelColor.B)*31/255)
			out[position] = byte(pixel >> 8)
			out[position+1] = byte(pixel)
			out[position+2] = pixelColor.A
			position += 3
		}
	}
	return out, w, h
}

// EncodeRGB565AlphaResource converts a decoded web image to the lightweight
// RGB565+straight-alpha payload used by Sky Operetta's OM3 and OM4 bridges.
// Requested dimensions describe the OM4 drawing box; the byte budget may
// reduce them proportionally for bounded handset memory.
func EncodeRGB565AlphaResource(raw []byte, width, height, maxBytes int) ([]byte, int, int, error) {
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, 0, 0, err
	}
	if width > 0 && height > 0 &&
		(img.Bounds().Dx() != width || img.Bounds().Dy() != height) {
		dst := image.NewRGBA(image.Rect(0, 0, width, height))
		draw.CatmullRom.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Over, nil)
		img = dst
	}
	data, encodedWidth, encodedHeight := encodeRGB565AlphaBE(img, maxBytes)
	return data, encodedWidth, encodedHeight, nil
}

func flattenImageOnWhite(img image.Image) image.Image {
	if img == nil {
		return img
	}
	b := img.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), img, b.Min, draw.Over)
	return dst
}

func encodePNGToBudget(img image.Image, maxBytes int) ([]byte, int, int, error) {
	current := img
	var last []byte
	for attempt := 0; attempt < 14; attempt++ {
		var out bytes.Buffer
		enc := png.Encoder{CompressionLevel: png.BestCompression}
		if err := enc.Encode(&out, current); err != nil {
			return nil, 0, 0, err
		}
		last = append(last[:0], out.Bytes()...)
		b := current.Bounds()
		w, h := b.Dx(), b.Dy()
		if maxBytes <= 0 || len(last) <= maxBytes || (w <= 2 && h <= 2) {
			return append([]byte(nil), last...), w, h, nil
		}

		ratio := math.Sqrt(float64(maxBytes)/float64(len(last))) * 0.88
		if ratio > 0.84 {
			ratio = 0.84
		}
		if ratio < 0.35 {
			ratio = 0.35
		}
		newW := int(math.Floor(float64(w) * ratio))
		newH := int(math.Floor(float64(h) * ratio))
		if newW < 1 {
			newW = 1
		}
		if newH < 1 {
			newH = 1
		}
		if newW == w && w > 1 {
			newW--
		}
		if newH == h && h > 1 {
			newH--
		}
		dst := image.NewNRGBA(image.Rect(0, 0, newW, newH))
		draw.CatmullRom.Scale(dst, dst.Bounds(), current, b, draw.Over, nil)
		current = dst
	}
	b := current.Bounds()
	return append([]byte(nil), last...), b.Dx(), b.Dy(), nil
}

func encodeJPEGToBudget(img image.Image, quality, maxBytes int) ([]byte, int, int, int, error) {
	if quality <= 0 {
		quality = 40
	}
	current := img
	currentQuality := quality
	var last []byte
	for attempt := 0; attempt < 12; attempt++ {
		var out bytes.Buffer
		if err := jpeg.Encode(&out, current, &jpeg.Options{Quality: currentQuality}); err != nil {
			return nil, 0, 0, currentQuality, err
		}
		last = append(last[:0], out.Bytes()...)
		b := current.Bounds()
		w, h := b.Dx(), b.Dy()
		if maxBytes <= 0 || len(last) <= maxBytes || (w <= 8 && h <= 8) {
			return append([]byte(nil), last...), w, h, currentQuality, nil
		}

		// JPEG size roughly follows pixel count. Scale by the square root of the
		// byte ratio, leaving headroom for entropy/headers, and lower quality a
		// little on every retry. This converges quickly for CLDC-sized budgets.
		ratio := math.Sqrt(float64(maxBytes)/float64(len(last))) * 0.90
		if ratio > 0.88 {
			ratio = 0.88
		}
		if ratio < 0.45 {
			ratio = 0.45
		}
		newW := int(math.Floor(float64(w) * ratio))
		newH := int(math.Floor(float64(h) * ratio))
		if newW < 1 {
			newW = 1
		}
		if newH < 1 {
			newH = 1
		}
		if newW == w && w > 1 {
			newW--
		}
		if newH == h && h > 1 {
			newH--
		}
		dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
		draw.CatmullRom.Scale(dst, dst.Bounds(), current, b, draw.Over, nil)
		current = dst
		if currentQuality > 25 {
			currentQuality -= 3
		}
	}
	b := current.Bounds()
	return append([]byte(nil), last...), b.Dx(), b.Dy(), currentQuality, nil
}

func encodeImage(img image.Image, prefs RenderOptions) ([]byte, int, int, string, int, error) {
	img, w, h := clampImageToScreenWidth(img, prefs.ScreenW)

	want := strings.ToLower(strings.TrimSpace(prefs.ImageMIME))
	if want == "" {
		want = "image/jpeg"
	}
	if want == "image/jpeg" && imageHasAlpha(img) {
		if prefs.DialectID == "om2-basic" {
			img = flattenImageOnWhite(img)
			w, h = img.Bounds().Dx(), img.Bounds().Dy()
		} else {
			want = "image/png"
		}
	}
	if usesLowMemoryImagePalette(prefs) {
		img = quantizeLowMemoryImage(img, prefs.HighQuality)
		w, h = img.Bounds().Dx(), img.Bounds().Dy()
	}

	var out bytes.Buffer
	quality := 0

	switch want {
	case RGB565MIME:
		maxBytes := prefs.MaxInlineKB * 1024
		data, rgbW, rgbH := encodeRGB565BE(img, maxBytes)
		return data, rgbW, rgbH, want, quality, nil
	case RGB565AlphaMIME:
		maxBytes := prefs.MaxInlineKB * 1024
		data, rgbW, rgbH := encodeRGB565AlphaBE(img, maxBytes)
		return data, rgbW, rgbH, want, quality, nil
	case "image/png":
		if prefs.DialectID == "om2-basic" && prefs.MaxInlineKB > 0 {
			data, fitW, fitH, err := encodePNGToBudget(img, prefs.MaxInlineKB*1024)
			return data, fitW, fitH, want, quality, err
		}
		enc := png.Encoder{CompressionLevel: png.DefaultCompression}
		if prefs.HighQuality || usesLowMemoryImagePalette(prefs) {
			enc.CompressionLevel = png.BestCompression
		}
		if err := enc.Encode(&out, img); err != nil {
			return nil, 0, 0, want, quality, err
		}
		return append([]byte(nil), out.Bytes()...), w, h, want, quality, nil
	case "image/gif":
		options := &gif.Options{NumColors: 128}
		if prefs.HighQuality {
			options.NumColors = 256
		}
		if err := gif.Encode(&out, img, options); err != nil {
			return nil, 0, 0, want, quality, err
		}
		return append([]byte(nil), out.Bytes()...), w, h, want, quality, nil
	default:
		want = "image/jpeg"
		quality = jpegQualityFor(prefs)
		if prefs.DialectID == "om2-basic" && prefs.MaxInlineKB > 0 {
			data, fitW, fitH, fitQuality, err := encodeJPEGToBudget(img, quality, prefs.MaxInlineKB*1024)
			return data, fitW, fitH, want, fitQuality, err
		}
		if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: quality}); err != nil {
			return nil, 0, 0, want, quality, err
		}
		return append([]byte(nil), out.Bytes()...), w, h, want, quality, nil
	}
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

// RenderDocument is the backwards-compatible Opera Mini facade. New formats
// should reuse TransformDocument and provide their own presentation encoder.
func RenderDocument(doc *UpstreamDocument, hdr http.Header, opts *RenderOptions) (*Page, error) {
	if doc == nil {
		return encodeErrorPage("", "Internal server error", opts)
	}
	effectiveURL := doc.URL
	if effectiveURL == "" {
		effectiveURL = "about:blank"
	}
	if doc.Header == nil {
		doc.Header = http.Header{}
	}
	body := doc.Body
	if len(body) == 0 {
		body = doc.RawBody
	}
	if len(body) == 0 {
		return encodeErrorPage(effectiveURL, "Empty response", opts)
	}
	if looksLikeOMS(body) {
		if os.Getenv("OMS_ALLOW_RAW_PASSTHROUGH") != "1" {
			return encodeErrorPage(effectiveURL, "Pre-encoded OMS response rejected", opts)
		}
		page := &Page{
			Data:       append([]byte(nil), body...),
			SetCookies: append([]string(nil), doc.SetCookies...),
			Stats: TrafficStats{
				OriginTransferBytes: doc.TransferBytes,
				OriginDecodedBytes:  len(body),
				EncodedBytes:        len(body),
			},
		}
		return page, nil
	}
	model, err := TransformDocument(doc, hdr, opts)
	if err != nil {
		return encodeErrorPage(effectiveURL, "Internal server error while parsing", opts)
	}
	return EncodeDocument(model, opts)
}

// TransformDocument converts an origin response into a reusable presentation
// display list. It performs HTML/CSS simplification and asset transcoding but
// contains no Opera Mini auth, framing, version, compression or pagination.
func TransformDocument(doc *UpstreamDocument, hdr http.Header, opts *RenderOptions) (*presentation.Document, error) {
	if doc == nil {
		return nil, fmt.Errorf("transform content: nil origin response")
	}
	effectiveURL := doc.URL
	if effectiveURL == "" {
		effectiveURL = "about:blank"
	}
	if doc.Header == nil {
		doc.Header = http.Header{}
	}
	if shouldOfferDownload(doc.Header) {
		return transformDownloadDocument(doc, opts), nil
	}
	body := doc.Body
	if len(body) == 0 {
		body = doc.RawBody
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("transform content: empty response")
	}
	if looksLikeOMS(body) {
		return nil, fmt.Errorf("transform content: source is already OMS encoded")
	}
	utf8Body := decodeLegacyToUTF8(body, doc.Header.Get("Content-Type"))
	decodedLen := len(utf8Body)
	parsed, err := html.Parse(bytes.NewReader(utf8Body))
	if err != nil {
		return nil, fmt.Errorf("transform content: parse HTML: %w", err)
	}
	rp := defaultRenderPrefs()
	var jar http.CookieJar
	if opts != nil {
		rp = *opts
		jar = opts.Jar
	}
	target := newContentTarget(effectiveURL)
	target.AddStyle(styleDefault)
	base := effectiveURL
	if i := strings.Index(base, "?"); i != -1 {
		base = base[:i]
	}
	base = findBaseURL(parsed, base)
	rp.ReqHeaders = hdr
	rp.Referrer = effectiveURL
	rp.Styles = buildStylesheet(parsed, base, hdr, jar, &rp)
	chosenCol := ""
	chosenBg := ""
	chosenLink := "#0000ad"
	if bodyNode := findFirstByTag(parsed, "body"); bodyNode != nil {
		var bgHex, fgHex string
		if rp.Styles != nil {
			if props := computeStyleFor(bodyNode, rp.Styles); props != nil {
				if v := props["background-color"]; v != "" {
					bgHex = v
				}
				if v := props["color"]; v != "" {
					fgHex = v
				}
			}
		}
		if fgHex == "" {
			if v := getAttr(bodyNode, "text"); v != "" {
				fgHex = v
			}
		}
		if bgHex == "" {
			if v := getAttr(bodyNode, "bgcolor"); v != "" {
				bgHex = v
			}
		}
		if value := cssToHex(getAttr(bodyNode, "link")); value != "" {
			chosenLink = value
		}
		if bgHex == "" {
			if v := getAttr(bodyNode, "bgcolor"); v != "" {
				bgHex = v
			}
		}
		if stl := getAttr(bodyNode, "style"); stl != "" {
			if v := parseCssColor(stl, "background-color"); v != "" {
				bgHex = v
			}
			if v := parseCssColor(stl, "color"); v != "" {
				fgHex = v
			}
		}
		if bgHex != "" {
			target.AddBgcolor(bgHex)
		}
		chosenBg = bgHex
		chosenCol = ""
		if fgHex != "" {
			target.AddStyle(styleDefault | (uint32(calcColor(readableTextColorForOM(fgHex, chosenBg))) << 8))
			chosenCol = fgHex
		} else if bgHex != "" && isDarkHex(bgHex) {
			target.AddStyle(styleDefault | (uint32(calcColor(readableTextColorForOM("#eeeeee", chosenBg))) << 8))
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
	st.linkColor = chosenLink
	st.css = rp.Styles
	target.AddStyle(styleDefault)
	walkRich(parsed, base, target, visited, &st, rp)
	if len(doc.SetCookies) > 0 {
		var pairs []string
		for _, sc := range doc.SetCookies {
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
	model := target.Document()
	model.Title = extractTitle(parsed)
	model.SetCookies = append([]string(nil), doc.SetCookies...)
	model.Metrics.OriginTransferBytes = doc.TransferBytes
	model.Metrics.OriginDecodedBytes = decodedLen

	return model, nil
}

// EncodeDocument maps a protocol-independent presentation document to the
// Opera Mini OMS/OBML dialect selected by opts. It owns framing, auth echo,
// pagination, transport version and compression.
func EncodeDocument(model *presentation.Document, opts *RenderOptions) (*Page, error) {
	if model == nil {
		return nil, fmt.Errorf("encode OMS: nil presentation document")
	}
	rp := defaultRenderPrefs()
	if opts != nil {
		rp = *opts
	}
	effectiveURL := model.URL
	if effectiveURL == "" {
		effectiveURL = "about:blank"
	}

	// Configure the dialect before emitting style records: OM3 uses a wider
	// style payload than OM1/OM2.
	p := NewPage()
	p.SetTransport(rp.ClientVersion, rp.Compression)
	p.SetCookies = append([]string(nil), model.SetCookies...)
	p.Stats.OriginTransferBytes = model.Metrics.OriginTransferBytes
	p.Stats.OriginDecodedBytes = model.Metrics.OriginDecodedBytes
	p.FormHidden = cloneHiddenFields(model.FormHidden)
	p.NoCache = model.NoCache
	p.AddString("1/" + effectiveURL)
	if rp.AuthCode != "" {
		p.AddAuthcode(rp.AuthCode)
	}
	if rp.AuthPrefix != "" {
		p.AddAuthprefix(rp.AuthPrefix)
	}
	encodeContentOperations(p, model)
	pageIdx := 1
	if opts != nil {
		if opts.Page > 0 {
			pageIdx = opts.Page
		}
	}
	maxTags, maxPageBytes, maxPageHeap := EffectivePaginationLimits(&rp)
	if pageIdx < 1 {
		pageIdx = 1
	}
	{
		fullRaw := append([]byte(nil), p.Data...)
		packed := NewPage()
		packed.Data = fullRaw
		packed.SetTransport(rp.ClientVersion, rp.Compression)
		packed.finalize()
		p.CachePacked = append([]byte(nil), packed.Data...)
	}
	parts := splitByTagsWithBudgets(p.Data, maxTags, rp.ClientVersion, maxPageBytes, maxPageHeap)
	if len(parts) == 0 {
		p.finalize()
		return p, nil
	}
	if pageIdx > len(parts) {
		pageIdx = len(parts)
	}
	sel := parts[pageIdx-1]
	if pageIdx > 1 {
		sel = rewriteInitialURLRaw(sel, pageIdx, maxTags)
	}
	serverBase := ""
	if opts != nil {
		serverBase = opts.ServerBase
	}
	if len(parts) > 1 && serverBase != "" {
		nav := NewPage()
		nav.SetTransport(rp.ClientVersion, rp.Compression)
		nav.AddHr("")
		if pageIdx > 1 {
			prevURL := BuildPaginationLink(effectiveURL, &rp, pageIdx-1, maxTags)
			nav.AddLink("0/"+prevURL, "[<<]")
		} else {
			nav.AddText("[<<]")
		}
		nav.AddText(" ")
		if pageIdx < len(parts) {
			nextURL := BuildPaginationLink(effectiveURL, &rp, pageIdx+1, maxTags)
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
				nav.AddText("...")
			}
			label := fmt.Sprintf("[%d]", n)
			if n == pageIdx {
				nav.AddText("*" + label)
			} else {
				pageURL := BuildPaginationLink(effectiveURL, &rp, n, maxTags)
				nav.AddLink("0/"+pageURL, label)
			}
			lastShown = n
		}
		nav.AddBreak()
		budget := maxPageBytes
		allowed := budget - len(nav.Data)
		if allowed < 1024 {
			allowed = 1024
		}
		sel = shrinkPartToMaxBytes(sel, allowed, rp.ClientVersion)
		sel = append(sel, nav.Data...)
	}
	p.Data = sel
	p.partCur = pageIdx
	p.partCnt = len(parts)
	p.SetTransport(rp.ClientVersion, rp.Compression)
	p.finalize()
	return p, nil
}

// LoadPageWithHeadersAndOptions performs HTTP GET with optional headers and rendering options.
func LoadPageWithHeadersAndOptions(oURL string, hdr http.Header, opts *RenderOptions) (*Page, error) {
	return LoadPageWithHeadersAndOptionsCtx(context.Background(), oURL, hdr, opts)
}

func LoadPageWithHeadersAndOptionsCtx(ctx context.Context, oURL string, hdr http.Header, opts *RenderOptions) (*Page, error) {
	doc, err := FetchDocumentWithHeadersAndOptionsCtx(ctx, oURL, hdr, opts)
	if err != nil {
		return nil, err
	}
	return RenderDocument(doc, hdr, opts)
}

// FetchDocumentWithHeadersAndOptionsCtx performs origin acquisition without
// transforming or encoding the response. The RenderOptions argument remains a
// temporary compatibility bridge for form submission and cookie-jar state.
func FetchDocumentWithHeadersAndOptionsCtx(ctx context.Context, oURL string, hdr http.Header, opts *RenderOptions) (*UpstreamDocument, error) {
	effectiveURL := oURL
	method := http.MethodGet
	var bodyReader io.Reader
	var contentTypeOverride string
	debugHTTP := os.Getenv("OMS_HTTP_DEBUG") == "1"

	if hdr == nil {
		hdr = http.Header{}
	}

	if opts != nil {
		if fb := strings.TrimSpace(opts.FormBody); debugHTTP && fb != "" && fb != "0" {
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
		if submission := prepareOperaMiniSubmission(oURL, opts.FormBody); submission != nil {
			if debugHTTP {
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

	req, err := http.NewRequestWithContext(ctx, method, effectiveURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("build origin request: %w", err)
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
	// Internal session routing must never cross the origin trust boundary.
	req.Header.Del("X-Operetta-Client-Key")
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
	if debugHTTP {
		var ck string
		if c := req.Header.Get("Cookie"); c != "" {
			ck = c
		}
		xk := req.Header.Get("X-Operetta-Client-Key")
		var jarInfo string
		if hc.Jar != nil {
			if u := req.URL; u != nil {
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
		return nil, fmt.Errorf("fetch origin %q: %w", effectiveURL, err)
	}
	defer resp.Body.Close()

	rawBody, err := readAllLimited(resp.Body, maxOriginTransferBytes)
	if err != nil {
		return nil, fmt.Errorf("read origin %q: %w", effectiveURL, err)
	}
	transferBytes := len(rawBody)
	body := rawBody
	if encoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))); encoding != "" {
		switch encoding {
		case "gzip":
			gr, gerr := gzip.NewReader(bytes.NewReader(rawBody))
			if gerr != nil {
				return nil, fmt.Errorf("decode gzip response %q: %w", effectiveURL, gerr)
			}
			if decoded, derr := readAllLimited(gr, maxOriginDecodedBytes); derr == nil {
				body = decoded
			} else {
				_ = gr.Close()
				return nil, fmt.Errorf("decode gzip response %q: %w", effectiveURL, derr)
			}
			_ = gr.Close()
		case "deflate":
			if zr, zerr := zlib.NewReader(bytes.NewReader(rawBody)); zerr == nil {
				if decoded, derr := readAllLimited(zr, maxOriginDecodedBytes); derr == nil {
					body = decoded
				} else {
					_ = zr.Close()
					return nil, fmt.Errorf("decode deflate response %q: %w", effectiveURL, derr)
				}
				_ = zr.Close()
			} else if fr := flate.NewReader(bytes.NewReader(rawBody)); fr != nil {
				if decoded, derr := readAllLimited(fr, maxOriginDecodedBytes); derr == nil {
					body = decoded
				} else {
					_ = fr.Close()
					return nil, fmt.Errorf("decode raw deflate response %q: %w", effectiveURL, derr)
				}
				_ = fr.Close()
			}
		}
	}
	if len(body) == 0 {
		body = rawBody
	}
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
	}

	finalURL := effectiveURL
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	doc := &UpstreamDocument{
		URL:           finalURL,
		RawBody:       append([]byte(nil), rawBody...),
		Body:          append([]byte(nil), body...),
		TransferBytes: transferBytes,
		Header:        cloneHeader(resp.Header),
		Status:        resp.StatusCode,
		ContentLength: resp.ContentLength,
		SetCookies:    append([]string(nil), resp.Header["Set-Cookie"]...),
	}
	return doc, nil
}

const (
	maxOriginTransferBytes = 8 << 20
	maxOriginDecodedBytes  = 32 << 20
)

func readAllLimited(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("invalid read limit %d", limit)
	}
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return b, nil
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
	actionMethodCode := ""
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
		isActionKey := looksLikeActionKey(key)
		if actionOverride == "" && isActionKey {
			actionOverride = key
		}
		lk := strings.ToLower(key)
		if strings.Contains(lk, "pass") || strings.Contains(lk, "pwd") || strings.Contains(lk, "token") {
			hasSensitive = hasSensitive || (val != "")
		}
		if isActionKey {
			if actionMethodCode == "" {
				actionMethodCode = val
			}
			continue
		}
		normalizedKey := key
		values.Add(normalizedKey, val)
	}
	if !seenOPF && actionMethodCode != "" {
		method = formMethodFromCode(actionMethodCode, method)
	}
	if method == http.MethodGet && hasSensitive {
		////if os.Getenv("OMS_HTTP_DEBUG") == "1" {
		if seenOPF {
			log.Printf("SUBMISSION heuristic: overriding OPF to POST (has sensitive fields)")
		} else {
			log.Printf("SUBMISSION heuristic: forcing POST (has sensitive fields, no opf)")
		}
		////}
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
		if hasSensitive && strings.HasPrefix(strings.ToLower(strings.TrimSpace(targetURL)), "http://") {
			targetURL = upgradeURLStringToHTTPS(targetURL)
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
	if hasSensitive && method == http.MethodPost {
		target = upgradeURLToHTTPS(target)
	}
	finalURL := target.String()
	if hasSensitive && method == http.MethodPost {
		finalURL = upgradeURLStringToHTTPS(finalURL)
	}
	return &formSubmission{
		Method:      http.MethodPost,
		URL:         finalURL,
		Body:        values.Encode(),
		ContentType: "application/x-www-form-urlencoded",
	}
}

func formMethodFromCode(code, fallback string) string {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "2", "post":
		return http.MethodPost
	case "0", "1", "get", "":
		if fallback != "" {
			return fallback
		}
	}
	return http.MethodGet
}

func upgradeURLToHTTPS(u *url.URL) *url.URL {
	if u == nil {
		return nil
	}
	if !strings.EqualFold(strings.TrimSpace(u.Scheme), "http") {
		return u
	}
	clone := *u
	clone.Scheme = "https"
	clone.Host = stripDefaultPort(clone.Host, "80")
	return &clone
}

func upgradeURLStringToHTTPS(raw string) string {
	if raw == "" {
		return raw
	}
	lower := strings.ToLower(strings.TrimSpace(raw))
	if !strings.HasPrefix(lower, "http://") {
		return raw
	}
	if u, err := url.Parse(raw); err == nil {
		upgraded := upgradeURLToHTTPS(u)
		if upgraded != nil {
			return upgraded.String()
		}
	}
	return "https://" + strings.TrimPrefix(raw, "http://")
}

func stripDefaultPort(hostPort, defaultPort string) string {
	if hostPort == "" || defaultPort == "" {
		return hostPort
	}
	if h, p, err := net.SplitHostPort(hostPort); err == nil {
		if p == defaultPort {
			return h
		}
		return hostPort
	}
	suffix := ":" + defaultPort
	if strings.HasSuffix(hostPort, suffix) && !strings.Contains(hostPort, "]") {
		return strings.TrimSuffix(hostPort, suffix)
	}
	return hostPort
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
			if strings.TrimSpace(collapseHTMLWhitespace(s.Data)) == "" {
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

func previousSignificantSibling(n *html.Node) *html.Node {
	for s := n.PrevSibling; s != nil; s = s.PrevSibling {
		if s.Type == html.TextNode {
			if strings.TrimSpace(collapseHTMLWhitespace(s.Data)) == "" {
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

func shouldPreserveInlineWhitespace(n *html.Node) bool {
	if n == nil || n.Type != html.TextNode {
		return false
	}
	if strings.TrimSpace(collapseHTMLWhitespace(n.Data)) != "" {
		return false
	}
	return isInlineContentBoundary(previousSignificantSibling(n)) && isInlineContentBoundary(nextSignificantSibling(n))
}

func isInlineContentBoundary(n *html.Node) bool {
	if n == nil {
		return false
	}
	if n.Type == html.TextNode {
		return strings.TrimSpace(collapseHTMLWhitespace(n.Data)) != ""
	}
	if n.Type != html.ElementNode {
		return false
	}
	tag := strings.ToLower(n.Data)
	return tag == "a" || isInlinePhrasingTag(tag)
}

func isCompactInlineLinkGroup(n *html.Node) bool {
	if n == nil || n.Parent == nil || n.Parent.Type != html.ElementNode {
		return false
	}
	parentTag := strings.ToLower(n.Parent.Data)
	switch parentTag {
	case "nav", "span":
		return true
	case "div", "p", "td", "th", "li":
	default:
		return false
	}
	if hasAnyClass(n.Parent, "breadcrumb", "breadcrumbs", "crumbs", "path", "pager", "pages", "pagination", "nav", "navbar", "bottom", "list_menu", "loc") {
		return true
	}
	linkCount := 0
	visibleText := 0
	for c := n.Parent.FirstChild; c != nil; c = c.NextSibling {
		switch c.Type {
		case html.TextNode:
			visibleText += len(strings.TrimSpace(c.Data))
		case html.ElementNode:
			tag := strings.ToLower(c.Data)
			if tag == "a" {
				linkCount++
				visibleText += len(strings.TrimSpace(collectText(c)))
				continue
			}
			if !isInlinePhrasingTag(tag) {
				return false
			}
			visibleText += len(strings.TrimSpace(collectText(c)))
		}
		if visibleText > 96 {
			return false
		}
	}
	return linkCount >= 2 && visibleText <= 96
}

func isPhrasingParent(n *html.Node) bool {
	if n == nil || n.Type != html.ElementNode {
		return false
	}
	switch strings.ToLower(n.Data) {
	case "p", "span", "small", "b", "strong", "i", "em", "u", "li", "td", "th", "label":
		return true
	}
	return false
}

func isInlinePhrasingTag(tag string) bool {
	switch strings.ToLower(tag) {
	case "span", "small", "b", "strong", "i", "em", "u", "font", "img", "sup", "sub", "code", "time":
		return true
	}
	return false
}

func shouldInsertInlineGapAfterLink(n *html.Node) bool {
	for ns := n.NextSibling; ns != nil; ns = ns.NextSibling {
		if ns.Type == html.TextNode {
			if strings.TrimSpace(collapseHTMLWhitespace(ns.Data)) == "" {
				if shouldPreserveInlineWhitespace(ns) {
					return false
				}
				continue
			}
			return false
		}
		if ns.Type == html.ElementNode {
			return strings.EqualFold(ns.Data, "a") || isInlinePhrasingTag(ns.Data)
		}
	}
	return false
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
