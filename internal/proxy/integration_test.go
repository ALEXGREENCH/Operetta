package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"operetta/oms"
)

func newIntegrationProxy(t *testing.T) *Server {
	t.Helper()
	cfg := Config{
		IndexHTML:         defaultIndexHTML,
		Logger:            log.New(io.Discard, "", 0),
		Clock:             time.Now,
		SitesDir:          t.TempDir(),
		EnableDiagnostics: true,
	}
	s := New(cfg)
	s.cookieJars = NewCookieJarStore()
	s.auth = newAuthStore(time.Now)
	s.cache = newPageCache(time.Now)
	s.forms = newFormStore()

	prevJar := oms.ProxyCookieJarStore
	prevKey := oms.ProxyDeriveClientKey
	oms.ProxyCookieJarStore = s.cookieJars
	oms.ProxyDeriveClientKey = DeriveUpstreamClientKey
	t.Cleanup(func() {
		oms.ProxyCookieJarStore = prevJar
		oms.ProxyDeriveClientKey = prevKey
	})
	return s
}

func newProxyClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	return &http.Client{Jar: jar}
}

func buildOperaMiniPayload(target, deviceBlock, formBody string) []byte {
	return buildOperaMiniPayloadWithPart(0, target, deviceBlock, formBody)
}

func buildOperaMiniPayloadWithPart(part int, target, deviceBlock, formBody string) []byte {
	fields := []string{
		"k=image/jpeg",
		"o=280",
		"u=/obml/" + strconv.Itoa(part) + "/" + target,
		"q=ru",
		"v=Opera Mini/2.0.4509/hifi/woodland/ru",
		"i=Opera/8.01 (J2ME/MIDP; Opera Mini/2.0.4509/1630; ru; U; ssr)",
		"s=-1",
		"n=1",
		"A=CLDC-1.1",
		"B=MIDP-2.0",
		"C=j2me",
		"D=ru",
		"E=ISO-8859-1",
		"d=" + deviceBlock,
		"g=1",
		"b=operetta-test",
		"y=ru",
		"t=0",
		"w=1;0",
		"e=def",
	}
	if strings.TrimSpace(formBody) != "" {
		fields = append(fields, "j="+formBody)
	}
	return []byte(strings.Join(fields, "\x00") + "\x00")
}

func postOperaMini(t *testing.T, client *http.Client, proxyURL, target, deviceBlock, formBody string) (*http.Response, []byte) {
	return postOperaMiniPart(t, client, proxyURL, 0, target, deviceBlock, formBody)
}

func postOperaMiniPart(t *testing.T, client *http.Client, proxyURL string, part int, target, deviceBlock, formBody string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, proxyURL+"/", bytes.NewReader(buildOperaMiniPayloadWithPart(part, target, deviceBlock, formBody)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/xml")
	req.Header.Set("User-Agent", "OperaMiniTest/1.0")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", proxyURL, err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("ReadAll response: %v", err)
	}
	return resp, body
}

func mustContain(t *testing.T, items []string, want string) {
	t.Helper()
	for _, item := range items {
		if item == want || strings.Contains(item, want) {
			return
		}
	}
	t.Fatalf("expected %q in %v", want, items)
}

func TestHandleRootOperaMiniPostProducesSemanticOMS(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!DOCTYPE html><html><head><title>Fixture article</title></head><body><article><h1>Operetta headline</h1><p>Readable article body.</p><a href="/more">Read more</a></article></body></html>`)
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(newIntegrationProxy(t))
	defer proxy.Close()

	client := newProxyClient(t)
	device := "w:240;h:320;c:65536;m:1966084;i:1;q:0;f:0;j:0;l:256"
	resp, body := postOperaMini(t, client, proxy.URL, upstream.URL+"/article", device, "")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%q", resp.StatusCode, string(body))
	}
	if got := resp.Header.Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("content-type=%q", got)
	}
	if !resp.Close {
		t.Fatalf("expected response.Close=true")
	}
	if got := resp.Header.Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Fatalf("content-length=%q want=%d", got, len(body))
	}

	analysis := analyzeOMS(body)
	if !analysis.ParseOK {
		t.Fatalf("parse error: %s", analysis.ParseError)
	}
	if analysis.LastTag != "Q" {
		t.Fatalf("last tag=%q", analysis.LastTag)
	}
	if analysis.Summary.InitialURL != upstream.URL+"/article" {
		t.Fatalf("initial url=%q", analysis.Summary.InitialURL)
	}
	mustContain(t, analysis.Summary.Text, "Operetta headline")
	mustContain(t, analysis.Summary.Text, "Readable article body.")
	mustContain(t, analysis.Summary.Links, "0/"+upstream.URL+"/more")
}

func TestHandleRootOperaMiniPostImagesOffUsesAltFallback(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!DOCTYPE html><html><body><img src="/hero.png" alt="Hero"><p>After image</p></body></html>`)
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(newIntegrationProxy(t))
	defer proxy.Close()

	client := newProxyClient(t)
	device := "w:240;h:320;c:65536;m:1966084;i:2;q:0;f:0;j:0;l:256"
	_, body := postOperaMini(t, client, proxy.URL, upstream.URL+"/images-off", device, "")

	analysis := analyzeOMS(body)
	if !analysis.ParseOK {
		t.Fatalf("parse error: %s", analysis.ParseError)
	}
	if analysis.Counts["I"] != 0 || analysis.Counts["J"] != 0 {
		t.Fatalf("expected no inline/placeholders, counts=%v", analysis.Counts)
	}
	mustContain(t, analysis.Summary.Text, "[Hero]")
	mustContain(t, analysis.Summary.Text, "After image")
}

func TestHandleRootOperaMiniPostAugmentsHiddenFormAndCookies(t *testing.T) {
	var submitMethod string
	var submitCookie string
	var submitBody url.Values

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/form":
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "abc123", Path: "/"})
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, `<!DOCTYPE html><html><body><form action="/submit" method="post"><input type="hidden" name="csrf" value="token123"><input type="text" name="q" value=""><input type="submit" name="go" value="Send"></form></body></html>`)
		case "/submit":
			submitMethod = r.Method
			submitCookie = r.Header.Get("Cookie")
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("ReadAll submit body: %v", err)
			}
			submitBody, err = url.ParseQuery(string(raw))
			if err != nil {
				t.Fatalf("ParseQuery submit body: %v", err)
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, `<html><body><p>Submitted OK</p></body></html>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(newIntegrationProxy(t))
	defer proxy.Close()

	client := newProxyClient(t)
	device := "w:240;h:320;c:65536;m:1966084;i:1;q:0;f:0;j:0;l:256"

	_, firstBody := postOperaMini(t, client, proxy.URL, upstream.URL+"/form", device, "")
	firstAnalysis := analyzeOMS(firstBody)
	if !firstAnalysis.ParseOK {
		t.Fatalf("first parse error: %s", firstAnalysis.ParseError)
	}
	if len(firstAnalysis.Summary.Forms) == 0 {
		t.Fatalf("expected form summary, got %+v", firstAnalysis.Summary)
	}
	if got := firstAnalysis.Summary.Forms[0].Hidden["csrf"]; got != "token123" {
		t.Fatalf("hidden csrf=%q", got)
	}
	form := firstAnalysis.Summary.Forms[0]
	if form.Action != upstream.URL+"/submit" {
		t.Fatalf("form action=%q", form.Action)
	}
	if form.Method != "2" {
		t.Fatalf("form method marker=%q", form.Method)
	}

	submitted := url.Values{}
	submitted.Set(form.Action, form.Method)
	submitted.Set("q", "opera")
	submitted.Set("go", "Send")
	_, secondBody := postOperaMini(t, client, proxy.URL, upstream.URL+"/form", device, submitted.Encode())
	secondAnalysis := analyzeOMS(secondBody)
	if !secondAnalysis.ParseOK {
		t.Fatalf("second parse error: %s", secondAnalysis.ParseError)
	}
	mustContain(t, secondAnalysis.Summary.Text, "Submitted OK")

	if submitMethod != http.MethodPost {
		t.Fatalf("submit method=%q", submitMethod)
	}
	if submitBody.Get("q") != "opera" {
		t.Fatalf("submit q=%q", submitBody.Get("q"))
	}
	if submitBody.Get("csrf") != "token123" {
		t.Fatalf("submit csrf=%q", submitBody.Get("csrf"))
	}
	if !strings.Contains(submitCookie, "sid=abc123") {
		t.Fatalf("submit cookie=%q", submitCookie)
	}
}

func TestHandleRootOperaMiniPostSkipsPrefetchForExplicitAction(t *testing.T) {
	var prefetchGETs int
	var submitMethod string
	var submitBody url.Values

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/make":
			if r.Method == http.MethodGet && r.URL.RawQuery == "" {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = io.WriteString(w, `<!DOCTYPE html><html><body><form action="/make?captcha=1&amp;&amp;nonce" method="post"><input name="secretKey" maxlength="4"><input type="submit" name="notify" value="OK"></form></body></html>`)
				return
			}
			if r.Method == http.MethodGet {
				prefetchGETs++
			}
			fallthrough
		default:
			submitMethod = r.Method
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("ReadAll submit body: %v", err)
			}
			submitBody, err = url.ParseQuery(string(raw))
			if err != nil {
				t.Fatalf("ParseQuery submit body: %v", err)
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, `<html><body><p>Captcha accepted</p></body></html>`)
		}
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(newIntegrationProxy(t))
	defer proxy.Close()

	client := newProxyClient(t)
	device := "w:240;h:320;c:65536;m:1966084;i:1;q:0;f:0;j:0;l:256"

	_, firstBody := postOperaMini(t, client, proxy.URL, upstream.URL+"/make", device, "")
	firstAnalysis := analyzeOMS(firstBody)
	if !firstAnalysis.ParseOK {
		t.Fatalf("first parse error: %s", firstAnalysis.ParseError)
	}
	if len(firstAnalysis.Summary.Forms) == 0 {
		t.Fatalf("expected form summary, got %+v", firstAnalysis.Summary)
	}
	form := firstAnalysis.Summary.Forms[0]
	if form.Method != "2" {
		t.Fatalf("form method marker=%q", form.Method)
	}

	submitted := url.Values{}
	submitted.Set(form.Action, form.Method)
	submitted.Set("secretKey", "7972")
	submitted.Set("notify", "OK")
	_, secondBody := postOperaMini(t, client, proxy.URL, upstream.URL+"/make", device, submitted.Encode())
	secondAnalysis := analyzeOMS(secondBody)
	if !secondAnalysis.ParseOK {
		t.Fatalf("second parse error: %s", secondAnalysis.ParseError)
	}
	mustContain(t, secondAnalysis.Summary.Text, "Captcha accepted")

	if prefetchGETs != 0 {
		t.Fatalf("unexpected prefetch GET count=%d", prefetchGETs)
	}
	if submitMethod != http.MethodPost {
		t.Fatalf("submit method=%q", submitMethod)
	}
	if submitBody.Get("secretKey") != "7972" {
		t.Fatalf("submit secretKey=%q", submitBody.Get("secretKey"))
	}
}

func TestHandleFetchPaginationServesCachedPartsWithNav(t *testing.T) {
	var body strings.Builder
	body.WriteString(`<!DOCTYPE html><html><body>`)
	for i := 0; i < 160; i++ {
		body.WriteString(`<p>Paragraph `)
		body.WriteString(strconv.Itoa(i))
		body.WriteString(` with enough text to force pagination in the OMS stream.</p>`)
	}
	body.WriteString(`</body></html>`)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, body.String())
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(newIntegrationProxy(t))
	defer proxy.Close()

	client := newProxyClient(t)
	firstURL := proxy.URL + "/fetch?url=" + url.QueryEscape(upstream.URL+"/long") + "&pp=24&w=240&h=320&img=1"
	firstResp, err := client.Get(firstURL)
	if err != nil {
		t.Fatalf("GET first page: %v", err)
	}
	_, _ = io.Copy(io.Discard, firstResp.Body)
	firstResp.Body.Close()

	secondURL := firstURL + "&page=2"
	secondResp, err := client.Get(secondURL)
	if err != nil {
		t.Fatalf("GET second page: %v", err)
	}
	secondBody, err := io.ReadAll(secondResp.Body)
	secondResp.Body.Close()
	if err != nil {
		t.Fatalf("ReadAll second page: %v", err)
	}

	if got := secondResp.Header.Get("X-Operetta-Page"); got != "2" {
		t.Fatalf("X-Operetta-Page=%q", got)
	}
	pageCount, err := strconv.Atoi(secondResp.Header.Get("X-Operetta-Pages"))
	if err != nil || pageCount < 2 {
		t.Fatalf("X-Operetta-Pages=%q err=%v", secondResp.Header.Get("X-Operetta-Pages"), err)
	}

	analysis := analyzeOMS(secondBody)
	if !analysis.ParseOK {
		t.Fatalf("parse error: %s", analysis.ParseError)
	}
	if !strings.Contains(analysis.Summary.InitialURL, "__om=") {
		t.Fatalf("initial url=%q", analysis.Summary.InitialURL)
	}
	mustContain(t, analysis.Summary.Links, "__om=")
}

func TestHandleRootLegacyPartPrefixDoesNotAdvancePagination(t *testing.T) {
	var body strings.Builder
	body.WriteString(`<!DOCTYPE html><html><body>`)
	for i := 0; i < 120; i++ {
		body.WriteString(`<p>Cached paragraph `)
		body.WriteString(strconv.Itoa(i))
		body.WriteString(` with enough text to force several compact OMS parts.</p>`)
	}
	body.WriteString(`</body></html>`)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, body.String())
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(newIntegrationProxy(t))
	defer proxy.Close()

	client := newProxyClient(t)
	device := "w:240;h:320;c:65536;m:1966084;i:1;q:0;f:0;j:0;l:256"
	targetWithPrefs := upstream.URL + "/long#__om=pp=24"
	firstResp, firstBody := postOperaMini(t, client, proxy.URL, targetWithPrefs, device, "")
	if firstResp.StatusCode != http.StatusOK {
		t.Fatalf("first status=%d body=%q", firstResp.StatusCode, string(firstBody))
	}

	navigatedResp, navigatedBody := postOperaMiniPart(t, client, proxy.URL, 1, targetWithPrefs, device, "")
	if navigatedResp.StatusCode != http.StatusOK {
		t.Fatalf("navigated status=%d body=%q", navigatedResp.StatusCode, string(navigatedBody))
	}
	navigatedAnalysis := analyzeOMS(navigatedBody)
	if !navigatedAnalysis.ParseOK {
		t.Fatalf("navigated parse error: %s", navigatedAnalysis.ParseError)
	}
	if got := navigatedAnalysis.V2BE["partCur"]; got != 1 {
		t.Fatalf("legacy /obml/1 selected part=%d", got)
	}
	if strings.Contains(navigatedAnalysis.Summary.InitialURL, "__om=") {
		t.Fatalf("legacy /obml/1 prefix advanced initial url=%q", navigatedAnalysis.Summary.InitialURL)
	}

	explicitSecond := upstream.URL + "/long#__om=page=2&pp=24"
	secondResp, secondBody := postOperaMiniPart(t, client, proxy.URL, 1, explicitSecond, device, "")
	if got := secondResp.Header.Get("X-Operetta-Page"); got != "2" {
		t.Fatalf("explicit X-Operetta-Page=%q", got)
	}
	analysis := analyzeOMS(secondBody)
	if !analysis.ParseOK {
		t.Fatalf("parse error: %s", analysis.ParseError)
	}
	if !strings.Contains(analysis.Summary.InitialURL, "__om=") {
		t.Fatalf("initial url=%q", analysis.Summary.InitialURL)
	}
	mustContain(t, analysis.Summary.Links, "__om=")
	mustContain(t, analysis.Summary.Links, "c%3D65536")
	mustContain(t, analysis.Summary.Links, "m%3D1966084")
	mustContain(t, analysis.Summary.Links, "l%3D256")
}

func TestHandleValidateIncludesSemanticSummary(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!DOCTYPE html><html><body><a href="/docs">Docs</a><form action="/submit"><input type="hidden" name="csrf" value="validate-token"><input type="text" name="q" value=""></form><p>Validate me</p></body></html>`)
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(newIntegrationProxy(t))
	defer proxy.Close()

	client := newProxyClient(t)
	resp, err := client.Get(proxy.URL + "/validate?url=" + url.QueryEscape(upstream.URL+"/validate"))
	if err != nil {
		t.Fatalf("GET /validate: %v", err)
	}
	defer resp.Body.Close()

	var result validateResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("Decode validate JSON: %v", err)
	}
	if !result.Full.ParseOK {
		t.Fatalf("full parse error: %s", result.Full.ParseError)
	}
	if !result.Compact.ParseOK {
		t.Fatalf("compact parse error: %s", result.Compact.ParseError)
	}
	if result.Full.Summary.InitialURL != upstream.URL+"/validate" {
		t.Fatalf("full initial url=%q", result.Full.Summary.InitialURL)
	}
	mustContain(t, result.Full.Summary.Links, "0/"+upstream.URL+"/docs")
	mustContain(t, result.Full.Summary.Text, "Validate me")
	if len(result.Full.Summary.Forms) == 0 || result.Full.Summary.Forms[0].Hidden["csrf"] != "validate-token" {
		t.Fatalf("unexpected forms summary: %+v", result.Full.Summary.Forms)
	}
}
