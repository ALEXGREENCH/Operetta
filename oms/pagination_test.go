package oms

import "testing"

func TestBuildPaginationLink(t *testing.T) {
	opts := defaultRenderPrefs()
	opts.ImagesOn = false
	opts.ImageMIME = ""
	opts.MaxInlineKB = 96
	link := BuildPaginationLink("https://example.com/page", &opts, 2, 1600)
	want := "https://example.com/page?__om=img%3D2%26maxkb%3D96%26page%3D2%26pp%3D1600"
	if link != want {
		t.Fatalf("unexpected link: %s", link)
	}

	withQuery := BuildPaginationLink("https://example.com/page?skip=5", &opts, 3, 1600)
	wantWithQuery := "https://example.com/page?__om=img%3D2%26maxkb%3D96%26page%3D3%26pp%3D1600&skip=5"
	if withQuery != wantWithQuery {
		t.Fatalf("unexpected link with query: %s", withQuery)
	}

	first := BuildPaginationLink("https://example.com/page?__om=page%3D2%26pp%3D1600&__p=2", &opts, 1, 1600)
	if first != "https://example.com/page" {
		t.Fatalf("expected first-page link without markers, got %s", first)
	}
}
