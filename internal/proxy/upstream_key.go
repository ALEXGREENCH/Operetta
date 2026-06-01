package proxy

import (
	"net/http"
	"strings"
)

// DeriveUpstreamClientKey derives a stable key for outgoing upstream requests.
// It prefers the explicit header we inject, then falls back to auth cookie, then UA.
func DeriveUpstreamClientKey(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Operetta-Client-Key")); v != "" {
		return v
	}
	if c, err := r.Cookie(authCookieName); err == nil && c != nil {
		if v := strings.TrimSpace(c.Value); v != "" {
			return v
		}
	}
	host := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
	if host == "" {
		host = r.RemoteAddr
	}
	return host + "|" + r.Header.Get("User-Agent")
}
