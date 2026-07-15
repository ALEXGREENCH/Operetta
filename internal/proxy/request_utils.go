package proxy

import (
	"bytes"
	"net/http"
	"net/url"
	"strconv"
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
	normalized, _, _ := normalizeObmlURLWithPart(u)
	return normalized
}

func normalizeObmlURLWithPart(u string) (string, int, bool) {
	s := strings.TrimSpace(u)
	if s == "" {
		return s, 0, false
	}
	s = urlDecode(s)
	part := 0
	hasPart := false
	if strings.HasPrefix(s, "/obml/") {
		s = s[len("/obml/"):]
		i := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i > 0 {
			if n, err := strconv.Atoi(s[:i]); err == nil {
				part = n
				hasPart = true
			}
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
	return s, part, hasPart
}

func extractOMFragment(raw string) (string, map[string]string) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw, nil
	}
	var out map[string]string
	addExtras := func(data string) {
		data = strings.TrimSpace(data)
		if data == "" {
			return
		}
		vals, err := url.ParseQuery(data)
		if err != nil {
			return
		}
		if out == nil {
			out = make(map[string]string, len(vals))
		}
		for k, vs := range vals {
			if len(vs) > 0 {
				out[k] = vs[0]
			}
		}
	}

	q := parsed.Query()
	if data := q.Get("__om"); strings.TrimSpace(data) != "" {
		addExtras(data)
		q.Del("__om")
	}
	// Older encoded parts use __p solely as a cache/history discriminator.
	// Accept it as an explicit page on reload, but never forward it upstream.
	if part := strings.TrimSpace(q.Get("__p")); part != "" {
		if _, exists := out["page"]; !exists {
			if n, err := strconv.Atoi(part); err == nil && n > 0 {
				if out == nil {
					out = make(map[string]string)
				}
				out["page"] = strconv.Itoa(n)
			}
		}
		q.Del("__p")
	}
	parsed.RawQuery = q.Encode()

	frag := strings.TrimSpace(parsed.Fragment)
	parsed.Fragment = ""
	base := parsed.String()
	if frag == "" {
		return base, out
	}
	if !strings.HasPrefix(frag, "__om=") {
		return base, out
	}
	data := strings.TrimPrefix(frag, "__om=")
	addExtras(data)
	return base, out
}

func mergeOMOptions(params map[string]string, extras map[string]string) {
	for k, v := range extras {
		switch k {
		case "w":
			params["om_w"] = v
		case "h":
			params["om_h"] = v
		case "c":
			params["om_c"] = v
		case "m":
			params["om_m"] = v
		case "l":
			params["om_l"] = v
		default:
			params[k] = v
		}
	}
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
