package oms

import (
	"bytes"
	"encoding/binary"
	"net/http"
	"strings"
	"testing"

	"operetta/presentation"
)

func TestRenderDocumentRejectsRawOMSPassthroughByDefault(t *testing.T) {
	t.Setenv("OMS_ALLOW_RAW_PASSTHROUGH", "")
	untrusted := NewPage()
	untrusted.AddString("1/http://attacker.test/")
	untrusted.AddText("crafted payload")
	untrusted.Finalize()
	source := &UpstreamDocument{
		URL:     "http://attacker.test/payload",
		Body:    append([]byte(nil), untrusted.Data...),
		RawBody: append([]byte(nil), untrusted.Data...),
	}
	page, err := RenderDocument(source, http.Header{}, &RenderOptions{
		ClientVersion: ClientVersion3,
		Compression:   CompressionNone,
	})
	if err != nil {
		t.Fatalf("RenderDocument: %v", err)
	}
	if bytes.Equal(page.Data, untrusted.Data) {
		t.Fatal("untrusted OMS payload was passed through unchanged")
	}
	word := binary.LittleEndian.Uint16(page.Data[:2])
	if got := byte(word); got != versionHeaderByte(ClientVersion3) {
		t.Fatalf("error response version=0x%02x, want OM3", got)
	}
}

func TestRenderDocumentOM3StyleWidthAndPrimaryColors(t *testing.T) {
	tests := []struct {
		name string
		hex  string
		rgb  uint32
	}{
		{name: "red", hex: "#ff0000", rgb: 0xff0000},
		{name: "blue", hex: "#0000ff", rgb: 0x0000ff},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := &presentation.Document{
				URL: "http://fixture.test/",
				Operations: []presentation.Operation{
					{Kind: presentation.Style, Style: presentation.TextStyle{Foreground: tt.hex}},
					{Kind: presentation.Text, Text: "needle"},
				},
			}
			page, err := EncodeDocument(model, &RenderOptions{
				ClientVersion: ClientVersion3,
				Compression:   CompressionDeflate,
			})
			if err != nil {
				t.Fatalf("EncodeDocument: %v", err)
			}
			parsed, err := parsePage(page)
			if err != nil {
				t.Fatalf("parsePage: %v", err)
			}
			var color uint32
			var needleColor uint32
			found := false
			for _, token := range parsed.tokens {
				switch token.tag {
				case 'S':
					if len(token.data) != 6 {
						t.Fatalf("OM3 style payload length=%d, want 6", len(token.data))
					}
					color = binary.BigEndian.Uint32(token.data[1:5])
				case 'T':
					if len(token.strings) > 0 && token.strings[0] == "needle" {
						found = true
						needleColor = color
					}
				}
			}
			if !found {
				t.Fatal("needle text was not encoded")
			}
			if needleColor != tt.rgb {
				t.Fatalf("OM3 color=0x%06x, want 0x%06x", needleColor, tt.rgb)
			}
		})
	}
}

func TestTransformAndEncodeRemainSeparate(t *testing.T) {
	body := []byte(`<html><body><a href="/next">Next</a></body></html>`)
	source := &UpstreamDocument{
		URL:           "http://fixture.test/start",
		Body:          body,
		RawBody:       body,
		Header:        http.Header{"Content-Type": {"text/html; charset=utf-8"}},
		TransferBytes: len(body),
	}
	model, err := TransformDocument(source, http.Header{}, &RenderOptions{ImagesOn: true})
	if err != nil {
		t.Fatalf("TransformDocument: %v", err)
	}
	if len(model.Operations) == 0 {
		t.Fatal("transform returned no presentation operations")
	}

	for _, version := range []ClientVersion{ClientVersion1, ClientVersion2, ClientVersion3} {
		page, err := EncodeDocument(model, &RenderOptions{
			ClientVersion: version,
			Compression:   CompressionNone,
		})
		if err != nil {
			t.Fatalf("EncodeDocument(version=%d): %v", version, err)
		}
		word := binary.LittleEndian.Uint16(page.Data[:2])
		if got, want := byte(word), versionHeaderByte(version); got != want {
			t.Fatalf("version=%d header=0x%02x, want 0x%02x", version, got, want)
		}
	}
}

func TestTransformPreservesLegacyBodyLinkColor(t *testing.T) {
	body := []byte(`<html><body text="#404040" link="#006600"><a href="/next"><b>Next</b></a></body></html>`)
	model, err := TransformDocument(&UpstreamDocument{
		URL: "https://fixture.test/", Body: body, RawBody: body,
		Header: http.Header{"Content-Type": {"text/html; charset=utf-8"}},
	}, http.Header{}, &RenderOptions{})
	if err != nil {
		t.Fatalf("TransformDocument: %v", err)
	}
	wantColor := false
	wantBold := false
	for _, operation := range model.Operations {
		if operation.Kind != presentation.Style {
			continue
		}
		wantColor = wantColor || operation.Style.Foreground == "#006600"
		wantBold = wantBold || (operation.Style.Foreground == "#006600" && operation.Style.Bold)
	}
	if !wantColor || !wantBold {
		t.Fatalf("legacy link style not preserved: color=%v bold=%v operations=%+v", wantColor, wantBold, model.Operations)
	}
}

func TestTransformBlockBackgroundEndsOnLineBoundaryBeforeRestore(t *testing.T) {
	body := []byte(`<html><head><style>.top{background-color:#ffff00}.next{background-color:#ffffff}</style></head><body><div class="top">Top block</div><div class="next">Next block</div></body></html>`)
	model, err := TransformDocument(&UpstreamDocument{
		URL: "https://fixture.test/", Body: body, RawBody: body,
		Header: http.Header{"Content-Type": {"text/html; charset=utf-8"}},
	}, http.Header{}, &RenderOptions{})
	if err != nil {
		t.Fatalf("TransformDocument: %v", err)
	}

	top := -1
	haveYellow := false
	for i, op := range model.Operations {
		if op.Kind == presentation.Background && strings.HasPrefix(strings.ToLower(op.Color), "#ffff") {
			haveYellow = true
		}
		if op.Kind == presentation.Text && strings.Contains(op.Text, "Top block") {
			top = i
			break
		}
	}
	if top < 0 || !haveYellow {
		t.Fatalf("missing coloured top block: %+v", model.Operations)
	}

	haveBreak := false
	for i := top + 1; i < len(model.Operations); i++ {
		op := model.Operations[i]
		if op.Kind == presentation.Break {
			haveBreak = true
			continue
		}
		if op.Kind == presentation.Background {
			if !haveBreak {
				t.Fatalf("background restored before line boundary after top block: %+v", model.Operations)
			}
			return
		}
	}
	t.Fatal("expected parent/next background restore after top block")
}

func TestTransformUsesOM4CompatibleDefaultLinkColor(t *testing.T) {
	body := []byte(`<html><body text="#404040"><a href="/next">Next</a></body></html>`)
	model, err := TransformDocument(&UpstreamDocument{
		URL: "https://fixture.test/", Body: body, RawBody: body,
		Header: http.Header{"Content-Type": {"text/html; charset=utf-8"}},
	}, http.Header{}, &RenderOptions{})
	if err != nil {
		t.Fatalf("TransformDocument: %v", err)
	}
	for _, operation := range model.Operations {
		if operation.Kind == presentation.Style && operation.Style.Foreground == "#0000ad" {
			return
		}
	}
	t.Fatalf("default link color not preserved: %+v", model.Operations)
}

func TestTransformDocumentPreservesModernFormSemantics(t *testing.T) {
	body := []byte(`<html><body>
		<a href="/account" aria-label="Account"></a>
		<form><input type="search" name="q" placeholder="Search the web">
		<input type="password" name="password" aria-label="Password">
		<label>Phone<input type="tel" name="phone"></label>
		<textarea name="message" placeholder="Message">draft</textarea></form>
		<noscript>JavaScript-free fallback</noscript><script>secret()</script>
	</body></html>`)
	source := &UpstreamDocument{
		URL:           "https://fixture.test/start",
		Body:          body,
		RawBody:       body,
		Header:        http.Header{"Content-Type": {"text/html; charset=utf-8"}},
		TransferBytes: len(body),
	}
	model, err := TransformDocument(source, http.Header{}, &RenderOptions{ImagesOn: true})
	if err != nil {
		t.Fatalf("TransformDocument: %v", err)
	}

	var texts []string
	inputs := map[string]presentation.Operation{}
	for _, op := range model.Operations {
		switch op.Kind {
		case presentation.Text:
			texts = append(texts, op.Text)
		case presentation.TextInput, presentation.PasswordInput:
			inputs[op.Name] = op
		}
	}
	joined := strings.Join(texts, " ")
	for _, want := range []string{"Account", "Search the web", "Password", "Phone", "Message", "JavaScript-free fallback"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("presentation text %q does not contain %q", joined, want)
		}
	}
	if strings.Contains(joined, "secret") {
		t.Fatalf("script content leaked into presentation text: %q", joined)
	}
	if op, ok := inputs["q"]; !ok || op.Kind != presentation.TextInput {
		t.Fatalf("search input missing or has wrong kind: %+v", op)
	}
	if op, ok := inputs["password"]; !ok || op.Kind != presentation.PasswordInput {
		t.Fatalf("password input missing or has wrong kind: %+v", op)
	}
	if op, ok := inputs["phone"]; !ok || op.Kind != presentation.TextInput {
		t.Fatalf("wrapped tel input missing or has wrong kind: %+v", op)
	}
	if op, ok := inputs["message"]; !ok || op.Value != "draft" {
		t.Fatalf("textarea value was not preserved: %+v", op)
	}
}
