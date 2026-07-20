package operamini4

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestReferenceClientExchange(t *testing.T) {
	wantFrames := []Frame{
		{Type: 9, Channel: 1},
		{Type: 10, Channel: 1, Payload: []byte{0, 0, 3}},
		{Type: 11, Channel: 1, Payload: []byte("abc")},
		{Type: 6, Channel: 1},
	}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		if IsBootstrapHello(body) {
			_, _ = w.Write(BootstrapResponse(time.Unix(1_750_000_000, 0), body[3:]))
			return
		}
		request, err := ParseSessionRequest(body)
		if err != nil {
			t.Errorf("parse session request: %v", err)
			return
		}
		if request.Header != "session-header" {
			t.Errorf("header=%q", request.Header)
		}
		if len(request.Frames) != 1 || !bytes.Equal(request.Frames[0].Payload, []byte("navigate")) {
			t.Errorf("unexpected request frames: %+v", request.Frames)
		}
		response, err := request.BuildResponseFrames(wantFrames)
		if err != nil {
			t.Errorf("build response: %v", err)
			return
		}
		_, _ = w.Write(response)
	}))
	defer endpoint.Close()

	client, err := NewReferenceClient(endpoint.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	source := &SessionRequest{
		Header: "session-header",
		Frames: []Frame{{Type: 10, Channel: 1, Payload: []byte("navigate")}},
	}
	gotFrames, err := client.Exchange(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotFrames) != len(wantFrames) {
		t.Fatalf("frames=%d, want %d", len(gotFrames), len(wantFrames))
	}
	for i := range wantFrames {
		if gotFrames[i].Type != wantFrames[i].Type || gotFrames[i].Channel != wantFrames[i].Channel || !bytes.Equal(gotFrames[i].Payload, wantFrames[i].Payload) {
			t.Fatalf("frame[%d]=%+v, want %+v", i, gotFrames[i], wantFrames[i])
		}
	}
}
