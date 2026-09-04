package operamini4

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Scene is the protocol-neutral, geometry-preserving representation used by
// the Operetta render comparison tools.
type Scene struct {
	Schema    string          `json:"schema"`
	Protocol  string          `json:"protocol"`
	Viewport  SceneViewport   `json:"viewport"`
	Document  SceneDocument   `json:"document"`
	Fragments []SceneFragment `json:"fragments"`
}

// SceneViewport describes the logical Opera Mini viewport.
type SceneViewport struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// SceneDocument contains page-level scene metadata.
type SceneDocument struct {
	Height     int    `json:"height"`
	Background string `json:"background"`
	URL        string `json:"url,omitempty"`
	Title      string `json:"title,omitempty"`
}

// SceneLink identifies a link hit region and its normalized target.
type SceneLink struct {
	Index  int    `json:"index"`
	Target string `json:"target"`
}

// SceneImage identifies an embedded image without copying its payload into
// scene JSON.
type SceneImage struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Digest string `json:"digest,omitempty"`
	MIME   string `json:"mime,omitempty"`
}

// SceneControl describes an OM4 form control drawing.
type SceneControl struct {
	Tag   int `json:"tag"`
	Flags int `json:"flags,omitempty"`
}

// SceneFragment is one ordered drawing or interaction primitive.
type SceneFragment struct {
	Index      int           `json:"index"`
	Kind       string        `json:"kind"`
	X          int           `json:"x"`
	Y          int           `json:"y"`
	Width      int           `json:"width"`
	Height     int           `json:"height"`
	Color      string        `json:"color,omitempty"`
	Background string        `json:"background,omitempty"`
	Style      int           `json:"style,omitempty"`
	Text       string        `json:"text,omitempty"`
	Link       *SceneLink    `json:"link,omitempty"`
	Image      *SceneImage   `json:"image,omitempty"`
	Control    *SceneControl `json:"control,omitempty"`
}

// BuildScene converts a decoded OM4 application document to sky.scene.v1.
// Drawing order and absolute geometry are preserved; link hit regions follow
// the drawings in their source order.
func BuildScene(document *ApplicationDocument) Scene {
	background := "#ffffff"
	backgroundArea := -1
	fragments := make([]SceneFragment, 0, len(document.Drawings)+len(document.Links))
	for _, drawing := range document.Drawings {
		fragment := SceneFragment{
			Index: len(fragments), X: drawing.X, Y: drawing.Y,
			Width: drawing.Width, Height: drawing.Height,
		}
		switch drawing.Kind {
		case 'B':
			fragment.Kind = "background"
			fragment.Color = sceneColor(drawing.Color)
			area := drawing.Width * drawing.Height
			if drawing.X == 0 && drawing.Y == 0 && drawing.Width >= document.Header.ViewportWidth && area > backgroundArea {
				background = fragment.Color
				backgroundArea = area
			}
		case 'T':
			fragment.Kind = "text"
			fragment.Color = sceneColor(drawing.Color)
			fragment.Style = int(drawing.Font)
			fragment.Text = drawing.Text
		case 'I':
			fragment.Kind = "image"
			fragment.Color = sceneColor(drawing.Color)
			fragment.Image = sceneImageAt(document.Page, drawing.ImagePointer, drawing.Width, drawing.Height)
		case 'F':
			fragment.Kind = "control"
			fragment.Color = sceneColor(drawing.Color)
			fragment.Control = &SceneControl{Tag: int(drawing.ControlKind), Flags: int(drawing.ControlFlags)}
		default:
			continue
		}
		fragments = append(fragments, fragment)
	}
	for index, link := range document.Links {
		fragments = append(fragments, SceneFragment{
			Index: len(fragments), Kind: "link", X: link.X, Y: link.Y,
			Width: link.Width, Height: link.Height,
			Link: &SceneLink{Index: index, Target: normalizeSceneTarget(link.URL, document.Header.Base, document.Header.URL)},
		})
	}
	return Scene{
		Schema: "sky.scene.v1", Protocol: "om4",
		Viewport: SceneViewport{Width: document.Header.ViewportWidth, Height: 320},
		Document: SceneDocument{
			Height: document.Header.DocumentHeight, Background: background,
			URL: normalizeSceneTarget(document.Header.URL, document.Header.Base), Title: document.Header.Title,
		},
		Fragments: fragments,
	}
}

// MarshalScene encodes a decoded OM4 document as stable, indented
// sky.scene.v1 JSON terminated by a newline.
func MarshalScene(document *ApplicationDocument) ([]byte, error) {
	data, err := json.MarshalIndent(BuildScene(document), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func sceneColor(value uint32) string {
	return fmt.Sprintf("#%06x", value&0xffffff)
}

func sceneImageAt(page []byte, pointer, width, height int) *SceneImage {
	image := &SceneImage{Width: width, Height: height}
	if pointer < 0 || pointer+2 > len(page) {
		return image
	}
	length := int(binary.BigEndian.Uint16(page[pointer : pointer+2]))
	if length <= 0 || pointer+2+length > len(page) {
		return image
	}
	data := page[pointer+2 : pointer+2+length]
	digest := sha256.Sum256(data)
	image.Digest = fmt.Sprintf("sha256:%x", digest)
	switch {
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		image.MIME = "image/jpeg"
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		image.MIME = "image/png"
	case len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		image.MIME = "image/gif"
	}
	return image
}

func normalizeSceneTarget(value string, bases ...string) string {
	value = strings.TrimLeft(value, "\x00")
	value = stripSceneNavigationPrefix(value)
	target, err := url.Parse(value)
	if err != nil {
		return value
	}
	if target.IsAbs() {
		return target.String()
	}
	for _, rawBase := range bases {
		rawBase = strings.TrimLeft(rawBase, "\x00")
		rawBase = stripSceneNavigationPrefix(rawBase)
		base, baseErr := url.Parse(rawBase)
		if baseErr == nil && base.IsAbs() {
			return base.ResolveReference(target).String()
		}
	}
	return target.String()
}

func stripSceneNavigationPrefix(value string) string {
	if len(value) > 2 && (strings.HasPrefix(value, "0/") || strings.HasPrefix(value, "1/")) {
		return value[2:]
	}
	return value
}
