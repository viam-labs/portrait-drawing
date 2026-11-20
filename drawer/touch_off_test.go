package drawer

import (
	"math"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/spatialmath"
	"go.viam.com/test"
)

func TestContactZ_interpolatesBetweenSteps(t *testing.T) {
	// Stepping 0.2 mm into a 10 N/mm contact that starts at 280.13.
	samples := []touchSample{{281.0, 0}, {280.8, 0.02}, {280.6, 0.03}, {280.4, 0.01}, {280.2, 0.0}, {280.0, 1.3}, {279.8, 3.3}}
	z, ok := contactZ(samples, 1.5)
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, z, test.ShouldAlmostEqual, 279.98)

	_, ok = contactZ(samples[:5], 1.5)
	test.That(t, ok, test.ShouldBeFalse)
}

func TestPlaneThrough_recoversATilt(t *testing.T) {
	plane, err := planeThrough([3]r3.Vector{{X: 160, Y: -110, Z: 280.0}, {X: 290, Y: -110, Z: 280.078}, {X: 225, Y: -30, Z: 280.0 + 0.039 - 0.216}})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, plane.bx, test.ShouldAlmostEqual, 0.0006)
	test.That(t, plane.by, test.ShouldAlmostEqual, -0.0027)
	test.That(t, plane.z(225, -70), test.ShouldAlmostEqual, 280.0+0.039-0.108)
}

func TestPlaneThrough_refusesPointsInALine(t *testing.T) {
	_, err := planeThrough([3]r3.Vector{{X: 0, Y: 0, Z: 280}, {X: 10, Y: 10, Z: 280}, {X: 20, Y: 20, Z: 280.1}})
	test.That(t, err, test.ShouldNotBeNil)
}

func TestTouchPointsFor_spansTheDrawing(t *testing.T) {
	pts := touchPointsFor([]Polyline{{{10, 20}, {90, 25}}, {{40, 140}, {50, 130}}}, 0)
	test.That(t, pts[:], test.ShouldContain, [2]float64{10, 20})
	test.That(t, pts[:], test.ShouldContain, [2]float64{90, 25})
	test.That(t, pts[:], test.ShouldContain, [2]float64{40, 140})
}

func TestTouchPointsFor_touchesTheSheetsNotTheGapsBetween(t *testing.T) {
	// Crosses on two cards 200 mm apart: a box around them would touch the mat.
	cross := func(x, y float64) []Polyline {
		return []Polyline{{{x - 5, y}, {x + 5, y}}, {{x, y - 5}, {x, y + 5}}}
	}
	var polys []Polyline
	polys = append(polys, cross(0, 0)...)
	polys = append(polys, cross(200, 0)...)
	polys = append(polys, cross(100, 150)...)
	for _, p := range touchPointsFor(polys, 0) {
		near := false
		for _, c := range [][2]float64{{0, 0}, {200, 0}, {100, 150}} {
			near = near || math.Hypot(p[0]-c[0], p[1]-c[1]) <= 5
		}
		test.That(t, near, test.ShouldBeTrue)
	}
}

func TestTouchPointsFor_aTinyDrawingStillSpreadsTheTouches(t *testing.T) {
	pts := touchPointsFor([]Polyline{{{40, 30}, {44, 31}}}, 0)
	test.That(t, pts[1][0]-pts[0][0], test.ShouldEqual, minTouchSpreadMM)
	test.That(t, pts[2][1]-pts[0][1], test.ShouldEqual, minTouchSpreadMM)
}

func TestTouchPointsFor_onlyWhatIsLeftToDraw(t *testing.T) {
	pts := touchPointsFor([]Polyline{{{0, 0}, {100, 150}}, {{30, 30}, {70, 80}}}, 1)
	test.That(t, pts[0], test.ShouldResemble, [2]float64{30, 30})
}

func TestOnPlane_keepsEachHeightAboveThePaper(t *testing.T) {
	wps := drawWaypoints([]Polyline{{{0, 0}, {100, 0}}}, 0, r3.Vector{X: 150, Y: -20, Z: 280.5}, r3.Vector{X: 1}, 284.5, 280.5, 282.5)
	plane := &touchPlane{x0: 150, y0: -20, z0: 280.1, bx: 0.003}
	onPlane(wps, plane, 280.5, 0.2)
	// approach, descent, pen-down, point 1, pen-up: only the pen-down ones press
	test.That(t, wps[0].z, test.ShouldAlmostEqual, 284.1)
	test.That(t, wps[1].z, test.ShouldAlmostEqual, 282.1)
	test.That(t, wps[2].z, test.ShouldAlmostEqual, 279.9)
	test.That(t, wps[3].z, test.ShouldAlmostEqual, 280.2)
	test.That(t, wps[4].z, test.ShouldAlmostEqual, 284.4)
}

func TestConfigValidate_touchOff(t *testing.T) {
	cfg := guardedConfig(validGuard())
	cfg.TouchOff = &TouchOffConfig{}
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldBeNil)

	cfg.TouchOff = &TouchOffConfig{Force: 5}
	_, _, err = cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "below force_guard.max_force")

	cfg.TouchOff = &TouchOffConfig{StepMM: -0.1}
	_, _, err = cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)

	unguarded := guardedConfig(nil)
	unguarded.TouchOff = &TouchOffConfig{}
	_, _, err = unguarded.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "needs force_guard")
}

func TestTouchedPaper_isReusedOnlyForTheSameSheet(t *testing.T) {
	at := func(x float64) surface {
		return surface{corner: spatialmath.NewPoseFromPoint(r3.Vector{X: x, Y: -118.94, Z: 280.52}), across: r3.Vector{Y: 1}}
	}
	touched := &touchedPaper{corner: r3.Vector{X: 149.71, Y: -118.94, Z: 280.52}, across: r3.Vector{Y: 1}}
	test.That(t, touched.same(at(149.71)), test.ShouldBeTrue)
	test.That(t, touched.same(at(152)), test.ShouldBeFalse)
	var none *touchedPaper
	test.That(t, none.same(at(149.71)), test.ShouldBeFalse)
}

func TestTouchOffConfig_pressDefaultsButZeroIsAllowed(t *testing.T) {
	test.That(t, (&TouchOffConfig{}).withDefaults().pressMM(), test.ShouldEqual, defaultPressMM)
	zero := 0.0
	test.That(t, (&TouchOffConfig{PressMM: &zero}).withDefaults().pressMM(), test.ShouldEqual, 0.0)
	neg := -0.1
	cfg := guardedConfig(validGuard())
	cfg.TouchOff = &TouchOffConfig{PressMM: &neg}
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
}
