package operamini

import (
	"encoding/binary"
	"testing"

	"operetta/gateway"
	"operetta/presentation"
)

func TestNegotiateVersionFamilies(t *testing.T) {
	tests := []struct {
		raw        string
		header     byte
		styleBytes int
		support    SupportLevel
	}{
		{raw: "1.0", header: 0x0d, styleBytes: 4, support: Experimental},
		{raw: "Opera Mini/2.06", header: 0x18, styleBytes: 4, support: Verified},
		{raw: "3.2", header: 0x1a, styleBytes: 6, support: Experimental},
	}
	for _, tt := range tests {
		dialect, err := Negotiate(tt.raw)
		if err != nil {
			t.Fatalf("Negotiate(%q): %v", tt.raw, err)
		}
		if dialect.HeaderByte != tt.header || dialect.StylePayloadSize != tt.styleBytes || dialect.Support != tt.support {
			t.Fatalf("Negotiate(%q)=%+v", tt.raw, dialect)
		}
	}
	if _, err := Negotiate("3.3"); err == nil {
		t.Fatal("expected 3.3 to be rejected")
	}
}

func TestEncoderUsesNegotiatedDialect(t *testing.T) {
	doc := &presentation.Document{
		URL: "http://fixture.test/",
		Operations: []presentation.Operation{
			{Kind: presentation.Text, Text: "hello"},
		},
	}
	encoder := Encoder{}
	for _, tt := range []struct {
		version string
		header  byte
	}{
		{version: "1.0", header: 0x0d},
		{version: "2.06", header: 0x18},
		{version: "3.2", header: 0x1a},
	} {
		artifact, err := encoder.Encode(t.Context(), gateway.TransformResult{Document: doc}, gateway.EncodeRequest{
			Dialect:     tt.version,
			Compression: "none",
		})
		if err != nil {
			t.Fatalf("Encode(%s): %v", tt.version, err)
		}
		if len(artifact.Data) < 2 {
			t.Fatalf("Encode(%s) returned short artifact", tt.version)
		}
		if got := byte(binary.LittleEndian.Uint16(artifact.Data[:2])); got != tt.header {
			t.Fatalf("Encode(%s) header=0x%02x, want 0x%02x", tt.version, got, tt.header)
		}
	}
}
