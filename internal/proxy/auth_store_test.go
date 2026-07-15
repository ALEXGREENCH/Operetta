package proxy

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestAuthSessionKeyIsOpaqueAndStableWithinRequest(t *testing.T) {
	store := newAuthStore(time.Now)
	req := httptest.NewRequest(http.MethodPost, "http://operetta/", nil)
	req.RemoteAddr = "192.0.2.10:1234"
	req.Header.Set("User-Agent", "shared-device")
	first := store.keyForRequest(req)
	second := store.keyForRequest(req)
	if first != second {
		t.Fatalf("request session changed: %q != %q", first, second)
	}
	if !validSessionID(store.cookieFor(first).Value) {
		t.Fatalf("cookie does not contain an opaque session id: %q", store.cookieFor(first).Value)
	}
}

func TestExpiredAuthCodeCannotBeResurrected(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	store := newAuthStore(func() time.Time { return now })
	store.updateToken("sid:test", authTokens{Prefix: "p", Code: "c"})
	if _, ok := store.ensureByCode("p", "c"); !ok {
		t.Fatal("fresh token was not found")
	}
	now = now.Add(store.ttl + time.Second)
	if _, ok := store.ensureByCode("p", "c"); ok {
		t.Fatal("expired token was resurrected")
	}
}

func TestEnsureIsAtomic(t *testing.T) {
	store := newAuthStore(time.Now)
	const workers = 16
	results := make(chan authTokens, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- store.ensure("sid:shared")
		}()
	}
	wg.Wait()
	close(results)
	var first authTokens
	for tok := range results {
		if first.Code == "" {
			first = tok
			continue
		}
		if tok.Code != first.Code || tok.Prefix != first.Prefix {
			t.Fatalf("concurrent ensure returned different tokens: %+v / %+v", first, tok)
		}
	}
}
