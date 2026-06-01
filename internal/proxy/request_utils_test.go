package proxy

import "testing"

func TestExtractOMFragment(t *testing.T) {
	base, extras := extractOMFragment("https://example.com/path#__om=page=2&pp=1600&img=2")
	if base != "https://example.com/path" {
		t.Fatalf("expected base URL without fragment, got %q", base)
	}
	if extras["page"] != "2" || extras["pp"] != "1600" || extras["img"] != "2" {
		t.Fatalf("unexpected extras: %#v", extras)
	}
}

func TestExtractOMFragmentUnknown(t *testing.T) {
	raw := "https://example.com/path#section"
	base, extras := extractOMFragment(raw)
	if base != "https://example.com/path" {
		t.Fatalf("expected base %q, got %q", "https://example.com/path", base)
	}
	if extras != nil {
		t.Fatalf("expected nil extras for unrelated fragment, got %#v", extras)
	}
}
