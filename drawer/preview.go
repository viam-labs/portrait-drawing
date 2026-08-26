package drawer

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
)

const (
	previewMaxPx     = 4000
	previewBorderLum = 200
)

// renderPreviewPNG draws the polylines as they will be traced on the paper —
// paper-local mm scaled to pixels — so the preview shows the pen's path rather
// than what the camera saw.
func renderPreviewPNG(polylines []Polyline, paperWidthMM, paperHeightMM, pxPerMM float64) ([]byte, error) {
	w := int(math.Round(paperWidthMM * pxPerMM))
	h := int(math.Round(paperHeightMM * pxPerMM))
	if w < 1 || h < 1 {
		return nil, fmt.Errorf("preview would be %dx%d px; raise preview_px_per_mm", w, h)
	}
	if w > previewMaxPx || h > previewMaxPx {
		return nil, fmt.Errorf("preview would be %dx%d px, over the %d px limit; lower preview_px_per_mm", w, h, previewMaxPx)
	}

	img := image.NewGray(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	border := color.Gray{Y: previewBorderLum}
	for x := 0; x < w; x++ {
		img.SetGray(x, 0, border)
		img.SetGray(x, h-1, border)
	}
	for y := 0; y < h; y++ {
		img.SetGray(0, y, border)
		img.SetGray(w-1, y, border)
	}

	ink := color.Gray{Y: 0}
	for _, poly := range polylines {
		for i := 1; i < len(poly); i++ {
			drawLine(img,
				int(math.Round(poly[i-1][0]*pxPerMM)), int(math.Round(poly[i-1][1]*pxPerMM)),
				int(math.Round(poly[i][0]*pxPerMM)), int(math.Round(poly[i][1]*pxPerMM)),
				ink)
		}
		if len(poly) == 1 {
			setGrayInBounds(img, int(math.Round(poly[0][0]*pxPerMM)), int(math.Round(poly[0][1]*pxPerMM)), ink)
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("encode preview png: %w", err)
	}
	return buf.Bytes(), nil
}

func drawLine(img *image.Gray, x0, y0, x1, y1 int, c color.Gray) {
	dx := abs(x1 - x0)
	dy := -abs(y1 - y0)
	sx, sy := 1, 1
	if x0 > x1 {
		sx = -1
	}
	if y0 > y1 {
		sy = -1
	}
	err := dx + dy
	for {
		setGrayInBounds(img, x0, y0, c)
		if x0 == x1 && y0 == y1 {
			return
		}
		e2 := 2 * err
		if e2 >= dy {
			err += dy
			x0 += sx
		}
		if e2 <= dx {
			err += dx
			y0 += sy
		}
	}
}

func setGrayInBounds(img *image.Gray, x, y int, c color.Gray) {
	if !(image.Point{X: x, Y: y}).In(img.Bounds()) {
		return
	}
	img.SetGray(x, y, c)
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
