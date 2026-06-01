package oms

import (
	"bytes"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func TestTableWithAnchorsKeepsLinks(t *testing.T) {
	const src = `<!DOCTYPE html><html><body><table><tr><td class="tdate">06.10</td><td class="tnews"><a href="/opennews/art.shtml?num=64005">TinyUSB 0.19 release</a> <span class="cnt">(18 +12)</span></td></tr></table></body></html>`
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	page := NewPage()
	visited := map[*html.Node]bool{}
	page.AddStyle(styleDefault)
	st := walkState{curStyle: styleDefault}
	prefs := defaultRenderPrefs()
	walkRich(doc, "https://www.opennet.ru/", page, visited, &st, prefs)
	if !bytes.Contains(page.Data, []byte("0/https://www.opennet.ru/opennews/art.shtml?num=64005")) {
		t.Fatalf("table anchors should stay clickable, data=%q", page.Data)
	}
}
