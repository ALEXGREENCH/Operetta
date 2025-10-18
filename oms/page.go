package oms

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"encoding/binary"
	"os"
	"strconv"
	"strings"
)

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
	Data          []byte
	CachePacked   []byte
	tagCount      int
	strCount      int
	SetCookies    []string
	partCur       int
	partCnt       int
	clientVersion ClientVersion
	compression   CompressionMethod
	// FormHidden records hidden input fields discovered on the page keyed by form action URL.
	FormHidden map[string]map[string]string
	// NoCache indicates that the page should not be persisted in the render cache.
	NoCache bool
}

// NewPage allocates an empty page.
func NewPage() *Page {
	p := &Page{FormHidden: make(map[string]map[string]string)}
	p.SetTransport(ClientVersion2, CompressionDeflate)
	return p
}

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

// Deprecated: do not use directly; use AddStyle(curStyle | (color<<8)) instead.
func (p *Page) AddTextcolor(color string) {
	p.AddStyle(uint32(calcColor(color)) << 8)
}

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
	safe := normalizeBgForBlackText(color)
	if safe == "" {
		safe = "#1a1a1a"
	}
	safe = ensureMinForRGB565(safe)
	p.addTag('D')
	var buf [2]byte
	binary.BigEndian.PutUint16(buf[:], calcColor(safe))
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

// EndSelect finalises the current select control.
func (p *Page) EndSelect() { p.addTag('l') }

// AddButton adds a push button control.
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

// SetTransport configures OMS protocol version and payload compression.
func (p *Page) SetTransport(version ClientVersion, compression CompressionMethod) {
	p.clientVersion = normalizeClientVersion(version)
	p.compression = normalizeCompression(compression)
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
	if p.clientVersion == 0 {
		p.clientVersion = ClientVersion2
	}
	if p.compression == 0 {
		p.compression = CompressionDeflate
	}
	// Append end-of-page marker like the C implementation (avoid duplicates)
	if len(p.Data) == 0 || p.Data[len(p.Data)-1] != 'Q' {
		p.addTag('Q')
	}
	// Derive TagCount by scanning payload to avoid mismatches
	baseTags, baseStrings := analyzePayloadCounts(p.Data)
	cnt := adjustTagCount(baseTags)
	// If no override set, bump by +1 to avoid OM2 AIOOBE on some pages
	if os.Getenv("OMS_TAGCOUNT_MODE") == "" && os.Getenv("OMS_TAGCOUNT_DELTA") == "" {
		cnt++
	}

	// Derive string count and use swapped value for robust client handling
	stag := baseStrings + 1
	p.tagCount = baseTags
	p.strCount = baseStrings

	swap16 := func(v int) uint16 {
		x := uint16(v & 0xFFFF)
		return (x<<8)&0xFF00 | (x>>8)&0x00FF
	}

	pc := 1
	pt := 1
	if p.partCur > 0 {
		pc = p.partCur
	}
	if p.partCnt > 0 {
		pt = p.partCnt
	}

	var pre bytes.Buffer
	if p.clientVersion == ClientVersion1 {
		type v1Header struct {
			Res1        [9]uint16
			TagCount    uint16
			PartCurrent uint16
			PartCount   uint16
			Res2        uint16
			StagCount   uint16
			Res3        uint16
			Res4        uint8
			Cachable    uint16
		}
		v1 := v1Header{
			TagCount:    swap16(cnt),
			PartCurrent: swap16(pc),
			PartCount:   swap16(pt),
			StagCount:   swap16(stag),
			Cachable:    0xFFFF,
		}
		_ = binary.Write(&pre, binary.LittleEndian, &v1)
	} else {
		v2 := v2Header{
			TagCount:    swap16(cnt),
			PartCurrent: swap16(pc),
			PartCount:   swap16(pt),
			StagCount:   swap16(stag),
			Cachable:    0xFFFF,
		}
		_ = binary.Write(&pre, binary.LittleEndian, &v2)
	}
	_, _ = pre.Write(p.Data)

	rawBody := pre.Bytes()
	var payload []byte
	switch normalizeCompression(p.compression) {
	case CompressionNone:
		payload = append(payload, rawBody...)
	case CompressionGzip:
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		if _, err := gw.Write(rawBody); err != nil {
			payload = append(payload, rawBody...)
		} else {
			_ = gw.Close()
			payload = buf.Bytes()
		}
	default:
		var buf bytes.Buffer
		fw, err := flate.NewWriter(&buf, flate.DefaultCompression)
		if err != nil {
			payload = append(payload, rawBody...)
		} else {
			if _, err := fw.Write(rawBody); err != nil {
				payload = append(payload, rawBody...)
			} else {
				_ = fw.Close()
				payload = buf.Bytes()
			}
		}
	}

	size := 6 + len(payload)
	header := make([]byte, 6)
	headerWord := uint16(versionHeaderByte(p.clientVersion)) | uint16(compressionHeaderByte(p.compression))<<8
	binary.LittleEndian.PutUint16(header[:2], headerWord)
	binary.BigEndian.PutUint32(header[2:], uint32(size))
	p.Data = append(header, payload...)
	// Preserve pre-set packed full-page if caller prepared it earlier.
	// Otherwise, default to caching the finalized data.
	if len(p.CachePacked) == 0 {
		p.CachePacked = append([]byte(nil), p.Data...)
	}
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
	return base
}

func analyzePayloadCounts(b []byte) (int, int) {
	if len(b) < 2 {
		return 0, 0
	}
	p := 0
	// Skip initial URL string
	l := int(binary.BigEndian.Uint16(b[p : p+2]))
	p += 2 + l
	n := 0
	strings := 1 // initial URL string
	limit := len(b)
	for p < limit {
		tag := b[p]
		n++
		p++
		switch tag {
		case 'T', 'L':
			if p+2 > limit {
				return n, strings
			}
			l := int(binary.BigEndian.Uint16(b[p : p+2]))
			strings++
			p += 2 + l
		case 'E', 'B', '+', 'V', 'Q', 'l':
		case 'D', 'R':
			p += 2
		case 'S':
			p += 4
		case 'J':
			p += 4
		case 'I':
			if p+8 > limit {
				return n, strings
			}
			dl := int(binary.BigEndian.Uint16(b[p+4 : p+6]))
			p += 8 + dl
		case 'k':
			p++
			if p+2 > limit {
				return n, strings
			}
			l := int(binary.BigEndian.Uint16(b[p : p+2]))
			strings++
			p += 2 + l
		case 'h':
			for i := 0; i < 2; i++ {
				if p+2 > limit {
					return n, strings
				}
				l := int(binary.BigEndian.Uint16(b[p : p+2]))
				strings++
				p += 2 + l
			}
		case 'x':
			p++
			for i := 0; i < 2; i++ {
				if p+2 > limit {
					return n, strings
				}
				l := int(binary.BigEndian.Uint16(b[p : p+2]))
				strings++
				p += 2 + l
			}
		case 'p', 'u', 'i', 'b', 'e':
			for i := 0; i < 2; i++ {
				if p+2 > limit {
					return n, strings
				}
				l := int(binary.BigEndian.Uint16(b[p : p+2]))
				strings++
				p += 2 + l
			}
		case 'c', 'r':
			for i := 0; i < 2; i++ {
				if p+2 > limit {
					return n, strings
				}
				l := int(binary.BigEndian.Uint16(b[p : p+2]))
				strings++
				p += 2 + l
			}
			p++
		case 's':
			if p+2 > limit {
				return n, strings
			}
			l := int(binary.BigEndian.Uint16(b[p : p+2]))
			strings++
			p += 2 + l
			if p+1 > limit {
				return n, strings
			}
			p++
			if p+2 > limit {
				return n, strings
			}
			p += 2
		case 'o':
			for i := 0; i < 2; i++ {
				if p+2 > limit {
					return n, strings
				}
				l := int(binary.BigEndian.Uint16(b[p : p+2]))
				strings++
				p += 2 + l
			}
			p++
		default:
			return n, strings
		}
	}
	return n, strings
}

// computeTagCount scans the payload (p.Data) and counts tags conservatively.
// It skips the initial OMS_STRING with page URL and then walks tagged payload.
func computeTagCount(b []byte) int {
	tags, _ := analyzePayloadCounts(b)
	return tags
}

func countStrings(b []byte) int {
	_, strings := analyzePayloadCounts(b)
	return strings
}

// SetPart allows external callers to set pagination metadata on the page.
func (p *Page) SetPart(cur, cnt int) {
	if cur < 0 {
		cur = 0
	}
	if cnt < 0 {
		cnt = 0
	}
	p.partCur = cur
	p.partCnt = cnt
}

// Finalize exposes page finalization for external callers.
// It wraps the internal finalize to build the complete OMS payload.
func (p *Page) Finalize() { p.finalize() }
