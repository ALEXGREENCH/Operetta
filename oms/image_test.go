package oms

import (
	"image"
	"image/color"
	"math"
	"testing"
)

func TestClampImageToScreenWidthDownscale(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 400, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 400; x++ {
			src.Set(x, y, color.RGBA{uint8((x + y) % 256), uint8((2 * x) % 256), uint8((3 * y) % 256), 0xFF})
		}
	}

	dst, w, h := clampImageToScreenWidth(src, 160)
	if w != 160 {
		t.Fatalf("width mismatch: got %d", w)
	}
	expectedH := int(math.Round(float64(200) * float64(160) / float64(400)))
	if h != expectedH {
		t.Fatalf("height mismatch: got %d want %d", h, expectedH)
	}
	if dst.Bounds().Dx() != 160 || dst.Bounds().Dy() != expectedH {
		t.Fatalf("scaled bounds mismatch: got %dx%d", dst.Bounds().Dx(), dst.Bounds().Dy())
	}
	if dst == src {
		t.Fatal("expected a new image instance after downscale")
	}
}

func TestClampImageToScreenWidthNoChange(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 300, 120))
	dst, w, h := clampImageToScreenWidth(src, 400)
	if w != 300 || h != 120 {
		t.Fatalf("unexpected dimensions: got %dx%d", w, h)
	}
	if dst != src {
		t.Fatal("expected original image to be reused when within limits")
	}
}

func TestEncodeImageRespectsScreenWidth(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 360, 180))
	for y := 0; y < 180; y++ {
		for x := 0; x < 360; x++ {
			src.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 0x7F, 0xFF})
		}
	}

	opts := defaultRenderPrefs()
	opts.ImageMIME = "image/jpeg"
	opts.ScreenW = 120
	data, w, h, mime, _, err := encodeImage(src, opts)
	if err != nil {
		t.Fatalf("encodeImage returned error: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("encodeImage returned empty payload")
	}
	if mime != "image/jpeg" {
		t.Fatalf("unexpected mime: %s", mime)
	}
	if w != 120 {
		t.Fatalf("width not clamped: got %d", w)
	}
	expectedH := int(math.Round(float64(180) * float64(120) / float64(360)))
	if h != expectedH {
		t.Fatalf("height mismatch: got %d want %d", h, expectedH)
	}
}

func TestEncodeImageHighQualityRetainsRequestedQuality(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 200, 150))
	for y := 0; y < 150; y++ {
		for x := 0; x < 200; x++ {
			src.Set(x, y, color.RGBA{uint8((x + y) % 256), uint8((x * 2) % 256), 0x55, 0xFF})
		}
	}

	opts := defaultRenderPrefs()
	opts.HighQuality = true
	opts.ImageMIME = "image/jpeg"
	_, _, _, _, quality, err := encodeImage(src, opts)
	if err != nil {
		t.Fatalf("encodeImage returned error: %v", err)
	}
	want := jpegQualityFor(opts)
	if quality != want {
		t.Fatalf("expected quality=%d in high quality mode, got %d", want, quality)
	}
}

func TestEncodeImageGifHonorsMime(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			src.Set(x, y, color.RGBA{uint8((x * 5) % 256), uint8((y * 3) % 256), uint8((x + y) % 256), 0xFF})
		}
	}

	opts := defaultRenderPrefs()
	opts.ImageMIME = "image/gif"
	data, w, h, mime, _, err := encodeImage(src, opts)
	if err != nil {
		t.Fatalf("encodeImage returned error: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("encodeImage returned empty payload for gif")
	}
	if mime != "image/gif" {
		t.Fatalf("expected image/gif mime, got %s", mime)
	}
	if w != 64 || h != 64 {
		t.Fatalf("unexpected dimensions: %dx%d", w, h)
	}
}
