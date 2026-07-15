package oms

import "testing"

func TestMediaRuleActiveColorSchemeDefaultsToLight(t *testing.T) {
	opts := &RenderOptions{ScreenW: 240, ScreenH: 320, NumColors: 65536}

	if mediaRuleActive("only screen and (prefers-color-scheme: dark)", opts) {
		t.Fatalf("dark color scheme media should not be active for OM render options")
	}
	if !mediaRuleActive("only screen and (prefers-color-scheme: light)", opts) {
		t.Fatalf("light color scheme media should be active")
	}
}

func TestMediaRuleActiveEvaluatesScreenFeatures(t *testing.T) {
	opts := &RenderOptions{ScreenW: 240, ScreenH: 320}

	if !mediaRuleActive("screen and (max-width: 640px)", opts) {
		t.Fatalf("expected max-width screen rule to match")
	}
	if mediaRuleActive("screen and (min-width: 640px)", opts) {
		t.Fatalf("expected min-width screen rule not to match")
	}
	if mediaRuleActive("print", opts) {
		t.Fatalf("print media should not match")
	}
}
