package thumb

import (
	"bytes"
	"image"
	"image/png"
	"testing"
)

func TestFromImage(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 800, 600))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	thumb, err := FromImage(buf.Bytes())
	if err != nil {
		t.Fatalf("FromImage failed: %v", err)
	}
	if len(thumb) == 0 {
		t.Fatal("expected thumbnail bytes")
	}
	decoded, format, err := image.Decode(bytes.NewReader(thumb))
	if err != nil {
		t.Fatalf("decode thumbnail failed: %v", err)
	}
	if format != "jpeg" {
		t.Fatalf("expected jpeg thumbnail, got %s", format)
	}
	bounds := decoded.Bounds()
	if bounds.Dx() > maxWidth || bounds.Dy() > maxHeight {
		t.Fatalf("thumbnail too large: %dx%d", bounds.Dx(), bounds.Dy())
	}
}

func TestScaleNoUpscaling(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	scaled := scale(img)
	bounds := scaled.Bounds()
	if bounds.Dx() != 100 || bounds.Dy() != 100 {
		t.Fatalf("expected 100x100, got %dx%d", bounds.Dx(), bounds.Dy())
	}
}
