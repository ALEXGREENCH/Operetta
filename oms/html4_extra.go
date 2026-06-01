package oms

import (
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

type elementContext struct {
	node    *html.Node
	base    string
	page    *Page
	visited map[*html.Node]bool
	state   *walkState
	prefs   RenderOptions
}

func (ctx *elementContext) renderChildren() {
	if ctx.node.FirstChild != nil {
		walkRich(ctx.node.FirstChild, ctx.base, ctx.page, ctx.visited, ctx.state, ctx.prefs)
	}
}

func (ctx *elementContext) pushStyle(style uint32) func() {
	if style == ctx.state.curStyle {
		return func() {}
	}
	ctx.state.pushStyle(ctx.page, style)
	return func() { ctx.state.popStyle(ctx.page) }
}

func (ctx *elementContext) pushColor(hex string) func() {
	if hex == "" {
		return func() {}
	}
	ctx.state.pushColor(ctx.page, hex)
	return func() { ctx.state.popColor(ctx.page) }
}

var extraHTML4Handlers map[string]func(*elementContext) bool

func init() {
	extraHTML4Handlers = map[string]func(*elementContext) bool{
		"font":     handleFont,
		"basefont": handleBaseFont,
		"dir":      handleDirectoryList,
		"menu":     handleDirectoryList,
		"frameset": handleFrameSet,
		"frame":    handleFrame,
		"noframes": handleNoFrames,
		"map":      handleMap,
		"area":     handleArea,
		"marquee":  handleMarquee,
		"blink":    handleBlink,
		"abbr":     handleAbbr,
		"acronym":  handleAbbr,
		"bdo":      handleBdo,
		"isindex":  handleIsIndex,
		"applet":   handleApplet,
		"param":    handleParam,
		"b":        handleBold,
		"strong":   handleBold,
		"i":        handleItalic,
		"em":       handleItalic,
		"u":        handleUnderline,
		"s":        handleStrike,
		"strike":   handleStrike,
		"del":      handleStrike,
		"ins":      handleUnderline,
	}
}

func handleBold(ctx *elementContext) bool {
	cleanup := ctx.pushStyle(ctx.state.curStyle | styleBoldBit)
	defer cleanup()
	ctx.renderChildren()
	return true
}

func handleItalic(ctx *elementContext) bool {
	cleanup := ctx.pushStyle(ctx.state.curStyle | styleItalicBit)
	defer cleanup()
	ctx.renderChildren()
	return true
}

func handleUnderline(ctx *elementContext) bool {
	cleanup := ctx.pushStyle(ctx.state.curStyle | styleUnderBit)
	defer cleanup()
	ctx.renderChildren()
	return true
}

func handleStrike(ctx *elementContext) bool {
	// В OMS/OM2 отдельного бита для зачёркивания нет.
	// Минимально отличим текст (как делают старые прокси) – используем underline.
	// Если позже введёшь собственный бит – поменяй здесь.
	cleanup := ctx.pushStyle(ctx.state.curStyle | styleUnderBit)
	defer cleanup()
	ctx.renderChildren()
	return true
}

func handleFont(ctx *elementContext) bool {
	color := cssToHex(getAttr(ctx.node, "color"))
	cleanupColor := ctx.pushColor(color)
	defer cleanupColor()

	style := ctx.state.curStyle
	sizeAttr := strings.TrimSpace(getAttr(ctx.node, "size"))
	if sizeAttr != "" {
		if strings.HasPrefix(sizeAttr, "+") {
			if v, err := strconv.Atoi(sizeAttr[1:]); err == nil && v > 0 {
				style |= styleBoldBit
			}
		} else if strings.HasPrefix(sizeAttr, "-") {
			if v, err := strconv.Atoi(sizeAttr[1:]); err == nil && v > 0 {
				style |= styleItalicBit
			}
		} else if v, err := strconv.Atoi(sizeAttr); err == nil {
			if v >= 5 {
				style |= styleBoldBit
			} else if v <= 2 {
				style |= styleItalicBit
			}
		}
	}
	cleanupStyle := ctx.pushStyle(style)
	defer cleanupStyle()

	ctx.renderChildren()
	return true
}

func handleBaseFont(ctx *elementContext) bool {
	if col := cssToHex(getAttr(ctx.node, "color")); col != "" {
		ctx.page.AddTextcolor(col)
		ctx.state.curColor = col
	}
	if size := strings.TrimSpace(getAttr(ctx.node, "size")); size != "" {
		if v, err := strconv.Atoi(strings.TrimLeft(size, "+")); err == nil && v >= 5 {
			ctx.state.curStyle |= styleBoldBit
			ctx.page.AddStyle(ctx.state.curStyle)
		}
	}
	return true
}

func handleDirectoryList(ctx *elementContext) bool {
	ctx.state.pushList("ul")
	if top := ctx.state.currentList(); top != nil {
		top.bullet = "- "
	}
	ctx.renderChildren()
	return true
}

func handleFrameSet(ctx *elementContext) bool {
	frames := []*html.Node{}
	for child := ctx.node.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.ElementNode && strings.EqualFold(child.Data, "frame") {
			frames = append(frames, child)
		}
	}
	if len(frames) == 0 {
		// If nested framesets exist, render children so handlers can see them.
		ctx.renderChildren()
		return true
	}
	ctx.page.AddParagraph()
	ctx.page.AddPlus()
	ctx.page.AddText("Frames")
	ctx.page.AddBreak()
	for i, fr := range frames {
		label := strings.TrimSpace(getAttr(fr, "title"))
		if label == "" {
			label = strings.TrimSpace(getAttr(fr, "name"))
		}
		if label == "" {
			label = fmt.Sprintf("Frame %d", i+1)
		}
		src := strings.TrimSpace(getAttr(fr, "src"))
		if src == "" {
			ctx.page.AddText(label)
		} else {
			ctx.page.AddLink(resolveLink(ctx.base, src), label)
		}
		ctx.page.AddBreak()
	}
	return true
}

func handleFrame(ctx *elementContext) bool {
	src := strings.TrimSpace(getAttr(ctx.node, "src"))
	if src == "" {
		return true
	}
	label := strings.TrimSpace(getAttr(ctx.node, "title"))
	if label == "" {
		label = strings.TrimSpace(getAttr(ctx.node, "name"))
	}
	if label == "" {
		label = "[Frame]"
	}
	ctx.page.AddLink(resolveLink(ctx.base, src), label)
	ctx.page.AddBreak()
	return true
}

func handleNoFrames(ctx *elementContext) bool {
	ctx.page.AddParagraph()
	ctx.renderChildren()
	return true
}

func handleMap(ctx *elementContext) bool {
	name := strings.TrimSpace(getAttr(ctx.node, "name"))
	if name != "" {
		ctx.page.AddPlus()
		ctx.page.AddText("Image Map: " + name)
		ctx.page.AddBreak()
	}
	for area := ctx.node.FirstChild; area != nil; area = area.NextSibling {
		if area.Type != html.ElementNode || !strings.EqualFold(area.Data, "area") {
			continue
		}
		href := strings.TrimSpace(getAttr(area, "href"))
		label := strings.TrimSpace(getAttr(area, "alt"))
		if label == "" {
			label = "Area"
		}
		if href != "" {
			ctx.page.AddLink(resolveLink(ctx.base, href), label)
		} else {
			ctx.page.AddText(label)
		}
		ctx.page.AddBreak()
	}
	return true
}

func handleArea(ctx *elementContext) bool {
	href := strings.TrimSpace(getAttr(ctx.node, "href"))
	label := strings.TrimSpace(getAttr(ctx.node, "alt"))
	if label == "" {
		label = "Area"
	}
	if href != "" {
		ctx.page.AddLink(resolveLink(ctx.base, href), label)
	} else {
		ctx.page.AddText(label)
	}
	ctx.page.AddBreak()
	return true
}

func handleMarquee(ctx *elementContext) bool {
	cleanup := ctx.pushStyle(ctx.state.curStyle | styleBoldBit)
	defer cleanup()
	ctx.page.AddText("? ")
	ctx.renderChildren()
	ctx.page.AddText(" ?")
	ctx.page.AddBreak()
	return true
}

func handleBlink(ctx *elementContext) bool {
	cleanup := ctx.pushStyle(ctx.state.curStyle | styleItalicBit)
	defer cleanup()
	ctx.page.AddText("*")
	ctx.renderChildren()
	ctx.page.AddText("*")
	return true
}

func handleAbbr(ctx *elementContext) bool {
	title := strings.TrimSpace(getAttr(ctx.node, "title"))
	ctx.renderChildren()
	if title != "" {
		ctx.page.AddText(" (" + title + ")")
	}
	return true
}

func handleBdo(ctx *elementContext) bool {
	dir := strings.ToLower(strings.TrimSpace(getAttr(ctx.node, "dir")))
	switch dir {
	case "rtl":
		cleanup := ctx.pushStyle((ctx.state.curStyle | styleRightBit) &^ styleCenterBit)
		defer cleanup()
		ctx.renderChildren()
	case "ltr":
		cleanup := ctx.pushStyle(ctx.state.curStyle &^ styleRightBit)
		defer cleanup()
		ctx.renderChildren()
	default:
		ctx.renderChildren()
	}
	return true
}

func handleIsIndex(ctx *elementContext) bool {
	prompt := strings.TrimSpace(getAttr(ctx.node, "prompt"))
	if prompt == "" {
		prompt = "Search"
	}
	ctx.page.AddParagraph()
	ctx.page.AddForm("")
	ctx.page.AddText(prompt + ": ")
	ctx.page.AddTextInput("isindex", "")
	ctx.page.AddSubmit("submit", "Go")
	ctx.page.AddBreak()
	return true
}

func handleApplet(ctx *elementContext) bool {
	src := strings.TrimSpace(getAttr(ctx.node, "codebase"))
	if src == "" {
		src = strings.TrimSpace(getAttr(ctx.node, "archive"))
	}
	if src == "" {
		src = strings.TrimSpace(getAttr(ctx.node, "code"))
	}
	label := strings.TrimSpace(getAttr(ctx.node, "alt"))
	if label == "" {
		label = "[Applet]"
	}
	if src != "" {
		ctx.page.AddLink(resolveLink(ctx.base, src), label)
	} else {
		ctx.page.AddText(label)
	}
	params := collectAppletParams(ctx.node)
	if len(params) > 0 {
		for _, p := range params {
			ctx.page.AddText(" " + p)
		}
		ctx.page.AddBreak()
	}
	return true
}

func handleParam(ctx *elementContext) bool {
	parent := ctx.node.Parent
	if parent == nil {
		return true
	}
	tag := strings.ToLower(parent.Data)
	if tag != "object" && tag != "applet" {
		return true
	}
	name := strings.TrimSpace(getAttr(ctx.node, "name"))
	value := strings.TrimSpace(getAttr(ctx.node, "value"))
	if name != "" && value != "" {
		ctx.page.AddText(fmt.Sprintf("[%s=%s]", name, value))
		ctx.page.AddBreak()
	}
	return true
}

func collectAppletParams(n *html.Node) []string {
	out := []string{}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode || !strings.EqualFold(c.Data, "param") {
			continue
		}
		name := strings.TrimSpace(getAttr(c, "name"))
		value := strings.TrimSpace(getAttr(c, "value"))
		if name == "" || value == "" {
			continue
		}
		out = append(out, fmt.Sprintf("%s=%s", name, value))
	}
	return out
}
