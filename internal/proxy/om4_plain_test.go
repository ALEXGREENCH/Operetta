package proxy

import (
	"testing"

	"operetta/protocol/operamini4"
)

func TestEmbeddedOM4StartupCanNavigateWithoutExternalCapture(t *testing.T) {
	request, err := operamini4.DefaultStartupRequest()
	if err != nil {
		t.Fatal(err)
	}
	const target = "https://example.com/om4"
	if !replacePlainOM4Target(request, target) {
		t.Fatal("embedded template has no replaceable navigation URL")
	}
	urls := request.RequestURLs()
	if len(urls) == 0 || urls[len(urls)-1] != target {
		t.Fatalf("navigation URLs = %#v", urls)
	}
}

func TestPlainOM4NavigationRequestCarriesTarget(t *testing.T) {
	request := plainOM4NavigationRequest("https://example.org/local")
	urls := request.RequestURLs()
	if len(urls) != 1 || urls[0] != "https://example.org/local" {
		t.Fatalf("navigation URLs = %#v", urls)
	}
}
