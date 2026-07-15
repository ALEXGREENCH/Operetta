package proxy

import (
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"sync"

	"golang.org/x/net/publicsuffix"
)

// CookieJarStore keeps a map of per-client cookie jars keyed by a stable client identifier.
// It allows the proxy and the OMS renderer to share upstream session state transparently.
type CookieJarStore struct {
	mu   sync.Mutex
	jars map[string]http.CookieJar
}

const maxCookieJars = 2048

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
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		panic(fmt.Errorf("create upstream cookie jar: %w", err))
	}
	for len(s.jars) >= maxCookieJars {
		for candidate := range s.jars {
			delete(s.jars, candidate)
			break
		}
	}
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
