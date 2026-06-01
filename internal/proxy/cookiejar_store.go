package proxy

import (
	"net"
	"net/http"
	"net/http/cookiejar"
	"sync"
)

// CookieJarStore keeps a map of per-client cookie jars keyed by a stable client identifier.
// It allows the proxy and the OMS renderer to share upstream session state transparently.
type CookieJarStore struct {
	mu   sync.Mutex
	jars map[string]http.CookieJar
}

func NewCookieJarStore() *CookieJarStore {
	return &CookieJarStore{jars: make(map[string]http.CookieJar)}
}

// Get returns the existing jar for the provided key or allocates a new one on demand.
func (s *CookieJarStore) Get(key string) http.CookieJar {
	s.mu.Lock()
	defer s.mu.Unlock()
	if jar, ok := s.jars[key]; ok {
		return jar
	}
	jar, _ := cookiejar.New(nil)
	s.jars[key] = jar
	return jar
}

// DeriveClientKey synthesises a stable identifier for the incoming HTTP request combining
// the remote host and User-Agent. The value is suitable for addressing CookieJarStore entries.
func DeriveClientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || host == "" {
		host = r.RemoteAddr
	}
	return host + "|" + r.UserAgent()
}

var CookieJarStoreInstance = NewCookieJarStore()
