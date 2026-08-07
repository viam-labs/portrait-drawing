package strokegenerator

import (
	"encoding/base64"
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

func TestConfigValidate_negativeDetail(t *testing.T) {
	cfg := &Config{Detail: ptr(-1.0)}
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "detail")
}

func TestConfigValidate_negativeFaceDetail(t *testing.T) {
	cfg := &Config{FaceDetail: ptr(-1.0)}
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "face_detail")
}

func TestConfigValidate_zeroDetailAllowed(t *testing.T) {
	// 0 is a meaningful value (use fixed low/high) and must pass validation.
	cfg := &Config{Detail: ptr(0.0), FaceDetail: ptr(0.0)}
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldBeNil)
}

func TestConfigValidate_lowOutOfRange(t *testing.T) {
	cfg := &Config{Low: 300}
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "low")
}

func TestConfigValidate_validFullConfig(t *testing.T) {
	cfg := &Config{
		Region: 30, Low: 50, High: 150, Merge: 3, Prune: 20,
		MinLen: 100, Smooth: 2.0, MinDist: 10.0,
	}
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldBeNil)
}

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

func TestBuildCLIArgs_omitsUnsetFaceKnobs(t *testing.T) {
	// Unset (nil) face-aware knobs must NOT be passed, so the Python pipeline
	// applies its own defaults (the single source of truth for tuned values).
	s := &strokeGenerator{cfg: &Config{
		Region: defaultRegion, Low: defaultLow, High: defaultHigh,
		Merge: defaultMerge, Prune: defaultPrune, MinLen: defaultMinLen,
		Smooth: defaultSmooth, MinDist: defaultMinDist,
	}}
	args := s.buildCLIArgs(&generateArgs{
		PaperWidthMM: 215.9, PaperHeightMM: 279.4, MarginMM: 40.0, Rotate: 0, Mirror: false,
	})
	test.That(t, argValue(args, "--paper-width-mm"), test.ShouldEqual, "215.9")
	test.That(t, argValue(args, "--region"), test.ShouldEqual, "10")
	test.That(t, args, test.ShouldNotContain, "--detail")
	test.That(t, args, test.ShouldNotContain, "--face-detail")
	test.That(t, args, test.ShouldNotContain, "--face-size-px")
	test.That(t, args, test.ShouldNotContain, "--isolate-subject")
	test.That(t, args, test.ShouldNotContain, "--no-isolate-subject")
	test.That(t, args, test.ShouldNotContain, "--mirror")
}

func TestBuildCLIArgs_passesSetFaceKnobs(t *testing.T) {
	s := &strokeGenerator{cfg: &Config{
		Detail: ptr(2.0), FaceDetail: ptr(0.0), FaceSizePx: ptr(400),
		IsolateSubject: ptr(true),
	}}
	args := s.buildCLIArgs(&generateArgs{
		PaperWidthMM: 100, PaperHeightMM: 100, MarginMM: 10, Rotate: 0, Mirror: false,
	})
	test.That(t, argValue(args, "--detail"), test.ShouldEqual, "2")
	test.That(t, argValue(args, "--face-detail"), test.ShouldEqual, "0")
	test.That(t, argValue(args, "--face-size-px"), test.ShouldEqual, "400")
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
