package proxy

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"

	"operetta/oms"
)

func dumpOMS(logger *log.Logger, b []byte) {
	if logger == nil || len(b) == 0 {
		return
	}
	size := len(b)
	if size > 256 {
		size = 256
	}
	logger.Printf("OMS %d bytes (first %d shown): %s", len(b), size, hexBlock(b, 0, size))

	if os.Getenv("OMS_DEBUG_SCAN") == "1" {
		if msg := scanOMSForSuspicious(b); msg != "" {
			logger.Print(msg)
		}
	}
}

func hexBlock(b []byte, start, end int) string {
	if start < 0 {
		start = 0
	}
	if end > len(b) {
		end = len(b)
	}
	if start >= end {
		return ""
	}
	segment := b[start:end]
	dst := make([]byte, hex.EncodedLen(len(segment)))
	hex.Encode(dst, segment)
	return string(dst)
}

func scanOMSForSuspicious(b []byte) string {
	if len(b) < 6 {
		return "OMS scan: payload shorter than header"
	}
	headerWord := binary.LittleEndian.Uint16(b[:2])
	versionByte := byte(headerWord & 0xFF)
	compByte := byte(headerWord >> 8)
	raw, err := decompressOMSBody(compByte, b[6:])
	if err != nil {
		return fmt.Sprintf("OMS scan: failed to decompress (comp=0x%02x): %v", compByte, err)
	}

	headerLen := 35
	if oms.ClientVersion(versionByte) == oms.ClientVersion1 {
		headerLen = 33
	}
	if len(raw) < headerLen {
		return fmt.Sprintf("OMS scan: decoded body too short (%d bytes)", len(raw))
	}
	body := raw[headerLen:]
	if len(body) == 0 {
		return "OMS scan: decoded body empty"
	}
	if idx := bytes.IndexByte(body, 0xFF); idx >= 0 {
		start := idx - 8
		if start < 0 {
			start = 0
		}
		end := idx + 8
		if end > len(body) {
			end = len(body)
		}
		return fmt.Sprintf("OMS scan: found 0xFF tag byte at body[%d], window=%s", idx, hexBlock(body, start, end))
	}
	return ""
}

func decompressOMSBody(comp byte, payload []byte) ([]byte, error) {
	switch oms.CompressionMethod(comp) {
	case oms.CompressionNone:
		return append([]byte(nil), payload...), nil
	case oms.CompressionGzip:
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
