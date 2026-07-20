// Command om4probe replays a locally captured, decrypted-compatible OM4
// request against a reference endpoint and saves its application response.
package main

import (
	"bytes"
	"compress/flate"
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"operetta/protocol/operamini4"
)

func main() {
	in := flag.String("in", "", "captured Operetta OM4 request")
	decrypted := flag.Bool("decrypted", false, "input is a historical decrypted request.bin corpus file")
	framesInput := flag.Bool("frames", false, "input is a decrypted request.frames.bin corpus file")
	out := flag.String("out", "official-om4-response.bin", "application response output")
	endpoint := flag.String("endpoint", "http://server4.operamini.com/", "reference endpoint")
	flag.Parse()
	if *in == "" {
		flag.Usage()
		os.Exit(2)
	}
	captured, err := os.ReadFile(*in)
	check(err)
	var source *operamini4.SessionRequest
	if *framesInput {
		frames, readErr := operamini4.ReadFrameFile(*in)
		check(readErr)
		source = &operamini4.SessionRequest{Header: "local-replay", Frames: frames}
	} else if *decrypted {
		source, err = operamini4.ParseDecryptedSessionRequest(captured)
	} else {
		source, err = operamini4.ParseSessionRequest(captured)
	}
	check(err)

	nonce := make([]byte, 8)
	_, err = rand.Read(nonce)
	check(err)
	hello := append([]byte{1, 1, 0}, nonce...)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	client := &http.Client{}
	bootstrap := post(ctx, client, *endpoint, hello, false)
	request, session, err := operamini4.BuildUpstreamRequest(bootstrap, nonce, source)
	check(err)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, *endpoint, bytes.NewReader(request))
	check(err)
	req.Header.Set("Content-Type", "application/xml")
	resp, err := client.Do(req)
	check(err)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		check(fmt.Errorf("reference status %s", resp.Status))
	}
	application, frames, err := session.ReadResponse(resp.Body)
	check(err)
	check(operamini4.WriteFrameFile(*out+".frames.bin", frames))
	var compressed bytes.Buffer
	for _, frame := range frames {
		if frame.Type == 11 && frame.Channel == 1 {
			_, _ = compressed.Write(frame.Payload)
		}
	}
	if compressed.Len() > 0 {
		check(os.WriteFile(*out+".compressed", compressed.Bytes(), 0o644))
		inflater := flate.NewReader(bytes.NewReader(compressed.Bytes()))
		inflated, inflateErr := io.ReadAll(inflater)
		_ = inflater.Close()
		check(os.WriteFile(*out+".inflated", inflated, 0o644))
		fmt.Printf("compressed=%d inflated=%d inflate_error=%v\n", compressed.Len(), len(inflated), inflateErr)
	}
	check(os.WriteFile(*out, application, 0o644))
	fmt.Printf("bootstrap=%d request=%d frames=%d application=%d output=%s\n", len(bootstrap), len(request), len(frames), len(application), *out)
	for i, frame := range frames {
		fmt.Printf("frame[%d] type=%d channel=%d payload=%d\n", i, frame.Type, frame.Channel, len(frame.Payload))
	}
}

func post(ctx context.Context, client *http.Client, endpoint string, payload []byte, streaming bool) []byte {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	check(err)
	req.Header.Set("Content-Type", "application/xml")
	resp, err := client.Do(req)
	check(err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	check(err)
	return data
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "om4probe:", err)
		os.Exit(1)
	}
}
