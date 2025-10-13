package proxy

import (
    "crypto/rand"
    "crypto/sha256"
    "encoding/hex"
    "net/http"
    "strings"
    "sync"
    "time"
)

const authCookieName = "OPERETTA_AUTH"

type authTokens struct {
    Code      string
    Prefix    string
    ExpiresAt time.Time
}

type authStore struct {
    mu       sync.Mutex
    sessions map[string]authTokens
    ttl      time.Duration
    clock    func() time.Time
}

func newAuthStore(clock func() time.Time) *authStore {
    if clock == nil {
        clock = time.Now
    }
    return &authStore{sessions: make(map[string]authTokens), ttl: 7 * 24 * time.Hour, clock: clock}
}

func (s *authStore) get(key string) (authTokens, bool) {
    s.mu.Lock()
    defer s.mu.Unlock()
    tok, ok := s.sessions[key]
    if !ok {
        return authTokens{}, false
    }
    if !tok.ExpiresAt.IsZero() && s.clock().After(tok.ExpiresAt) {
        delete(s.sessions, key)
        return authTokens{}, false
    }
    return tok, true
}

func (s *authStore) put(key string, tok authTokens) {
    s.mu.Lock()
    tok.ExpiresAt = s.clock().Add(s.ttl)
    s.sessions[key] = tok
    s.mu.Unlock()
}

func (s *authStore) ensure(key string) authTokens {
    if tok, ok := s.get(key); ok {
        return tok
    }
    tok := generateAuthTokens()
    s.put(key, tok)
    return tok
}

func (s *authStore) cookieFor(key string) *http.Cookie {
    return &http.Cookie{
        Name:     authCookieName,
        Value:    key,
        Path:     "/",
        HttpOnly: true,
        SameSite: http.SameSiteLaxMode,
        Expires:  s.clock().Add(s.ttl),
    }
}

func clientAuthKeyFromRequest(r *http.Request) string {
    if c, err := r.Cookie(authCookieName); err == nil && c != nil && strings.TrimSpace(c.Value) != "" {
        return c.Value
    }
    return deriveClientKey(r)
}

func generateAuthTokens() authTokens {
    prefix := "t19-14"
    buf := make([]byte, 32)
    _, _ = rand.Read(buf)
    sum := sha256.Sum256(buf)
    code := hex.EncodeToString(sum[:])
    return authTokens{Code: code, Prefix: prefix}
}

