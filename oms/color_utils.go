package oms

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

const defaultTextColorHex = "#000000"

type rgbColor struct {
	R uint8
	G uint8
	B uint8
}

func (c rgbColor) hex() string {
	return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B)
}

func (c rgbColor) brightness() int {
	return int(0.299*float64(c.R) + 0.587*float64(c.G) + 0.114*float64(c.B))
}

func (c rgbColor) relativeLuminance() float64 {
	toLinear := func(channel uint8) float64 {
		v := float64(channel) / 255.0
		if v <= 0.03928 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return 0.2126*toLinear(c.R) + 0.7152*toLinear(c.G) + 0.0722*toLinear(c.B)
}

func (c rgbColor) contrastRatio(other rgbColor) float64 {
	la := c.relativeLuminance()
	lb := other.relativeLuminance()
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func (c rgbColor) lighten(percent int) rgbColor {
	if percent <= 0 {
		return c
	}
	if percent >= 100 {
		return rgbColor{R: 255, G: 255, B: 255}
	}
	scale := func(channel uint8) uint8 {
		delta := (255 - int(channel)) * percent / 100
		value := int(channel) + delta
		if value > 255 {
			value = 255
		}
		return uint8(value)
	}
	return rgbColor{
		R: scale(c.R),
		G: scale(c.G),
		B: scale(c.B),
	}
}

func (c rgbColor) ensureRGB565Floor() rgbColor {
	const (
		minR = 16
		minG = 16
		minB = 16
	)
	if c.R < minR {
		c.R = minR
	}
	if c.G < minG {
		c.G = minG
	}
	if c.B < minB {
		c.B = minB
	}
	return c
}

func parseHexColor(value string) (rgbColor, bool) {
	hex := strings.TrimPrefix(strings.TrimSpace(value), "#")
	if len(hex) != 6 {
		return rgbColor{}, false
	}
	r, errR := strconv.ParseInt(hex[0:2], 16, 64)
	g, errG := strconv.ParseInt(hex[2:4], 16, 64)
	b, errB := strconv.ParseInt(hex[4:6], 16, 64)
	if errR != nil {
		r = 0
	}
	if errG != nil {
		g = 0
	}
	if errB != nil {
		b = 0
	}
	return rgbColor{uint8(r), uint8(g), uint8(b)}, true
}

func parseShorthandHex(value string) (rgbColor, bool) {
	hex := strings.TrimPrefix(strings.TrimSpace(value), "#")
	switch length := len(hex); {
	case length == 3:
		exp := []byte{
			hex[0], hex[0],
			hex[1], hex[1],
			hex[2], hex[2],
		}
		return parseHexColor(string(exp))
	case length >= 6:
		return parseHexColor(hex[:6])
	default:
		return rgbColor{}, false
	}
}

func parseCSSColor(input string) (rgbColor, bool) {
	if input == "" {
		return rgbColor{}, false
	}
	s := strings.TrimSpace(strings.ToLower(input))
	switch s {
	case "":
		return rgbColor{}, false
	case "black":
		return rgbColor{}, true
	case "white":
		return rgbColor{R: 255, G: 255, B: 255}, true
	case "transparent":
		return rgbColor{}, false
	}
	if strings.HasPrefix(s, "#") {
		if col, ok := parseShorthandHex(s); ok {
			return col, true
		}
		return rgbColor{}, false
	}
	if strings.HasPrefix(s, "rgb(") || strings.HasPrefix(s, "rgba(") {
		col, ok := parseRGBFunctional(s)
		if ok {
			return col, true
		}
	}
	return rgbColor{}, false
}

func parseRGBFunctional(expr string) (rgbColor, bool) {
	open := strings.IndexByte(expr, '(')
	close := strings.LastIndexByte(expr, ')')
	if open < 0 || close <= open+1 {
		return rgbColor{}, false
	}
	parts := strings.Split(expr[open+1:close], ",")
	if len(parts) < 3 {
		return rgbColor{}, false
	}
	toByte := func(component string) uint8 {
		component = strings.TrimSpace(component)
		if component == "" {
			return 0
		}
		if strings.HasSuffix(component, "%") {
			component = strings.TrimSuffix(component, "%")
			if component == "" {
				return 0
			}
			value, err := strconv.Atoi(component)
			if err != nil {
				return 0
			}
			if value < 0 {
				value = 0
			} else if value > 100 {
				value = 100
			}
			return uint8(float64(value) * 255.0 / 100.0)
		}
		value, err := strconv.Atoi(component)
		if err != nil {
			return 0
		}
		if value < 0 {
			value = 0
		} else if value > 255 {
			value = 255
		}
		return uint8(value)
	}
	return rgbColor{
		R: toByte(parts[0]),
		G: toByte(parts[1]),
		B: toByte(parts[2]),
	}, true
}

func isWhiteHex(hex string) bool {
	return strings.EqualFold(strings.TrimSpace(hex), "#ffffff")
}

func hexBrightness(hex string) int {
	col, ok := parseHexColor(hex)
	if !ok {
		return 255
	}
	return col.brightness()
}

func isDarkHex(hex string) bool {
	return hexBrightness(hex) < 60
}

func relLuma(hex string) float64 {
	col, ok := parseHexColor(hex)
	if !ok {
		return 1.0
	}
	return col.relativeLuminance()
}

func contrastRatio(a, b string) float64 {
	la := relLuma(a)
	lb := relLuma(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func lightenHex(hex string, percent int) string {
	col, ok := parseHexColor(hex)
	if !ok {
		return "#" + strings.TrimPrefix(strings.TrimSpace(hex), "#")
	}
	return col.lighten(percent).hex()
}

func ensureMinForRGB565(hex string) string {
	col, ok := parseHexColor(hex)
	if !ok {
		return "#" + strings.TrimPrefix(strings.TrimSpace(hex), "#")
	}
	return col.ensureRGB565Floor().hex()
}

func normalizeBgForBlackText(bg string) string {
	bgHex := cssToHex(bg)
	if bgHex == "" {
		return ""
	}
	col, ok := parseHexColor(bgHex)
	if !ok {
		return ""
	}
	const targetCR = 4.5
	const lightenStep = 12
	const attempts = 8

	if col.contrastRatio(rgbColor{}) >= targetCR {
		return bgHex
	}
	for i := 0; i < attempts; i++ {
		col = col.lighten(lightenStep)
		if col.contrastRatio(rgbColor{}) >= targetCR {
			break
		}
	}
	return col.ensureRGB565Floor().hex()
}

func cssToHex(v string) string {
	normalized := strings.TrimSpace(v)
	if normalized == "" {
		return ""
	}
	if strings.HasPrefix(normalized, "#") {
		if col, ok := parseShorthandHex(normalized); ok {
			return col.hex()
		}
		return ""
	}
	switch strings.ToLower(normalized) {
	case "black":
		return "#000000"
	case "white":
		return "#ffffff"
	case "transparent":
		return ""
	}
	if col, ok := parseCSSColor(normalized); ok {
		return col.hex()
	}
	return ""
}
