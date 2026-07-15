package oms

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAddStringCapsUint16LengthWithoutBreakingUTF8(t *testing.T) {
	p := NewPage()
	p.AddString(strings.Repeat("я", 40000))
	if len(p.Data) < 2 {
		t.Fatal("missing encoded string")
	}
	encodedLen := int(binary.BigEndian.Uint16(p.Data[:2]))
	if encodedLen > 0xffff || encodedLen != len(p.Data)-2 {
		t.Fatalf("encoded length=%d payload=%d", encodedLen, len(p.Data)-2)
	}
	if !utf8.Valid(p.Data[2:]) {
		t.Fatal("truncation split a UTF-8 sequence")
	}
}

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
	selected = rewriteInitialURLRaw(selected, 2, maxTags)
	initialLen := int(binary.BigEndian.Uint16(selected[:2]))
	initialURL := string(selected[2 : 2+initialLen])
	if !strings.Contains(initialURL, "__om=page%3D2%26pp%3D3") {
		t.Fatalf("pagination state missing from initial URL %q", initialURL)
	}
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

func TestCachedPartNavigationPrecedesSingleEndMarker(t *testing.T) {
	page := NewPage()
	page.SetTransport(ClientVersion3, CompressionNone)
	page.AddString("1/https://example.test/large")
	for i := 0; i < 32; i++ {
		page.AddText(strings.Repeat("payload", 600))
		page.AddBreak()
	}
	page.finalize()
	opts := &RenderOptions{
		ClientVersion:   ClientVersion3,
		Compression:     CompressionNone,
		MaxTagsPerPage:  700,
		MaxBytesPerPage: 96 * 1024,
		ServerBase:      "http://operetta.test",
	}
	encoded, current, total, err := SelectOMSPartFromPackedWithNav(
		page.CachePacked, 2, opts.MaxTagsPerPage, opts.ServerBase,
		"https://example.test/large", opts,
	)
	if err != nil {
		t.Fatalf("SelectOMSPartFromPackedWithNav: %v", err)
	}
	if current != 2 || total < 2 {
		t.Fatalf("part=%d/%d, want cached second part", current, total)
	}
	decoded, err := decompressPayload(CompressionNone, encoded[6:])
	if err != nil {
		t.Fatalf("decompress cached part: %v", err)
	}
	tokens := tokensFromRawPart(t, decoded[35:], ClientVersion3)
	endMarkers := 0
	for index, token := range tokens {
		if token.tag == 'Q' {
			endMarkers++
			if index != len(tokens)-1 {
				t.Fatalf("Q at token %d/%d before injected navigation", index, len(tokens))
			}
		}
	}
	if endMarkers != 1 {
		t.Fatalf("end markers=%d, want exactly one", endMarkers)
	}
}

func TestSplitByTagsKeepsLinkBlocksBalanced(t *testing.T) {
	basePage := NewPage()
	basePage.AddString("1/http://example.com/")
	for i := 0; i < 5; i++ {
		basePage.AddLink(fmt.Sprintf("0/http://example.com/%d", i), fmt.Sprintf("Link %d", i))
	}

	parts := splitByTags(append([]byte(nil), basePage.Data...), 2, ClientVersion2)
	if len(parts) < 2 {
		t.Fatalf("expected multiple parts, got %d", len(parts))
	}
	for i, part := range parts {
		assertBalancedLinksInRawPart(t, part, ClientVersion2, fmt.Sprintf("part %d", i+1))
	}
}

func TestShrinkPartToMaxBytesDoesNotLeaveOpenLink(t *testing.T) {
	basePage := NewPage()
	basePage.AddString("1/http://example.com/")
	basePage.AddText("before")
	basePage.AddBreak()
	basePage.AddLink("0/http://example.com/long", strings.Repeat("long text ", 20))
	basePage.AddText("after")

	raw := append([]byte(nil), basePage.Data...)
	prefixLen := 2 + len("1/http://example.com/")
	urlLen := len("0/http://example.com/long")
	limitInsideLink := prefixLen + 1 + 2 + urlLen
	shrunk := shrinkPartToMaxBytes(raw, limitInsideLink, ClientVersion2)

	assertBalancedLinksInRawPart(t, shrunk, ClientVersion2, "shrunk")
	for _, tok := range tokensFromRawPart(t, shrunk, ClientVersion2) {
		if tok.tag == 'L' {
			t.Fatalf("expected shrink to drop an incomplete link, tokens=%v", tokensFromRawPart(t, shrunk, ClientVersion2))
		}
	}
}

func assertBalancedLinksInRawPart(t *testing.T, raw []byte, version ClientVersion, label string) {
	t.Helper()
	depth := 0
	for _, tok := range tokensFromRawPart(t, raw, version) {
		switch tok.tag {
		case 'L':
			depth++
		case 'E':
			depth--
			if depth < 0 {
				t.Fatalf("%s has unmatched E tag, tokens=%v", label, tokensFromRawPart(t, raw, version))
			}
		}
	}
	if depth != 0 {
		t.Fatalf("%s has unclosed link depth %d, tokens=%v", label, depth, tokensFromRawPart(t, raw, version))
	}
}

func tokensFromRawPart(t *testing.T, raw []byte, version ClientVersion) []obmlToken {
	t.Helper()
	if len(raw) < 2 {
		t.Fatalf("raw part too short: %d", len(raw))
	}
	initialLen := int(binary.BigEndian.Uint16(raw[:2]))
	bodyStart := 2 + initialLen
	if bodyStart > len(raw) {
		t.Fatalf("invalid initial string length %d for raw len %d", initialLen, len(raw))
	}
	tokens, err := parseTokens(raw[bodyStart:], version)
	if err != nil {
		t.Fatalf("parseTokens: %v", err)
	}
	return tokens
}

func TestLooksLikeOMS(t *testing.T) {
	// Build a valid OMS payload for testing.
	validOMS := func(version ClientVersion, compression CompressionMethod) []byte {
		p := NewPage()
		p.AddString("1/http://example.com/")
		p.AddText("Hello")
		p.SetTransport(version, compression)
		p.finalize()
		return p.Data
	}

	tests := []struct {
		name  string
		input []byte
		want  bool
	}{
		{"valid om2-deflate", validOMS(ClientVersion2, CompressionDeflate), true},
		{"valid om3-deflate", validOMS(ClientVersion3, CompressionDeflate), true},
		{"valid om1-deflate", validOMS(ClientVersion1, CompressionDeflate), true},
		{"valid om3-gzip", validOMS(ClientVersion3, CompressionGzip), true},
		{"valid om3-none", validOMS(ClientVersion3, CompressionNone), true},
		{"too_short", []byte{0x18, 0x00, 0x00, 0x00, 0x00, 0x00}, false},
		{"bad_version_byte", append([]byte{0xFF, 0x00}, validOMS(ClientVersion2, CompressionDeflate)[2:]...), false},
		{"random_html", []byte("<html><body>Hello</body></html>"), false},
		{"empty", []byte{}, false},
		{"nil", nil, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := looksLikeOMS(tc.input)
			if got != tc.want {
				t.Fatalf("looksLikeOMS(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
