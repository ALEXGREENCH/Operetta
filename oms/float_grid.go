package oms

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	stddraw "image/draw"
	"math"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
	"golang.org/x/net/html"
)

// floatIconCell is the deliberately narrow subset of CSS floats that can be
// represented faithfully by the old OMS display list. Text-only floats keep
// using the normal linear fallback; icon drawers are rasterized per cell so a
// complete tile remains both visually rectangular and independently clickable.
type floatIconCell struct {
	node  *html.Node
	image *html.Node
	label string
	href  string
}

func cssPercent(value string) (float64, bool) {
	value = strings.TrimSpace(strings.ToLower(value))
	if !strings.HasSuffix(value, "%") {
		return 0, false
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(value, "%")), 64)
	if err != nil || n <= 0 || n > 100 {
		return 0, false
	}
	return n, true
}

func floatIconGridCells(container *html.Node, ss *Stylesheet) ([]floatIconCell, int, bool) {
	if container == nil || ss == nil {
		return nil, 0, false
	}
	var cells []floatIconCell
	firstWidth := 0.0
	for child := container.FirstChild; child != nil; child = child.NextSibling {
		if child.Type != html.ElementNode {
			continue
		}
		props := computeStyleFor(child, ss)
		floatValue := strings.ToLower(strings.TrimSpace(cssPropValue(props, getAttr(child, "style"), "float")))
		if floatValue != "left" {
			continue
		}
		width, ok := cssPercent(cssPropValue(props, getAttr(child, "style"), "width"))
		if !ok {
			continue
		}
		if firstWidth == 0 {
			firstWidth = width
		}
		// Mixed float widths are real layout, not an icon drawer. Leave them to
		// the conservative linear renderer instead of guessing a column model.
		if firstWidth > 0 && (width < firstWidth-1.0 || width > firstWidth+1.0) {
			return nil, 0, false
		}

		img := findFirstByTag(child, "img")
		link := findFirstByTag(child, "a")
		label := strings.TrimSpace(condenseSpaces(collectText(child)))
		if img == nil || label == "" {
			// Empty slot elements are common at the end of app drawers. They do
			// not need a bitmap or a dead focus target in the compact OMS view.
			continue
		}
		href := ""
		if link != nil {
			href = strings.TrimSpace(getAttr(link, "href"))
		}
		cells = append(cells, floatIconCell{node: child, image: img, label: label, href: href})
	}
	if len(cells) < 2 || firstWidth <= 0 {
		return nil, 0, false
	}
	columns := int(math.Round(100.0 / firstWidth))
	if columns < 2 || columns > 6 {
		return nil, 0, false
	}
	return cells, columns, true
}

func rawDataURI(uri string) ([]byte, bool) {
	comma := strings.IndexByte(uri, ',')
	if !strings.HasPrefix(uri, "data:") || comma < 0 {
		return nil, false
	}
	meta := strings.ToLower(uri[len("data:"):comma])
	payload := uri[comma+1:]
	if strings.Contains(meta, ";base64") {
		decoded, err := base64.StdEncoding.DecodeString(payload)
		return decoded, err == nil && len(decoded) > 0
	}
	decoded, err := url.PathUnescape(payload)
	if err != nil {
		return nil, false
	}
	return []byte(decoded), decoded != ""
}

func loadFloatGridImage(absURL string, prefs RenderOptions) (image.Image, bool) {
	var raw []byte
	var ok bool
	if strings.HasPrefix(strings.ToLower(absURL), "data:") {
		raw, ok = rawDataURI(absURL)
	} else {
		raw, ok = fetchText(absURL, prefs.ReqHeaders, prefs.Jar, "image/*")
	}
	if !ok || len(raw) == 0 {
		return nil, false
	}
	decoded, _, err := image.Decode(bytes.NewReader(raw))
	return decoded, err == nil
}

func gridColor(value string, fallback color.RGBA) color.RGBA {
	if parsed, ok := parseCSSColor(value); ok {
		return color.RGBA{R: parsed.R, G: parsed.G, B: parsed.B, A: 0xff}
	}
	return fallback
}

func omRasterBackground(value string, fallback color.RGBA) color.RGBA {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "transparent") {
		return fallback
	}
	safe := normalizeBgForBlackText(value)
	if safe == "" {
		safe = value
	}
	safe = ensureMinForRGB565(safe)
	return gridColor(safe, fallback)
}

func fitGridLabel(label string, face font.Face, maxWidth int) string {
	label = strings.TrimSpace(condenseSpaces(label))
	if label == "" || maxWidth <= 0 {
		return ""
	}
	if font.MeasureString(face, label).Ceil() <= maxWidth {
		return label
	}
	suffix := "..."
	for len(label) > 0 {
		_, size := utf8.DecodeLastRuneInString(label)
		if size <= 0 {
			break
		}
		label = label[:len(label)-size]
		candidate := strings.TrimSpace(label) + suffix
		if font.MeasureString(face, candidate).Ceil() <= maxWidth {
			return candidate
		}
	}
	return ""
}

func renderFloatIconTile(cell floatIconCell, base string, st *walkState, prefs RenderOptions, width int) ([]byte, int, int, bool) {
	cellProps := computeStyleFor(cell.node, st.css)
	height := cssValueToPx(cssPropValue(cellProps, getAttr(cell.node, "style"), "height"), prefs.ScreenH)
	if height <= 0 {
		height = 72
	}
	if height < 36 {
		height = 36
	}
	if height > 160 {
		height = 160
	}

	bg := omRasterBackground(st.curBg, color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff})
	if own := cssPropValue(cellProps, getAttr(cell.node, "style"), "background-color"); own != "" {
		bg = omRasterBackground(own, bg)
	}
	fg := gridColor(cssEffectiveProp(cell.node, st.css, cellProps, "color"), color.RGBA{A: 0xff})
	tile := image.NewRGBA(image.Rect(0, 0, width, height))
	stddraw.Draw(tile, tile.Bounds(), image.NewUniform(bg), image.Point{}, stddraw.Src)

	src := strings.TrimSpace(getAttr(cell.image, "src"))
	if src == "" {
		src = strings.TrimSpace(getAttr(cell.image, "data-src"))
	}
	abs := resolveAbsURL(base, src)
	icon, ok := loadFloatGridImage(abs, prefs)
	if !ok {
		return nil, 0, 0, false
	}
	imgProps := computeStyleFor(cell.image, st.css)
	iconW := cssValueToPx(cssPropValue(imgProps, getAttr(cell.image, "style"), "width"), width)
	iconH := cssValueToPx(cssPropValue(imgProps, getAttr(cell.image, "style"), "height"), height)
	if iconW <= 0 {
		iconW = icon.Bounds().Dx()
	}
	if iconH <= 0 {
		iconH = icon.Bounds().Dy()
	}
	maxIconW := width - 6
	maxIconH := height - 22
	if maxIconW < 1 || maxIconH < 1 {
		return nil, 0, 0, false
	}
	scale := math.Min(1.0, math.Min(float64(maxIconW)/float64(iconW), float64(maxIconH)/float64(iconH)))
	iconW = int(float64(iconW)*scale + 0.5)
	iconH = int(float64(iconH)*scale + 0.5)
	iconX := (width - iconW) / 2
	iconY := 4
	xdraw.CatmullRom.Scale(tile, image.Rect(iconX, iconY, iconX+iconW, iconY+iconH), icon, icon.Bounds(), stddraw.Over, nil)

	face := basicfont.Face7x13
	label := fitGridLabel(cell.label, face, width-4)
	if label != "" {
		labelWidth := font.MeasureString(face, label).Ceil()
		baseline := iconY + iconH + 4 + face.Metrics().Ascent.Ceil()
		maxBaseline := height - face.Metrics().Descent.Ceil() - 2
		if baseline > maxBaseline {
			baseline = maxBaseline
		}
		drawer := font.Drawer{
			Dst:  tile,
			Src:  image.NewUniform(fg),
			Face: face,
			Dot:  fixed.P((width-labelWidth)/2, baseline),
		}
		drawer.DrawString(label)
	}

	encoded, encodedW, encodedH, _, _, err := encodeImage(tile, prefs)
	if err != nil || len(encoded) == 0 {
		return nil, 0, 0, false
	}
	return encoded, encodedW, encodedH, true
}

func renderFloatIconGrid(container *html.Node, base string, p renderTarget, st *walkState, prefs RenderOptions) bool {
	if !prefs.ImagesOn || st == nil || st.css == nil {
		return false
	}
	cells, columns, ok := floatIconGridCells(container, st.css)
	if !ok {
		return false
	}
	screenW := prefs.ScreenW
	if screenW <= 0 {
		screenW = 240
	}
	cellW := screenW / columns
	if cellW < 24 {
		return false
	}

	type renderedCell struct {
		data          []byte
		width, height int
		link          string
	}
	rendered := make([]renderedCell, 0, len(cells))
	for _, cell := range cells {
		data, width, height, ok := renderFloatIconTile(cell, base, st, prefs, cellW)
		if !ok {
			return false
		}
		link := ""
		if cell.href != "" {
			if resolved, valid := resolveNavigableLink(base, cell.href); valid {
				link = resolved
			}
		}
		rendered = append(rendered, renderedCell{data: data, width: width, height: height, link: link})
	}
	if len(rendered) < 2 {
		return false
	}

	for index, cell := range rendered {
		if cell.link != "" {
			p.BeginLink(cell.link)
		}
		p.AddImageInline(cell.width, cell.height, cell.data)
		if cell.link != "" {
			p.EndLink()
		}
		if (index+1)%columns == 0 || index == len(rendered)-1 {
			p.AddBreak()
		}
	}
	return true
}
