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

func TestBuildCLIArgs_defaults(t *testing.T) {
	s := &strokeGenerator{cfg: &Config{
		Region: defaultRegion, Low: defaultLow, High: defaultHigh,
		Merge: defaultMerge, Prune: defaultPrune, MinLen: defaultMinLen,
		Smooth: defaultSmooth, MinDist: defaultMinDist,
	}}
	args := s.buildCLIArgs(&generateArgs{
		PaperWidthMM: 215.9, PaperHeightMM: 279.4, MarginMM: 40.0, Rotate: 0, Mirror: false,
	})
	test.That(t, args, test.ShouldContain, "--paper-width-mm")
	test.That(t, args, test.ShouldContain, "215.9")
	test.That(t, args, test.ShouldContain, "--rotate")
	test.That(t, args, test.ShouldContain, "0")
	test.That(t, args, test.ShouldContain, "--region")
	test.That(t, args, test.ShouldContain, "25")
	test.That(t, args, test.ShouldNotContain, "--mirror")
}

func TestBuildCLIArgs_mirrorAppended(t *testing.T) {
	s := &strokeGenerator{cfg: &Config{}}
	args := s.buildCLIArgs(&generateArgs{
		PaperWidthMM: 100, PaperHeightMM: 100, MarginMM: 10, Rotate: 90, Mirror: true,
	})
	test.That(t, args, test.ShouldContain, "--mirror")
}
