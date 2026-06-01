package oms

import (
	"bytes"
	"testing"

	"golang.org/x/net/html"
)

func TestAddTextWithColorAppliesTemporaryStyle(t *testing.T) {
	t.Parallel()
	page := NewPage()
	state := &walkState{
		curStyle: styleDefault,
		curColor: defaultTextColorHex,
	}
	node := &html.Node{
		Type: html.ElementNode,
		Attr: []html.Attribute{
			{Key: "style", Val: "color: #ff0000;"},
		},
	}

	addTextWithColor(page, state, node, "hello")

	if count := bytes.Count(page.Data, []byte{'S'}); count != 2 {
		t.Fatalf("expected two temporary style tags, got %d", count)
	}
	if !bytes.Contains(page.Data, []byte{'T'}) {
		t.Fatalf("expected text tag in output payload")
	}
	if state.curColor != defaultTextColorHex {
		t.Fatalf("walk state color mutated, got %q", state.curColor)
	}
}

func TestAddTextWithColorSkipsWhenColorMatches(t *testing.T) {
	t.Parallel()
	page := NewPage()
	state := &walkState{
		curStyle: styleDefault,
		curColor: "#ff0000",
	}
	node := &html.Node{
		Type: html.ElementNode,
		Attr: []html.Attribute{
			{Key: "style", Val: "color: #ff0000;"},
		},
	}

	addTextWithColor(page, state, node, "sample")

	if count := bytes.Count(page.Data, []byte{'S'}); count != 0 {
		t.Fatalf("expected no temporary style tags, got %d", count)
	}
	if state.curColor != "#ff0000" {
		t.Fatalf("walk state color should remain unchanged, got %q", state.curColor)
	}
}
