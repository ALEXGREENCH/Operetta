package oms

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"image"
	_ "image/png"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

const (
	tinyPNGBase64  = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAAEElEQVR4nGJKS0sDBAAA//8CcAE1noYijgAAAABJRU5ErkJggg=="
	tinyPNGDataURI = "data:image/png;base64," + tinyPNGBase64
)

type obmlFixture struct {
	name        string
	html        string
	url         string
	contentType string
	header      http.Header
	status      int
	opts        *RenderOptions
	assert      func(*testing.T, *fixtureResult)
}

type obmlToken struct {
	tag     byte
	data    []byte
	strings []string
	extra   []byte
}

type fixtureResult struct {
	page    *Page
	version ClientVersion
	initial string
	payload []byte
	tokens  []obmlToken

	cachedTexts []string
	cachedLinks []string
}

func TestFetchAndEncodeImageDataURI(t *testing.T) {
	t.Parallel()
	if _, err := base64.StdEncoding.DecodeString(tinyPNGBase64); err != nil {
		t.Fatalf("base64 decode tiny png: %v", err)
	}
	raw, _ := base64.StdEncoding.DecodeString(tinyPNGBase64)
	if _, _, err := image.Decode(bytes.NewReader(raw)); err != nil {
		t.Fatalf("image.Decode failed: %v", err)
	}
	if _, _, _, _, _, ok := decodeDataURI(tinyPNGDataURI, defaultRenderPrefs()); !ok {
		t.Fatalf("decodeDataURI failed")
	}
	data, w, h, ok := fetchAndEncodeImage(tinyPNGDataURI, defaultRenderPrefs())
	if !ok {
		t.Fatalf("fetchAndEncodeImage returned ok=false")
	}
	if len(data) == 0 {
		t.Fatalf("expected non-empty image bytes")
	}
	if w != 1 || h != 1 {
		t.Fatalf("expected 1x1 image, got %dx%d", w, h)
	}
}

func TestCollectTextCaption(t *testing.T) {
	doc, err := html.Parse(strings.NewReader(`<table><caption>Quarterly report</caption></table>`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	caption := findFirstByTag(doc, "caption")
	if caption == nil {
		t.Fatalf("caption not found")
	}
	if txt := strings.TrimSpace(collectText(caption)); txt != "Quarterly report" {
		t.Fatalf("collectText caption=%q", txt)
	}
}

func renderFixture(t *testing.T, fx obmlFixture) *fixtureResult {
	t.Helper()
	if strings.TrimSpace(fx.html) == "" {
		t.Fatalf("fixture %q has empty HTML body", fx.name)
	}
	url := fx.url
	if url == "" {
		url = "http://fixture.test/"
	}
	contentType := fx.contentType
	if contentType == "" {
		contentType = "text/html; charset=utf-8"
	}
	hdr := http.Header{}
	if fx.header != nil {
		for k, vs := range fx.header {
			copied := make([]string, len(vs))
			copy(copied, vs)
			hdr[k] = copied
		}
	}
	if hdr.Get("Content-Type") == "" {
		hdr.Set("Content-Type", contentType)
	}
	status := fx.status
	if status == 0 {
		status = http.StatusOK
	}
	bodyBytes := []byte(fx.html)
	doc := &UpstreamDocument{
		URL:           url,
		Body:          append([]byte(nil), bodyBytes...),
		RawBody:       append([]byte(nil), bodyBytes...),
		Header:        hdr,
		Status:        status,
		TransferBytes: len(bodyBytes),
		ContentLength: int64(len(bodyBytes)),
	}
	page, err := RenderDocument(doc, http.Header{}, fx.opts)
	if err != nil {
		t.Fatalf("RenderDocument(%q) failed: %v", fx.name, err)
	}
	page.Normalize()
	res, err := parsePage(page)
	if err != nil {
		t.Fatalf("parsePage(%q): %v", fx.name, err)
	}
	return res
}

func parsePage(page *Page) (*fixtureResult, error) {
	if page == nil || len(page.Data) < 6 {
		return nil, fmt.Errorf("invalid page data length")
	}
	headerWord := binary.LittleEndian.Uint16(page.Data[:2])
	version := clientVersionFromHeaderByte(byte(headerWord & 0xFF))
	compression := compressionFromHeaderByte(byte(headerWord >> 8))
	decoded, err := decompressPayload(compression, page.Data[6:])
	if err != nil {
		return nil, err
	}
	headerLen := 35
	if version == ClientVersion1 {
		headerLen = 33
	}
	if len(decoded) < headerLen+2 {
		return nil, fmt.Errorf("decoded payload too short: %d", len(decoded))
	}
	payload := append([]byte(nil), decoded[headerLen:]...)
	idx := 0
	if idx+2 > len(payload) {
		return nil, fmt.Errorf("missing initial string")
	}
	strLen := int(binary.BigEndian.Uint16(payload[idx : idx+2]))
	idx += 2
	if idx+strLen > len(payload) {
		return nil, fmt.Errorf("invalid initial string length %d", strLen)
	}
	initial := string(payload[idx : idx+strLen])
	idx += strLen
	tokens, err := parseTokens(payload[idx:], version)
	if err != nil {
		return nil, err
	}
	return &fixtureResult{
		page:    page,
		version: version,
		initial: initial,
		payload: payload,
		tokens:  tokens,
	}, nil
}

func parseTokens(payload []byte, version ClientVersion) ([]obmlToken, error) {
	styleLen := 4
	if normalizeClientVersion(version) == ClientVersion3 {
		styleLen = 6
	}
	idx := 0
	limit := len(payload)
	var tokens []obmlToken
	readString := func() (string, error) {
		if idx+2 > limit {
			return "", fmt.Errorf("short string length")
		}
		l := int(binary.BigEndian.Uint16(payload[idx : idx+2]))
		idx += 2
		if idx+l > limit {
			return "", fmt.Errorf("short string data")
		}
		s := string(payload[idx : idx+l])
		idx += l
		return s, nil
	}
	for idx < limit {
		tag := payload[idx]
		idx++
		tok := obmlToken{tag: tag}
		switch tag {
		case 'T', 'L':
			s, err := readString()
			if err != nil {
				return nil, fmt.Errorf("tag %q string: %w", tag, err)
			}
			tok.strings = []string{s}
		case 'k':
			if idx >= limit {
				return nil, fmt.Errorf("tag k missing type byte")
			}
			tok.data = append(tok.data, payload[idx])
			idx++
			s, err := readString()
			if err != nil {
				return nil, fmt.Errorf("tag k string: %w", err)
			}
			tok.strings = []string{s}
		case 'D', 'R':
			if idx+2 > limit {
				return nil, fmt.Errorf("tag %q missing 2 bytes", tag)
			}
			tok.data = append(tok.data, payload[idx:idx+2]...)
			idx += 2
		case 'S':
			if idx+styleLen > limit {
				return nil, fmt.Errorf("tag S missing %d bytes", styleLen)
			}
			tok.data = append(tok.data, payload[idx:idx+styleLen]...)
			idx += styleLen
		case 'J':
			if idx+4 > limit {
				return nil, fmt.Errorf("tag J missing 4 bytes")
			}
			tok.data = append(tok.data, payload[idx:idx+4]...)
			idx += 4
		case 'I':
			if idx+8 > limit {
				return nil, fmt.Errorf("tag I missing header")
			}
			hdr := payload[idx : idx+8]
			tok.data = append(tok.data, hdr...)
			idx += 8
			dl := int(binary.BigEndian.Uint16(hdr[4:6]))
			if idx+dl > limit {
				return nil, fmt.Errorf("tag I payload overflow")
			}
			tok.extra = append(tok.extra, payload[idx:idx+dl]...)
			idx += dl
		case 'h':
			for i := 0; i < 2; i++ {
				s, err := readString()
				if err != nil {
					return nil, fmt.Errorf("tag h string: %w", err)
				}
				tok.strings = append(tok.strings, s)
			}
		case 'x':
			if idx >= limit {
				return nil, fmt.Errorf("tag x missing config byte")
			}
			tok.data = append(tok.data, payload[idx])
			idx++
			for i := 0; i < 2; i++ {
				s, err := readString()
				if err != nil {
					return nil, fmt.Errorf("tag x string: %w", err)
				}
				tok.strings = append(tok.strings, s)
			}
		case 'p', 'u', 'i', 'b', 'e':
			for i := 0; i < 2; i++ {
				s, err := readString()
				if err != nil {
					return nil, fmt.Errorf("tag %q string: %w", tag, err)
				}
				tok.strings = append(tok.strings, s)
			}
		case 'c', 'r':
			for i := 0; i < 2; i++ {
				s, err := readString()
				if err != nil {
					return nil, fmt.Errorf("tag %q string: %w", tag, err)
				}
				tok.strings = append(tok.strings, s)
			}
			if idx >= limit {
				return nil, fmt.Errorf("tag %q missing state byte", tag)
			}
			tok.data = append(tok.data, payload[idx])
			idx++
		case 's':
			s, err := readString()
			if err != nil {
				return nil, fmt.Errorf("tag s name: %w", err)
			}
			tok.strings = append(tok.strings, s)
			if idx >= limit {
				return nil, fmt.Errorf("tag s missing multiple flag")
			}
			tok.data = append(tok.data, payload[idx])
			idx++
			if idx+2 > limit {
				return nil, fmt.Errorf("tag s missing count")
			}
			tok.data = append(tok.data, payload[idx:idx+2]...)
			idx += 2
		case 'o':
			for i := 0; i < 2; i++ {
				s, err := readString()
				if err != nil {
					return nil, fmt.Errorf("tag o string: %w", err)
				}
				tok.strings = append(tok.strings, s)
			}
			if idx >= limit {
				return nil, fmt.Errorf("tag o missing selected flag")
			}
			tok.data = append(tok.data, payload[idx])
			idx++
		case 'E', 'B', '+', 'V', 'Q', 'l':
		default:
			return nil, fmt.Errorf("unknown tag %q", tag)
		}
		tokens = append(tokens, tok)
	}
	return tokens, nil
}

func (fr *fixtureResult) initialURL() string {
	if strings.HasPrefix(fr.initial, "1/") {
		return fr.initial[2:]
	}
	return fr.initial
}

func (fr *fixtureResult) textStrings() []string {
	if fr.cachedTexts != nil {
		return fr.cachedTexts
	}
	var texts []string
	for _, tok := range fr.tokens {
		if tok.tag == 'T' {
			texts = append(texts, tok.strings...)
		}
	}
	fr.cachedTexts = texts
	return texts
}

func (fr *fixtureResult) linkURLs() []string {
	if fr.cachedLinks != nil {
		return fr.cachedLinks
	}
	var links []string
	for _, tok := range fr.tokens {
		if tok.tag == 'L' && len(tok.strings) > 0 {
			links = append(links, tok.strings[0])
		}
	}
	fr.cachedLinks = links
	return links
}

func (fr *fixtureResult) backgroundColors() []uint16 {
	var colors []uint16
	for _, tok := range fr.tokens {
		if tok.tag == 'D' && len(tok.data) == 2 {
			colors = append(colors, binary.BigEndian.Uint16(tok.data))
		}
	}
	return colors
}

func (fr *fixtureResult) countTag(tag byte) int {
	count := 0
	for _, tok := range fr.tokens {
		if tok.tag == tag {
			count++
		}
	}
	return count
}

func (fr *fixtureResult) tokensByTag(tag byte) []obmlToken {
	var out []obmlToken
	for _, tok := range fr.tokens {
		if tok.tag == tag {
			out = append(out, tok)
		}
	}
	return out
}

func (fr *fixtureResult) mustContainText(t *testing.T, want string) {
	t.Helper()
	for _, txt := range fr.textStrings() {
		if strings.Contains(txt, want) {
			return
		}
	}
	t.Fatalf("expected text %q, got %v", want, fr.textStrings())
}

func (fr *fixtureResult) mustNotContainText(t *testing.T, ban string) {
	t.Helper()
	for _, txt := range fr.textStrings() {
		if strings.Contains(txt, ban) {
			t.Fatalf("did not expect text %q, got %v", ban, fr.textStrings())
		}
	}
}

func (fr *fixtureResult) mustHaveLink(t *testing.T, want string) {
	t.Helper()
	for _, link := range fr.linkURLs() {
		if link == want {
			return
		}
	}
	t.Fatalf("expected link %q, got %v", want, fr.linkURLs())
}

func (fr *fixtureResult) mustHaveLinkPrefix(t *testing.T, prefix string) {
	t.Helper()
	for _, link := range fr.linkURLs() {
		if strings.HasPrefix(link, prefix) {
			return
		}
	}
	t.Fatalf("expected link with prefix %q, got %v", prefix, fr.linkURLs())
}

func (fr *fixtureResult) mustHaveBackgroundColor(t *testing.T, want uint16) {
	t.Helper()
	for _, col := range fr.backgroundColors() {
		if col == want {
			return
		}
	}
	t.Fatalf("expected background color 0x%04x, got %v", want, fr.backgroundColors())
}

func (fr *fixtureResult) formHiddenValue(action, name string) (string, bool) {
	if fr.page == nil || fr.page.FormHidden == nil {
		return "", false
	}
	fields := fr.page.FormHidden[action]
	if fields == nil {
		return "", false
	}
	val, ok := fields[name]
	return val, ok
}

func TestRenderDocumentSyntheticSites(t *testing.T) {
	fixtures := []obmlFixture{
		{
			name: "basic_structure",
			html: `<!DOCTYPE html><html><head><title>Fixture</title></head><body><header><h1>Main Title</h1></header><p>Paragraph with <strong>bold</strong> text.</p></body></html>`,
			assert: func(t *testing.T, res *fixtureResult) {
				if got := res.initialURL(); !strings.HasPrefix(got, "http://fixture.test/") {
					t.Fatalf("initial URL mismatch: %s", got)
				}
				res.mustContainText(t, "Main Title")
				res.mustContainText(t, "Paragraph with")
				res.mustContainText(t, "bold")
			},
		},
		{
			name: "body_inline_background",
			html: `<html><body style="background-color:#102030;color:#f0f0f0">Colorful body</body></html>`,
			assert: func(t *testing.T, res *fixtureResult) {
				res.mustContainText(t, "Colorful body")
				bg := normalizeBgForBlackText("#102030")
				if bg == "" {
					bg = "#1a1a1a"
				}
				bg = ensureMinForRGB565(bg)
				res.mustHaveBackgroundColor(t, calcColor(bg))
			},
		},
		{
			name: "body_css_background",
			html: `<html><head><style>body{background:#0a1b2c;color:#fafafa;}</style></head><body>CSS background</body></html>`,
			assert: func(t *testing.T, res *fixtureResult) {
				res.mustContainText(t, "CSS background")
				bg := normalizeBgForBlackText("#0a1b2c")
				if bg == "" {
					bg = "#1a1a1a"
				}
				bg = ensureMinForRGB565(bg)
				res.mustHaveBackgroundColor(t, calcColor(bg))
			},
		},
		{
			name: "relative_links_with_base",
			url:  "https://unit.test/index.html",
			html: `<html><head><base href="https://unit.test/app/"></head><body><a href="profile">Profile</a><a href="/logout">Log out</a></body></html>`,
			assert: func(t *testing.T, res *fixtureResult) {
				res.mustHaveLink(t, "0/https://unit.test/app/profile")
				res.mustHaveLink(t, "0/https://unit.test/logout")
			},
		},
		{
			name: "html5_sectioning",
			html: `<main><section><article><h2>Section Title</h2><p>Article copy.</p></article></section><aside><p>Sidebar note</p></aside></main>`,
			assert: func(t *testing.T, res *fixtureResult) {
				res.mustContainText(t, "Section Title")
				res.mustContainText(t, "Article copy")
				res.mustContainText(t, "Sidebar note")
			},
		},
		{
			name: "unordered_list",
			html: `<ul><li>First</li><li>Second</li><li>Third</li></ul>`,
			assert: func(t *testing.T, res *fixtureResult) {
				res.mustContainText(t, "First")
				res.mustContainText(t, "Second")
				res.mustContainText(t, "Third")
			},
		},
		{
			name: "ordered_list_nested",
			html: `<ol><li>Alpha<ul><li>Beta</li></ul></li><li>Gamma</li></ol>`,
			assert: func(t *testing.T, res *fixtureResult) {
				res.mustContainText(t, "Alpha")
				res.mustContainText(t, "Beta")
				res.mustContainText(t, "Gamma")
			},
		},
		{
			name: "table_with_header_cells",
			html: `<table><caption>Quarterly report</caption><thead><tr><th>Quarter</th><th>Revenue</th></tr></thead><tbody><tr><td>Q1</td><td>$100</td></tr><tr><td>Q2</td><td>$150</td></tr></tbody></table>`,
			assert: func(t *testing.T, res *fixtureResult) {
				res.mustContainText(t, "Quarterly report")
				res.mustContainText(t, "Quarter | Revenue")
				res.mustContainText(t, "$150")
			},
		},
		{
			name: "iframe_frame_link",
			html: `<iframe src="https://videos.example.com/embed/123"></iframe>`,
			assert: func(t *testing.T, res *fixtureResult) {
				res.mustHaveLink(t, "0/https://videos.example.com/embed/123")
				res.mustContainText(t, "[Frame]")
			},
		},
		{
			name: "video_media_link",
			html: `<video controls src="/videos/clip.mp4"><source src="/videos/alternate.ogv" type="video/ogg"></video>`,
			assert: func(t *testing.T, res *fixtureResult) {
				res.mustHaveLink(t, "0/http://fixture.test/videos/clip.mp4")
				res.mustContainText(t, "[Media]")
			},
		},
		{
			name: "object_embed_link",
			html: `<object data="https://cdn.example.com/app.swf"></object>`,
			assert: func(t *testing.T, res *fixtureResult) {
				res.mustHaveLink(t, "0/https://cdn.example.com/app.swf")
				res.mustContainText(t, "[Object]")
			},
		},
		{
			name: "img_data_uri_inline",
			html: `<img alt="Tiny" src="` + tinyPNGDataURI + `">`,
			assert: func(t *testing.T, res *fixtureResult) {
				if res.countTag('I') == 0 {
					t.Fatalf("expected inline image tag, tokens=%v", res.tokensByTag('I'))
				}
			},
		},
		{
			name: "css_background_image_on",
			html: `<style>.balls{display:inline-block;width:16px;height:16px;background-image:url(` + tinyPNGDataURI + `);}</style><div class="balls"></div>`,
			assert: func(t *testing.T, res *fixtureResult) {
				if res.countTag('I') == 0 {
					t.Fatalf("expected inline background image, tokens=%v", res.tokensByTag('I'))
				}
			},
		},
		{
			name: "picture_data_uri_link",
			html: `<picture><source srcset="` + tinyPNGDataURI + `" type="image/png"><img src="` + tinyPNGDataURI + `" alt="Alt"></picture>`,
			assert: func(t *testing.T, res *fixtureResult) {
				if res.countTag('I') == 0 {
					t.Fatalf("expected inline picture image, tokens=%v", res.tokensByTag('I'))
				}
			},
		},
		{
			name: "details_summary_rendering",
			html: `<details open><summary>More Info</summary><p>Hidden details shown.</p></details>`,
			assert: func(t *testing.T, res *fixtureResult) {
				res.mustContainText(t, "More Info")
				res.mustContainText(t, "Hidden details shown")
			},
		},
		{
			name: "css_background_image_off",
			html: `<style>.balls{display:inline-block;width:16px;height:16px;background-image:url(` + tinyPNGDataURI + `);}</style><div class="balls"></div>`,
			opts: func() *RenderOptions {
				o := defaultRenderPrefs()
				o.ImagesOn = false
				return &o
			}(),
			assert: func(t *testing.T, res *fixtureResult) {
				if res.countTag('I') != 0 || res.countTag('J') != 0 {
					t.Fatalf("expected no background images when images off, tokens_I=%v tokens_J=%v", res.tokensByTag('I'), res.tokensByTag('J'))
				}
				res.mustContainText(t, "*")
			},
		},
		{
			name: "form_hidden_inputs",
			html: `<form action="/submit"><input type="hidden" name="token" value="abc123"><input type="submit" value="Send"></form>`,
			assert: func(t *testing.T, res *fixtureResult) {
				action := "http://fixture.test/submit"
				if val, ok := res.formHiddenValue(action, "token"); !ok || val != "abc123" {
					t.Fatalf("expected hidden token for %s, got %q (ok=%v)", action, val, ok)
				}
			},
		},
		{
			name: "form_text_and_password_inputs",
			html: `<form><input type="text" name="q" value="opera"><textarea name="notes" value="remember"></textarea><input type="password" name="pass" value="secret"><button type="submit" name="go" value="Go">Submit</button></form>`,
			assert: func(t *testing.T, res *fixtureResult) {
				if res.countTag('x') < 2 {
					t.Fatalf("expected at least two text input tags, got %d", res.countTag('x'))
				}
				if res.countTag('p') != 1 {
					t.Fatalf("expected password input tag, got %d", res.countTag('p'))
				}
				if res.countTag('u') == 0 {
					t.Fatalf("expected submit/button tag, tokens=%v", res.tokensByTag('u'))
				}
			},
		},
		{
			name: "checkbox_and_radio_inputs",
			html: `<form><input type="checkbox" name="terms" value="yes" checked><input type="radio" name="choice" value="a" checked><input type="radio" name="choice" value="b"></form>`,
			assert: func(t *testing.T, res *fixtureResult) {
				checks := res.tokensByTag('c')
				if len(checks) != 1 || len(checks[0].data) == 0 || checks[0].data[0] != 1 {
					t.Fatalf("expected checked checkbox token, got %v", checks)
				}
				radios := res.tokensByTag('r')
				if len(radios) != 2 {
					t.Fatalf("expected two radio tokens, got %v", radios)
				}
				if len(radios[0].data) == 0 || radios[0].data[0] != 1 {
					t.Fatalf("expected first radio selected, got %v", radios[0])
				}
				if len(radios[1].data) == 0 || radios[1].data[0] != 0 {
					t.Fatalf("expected second radio unselected, got %v", radios[1])
				}
			},
		},
		{
			name: "select_multiple_options",
			html: `<form><select name="opts" multiple><option value="a" selected>Alpha</option><option value="b">Beta</option></select></form>`,
			assert: func(t *testing.T, res *fixtureResult) {
				selects := res.tokensByTag('s')
				if len(selects) != 1 || len(selects[0].data) < 3 || selects[0].data[0] != 1 {
					t.Fatalf("expected select(multiple) token, got %v", selects)
				}
				opts := res.tokensByTag('o')
				if len(opts) != 2 {
					t.Fatalf("expected two option tokens, got %v", opts)
				}
				if len(opts[0].data) == 0 || opts[0].data[0] != 1 {
					t.Fatalf("expected first option selected, got %v", opts[0])
				}
				if len(opts[1].data) == 0 || opts[1].data[0] != 0 {
					t.Fatalf("expected second option unselected, got %v", opts[1])
				}
			},
		},
		{
			name: "label_fieldset_legend",
			html: `<form><fieldset><legend>Profile</legend><label>Username<input name="user"></label></fieldset></form>`,
			assert: func(t *testing.T, res *fixtureResult) {
				res.mustContainText(t, "Profile")
				res.mustContainText(t, "Username: ")
			},
		},
		{
			name: "noscript_exclusion",
			html: `<noscript>Visible fallback</noscript><p>Main body</p><script>document.write('should not appear')</script>`,
			assert: func(t *testing.T, res *fixtureResult) {
				res.mustContainText(t, "Main body")
				res.mustNotContainText(t, "Visible fallback")
				res.mustNotContainText(t, "should not appear")
			},
		},
		{
			name: "figure_and_caption",
			html: `<figure><img src="` + tinyPNGDataURI + `" alt="Diagram"><figcaption>Figure caption</figcaption></figure>`,
			assert: func(t *testing.T, res *fixtureResult) {
				if res.countTag('I') == 0 {
					t.Fatalf("expected inline figure image, tokens=%v", res.tokensByTag('I'))
				}
				res.mustContainText(t, "Figure caption")
			},
		},
		{
			name: "blockquote_rendering",
			html: `<blockquote><p>Quote line</p></blockquote>`,
			assert: func(t *testing.T, res *fixtureResult) {
				res.mustContainText(t, "> Quote line")
			},
		},
		{
			name: "definition_list_rendering",
			html: `<dl><dt>Term</dt><dd>Definition</dd></dl>`,
			assert: func(t *testing.T, res *fixtureResult) {
				res.mustContainText(t, "Term")
				res.mustContainText(t, ": Definition")
			},
		},
		{
			name: "preformatted_text",
			html: `<pre>Line 1
    Line 2</pre><code>return 42;</code>`,
			assert: func(t *testing.T, res *fixtureResult) {
				res.mustContainText(t, "Line 1")
				res.mustContainText(t, "Line 2")
				found := false
				for _, txt := range res.textStrings() {
					if strings.Contains(txt, "Line 1") && strings.Contains(txt, "\n    Line 2") {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("expected preformatted newline in %v", res.textStrings())
				}
				res.mustContainText(t, "return 42;")
			},
		},
	}
	for _, fx := range fixtures {
		fx := fx
		t.Run(fx.name, func(t *testing.T) {
			res := renderFixture(t, fx)
			if fx.assert != nil {
				fx.assert(t, res)
			}
		})
	}
}
