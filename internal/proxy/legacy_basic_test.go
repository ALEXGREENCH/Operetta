package proxy

import (
	"encoding/binary"
	"testing"

	"operetta/oms"
)

func TestIsLegacyBasicOM2(t *testing.T) {
	if !isLegacyBasicOM2(map[string]string{
		"o": "24",
		"v": "Opera Mini/2.0.4509/lofi/woodland/int",
		"b": "woodland",
	}) {
		t.Fatal("original OM2 Basic signature was not detected")
	}
	if isLegacyBasicOM2(map[string]string{
		"o": "280",
		"v": "Opera Mini/2.0.4509/hifi/woodland/ru",
		"b": "mod2.06",
		"e": "def",
	}) {
		t.Fatal("Opera Mini Mod 2.06 must keep the normal transport")
	}
}

func TestLegacyBasicImageMIMEFollowsClientJPEGProbe(t *testing.T) {
	if got := legacyBasicImageMIME(map[string]string{}); got != "image/png" {
		t.Fatalf("no k capability must use PNG, got %q", got)
	}
	if got := legacyBasicImageMIME(map[string]string{"k": "image/jpeg"}); got != "image/jpeg" {
		t.Fatalf("JPEG-capable Basic must keep JPEG, got %q", got)
	}
	if got := legacyBasicImageMIME(map[string]string{"k": "image/png"}); got != "image/png" {
		t.Fatalf("non-JPEG capability must stay on guaranteed PNG, got %q", got)
	}
}

func TestLegacyBasicOM2FrameDeflate(t *testing.T) {
	page := oms.NewPage()
	page.SetTransport(oms.ClientVersion2, oms.CompressionDeflate)
	const wantURL = "1/http://example.com/"
	page.AddString(wantURL)
	page.AddText("OK")
	page.Finalize()

	if len(page.Data) < 6 || page.Data[0] != 0x18 || page.Data[1] != byte(oms.CompressionDeflate) {
		t.Fatalf("unexpected source frame: % x", page.Data[:minInt(len(page.Data), 8)])
	}

	wire, err := legacyBasicOM2Frame(page.Data)
	if err != nil {
		t.Fatal(err)
	}
	if wire[0] != 0x18 || wire[1] != byte(oms.CompressionNone) {
		t.Fatalf("legacy transport=%02x %02x", wire[0], wire[1])
	}
	if got := int(binary.BigEndian.Uint32(wire[2:6])); got != len(wire) {
		t.Fatalf("legacy size header=%d actual=%d", got, len(wire))
	}

	body := wire[6:]
	if len(body) < legacyBasicOM2HeaderBytes+2 {
		t.Fatalf("short legacy body: %d", len(body))
	}
	urlOffset := legacyBasicOM2HeaderBytes
	urlLen := int(binary.BigEndian.Uint16(body[urlOffset : urlOffset+2]))
	if urlOffset+2+urlLen > len(body) {
		t.Fatalf("bad URL length %d for body %d", urlLen, len(body))
	}
	if got := string(body[urlOffset+2 : urlOffset+2+urlLen]); got != wantURL {
		t.Fatalf("legacy readUTF alignment: got %q want %q", got, wantURL)
	}
}

func TestLegacyBasicSmokePageParsesAtLegacyOffsets(t *testing.T) {
	page := renderLegacyBasicSmokePage()
	wire, err := legacyBasicOM2Frame(page.Data)
	if err != nil {
		t.Fatal(err)
	}
	if wire[0] != 0x18 || wire[1] != byte(oms.CompressionNone) {
		t.Fatalf("smoke transport=%02x %02x", wire[0], wire[1])
	}
	body := wire[6:]
	if len(body) < legacyBasicOM2HeaderBytes+2 {
		t.Fatalf("short smoke body: %d", len(body))
	}
	urlOffset := legacyBasicOM2HeaderBytes
	urlLen := int(binary.BigEndian.Uint16(body[urlOffset : urlOffset+2]))
	tagOffset := urlOffset + 2 + urlLen
	if tagOffset >= len(body) || body[tagOffset] != 'S' {
		t.Fatalf("first smoke tag offset=%d byte=%02x", tagOffset, body[tagOffset])
	}
}

func TestLegacyBasicInlineImageMatchesBasicReadUTFLayout(t *testing.T) {
	page := oms.NewPage()
	page.SetTransport(oms.ClientVersion2, oms.CompressionNone)
	page.AddString("1/http://example.org/")
	imageBytes := []byte{0xff, 0xd8, 0xff, 0xd9}
	page.AddImageInline(12, 7, imageBytes)
	page.Finalize()

	body := page.Data[6:]
	p := legacyBasicOM2HeaderBytes
	urlLen := int(binary.BigEndian.Uint16(body[p : p+2]))
	p += 2 + urlLen
	if body[p] != 'I' {
		t.Fatalf("image tag=%02x", body[p])
	}
	p++
	if got := int(binary.BigEndian.Uint16(body[p : p+2])); got != 12 {
		t.Fatalf("width=%d", got)
	}
	p += 2
	if got := int(binary.BigEndian.Uint16(body[p : p+2])); got != 7 {
		t.Fatalf("height=%d", got)
	}
	p += 2
	dataLen := int(binary.BigEndian.Uint16(body[p : p+2]))
	p += 2
	if dataLen != len(imageBytes) {
		t.Fatalf("image data len=%d", dataLen)
	}
	// Basic calls DataInputStream.readUTF() here. Operetta's legacy-reserved
	// u16 is zero, which is exactly an empty modified-UTF string.
	utfLen := int(binary.BigEndian.Uint16(body[p : p+2]))
	p += 2
	if utfLen != 0 {
		t.Fatalf("legacy image UTF length=%d", utfLen)
	}
	if got := body[p : p+dataLen]; string(got) != string(imageBytes) {
		t.Fatalf("image payload=%x want=%x", got, imageBytes)
	}
}

func TestLegacyBasicOM2FrameNone(t *testing.T) {
	page := oms.NewPage()
	page.SetTransport(oms.ClientVersion2, oms.CompressionNone)
	page.AddString("1/http://example.org/")
	page.AddText("OK")
	page.Finalize()

	wire, err := legacyBasicOM2Frame(page.Data)
	if err != nil {
		t.Fatal(err)
	}
	if wire[0] != 0x18 || wire[1] != byte(oms.CompressionNone) {
		t.Fatalf("legacy transport=%02x %02x", wire[0], wire[1])
	}
	if len(wire) != len(page.Data) {
		t.Fatalf("none frame size=%d source=%d", len(wire), len(page.Data))
	}
	if got := int(binary.BigEndian.Uint32(wire[2:6])); got != len(wire) {
		t.Fatalf("legacy size header=%d actual=%d", got, len(wire))
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
