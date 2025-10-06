package proxy

import (
	neturl "net/url"
	"strings"
)

// urlDecode converts percent-encoded sequences like %2f into their byte values.
func urlDecode(url string) string {
	b := make([]byte, 0, len(url))
	for i := 0; i < len(url); i++ {
		c := url[i]
		if c == '%' && i+2 < len(url) {
			hi := fromHex(url[i+1])
			lo := fromHex(url[i+2])
			b = append(b, hi<<4|lo)
			i += 2
		} else {
			b = append(b, c)
		}
	}
	return string(b)
}

func fromHex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	}
	return 0
}

// buildURL builds a new URL based on the current url, action, and get params.
// It mirrors the logic from the legacy C implementation.
func buildURL(base, action, get string) string {
	decodedBase := urlDecode(urlDecode(base))
	decodedAction := ""
	if action != "" {
		decodedAction = urlDecode(action)
	}

	if parsedBase, err := neturl.Parse(decodedBase); err == nil && parsedBase.Scheme != "" {
		finalURL := parsedBase
		if decodedAction != "" {
			if ref, err := neturl.Parse(decodedAction); err == nil {
				if ref.IsAbs() {
					finalURL = ref
				} else {
					finalURL = parsedBase.ResolveReference(ref)
				}
			}
		}
		if get != "" {
			if finalURL.RawQuery != "" {
				finalURL.RawQuery += "&" + get
			} else {
				finalURL.RawQuery = get
			}
		}
		return finalURL.String()
	}

	newURL := decodedBase
	if decodedAction != "" {
		switch {
		case strings.Contains(decodedAction, "://"):
			newURL = decodedAction
		case strings.HasPrefix(decodedAction, "/"):
			hostPrefix := decodedBase
			if idx := strings.Index(decodedBase, "//"); idx != -1 {
				hostStart := idx + 2
				if slash := strings.Index(decodedBase[hostStart:], "/"); slash != -1 {
					hostPrefix = decodedBase[:hostStart+slash]
				}
			}
			newURL = strings.TrimRight(hostPrefix, "/") + decodedAction
		default:
			basePrefix := decodedBase
			if strings.HasSuffix(basePrefix, "/") {
				basePrefix = strings.TrimRight(basePrefix, "/")
			}
			if last := strings.LastIndex(basePrefix, "/"); last != -1 {
				basePrefix = basePrefix[:last]
			}
			newURL = strings.TrimRight(basePrefix, "/") + "/" + decodedAction
		}
	}

	if get != "" {
		if strings.Contains(newURL, "?") {
			newURL += "&" + get
		} else {
			newURL += "?" + get
		}
	}

	return newURL
}
