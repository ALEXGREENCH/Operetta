package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

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
