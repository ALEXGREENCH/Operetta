package proxy

import (
	"net/http/httptest"
	"strings"
	"testing"

	"operetta/protocol/operamini4"
)

func TestOM4CookieJarKeyUsesOpaqueSessionHeader(t *testing.T) {
	httpRequest := httptest.NewRequest("POST", "http://127.0.0.1:8081/", nil)
	httpRequest.RemoteAddr = "192.0.2.10:1234"
	session := &operamini4.SessionRequest{Header: "secret-session-header"}
	first := om4CookieJarKey(httpRequest, session)
	second := om4CookieJarKey(httpRequest, session)
	if first != second || !strings.HasPrefix(first, "OM4|") || strings.Contains(first, session.Header) {
		t.Fatalf("unsafe or unstable OM4 jar key %q / %q", first, second)
	}
}

func TestResolveNativeURLStripsLegacyOMSNavigationPrefix(t *testing.T) {
	got := resolveNativeURL("https://sefan.ru/", "0/https://sefan.ru/best/?lan=ru")
	if got != "https://sefan.ru/best/?lan=ru" {
		t.Fatalf("resolveNativeURL=%q", got)
	}
	got = resolveNativeURL("https://sefan.ru/dir/", "0/next.html")
	if got != "https://sefan.ru/dir/next.html" {
		t.Fatalf("resolveNativeURL relative=%q", got)
	}
}

func TestNativeFramesChallengeIsOnlySentOnce(t *testing.T) {
	response := []operamini4.Frame{
		{Type: 15, Channel: 0, Payload: make([]byte, 8)},
		{Type: 17, Channel: 0},
		{Type: 9, Channel: 1},
	}
	first := nativeFramesForRequest(&operamini4.SessionRequest{}, response)
	if len(first) != 3 || first[0].Type != 15 {
		t.Fatalf("initial response lost challenge: %+v", first)
	}
	subsequent := nativeFramesForRequest(&operamini4.SessionRequest{Frames: []operamini4.Frame{{Type: 15, Channel: 0, Payload: make([]byte, 8)}}}, response)
	if len(subsequent) != 2 || subsequent[0].Type != 17 {
		t.Fatalf("subsequent response retained challenge: %+v", subsequent)
	}
}
