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

	data, w, h, mime, _, err := encodeImage(src, RenderOptions{ImageMIME: "image/jpeg", ScreenW: 120})
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
