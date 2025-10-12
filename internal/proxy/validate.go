package proxy

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"io"
)

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

type validateResult struct {
	URL     string      `json:"url"`
	Full    omsAnalysis `json:"full"`
	Compact omsAnalysis `json:"compact"`
}

func analyzeOMS(b []byte) omsAnalysis {
	out := omsAnalysis{Counts: map[string]int{}, V2BE: map[string]uint32{}}
	if len(b) < 6 {
		return out
	}
	out.Magic = binary.LittleEndian.Uint16(b[:2])
	out.Size = binary.BigEndian.Uint32(b[2:6])
	fr := flate.NewReader(bytes.NewReader(b[6:]))
	dec, err := io.ReadAll(fr)
	fr.Close()
	if err != nil {
		return out
	}
	out.DecLen = len(dec)
	last := byte(0)
	if len(dec) > 0 {
		last = dec[len(dec)-1]
	}
	if last >= 32 && last <= 126 {
		out.LastTag = string([]byte{last})
	} else {
		out.LastTag = "."
	}
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
		if len(tags) < maxShow {
			maxShow = len(tags)
		}
		buf := make([]rune, maxShow)
		for i := 0; i < maxShow; i++ {
			t := tags[i]
			if t >= 32 && t <= 126 {
				buf[i] = rune(t)
			} else {
				buf[i] = '.'
			}
		}
		out.TagsHead = string(buf)
		sampleKeys := []byte{'T', 'L', 'E', 'B', '+', 'V', 'D', 'S', 'R', 'k', 'h', 'x', 'p', 'u', 'i', 'b', 'e', 'c', 'r', 's', 'o', 'Q'}
		for _, k := range sampleKeys {
			if v, ok := counts[k]; ok && v > 0 {
				out.Counts[string([]byte{k})] = v
			}
		}
	}
	return out
}

func hexU16(v uint16) string {
	const hexd = "0123456789abcdef"
	return string([]byte{hexd[v>>12&0xF], hexd[v>>8&0xF], hexd[v>>4&0xF], hexd[v&0xF]})
}

func parseTags(dec []byte) (int, []byte, map[byte]int) {
	if len(dec) < 35 {
		return 0, nil, map[byte]int{}
	}
	p := 35
	if p+2 <= len(dec) {
		l := int(binary.BigEndian.Uint16(dec[p : p+2]))
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
		case 'T', 'L':
			if p+2 > limit {
				return len(tags), tags, counts
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
				return len(tags), tags, counts
			}
			dl := int(binary.BigEndian.Uint16(dec[p+4 : p+6]))
			p += 8 + dl
		case 'k':
			p++
			if p+2 > limit {
				return len(tags), tags, counts
			}
			l := int(binary.BigEndian.Uint16(dec[p : p+2]))
			p += 2 + l
		case 'h':
			for i := 0; i < 2; i++ {
				if p+2 > limit {
					return len(tags), tags, counts
				}
				l := int(binary.BigEndian.Uint16(dec[p : p+2]))
				p += 2 + l
			}
		case 'x':
			p++
			for i := 0; i < 2; i++ {
				if p+2 > limit {
					return len(tags), tags, counts
				}
				l := int(binary.BigEndian.Uint16(dec[p : p+2]))
				p += 2 + l
			}
		case 'p', 'u', 'i', 'b', 'e':
			for i := 0; i < 2; i++ {
				if p+2 > limit {
					return len(tags), tags, counts
				}
				l := int(binary.BigEndian.Uint16(dec[p : p+2]))
				p += 2 + l
			}
		case 'c', 'r':
			for i := 0; i < 2; i++ {
				if p+2 > limit {
					return len(tags), tags, counts
				}
				l := int(binary.BigEndian.Uint16(dec[p : p+2]))
				p += 2 + l
			}
			p++
		case 's':
			if p+2 > limit {
				return len(tags), tags, counts
			}
			l := int(binary.BigEndian.Uint16(dec[p : p+2]))
			p += 2 + l
			if p+1 > limit {
				return len(tags), tags, counts
			}
			p++
			if p+2 > limit {
				return len(tags), tags, counts
			}
			p += 2
		case 'o':
			for i := 0; i < 2; i++ {
				if p+2 > limit {
					return len(tags), tags, counts
				}
				l := int(binary.BigEndian.Uint16(dec[p : p+2]))
				p += 2 + l
			}
			p++
		default:
			return len(tags), tags, counts
		}
	}
	return len(tags), tags, counts
}
