package operamini4

import "testing"

func TestDefaultStartupRequestIsFreshAndCarriesNavigationURL(t *testing.T) {
	first, err := DefaultStartupRequest()
	if err != nil {
		t.Fatal(err)
	}
	second, err := DefaultStartupRequest()
	if err != nil {
		t.Fatal(err)
	}
	urls := first.RequestURLs()
	if len(urls) == 0 {
		t.Fatal("default startup request has no navigation URL")
	}
	if len(first.Frames) == 0 || len(first.Frames[0].Payload) == 0 {
		t.Fatal("default startup request has no frame payload")
	}
	first.Frames[0].Payload[0] ^= 0xff
	if first.Frames[0].Payload[0] == second.Frames[0].Payload[0] {
		t.Fatal("DefaultStartupRequest returned shared mutable payload")
	}
}
