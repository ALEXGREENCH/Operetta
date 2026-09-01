package oms

import (
	"image"
	"image/color"
	stddraw "image/draw"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/net/html"
)

type linkedImage struct {
	link  *html.Node
	image *html.Node
	maxW  int
}

const legacyBasicInlineImageLimit = 38

func constrainLegacyBasicInlineImage(prefs RenderOptions, width, height int) (int, int) {
	if !prefs.LegacyBasicOM2 || width <= 0 || height <= 0 ||
		(width <= legacyBasicInlineImageLimit && height <= legacyBasicInlineImageLimit) {
		return width, height
	}
	scale := math.Min(float64(legacyBasicInlineImageLimit)/float64(width),
		float64(legacyBasicInlineImageLimit)/float64(height))
	width = int(math.Floor(float64(width) * scale))
	height = int(math.Floor(float64(height) * scale))
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}
	return width, height
}

func elementChildren(n *html.Node) []*html.Node {
	var out []*html.Node
	if n == nil {
		return out
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.ElementNode {
			out = append(out, child)
		}
	}
	return out
}

func imageNodeSource(n *html.Node) string {
	if n == nil {
		return ""
	}
	src := strings.TrimSpace(getAttr(n, "src"))
	if src == "" {
		src = strings.TrimSpace(getAttr(n, "data-src"))
	}
	if src == "" {
		src = strings.TrimSpace(getAttr(n, "data-original"))
	}
	if src == "" {
		src = strings.TrimSpace(getAttr(n, "data-lazy-src"))
	}
	if src == "" {
		src = pickSrcFromSrcset(strings.TrimSpace(getAttr(n, "srcset")))
	}
	return src
}

func imageNodeAlt(n *html.Node) string {
	if n == nil {
		return "Image"
	}
	for _, attr := range []string{"alt", "aria-label", "title"} {
		if value := condenseSpaces(getAttr(n, attr)); value != "" {
			return value
		}
	}
	return "Image"
}

func imageNodeDimension(n *html.Node, st *walkState, prefs RenderOptions, containingWidth int) (int, int) {
	if n == nil {
		return 0, 0
	}
	width, height := 0, 0
	widthCSS, heightCSS := "", ""
	if st != nil && st.css != nil {
		props := computeStyleFor(n, st.css)
		widthCSS = cssPropValue(props, getAttr(n, "style"), "width")
		heightCSS = cssPropValue(props, getAttr(n, "style"), "height")
		widthBase := prefs.ScreenW
		if containingWidth > 0 {
			widthBase = containingWidth
		}
		width = cssValueToPx(widthCSS, widthBase)
		height = cssValueToPx(heightCSS, prefs.ScreenH)
	}
	if width <= 0 && strings.TrimSpace(widthCSS) == "" {
		width, _ = strconv.Atoi(strings.TrimSpace(getAttr(n, "width")))
	}
	if height <= 0 && strings.TrimSpace(heightCSS) == "" {
		height, _ = strconv.Atoi(strings.TrimSpace(getAttr(n, "height")))
	}
	return width, height
}

func renderSizedImageNode(n *html.Node, base string, p renderTarget, st *walkState, prefs RenderOptions, maxWidth int) {
	src := imageNodeSource(n)
	alt := imageNodeAlt(n)
	width, height := imageNodeDimension(n, st, prefs, maxWidth)
	if width <= 0 && height <= 0 {
		renderImageFromURL(p, st, base, src, alt, prefs)
		return
	}
	if !prefs.ImagesOn || src == "" {
		p.AddText("[" + alt + "]")
		return
	}
	abs := resolveAbsURL(base, src)
	decoded, ok := loadFloatGridImage(abs, prefs)
	if !ok {
		renderImageFromURL(p, st, base, src, alt, prefs)
		return
	}
	sourceW, sourceH := decoded.Bounds().Dx(), decoded.Bounds().Dy()
	if width <= 0 && sourceH > 0 {
		width = int(math.Round(float64(sourceW) * float64(height) / float64(sourceH)))
	}
	if height <= 0 && sourceW > 0 {
		height = int(math.Round(float64(sourceH) * float64(width) / float64(sourceW)))
	}
	if maxWidth > 0 && width > maxWidth {
		if width > 0 && height > 0 {
			height = int(math.Round(float64(height) * float64(maxWidth) / float64(width)))
		}
		width = maxWidth
	}
	width, height = constrainLegacyBasicInlineImage(prefs, width, height)
	if width <= 0 || height <= 0 {
		renderImageFromURL(p, st, base, src, alt, prefs)
		return
	}
	resized := image.NewRGBA(image.Rect(0, 0, width, height))
	xdraw.CatmullRom.Scale(resized, resized.Bounds(), decoded, decoded.Bounds(), stddraw.Over, nil)
	data, encodedW, encodedH, _, _, err := encodeImage(resized, prefs)
	if err != nil || len(data) == 0 {
		renderImageFromURL(p, st, base, src, alt, prefs)
		return
	}
	if prefs.MaxInlineKB <= 0 || len(data) <= prefs.MaxInlineKB*1024 {
		p.AddImageInline(encodedW, encodedH, data)
		return
	}
	p.AddImagePlaceholder(encodedW, encodedH)
}

func emitLinkedImage(item linkedImage, base string, p renderTarget, st *walkState, prefs RenderOptions) bool {
	if item.image == nil {
		return false
	}
	link := ""
	if item.link != nil {
		link, _ = resolveNavigableLink(base, getAttr(item.link, "href"))
	}
	if link != "" {
		p.BeginLink(link)
	}
	previous := st.inLink
	st.inLink = link != ""
	renderSizedImageNode(item.image, base, p, st, prefs, item.maxW)
	st.inLink = previous
	if link != "" {
		p.EndLink()
	}
	return true
}

func toolbarRow(table *html.Node) *html.Node {
	if table == nil {
		return nil
	}
	for child := table.FirstChild; child != nil; child = child.NextSibling {
		if child.Type != html.ElementNode {
			continue
		}
		if strings.EqualFold(child.Data, "tr") {
			return child
		}
		if strings.EqualFold(child.Data, "thead") || strings.EqualFold(child.Data, "tbody") || strings.EqualFold(child.Data, "tfoot") {
			if row := findFirstChild(child, "tr"); row != nil {
				return row
			}
		}
	}
	return nil
}

func nearestBackground(n *html.Node, st *walkState) color.RGBA {
	fallback := color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
	if st != nil {
		fallback = omRasterBackground(st.curBg, fallback)
	}
	for current := n; current != nil; current = current.Parent {
		if current.Type != html.ElementNode || st == nil || st.css == nil {
			continue
		}
		props := computeStyleFor(current, st.css)
		if value := cssPropValue(props, getAttr(current, "style"), "background-color"); value != "" && !strings.EqualFold(strings.TrimSpace(value), "transparent") {
			return omRasterBackground(value, fallback)
		}
	}
	return fallback
}

func renderToolbarTile(item linkedImage, table *html.Node, base string, st *walkState, prefs RenderOptions, width, height int) ([]byte, int, int, bool) {
	abs := resolveAbsURL(base, imageNodeSource(item.image))
	icon, ok := loadFloatGridImage(abs, prefs)
	if !ok {
		return nil, 0, 0, false
	}
	iconW, iconH := icon.Bounds().Dx(), icon.Bounds().Dy()
	if st != nil && st.css != nil {
		props := computeStyleFor(item.image, st.css)
		if value := cssValueToPx(cssPropValue(props, getAttr(item.image, "style"), "width"), width); value > 0 {
			iconW = value
		}
		if value := cssValueToPx(cssPropValue(props, getAttr(item.image, "style"), "height"), height); value > 0 {
			iconH = value
		}
	}
	maxW, maxH := width-6, height-6
	if maxW < 1 || maxH < 1 {
		return nil, 0, 0, false
	}
	scale := math.Min(1, math.Min(float64(maxW)/float64(iconW), float64(maxH)/float64(iconH)))
	iconW = int(math.Round(float64(iconW) * scale))
	iconH = int(math.Round(float64(iconH) * scale))

	tile := image.NewRGBA(image.Rect(0, 0, width, height))
	stddraw.Draw(tile, tile.Bounds(), image.NewUniform(nearestBackground(table, st)), image.Point{}, stddraw.Src)
	x := (width - iconW) / 2
	y := (height - iconH) / 2
	xdraw.CatmullRom.Scale(tile, image.Rect(x, y, x+iconW, y+iconH), icon, icon.Bounds(), stddraw.Over, nil)
	encoded, encodedW, encodedH, _, _, err := encodeImage(tile, prefs)
	return encoded, encodedW, encodedH, err == nil && len(encoded) > 0
}

// renderIconToolbar preserves single-row navigation tables as equally-sized,
// independently focusable tiles. Linear table recursion otherwise places each
// icon on a separate line because OMS has no table column record.
func renderIconToolbar(table *html.Node, base string, p renderTarget, st *walkState, prefs RenderOptions) bool {
	if !prefs.ImagesOn || st == nil || st.css == nil {
		return false
	}
	row := toolbarRow(table)
	if row == nil {
		return false
	}
	var items []linkedImage
	for _, cell := range elementChildren(row) {
		if !strings.EqualFold(cell.Data, "td") && !strings.EqualFold(cell.Data, "th") {
			continue
		}
		link := findFirstByTag(cell, "a")
		img := findFirstByTag(cell, "img")
		if link == nil || img == nil || strings.TrimSpace(collectText(cell)) != "" {
			return false
		}
		if _, ok := resolveNavigableLink(base, getAttr(link, "href")); !ok {
			return false
		}
		items = append(items, linkedImage{link: link, image: img})
	}
	if len(items) < 3 || len(items) > 6 {
		return false
	}
	screenW := prefs.ScreenW
	if screenW <= 0 {
		screenW = 240
	}
	height := 28
	for index, item := range items {
		left := screenW * index / len(items)
		right := screenW * (index + 1) / len(items)
		tileWidth := right - left
		if prefs.LegacyBasicOM2 && tileWidth > legacyBasicInlineImageLimit {
			tileWidth = legacyBasicInlineImageLimit
		}
		data, width, encodedH, ok := renderToolbarTile(item, table, base, st, prefs, tileWidth, height)
		if !ok {
			return false
		}
		link, _ := resolveNavigableLink(base, getAttr(item.link, "href"))
		p.BeginLink(link)
		p.AddImageInline(width, encodedH, data)
		p.EndLink()
	}
	p.AddBreak()
	return true
}

// renderInlineImageStrip keeps image-only inline-block children in one OMS
// paragraph. This covers compact photo/video strips without mistaking general
// text columns for a grid.
func renderInlineImageStrip(container *html.Node, base string, p renderTarget, st *walkState, prefs RenderOptions) bool {
	if !prefs.ImagesOn || st == nil || st.css == nil {
		return false
	}
	children := elementChildren(container)
	if len(children) < 2 || len(children) > 6 {
		return false
	}
	items := make([]linkedImage, 0, len(children))
	containerWidth := prefs.ScreenW
	containerProps := computeStyleFor(container, st.css)
	if width := cssValueToPx(cssPropValue(containerProps, getAttr(container, "style"), "width"), prefs.ScreenW); width > 0 {
		containerWidth = width
	}
	if width := cssValueToPx(cssPropValue(containerProps, getAttr(container, "style"), "max-width"), prefs.ScreenW); width > 0 && (containerWidth <= 0 || width < containerWidth) {
		containerWidth = width
	}
	if containerWidth <= 0 {
		containerWidth = 240
	}
	for _, child := range children {
		props := computeStyleFor(child, st.css)
		display := strings.ToLower(strings.TrimSpace(cssPropValue(props, getAttr(child, "style"), "display")))
		if display != "inline-block" || strings.TrimSpace(collectText(child)) != "" {
			return false
		}
		link := findFirstByTag(child, "a")
		img := findFirstByTag(child, "img")
		if link == nil || img == nil {
			return false
		}
		childWidth := cssValueToPx(cssPropValue(props, getAttr(child, "style"), "width"), containerWidth)
		items = append(items, linkedImage{link: link, image: img, maxW: childWidth})
	}
	for _, item := range items {
		if !emitLinkedImage(item, base, p, st, prefs) {
			return false
		}
	}
	p.AddBreak()
	return true
}

func directSectionHeading(container *html.Node) *html.Node {
	for _, child := range elementChildren(container) {
		switch strings.ToLower(child.Data) {
		case "h1", "h2", "h3", "h4", "h5", "h6":
			return child
		}
	}
	return nil
}

// renderSectionTitle orders the semantic heading before a floated action link
// and keeps both on one compact line ("Popular photos · All").
func legacyBasicSectionGap(screenW int, title, action string) string {
	if screenW <= 0 {
		screenW = 240
	}
	// The original OM2 renderer has no inline float primitive: alignment bits
	// apply to the complete paragraph. Its default small MIDP font is roughly
	// seven pixels per printable glyph and four pixels per space. Leave a small
	// safety margin so a slightly wider vendor font does not wrap the action.
	const (
		glyphWidth = 7
		spaceWidth = 4
		margin     = 12
	)
	used := (utf8.RuneCountInString(title) + utf8.RuneCountInString(action)) * glyphWidth
	spaces := (screenW - margin - used) / spaceWidth
	// Short labels hit a visibly earlier x-position on the tiny built-in MIDP
	// font than the conservative average above. Compensate only for those; long
	// headings already line up well and need the extra wrap safety.
	if utf8.RuneCountInString(title) <= 10 {
		spaces += 11
	}
	if spaces < 2 {
		spaces = 2
	}
	if spaces > 52 {
		spaces = 52
	}
	return strings.Repeat(" ", spaces)
}

func renderSectionTitle(container *html.Node, base string, p renderTarget, st *walkState, prefs RenderOptions) bool {
	if !hasAnyClass(container, "title", "section_title") {
		return false
	}
	heading := directSectionHeading(container)
	if heading == nil {
		return false
	}
	var action *html.Node
	for _, child := range elementChildren(container) {
		if !strings.EqualFold(child.Data, "a") {
			continue
		}
		if hasClass(child, "right") || (st != nil && st.css != nil && strings.EqualFold(strings.TrimSpace(computeStyleFor(child, st.css)["float"]), "right")) {
			action = child
			break
		}
	}
	title := strings.TrimSpace(condenseSpaces(collectText(heading)))
	actionText := ""
	if action != nil {
		actionText = strings.TrimSpace(condenseSpaces(collectText(action)))
	}
	if title == "" || actionText == "" {
		return false
	}
	title = applyNodeTextTransform(container, st, title)
	actionText = applyNodeTextTransform(container, st, actionText)
	st.pushStyle(p, st.curStyle|styleBoldBit)
	p.AddText(title)
	st.popStyle(p)
	if prefs.LegacyBasicOM2 {
		p.AddText(legacyBasicSectionGap(prefs.ScreenW, title, actionText))
	} else {
		p.AddText(" · ")
	}
	link, ok := resolveNavigableLink(base, getAttr(action, "href"))
	if ok {
		p.BeginLink(link)
	}
	if st.linkColor != "" {
		st.pushColor(p, st.linkColor)
	}
	p.AddText(actionText)
	if st.linkColor != "" {
		st.popColor(p)
	}
	if ok {
		p.EndLink()
	}
	p.AddBreak()
	return true
}

func nodeFloat(n *html.Node, st *walkState) string {
	if n == nil {
		return ""
	}
	if hasClass(n, "left") {
		return "left"
	}
	if hasClass(n, "right") {
		return "right"
	}
	if st == nil || st.css == nil {
		return ""
	}
	props := computeStyleFor(n, st.css)
	return strings.ToLower(strings.TrimSpace(cssPropValue(props, getAttr(n, "style"), "float")))
}

func walkNodeOnly(n *html.Node, base string, p renderTarget, visited map[*html.Node]bool, st *walkState, prefs RenderOptions) {
	if n == nil {
		return
	}
	next := n.NextSibling
	n.NextSibling = nil
	walkRich(n, base, p, visited, st, prefs)
	n.NextSibling = next
}

// renderFloatMetadataRow linearizes a compact left/right pair without letting
// the right-alignment style affect the whole OMS paragraph.
func renderFloatMetadataRow(container *html.Node, base string, p renderTarget, visited map[*html.Node]bool, st *walkState, prefs RenderOptions) bool {
	children := elementChildren(container)
	if len(children) != 2 || nodeFloat(children[0], st) != "left" || nodeFloat(children[1], st) != "right" {
		return false
	}
	leftText := strings.TrimSpace(condenseSpaces(collectText(children[0])))
	rightText := strings.TrimSpace(condenseSpaces(collectText(children[1])))
	if leftText == "" || rightText == "" || len([]rune(leftText))+len([]rune(rightText)) > 120 {
		return false
	}
	walkNodeOnly(children[0], base, p, visited, st, prefs)
	p.AddText(" · ")
	walkNodeOnly(children[1], base, p, visited, st, prefs)
	p.AddBreak()
	return true
}

// renderLeadingRightMetadata places a short leading right float (typically a
// timestamp) after the identity block it annotates, then renders the remaining
// card body in source order.
func renderLeadingRightMetadata(container *html.Node, base string, p renderTarget, visited map[*html.Node]bool, st *walkState, prefs RenderOptions) bool {
	children := elementChildren(container)
	if len(children) < 2 || nodeFloat(children[0], st) != "right" {
		return false
	}
	meta := strings.TrimSpace(condenseSpaces(collectText(children[0])))
	primary := strings.TrimSpace(condenseSpaces(collectText(children[1])))
	if meta == "" || primary == "" || len([]rune(meta)) > 48 || findFirstByTag(children[0], "img") != nil {
		return false
	}
	walkNodeOnly(children[1], base, p, visited, st, prefs)
	p.AddText(" · ")
	walkNodeOnly(children[0], base, p, visited, st, prefs)
	p.AddBreak()
	for _, child := range children[2:] {
		walkNodeOnly(child, base, p, visited, st, prefs)
	}
	return true
}

// renderMediaObject handles the common mobile pattern "floated thumbnail +
// overflow-hidden text" as one logical first line followed by normal text.
func renderMediaObject(container *html.Node, base string, p renderTarget, visited map[*html.Node]bool, st *walkState, prefs RenderOptions) bool {
	if st == nil || st.css == nil {
		return false
	}
	children := elementChildren(container)
	if len(children) < 2 || len(children) > 3 {
		return false
	}
	media := children[0]
	props := computeStyleFor(media, st.css)
	if !strings.EqualFold(strings.TrimSpace(cssPropValue(props, getAttr(media, "style"), "float")), "left") {
		return false
	}
	img := findFirstByTag(media, "img")
	if img == nil || strings.TrimSpace(collectText(children[1])) == "" {
		return false
	}
	link := findFirstByTag(media, "a")
	if !emitLinkedImage(linkedImage{link: link, image: img}, base, p, st, prefs) {
		return false
	}
	p.AddText(" ")
	for _, content := range children[1:] {
		if content.FirstChild != nil {
			walkRich(content.FirstChild, base, p, visited, st, prefs)
		}
	}
	p.AddBreak()
	return true
}
