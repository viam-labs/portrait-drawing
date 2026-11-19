package drawer

import (
	"math"
	"math/rand"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/test"
)

func TestSurfaceModel_fitsATiltedPlane(t *testing.T) {
	m := newSurfaceModel(280)
	plane := func(x, y float64) float64 { return 280.2 + 0.0006*x - 0.0027*y }
	for _, p := range [][2]float64{{160, -110}, {160, -30}, {290, -110}, {290, -30}, {225, -70}, {200, -90}, {260, -50}, {180, -40}} {
		m.add(p[0], p[1], plane(p[0], p[1]))
	}
	test.That(t, m.fitted, test.ShouldBeTrue)
	test.That(t, m.predict(240, -60), test.ShouldAlmostEqual, plane(240, -60), 1e-9)
}

func TestSurfaceModel_pointsInALineGiveALevelFit(t *testing.T) {
	m := newSurfaceModel(280)
	for i := 0; i < minFitPoints; i++ {
		m.add(200+float64(i), -50, 280.4)
	}
	test.That(t, m.predict(200, 50), test.ShouldAlmostEqual, 280.4, 1e-9)
}

func TestSurfaceModel_unfittedPredictsTheConfiguredHeight(t *testing.T) {
	test.That(t, newSurfaceModel(280.52).predict(1, 2), test.ShouldEqual, 280.52)
}

// card is the probe map from armfarm07: a felt tip on card stiffens at about
// 10 N per mm, and the card tilts 0.27 mm per 100 mm across.
type card struct {
	stiffness float64
	surface   func(x, y float64) float64
}

func probedCard() card {
	return card{stiffness: 10, surface: func(x, y float64) float64 { return 280.2 + 0.0006*(x-226) - 0.0027*(y+68) }}
}

func (c card) force(x, y, z float64) float64 {
	return math.Max(0, c.stiffness*(c.surface(x, y)-z))
}

// draw runs strokes of points 2 mm apart at random places on the card and
// returns the force at every point the pen went to.
func draw(f *forceController, c card, strokes int, rng *rand.Rand) []float64 {
	var forces []float64
	for s := 0; s < strokes; s++ {
		x, y := 160+rng.Float64()*130, -110+rng.Float64()*80
		dx, dy := math.Sincos(rng.Float64() * 2 * math.Pi)
		for p := 0; p < 5; p++ {
			px, py := x+2*float64(p)*dx, y+2*float64(p)*dy
			z := f.height(px, py)
			force := c.force(px, py, z) + 0.1*(rng.Float64()*2-1)
			f.observe(px, py, z, math.Max(0, force))
			forces = append(forces, force)
		}
	}
	return forces
}

func TestForceControl_findsAndHoldsTheTargetOnATiltedCard(t *testing.T) {
	cfg := (&ForceControlConfig{}).withDefaults()
	// The configured height is 0.3 mm above where the pen touches, as on the arm.
	f := newForceController(cfg, newSurfaceModel(280.52), 280.52)
	forces := draw(f, probedCard(), 150, rand.New(rand.NewSource(1))) //nolint:gosec // seeded so the test repeats

	peak, near := 0.0, 0
	settled := forces[100:]
	for _, force := range forces {
		peak = math.Max(peak, force)
	}
	for _, force := range settled {
		if math.Abs(force-cfg.TargetForce) <= 0.5 {
			near++
		}
	}
	t.Logf("peak %.2f N; %d of %d settled points within 0.5 N of target; tilt learned %.2f mm/100mm",
		peak, near, len(settled), 100*f.model.c)
	test.That(t, peak, test.ShouldBeLessThan, 3.0)
	test.That(t, float64(near)/float64(len(settled)), test.ShouldBeGreaterThan, 0.9)
	test.That(t, f.model.fitted, test.ShouldBeTrue)
	test.That(t, 100*f.model.c, test.ShouldAlmostEqual, -0.27, 0.1)
}

func TestForceControl_aLaterDrawingStartsOnTheLearnedSurface(t *testing.T) {
	cfg := (&ForceControlConfig{}).withDefaults()
	model := newSurfaceModel(280.52)
	rng := rand.New(rand.NewSource(2)) //nolint:gosec // seeded so the test repeats
	draw(newForceController(cfg, model, 280.52), probedCard(), 150, rng)

	forces := draw(newForceController(cfg, model, 280.52), probedCard(), 5, rng)
	for _, force := range forces {
		test.That(t, force, test.ShouldBeBetween, 0.3, 2.0)
	}
}

func TestForceControl_neverStraysFurtherThanMaxOffset(t *testing.T) {
	cfg := (&ForceControlConfig{}).withDefaults()
	f := newForceController(cfg, newSurfaceModel(280.5), 280.5)
	// No paper at all: the pen goes looking, but only so far.
	for i := 0; i < 100; i++ {
		z := f.height(200, -50)
		test.That(t, z, test.ShouldBeGreaterThanOrEqualTo, 280.5-cfg.MaxOffsetMM)
		f.observe(200, -50, z, 0)
	}
	test.That(t, f.height(200, -50), test.ShouldEqual, 280.5-cfg.MaxOffsetMM)
}

func TestForceControl_tooMuchForceBacksOffAtMostOneStep(t *testing.T) {
	cfg := (&ForceControlConfig{}).withDefaults()
	f := newForceController(cfg, newSurfaceModel(280.5), 280.5)
	f.observe(200, -50, 280.5, 4.5)
	test.That(t, f.height(200, -50), test.ShouldAlmostEqual, 280.5+cfg.MaxStepMM, 1e-9)
}

func TestConfigValidate_forceControl(t *testing.T) {
	cfg := guardedConfig(validGuard())
	cfg.ForceControl = &ForceControlConfig{}
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldBeNil)

	cfg.ForceControl = &ForceControlConfig{TargetForce: 5}
	_, _, err = cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "below force_guard.max_force")

	cfg.ForceControl = &ForceControlConfig{GainMMPerUnit: -1}
	_, _, err = cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)

	unguarded := guardedConfig(nil)
	unguarded.ForceControl = &ForceControlConfig{}
	_, _, err = unguarded.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "needs force_guard")

	unguarded.ForceControl, unguarded.LineToleranceMM = nil, -0.1
	_, _, err = unguarded.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "line_tolerance_mm")
}

func TestDrawWaypoints_marksTheMovesThatEndOnThePaper(t *testing.T) {
	wps := drawWaypoints([]Polyline{{{0, 0}, {10, 5}, {20, 5}}}, 0, r3.Vector{Z: 50}, r3.Vector{X: 1}, 55, 50, 52)
	var contact []string
	for _, wp := range wps {
		if wp.contact {
			contact = append(contact, wp.label)
		}
	}
	test.That(t, contact, test.ShouldResemble, []string{"polyline 0 pen-down", "polyline 0 point 1", "polyline 0 point 2"})
}

func TestForceRun_endForceIsTheLatestMovesLastReadings(t *testing.T) {
	run := &forceRun{}
	_, ok := run.endForce()
	test.That(t, ok, test.ShouldBeFalse)
	for _, f := range []float64{0, 0, 0.5, 1.0, 1.5} {
		run.record(false, f)
	}
	run.record(true, 9)
	got, ok := run.endForce()
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, got, test.ShouldAlmostEqual, 1.0)
}
