package oms

import "testing"

func TestEnsureHiddenFieldOverrides_GoogleSearchAddsUdm(t *testing.T) {
	fields := map[string]string{
		"hl": "en",
	}
	ensureHiddenFieldOverrides("https://www.google.com/search", fields)
	if got := fields["udm"]; got != "2" {
		t.Fatalf("expected udm=2, got %q", got)
	}
}

func TestEnsureHiddenFieldOverrides_KeepsExistingUdm(t *testing.T) {
	fields := map[string]string{
		"udm": "14",
	}
	ensureHiddenFieldOverrides("https://www.google.com/search", fields)
	if got := fields["udm"]; got != "14" {
		t.Fatalf("expected udm to remain 14, got %q", got)
	}
}

func TestEnsureHiddenFieldOverrides_NonGoogleAction(t *testing.T) {
	fields := map[string]string{
		"hl": "en",
	}
	ensureHiddenFieldOverrides("https://example.com/search", fields)
	if _, ok := fields["udm"]; ok {
		t.Fatalf("did not expect udm to be added for non-Google action")
	}
}
