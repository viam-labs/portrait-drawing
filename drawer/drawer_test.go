package drawer

import (
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/test"
)

func TestConfigValidate_missingArm(t *testing.T) {
	cfg := &Config{PaperTopLeftCorner: &r3.Vector{}, PaperWidthMM: 100, PaperHeightMM: 60}
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

func TestConfigValidate_missingPaperWidth(t *testing.T) {
	cfg := &Config{Arm: "my-arm", PaperTopLeftCorner: &r3.Vector{}, PaperHeightMM: 60}
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "paper_width_mm")
}

func TestConfigValidate_missingPaperHeight(t *testing.T) {
	cfg := &Config{Arm: "my-arm", PaperTopLeftCorner: &r3.Vector{}, PaperWidthMM: 100}
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "paper_height_mm")
}

func TestConfigValidate_negativeLiftOff(t *testing.T) {
	cfg := &Config{
		Arm:                "my-arm",
		PaperTopLeftCorner: &r3.Vector{X: 1, Y: 2, Z: 3},
		PaperWidthMM:       100,
		PaperHeightMM:      60,
		LiftOffZMM:         -1,
	}
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "lift_off_z_mm")
}

func TestConfigValidate_valid(t *testing.T) {
	cfg := &Config{
		Arm:                "my-arm",
		PaperTopLeftCorner: &r3.Vector{X: 400, Y: 0, Z: 200},
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
