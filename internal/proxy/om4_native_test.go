package proxy

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"operetta/protocol/operamini4"
)

func TestNativeOM4NavigationWorksWithoutCapturedOnboarding(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `<html><head><title>Native smoke</title></head><body>`+
			`<p class="noise">remove me</p><p id="result">loading</p>`+
			`<button id="load" onclick="setTimeout(function(){document.getElementById('result').textContent='clean clone works 😀'},100)">load</button>`+
			`</body></html>`)
	}))
	defer upstream.Close()

	sitesDir := t.TempDir()
	config := `{"bake":{"mode":"on","waitAfterLoadMs":300,"waitIdleMs":50,"waitDomIdleMs":50,"maxSettleMs":700,"emojiAsImages":true},` +
		`"rewrite":{"clickSelectors":["#load"],"removeSelectors":["#load",".noise"]}}`
	if err := os.WriteFile(filepath.Join(sitesDir, "127.0.0.1.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	server := New(Config{Logger: log.New(io.Discard, "", 0), SitesDir: sitesDir})
	gateway := httptest.NewServer(server)
	defer gateway.Close()
	defer server.Close()

	request, err := operamini4.DefaultStartupRequest()
	if err != nil {
		t.Fatal(err)
	}
	request.Header = strings.Repeat("0", 64)
	if !replacePlainOM4Target(request, upstream.URL) {
		t.Fatal("default startup request has no replaceable URL")
	}
	client, err := operamini4.NewReferenceClient(gateway.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	frames, err := client.Exchange(ctx, request)
	if err != nil {
		t.Fatalf("native exchange: %v", err)
	}
	secondRequest, err := operamini4.DefaultStartupRequest()
	if err != nil {
		t.Fatal(err)
	}
	secondRequest.Header = request.Header
	if !replacePlainOM4Target(secondRequest, upstream.URL) {
		t.Fatal("second startup request has no replaceable URL")
	}
	frames, err = client.Exchange(ctx, secondRequest)
	if err != nil {
		t.Fatalf("second native exchange on one comparison client: %v", err)
	}
	document, err := operamini4.DecodeApplicationDocument(frames)
	if err != nil {
		t.Fatalf("decode native document: %v", err)
	}
	if document.Header.Title != "Native smoke" {
		t.Fatalf("native document title = %q", document.Header.Title)
	}
	var rendered strings.Builder
	for _, element := range document.Texts {
		rendered.WriteString(element.Text)
		rendered.WriteByte(' ')
	}
	if !strings.Contains(rendered.String(), "clean clone works") {
		t.Fatalf("native text not rendered: %+v", document.Texts)
	}
	if strings.Contains(rendered.String(), "remove me") || strings.Contains(rendered.String(), "loading") {
		t.Fatalf("native OM4 bypassed site rewrite: %q", rendered.String())
	}
	images := 0
	for _, drawing := range document.Drawings {
		if drawing.Kind == 'I' {
			images++
			resource := officialImageResource(document.Page, drawing.ImagePointer)
			if len(resource) == 0 || !bytes.HasPrefix(resource, []byte("\x89PNG\r\n\x1a\n")) {
				t.Fatalf("emoji resource is not PNG: bytes=%d prefix=%x", len(resource), resource[:min(len(resource), 8)])
			}
			if len(resource) > 2048 {
				t.Fatalf("emoji PNG is too large for a tiny glyph: %d bytes", len(resource))
			}
		}
	}
	if images == 0 {
		t.Fatalf("rasterized emoji did not reach native OM4 drawings: %+v", document.Drawings)
	}
}

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

func TestSpacesMobileHostSurvivesWorldRedirects(t *testing.T) {
	for _, host := range []string{"world82.spcs.bio", "WORLD83.SPCS.BIO", "world1.spcs.bio"} {
		if !isSpacesMobileHost(host) {
			t.Fatalf("Spaces mobile host not recognized: %q", host)
		}
	}
	for _, host := range []string{"spcs.bio", "world.spcs.bio", "evilworld83.spcs.bio", "world83.example.com"} {
		if isSpacesMobileHost(host) {
			t.Fatalf("non-Spaces host recognized: %q", host)
		}
	}
}

func TestPlaceSectionTextsIgnoresDecorativeSeparators(t *testing.T) {
	lines := []operamini4.WelcomeLine{{Text: "·"}, {Text: "timestamp"}, {Text: "author"}}
	placed := make([]bool, len(lines))
	place := func(index, x, y, width, height int) {
		lines[index].X, lines[index].Y = x, y
		lines[index].Width, lines[index].Height = width, height
		placed[index] = true
	}
	placeSectionTexts(lines, placed, 0, len(lines), [][4]int{
		{10, 20, 30, 14}, {40, 50, 60, 14},
	}, place)
	if placed[0] || !placed[1] || !placed[2] {
		t.Fatalf("separator/data placement = %v", placed)
	}
	if lines[1].X != 10 || lines[1].Y != 20 || lines[2].X != 40 || lines[2].Y != 50 {
		t.Fatalf("section text shifted by separator: %+v", lines)
	}
}

func TestEllipsizeNativeTextFitsBanner(t *testing.T) {
	got := ellipsizeNativeText("🎮Мини игры: Шашки, шахматы, раскраски...", 205)
	if nativeTextWidth(got) > 205 || !strings.HasSuffix(got, "…") || !strings.Contains(got, "Мини игры") {
		t.Fatalf("ellipsized banner = %q width=%d", got, nativeTextWidth(got))
	}
	if got := ellipsizeNativeText("Короткий текст", 205); got != "Короткий текст" {
		t.Fatalf("short banner changed to %q", got)
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
