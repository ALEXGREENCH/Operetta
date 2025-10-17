package oms

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"strconv"
	//"strings"
)

func decompressPayload(method CompressionMethod, payload []byte) ([]byte, error) {
	switch normalizeCompression(method) {
	case CompressionNone:
		return append([]byte(nil), payload...), nil
	case CompressionGzip:
		gr, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		defer gr.Close()
		return io.ReadAll(gr)
	default:
		fr := flate.NewReader(bytes.NewReader(payload))
		defer fr.Close()
		return io.ReadAll(fr)
	}
}

func compressPayload(method CompressionMethod, payload []byte) ([]byte, error) {
	switch normalizeCompression(method) {
	case CompressionNone:
		return append([]byte(nil), payload...), nil
	case CompressionGzip:
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write(payload); err != nil {
			return nil, err
		}
		if err := zw.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	default:
		var buf bytes.Buffer
		zw, err := flate.NewWriter(&buf, flate.DefaultCompression)
		if err != nil {
			return nil, err
		}
		if _, err := zw.Write(payload); err != nil {
			return nil, err
		}
		if err := zw.Close(); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
}

// NormalizeOMS adjusts an OMS response bytes.
func NormalizeOMS(b []byte) ([]byte, error) {
	if len(b) < 6 {
		return b, nil
	}
	headerWord := binary.LittleEndian.Uint16(b[:2])
	version := clientVersionFromHeaderByte(byte(headerWord & 0xFF))
	compression := compressionFromHeaderByte(byte(headerWord >> 8))
	dec, err := decompressPayload(compression, b[6:])
	if err != nil {
		return b, nil
	}
	headerLen := 35
	if version == ClientVersion1 {
		headerLen = 33
	}
	if len(dec) < headerLen {
		return b, nil
	}
	if dec[len(dec)-1] != 'Q' {
		dec = append(dec, 'Q')
	}

	parsed := parseTagCountFromDec(dec, headerLen)
	if parsed < 1 {
		parsed = 1
	}
	wantCnt := parsed + 1
	swap := func(v uint16) uint16 { return (v<<8)&0xFF00 | (v>>8)&0x00FF }
	binary.LittleEndian.PutUint16(dec[18:20], swap(uint16(wantCnt)))
	binary.LittleEndian.PutUint16(dec[26:28], swap(uint16(0x0400)))

	encoded, err := compressPayload(compression, dec)
	if err != nil {
		return b, nil
	}
	size := 6 + len(encoded)
	header := make([]byte, 6)
	binary.LittleEndian.PutUint16(header[:2], headerWord)
	binary.BigEndian.PutUint32(header[2:], uint32(size))
	out := append(header, encoded...)
	return out, nil
}

// NormalizeOMSWithStag adjusts an OMS response bytes and sets stag_count to the provided value.
// stag is written as a swapped little-endian field.
func NormalizeOMSWithStag(b []byte, stag int) ([]byte, error) {
	if len(b) < 6 {
		return b, nil
	}
	headerWord := binary.LittleEndian.Uint16(b[:2])
	version := clientVersionFromHeaderByte(byte(headerWord & 0xFF))
	compression := compressionFromHeaderByte(byte(headerWord >> 8))
	dec, err := decompressPayload(compression, b[6:])
	if err != nil {
		return b, nil
	}
	headerLen := 35
	if version == ClientVersion1 {
		headerLen = 33
	}
	if len(dec) < headerLen {
		return b, nil
	}
	if dec[len(dec)-1] != 'Q' {
		dec = append(dec, 'Q')
	}
	parsed := parseTagCountFromDec(dec, headerLen)
	if parsed < 1 {
		parsed = 1
	}
	wantCnt := parsed + 1
	swap := func(v uint16) uint16 { return (v<<8)&0xFF00 | (v>>8)&0x00FF }
	binary.LittleEndian.PutUint16(dec[18:20], swap(uint16(wantCnt)))
	if stag < 0 {
		stag = 0
	}
	binary.LittleEndian.PutUint16(dec[26:28], swap(uint16(stag)))
	encoded, err := compressPayload(compression, dec)
	if err != nil {
		return b, nil
	}
	size := 6 + len(encoded)
	header := make([]byte, 6)
	binary.LittleEndian.PutUint16(header[:2], headerWord)
	binary.BigEndian.PutUint32(header[2:], uint32(size))
	out := append(header, encoded...)
	return out, nil
}

// SelectOMSPartFromPacked returns a selected part from a packed OMS payload.
func SelectOMSPartFromPacked(data []byte, page, maxTags int) ([]byte, int, int, error) {
	if page <= 0 {
		page = 1
	}
	if maxTags <= 0 {
		return data, 1, 1, nil
	}
	if len(data) < 6 {
		return data, 1, 1, io.ErrUnexpectedEOF
	}
	headerWord := binary.LittleEndian.Uint16(data[:2])
	version := clientVersionFromHeaderByte(byte(headerWord & 0xFF))
	compression := compressionFromHeaderByte(byte(headerWord >> 8))
	decoded, err := decompressPayload(compression, data[6:])
	if err != nil {
		return data, 1, 1, err
	}
	headerLen := 35
	if version == ClientVersion1 {
		headerLen = 33
	}
	if len(decoded) < headerLen {
		return data, 1, 1, io.ErrUnexpectedEOF
	}
	raw := decoded[headerLen:]
	parts := splitByTags(raw, maxTags)
	if len(parts) == 0 {
		return data, 1, 1, nil
	}
	total := len(parts)
	if page > total {
		page = total
	}
	selected := append([]byte(nil), parts[page-1]...)
	// Ensure legacy clients treat parts as distinct pages by rewriting the
	// first OMS string to include a page discriminator.
	if page > 1 {
		selected = rewriteInitialURLRaw(selected, page)
	}
	partPage := NewPage()
	partPage.Data = selected
	partPage.partCur = page
	partPage.partCnt = total
	partPage.SetTransport(version, compression)
	partPage.finalize()
	// Normalize to enforce conservative header fields (e.g., stag_count=0x0400)
	// that some OM 2.x builds expect for pagination to work correctly.
	partPage.Normalize()
	return partPage.Data, page, total, nil
}

// rewriteInitialURLRaw rewrites the very first OMS string ("1/<url>") inside a raw part
// to include a page discriminator so legacy clients (OM 2.x) treat different parts
// as different pages in history/cache. It appends "__p=<page>" as a query parameter.
func rewriteInitialURLRaw(raw []byte, page int) []byte {
	if page <= 1 || len(raw) < 2 {
		return raw
	}
	ln := int(binary.BigEndian.Uint16(raw[0:2]))
	if 2+ln > len(raw) {
		return raw
	}
	s := string(raw[2 : 2+ln])
	if len(s) < 2 || s[0] != '1' || s[1] != '/' {
		return raw
	}
	base := s[2:]
	sep := "?"
	for i := 0; i < len(base); i++ {
		if base[i] == '?' {
			sep = "&"
			break
		}
	}
	// Build new string
	nb := []byte("1/" + base + sep + "__p=" + strconv.Itoa(page))
	out := make([]byte, 2+len(nb))
	binary.BigEndian.PutUint16(out[0:2], uint16(len(nb)))
	copy(out[2:], nb)
	out = append(out, raw[2+ln:]...)
	return out
}

// SelectOMSPartFromPackedWithNav selects a part from a packed OMS payload and
// injects navigation (top and bottom) similar to classic WAP portals.
// It preserves render options that affect output via URL parameters to keep
// cache keys stable between clicks.
func SelectOMSPartFromPackedWithNav(data []byte, page, maxTags int, serverBase, target string, opts *RenderOptions) ([]byte, int, int, error) {
	if page <= 0 {
		page = 1
	}
	if maxTags <= 0 {
		return data, 1, 1, nil
	}
	if len(data) < 6 {
		return data, 1, 1, io.ErrUnexpectedEOF
	}

	headerWord := binary.LittleEndian.Uint16(data[:2])
	version := clientVersionFromHeaderByte(byte(headerWord & 0xFF))
	compression := compressionFromHeaderByte(byte(headerWord >> 8))
	decoded, err := decompressPayload(compression, data[6:])
	if err != nil {
		return data, 1, 1, err
	}
	headerLen := 35
	if version == ClientVersion1 {
		headerLen = 33
	}
	if len(decoded) < headerLen {
		return data, 1, 1, io.ErrUnexpectedEOF
	}
	raw := decoded[headerLen:]
	parts := splitByTags(raw, maxTags)
	if len(parts) == 0 {
		return data, 1, 1, nil
	}
	total := len(parts)
	if page > total {
		page = total
	}
	selected := append([]byte(nil), parts[page-1]...)

	// Rewrite initial URL for unique client history/cache.
	if page > 1 {
		selected = rewriteInitialURLRaw(selected, page)
	}

	// Build navigation controls
	rp := defaultRenderPrefs()
	if opts != nil {
		rp.ImagesOn = opts.ImagesOn
		rp.HighQuality = opts.HighQuality
		rp.ImageMIME = opts.ImageMIME
		rp.MaxInlineKB = opts.MaxInlineKB
		rp.ScreenW = opts.ScreenW
		rp.ScreenH = opts.ScreenH
		rp.AuthCode = opts.AuthCode
		rp.AuthPrefix = opts.AuthPrefix
		rp.GatewayVersion = opts.GatewayVersion
		rp.ClientVersion = opts.ClientVersion
	}
	buildNav := func(cur, total int) []byte {
		nav := NewPage()
		// Compact, finger-friendly: [<<] [<] 1 2 … N [>] [>>]
		nav.AddHr("")
		// Prev block
		if cur > 1 {
			prevURL := serverBase + "/fetch?" + BuildPaginationQuery(target, &rp, cur-1, maxTags)
			firstURL := serverBase + "/fetch?" + BuildPaginationQuery(target, &rp, 1, maxTags)
			nav.AddLink("0/"+firstURL, "[<<]")
			nav.AddText(" ")
			nav.AddLink("0/"+prevURL, "[<]")
		} else {
			nav.AddText("[<<] [<]")
		}
		nav.AddText(" ")
		// Numeric page list (window around current + ends)
		pageSet := map[int]struct{}{}
		add := func(n int) {
			if n >= 1 && n <= total {
				pageSet[n] = struct{}{}
			}
		}
		for i := 1; i <= 3; i++ {
			add(i)
		}
		for i := cur - 2; i <= cur+2; i++ {
			add(i)
		}
		for i := total - 2; i <= total; i++ {
			add(i)
		}
		var ordered []int
		for n := range pageSet {
			ordered = append(ordered, n)
		}
		sort.Ints(ordered)
		last := 0
		for _, n := range ordered {
			if last != 0 && n-last > 1 {
				nav.AddText("…")
			}
			label := fmt.Sprintf("[%d]", n)
			if n == cur {
				nav.AddText("·" + label) // mark current
			} else {
				var url string
				if n == 1 {
					url = "0/" + target
				} else {
					url = "0/" + serverBase + "/fetch?" + BuildPaginationQuery(target, &rp, n, maxTags)
				}
				nav.AddLink(url, label)
			}
			nav.AddText(" ")
			last = n
		}
		// Next block
		if cur < total {
			nextURL := serverBase + "/fetch?" + BuildPaginationQuery(target, &rp, cur+1, maxTags)
			lastURL := serverBase + "/fetch?" + BuildPaginationQuery(target, &rp, total, maxTags)
			nav.AddLink("0/"+nextURL, "[>]")
			nav.AddText(" ")
			nav.AddLink("0/"+lastURL, "[>>]")
		} else {
			nav.AddText("[>] [>>]")
		}
		nav.AddBreak()
		nav.AddText(fmt.Sprintf("Page %d/%d", cur, total))
		nav.AddBreak()
		// Quick jump form
		/*
			formAction := "0/" + serverBase + "/fetch"
			nav.AddForm(formAction)
			nav.AddHidden("url", target)
			nav.AddHidden("pp", strconv.Itoa(maxTags))
			if rp.ImagesOn {
				nav.AddHidden("img", "1")
			}
			if rp.HighQuality {
				nav.AddHidden("hq", "1")
			}
			if rp.ImageMIME != "" {
				nav.AddHidden("mime", rp.ImageMIME)
			}
			if rp.MaxInlineKB > 0 {
				nav.AddHidden("maxkb", strconv.Itoa(rp.MaxInlineKB))
			}
			if rp.ScreenW > 0 {
				nav.AddHidden("w", strconv.Itoa(rp.ScreenW))
			}
			if rp.ScreenH > 0 {
				nav.AddHidden("h", strconv.Itoa(rp.ScreenH))
			}
			if strings.TrimSpace(rp.AuthCode) != "" {
				nav.AddHidden("c", rp.AuthCode)
			}
			if strings.TrimSpace(rp.AuthPrefix) != "" {
				nav.AddHidden("h", rp.AuthPrefix)
			}
			if rp.GatewayVersion > 0 {
				nav.AddHidden("o", strconv.Itoa(rp.GatewayVersion))
			}
			switch normalizeClientVersion(rp.ClientVersion) {
			case ClientVersion1:
				nav.AddHidden("version", "1")
			case ClientVersion3:
				nav.AddHidden("version", "3")
			}
			nav.AddTextInput("page", "")
			nav.AddText(" ")
			nav.AddSubmit("go", "OK")
			nav.AddHr("")
		*/
		return nav.Data
	}
	// Build top and bottom navs
	topNav := buildNav(page, total)
	bottomNav := buildNav(page, total)

	// Respect page byte budget (~32KB): shrink content first
	budget := maxBytesBudget()
	allowed := budget - (len(topNav) + len(bottomNav))
	if allowed < 1024 {
		allowed = 1024
	}
	selected = shrinkPartToMaxBytes(selected, allowed)

	// Insert top nav after initial string and prelude tags ('S','D','k')
	// Compute splice position
	pos := 0
	if len(selected) >= 2 {
		l := int(binary.BigEndian.Uint16(selected[0:2]))
		pos = 2 + l
		for pos < len(selected) {
			tag := selected[pos]
			pos++
			switch tag {
			case 'S':
				if pos+4 > len(selected) {
					pos = len(selected)
					break
				}
				pos += 4
			case 'D':
				if pos+2 > len(selected) {
					pos = len(selected)
					break
				}
				pos += 2
			case 'k':
				if pos+1 > len(selected) {
					pos = len(selected)
					break
				}
				pos += 1
				if pos+2 > len(selected) {
					pos = len(selected)
					break
				}
				ln := int(binary.BigEndian.Uint16(selected[pos : pos+2]))
				pos += 2 + ln
			default:
				pos--
				goto Splice
			}
		}
	}
Splice:
	withTop := make([]byte, 0, len(selected)+len(topNav)+len(bottomNav))
	withTop = append(withTop, selected[:pos]...)
	withTop = append(withTop, topNav...)
	withTop = append(withTop, selected[pos:]...)
	withTop = append(withTop, bottomNav...)

	// Finalize
	partPage := NewPage()
	partPage.Data = withTop
	partPage.partCur = page
	partPage.partCnt = total
	partPage.SetTransport(version, compression)
	partPage.finalize()
	partPage.Normalize()
	return partPage.Data, page, total, nil
}

// parseTagCountFromDec walks the inflated stream (starting with V2 header) and returns number of tags.
func parseTagCountFromDec(dec []byte, headerLen int) int {
	if len(dec) < headerLen {
		return 0
	}
	p := headerLen
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
		case 'T', 'L':
			if p+2 > limit {
				return n
			}
			l := int(binary.BigEndian.Uint16(dec[p : p+2]))
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
				return n
			}
			dl := int(binary.BigEndian.Uint16(dec[p+4 : p+6]))
			p += 8 + dl
		case 'k':
			p++
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
			p++
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
			p++
		case 's':
			if p+2 > limit {
				return n
			}
			l := int(binary.BigEndian.Uint16(dec[p : p+2]))
			p += 2 + l
			if p+1 > limit {
				return n
			}
			p++
			if p+2 > limit {
				return n
			}
			p += 2
		case 'o':
			for i := 0; i < 2; i++ {
				if p+2 > limit {
					return n
				}
				l := int(binary.BigEndian.Uint16(dec[p : p+2]))
				p += 2 + l
			}
			p++
		default:
			return n
		}
	}
	return n
}
