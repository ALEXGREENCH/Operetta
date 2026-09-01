package proxy

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"io"
	"strings"

	"operetta/oms"
)

func headerByteToClientVersion(b byte) oms.ClientVersion {
	switch b {
	case 0x0d:
		return oms.ClientVersion1
	case 0x1a:
		return oms.ClientVersion3
	default:
		return oms.ClientVersion2
	}
}

type omsAnalysis struct {
	Magic      uint16             `json:"magic"`
	Size       uint32             `json:"size"`
	DecLen     int                `json:"decLen"`
	LastTag    string             `json:"lastTag"`
	V2BE       map[string]uint32  `json:"v2be"`
	TagCountSw string             `json:"tagCountSw"`
	ParsedTags int                `json:"parsedTags"`
	ParseOK    bool               `json:"parseOk"`
	ParseError string             `json:"parseError,omitempty"`
	TagsHead   string             `json:"tagsHead"`
	Counts     map[string]int     `json:"counts"`
	Summary    omsSemanticSummary `json:"summary"`
}

type omsSemanticSummary struct {
	InitialURL string           `json:"initialUrl,omitempty"`
	Links      []string         `json:"links,omitempty"`
	Text       []string         `json:"text,omitempty"`
	Forms      []omsFormSummary `json:"forms,omitempty"`
}

type omsFormSummary struct {
	Action         string            `json:"action,omitempty"`
	Method         string            `json:"method,omitempty"`
	Hidden         map[string]string `json:"hidden,omitempty"`
	TextInputs     []string          `json:"textInputs,omitempty"`
	PasswordInputs []string          `json:"passwordInputs,omitempty"`
	Selects        []string          `json:"selects,omitempty"`
	Submits        []string          `json:"submits,omitempty"`
	Checks         int               `json:"checks,omitempty"`
	Radios         int               `json:"radios,omitempty"`
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
	version := headerByteToClientVersion(byte(out.Magic & 0xFF))
	fr := flate.NewReader(bytes.NewReader(b[6:]))
	dec, err := io.ReadAll(fr)
	fr.Close()
	if err != nil {
		out.ParseError = err.Error()
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
	if len(dec) < 35 {
		out.ParseError = "decoded payload too short"
		return out
	}

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

	parsed, tags, counts, summary, parseErr := parseTags(dec, version)
	out.ParsedTags = parsed
	out.ParseOK = parseErr == nil
	if parseErr != nil {
		out.ParseError = parseErr.Error()
	}
	out.Summary = summary

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
	return out
}

func hexU16(v uint16) string {
	const hexd = "0123456789abcdef"
	return string([]byte{hexd[v>>12&0xF], hexd[v>>8&0xF], hexd[v>>4&0xF], hexd[v&0xF]})
}

func parseTags(dec []byte, version oms.ClientVersion) (int, []byte, map[byte]int, omsSemanticSummary, error) {
	if len(dec) < 35 {
		return 0, nil, map[byte]int{}, omsSemanticSummary{}, fmt.Errorf("decoded payload too short")
	}
	p := 35
	summary := omsSemanticSummary{}
	initial, err := readOMSString(dec, &p)
	if err != nil {
		return 0, nil, map[byte]int{}, summary, fmt.Errorf("initial url: %w", err)
	}
	summary.InitialURL = trimOMSPagePrefix(initial)

	tags := make([]byte, 0, 256)
	counts := map[byte]int{}
	limit := len(dec)
	styleDataLen := 4
	colorDataLen := 2
	if version == oms.ClientVersion3 {
		styleDataLen = 6
		colorDataLen = 4
	}
	currentForm := -1

	for p < limit {
		tag := dec[p]
		tags = append(tags, tag)
		counts[tag]++
		p++
		switch tag {
		case 'T':
			s, err := readOMSString(dec, &p)
			if err != nil {
				return len(tags), tags, counts, summary, fmt.Errorf("tag T string: %w", err)
			}
			appendSample(&summary.Text, clipSampleText(s), 12)
		case 'L':
			s, err := readOMSString(dec, &p)
			if err != nil {
				return len(tags), tags, counts, summary, fmt.Errorf("tag L string: %w", err)
			}
			appendSample(&summary.Links, clipSampleText(s), 12)
		case 'E', 'B', '+', 'V', 'Q', 'l', 'C':
		case 'D', 'R':
			if p+colorDataLen > limit {
				return len(tags), tags, counts, summary, fmt.Errorf("tag %q missing %d bytes", tag, colorDataLen)
			}
			p += colorDataLen
		case 'S':
			if p+styleDataLen > limit {
				return len(tags), tags, counts, summary, fmt.Errorf("tag S missing %d bytes", styleDataLen)
			}
			p += styleDataLen
		case 'J':
			if p+4 > limit {
				return len(tags), tags, counts, summary, fmt.Errorf("tag J missing 4 bytes")
			}
			p += 4
		case 'I':
			if p+8 > limit {
				return len(tags), tags, counts, summary, fmt.Errorf("tag I missing header")
			}
			dl := int(binary.BigEndian.Uint16(dec[p+4 : p+6]))
			p += 8
			if p+dl > limit {
				return len(tags), tags, counts, summary, fmt.Errorf("tag I payload overflow")
			}
			p += dl
		case 'k':
			if p >= limit {
				return len(tags), tags, counts, summary, fmt.Errorf("tag k missing type byte")
			}
			p++
			if _, err := readOMSString(dec, &p); err != nil {
				return len(tags), tags, counts, summary, fmt.Errorf("tag k string: %w", err)
			}
		case 'h':
			action, err := readOMSString(dec, &p)
			if err != nil {
				return len(tags), tags, counts, summary, fmt.Errorf("tag h action: %w", err)
			}
			method, err := readOMSString(dec, &p)
			if err != nil {
				return len(tags), tags, counts, summary, fmt.Errorf("tag h method: %w", err)
			}
			summary.Forms = append(summary.Forms, omsFormSummary{
				Action: trimOMSPagePrefix(action),
				Method: method,
			})
			currentForm = len(summary.Forms) - 1
		case 'x':
			if p >= limit {
				return len(tags), tags, counts, summary, fmt.Errorf("tag x missing config byte")
			}
			p++
			name, err := readOMSString(dec, &p)
			if err != nil {
				return len(tags), tags, counts, summary, fmt.Errorf("tag x name: %w", err)
			}
			if _, err := readOMSString(dec, &p); err != nil {
				return len(tags), tags, counts, summary, fmt.Errorf("tag x value: %w", err)
			}
			appendFormField(&summary, currentForm, "text", name, "")
		case 'p', 'u', 'i', 'b', 'e':
			name, err := readOMSString(dec, &p)
			if err != nil {
				return len(tags), tags, counts, summary, fmt.Errorf("tag %q name: %w", tag, err)
			}
			value, err := readOMSString(dec, &p)
			if err != nil {
				return len(tags), tags, counts, summary, fmt.Errorf("tag %q value: %w", tag, err)
			}
			switch tag {
			case 'p':
				appendFormField(&summary, currentForm, "password", name, "")
			case 'u':
				appendFormField(&summary, currentForm, "submit", firstNonEmpty(name, value), "")
			case 'i':
				appendFormField(&summary, currentForm, "hidden", name, value)
			}
		case 'c', 'r':
			if _, err := readOMSString(dec, &p); err != nil {
				return len(tags), tags, counts, summary, fmt.Errorf("tag %q name: %w", tag, err)
			}
			if _, err := readOMSString(dec, &p); err != nil {
				return len(tags), tags, counts, summary, fmt.Errorf("tag %q value: %w", tag, err)
			}
			if p >= limit {
				return len(tags), tags, counts, summary, fmt.Errorf("tag %q missing state byte", tag)
			}
			p++
			appendFormCounter(&summary, currentForm, tag)
		case 's':
			name, err := readOMSString(dec, &p)
			if err != nil {
				return len(tags), tags, counts, summary, fmt.Errorf("tag s name: %w", err)
			}
			if p >= limit {
				return len(tags), tags, counts, summary, fmt.Errorf("tag s missing multiple flag")
			}
			p++
			if p+2 > limit {
				return len(tags), tags, counts, summary, fmt.Errorf("tag s missing count")
			}
			p += 2
			appendFormField(&summary, currentForm, "select", name, "")
		case 'o':
			if _, err := readOMSString(dec, &p); err != nil {
				return len(tags), tags, counts, summary, fmt.Errorf("tag o value: %w", err)
			}
			if _, err := readOMSString(dec, &p); err != nil {
				return len(tags), tags, counts, summary, fmt.Errorf("tag o label: %w", err)
			}
			if p >= limit {
				return len(tags), tags, counts, summary, fmt.Errorf("tag o missing selected flag")
			}
			p++
		default:
			return len(tags), tags, counts, summary, fmt.Errorf("unknown tag %q", tag)
		}
	}
	return len(tags), tags, counts, summary, nil
}

func readOMSString(dec []byte, p *int) (string, error) {
	if *p+2 > len(dec) {
		return "", fmt.Errorf("missing string length")
	}
	l := int(binary.BigEndian.Uint16(dec[*p : *p+2]))
	*p += 2
	if *p+l > len(dec) {
		return "", fmt.Errorf("short string data")
	}
	s := string(dec[*p : *p+l])
	*p += l
	return s, nil
}

func trimOMSPagePrefix(s string) string {
	if strings.HasPrefix(s, "1/") {
		return s[2:]
	}
	return s
}

func clipSampleText(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 96 {
		return s[:96] + "..."
	}
	return s
}

func appendSample(dst *[]string, value string, limit int) {
	value = strings.TrimSpace(value)
	if value == "" || len(*dst) >= limit {
		return
	}
	*dst = append(*dst, value)
}

func appendFormField(summary *omsSemanticSummary, idx int, kind, name, value string) {
	if idx < 0 || idx >= len(summary.Forms) {
		return
	}
	name = strings.TrimSpace(name)
	value = strings.TrimSpace(value)
	form := &summary.Forms[idx]
	switch kind {
	case "hidden":
		if name == "" {
			return
		}
		if form.Hidden == nil {
			form.Hidden = map[string]string{}
		}
		form.Hidden[name] = value
	case "text":
		appendSample(&form.TextInputs, name, 8)
	case "password":
		appendSample(&form.PasswordInputs, name, 8)
	case "select":
		appendSample(&form.Selects, name, 8)
	case "submit":
		appendSample(&form.Submits, firstNonEmpty(name, value), 8)
	}
}

func appendFormCounter(summary *omsSemanticSummary, idx int, tag byte) {
	if idx < 0 || idx >= len(summary.Forms) {
		return
	}
	switch tag {
	case 'c':
		summary.Forms[idx].Checks++
	case 'r':
		summary.Forms[idx].Radios++
	}
}
