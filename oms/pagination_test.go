package oms

import "testing"

func TestBuildPaginationLink(t *testing.T) {
	opts := defaultRenderPrefs()
	opts.ImagesOn = false
	opts.ImageMIME = ""
	opts.MaxInlineKB = 96
	link := BuildPaginationLink("https://example.com/page", &opts, 2, 1600)
	want := "https://example.com/page#__om=img=2&maxkb=96&page=2&pp=1600"
	if link != want {
		t.Fatalf("unexpected link: %s", link)
	}

	first := BuildPaginationLink("https://example.com/page?__p=2", &opts, 1, 1600)
	if first != "https://example.com/page" {
		t.Fatalf("expected first-page link without markers, got %s", first)
	}
}
