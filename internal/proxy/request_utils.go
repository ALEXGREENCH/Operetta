package proxy

import (
	"bytes"
	"net/http"
	"strings"
)

func parseNullKV(b []byte) map[string]string {
	out := make(map[string]string)
	if len(b) == 0 {
		return out
	}
	parts := bytes.Split(b, []byte{0})
	for _, part := range parts {
		if len(part) == 0 {
			continue
		}
		kv := string(part)
		if i := strings.IndexByte(kv, '='); i != -1 {
			out[kv[:i]] = kv[i+1:]
		}
	}
	return out
}

func normalizeObmlURL(u string) string {
	s := strings.TrimSpace(u)
	if s == "" {
		return s
	}
	s = urlDecode(s)
	if strings.HasPrefix(s, "/obml/") {
		s = s[len("/obml/"):]
		i := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i < len(s) && s[i] == '/' {
			s = s[i+1:]
		} else if i > 0 {
			s = s[i:]
		}
	}
	if strings.HasPrefix(s, "0/") {
		s = s[2:]
	}
	lower := strings.ToLower(s)
	if !(strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")) {
		s = "http://" + s
	}
	return s
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func keysOf(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}
