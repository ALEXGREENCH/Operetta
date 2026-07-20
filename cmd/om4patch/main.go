// Command om4patch creates a local Opera Mini 4 MIDlet that trusts Operetta's
// development signing key and points its default transports at localhost.
package main

import (
	"archive/zip"
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"operetta/protocol/operamini4"
)

const originalSigningModulus = "c1dd7ab77e2c967746fe10681026c920f864811321bcb8be6bbfa5a03fda4e16c9c8db3af280f7703366e778e93c55e7159a8852d2b1381e521a337f22b1406cddf41a3114aecb4f4bfe79e0c5aa2ba8824fc989cb8bdcbf8ec5cef5176bfd4059f229b91bfa025126b295f9c409e75f6f6415ee094fd7f5dfd395a1f431668c5a08e88de891dc4dd38d4e9aa9b9c00dc604a0428e3aa5a28ccfa75af099147b"

func main() {
	in := flag.String("in", "", "source Opera Mini 4 JAR")
	out := flag.String("out", "", "patched output JAR")
	httpEndpoint := flag.String("http", "http://127.0.0.1:8081/", "default HTTP endpoint")
	socketEndpoint := flag.String("socket", "socket://127.0.0.1:1080", "default socket endpoint")
	flag.Parse()
	if *in == "" || *out == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := patch(*in, *out, *httpEndpoint, *socketEndpoint); err != nil {
		fmt.Fprintln(os.Stderr, "om4patch:", err)
		os.Exit(1)
	}
	fmt.Println("created", *out)
}

func patch(input, output, httpEndpoint, socketEndpoint string) error {
	source, err := zip.OpenReader(input)
	if err != nil {
		return err
	}
	defer source.Close()

	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(output), ".om4patch-*.jar")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	ok := false
	defer func() {
		temp.Close()
		if !ok {
			_ = os.Remove(tempName)
		}
	}()

	writer := zip.NewWriter(temp)
	foundKey := false
	for _, file := range source.File {
		reader, err := file.Open()
		if err != nil {
			return err
		}
		data, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}

		switch file.Name {
		case "I.class":
			oldKey := []byte(originalSigningModulus)
			if bytes.Count(data, oldKey) != 1 {
				return fmt.Errorf("I.class: expected exactly one original signing key")
			}
			data = bytes.Replace(data, oldKey, []byte(operamini4.SigningModulusHex), 1)
			foundKey = true
		case "META-INF/MANIFEST.MF":
			data = replaceManifestValue(data, "DHandlerM1", httpEndpoint)
			data = replaceManifestValue(data, "DHandlerM2", socketEndpoint)
		}

		header := file.FileHeader
		header.CRC32 = 0
		header.CompressedSize = 0
		header.CompressedSize64 = 0
		header.UncompressedSize = 0
		header.UncompressedSize64 = 0
		dest, err := writer.CreateHeader(&header)
		if err != nil {
			return err
		}
		if _, err := dest.Write(data); err != nil {
			return err
		}
	}
	if !foundKey {
		return fmt.Errorf("I.class not found")
	}
	if err := writer.Close(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempName, output); err != nil {
		_ = os.Remove(output)
		if err := os.Rename(tempName, output); err != nil {
			return err
		}
	}
	ok = true
	return nil
}

func replaceManifestValue(manifest []byte, name, value string) []byte {
	lines := bytes.Split(manifest, []byte("\n"))
	prefix := []byte(name + ": ")
	for i := range lines {
		line := bytes.TrimSuffix(lines[i], []byte("\r"))
		if bytes.HasPrefix(line, prefix) {
			ending := []byte(nil)
			if bytes.HasSuffix(lines[i], []byte("\r")) {
				ending = []byte("\r")
			}
			lines[i] = append(append(append([]byte(nil), prefix...), value...), ending...)
			break
		}
	}
	return bytes.Join(lines, []byte("\n"))
}
