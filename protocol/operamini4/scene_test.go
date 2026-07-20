package operamini4

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildScenePreservesGeometryLinksAndImageDigest(t *testing.T) {
	page := append([]byte{0, 4}, []byte{0xff, 0xd8, 0xff, 0xd9}...)
	document := &ApplicationDocument{
		Page: page,
		Header: PageHeader{
			ViewportWidth: 231, DocumentHeight: 400,
			URL: "1/http://example.test/current/index.html", Base: "http://example.test/root/", Title: "Fixture",
		},
		Drawings: []DrawingElement{
			{Kind: 'B', X: 0, Y: 0, Width: 231, Height: 400, Color: 0xff102030},
			{Kind: 'T', X: 5, Y: 7, Width: 40, Height: 14, Color: 0xffabcdef, Font: 2, Text: "Hello"},
			{Kind: 'I', X: 3, Y: 25, Width: 8, Height: 6, ImagePointer: 0},
			{Kind: 'F', X: 4, Y: 34, Width: 80, Height: 16, Color: 0xff010203, ControlKind: 7, ControlFlags: 3},
		},
		Links: []LinkElement{
			{X: 5, Y: 7, Width: 40, Height: 14, URL: "\x00registration/?from=scene&next=%2Fkeep"},
		},
	}
	scene := BuildScene(document)
	if scene.Schema != "sky.scene.v1" || scene.Protocol != "om4" || scene.Document.Background != "#102030" {
		t.Fatalf("scene header: %+v", scene)
	}
	if scene.Document.URL != "http://example.test/current/index.html" || len(scene.Fragments) != 5 {
		t.Fatalf("scene URL/fragments: %+v", scene)
	}
	text := scene.Fragments[1]
	if text.Kind != "text" || text.X != 5 || text.Y != 7 || text.Style != 2 || text.Text != "Hello" || text.Color != "#abcdef" {
		t.Fatalf("text fragment: %+v", text)
	}
	image := scene.Fragments[2]
	if image.Image == nil || image.Image.MIME != "image/jpeg" || !strings.HasPrefix(image.Image.Digest, "sha256:") {
		t.Fatalf("image fragment: %+v", image)
	}
	control := scene.Fragments[3]
	if control.Control == nil || control.Control.Tag != 7 || control.Control.Flags != 3 {
		t.Fatalf("control fragment: %+v", control)
	}
	link := scene.Fragments[4]
	if link.Link == nil || link.Link.Target != "http://example.test/root/registration/?from=scene&next=%2Fkeep" {
		t.Fatalf("link fragment: %+v", link)
	}
}

func TestBuildSceneResolvesRelativeLinkAgainstDocumentURLFallback(t *testing.T) {
	document := &ApplicationDocument{
		Header: PageHeader{URL: "1/http://example.test/path/page.html"},
		Links:  []LinkElement{{URL: "0/../next?q=one%20two&keep=1"}},
	}
	scene := BuildScene(document)
	if got := scene.Fragments[0].Link.Target; got != "http://example.test/next?q=one%20two&keep=1" {
		t.Fatalf("resolved target = %q", got)
	}
}

func TestMarshalSceneProducesJSONWithTrailingNewline(t *testing.T) {
	data, err := MarshalScene(&ApplicationDocument{Header: PageHeader{ViewportWidth: 240}})
	if err != nil {
		t.Fatalf("MarshalScene: %v", err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Fatalf("scene JSON has no trailing newline: %q", data)
	}
	var scene Scene
	if err := json.Unmarshal(data, &scene); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if scene.Schema != "sky.scene.v1" || scene.Viewport.Width != 240 {
		t.Fatalf("scene: %+v", scene)
	}
}
