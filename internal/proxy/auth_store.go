package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	authCookieName       = "OPERETTA_AUTH"
	defaultMaxAuthStates = 4096
	sessionKeyPrefix     = "sid:"
)

type authTokens struct {
	Code      string
	Prefix    string
	ExpiresAt time.Time
}

type authStore struct {
	mu       sync.Mutex
	sessions map[string]authTokens
	byToken  map[string]string
	ttl      time.Duration
	clock    func() time.Time
	max      int
}

func newAuthStore(clock func() time.Time) *authStore {
	if clock == nil {
		clock = time.Now
	}
	return &authStore{
		sessions: make(map[string]authTokens),
		byToken:  make(map[string]string),
		ttl:      7 * 24 * time.Hour,
		clock:    clock,
		max:      defaultMaxAuthStates,
	}
}

func (s *authStore) get(key string) (authTokens, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked(key, s.clock())
}

func (s *authStore) getLocked(key string, now time.Time) (authTokens, bool) {
	tok, ok := s.sessions[key]
	if !ok {
		return authTokens{}, false
	}
	if !tok.ExpiresAt.IsZero() && !now.Before(tok.ExpiresAt) {
		s.deleteLocked(key, tok)
		return authTokens{}, false
	}
	return tok, true
}

func (s *authStore) put(key string, tok authTokens) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.storeLocked(key, tok, s.clock())
}

func (s *authStore) ensure(key string) authTokens {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	if tok, ok := s.getLocked(key, now); ok {
		return tok
	}
	tok := generateAuthTokens()
	s.storeLocked(key, tok, now)
	return tok
}

func (s *authStore) cookieFor(key string) *http.Cookie {
	value := strings.TrimPrefix(key, sessionKeyPrefix)
	return &http.Cookie{
		Name:     authCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  s.clock().Add(s.ttl),
	}
}

type authSessionContextKey struct{}

func (s *authStore) keyForRequest(r *http.Request) string {
	if r == nil {
		return sessionKeyPrefix + randomHex(32)
	}
	if key, ok := r.Context().Value(authSessionContextKey{}).(string); ok && key != "" {
		return key
	}
	key := ""
	if cookie, err := r.Cookie(authCookieName); err == nil && cookie != nil {
		if value := strings.TrimSpace(cookie.Value); validSessionID(value) {
			key = sessionKeyPrefix + strings.ToLower(value)
		}
	}
	if key == "" {
		key = sessionKeyPrefix + randomHex(32)
	}
	*r = *r.WithContext(context.WithValue(r.Context(), authSessionContextKey{}, key))
	return key
}

func validSessionID(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}

func generateAuthTokens() authTokens {
	return authTokens{Code: randomHex(32), Prefix: "t19-14"}
}

func randomHex(bytesCount int) string {
	buf := make([]byte, bytesCount)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Errorf("secure random generation failed: %w", err))
	}
	return hex.EncodeToString(buf)
}

func (s *authStore) ensureByCode(prefix, code string) (authTokens, bool) {
	if strings.Contains(prefix, ".") && code == "" {
		parts := strings.SplitN(prefix, ".", 2)
		prefix = parts[0]
		if len(parts) > 1 {
			code = parts[1]
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	indexKey := tokenIndexKey(prefix, code)
	if key, ok := s.byToken[indexKey]; ok {
		if tok, valid := s.getLocked(key, now); valid {
			tok.ExpiresAt = now.Add(s.ttl)
			s.sessions[key] = tok
			return tok, true
		}
		delete(s.byToken, indexKey)
	}
	return authTokens{Prefix: prefix, Code: code, ExpiresAt: now.Add(s.ttl)}, false
}

func (s *authStore) updateToken(key string, tok authTokens) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.storeLocked(key, tok, s.clock())
}

func (s *authStore) storeLocked(key string, tok authTokens, now time.Time) {
	if old, exists := s.sessions[key]; exists {
		delete(s.byToken, tokenIndexKey(old.Prefix, old.Code))
	}
	s.pruneLocked(now)
	for len(s.sessions) >= s.max {
		if !s.evictOldestLocked() {
			break
		}
	}
	tok.ExpiresAt = now.Add(s.ttl)
	s.sessions[key] = tok
	s.byToken[tokenIndexKey(tok.Prefix, tok.Code)] = key
}

func (s *authStore) pruneLocked(now time.Time) {
	for key, tok := range s.sessions {
		if !tok.ExpiresAt.IsZero() && !now.Before(tok.ExpiresAt) {
			s.deleteLocked(key, tok)
		}
	}
}

func (s *authStore) evictOldestLocked() bool {
	var selected string
	var expiry time.Time
	for key, tok := range s.sessions {
		if selected == "" || tok.ExpiresAt.Before(expiry) {
			selected = key
			expiry = tok.ExpiresAt
		}
	}
	if selected == "" {
		return false
	}
	s.deleteLocked(selected, s.sessions[selected])
	return true
}

func (s *authStore) deleteLocked(key string, tok authTokens) {
	delete(s.sessions, key)
	indexKey := tokenIndexKey(tok.Prefix, tok.Code)
	if indexedKey, ok := s.byToken[indexKey]; ok && indexedKey == key {
		delete(s.byToken, indexKey)
	}
}

func tokenIndexKey(prefix, code string) string {
	return prefix + "\x00" + code
}
