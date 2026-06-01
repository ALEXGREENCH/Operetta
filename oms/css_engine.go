package oms

import (
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/andybalholm/cascadia"
	cssast "github.com/aymerick/douceur/css"
	"github.com/aymerick/douceur/parser"
	"golang.org/x/net/html"
)

type propState struct {
	val       string
	spec      cascadia.Specificity
	order     int
	important bool
}

type cssDeclaration struct {
	property  string
	value     string
	important bool
}

type cssRule struct {
	selector     cascadia.Sel
	specificity  cascadia.Specificity
	declarations []cssDeclaration
	order        int
}

type Stylesheet struct {
	rules []cssRule
}

// Expose limited helpers for debug tools.
func BuildStylesheetForDebug(doc *html.Node, base string, hdr http.Header, jar http.CookieJar) *Stylesheet {
	return buildStylesheet(doc, base, hdr, jar)
}

func ComputeStyleForDebug(n *html.Node, ss *Stylesheet) map[string]string {
	return computeStyleFor(n, ss)
}

type cssParseContext struct {
	baseURL string
	hdr     http.Header
	jar     http.CookieJar
	opts    *RenderOptions
	depth   int
	visited map[string]struct{}
	budget  *int
}

func (ctx *cssParseContext) child(newBase string) *cssParseContext {
	if ctx == nil {
		return &cssParseContext{baseURL: newBase}
	}
	next := *ctx
	next.baseURL = newBase
	next.depth = ctx.depth + 1
	return &next
}

func buildStylesheet(
	doc *html.Node,
	base string,
	hdr http.Header,
	jar http.CookieJar,
	opts ...*RenderOptions,
) *Stylesheet {
	if doc == nil {
		return nil
	}

	var ro *RenderOptions
	if len(opts) > 0 {
		ro = opts[0]
	}

	ss := &Stylesheet{}
	order := 0
	budget := 16

	ctx := &cssParseContext{
		baseURL: base,
		hdr:     hdr,
		jar:     jar,
		opts:    ro,
		visited: map[string]struct{}{},
		budget:  &budget,
	}

	var collectInline func(*html.Node)
	collectInline = func(n *html.Node) {
		if n.Type == html.ElementNode && strings.EqualFold(n.Data, "style") {
			if n.FirstChild != nil && n.FirstChild.Type == html.TextNode {
				if rs, ord := parseCSSText(n.FirstChild.Data, order, ctx); len(rs) > 0 {
					ss.rules = append(ss.rules, rs...)
					order = ord
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			collectInline(c)
		}
	}
	collectInline(doc)

	var links []string
	var collectLinks func(*html.Node)
	collectLinks = func(n *html.Node) {
		if n.Type == html.ElementNode && strings.EqualFold(n.Data, "link") {
			rel := strings.ToLower(strings.TrimSpace(getAttr(n, "rel")))
			if rel != "" && !strings.Contains(rel, "stylesheet") {
				// not a stylesheet link
			} else {
				typ := strings.ToLower(strings.TrimSpace(getAttr(n, "type")))
				if typ != "" && typ != "text/css" {
					// skip non-css types
				} else if href := strings.TrimSpace(getAttr(n, "href")); href != "" {
					links = append(links, href)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			collectLinks(c)
		}
	}
	collectLinks(doc)

	const maxExternal = 12
	for _, link := range links {
		if len(ss.rules) >= maxExternal*64 {
			break
		}
		if ctx.budget != nil && *ctx.budget <= 0 {
			break
		}
		abs := resolveAbsURL(base, link)
		if abs == "" {
			continue
		}
		if ctx.visited != nil {
			if _, seen := ctx.visited[abs]; seen {
				continue
			}
			ctx.visited[abs] = struct{}{}
		}
		if ctx.budget != nil {
			*ctx.budget--
		}
		if b, ok := fetchText(abs, hdr, jar, "text/css"); ok {
			if rs, ord := parseCSSText(string(b), order, ctx.child(abs)); len(rs) > 0 {
				ss.rules = append(ss.rules, rs...)
				order = ord
			}
		}
	}

	if len(ss.rules) == 0 {
		return nil
	}
	return ss
}

func parseCSSText(txt string, startOrder int, ctx *cssParseContext) ([]cssRule, int) {
	trimmed := strings.TrimSpace(txt)
	if trimmed == "" {
		return nil, startOrder
	}
	if ctx != nil && ctx.depth >= 16 {
		return nil, startOrder
	}
	sheet, err := parser.Parse(trimmed)
	if err != nil && (sheet == nil || len(sheet.Rules) == 0) {
		// Douceur failed hard; try very simple fallback parser so we can at least
		// extract straightforward class selectors like `.balls { ... }`.
		if fr, ord := parseSimpleCSSFallback(trimmed, startOrder); len(fr) > 0 {
			return fr, ord
		}
		fmt.Println("Error parsing CSS text:", err)
		return nil, startOrder
	}

	rules := make([]cssRule, 0, len(sheet.Rules)*2)
	order := startOrder

	var walk func([]*cssast.Rule, *cssParseContext)
	walk = func(list []*cssast.Rule, cur *cssParseContext) {
		if cur != nil && cur.depth >= 16 {
			return
		}
		for _, rule := range list {
			if rule == nil {
				continue
			}
			switch rule.Kind {
			case cssast.AtRule:
				name := strings.ToLower(strings.TrimSpace(rule.Name))
				switch name {
				case "@media":
					if mediaRuleActive(rule.Prelude, curOpts(cur)) {
						walk(rule.Rules, cur)
					}
				case "@supports":
					walk(rule.Rules, cur)
				case "@import":
					importURL, media := extractImportTarget(rule.Prelude)
					if importURL == "" {
						continue
					}
					if media != "" && !mediaRuleActive(media, curOpts(cur)) {
						continue
					}
					abs := resolveAbsURL(curBase(cur), importURL)
					if abs == "" {
						abs = importURL
					}
					if cur != nil && cur.visited != nil {
						if _, seen := cur.visited[abs]; seen {
							continue
						}
						cur.visited[abs] = struct{}{}
					}
					if cur != nil && cur.budget != nil {
						if *cur.budget <= 0 {
							continue
						}
						*cur.budget--
					}
					if b, ok := fetchText(abs, curHeader(cur), curJar(cur), "text/css"); ok {
						child := cur.child(abs)
						if rs, ord := parseCSSText(string(b), order, child); len(rs) > 0 {
							rules = append(rules, rs...)
							order = ord
						}
					}
				default:
					if rule.EmbedsRules() {
						walk(rule.Rules, cur)
					}
				}
			case cssast.QualifiedRule:
				decls := convertDeclarations(rule.Declarations, curBase(cur))
				if len(decls) == 0 || len(rule.Selectors) == 0 {
					continue
				}
				// Sanitize unsupported pseudo selectors to avoid parser errors
				sanitized := make([]string, 0, len(rule.Selectors))
				for _, s := range rule.Selectors {
					if ss := sanitizeSelectorForCascadia(s); ss != "" {
						sanitized = append(sanitized, ss)
					}
				}
				if len(sanitized) == 0 {
					continue
				}
				group, err := cascadia.ParseGroup(strings.Join(sanitized, ","))
				if err != nil {
					if cssDebug() {
						fmt.Println("Error parsing CSS group:", err)
					}
					continue
				}
				for _, sel := range group {
					if sel == nil || sel.PseudoElement() != "" {
						continue
					}
					rules = append(rules, cssRule{selector: sel, specificity: sel.Specificity(), declarations: cloneDecls(decls), order: order})
					order++
				}
			}
		}
	}

	walk(sheet.Rules, ctx)
	return rules, order
}

// parseSimpleCSSFallback implements a best-effort CSS extractor that handles only
// flat rule-sets: selector-list '{' declarations '}'. It ignores at-rules and
// nested constructs. It's intentionally conservative but robust against most
// minified CSS.
func parseSimpleCSSFallback(txt string, startOrder int) ([]cssRule, int) {
	s := txt
	// Strip comments
	for {
		i := strings.Index(s, "/*")
		if i == -1 {
			break
		}
		j := strings.Index(s[i+2:], "*/")
		if j == -1 {
			s = s[:i]
			break
		}
		s = s[:i] + s[i+2+j+2:]
	}
	out := []cssRule{}
	order := startOrder
	i := 0
	for i < len(s) {
		// Skip whitespace
		for i < len(s) && (s[i] == ' ' || s[i] == '\n' || s[i] == '\r' || s[i] == '\t') {
			i++
		}
		if i >= len(s) {
			break
		}
		// Skip at-rules entirely (until next '{' then balanced '}')
		if s[i] == '@' {
			// consume until block end
			// find first '{'
			open := strings.IndexByte(s[i:], '{')
			if open == -1 {
				break
			}
			open += i
			depth := 0
			j := open
			for j < len(s) {
				if s[j] == '{' {
					depth++
				}
				if s[j] == '}' {
					depth--
					if depth == 0 {
						j++
						break
					}
				}
				j++
			}
			if j <= open {
				break
			}
			i = j
			continue
		}
		// Read selector prelude up to '{'
		j := strings.IndexByte(s[i:], '{')
		if j == -1 {
			break
		}
		pre := strings.TrimSpace(s[i : i+j])
		i = i + j + 1
		// Read declarations up to matching '}'
		depth := 1
		start := i
		for i < len(s) && depth > 0 {
			if s[i] == '{' {
				depth++
			}
			if s[i] == '}' {
				depth--
			}
			i++
		}
		if depth != 0 {
			break
		}
		block := strings.TrimSpace(s[start : i-1])
		// Parse declarations
		var decls []cssDeclaration
		for _, part := range strings.Split(block, ";") {
			kv := strings.SplitN(part, ":", 2)
			if len(kv) != 2 {
				continue
			}
			nd := normalizeDecl(kv[0], kv[1], false, "")
			if len(nd) > 0 {
				decls = append(decls, nd...)
			}
		}
		if len(decls) == 0 {
			continue
		}
		// Build rules for each selector in group
		for _, sel := range strings.Split(pre, ",") {
			sel = strings.TrimSpace(sanitizeSelectorForCascadia(sel))
			if sel == "" {
				continue
			}
			// Ignore complex combinators; keep simple selectors (.class, tag.class, #id)
			gr, err := cascadia.ParseGroup(sel)
			if err != nil || len(gr) == 0 {
				continue
			}
			for _, ssel := range gr {
				if ssel == nil || ssel.PseudoElement() != "" {
					continue
				}
				out = append(out, cssRule{selector: ssel, specificity: ssel.Specificity(), declarations: cloneDecls(decls), order: order})
				order++
			}
		}
	}
	if len(out) == 0 {
		return nil, startOrder
	}
	return out, order
}

// sanitizeSelectorForCascadia removes pseudo-classes/elements (including vendor-specific)
// that cascadia may not understand, producing a simplified selector.
func sanitizeSelectorForCascadia(s string) string {
	if s == "" {
		return s
	}
	b := strings.Builder{}
	// Copy while stripping any ':' pseudo segment and its optional (..)
	i := 0
	for i < len(s) {
		if s[i] == ':' { // start of pseudo
			// skip any successive ':' and ident
			j := i + 1
			for j < len(s) && (isCSSIdentChar(s[j]) || s[j] == '-') {
				j++
			}
			// optional functional pseudo
			if j < len(s) && s[j] == '(' {
				depth := 1
				j++
				for j < len(s) && depth > 0 {
					if s[j] == '(' {
						depth++
					}
					if s[j] == ')' {
						depth--
					}
					j++
				}
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	out := strings.TrimSpace(b.String())
	// Collapse accidental multiple spaces
	out = strings.Join(strings.Fields(out), " ")
	return out
}

func isCSSIdentChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '\\'
}

func cssDebug() bool { return os.Getenv("OMS_CSS_DEBUG") == "1" }

func cloneDecls(src []cssDeclaration) []cssDeclaration {
	out := make([]cssDeclaration, len(src))
	copy(out, src)
	return out
}

// normalizeDecl expands shorthands and absolutizes URL-bearing values relative to base.
func normalizeDecl(prop, val string, important bool, base string) []cssDeclaration {
	p := strings.ToLower(strings.TrimSpace(prop))
	v := strings.TrimSpace(val)
	if p == "" || v == "" {
		return nil
	}
	if p == "background" {
		return expandBackgroundShorthand(v, important, base)
	}
	if (p == "background-image" || p == "list-style-image") && strings.Contains(strings.ToLower(v), "url(") {
		return []cssDeclaration{{property: p, value: absolutizeCSSURLs(v, base), important: important}}
	}
	return []cssDeclaration{{property: p, value: v, important: important}}
}

func convertDeclarations(list []*cssast.Declaration, base string) []cssDeclaration {
	if len(list) == 0 {
		return nil
	}
	out := make([]cssDeclaration, 0, len(list))
	for _, decl := range list {
		if decl == nil {
			continue
		}
		nd := normalizeDecl(decl.Property, decl.Value, decl.Important, base)
		if len(nd) > 0 {
			out = append(out, nd...)
		}
	}
	return out
}

// expandBackgroundShorthand extracts image, position, repeat and color from background shorthand.
func expandBackgroundShorthand(val string, important bool, base string) []cssDeclaration {
	v := strings.TrimSpace(val)
	if v == "" {
		return nil
	}
	lower := strings.ToLower(v)
	out := []cssDeclaration{}
	// image
	if strings.Contains(lower, "url(") {
		out = append(out, cssDeclaration{property: "background-image", value: absolutizeCSSURLs(v, base), important: important})
	}
	// repeat
	if strings.Contains(lower, "no-repeat") {
		out = append(out, cssDeclaration{property: "background-repeat", value: "no-repeat", important: important})
	} else if strings.Contains(lower, "repeat-x") {
		out = append(out, cssDeclaration{property: "background-repeat", value: "repeat-x", important: important})
	} else if strings.Contains(lower, "repeat-y") {
		out = append(out, cssDeclaration{property: "background-repeat", value: "repeat-y", important: important})
	} else if strings.Contains(lower, "repeat") {
		out = append(out, cssDeclaration{property: "background-repeat", value: "repeat", important: important})
	}
	// position
	posTokens := []string{}
	for _, tok := range strings.Fields(lower) {
		if tok == "/" {
			break
		}
		if strings.HasPrefix(tok, "url(") {
			continue
		}
		if strings.Contains(tok, "repeat") {
			continue
		}
		if cssToHex(tok) != "" {
			continue
		}
		if tok == "left" || tok == "right" || tok == "top" || tok == "bottom" || tok == "center" || strings.HasSuffix(tok, "px") || strings.HasSuffix(tok, "%") {
			posTokens = append(posTokens, tok)
		}
		if len(posTokens) >= 2 {
			break
		}
	}
	if len(posTokens) > 0 {
		out = append(out, cssDeclaration{property: "background-position", value: strings.Join(posTokens, " "), important: important})
	}
	// color
	if col := extractColorFromValue(lower); col != "" {
		out = append(out, cssDeclaration{property: "background-color", value: col, important: important})
	}
	if len(out) == 0 {
		return []cssDeclaration{{property: "background", value: v, important: important}}
	}
	return out
}

// absolutizeCSSURLs converts url(...) inside a value to absolute using base.
func absolutizeCSSURLs(val, base string) string {
	if base == "" || !strings.Contains(strings.ToLower(val), "url(") {
		return val
	}
	out := val
	i := 0
	for i < len(out) {
		j := strings.Index(strings.ToLower(out[i:]), "url(")
		if j == -1 {
			break
		}
		j += i
		k := j + 4
		depth := 1
		for k < len(out) && depth > 0 {
			if out[k] == '(' {
				depth++
			} else if out[k] == ')' {
				depth--
			}
			k++
		}
		if depth != 0 {
			break
		}
		inside := strings.TrimSpace(out[j+4 : k-1])
		unq := trimCSSString(inside)
		if unq != "" && !strings.HasPrefix(unq, "data:") && !strings.Contains(unq, "://") {
			abs := resolveAbsURL(base, unq)
			if abs != "" {
				newv := "url(" + abs + ")"
				out = out[:j] + newv + out[k:]
				i = j + len(newv)
				continue
			}
		}
		i = k
	}
	return out
}

func curOpts(ctx *cssParseContext) *RenderOptions {
	if ctx == nil {
		return nil
	}
	return ctx.opts
}

func curHeader(ctx *cssParseContext) http.Header {
	if ctx == nil {
		return nil
	}
	return ctx.hdr
}

func curJar(ctx *cssParseContext) http.CookieJar {
	if ctx == nil {
		return nil
	}
	return ctx.jar
}

func curBase(ctx *cssParseContext) string {
	if ctx == nil {
		return ""
	}
	return ctx.baseURL
}

func extractImportTarget(prelude string) (string, string) {
	s := strings.TrimSpace(prelude)
	if s == "" {
		return "", ""
	}
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "url(") {
		end := strings.Index(s, ")")
		if end == -1 {
			return "", ""
		}
		inner := strings.TrimSpace(s[4:end])
		target := trimCSSString(inner)
		media := strings.TrimSpace(s[end+1:])
		return target, media
	}
	if (s[0] == '"' || s[0] == '\'') && len(s) > 1 {
		if idx := strings.IndexByte(s[1:], s[0]); idx != -1 {
			target := s[1 : idx+1]
			media := strings.TrimSpace(s[idx+2:])
			return target, media
		}
	}
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return "", ""
	}
	target := trimCSSString(fields[0])
	media := strings.TrimSpace(strings.TrimPrefix(s, fields[0]))
	return target, media
}

func trimCSSString(v string) string {
	vv := strings.TrimSpace(v)
	if len(vv) >= 2 {
		if (vv[0] == '"' && vv[len(vv)-1] == '"') || (vv[0] == '\'' && vv[len(vv)-1] == '\'') {
			return vv[1 : len(vv)-1]
		}
	}
	return vv
}

func mediaRuleActive(prelude string, opts *RenderOptions) bool {
	if strings.TrimSpace(prelude) == "" {
		return true
	}

	queries := strings.Split(prelude, ",")
	for _, raw := range queries {
		query := strings.ToLower(strings.TrimSpace(raw))
		if query == "" {
			continue
		}

		mediaType := ""
		rest := query
		parts := strings.Fields(query)
		if len(parts) > 0 && !strings.HasPrefix(parts[0], "(") {
			mediaType = parts[0]
			rest = strings.TrimSpace(strings.TrimPrefix(query, mediaType))
		}

		switch mediaType {
		case "", "all", "screen", "handheld", "projection":
			// ok
		case "print", "speech", "aural", "braille", "embossed", "tty", "tv":
			continue
		default:
			if evaluateMediaFeatures(rest, opts) {
				return true
			}
		}
	}
	return false
}

func evaluateMediaFeatures(expr string, opts *RenderOptions) bool {
	width := 0
	height := 0
	if opts != nil {
		width = opts.ScreenW
		height = opts.ScreenH
	}
	if width <= 0 {
		width = 240
	}
	if height <= 0 {
		height = 320
	}

	clauses := strings.Split(expr, "and")
	for _, clause := range clauses {
		c := strings.TrimSpace(clause)
		if c == "" {
			continue
		}
		if strings.HasPrefix(c, "(") && strings.HasSuffix(c, ")") {
			c = strings.TrimSpace(c[1 : len(c)-1])
		}
		parts := strings.SplitN(c, ":", 2)
		feature := strings.TrimSpace(parts[0])
		value := ""
		if len(parts) == 2 {
			value = strings.TrimSpace(parts[1])
		}

		switch feature {
		case "orientation":
			orientation := "portrait"
			if width > height {
				orientation = "landscape"
			}
			if value != "" && value != orientation {
				return false
			}
		case "min-width":
			if px, ok := cssLengthToPx(value, width); ok && width < px {
				return false
			}
		case "max-width":
			if px, ok := cssLengthToPx(value, width); ok && width > px {
				return false
			}
		case "min-height":
			if px, ok := cssLengthToPx(value, height); ok && height < px {
				return false
			}
		case "max-height":
			if px, ok := cssLengthToPx(value, height); ok && height > px {
				return false
			}
		case "prefers-color-scheme":
			scheme := "light"
			if opts != nil && opts.NumColors >= 256 {
				scheme = "dark"
			}
			if value != "" && value != scheme {
				return false
			}
		default:
			// Unsupported feature, assume true for simplicity
		}
	}
	return true
}

func cssLengthToPx(val string, base int) (int, bool) {
	v := strings.ToLower(strings.TrimSpace(val))
	if v == "" {
		return 0, false
	}
	if strings.HasSuffix(v, "px") {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v[:len(v)-2]), 64); err == nil {
			return int(f + 0.5), true
		}
		return 0, false
	}
	if strings.HasSuffix(v, "%") {
		if base <= 0 {
			return 0, false
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(v[:len(v)-1]), 64); err == nil {
			return int(float64(base) * f / 100.0), true
		}
		return 0, false
	}
	if strings.HasSuffix(v, "em") || strings.HasSuffix(v, "rem") {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v[:len(v)-2]), 64); err == nil {
			return int(f*16.0 + 0.5), true
		}
		return 0, false
	}
	if strings.HasSuffix(v, "vw") {
		if base <= 0 {
			base = 100
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(v[:len(v)-2]), 64); err == nil {
			return int(float64(base) * f / 100.0), true
		}
		return 0, false
	}
	if strings.HasSuffix(v, "vh") {
		if base <= 0 {
			return 0, false
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(v[:len(v)-2]), 64); err == nil {
			return int(float64(base) * f / 100.0), true
		}
		return 0, false
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return int(f + 0.5), true
	}
	return 0, false
}

func computeStyleFor(n *html.Node, ss *Stylesheet) map[string]string {
	if ss == nil || n == nil || n.Type != html.ElementNode {
		return nil
	}

	props := map[string]propState{}

	for _, rule := range ss.rules {
		if rule.selector == nil || !rule.selector.Match(n) {
			continue
		}
		for _, decl := range rule.declarations {
			applyDeclaration(props, decl, rule.specificity, rule.order)
		}
	}

	if inline := strings.TrimSpace(getAttr(n, "style")); inline != "" {
		if decls, err := parser.ParseDeclarations(inline); err == nil {
			for i, d := range decls {
				if d == nil {
					continue
				}
				decl := cssDeclaration{
					property:  strings.ToLower(strings.TrimSpace(d.Property)),
					value:     d.Value,
					important: d.Important,
				}
				applyDeclaration(props, decl, cascadia.Specificity{1 << 12, 0, 0}, (1<<30)+i)
			}
		} else {
			parts := strings.Split(inline, ";")
			for i, part := range parts {
				kv := strings.SplitN(part, ":", 2)
				if len(kv) != 2 {
					continue
				}
				value := strings.TrimSpace(kv[1])
				important := false
				lower := strings.ToLower(value)
				if strings.HasSuffix(lower, "!important") {
					important = true
					value = strings.TrimSpace(value[:len(value)-10])
				}
				decl := cssDeclaration{
					property:  strings.ToLower(strings.TrimSpace(kv[0])),
					value:     value,
					important: important,
				}
				applyDeclaration(props, decl, cascadia.Specificity{1 << 12, 0, 0}, (1<<30)+i)
			}
		}
	}

	if len(props) == 0 {
		return nil
	}
	out := make(map[string]string, len(props))
	for k, st := range props {
		out[k] = st.val
	}
	return out
}

func applyDeclaration(store map[string]propState, decl cssDeclaration, spec cascadia.Specificity, order int) {
	prop := strings.ToLower(strings.TrimSpace(decl.property))
	if prop == "" {
		return
	}
	value := strings.TrimSpace(decl.value)
	if value == "" {
		return
	}
	if prop == "background" {
		if col := extractColorFromValue(value); col != "" {
			prop = "background-color"
			value = col
		}
	}
	lower := strings.ToLower(value)
	switch prop {
	case "background-color", "fill":
		if hex := cssToHex(lower); hex != "" {
			value = ensureMinForRGB565(normalizeBgForBlackText(hex))
			lower = strings.ToLower(value)
		}
	case "color":
		if hex := cssToHex(lower); hex != "" {
			value = hex
			lower = strings.ToLower(value)
		}
	}
	if shouldSkipWhiteFill(prop, lower) {
		return
	}
	entry := propState{val: value, spec: spec, order: order, important: decl.important}
	if prev, ok := store[prop]; ok {
		if prev.important && !decl.important {
			return
		}
		if decl.important && !prev.important {
			store[prop] = entry
			return
		}
		if prev.spec.Less(spec) {
			store[prop] = entry
			return
		}
		if spec.Less(prev.spec) {
			return
		}
		if order >= prev.order {
			store[prop] = entry
		}
		return
	}
	store[prop] = entry
}

func extractColorFromValue(input string) string {
	s := strings.TrimSpace(strings.ToLower(input))
	if s == "" {
		return ""
	}
	if col := cssToHex(s); col != "" {
		return col
	}
	cleaned := stripFunctions(s, "url")
	for i := 0; i < len(cleaned); i++ {
		if cleaned[i] == '#' {
			j := i + 1
			for j < len(cleaned) && ((cleaned[j] >= '0' && cleaned[j] <= '9') || (cleaned[j] >= 'a' && cleaned[j] <= 'f')) {
				j++
			}
			if col := cssToHex(cleaned[i:j]); col != "" {
				return col
			}
		}
	}
	keywords := []string{"rgba(", "rgb(", "hsla(", "hsl("}
	for _, kw := range keywords {
		idx := strings.Index(cleaned, kw)
		for idx != -1 {
			end := idx + len(kw)
			depth := 1
			for end < len(cleaned) && depth > 0 {
				switch cleaned[end] {
				case '(':
					depth++
				case ')':
					depth--
				}
				end++
			}
			if depth == 0 {
				snippet := cleaned[idx:end]
				if col := cssToHex(snippet); col != "" {
					return col
				}
			}
			next := strings.Index(cleaned[idx+len(kw):], kw)
			if next == -1 {
				break
			}
			idx = idx + len(kw) + next
		}
	}
	parts := strings.FieldsFunc(cleaned, func(r rune) bool {
		return unicode.IsSpace(r) || r == ',' || r == '/' || r == '(' || r == ')'
	})
	for _, part := range parts {
		if col := cssToHex(part); col != "" {
			return col
		}
	}
	return ""
}

func stripFunctions(s string, names ...string) string {
	if len(names) == 0 || s == "" {
		return s
	}
	lower := strings.ToLower(s)
	var b strings.Builder
	i := 0
	for i < len(lower) {
		matched := false
		for _, name := range names {
			keyword := name + "("
			if strings.HasPrefix(lower[i:], keyword) {
				matched = true
				depth := 0
				j := i
				for j < len(lower) {
					switch lower[j] {
					case '(':
						depth++
					case ')':
						depth--
						if depth == 0 {
							j++
							break
						}
					}
					j++
				}
				i = j
				break
			}
		}
		if matched {
			continue
		}
		b.WriteByte(lower[i])
		i++
	}
	return b.String()
}

func resolveAbsURL(base, href string) string {
	bu, err := url.Parse(base)
	if err != nil {
		return ""
	}
	hu, err := url.Parse(href)
	if err != nil {
		return ""
	}
	return bu.ResolveReference(hu).String()
}

func fetchText(absURL string, hdr http.Header, jar http.CookieJar, accept string) ([]byte, bool) {
	req, err := http.NewRequest(http.MethodGet, absURL, nil)
	if err != nil {
		return nil, false
	}
	if accept == "" {
		accept = "text/*"
	}
	req.Header.Set("Accept", accept)
	if hdr != nil {
		for k, vals := range hdr {
			for _, v := range vals {
				if strings.EqualFold(k, "accept") {
					continue
				}
				req.Header.Add(k, v)
			}
		}
	}
	client := &http.Client{Timeout: 8 * time.Second}
	if jar != nil {
		client.Jar = jar
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	rc := io.ReadCloser(resp.Body)
	switch strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))) {
	case "gzip":
		if gr, err := gzip.NewReader(resp.Body); err == nil {
			rc = gr
			defer gr.Close()
		}
	case "deflate":
		if zr, err := zlib.NewReader(resp.Body); err == nil {
			rc = zr
			defer zr.Close()
		} else if fr := flate.NewReader(resp.Body); fr != nil {
			rc = io.NopCloser(fr)
			defer fr.Close()
		}
	}
	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, false
	}
	return body, true
}

func shouldSkipWhiteFill(prop, val string) bool {
	p := strings.ToLower(strings.TrimSpace(prop))
	switch p {
	case "fill", "background-color", "background":
		if isWhiteHex(cssToHex(val)) {
			return true
		}
	}
	return false
}

func findTextColorFor(n *html.Node, st *walkState) string {
	if n == nil {
		return st.curColor
	}
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, "style") {
			parts := strings.Split(a.Val, ";")
			for _, part := range parts {
				kv := strings.SplitN(part, ":", 2)
				if len(kv) != 2 {
					continue
				}
				if strings.TrimSpace(strings.ToLower(kv[0])) != "color" {
					continue
				}
				if hex := cssToHex(kv[1]); hex != "" {
					return hex
				}
			}
		}
		if strings.EqualFold(a.Key, "color") {
			if hex := cssToHex(a.Val); hex != "" {
				return hex
			}
		}
	}
	return st.curColor
}
