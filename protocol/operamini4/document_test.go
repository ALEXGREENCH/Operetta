package operamini4

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"testing"
)

func TestDecodeApplicationDocument(t *testing.T) {
	page := make([]byte, 0)
	page = append(page, 1, 0, 240, 0, 1, 64, 0, 0, 0, 0, 0)
	page = appendUTF(page, "Example")
	page = append(page, 0, 0)
	page = appendUTF(page, "https://example.test/")
	page = appendUTF(page, "\x00child")
	page = append(page, 0)
	page = append(page, 'T', 0, 1, 0, 0, 2, 0, 20, 0, 0, 12)
	page = append(page, 0, 0, 0, 1, 0)
	page = appendUTF(page, "Hello")

	inflated := []byte{0, 0, 4, 'C', 0, 1, 7}
	inflated = appendUint24(inflated, len(page))
	inflated = append(inflated, page...)
	var compressed bytes.Buffer
	writer, err := flate.NewWriter(&compressed, flate.DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(inflated); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	frames := []Frame{
		{Type: 10, Channel: 1, Payload: []byte{0, 0, 42}},
		{Type: 11, Channel: 1, Payload: compressed.Bytes()},
	}
	document, err := DecodeApplicationDocument(frames)
	if err != nil {
		t.Fatal(err)
	}
	if document.ID != 42 || document.Header.Title != "Example" || document.Header.URL != "https://example.test/child" {
		t.Fatalf("unexpected document: id=%d header=%+v", document.ID, document.Header)
	}
	if len(document.Records) != 1 || document.Records[0].Type != 'C' {
		t.Fatalf("records=%+v", document.Records)
	}
	if len(document.Texts) != 1 || document.Texts[0].Text != "Hello" {
		t.Fatalf("texts=%+v", document.Texts)
	}
}

func appendUTF(dst []byte, value string) []byte {
	encoded := []byte(value)
	length := make([]byte, 2)
	binary.BigEndian.PutUint16(length, uint16(len(encoded)))
	dst = append(dst, length...)
	return append(dst, encoded...)
}

func appendUint24(dst []byte, value int) []byte {
	return append(dst, byte(value>>16), byte(value>>8), byte(value))
}
