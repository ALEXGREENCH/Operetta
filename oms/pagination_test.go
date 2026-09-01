package oms

import (
	"bytes"
	"strings"
	"testing"

	"operetta/presentation"
)

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

func TestEffectivePaginationLimitsUseReportedHeap(t *testing.T) {
	large := &RenderOptions{ClientVersion: ClientVersion2, HeapBytes: 16 << 20}
	tags, wire, heap := EffectivePaginationLimits(large)
	if tags != maxHeapAwareTagsPerPage || wire != 0 || heap != 8<<20 {
		t.Fatalf("large heap limits=(%d,%d,%d)", tags, wire, heap)
	}

	unknown := &RenderOptions{ClientVersion: ClientVersion2}
	tags, wire, heap = EffectivePaginationLimits(unknown)
	if tags != 1600 || wire != defaultPaginationBytes || heap != 0 {
		t.Fatalf("unknown heap limits=(%d,%d,%d)", tags, wire, heap)
	}
}

func TestLargeReportedHeapAvoidsFixedWirePagination(t *testing.T) {
	builder := presentation.NewBuilder("https://example.test/long")
	for index := 0; index < 50; index++ {
		builder.AddText(strings.Repeat("x", 1000))
		builder.AddBreak()
	}

	page, err := EncodeDocument(builder.Document(), &RenderOptions{
		ClientVersion: ClientVersion2,
		Compression:   CompressionNone,
		HeapBytes:     16 << 20,
	})
	if err != nil {
		t.Fatalf("EncodeDocument: %v", err)
	}
	_, total := page.PartInfo()
	if total != 1 {
		t.Fatalf("large virtual heap unexpectedly produced %d parts", total)
	}
}

func TestDecodedInlineImagesCountAgainstHeapPagination(t *testing.T) {
	builder := presentation.NewBuilder("https://example.test/images")
	for index := 0; index < 3; index++ {
		// The encoded payload is tiny, but each decoded 100x100 image may retain
		// roughly 40KB as an ARGB Java ME surface.
		builder.AddImageInline(100, 100, bytes.Repeat([]byte{byte(index)}, 128))
		builder.AddBreak()
	}

	page, err := EncodeDocument(builder.Document(), &RenderOptions{
		ClientVersion: ClientVersion2,
		Compression:   CompressionNone,
		HeapBytes:     100 << 10, // 50KB page budget after runtime reserve
	})
	if err != nil {
		t.Fatalf("EncodeDocument: %v", err)
	}
	_, total := page.PartInfo()
	if total != 3 {
		t.Fatalf("decoded image heap produced %d parts, want 3", total)
	}
}
