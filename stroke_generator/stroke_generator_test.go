package strokegenerator

import (
	"encoding/base64"
	"strings"
	"testing"

	"go.viam.com/test"
)

func TestConfigValidate_empty(t *testing.T) {
	cfg := &Config{}
	deps, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, deps, test.ShouldBeEmpty)
}

func TestConfigValidate_negativeSmooth(t *testing.T) {
	cfg := &Config{Smooth: -1}
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "smooth")
}

func ptr[T any](v T) *T { return &v }

func TestParseGenerateArgs_valid(t *testing.T) {
	payload := map[string]interface{}{
		"image_b64":       base64.StdEncoding.EncodeToString([]byte("fake image")),
		"paper_width_mm":  215.9,
		"paper_height_mm": 279.4,
		"margin_mm":       30.0,
		"rotate":          90,
		"mirror":          true,
	}
	got, err := parseGenerateArgs(payload)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, got.PaperWidthMM, test.ShouldEqual, 215.9)
	test.That(t, got.Rotate, test.ShouldEqual, 90)
	test.That(t, got.Mirror, test.ShouldBeTrue)
	test.That(t, got.MarginMM, test.ShouldEqual, 30.0)
}

func TestParseGenerateArgs_marginDefaults(t *testing.T) {
	payload := map[string]interface{}{
		"image_b64":       "abc",
		"paper_width_mm":  100.0,
		"paper_height_mm": 100.0,
	}
	got, err := parseGenerateArgs(payload)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, got.MarginMM, test.ShouldEqual, defaultMargin)
}

func TestParseGenerateArgs_missingImage(t *testing.T) {
	payload := map[string]interface{}{
		"paper_width_mm":  100.0,
		"paper_height_mm": 100.0,
	}
	_, err := parseGenerateArgs(payload)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "image_b64")
}

func TestParseGenerateArgs_missingPaperWidth(t *testing.T) {
	payload := map[string]interface{}{
		"image_b64":       "abc",
		"paper_height_mm": 100.0,
	}
	_, err := parseGenerateArgs(payload)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "paper_width_mm")
}

func TestParseGenerateArgs_badRotate(t *testing.T) {
	payload := map[string]interface{}{
		"image_b64":       "abc",
		"paper_width_mm":  100.0,
		"paper_height_mm": 100.0,
		"rotate":          45,
	}
	_, err := parseGenerateArgs(payload)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "rotate")
}

func TestParseGenerateArgs_negativeMargin(t *testing.T) {
	payload := map[string]interface{}{
		"image_b64":       "abc",
		"paper_width_mm":  100.0,
		"paper_height_mm": 100.0,
		"margin_mm":       -5.0,
	}
	_, err := parseGenerateArgs(payload)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "margin_mm")
}

// argValue returns the value following flag in an --flag value arg slice.
func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestBuildCLIArgs_passesSetKnobs(t *testing.T) {
	s := &strokeGenerator{cfg: &Config{
		Size: ptr(896), Clahe: ptr(2.5), Sigma: ptr(1.8),
		Low: ptr(3.0), High: ptr(10.0), IsolateSubject: ptr(true),
	}}
	args := s.buildCLIArgs(&generateArgs{
		PaperWidthMM: 100, PaperHeightMM: 100, MarginMM: 10,
	})
	test.That(t, argValue(args, "--size"), test.ShouldEqual, "896")
	test.That(t, argValue(args, "--clahe"), test.ShouldEqual, "2.5")
	test.That(t, argValue(args, "--sigma"), test.ShouldEqual, "1.8")
	test.That(t, argValue(args, "--low"), test.ShouldEqual, "3")
	test.That(t, argValue(args, "--high"), test.ShouldEqual, "10")
	test.That(t, args, test.ShouldContain, "--isolate-subject")
}

func TestBuildCLIArgs_mirrorAppended(t *testing.T) {
	s := &strokeGenerator{cfg: &Config{}}
	args := s.buildCLIArgs(&generateArgs{
		PaperWidthMM: 100, PaperHeightMM: 100, MarginMM: 10, Rotate: 90, Mirror: true,
	})
	test.That(t, args, test.ShouldContain, "--mirror")
}

func TestBuildCLIArgs_isolateSubjectDisabled(t *testing.T) {
	disabled := false
	s := &strokeGenerator{cfg: &Config{IsolateSubject: &disabled}}
	args := s.buildCLIArgs(&generateArgs{
		PaperWidthMM: 100, PaperHeightMM: 100, MarginMM: 10, Rotate: 0, Mirror: false,
	})
	test.That(t, args, test.ShouldContain, "--no-isolate-subject")
	test.That(t, args, test.ShouldNotContain, "--isolate-subject")
}

func TestConfigValidate_negativeCropMargin(t *testing.T) {
	below := -1.0
	_, _, err := (&Config{CropBelow: &below}).Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "crop_below")
}

func TestBuildCLIArgs_cropOmittedWhenUnset(t *testing.T) {
	s := &strokeGenerator{cfg: &Config{}}
	args := s.buildCLIArgs(&generateArgs{PaperWidthMM: 100, PaperHeightMM: 60, MarginMM: 10})
	test.That(t, strings.Join(args, " "), test.ShouldNotContainSubstring, "--crop")
}

func TestBuildCLIArgs_cropFlagsPassedThrough(t *testing.T) {
	crop, above, below, sides := true, 0.55, 0.1, 1.1
	s := &strokeGenerator{cfg: &Config{
		CropFace: &crop, CropAbove: &above, CropBelow: &below, CropSides: &sides,
	}}
	joined := strings.Join(s.buildCLIArgs(&generateArgs{
		PaperWidthMM: 100, PaperHeightMM: 60, MarginMM: 10,
	}), " ")
	test.That(t, joined, test.ShouldContainSubstring, "--crop-face")
	test.That(t, joined, test.ShouldContainSubstring, "--crop-above 0.55")
	test.That(t, joined, test.ShouldContainSubstring, "--crop-below 0.1")
	test.That(t, joined, test.ShouldContainSubstring, "--crop-sides 1.1")
}

func TestBuildCLIArgs_cropFaceDisabledIsExplicit(t *testing.T) {
	crop := false
	s := &strokeGenerator{cfg: &Config{CropFace: &crop}}
	joined := strings.Join(s.buildCLIArgs(&generateArgs{
		PaperWidthMM: 100, PaperHeightMM: 60, MarginMM: 10,
	}), " ")
	test.That(t, joined, test.ShouldContainSubstring, "--no-crop-face")
}

func TestConfigValidate_validFullConfig(t *testing.T) {
	cfg := &Config{
		Size: ptr(768), Clahe: ptr(2.0), Sigma: ptr(2.2),
		Low: ptr(4.0), High: ptr(12.0),
		MaxDepthMM: ptr(1500.0), MinDepthMM: ptr(350.0),
		Prune: 20, MinLen: 36, Smooth: 2.0, MinDist: 3.0,
	}
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldBeNil)
}

func TestConfigValidate_hysteresisOrder(t *testing.T) {
	_, _, err := (&Config{Low: ptr(20.0), High: ptr(5.0)}).Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "must not exceed high")
}

func TestConfigValidate_depthBandOrder(t *testing.T) {
	_, _, err := (&Config{MinDepthMM: ptr(2000.0), MaxDepthMM: ptr(1500.0)}).Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "min_depth_mm")
}

func TestConfigValidate_sizeTooSmall(t *testing.T) {
	_, _, err := (&Config{Size: ptr(32)}).Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "size")
}

func TestBuildCLIArgs_omitsUnsetKnobs(t *testing.T) {
	// Unset (nil) knobs must NOT be passed, so the Python pipeline applies its
	// own defaults — the single source of truth for the tuned values.
	s := &strokeGenerator{cfg: &Config{
		Prune: defaultPrune, MinLen: defaultMinLen,
		Smooth: defaultSmooth, MinDist: defaultMinDist,
	}}
	args := s.buildCLIArgs(&generateArgs{
		PaperWidthMM: 215.9, PaperHeightMM: 279.4, MarginMM: 40.0,
	})
	joined := strings.Join(args, " ")
	test.That(t, argValue(args, "--paper-width-mm"), test.ShouldEqual, "215.9")
	for _, flag := range []string{
		"--size", "--clahe", "--sigma", "--low", "--high",
		"--max-depth-mm", "--min-depth-mm", "--crop-face", "--isolate-subject",
	} {
		test.That(t, joined, test.ShouldNotContainSubstring, flag)
	}
}

func TestBuildCLIArgs_depthBoundsPassedThrough(t *testing.T) {
	s := &strokeGenerator{cfg: &Config{MaxDepthMM: ptr(1500.0), MinDepthMM: ptr(350.0)}}
	joined := strings.Join(s.buildCLIArgs(&generateArgs{
		PaperWidthMM: 215.9, PaperHeightMM: 279.4, MarginMM: 40.0,
	}), " ")
	test.That(t, joined, test.ShouldContainSubstring, "--max-depth-mm 1500")
	test.That(t, joined, test.ShouldContainSubstring, "--min-depth-mm 350")
}

func TestParseGenerateArgs_depthIsOptional(t *testing.T) {
	got, err := parseGenerateArgs(map[string]interface{}{
		"image_b64":       base64.StdEncoding.EncodeToString([]byte("img")),
		"paper_width_mm":  215.9,
		"paper_height_mm": 279.4,
	})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, got.DepthB64, test.ShouldBeEmpty)
}

func TestParseGenerateArgs_depthCarried(t *testing.T) {
	got, err := parseGenerateArgs(map[string]interface{}{
		"image_b64":       base64.StdEncoding.EncodeToString([]byte("img")),
		"depth_b64":       base64.StdEncoding.EncodeToString([]byte("DEPTHMAP...")),
		"paper_width_mm":  215.9,
		"paper_height_mm": 279.4,
	})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, got.DepthB64, test.ShouldNotBeEmpty)
}
