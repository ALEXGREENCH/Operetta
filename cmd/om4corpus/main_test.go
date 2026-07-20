package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"operetta/protocol/operamini4"
)

func TestOutputSceneCanWriteFileAndStdout(t *testing.T) {
	document := &operamini4.ApplicationDocument{
		Header: operamini4.PageHeader{ViewportWidth: 231, DocumentHeight: 400},
	}
	path := filepath.Join(t.TempDir(), "fixture.scene.json")
	var stdout bytes.Buffer
	if err := outputScene(document, path, true, &stdout); err != nil {
		t.Fatalf("outputScene: %v", err)
	}
	fileData, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(fileData, stdout.Bytes()) {
		t.Fatalf("file and stdout differ:\nfile=%s\nstdout=%s", fileData, stdout.Bytes())
	}
	if !bytes.Contains(fileData, []byte(`"schema": "sky.scene.v1"`)) {
		t.Fatalf("scene schema missing: %s", fileData)
	}
}
