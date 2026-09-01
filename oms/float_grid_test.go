package oms

import (
	"bytes"
	"encoding/binary"
	"image"
	"testing"
)

func TestCSSFloatIconDrawerRendersThreeColumnOMSTiles(t *testing.T) {
	opts := defaultRenderPrefs()
	opts.ScreenW = 240
	opts.ScreenH = 320
	opts.ImageMIME = "image/jpeg"
	opts.MaxInlineKB = 8
	opts.DialectID = "om2-basic"

	cell := func(path, label string) string {
		return `<div class="appcell"><a href="` + path + `"><img src="` + tinyPNGDataURI + `" alt=""><span class="label">` + label + `</span></a></div>`
	}
	html := `<html><head><style>
		body{background:#0d1117;color:#fff}.drawer{width:100%;overflow:hidden}
		.appcell{float:left;width:33%;height:82px;text-align:center;color:#fff}
		.drawer img{width:44px;height:44px;display:block}.label{display:block}
		.cell-empty{float:left;width:33%;height:82px}.clear{clear:both}
		</style></head><body><h3>Apps</h3><div class="drawer">` +
		cell("/one", "One") + cell("/two", "Two") + cell("/three", "Three") +
		cell("/four", "Four") + cell("/five", "Five") + cell("/six", "Six") +
		`<div class="cell-empty">&nbsp;</div><div class="clear"></div></div></body></html>`

	res := renderFixture(t, obmlFixture{name: "float_icon_drawer", html: html, opts: &opts})
	images := res.tokensByTag('I')
	if len(images) != 6 {
		t.Fatalf("expected six rasterized app tiles, got %d; tokens=%v", len(images), res.tokens)
	}
	for index, token := range images {
		if len(token.data) < 8 {
			t.Fatalf("tile %d has short image header: %x", index, token.data)
		}
		if got := int(binary.BigEndian.Uint16(token.data[0:2])); got != 80 {
			t.Fatalf("tile %d width=%d, want 80", index, got)
		}
		if got := int(binary.BigEndian.Uint16(token.data[2:4])); got != 82 {
			t.Fatalf("tile %d height=%d, want 82", index, got)
		}
		decoded, _, err := image.Decode(bytes.NewReader(token.extra))
		if err != nil {
			t.Fatalf("tile %d is not decodable: %v", index, err)
		}
		if decoded.Bounds().Dx() != 80 || decoded.Bounds().Dy() != 82 {
			t.Fatalf("tile %d decoded bounds=%v, want 80x82", index, decoded.Bounds())
		}
		if len(token.extra) > opts.MaxInlineKB*1024 {
			t.Fatalf("tile %d payload=%d exceeds OM2 Basic budget", index, len(token.extra))
		}
	}
	for _, path := range []string{"/one", "/two", "/three", "/four", "/five", "/six"} {
		res.mustHaveLink(t, "0/http://fixture.test"+path)
	}

	rowSizes := []int{}
	imagesInRow := 0
	for _, token := range res.tokens {
		if token.tag == 'I' {
			imagesInRow++
		}
		if token.tag == 'B' && imagesInRow > 0 {
			rowSizes = append(rowSizes, imagesInRow)
			imagesInRow = 0
		}
	}
	if len(rowSizes) != 2 || rowSizes[0] != 3 || rowSizes[1] != 3 {
		t.Fatalf("expected two OMS rows of three tiles, got %v", rowSizes)
	}
}

func TestLegacyBasicCSSFloatIconDrawerUsesNativeInlineTiles(t *testing.T) {
	opts := defaultRenderPrefs()
	opts.ScreenW = 240
	opts.ScreenH = 320
	opts.ImageMIME = "image/jpeg"
	opts.MaxInlineKB = 8
	opts.DialectID = "om2-basic"
	opts.LegacyBasicOM2 = true

	cell := func(path, label string) string {
		return `<div class="appcell"><a href="` + path + `"><img src="` + tinyPNGDataURI + `" alt=""><span class="label">` + label + `</span></a></div>`
	}
	html := `<style>.appcell{float:left;width:33%;height:82px;text-align:center}.appcell img{width:44px;height:44px;display:block}</style>` +
		`<div class="drawer">` + cell("/one", "One") + cell("/two", "Two") + cell("/three", "Three") + `</div>`
	res := renderFixture(t, obmlFixture{name: "legacy_basic_float_icon_drawer", html: html, opts: &opts})
	images := res.tokensByTag('I')
	if len(images) != 3 {
		t.Fatalf("expected three native tiles, got %d; tokens=%v", len(images), res.tokens)
	}
	for index, token := range images {
		width := int(binary.BigEndian.Uint16(token.data[0:2]))
		height := int(binary.BigEndian.Uint16(token.data[2:4]))
		if width > legacyBasicInlineImageLimit || height > legacyBasicInlineImageLimit {
			t.Fatalf("tile %d=%dx%d exceeds native inline limit", index, width, height)
		}
	}
}

func TestTextOnlyCSSFloatsKeepLinearFallback(t *testing.T) {
	res := renderFixture(t, obmlFixture{
		name: "text_float_fallback",
		html: `<style>.column{float:left;width:50%}</style><div><div class="column">Alpha</div><div class="column">Beta</div></div>`,
	})
	res.mustContainText(t, "Alpha")
	res.mustContainText(t, "Beta")
	if got := res.countTag('I'); got != 0 {
		t.Fatalf("text-only floats must not be rasterized, got %d images", got)
	}
}
