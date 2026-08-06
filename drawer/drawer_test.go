package drawer

import (
	"context"
	"errors"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/robot/framesystem"
	"go.viam.com/rdk/spatialmath"
	"go.viam.com/rdk/testutils/inject"
	"go.viam.com/test"
)

func validCorner() *poseConfig {
	return &poseConfig{
		Translation: r3.Vector{X: 400, Y: 0, Z: 200},
		Orientation: &spatialmath.OrientationConfig{Type: spatialmath.OrientationVectorDegreesType, Value: map[string]any{"th": 180.0, "x": 0.0, "y": 0.0, "z": 1.0}},
	}
}

func TestConfigValidate_missingArm(t *testing.T) {
	cfg := &Config{PaperTopLeftCorner: validCorner(), PaperWidthMM: 100, PaperHeightMM: 60}
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "arm")
}

func TestConfigValidate_missingPaperCorner(t *testing.T) {
	cfg := &Config{Arm: "my-arm", PaperWidthMM: 100, PaperHeightMM: 60}
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "paper_top_left_corner")
}

func TestConfigValidate_missingCornerOrientation(t *testing.T) {
	cfg := &Config{
		Arm:                "my-arm",
		PaperTopLeftCorner: &poseConfig{Translation: r3.Vector{X: 1}},
		PaperWidthMM:       100,
		PaperHeightMM:      60,
	}
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "orientation")
}

func TestConfigValidate_missingPaperWidth(t *testing.T) {
	cfg := &Config{Arm: "my-arm", PaperTopLeftCorner: validCorner(), PaperHeightMM: 60}
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "paper_width_mm")
}

func TestConfigValidate_missingPaperHeight(t *testing.T) {
	cfg := &Config{Arm: "my-arm", PaperTopLeftCorner: validCorner(), PaperWidthMM: 100}
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "paper_height_mm")
}

func TestConfigValidate_negativeLiftOff(t *testing.T) {
	cfg := &Config{
		Arm:                "my-arm",
		PaperTopLeftCorner: validCorner(),
		PaperWidthMM:       100,
		PaperHeightMM:      60,
		LiftOffZMM:         -1,
	}
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "lift_off_z_mm")
}

func TestConfigValidate_homePoseMissingOrientation(t *testing.T) {
	cfg := &Config{
		Arm:                "my-arm",
		PaperTopLeftCorner: validCorner(),
		PaperWidthMM:       100,
		PaperHeightMM:      60,
		HomePose:           &poseConfig{Translation: r3.Vector{X: 1}},
	}
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "home_pose.orientation")
}

func TestConfigValidate_valid(t *testing.T) {
	cfg := &Config{
		Arm:                "my-arm",
		PaperTopLeftCorner: validCorner(),
		PaperWidthMM:       100,
		PaperHeightMM:      60,
		LiftOffZMM:         5.0,
		InputRangeOverride: map[string]map[string]referenceframe.Limit{
			"my-arm": {"joint_0": {Min: -1, Max: 1}},
		},
	}
	deps, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, deps, test.ShouldResemble, []string{"my-arm"})
}

func TestConfigValidate_strokeGeneratorInDeps(t *testing.T) {
	cfg := &Config{
		Arm:                "my-arm",
		PaperTopLeftCorner: validCorner(),
		PaperWidthMM:       100,
		PaperHeightMM:      60,
		StrokeGenerator:    "my-generator",
	}
	deps, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, deps, test.ShouldResemble, []string{"my-arm", "my-generator"})
}

func TestParseDrawPayload_valid(t *testing.T) {
	payload := map[string]interface{}{
		"polylines": []interface{}{
			[]interface{}{[]interface{}{0.0, 0.0}, []interface{}{10.0, 5.0}},
			[]interface{}{[]interface{}{20.0, 20.0}},
		},
	}
	got, err := parseDrawPayload(payload)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(got), test.ShouldEqual, 2)
	test.That(t, len(got[0]), test.ShouldEqual, 2)
	test.That(t, got[0][0], test.ShouldResemble, [2]float64{0, 0})
	test.That(t, got[0][1], test.ShouldResemble, [2]float64{10, 5})
	test.That(t, got[1][0], test.ShouldResemble, [2]float64{20, 20})
}

func TestParseDrawPayload_missingPolylines(t *testing.T) {
	_, err := parseDrawPayload(map[string]interface{}{})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "polylines")
}

func TestParseDrawPayload_emptyPolylines(t *testing.T) {
	payload := map[string]interface{}{"polylines": []interface{}{}}
	_, err := parseDrawPayload(payload)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "polylines")
}

func TestParseDrawPayload_emptyInnerPolyline(t *testing.T) {
	payload := map[string]interface{}{
		"polylines": []interface{}{
			[]interface{}{[]interface{}{0.0, 0.0}},
			[]interface{}{},
		},
	}
	_, err := parseDrawPayload(payload)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "empty")
}

func TestParseDrawPayload_wrongPointArity(t *testing.T) {
	payload := map[string]interface{}{
		"polylines": []interface{}{
			[]interface{}{[]interface{}{0.0, 0.0, 5.0}},
		},
	}
	_, err := parseDrawPayload(payload)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "2 elements")
}

func TestParseDrawPayload_nonNumeric(t *testing.T) {
	payload := map[string]interface{}{
		"polylines": []interface{}{
			[]interface{}{[]interface{}{"not a number", 0.0}},
		},
	}
	_, err := parseDrawPayload(payload)
	test.That(t, err, test.ShouldNotBeNil)
}

func TestParseDrawImageArgs_valid(t *testing.T) {
	payload := map[string]interface{}{
		"image_b64": "abc",
		"margin_mm": 20.0,
		"rotate":    90,
		"mirror":    true,
	}
	got, err := parseDrawImageArgs(payload)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, got.ImageB64, test.ShouldEqual, "abc")
	test.That(t, got.MarginMM, test.ShouldEqual, 20.0)
	test.That(t, got.Rotate, test.ShouldEqual, 90)
	test.That(t, got.Mirror, test.ShouldBeTrue)
}

func TestParseDrawImageArgs_missingImage(t *testing.T) {
	_, err := parseDrawImageArgs(map[string]interface{}{"margin_mm": 20.0})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "image_b64")
}

func TestParseDrawImageArgs_negativeMargin(t *testing.T) {
	_, err := parseDrawImageArgs(map[string]interface{}{
		"image_b64": "abc",
		"margin_mm": -1.0,
	})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "margin_mm")
}

func TestParseDrawImageArgs_badRotate(t *testing.T) {
	_, err := parseDrawImageArgs(map[string]interface{}{
		"image_b64": "abc",
		"rotate":    45,
	})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "rotate")
}

func TestDrawImage_missingGenerator(t *testing.T) {
	d := &drawer{
		logger: logging.NewTestLogger(t),
		cfg:    &Config{},
	}
	_, err := d.drawImage(context.Background(), map[string]interface{}{"image_b64": "abc"})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "stroke_generator")
}

func TestBuildFrameSystemUsesService(t *testing.T) {
	fsSvc := inject.NewFrameSystemService("test")
	fsSvc.FrameSystemConfigFunc = func(_ context.Context) (*framesystem.Config, error) {
		return &framesystem.Config{Parts: nil}, nil
	}
	d := &drawer{
		logger:    logging.NewTestLogger(t),
		cfg:       &Config{},
		fsService: fsSvc,
	}
	fs, err := d.buildFrameSystem(context.Background())
	test.That(t, err, test.ShouldBeNil)
	test.That(t, fs, test.ShouldNotBeNil)
}

func TestBuildFrameSystemPropagatesServiceError(t *testing.T) {
	fsSvc := inject.NewFrameSystemService("test")
	fsSvc.FrameSystemConfigFunc = func(_ context.Context) (*framesystem.Config, error) {
		return nil, errors.New("boom")
	}
	d := &drawer{
		logger:    logging.NewTestLogger(t),
		cfg:       &Config{},
		fsService: fsSvc,
	}
	_, err := d.buildFrameSystem(context.Background())
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "build frame system")
	test.That(t, err.Error(), test.ShouldContainSubstring, "boom")
}

func TestBuildFrameSystemAppliesInputRangeOverride(t *testing.T) {
	fsSvc := inject.NewFrameSystemService("test")
	fsSvc.FrameSystemConfigFunc = func(_ context.Context) (*framesystem.Config, error) {
		return &framesystem.Config{Parts: nil}, nil
	}
	d := &drawer{
		logger: logging.NewTestLogger(t),
		cfg: &Config{
			InputRangeOverride: map[string]map[string]referenceframe.Limit{
				"nonexistent-arm": {"0": {Min: -1, Max: 1}},
			},
		},
		fsService: fsSvc,
	}
	_, err := d.buildFrameSystem(context.Background())
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "input_range_override")
}
