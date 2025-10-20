package oms

import (
	"strings"
	"testing"
)

func TestCssToHex(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"hex_passthrough", "#1a2b3c", "#1a2b3c"},
		{"hex_shorthand", "#abc", "#aabbcc"},
		{"hex_longer", "#abcdef7", "#abcdef"},
		{"named_white", "white", "#ffffff"},
		{"named_black", "black", "#000000"},
		{"transparent_ignored", "transparent", ""},
		{"rgb_function", "rgb(255, 64, 0)", "#ff4000"},
		{"rgba_function", "RGBA(10%,20%,30%,0.5)", "#19334c"},
		{"invalid", "nope", ""},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := cssToHex(tc.input); got != tc.expected {
				t.Fatalf("cssToHex(%q) = %q, expected %q", tc.input, got, tc.expected)
			}
		})
	}
}

func TestLightenHex(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		input    string
		percent  int
		expected string
	}{
		{"zero_percent", "#000000", 0, "#000000"},
		{"fifty_percent", "#000000", 50, "#7f7f7f"},
		{"full_percent_caps", "#123456", 100, "#ffffff"},
		{"invalid_passthrough", "invalid", 25, "#invalid"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := lightenHex(tc.input, tc.percent); got != tc.expected {
				t.Fatalf("lightenHex(%q,%d) = %q, expected %q", tc.input, tc.percent, got, tc.expected)
			}
		})
	}
}

func TestEnsureMinForRGB565(t *testing.T) {
	t.Parallel()
	if got := ensureMinForRGB565("#010203"); got != "#101010" {
		t.Fatalf("ensureMinForRGB565 returned %q, want %q", got, "#101010")
	}
	if got := ensureMinForRGB565("bad"); got != "#bad" {
		t.Fatalf("ensureMinForRGB565 passthrough mismatch: got %q", got)
	}
}

func TestNormalizeBgForBlackText(t *testing.T) {
	t.Parallel()
	t.Run("preserves_high_contrast", func(t *testing.T) {
		t.Parallel()
		const input = "#f0f0f0"
		if got := normalizeBgForBlackText(input); got != input {
			t.Fatalf("normalizeBgForBlackText(%q) changed output to %q", input, got)
		}
	})
	t.Run("enforces_minimum_contrast", func(t *testing.T) {
		t.Parallel()
		const input = "#000000"
		out := normalizeBgForBlackText(input)
		if out == "" {
			t.Fatalf("normalizeBgForBlackText(%q) returned empty string", input)
		}
		col, ok := parseHexColor(out)
		if !ok {
			t.Fatalf("normalizeBgForBlackText(%q) -> %q not parseable", input, out)
		}
		const minContrast = 4.5
		if contrast := contrastRatio(out, "#000000"); contrast < minContrast {
			t.Fatalf("contrast ratio %f below minimum %f for %q", contrast, minContrast, out)
		}
		for channelName, v := range map[string]uint8{"r": col.R, "g": col.G, "b": col.B} {
			if v < 16 {
				t.Fatalf("channel %s below 16 after normalization: %d", channelName, v)
			}
		}
	})
}

func TestHexBrightnessAndDarkness(t *testing.T) {
	t.Parallel()
	if val := hexBrightness("#000000"); val != 0 {
		t.Fatalf("hexBrightness(#000000) = %d, expected 0", val)
	}
	if !isDarkHex("#000000") {
		t.Fatalf("isDarkHex(#000000) = false, expected true")
	}
	if val := hexBrightness("oops"); val != 255 {
		t.Fatalf("hexBrightness fallback = %d, expected 255", val)
	}
}

func TestIrregularRgbParsing(t *testing.T) {
	t.Parallel()
	const input = "rgb( 10% , 120 , -5 )"
	got := cssToHex(input)
	if got != "#197800" {
		t.Fatalf("cssToHex(%q) = %q, expected #197800", input, got)
	}
}

func TestCssToHexIsCaseInsensitive(t *testing.T) {
	t.Parallel()
	input := strings.ToUpper("rgba(1,2,3,0.5)")
	if got := cssToHex(input); got != "#010203" {
		t.Fatalf("cssToHex upper case rgba mismatch: got %q", got)
	}
}
