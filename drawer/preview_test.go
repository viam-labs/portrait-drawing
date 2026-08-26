package drawer

import (
	"bytes"
	"image/png"
	"testing"

	"go.viam.com/test"
)

func TestRenderPreviewPNG_dimensionsMatchPaper(t *testing.T) {
	raw, err := renderPreviewPNG([]Polyline{{{0, 0}, {50, 30}}}, 100, 60, 2)
	test.That(t, err, test.ShouldBeNil)
	img, err := png.Decode(bytes.NewReader(raw))
	test.That(t, err, test.ShouldBeNil)
	test.That(t, img.Bounds().Dx(), test.ShouldEqual, 200)
	test.That(t, img.Bounds().Dy(), test.ShouldEqual, 120)
}

func TestRenderPreviewPNG_drawsInk(t *testing.T) {
	raw, err := renderPreviewPNG([]Polyline{{{10, 10}, {90, 50}}}, 100, 60, 2)
	test.That(t, err, test.ShouldBeNil)
	img, err := png.Decode(bytes.NewReader(raw))
	test.That(t, err, test.ShouldBeNil)
	r, _, _, _ := img.At(20, 20).RGBA()
	test.That(t, r, test.ShouldEqual, 0)
}

func TestRenderPreviewPNG_singlePointPolyline(t *testing.T) {
	raw, err := renderPreviewPNG([]Polyline{{{25, 15}}}, 100, 60, 2)
	test.That(t, err, test.ShouldBeNil)
	img, err := png.Decode(bytes.NewReader(raw))
	test.That(t, err, test.ShouldBeNil)
	r, _, _, _ := img.At(50, 30).RGBA()
	test.That(t, r, test.ShouldEqual, 0)
}

func TestRenderPreviewPNG_pointsOutsidePaperAreClipped(t *testing.T) {
	_, err := renderPreviewPNG([]Polyline{{{-500, -500}, {5000, 5000}}}, 100, 60, 2)
	test.That(t, err, test.ShouldBeNil)
}

func TestRenderPreviewPNG_tooLarge(t *testing.T) {
	_, err := renderPreviewPNG([]Polyline{{{0, 0}, {1, 1}}}, 216, 279, 50)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "preview_px_per_mm")
}

func TestRenderPreviewPNG_tooSmall(t *testing.T) {
	_, err := renderPreviewPNG([]Polyline{{{0, 0}, {1, 1}}}, 0.1, 0.1, 0.001)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "preview_px_per_mm")
}
