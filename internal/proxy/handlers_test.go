package proxy

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"operetta/oms"
)

func TestRootRejectsOversizedOperaRequest(t *testing.T) {
	server := New(Config{SitesDir: t.TempDir(), Logger: log.New(io.Discard, "", 0)})
	body := bytes.Repeat([]byte{'x'}, (256<<10)+1)
	req := httptest.NewRequest(http.MethodPost, "http://operetta/", bytes.NewReader(body))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, req)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d, want %d", response.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestParseOperaBool(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want bool
		ok   bool
	}{
		{"empty", "", false, false},
		{"zero", "0", false, true},
		{"one", "1", true, true},
		{"two", "2", true, true},
		{"true", "true", true, true},
		{"false", "false", false, true},
		{"hi", "high", true, true},
		{"lo", "low", false, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseOperaBool(tc.in)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("parseOperaBool(%q) = (%v,%v), want (%v,%v)", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestSafeInlineMediaTypeRejectsActiveContent(t *testing.T) {
	for _, contentType := range []string{"text/html", "application/javascript", "image/svg+xml"} {
		if safeInlineMediaType(contentType) {
			t.Fatalf("safeInlineMediaType(%q)=true", contentType)
		}
	}
	for _, contentType := range []string{"video/3gpp", "audio/mpeg", "image/jpeg"} {
		if !safeInlineMediaType(contentType) {
			t.Fatalf("safeInlineMediaType(%q)=false", contentType)
		}
	}
}

func TestOriginHeadersDoNotForwardBrowserCookies(t *testing.T) {
	s := newTestServer()

	req := httptest.NewRequest(http.MethodPost, "http://operetta/", nil)
	req.Header.Set("Cookie", "OPERETTA_AUTH=client; WMF-DP=dark; PHPSESS=foreign")
	params := map[string]string{
		"i": "Opera Mini",
		"q": "ru",
	}
	hdr := s.headersFromParams(req, params)
	if got := hdr.Get("Cookie"); got != "" {
		t.Fatalf("headersFromParams forwarded browser Cookie=%q", got)
	}

	qreq := httptest.NewRequest(http.MethodGet, "http://operetta/fetch?url=http://example.test/&ua=Opera", nil)
	qreq.Header.Set("Cookie", "OPERETTA_AUTH=client; WMF-DP=dark")
	qhdr := s.headersFromQuery(qreq)
	if got := qhdr.Get("Cookie"); got != "" {
		t.Fatalf("headersFromQuery forwarded browser Cookie=%q", got)
	}
}

func newTestServer() *Server {
	return &Server{
		renderPrefs: newRenderPrefStore(),
		cookieJars:  NewCookieJarStore(),
		auth:        newAuthStore(time.Now),
		cache:       newPageCache(time.Now),
	}
}

func TestRenderOptionsFromParamsQuality(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "http://operetta/fetch", nil)
	hdr := http.Header{}
	s := newTestServer()

	hi := s.renderOptionsFromParams(r, map[string]string{"d": "q:2"}, hdr, "")
	if hi.HighQuality != true {
		t.Fatalf("expected HighQuality true for q:2, got %v", hi.HighQuality)
	}

	lo := s.renderOptionsFromParams(r, map[string]string{"d": "q:0"}, hdr, "")
	if lo.HighQuality {
		t.Fatalf("expected HighQuality false for q:0, got %v", lo.HighQuality)
	}

	lo2 := s.renderOptionsFromParams(r, map[string]string{"d": "Q=lo"}, hdr, "")
	if lo2.HighQuality {
		t.Fatalf("expected HighQuality false for Q=lo, got %v", lo2.HighQuality)
	}

	hi2 := s.renderOptionsFromParams(r, map[string]string{"d": "Q=HI"}, hdr, "")
	if !hi2.HighQuality {
		t.Fatalf("expected HighQuality true for Q=HI, got %v", hi2.HighQuality)
	}

	viewport := s.renderOptionsFromParams(r, map[string]string{"d": "w:240;h:320;c:65536;m:1966084;i:1;q:0;f:0;j:0;l:256"}, hdr, "")
	if viewport.ScreenW != 240 || viewport.ScreenH != 320 {
		t.Fatalf("expected 240x320 viewport, got %dx%d", viewport.ScreenW, viewport.ScreenH)
	}

	dOff := s.renderOptionsFromParams(r, map[string]string{"d": "i:2"}, hdr, "")
	if dOff.ImagesOn {
		t.Fatalf("expected ImagesOn false for d=i:2, got %v", dOff.ImagesOn)
	}

	dOn := s.renderOptionsFromParams(r, map[string]string{"d": "i:1"}, hdr, "")
	if !dOn.ImagesOn {
		t.Fatalf("expected ImagesOn true for d=i:1, got %v", dOn.ImagesOn)
	}

	imgOff := s.renderOptionsFromParams(r, map[string]string{"i": "2"}, hdr, "")
	if imgOff.ImagesOn {
		t.Fatalf("expected ImagesOn false for i=2, got %v", imgOff.ImagesOn)
	}

	imgOn := s.renderOptionsFromParams(r, map[string]string{"img": "1"}, hdr, "")
	if !imgOn.ImagesOn {
		t.Fatalf("expected ImagesOn true for img=1, got %v", imgOn.ImagesOn)
	}

	fragParams := map[string]string{"u": "https://example.com/page#__om=page=2&pp=1600&img=2"}
	base, extras := extractOMFragment(fragParams["u"])
	fragParams["u"] = base
	mergeOMOptions(fragParams, extras)
	fragOpt := s.renderOptionsFromParams(r, fragParams, hdr, "")
	if fragOpt.ImagesOn {
		t.Fatalf("expected ImagesOn false from fragment, got %v", fragOpt.ImagesOn)
	}
	if fragOpt.Page != 2 {
		t.Fatalf("expected fragment page=2, got %d", fragOpt.Page)
	}
	if fragOpt.MaxTagsPerPage != 1600 {
		t.Fatalf("expected fragment pp=1600, got %d", fragOpt.MaxTagsPerPage)
	}

	omDevice := s.renderOptionsFromParams(r, map[string]string{
		"om_w": "240",
		"om_h": "320",
		"om_c": "65536",
		"om_m": "16777216",
		"om_l": "256",
	}, hdr, "")
	if omDevice.ScreenW != 240 || omDevice.ScreenH != 320 || omDevice.NumColors != 65536 || omDevice.HeapBytes != 16777216 || omDevice.AlphaLevels != 256 {
		t.Fatalf("unexpected safe OM device options: %+v", omDevice)
	}

	rgb565 := s.renderOptionsFromParams(r, map[string]string{
		"k": "image/x-rgb565", "maxkb": "16", "maxpagekb": "128", "d": "i:1",
	}, hdr, "")
	if rgb565.ImageMIME != oms.RGB565MIME || rgb565.MaxInlineKB != 16 || rgb565.MaxBytesPerPage != 128*1024 || !rgb565.ImagesOn {
		t.Fatalf("unexpected RGB565 options: %+v", rgb565)
	}
}

func TestRenderOptionsNegotiatesOperaClientVersion(t *testing.T) {
	server := newTestServer()
	req := httptest.NewRequest(http.MethodPost, "http://operetta/", nil)

	om3 := server.renderOptionsFromParams(req, map[string]string{
		"o": "285",
		"v": "Opera Mini/3.2",
	}, http.Header{}, "session")
	if om3.ClientVersion != oms.ClientVersion3 || om3.DialectID != "3.2" {
		t.Fatalf("OM3 negotiation=%d/%q", om3.ClientVersion, om3.DialectID)
	}

	om2 := server.renderOptionsFromParams(req, map[string]string{
		"o": "280",
		"v": "Opera Mini/2.0.4509/hifi/woodland/ru",
	}, http.Header{}, "session")
	if om2.ClientVersion != oms.ClientVersion2 || om2.DialectID != "2.0" {
		t.Fatalf("OM2 negotiation=%d/%q", om2.ClientVersion, om2.DialectID)
	}
}

func TestRenderOptionsFromQueryQuality(t *testing.T) {
	hdr := http.Header{}
	s := newTestServer()

	reqHigh := httptest.NewRequest(http.MethodGet, "http://operetta/fetch?url=http://example.com&hq=2", nil)
	high := s.renderOptionsFromQuery(reqHigh, hdr)
	if !high.HighQuality {
		t.Fatalf("expected HighQuality true for hq=2, got %v", high.HighQuality)
	}

	reqLow := httptest.NewRequest(http.MethodGet, "http://operetta/fetch?url=http://example.com&hq=0", nil)
	low := s.renderOptionsFromQuery(reqLow, hdr)
	if low.HighQuality {
		t.Fatalf("expected HighQuality false for hq=0, got %v", low.HighQuality)
	}

	reqImgOn := httptest.NewRequest(http.MethodGet, "http://operetta/fetch?url=http://example.com&img=1", nil)
	imgOn := s.renderOptionsFromQuery(reqImgOn, hdr)
	if !imgOn.ImagesOn {
		t.Fatalf("expected ImagesOn true for img=1, got %v", imgOn.ImagesOn)
	}

	reqImgOffMode := httptest.NewRequest(http.MethodGet, "http://operetta/fetch?url=http://example.com&img=2", nil)
	imgOffMode := s.renderOptionsFromQuery(reqImgOffMode, hdr)
	if imgOffMode.ImagesOn {
		t.Fatalf("expected ImagesOn false for img=2, got %v", imgOffMode.ImagesOn)
	}
}
