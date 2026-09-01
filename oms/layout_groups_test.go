package oms

import (
	"encoding/binary"
	"strings"
	"testing"
)

func TestIconNavigationTableRendersAsOneToolbarRow(t *testing.T) {
	opts := defaultRenderPrefs()
	opts.ScreenW = 240
	opts.ScreenH = 320
	opts.ImageMIME = "image/jpeg"
	opts.MaxInlineKB = 8

	var cells strings.Builder
	for index := 0; index < 5; index++ {
		cells.WriteString(`<td width="20%"><a href="/nav/`)
		cells.WriteString(string(rune('a' + index)))
		cells.WriteString(`"><img class="icon" src="` + tinyPNGDataURI + `" alt="Nav"></a></td>`)
	}
	res := renderFixture(t, obmlFixture{
		name: "icon_toolbar",
		html: `<style>.bar{background:#4c4c4c}.icon{width:16px;height:16px}</style>` +
			`<div class="bar"><table><tbody><tr>` + cells.String() + `</tr></tbody></table></div>`,
		opts: &opts,
	})

	images := res.tokensByTag('I')
	if len(images) != 5 {
		t.Fatalf("toolbar images=%d, want 5; tokens=%v", len(images), res.tokens)
	}
	for index, image := range images {
		if got := int(binary.BigEndian.Uint16(image.data[:2])); got != 48 {
			t.Fatalf("toolbar tile %d width=%d, want 48", index, got)
		}
		if got := int(binary.BigEndian.Uint16(image.data[2:4])); got != 28 {
			t.Fatalf("toolbar tile %d height=%d, want 28", index, got)
		}
	}
	imagesSeen := 0
	for _, token := range res.tokens {
		if token.tag == 'I' {
			imagesSeen++
		}
		if token.tag == 'B' && imagesSeen > 0 && imagesSeen < 5 {
			t.Fatalf("toolbar broke after %d icons; tokens=%v", imagesSeen, res.tokens)
		}
	}
}

func TestLegacyBasicIconToolbarStaysBelowNativeInlineLimit(t *testing.T) {
	opts := defaultRenderPrefs()
	opts.ScreenW = 240
	opts.ScreenH = 320
	opts.ImageMIME = "image/jpeg"
	opts.MaxInlineKB = 8
	opts.LegacyBasicOM2 = true

	var cells strings.Builder
	for index := 0; index < 5; index++ {
		cells.WriteString(`<td><a href="/nav/`)
		cells.WriteString(string(rune('a' + index)))
		cells.WriteString(`"><img class="icon" src="` + tinyPNGDataURI + `" alt="Nav"></a></td>`)
	}
	res := renderFixture(t, obmlFixture{
		name: "legacy_basic_icon_toolbar",
		html: `<style>.bar{background:#4c4c4c}.icon{width:16px;height:16px}</style>` +
			`<div class="bar"><table><tbody><tr>` + cells.String() + `</tr></tbody></table></div>`,
		opts: &opts,
	})

	images := res.tokensByTag('I')
	if len(images) != 5 {
		t.Fatalf("toolbar images=%d, want 5; tokens=%v", len(images), res.tokens)
	}
	seen := 0
	for _, token := range res.tokens {
		if token.tag == 'I' {
			seen++
			width := int(binary.BigEndian.Uint16(token.data[:2]))
			height := int(binary.BigEndian.Uint16(token.data[2:4]))
			if width > legacyBasicInlineImageLimit || height > legacyBasicInlineImageLimit {
				t.Fatalf("native OM2 tile=%dx%d exceeds %dpx inline limit", width, height, legacyBasicInlineImageLimit)
			}
		}
		if token.tag == 'B' && seen > 0 && seen < len(images) {
			t.Fatalf("native OM2 toolbar broke after %d icons; tokens=%v", seen, res.tokens)
		}
	}
}

func TestInlineBlockImageStripStaysHorizontal(t *testing.T) {
	res := renderFixture(t, obmlFixture{
		name: "inline_image_strip",
		html: `<style>.tile{display:inline-block;width:40%}</style><div class="strip">` +
			`<div class="tile"><a href="/one"><img src="` + tinyPNGDataURI + `" alt="One"></a></div>` +
			`<div class="tile"><a href="/two"><img src="` + tinyPNGDataURI + `" alt="Two"></a></div></div>`,
	})
	if res.countTag('I') != 2 {
		t.Fatalf("strip images=%d, want 2", res.countTag('I'))
	}
	seen := 0
	for _, token := range res.tokens {
		if token.tag == 'I' {
			seen++
		}
		if token.tag == 'B' && seen == 1 {
			t.Fatalf("image strip became vertical; tokens=%v", res.tokens)
		}
	}
}

func TestLegacyBasicImageStripScalesEachImageBelowInlineLimit(t *testing.T) {
	opts := defaultRenderPrefs()
	opts.LegacyBasicOM2 = true
	res := renderFixture(t, obmlFixture{
		name: "legacy_basic_inline_image_strip",
		html: `<style>.tile{display:inline-block;width:40%}.tile img{width:76px;height:53px}</style><div class="strip">` +
			`<div class="tile"><a href="/one"><img src="` + tinyPNGDataURI + `" alt="One"></a></div>` +
			`<div class="tile"><a href="/two"><img src="` + tinyPNGDataURI + `" alt="Two"></a></div></div>`,
		opts: &opts,
	})
	images := res.tokensByTag('I')
	if len(images) != 2 {
		t.Fatalf("strip images=%d, want 2", len(images))
	}
	for _, token := range images {
		width := int(binary.BigEndian.Uint16(token.data[:2]))
		height := int(binary.BigEndian.Uint16(token.data[2:4]))
		if width > legacyBasicInlineImageLimit || height > legacyBasicInlineImageLimit {
			t.Fatalf("native OM2 strip image=%dx%d exceeds inline limit", width, height)
		}
	}
}

func TestSectionTitleKeepsHeadingAndActionOnOneLine(t *testing.T) {
	res := renderFixture(t, obmlFixture{
		name: "section_title_action",
		html: `<style>.right{float:right}.title{font-weight:bold}</style>` +
			`<div class="title"><a class="right" href="/all">Все</a><h6>Популярные фото</h6></div>`,
	})
	if got := res.visibleText(); !strings.Contains(got, "Популярные фото · Все\n") {
		t.Fatalf("unexpected section title flow %q; tokens=%v", got, res.tokens)
	}
	res.mustHaveLink(t, "0/http://fixture.test/all")
}

func TestFloatedThumbnailMediaObjectStartsInline(t *testing.T) {
	res := renderFixture(t, obmlFixture{
		name: "media_object",
		html: `<style>.thumb{float:left;width:50px}.content{overflow:hidden}</style>` +
			`<div class="card"><div class="thumb"><img src="` + tinyPNGDataURI + `" alt=""></div>` +
			`<div class="content"><a href="/game"><b>Битва танков</b></a><div>Описание игры</div></div></div>`,
	})
	imageIndex, textIndex, breakIndex := -1, -1, -1
	for index, token := range res.tokens {
		if imageIndex < 0 && token.tag == 'I' {
			imageIndex = index
		}
		if token.tag == 'T' {
			for _, text := range token.strings {
				if strings.Contains(text, "Битва танков") {
					textIndex = index
				}
			}
		}
		if imageIndex >= 0 && breakIndex < 0 && token.tag == 'B' {
			breakIndex = index
		}
	}
	if imageIndex < 0 || textIndex < 0 || textIndex < imageIndex || (breakIndex >= 0 && breakIndex < textIndex) {
		t.Fatalf("thumbnail/title are not on the same first line; tokens=%v", res.tokens)
	}
}

func TestNestedGenericBlocksDoNotAccumulateParagraphSpacing(t *testing.T) {
	res := renderFixture(t, obmlFixture{
		name: "nested_blocks",
		html: `<div class="wrapper"><div><div><div>Compact content</div></div></div></div>`,
	})
	res.mustContainText(t, "Compact content")
	if got := res.countTag('V'); got != 0 {
		t.Fatalf("nested generic blocks emitted %d paragraph spacers; tokens=%v", got, res.tokens)
	}
}

func TestLeadingTimestampFollowsCardIdentity(t *testing.T) {
	res := renderFixture(t, obmlFixture{
		name: "leading_right_metadata",
		html: `<style>.right{float:right}</style><div class="card"><div class="row">` +
			`<span class="right">вчера в 17:37</span><div>Lesnik41</div>` +
			`<a href="/post">Заголовок</a><div>Описание</div></div></div>`,
	})
	visible := res.visibleText()
	if !strings.Contains(visible, "Lesnik41 · вчера в 17:37\n") {
		t.Fatalf("timestamp did not follow identity: %q tokens=%v", visible, res.tokens)
	}
	if strings.Index(visible, "Заголовок") < strings.Index(visible, "вчера в 17:37") {
		t.Fatalf("card body moved before metadata: %q", visible)
	}
}

func TestLeftRightMetadataPairBecomesCompactSuffix(t *testing.T) {
	res := renderFixture(t, obmlFixture{
		name: "float_metadata_pair",
		html: `<style>.left{float:left}.right{float:right}</style>` +
			`<div><span class="left">10 комментариев</span><span class="right">Канал: История</span></div>`,
	})
	if !strings.Contains(res.visibleText(), "10 комментариев · Канал: История\n") {
		t.Fatalf("unexpected metadata row %q; tokens=%v", res.visibleText(), res.tokens)
	}
}
