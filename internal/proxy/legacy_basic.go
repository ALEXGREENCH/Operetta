package proxy

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"strings"

	"operetta/oms"
)

const legacyBasicOM2HeaderBytes = 35

// isLegacyBasicOM2 recognizes the original Opera Mini 2.0 transport used by
// the small "woodland" builds. Those clients advertise o=24 and predate the
// 6-byte compression/length envelope used by later/modded 2.x builds (which
// normally advertise o=280).
func isLegacyBasicOM2(params map[string]string) bool {
	if strings.TrimSpace(params["o"]) != "24" {
		return false
	}
	version := strings.ToLower(strings.TrimSpace(params["v"]))
	return version == "" || strings.Contains(version, "opera mini/2.")
}

// legacyBasicImageMIME mirrors the original client capability probe. Basic
// sends k=image/jpeg only when its startup JPEG test succeeds; otherwise its
// image decoder accepts the mandatory MIDP PNG path only.
func legacyBasicImageMIME(params map[string]string) string {
	if strings.EqualFold(strings.TrimSpace(params["k"]), "image/jpeg") {
		return "image/jpeg"
	}
	return "image/png"
}

func applyLegacyBasicRenderProfile(opt *oms.RenderOptions, params map[string]string) {
	if opt == nil {
		return
	}
	opt.ImagesOn = true
	opt.ImageMIME = legacyBasicImageMIME(params)
	opt.MaxInlineKB = 8
	opt.DialectID = "om2-basic"
	opt.LegacyBasicOM2 = true
	opt.Compression = oms.CompressionNone
	// Server-side association is resolved before rendering. These synthetic
	// records are only understood by later clients.
	opt.AuthCode = ""
	opt.AuthPrefix = ""
}

// legacyBasicOM2Frame converts Operetta's normal OM2 wire frame into the
// uncompressed transport consumed by the original 2.0.4509 Basic client:
//
//	0x18 0x33 <be32 total-size> <35-byte OMS v2 page header> <UTF URL> ...
//
// c(InputStream,true) consumes 0x18 as H. The first c.a() control record then
// consumes '3' (0x33) plus the 32-bit frame size and stops the pre-header loop.
// q() subsequently consumes exactly the 35-byte OMS v2 page header.
func legacyBasicOM2Frame(data []byte) ([]byte, error) {
	if len(data) < 6 {
		return nil, fmt.Errorf("legacy OM2: short frame: %d", len(data))
	}
	if data[0] != 0x18 {
		return nil, fmt.Errorf("legacy OM2: unexpected version byte 0x%02x", data[0])
	}
	declared := int(binary.BigEndian.Uint32(data[2:6]))
	if declared != 0 && declared != len(data) {
		return nil, fmt.Errorf("legacy OM2: size mismatch header=%d actual=%d", declared, len(data))
	}

	var raw []byte
	switch oms.CompressionMethod(data[1]) {
	case oms.CompressionNone:
		raw = append([]byte(nil), data[6:]...)
	case oms.CompressionDeflate:
		fr := flate.NewReader(bytes.NewReader(data[6:]))
		decoded, err := io.ReadAll(fr)
		closeErr := fr.Close()
		if err != nil {
			return nil, fmt.Errorf("legacy OM2: inflate: %w", err)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("legacy OM2: inflate close: %w", closeErr)
		}
		raw = decoded
	case oms.CompressionGzip:
		gr, err := gzip.NewReader(bytes.NewReader(data[6:]))
		if err != nil {
			return nil, fmt.Errorf("legacy OM2: gzip header: %w", err)
		}
		decoded, readErr := io.ReadAll(gr)
		closeErr := gr.Close()
		if readErr != nil {
			return nil, fmt.Errorf("legacy OM2: gzip: %w", readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("legacy OM2: gzip close: %w", closeErr)
		}
		raw = decoded
	default:
		return nil, fmt.Errorf("legacy OM2: unsupported compression marker 0x%02x", data[1])
	}

	if len(raw) < legacyBasicOM2HeaderBytes {
		return nil, fmt.Errorf("legacy OM2: short decoded body: %d", len(raw))
	}

	out := make([]byte, 6+len(raw))
	out[0] = 0x18
	out[1] = byte(oms.CompressionNone)
	binary.BigEndian.PutUint32(out[2:6], uint32(len(out)))
	copy(out[6:], raw)
	return out, nil
}

func renderLegacyBasicSmokePage() *oms.Page {
	page := oms.NewPage()
	page.SetTransport(oms.ClientVersion2, oms.CompressionNone)
	page.AddString("1/http://opera-mini.ru/basic-smoke")
	page.AddStyle(oms.StyleDefault)
	page.AddPlus()
	page.AddText("OM2 BASIC OK")
	page.AddBreak()
	page.Finalize()
	return page
}

func (s *Server) writeOMSForClient(w http.ResponseWriter, data []byte, cookies []string, stats *oms.TrafficStats, legacyBasic bool) {
	if !legacyBasic {
		s.writeOMS(w, data, cookies, stats)
		return
	}
	framed, err := legacyBasicOM2Frame(data)
	if err != nil {
		if s.logger != nil {
			s.logger.Printf("legacy OM2 framing fallback: %v", err)
		}
		s.writeOMS(w, data, cookies, stats)
		return
	}
	var statsCopy *oms.TrafficStats
	if stats != nil {
		copyValue := *stats
		statsCopy = &copyValue
	}
	if s.logger != nil {
		s.logger.Printf("legacy OM2 Basic transport: in=%d out=%d framing=33-none header=35", len(data), len(framed))
	}
	s.writeOMS(w, framed, cookies, statsCopy)
}
