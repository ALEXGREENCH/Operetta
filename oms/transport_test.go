package oms

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"
)

func TestPageFinalizeEncodings(t *testing.T) {
	combos := []struct {
		name        string
		version     ClientVersion
		compression CompressionMethod
	}{
		{"om1-deflate", ClientVersion1, CompressionDeflate},
		{"om2-deflate", ClientVersion2, CompressionDeflate},
		{"om3-deflate", ClientVersion3, CompressionDeflate},
		{"om3-gzip", ClientVersion3, CompressionGzip},
		{"om3-none", ClientVersion3, CompressionNone},
	}
	for _, combo := range combos {
		t.Run(combo.name, func(t *testing.T) {
			p := NewPage()
			p.AddString("1/http://example.com/")
			p.AddText("Hello")
			p.SetTransport(combo.version, combo.compression)
			p.finalize()

			word := binary.LittleEndian.Uint16(p.Data[:2])
			if got := byte(word & 0xFF); got != versionHeaderByte(combo.version) {
				t.Fatalf("version byte mismatch: got 0x%02x want 0x%02x", got, versionHeaderByte(combo.version))
			}
			if got := byte(word >> 8); got != compressionHeaderByte(combo.compression) {
				t.Fatalf("compression byte mismatch: got 0x%02x want 0x%02x", got, compressionHeaderByte(combo.compression))
			}

			decoded, err := decompressPayload(combo.compression, p.Data[6:])
			if err != nil {
				t.Fatalf("decompressPayload: %v", err)
			}
			headerLen := 35
			if combo.version == ClientVersion1 {
				headerLen = 33
			}
			if len(decoded) < headerLen {
				t.Fatalf("decoded payload too short: len=%d", len(decoded))
			}
			if decoded[len(decoded)-1] != 'Q' {
				t.Fatalf("expected last tag 'Q', got 0x%02x", decoded[len(decoded)-1])
			}
		})
	}
}

func TestSelectOMSPartFromPacked(t *testing.T) {
	basePage := NewPage()
	basePage.AddString("1/http://example.com/")
	for i := 0; i < 6; i++ {
		basePage.AddText(fmt.Sprintf("Item-%d", i))
		basePage.AddBreak()
	}
	raw := append([]byte(nil), basePage.Data...)
	maxTags := 3
	parts := splitByTags(raw, maxTags, ClientVersion2)
	if len(parts) < 2 {
		t.Fatalf("expected multiple parts, got %d", len(parts))
	}

	// Finalize first part as the server would do.
	first := NewPage()
	first.Data = append([]byte(nil), parts[0]...)
	first.partCur = 1
	first.partCnt = len(parts)
	first.SetTransport(ClientVersion2, CompressionDeflate)
	first.finalize()

	partData, cur, total, err := SelectOMSPartFromPacked(first.Data, 2, maxTags)
	if err != nil {
		t.Fatalf("SelectOMSPartFromPacked: %v", err)
	}
	if cur != 2 {
		t.Fatalf("unexpected current part %d", cur)
	}
	decodedFirst, err := decompressPayload(CompressionDeflate, first.Data[6:])
	if err != nil {
		t.Fatalf("decompress first: %v", err)
	}
	rawAfter := decodedFirst[35:]
	partsAfter := splitByTags(rawAfter, maxTags, ClientVersion2)
	if total != len(partsAfter) {
		t.Fatalf("unexpected total parts: got %d want %d", total, len(partsAfter))
	}

	selected := append([]byte(nil), partsAfter[1]...)
	selected = rewriteInitialURLRaw(selected, 2)
	expected := NewPage()
	expected.Data = selected
	expected.partCur = 2
	expected.partCnt = len(partsAfter)
	expected.SetTransport(ClientVersion2, CompressionDeflate)
	expected.finalize()
	expected.Normalize()

	if !bytes.Equal(partData, expected.Data) {
		t.Fatalf("part data mismatch len(part)=%d len(expected)=%d",
			len(partData), len(expected.Data))
	}
}
