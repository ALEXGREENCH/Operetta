// Command om4corpus inspects decrypted OM4 corpus frame files.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"operetta/protocol/operamini4"
)

func main() {
	in := flag.String("in", "", "response.frames.bin corpus file")
	requestIn := flag.String("request-in", "", "historical decrypted request.bin corpus file")
	requestFramesIn := flag.String("request-frames-in", "", "decrypted request.frames.bin corpus file")
	out := flag.String("page-out", "", "optional decoded page bytecode output")
	jsonOutput := flag.Bool("json", false, "print JSON summary")
	sceneJSON := flag.Bool("scene-json", false, "print protocol-neutral sky.scene.v1 JSON")
	sceneOut := flag.String("scene-out", "", "write protocol-neutral sky.scene.v1 JSON to a file")
	drawingsOutput := flag.Bool("drawings", false, "print decoded drawing commands")
	flag.Parse()
	if *requestIn != "" || *requestFramesIn != "" {
		var request *operamini4.SessionRequest
		if *requestFramesIn != "" {
			frames, err := operamini4.ReadFrameFile(*requestFramesIn)
			check(err)
			request = &operamini4.SessionRequest{Frames: frames}
		} else {
			data, err := os.ReadFile(*requestIn)
			check(err)
			var parseErr error
			request, parseErr = operamini4.ParseDecryptedSessionRequest(data)
			check(parseErr)
		}
		fmt.Printf("sequence=%d header=%q frames=%d\n", request.Sequence, request.Header, len(request.Frames))
		for index, frame := range request.Frames {
			fmt.Printf("frame[%d] type=%d channel=%d payload=%d %q\n", index, frame.Type, frame.Channel, len(frame.Payload), printable(frame.Payload))
		}
		fmt.Printf("urls=%q\n", request.RequestURLs())
		return
	}
	if *in == "" {
		flag.Usage()
		os.Exit(2)
	}
	frames, err := operamini4.ReadFrameFile(*in)
	check(err)
	document, err := operamini4.DecodeApplicationDocument(frames)
	check(err)
	if *out != "" {
		check(os.WriteFile(*out, document.Page, 0o600))
	}
	if *jsonOutput {
		check(json.NewEncoder(os.Stdout).Encode(document))
		return
	}
	if *sceneJSON || *sceneOut != "" {
		check(outputScene(document, *sceneOut, *sceneJSON, os.Stdout))
		return
	}
	fmt.Printf("id=%d inflated=%d records=%d page=%d warning=%v\n", document.ID, len(document.Inflated), len(document.Records), len(document.Page), document.InflateWarning)
	fmt.Printf("title=%q url=%q base=%q viewport=%d height=%d flags=0x%02x\n", document.Header.Title, document.Header.URL, document.Header.Base, document.Header.ViewportWidth, document.Header.DocumentHeight, document.Header.Flags)
	if len(document.Records) > 0 {
		items := make([]string, 0, len(document.Records))
		for _, record := range document.Records {
			items = append(items, fmt.Sprintf("%q:%d", record.Type, len(record.Payload)))
		}
		fmt.Printf("records: %s\n", strings.Join(items, ", "))
	}
	if *drawingsOutput {
		for index, element := range document.Drawings {
			fmt.Printf("drawing[%d] kind=%c x=%d y=%d w=%d h=%d color=%08x font=%d pointer=%d text=%q\n",
				index, element.Kind, element.X, element.Y, element.Width, element.Height, element.Color, element.Font, element.ImagePointer, element.Text)
		}
		return
	}
	for _, element := range document.Texts {
		fmt.Printf("text x=%d y=%d w=%d h=%d style=%08x font=%d %q\n", element.X, element.Y, element.Width, element.Height, element.Style, element.Font, element.Text)
	}
}

func outputScene(document *operamini4.ApplicationDocument, path string, stdout bool, writer io.Writer) error {
	data, err := operamini4.MarshalScene(document)
	if err != nil {
		return err
	}
	if path != "" {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return err
		}
	}
	if stdout {
		_, err = writer.Write(data)
	}
	return err
}

func printable(data []byte) string {
	value := make([]byte, len(data))
	for index, item := range data {
		if item >= 0x20 && item <= 0x7e {
			value[index] = item
		} else {
			value[index] = '.'
		}
	}
	return string(value)
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "om4corpus:", err)
		os.Exit(1)
	}
}
