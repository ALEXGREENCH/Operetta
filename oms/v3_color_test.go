package oms

import (
	"reflect"
	"testing"
)

func TestVersionedColorPayloadWidths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		version  ClientVersion
		colorLen int
		styleLen int
	}{
		{name: "v1", version: ClientVersion1, colorLen: 2, styleLen: 4},
		{name: "v2", version: ClientVersion2, colorLen: 2, styleLen: 4},
		{name: "v3", version: ClientVersion3, colorLen: 4, styleLen: 6},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			page := NewPage()
			page.SetTransport(tt.version, CompressionNone)
			page.AddString("1/http://fixture.test/")
			page.AddBgcolor("#123456")
			page.AddHr("#abcdef")
			page.AddStyle(styleBoldBit | uint32(calcColor("#654321"))<<8)
			page.AddText("ok")
			page.Finalize()

			parsed, err := parsePage(page)
			if err != nil {
				t.Fatalf("parsePage(): %v", err)
			}
			var tags []byte
			for _, token := range parsed.tokens {
				tags = append(tags, token.tag)
				switch token.tag {
				case 'D', 'R':
					if got := len(token.data); got != tt.colorLen {
						t.Fatalf("tag %q payload=%d bytes, want %d", token.tag, got, tt.colorLen)
					}
				case 'S':
					if got := len(token.data); got != tt.styleLen {
						t.Fatalf("tag S payload=%d bytes, want %d", got, tt.styleLen)
					}
				}
			}
			if want := []byte{'D', 'R', 'S', 'T', 'Q'}; !reflect.DeepEqual(tags, want) {
				t.Fatalf("tags=%q, want %q", tags, want)
			}
		})
	}
}

func TestOM3PaginationNavigationUsesVersionedColorWidths(t *testing.T) {
	base := NewPage()
	base.SetTransport(ClientVersion3, CompressionNone)
	base.AddString("1/http://fixture.test/long")
	base.AddBgcolor("#ffffff")
	base.AddStyle(styleBoldBit | uint32(calcColor("#654321"))<<8)
	for i := 0; i < 8; i++ {
		base.AddText("page content")
		base.AddBreak()
	}
	base.Finalize()

	opts := &RenderOptions{
		ClientVersion:  ClientVersion3,
		Compression:    CompressionNone,
		GatewayVersion: 285,
	}
	const maxTags = 3
	_, _, total, err := SelectOMSPartFromPackedWithNav(
		base.Data,
		1,
		maxTags,
		"http://proxy.test",
		"http://fixture.test/long",
		opts,
	)
	if err != nil {
		t.Fatalf("SelectOMSPartFromPackedWithNav(): %v", err)
	}
	if total < 2 {
		t.Fatalf("total parts=%d, want at least 2", total)
	}

	for part := 1; part <= total; part++ {
		packed, current, gotTotal, err := SelectOMSPartFromPackedWithNav(
			base.Data,
			part,
			maxTags,
			"http://proxy.test",
			"http://fixture.test/long",
			opts,
		)
		if err != nil {
			t.Fatalf("part %d: SelectOMSPartFromPackedWithNav(): %v", part, err)
		}
		if current != part || gotTotal != total {
			t.Fatalf("part metadata=(%d/%d), want (%d/%d)", current, gotTotal, part, total)
		}

		parsed, err := parsePage(&Page{Data: packed})
		if err != nil {
			t.Fatalf("part %d: parsePage(): %v", part, err)
		}
		if parsed.version != ClientVersion3 {
			t.Fatalf("part %d: version=%d, want %d", part, parsed.version, ClientVersion3)
		}
		if len(parsed.tokens) == 0 || parsed.tokens[len(parsed.tokens)-1].tag != 'Q' {
			t.Fatalf("part %d: missing terminal Q", part)
		}

		sawNavigationRule := false
		for _, token := range parsed.tokens {
			switch token.tag {
			case 'D', 'R':
				if got := len(token.data); got != 4 {
					t.Fatalf("part %d: tag %q payload=%d bytes, want 4", part, token.tag, got)
				}
				if token.tag == 'R' {
					sawNavigationRule = true
				}
			case 'S':
				if got := len(token.data); got != 6 {
					t.Fatalf("part %d: tag S payload=%d bytes, want 6", part, got)
				}
			}
		}
		if !sawNavigationRule {
			t.Fatalf("part %d: navigation did not emit an R tag", part)
		}
	}
}
