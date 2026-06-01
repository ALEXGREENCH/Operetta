package proxy

import (
	"log"
	"net/http"
)

func withLogging(logger *log.Logger, next http.Handler) http.Handler {
	if logger == nil {
		logger = log.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.Printf("REQ %s %s Host=%s UA=%q From=%s", r.Method, r.URL.String(), r.Host, r.UserAgent(), r.RemoteAddr)
		logHeader := func(name string) {
			if v := r.Header.Get(name); v != "" {
				logger.Printf("HDR %s: %s", name, v)
			}
		}
		logHeader("X-Online-Host")
		logHeader("Proxy-Connection")
		logHeader("Connection")
		logHeader("Content-Type")
		logHeader("Content-Length")

		next.ServeHTTP(w, r)
	})
}
