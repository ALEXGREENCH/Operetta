// Package origin defines protocol-agnostic acquisition results and ports.
package origin

import "net/http"

// Response is an origin resource before it is transformed for a small client.
// Body contains the decoded HTTP entity when available; RawBody preserves the
// bytes read from the origin.
type Response struct {
	URL           string
	Body          []byte
	RawBody       []byte
	TransferBytes int
	Header        http.Header
	Status        int
	ContentLength int64
	SetCookies    []string
}
