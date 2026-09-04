package operamini4

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestNativeWelcomeDocumentRoundTrip(t *testing.T) {
	page, err := BuildWelcomePage(WelcomePage{
		Title: "Operetta",
		Base:  "https://example.test/",
		Lines: []WelcomeLine{
			{Text: "Добро пожаловать", Color: 0xffffffff, Font: 3, Height: 34},
			{Text: "Локальный сервер", URL: "https://example.test/next", Color: 0xffd7e6ee, Height: 28},
		},
	})
	if err != nil {
		t.Fatalf("BuildWelcomePage: %v", err)
	}
	frames, err := EncodeApplicationDocument(42, []DocumentRecord{{Type: 'o', Payload: []byte{0, 2}}}, page)
	if err != nil {
		t.Fatalf("EncodeApplicationDocument: %v", err)
	}
	document, err := DecodeApplicationDocument(frames)
	if err != nil {
		t.Fatalf("DecodeApplicationDocument: %v", err)
	}
	if document.ID != 42 || document.Header.Title != "Operetta" || document.Header.URL != "i:/firsttime/4.2/" {
		t.Fatalf("decoded header: id=%d %+v", document.ID, document.Header)
	}
	if len(document.Texts) != 2 || document.Texts[0].Text != "Добро пожаловать" || document.Texts[1].Text != "Локальный сервер" {
		t.Fatalf("decoded texts: %+v", document.Texts)
	}
	if document.Header.Base != "https://example.test/" || len(document.Links) != 1 || document.Links[0].URL != "https://example.test/next" {
		t.Fatalf("decoded links/header: base=%q links=%+v", document.Header.Base, document.Links)
	}
	if len(document.Drawings) != 4 || document.Drawings[2].Kind != 'T' || document.Drawings[2].X != 12 || document.Drawings[3].Y <= document.Drawings[2].Y {
		t.Fatalf("decoded absolute drawings: %+v", document.Drawings)
	}
	offset := document.Header.ContentOffset
	mc := bytes.Index(document.Page[offset:], []byte("MC"))
	if mc < 0 {
		t.Fatalf("missing MC layout at offset %d", offset)
	}
	mc += offset
	if slots := binary.BigEndian.Uint16(document.Page[mc+5 : mc+7]); slots != 4 {
		t.Fatalf("layout instruction slots=%d, want 4", slots)
	}
}

func TestSessionRequestURLsUsesLengthBoundaries(t *testing.T) {
	encode := func(value string) []byte {
		var out bytes.Buffer
		_ = binary.Write(&out, binary.BigEndian, uint16(len(value)))
		out.WriteString(value)
		return out.Bytes()
	}
	payload := append([]byte{1, 2, 3}, encode("https://sefan.ru/current")...)
	payload = append(payload, 0, 7, 9)
	payload = append(payload, encode("https://sefan.ru/loads/?new")...)
	payload = append(payload, byte(len("www.sefan.ru")))
	payload = append(payload, "www.sefan.ru"...)
	payload = append(payload, []byte("http://not-length-prefixed.invalid/")...)
	request := &SessionRequest{Frames: []Frame{{Type: 8, Channel: 1, Payload: payload}}}
	got := request.RequestURLs()
	if len(got) != 3 || got[0] != "https://sefan.ru/current" || got[1] != "https://sefan.ru/loads/?new" || got[2] != "http://www.sefan.ru" {
		t.Fatalf("RequestURLs=%q", got)
	}
}

func TestDecodeLinkElementsSkipsImageInteraction(t *testing.T) {
	var section bytes.Buffer
	for _, item := range []struct {
		kind  byte
		value string
	}{
		{kind: 'i', value: "https://example.test/header.jpg"},
		{kind: 'L', value: "https://example.test/next"},
	} {
		section.WriteByte(item.kind)
		section.WriteByte(1)
		writeRectangleBody(&section, 2, 3, 40, 12)
		if err := writeModifiedUTF(&section, ""); err != nil {
			t.Fatal(err)
		}
		if err := writeModifiedUTF(&section, item.value); err != nil {
			t.Fatal(err)
		}
	}
	var page bytes.Buffer
	page.WriteByte('S')
	writeUint24(&page, section.Len())
	page.Write(section.Bytes())
	links := DecodeLinkElements(page.Bytes(), 0)
	if len(links) != 1 || links[0].URL != "https://example.test/next" {
		t.Fatalf("links=%+v", links)
	}
}

func TestNativeWelcomeEmbedsInlineImage(t *testing.T) {
	imageData := []byte{0xff, 0xd8, 0xff, 0xd9}
	page, err := BuildWelcomePage(WelcomePage{
		Title: "Image",
		Lines: []WelcomeLine{{Image: imageData, Width: 20, Height: 10}},
	})
	if err != nil {
		t.Fatalf("BuildWelcomePage: %v", err)
	}
	blob := bytes.LastIndex(page, imageData)
	if blob < 2 || binary.BigEndian.Uint16(page[blob-2:blob]) != uint16(len(imageData)) {
		t.Fatalf("embedded image blob not found: offset=%d", blob)
	}
	pointer := []byte{byte((blob - 2) >> 16), byte((blob - 2) >> 8), byte(blob - 2)}
	if at := bytes.Index(page[:blob-2], pointer); at < 0 {
		t.Fatalf("image pointer %x not found", pointer)
	}
}

func TestNativeWelcomeAbsoluteImageAndBackgroundGeometry(t *testing.T) {
	page, err := BuildWelcomePage(WelcomePage{
		Title: "Absolute", HideAccent: true, DocumentHeight: 400,
		Lines: []WelcomeLine{{
			Image: []byte{0xff, 0xd8, 0xff, 0xd9}, X: 81, Y: 218, Width: 76, Height: 53,
			Positioned: true, Absolute: true, Background: 0xffcddae7,
			BackgroundX: 1, BackgroundY: 193, BackgroundWidth: 229, BackgroundHeight: 25, BackgroundPositioned: true,
		}},
	})
	if err != nil {
		t.Fatalf("BuildWelcomePage: %v", err)
	}
	frames, err := EncodeApplicationDocument(7, nil, page)
	if err != nil {
		t.Fatal(err)
	}
	document, err := DecodeApplicationDocument(frames)
	if err != nil {
		t.Fatal(err)
	}
	if document.Header.DocumentHeight != 400 || len(document.Drawings) != 3 {
		t.Fatalf("header/drawings: height=%d drawings=%+v", document.Header.DocumentHeight, document.Drawings)
	}
	background, image := document.Drawings[1], document.Drawings[2]
	if background.Kind != 'B' || background.X != 1 || background.Y != 193 || background.Width != 229 || image.Kind != 'I' || image.X != 81 || image.Y != 218 {
		t.Fatalf("absolute geometry: background=%+v image=%+v", background, image)
	}
}

func TestNativeWelcomeDocumentHeightIsExactOverride(t *testing.T) {
	page, err := BuildWelcomePage(WelcomePage{
		DocumentHeight: 400,
		Lines: []WelcomeLine{{
			Text: "positioned beyond logical page", X: 0, Y: 900,
			Width: 200, Height: 100, Positioned: true, Absolute: true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	header, err := DecodePageHeader(page)
	if err != nil {
		t.Fatal(err)
	}
	if header.DocumentHeight != 400 {
		t.Fatalf("document height = %d, want exact override 400", header.DocumentHeight)
	}
}

func TestNativeWelcomeMergesFragmentsOfOneSourceLink(t *testing.T) {
	page, err := BuildWelcomePage(WelcomePage{
		Lines: []WelcomeLine{
			{Image: []byte{0xff, 0xd8, 0xff, 0xd9}, URL: "https://example.test/chat", LinkGroup: 1, X: 5, Y: 20, Width: 16, Height: 16, Positioned: true, Absolute: true},
			{Text: "Chat", URL: "https://example.test/chat", LinkGroup: 1, X: 24, Y: 21, Width: 190, Height: 14, Positioned: true, Absolute: true},
			{Text: "Chat again", URL: "https://example.test/chat", LinkGroup: 2, X: 5, Y: 50, Width: 209, Height: 14, Positioned: true, Absolute: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	header, err := DecodePageHeader(page)
	if err != nil {
		t.Fatal(err)
	}
	links := DecodeLinkElements(page, header.ContentOffset)
	if len(links) != 2 {
		t.Fatalf("links=%+v, want two source anchors", links)
	}
	if links[0].X != 5 || links[0].Y != 20 || links[0].Width != 209 || links[0].Height != 16 {
		t.Fatalf("merged link=%+v, want 5,20 209x16", links[0])
	}
}

func TestNativeWelcomeUsesExplicitLinkBounds(t *testing.T) {
	page, err := BuildWelcomePage(WelcomePage{
		Lines: []WelcomeLine{{
			Text: "Chat", URL: "https://example.test/chat", X: 24, Y: 21, Width: 190, Height: 14, Positioned: true, Absolute: true,
			LinkX: 2, LinkY: 17, LinkWidth: 227, LinkHeight: 24, LinkPositioned: true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	header, err := DecodePageHeader(page)
	if err != nil {
		t.Fatal(err)
	}
	links := DecodeLinkElements(page, header.ContentOffset)
	if len(links) != 1 || links[0].X != 2 || links[0].Y != 17 || links[0].Width != 227 || links[0].Height != 24 {
		t.Fatalf("links=%+v, want explicit 2,17 227x24 bounds", links)
	}
}
