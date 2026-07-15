package proxy

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"operetta/oms"
)

func TestPageCacheKeyIsSessionAndDialectScoped(t *testing.T) {
	base := &oms.RenderOptions{
		CachePartition: "session-a",
		ImageMIME:      "image/jpeg",
		ImagesOn:       true,
		ScreenW:        240,
		ClientVersion:  oms.ClientVersion2,
		Compression:    oms.CompressionDeflate,
		ReqHeaders:     http.Header{"Accept-Language": {"en"}},
	}
	key := cacheKey("https://example.test/private", base)
	if key == "" {
		t.Fatal("expected cache key for a scoped session")
	}
	otherSession := *base
	otherSession.CachePartition = "session-b"
	if got := cacheKey("https://example.test/private", &otherSession); got == key {
		t.Fatal("two sessions received the same cache key")
	}
	otherDialect := *base
	otherDialect.ClientVersion = oms.ClientVersion3
	if got := cacheKey("https://example.test/private", &otherDialect); got == key {
		t.Fatal("two protocol dialects received the same cache key")
	}
	unscoped := *base
	unscoped.CachePartition = ""
	if got := cacheKey("https://example.test/private", &unscoped); got != "" {
		t.Fatalf("unscoped cache key=%q, want disabled", got)
	}
}

func TestPageCacheNeverReplaysOriginCookies(t *testing.T) {
	cache := newPageCache(time.Now)
	page := oms.NewPage()
	page.AddString("1/https://example.test/")
	for i := 0; i < 8; i++ {
		page.AddText("item")
		page.AddBreak()
	}
	page.SetCookies = []string{"OPERETTA_AUTH=attacker; Path=/"}
	page.Finalize()
	page.CachePacked = append([]byte(nil), page.Data...)

	storeOptions := &oms.RenderOptions{
		CachePartition: "session-a",
		ClientVersion:  oms.ClientVersion2,
		Compression:    oms.CompressionDeflate,
		MaxTagsPerPage: 2,
		ReqHeaders:     http.Header{},
	}
	cache.Store("https://example.test/", storeOptions, http.Header{}, page)
	selectOptions := *storeOptions
	selectOptions.Page = 2
	_, cookies, _, _, _, ok := cache.Select("https://example.test/", &selectOptions)
	if !ok {
		t.Fatal("expected cached second page")
	}
	if len(cookies) != 0 {
		t.Fatalf("cache replayed origin cookies: %v", cookies)
	}
}

func TestPageCacheKeepsNegotiatedPaginationBudget(t *testing.T) {
	cache := newPageCache(time.Now)
	page := oms.NewPage()
	page.SetTransport(oms.ClientVersion3, oms.CompressionNone)
	page.AddString("1/https://example.test/large")
	for i := 0; i < 32; i++ {
		page.AddText(strings.Repeat("payload", 600))
		page.AddBreak()
	}
	page.Finalize()

	options := &oms.RenderOptions{
		CachePartition:  "stable-pagination",
		ClientVersion:   oms.ClientVersion3,
		Compression:     oms.CompressionNone,
		MaxTagsPerPage:  700,
		MaxBytesPerPage: 96 * 1024,
		ReqHeaders:      http.Header{},
	}
	_, _, expectedTotal, err := oms.SelectOMSPartFromPackedWithBudget(
		page.CachePacked, 2, options.MaxTagsPerPage, options.MaxBytesPerPage,
	)
	if err != nil {
		t.Fatalf("expected pagination: %v", err)
	}
	_, _, legacyTotal, err := oms.SelectOMSPartFromPacked(
		page.CachePacked, 2, options.MaxTagsPerPage,
	)
	if err != nil {
		t.Fatalf("legacy pagination: %v", err)
	}
	if expectedTotal >= legacyTotal {
		t.Fatalf("fixture does not distinguish budgets: negotiated=%d legacy=%d", expectedTotal, legacyTotal)
	}

	cache.Store("https://example.test/large", options, http.Header{}, page)
	selectOptions := *options
	selectOptions.Page = 2
	_, _, current, cachedTotal, _, ok := cache.Select(
		"https://example.test/large", &selectOptions,
	)
	if !ok {
		t.Fatal("expected cached page")
	}
	if current != 2 || cachedTotal != expectedTotal {
		t.Fatalf("cache changed pagination to %d/%d, want 2/%d", current, cachedTotal, expectedTotal)
	}
}

func TestCookieJarRejectsPublicSuffixCookie(t *testing.T) {
	store := NewCookieJarStore()
	jar := store.Get("session")
	source := mustURL(t, "https://attacker.example/")
	jar.SetCookies(source, []*http.Cookie{{Name: "poison", Value: "1", Domain: ".com", Path: "/"}})
	if got := jar.Cookies(mustURL(t, "https://victim.com/")); len(got) != 0 {
		t.Fatalf("public-suffix cookie escaped origin: %v", got)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return u
}
