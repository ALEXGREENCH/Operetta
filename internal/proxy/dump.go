package proxy

import (
	"encoding/hex"
	"log"
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
