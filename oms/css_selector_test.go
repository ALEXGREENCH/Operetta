package oms

import "testing"

func TestSanitizeSelectorKeepsLinkAndDropsDynamicStates(t *testing.T) {
	if got := sanitizeSelectorForCascadia("A:link"); got != "A" {
		t.Fatalf("A:link sanitized to %q", got)
	}
	for _, selector := range []string{"A:hover", "a:visited", "button:focus", "a:focus-visible"} {
		if got := sanitizeSelectorForCascadia(selector); got != "" {
			t.Fatalf("dynamic selector %q sanitized to %q", selector, got)
		}
	}
}
