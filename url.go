package main

import "strings"

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
    // Ensure base is fully decoded because callers may double-encode it in links
    base = urlDecode(urlDecode(base))
    if action != "" {
        action = urlDecode(action)
    }
    newURL := ""
    if action != "" {
        if strings.Contains(action, "://") {
            newURL = action
        } else {
            if strings.HasPrefix(action, "/") {
                slash := strings.Index(base[7:], "/")
                if slash != -1 {
                    base = base[:7+slash]
                }
                action = action[1:]
            } else {
                slash := strings.LastIndex(base, "/")
                if slash > 6 { // position after http://
                    base = base[:slash]
                }
            }
            newURL = base + "/" + action
        }
    } else {
        newURL = base
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
