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

func TestExtractOMQuery(t *testing.T) {
	base, extras := extractOMFragment("https://example.com/path?skip=5&__om=c%3D65536%26h%3D320%26img%3D1%26l%3D256%26m%3D16777216%26page%3D3%26pp%3D1600%26w%3D240")
	if base != "https://example.com/path?skip=5" {
		t.Fatalf("expected base URL without __om query, got %q", base)
	}
	if extras["page"] != "3" || extras["w"] != "240" || extras["h"] != "320" || extras["c"] != "65536" || extras["m"] != "16777216" || extras["l"] != "256" {
		t.Fatalf("unexpected extras: %#v", extras)
	}
}

func TestExtractLegacyPartQuery(t *testing.T) {
	base, extras := extractOMFragment("https://example.com/path?skip=5&__p=2")
	if base != "https://example.com/path?skip=5" {
		t.Fatalf("expected base URL without __p, got %q", base)
	}
	if extras["page"] != "2" {
		t.Fatalf("expected legacy page=2, got %#v", extras)
	}
}

func TestMergeOMOptionsKeepsAuthFieldsSeparate(t *testing.T) {
	params := map[string]string{"h": "auth-prefix", "c": "auth-code"}
	mergeOMOptions(params, map[string]string{"w": "240", "h": "320", "c": "65536", "m": "16777216", "l": "256", "page": "3"})
	if params["h"] != "auth-prefix" || params["c"] != "auth-code" {
		t.Fatalf("auth fields were overwritten: %#v", params)
	}
	if params["om_w"] != "240" || params["om_h"] != "320" || params["om_c"] != "65536" || params["om_m"] != "16777216" || params["om_l"] != "256" {
		t.Fatalf("OM options were not mapped safely: %#v", params)
	}
	if params["page"] != "3" {
		t.Fatalf("expected page passthrough, got %#v", params)
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
