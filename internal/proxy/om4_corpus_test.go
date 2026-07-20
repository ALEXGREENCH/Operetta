package proxy

import (
	"bytes"
	"testing"

	"operetta/protocol/operamini4"
)

func TestSanitizeOM4RequestFramesRedactsSecretsWithoutMutatingRequest(t *testing.T) {
	original := []byte("contact=user%40example.test&password=hunter2&token=abc123&keep=yes")
	frames := []operamini4.Frame{{Type: 8, Channel: 1, Payload: append([]byte(nil), original...)}}

	safe := sanitizeOM4RequestFrames(frames)
	if !bytes.Equal(frames[0].Payload, original) {
		t.Fatalf("source frame was mutated: %q", frames[0].Payload)
	}
	got := string(safe[0].Payload)
	if got != "contact=user%40example.test&password=*******&token=******&keep=yes" {
		t.Fatalf("sanitized payload = %q", got)
	}
}

func TestSanitizeOM4RequestFramesLeavesNavigationAndNonSecretFields(t *testing.T) {
	frames := []operamini4.Frame{
		{Type: 8, Channel: 1, Payload: []byte("q=opera+mini&passwordish=visible")},
		{Type: 9, Channel: 1, Payload: []byte("password=not-a-form-frame")},
	}
	safe := sanitizeOM4RequestFrames(frames)
	for i := range frames {
		if !bytes.Equal(safe[i].Payload, frames[i].Payload) {
			t.Fatalf("frame %d unexpectedly changed: %q", i, safe[i].Payload)
		}
	}
}
