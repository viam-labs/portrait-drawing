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
