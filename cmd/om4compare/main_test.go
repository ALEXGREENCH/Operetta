package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"operetta/protocol/operamini4"
)

func TestExchangePairStartsSidesTogether(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	go func() {
		<-started
		<-started
		close(release)
	}()
	exchange := func(payload byte) exchangeFunc {
		return func(ctx context.Context, _ *operamini4.SessionRequest) ([]operamini4.Frame, error) {
			started <- struct{}{}
			select {
			case <-release:
				return []operamini4.Frame{{Channel: 1, Type: 8, Payload: []byte{payload}}}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}

	reference, native := exchangePair(
		exchange(0x11), &operamini4.SessionRequest{},
		exchange(0x22), &operamini4.SessionRequest{},
		time.Second,
	)
	if reference.err != nil || native.err != nil {
		t.Fatalf("exchange errors: reference=%v native=%v", reference.err, native.err)
	}
	if got := reference.frames[0].Payload[0]; got != 0x11 {
		t.Fatalf("reference payload: %#x", got)
	}
	if got := native.frames[0].Payload[0]; got != 0x22 {
		t.Fatalf("native payload: %#x", got)
	}
}

func TestExchangePairUsesIndependentTimeouts(t *testing.T) {
	waitForTimeout := func(ctx context.Context, _ *operamini4.SessionRequest) ([]operamini4.Frame, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	returnImmediately := func(context.Context, *operamini4.SessionRequest) ([]operamini4.Frame, error) {
		return []operamini4.Frame{{Channel: 1, Type: 8}}, nil
	}

	reference, native := exchangePair(
		waitForTimeout, &operamini4.SessionRequest{},
		returnImmediately, &operamini4.SessionRequest{},
		20*time.Millisecond,
	)
	if !errors.Is(reference.err, context.DeadlineExceeded) {
		t.Fatalf("reference error: %v", reference.err)
	}
	if native.err != nil || len(native.frames) != 1 {
		t.Fatalf("native result: frames=%d error=%v", len(native.frames), native.err)
	}
}

func TestComparisonJSONIncludesExchangeDurations(t *testing.T) {
	data, err := json.Marshal(comparison{ReferenceDurationMS: 31, NativeDurationMS: 17})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	value := string(data)
	if !strings.Contains(value, `"reference_duration_ms":31`) || !strings.Contains(value, `"native_duration_ms":17`) {
		t.Fatalf("comparison JSON: %s", value)
	}
}

func TestWriteComparisonSceneWritesCanonicalScene(t *testing.T) {
	document := &operamini4.ApplicationDocument{
		Header: operamini4.PageHeader{ViewportWidth: 231, URL: "1/http://example.test/"},
	}
	path := filepath.Join(t.TempDir(), "example.reference.scene.json")
	errorField := ""
	writeComparisonScene(path, "reference", document, &errorField)
	if errorField != "" {
		t.Fatalf("error field: %s", errorField)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var scene operamini4.Scene
	if err := json.Unmarshal(data, &scene); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if scene.Schema != "sky.scene.v1" || scene.Document.URL != "http://example.test/" {
		t.Fatalf("scene: %+v", scene)
	}
}

func TestWriteComparisonSceneReportsWriteError(t *testing.T) {
	document := &operamini4.ApplicationDocument{}
	path := filepath.Join(t.TempDir(), "missing", "example.operetta.scene.json")
	errorField := ""
	writeComparisonScene(path, "operetta", document, &errorField)
	if !strings.Contains(errorField, "write operetta scene:") {
		t.Fatalf("error field: %q", errorField)
	}
}

func TestTemplateLoadersFallBackToEmbeddedStartup(t *testing.T) {
	corpusDir := t.TempDir()
	startup, err := loadStartupRequest("", corpusDir)
	if err != nil {
		t.Fatalf("loadStartupRequest: %v", err)
	}
	navigation, err := loadNavigationTemplate("", corpusDir)
	if err != nil {
		t.Fatalf("loadNavigationTemplate: %v", err)
	}
	const target = "https://example.test/compare"
	if _, err := startupNavigationRequest(startup, "header", target); err != nil {
		t.Fatalf("startup template cannot navigate: %v", err)
	}
	if _, err := navigationRequestFromTemplate(navigation, "header", "h22-02-04", nil, target); err != nil {
		t.Fatalf("navigation template cannot navigate: %v", err)
	}
}

func TestCloneSessionRequestDoesNotSharePayloads(t *testing.T) {
	source, err := operamini4.DefaultStartupRequest()
	if err != nil {
		t.Fatal(err)
	}
	clone := cloneSessionRequest(source)
	clone.Frames[0].Payload[0] ^= 0xff
	if clone.Frames[0].Payload[0] == source.Frames[0].Payload[0] {
		t.Fatal("clone shares mutable frame payload")
	}
}

func TestComparisonHelpersExposeActionableDifferences(t *testing.T) {
	missing, extra := setDifference([]string{"same", "missing"}, []string{"same", "extra"})
	if strings.Join(missing, ",") != "missing" || strings.Join(extra, ",") != "extra" {
		t.Fatalf("text difference missing=%v extra=%v", missing, extra)
	}
	coverage := stringCoverage(
		styleColors([]string{"cc000000/5", "cc334488/0"}),
		styleColors([]string{"ff000000/2", "ff334488/0"}),
	)
	if coverage != 1 {
		t.Fatalf("RGB color coverage = %v", coverage)
	}
	reference := &operamini4.ApplicationDocument{Drawings: []operamini4.DrawingElement{
		{Kind: 'B', X: 0, Y: 0, Width: 100, Height: 100},
		{Kind: 'T', X: 10, Y: 10, Width: 50, Height: 10, Text: "hello"},
	}}
	native := &operamini4.ApplicationDocument{Drawings: []operamini4.DrawingElement{
		{Kind: 'B', X: 0, Y: 0, Width: 100, Height: 100},
		{Kind: 'T', X: 10, Y: 10, Width: 50, Height: 10, Text: "hello"},
	}}
	if got := drawingGeometryCoverage(reference, native); got != 1 {
		t.Fatalf("identical geometry coverage = %v", got)
	}
}
