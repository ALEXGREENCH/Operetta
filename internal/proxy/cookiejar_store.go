package proxy

import (
	"net"
	"net/http"
	"net/http/cookiejar"
	"sync"
)

type cookieJarStore struct {
	mu   sync.Mutex
	jars map[string]http.CookieJar
}

func newCookieJarStore() *cookieJarStore {
	return &cookieJarStore{jars: make(map[string]http.CookieJar)}
}

func (s *cookieJarStore) Get(key string) http.CookieJar {
	s.mu.Lock()
	defer s.mu.Unlock()
	if jar, ok := s.jars[key]; ok {
		return jar
	}
	jar, _ := cookiejar.New(nil)
	s.jars[key] = jar
	return jar
}

func deriveClientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || host == "" {
		host = r.RemoteAddr
	}
	return host + "|" + r.UserAgent()
}
