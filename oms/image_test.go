package oms

import (
	"bytes"
	"image"
	"image/color"
	_ "image/png"
	"math"
	"strings"
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

func TestLowMemoryJPEGQualityUsesTighterCompression(t *testing.T) {
	low := defaultRenderPrefs()
	low.HeapBytes = lowMemoryImageHeapLimit
	if got := jpegQualityFor(low); got != 32 {
		t.Fatalf("low-memory low-quality JPEG=%d, want 32", got)
	}
	low.HighQuality = true
	if got := jpegQualityFor(low); got != 76 {
		t.Fatalf("low-memory high-quality JPEG=%d, want 76", got)
	}
	normal := low
	normal.HeapBytes = lowMemoryImageHeapLimit + 1
	if got := jpegQualityFor(normal); got != 85 {
		t.Fatalf("normal-memory high-quality JPEG=%d, want 85", got)
	}
}

func TestLowMemoryPNGUsesRGB444AndRGB332Palettes(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	source.SetNRGBA(0, 0, color.NRGBA{R: 123, G: 77, B: 219, A: 201})

	for _, test := range []struct {
		name string
		high bool
		bits [3]uint
	}{
		{name: "rgb332", bits: [3]uint{3, 3, 2}},
		{name: "rgb444", high: true, bits: [3]uint{4, 4, 4}},
	} {
		t.Run(test.name, func(t *testing.T) {
			opts := defaultRenderPrefs()
			opts.ImageMIME = "image/png"
			opts.HeapBytes = lowMemoryImageHeapLimit
			opts.HighQuality = test.high
			data, _, _, _, _, err := encodeImage(source, opts)
			if err != nil {
				t.Fatal(err)
			}
			decoded, _, err := image.Decode(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			got := color.NRGBAModel.Convert(decoded.At(0, 0)).(color.NRGBA)
			want := color.NRGBA{
				R: expandQuantizedChannel(123, test.bits[0]),
				G: expandQuantizedChannel(77, test.bits[1]),
				B: expandQuantizedChannel(219, test.bits[2]),
				A: 201,
			}
			if got != want {
				t.Fatalf("quantized pixel=%v, want %v", got, want)
			}
		})
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

func TestEncodeImageRGB565UsesBigEndianAndFlattensAlpha(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 3, 1))
	src.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 255})
	src.SetNRGBA(1, 0, color.NRGBA{G: 255, A: 255})
	src.SetNRGBA(2, 0, color.NRGBA{B: 255, A: 0})

	opts := defaultRenderPrefs()
	opts.ImageMIME = RGB565MIME
	opts.MaxInlineKB = 16
	data, w, h, mime, _, err := encodeImage(src, opts)
	if err != nil {
		t.Fatalf("encodeImage returned error: %v", err)
	}
	if mime != RGB565MIME || w != 3 || h != 1 {
		t.Fatalf("unexpected result: mime=%q dimensions=%dx%d", mime, w, h)
	}
	want := []byte{0xf8, 0x00, 0x07, 0xe0, 0xff, 0xff}
	if len(data) != len(want) {
		t.Fatalf("payload length=%d want=%d", len(data), len(want))
	}
	for i := range want {
		if data[i] != want[i] {
			t.Fatalf("payload[%d]=%02x want=%02x", i, data[i], want[i])
		}
	}
}

func TestEncodeImageRGB565AlphaPreservesTransparency(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 3, 1))
	src.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 255})
	src.SetNRGBA(1, 0, color.NRGBA{G: 255, A: 128})
	src.SetNRGBA(2, 0, color.NRGBA{B: 255, A: 0})

	opts := defaultRenderPrefs()
	opts.ImageMIME = RGB565AlphaMIME
	opts.MaxInlineKB = 16
	data, w, h, mime, _, err := encodeImage(src, opts)
	if err != nil {
		t.Fatalf("encodeImage returned error: %v", err)
	}
	if mime != RGB565AlphaMIME || w != 3 || h != 1 {
		t.Fatalf("unexpected result: mime=%q dimensions=%dx%d", mime, w, h)
	}
	want := []byte{
		0xf8, 0x00, 0xff,
		0x07, 0xe0, 0x80,
		0x00, 0x1f, 0x00,
	}
	if len(data) != len(want) {
		t.Fatalf("payload length=%d want=%d", len(data), len(want))
	}
	for i := range want {
		if data[i] != want[i] {
			t.Fatalf("payload[%d]=%02x want=%02x", i, data[i], want[i])
		}
	}
}

func TestEncodeImageRGB565HonorsInlineBudget(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 240, 160))
	opts := defaultRenderPrefs()
	opts.ImageMIME = RGB565MIME
	opts.ScreenW = 240
	opts.MaxInlineKB = 8
	data, w, h, _, _, err := encodeImage(src, opts)
	if err != nil {
		t.Fatalf("encodeImage returned error: %v", err)
	}
	if len(data) > 8*1024 || len(data) != w*h*2 {
		t.Fatalf("payload=%d dimensions=%dx%d", len(data), w, h)
	}
	if w >= 240 || h >= 160 {
		t.Fatalf("image was not downscaled: %dx%d", w, h)
	}
}

func TestEncodeImageOM2BasicFitsPNGToInlineBudget(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 240, 320))
	for y := 0; y < 320; y++ {
		for x := 0; x < 240; x++ {
			src.SetNRGBA(x, y, color.NRGBA{
				R: uint8((x*11 + y*19) & 0xff),
				G: uint8((x*23 + y*3) & 0xff),
				B: uint8((x*y + x*7 + y) & 0xff),
				A: uint8(128 + ((x + y) & 0x7f)),
			})
		}
	}
	opts := defaultRenderPrefs()
	opts.DialectID = "om2-basic"
	opts.ImageMIME = "image/png"
	opts.ScreenW = 240
	opts.MaxInlineKB = 8
	data, w, h, mime, _, err := encodeImage(src, opts)
	if err != nil {
		t.Fatalf("encodeImage returned error: %v", err)
	}
	if mime != "image/png" {
		t.Fatalf("mime=%q", mime)
	}
	if len(data) == 0 || len(data) > 8*1024 {
		t.Fatalf("PNG payload=%d exceeds legacy budget", len(data))
	}
	if len(data) < 4 || data[0] != 0x89 || string(data[1:4]) != "PNG" {
		t.Fatalf("not a PNG payload: %x", data)
	}
	if w <= 0 || h <= 0 || w > 240 || h > 320 {
		t.Fatalf("dimensions=%dx%d", w, h)
	}
}

func TestEncodeImageOM2BasicFitsJPEGToInlineBudget(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 240, 320))
	for y := 0; y < 320; y++ {
		for x := 0; x < 240; x++ {
			src.Set(x, y, color.RGBA{
				R: uint8((x*13 + y*7) & 0xff),
				G: uint8((x*5 + y*17) & 0xff),
				B: uint8((x*y + x + y) & 0xff),
				A: 0xff,
			})
		}
	}
	opts := defaultRenderPrefs()
	opts.DialectID = "om2-basic"
	opts.ImageMIME = "image/jpeg"
	opts.ScreenW = 240
	opts.MaxInlineKB = 8
	data, w, h, mime, quality, err := encodeImage(src, opts)
	if err != nil {
		t.Fatalf("encodeImage returned error: %v", err)
	}
	if mime != "image/jpeg" {
		t.Fatalf("mime=%q", mime)
	}
	if len(data) == 0 || len(data) > 8*1024 {
		t.Fatalf("payload=%d exceeds legacy budget", len(data))
	}
	if w <= 0 || h <= 0 || w > 240 || h > 320 {
		t.Fatalf("dimensions=%dx%d", w, h)
	}
	if quality > 40 || quality < 25 {
		t.Fatalf("quality=%d", quality)
	}
}

func TestImageCacheVariantKeySeparatesDialectsAndCodecGeneration(t *testing.T) {
	opts := defaultRenderPrefs()
	opts.CachePartition = "test-session"
	opts.ScreenW = 240
	opts.ScreenH = 320
	opts.ImageMIME = "image/jpeg"
	opts.MaxInlineKB = 8
	base := imageCacheVariantKey("https://example.test/a.jpg", opts)
	if base == "" {
		t.Fatal("expected cache key")
	}
	opts.DialectID = "om2-basic"
	legacy := imageCacheVariantKey("https://example.test/a.jpg", opts)
	if legacy == base {
		t.Fatalf("dialect must partition image cache: %q", legacy)
	}
	if !strings.Contains(legacy, "|dialect=om2-basic|lowmem=false|v=4") {
		t.Fatalf("legacy cache key missing dialect/generation: %q", legacy)
	}
}

func TestEncodeImageOM2BasicFlattensAlphaToJPEG(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 80, 80))
	for y := 0; y < 80; y++ {
		for x := 0; x < 80; x++ {
			src.SetNRGBA(x, y, color.NRGBA{R: 220, G: uint8(x * 3), B: uint8(y * 3), A: uint8((x + y) & 0xff)})
		}
	}
	opts := defaultRenderPrefs()
	opts.DialectID = "om2-basic"
	opts.ImageMIME = "image/jpeg"
	opts.MaxInlineKB = 8
	data, _, _, mime, _, err := encodeImage(src, opts)
	if err != nil {
		t.Fatalf("encodeImage returned error: %v", err)
	}
	if mime != "image/jpeg" {
		t.Fatalf("expected flattened JPEG, got %q", mime)
	}
	if len(data) == 0 || len(data) > 8*1024 {
		t.Fatalf("payload=%d", len(data))
	}
}
