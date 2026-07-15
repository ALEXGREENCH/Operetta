package gateway_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"strings"
	"testing"

	"operetta/gateway"
	"operetta/origin"
	"operetta/protocol"
	"operetta/protocol/operamini"
	"operetta/protocol/plaintext"
	"operetta/transform/htmlmini"
)

type fixtureFetcher struct {
	response *origin.Response
}

func (f fixtureFetcher) Fetch(context.Context, gateway.FetchRequest) (*origin.Response, error) {
	copyResponse := *f.response
	copyResponse.Body = append([]byte(nil), f.response.Body...)
	copyResponse.RawBody = append([]byte(nil), f.response.RawBody...)
	return &copyResponse, nil
}

func TestServiceSwitchesEncoderWithoutChangingTransformer(t *testing.T) {
	body := []byte(`<html><body><h1>Tiny web</h1><a href="/next">Next</a></body></html>`)
	registry := protocol.MustRegistry(
		operamini.Encoder{DefaultVersion: "2.06"},
		plaintext.Encoder{},
	)
	service := gateway.Service{
		Fetcher: fixtureFetcher{response: &origin.Response{
			URL:           "http://fixture.test/start",
			Body:          body,
			RawBody:       body,
			TransferBytes: len(body),
		}},
		Transformer: htmlmini.Transformer{},
		Encoders:    registry,
	}
	base := gateway.Request{
		Fetch: gateway.FetchRequest{URL: "http://fixture.test/start"},
		Encode: gateway.EncodeRequest{Profile: gateway.ClientProfile{
			ViewportWidth: 240,
			Images:        false,
		}},
	}

	textRequest := base
	textRequest.Format = plaintext.Format
	textArtifact, err := service.Convert(context.Background(), textRequest)
	if err != nil {
		t.Fatalf("plain text conversion: %v", err)
	}
	if got := string(textArtifact.Data); !strings.Contains(got, "Tiny web") || !strings.Contains(got, "http://fixture.test/next") {
		t.Fatalf("unexpected plain text artifact %q", got)
	}

	omsRequest := base
	omsRequest.Format = operamini.Format
	omsRequest.Encode.Dialect = "3.2"
	omsRequest.Encode.Compression = "none"
	omsArtifact, err := service.Convert(context.Background(), omsRequest)
	if err != nil {
		t.Fatalf("Opera Mini conversion: %v", err)
	}
	if len(omsArtifact.Data) < 6 || bytes.Equal(omsArtifact.Data, textArtifact.Data) {
		t.Fatalf("unexpected OMS artifact length=%d", len(omsArtifact.Data))
	}
	if got := byte(binary.LittleEndian.Uint16(omsArtifact.Data[:2])); got != 0x1a {
		t.Fatalf("OM3 header byte=0x%02x, want 0x1a", got)
	}
}

func TestRegistryRejectsDuplicateFormats(t *testing.T) {
	if _, err := protocol.NewRegistry(plaintext.Encoder{}, plaintext.Encoder{}); err == nil {
		t.Fatal("expected duplicate format error")
	}
}
