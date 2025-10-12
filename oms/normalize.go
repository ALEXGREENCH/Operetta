package oms

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"encoding/binary"
	"io"
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
	partPage := NewPage()
	partPage.Data = selected
	partPage.partCur = page
	partPage.partCnt = total
	partPage.SetTransport(version, compression)
	partPage.finalize()
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
