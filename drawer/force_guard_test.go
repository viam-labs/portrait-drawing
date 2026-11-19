package drawer

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/testutils/inject"
	"go.viam.com/test"
)

func guardedConfig(guard *ForceGuardConfig) *Config {
	return &Config{
		Arm:                "my-arm",
		PaperTopLeftCorner: validCorner(),
		PaperWidthMM:       100,
		PaperHeightMM:      60,
		ForceGuard:         guard,
	}
}

func validGuard() *ForceGuardConfig {
	return &ForceGuardConfig{Sensor: "force-1", Reading: "Fz", MaxForce: 5, ContactForce: 1}
}

func TestConfigValidate_forceGuardSensorInDeps(t *testing.T) {
	deps, _, err := guardedConfig(validGuard()).Validate("")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, deps, test.ShouldResemble, []string{"my-arm", "force-1"})
}

func TestConfigValidate_forceGuardRejects(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*ForceGuardConfig)
		want   string
	}{
		"no sensor":             {func(g *ForceGuardConfig) { g.Sensor = "" }, "force_guard.sensor"},
		"no reading":            {func(g *ForceGuardConfig) { g.Reading = "" }, "force_guard.reading"},
		"no max":                {func(g *ForceGuardConfig) { g.MaxForce = 0 }, "max_force"},
		"no contact force":      {func(g *ForceGuardConfig) { g.ContactForce = 0 }, "contact_force"},
		"contact above max":     {func(g *ForceGuardConfig) { g.ContactForce = 6 }, "contact_force"},
		"negative margin":       {func(g *ForceGuardConfig) { g.ContactMarginMM = -1 }, "contact_margin_mm"},
		"margin up to lift-off": {func(g *ForceGuardConfig) { g.ContactMarginMM = defaultLiftOffZMM }, "lift_off_z_mm"},
	} {
		t.Run(name, func(t *testing.T) {
			guard := validGuard()
			tc.mutate(guard)
			_, _, err := guardedConfig(guard).Validate("")
			test.That(t, err, test.ShouldNotBeNil)
			test.That(t, err.Error(), test.ShouldContainSubstring, tc.want)
		})
	}
}

func TestDrawWaypoints_splitsDescentAtTheContactMargin(t *testing.T) {
	wps := drawWaypoints([]Polyline{{{0, 0}, {10, 5}}}, 0, r3.Vector{Z: 50}, r3.Vector{X: 1}, 55, 50, 52)
	test.That(t, len(wps), test.ShouldEqual, 5)
	test.That(t, wps[0].clear, test.ShouldBeTrue)
	test.That(t, wps[1].z, test.ShouldEqual, 52.0)
	test.That(t, wps[1].clear, test.ShouldBeTrue)
	test.That(t, wps[1].linear, test.ShouldBeTrue)
	for _, wp := range wps[2:] {
		test.That(t, wp.clear, test.ShouldBeFalse)
	}
	test.That(t, wps[2].z, test.ShouldEqual, 50.0)
}

func TestDrawWaypoints_noContactCheckWithoutAMargin(t *testing.T) {
	wps := drawWaypoints([]Polyline{{{0, 0}, {10, 5}}}, 0, r3.Vector{Z: 50}, r3.Vector{X: 1}, 55, 50, 50)
	test.That(t, len(wps), test.ShouldEqual, 4)
	for _, wp := range wps {
		test.That(t, wp.clear, test.ShouldBeFalse)
	}
}

func guardedDrawer(t *testing.T, force func() (interface{}, error)) (*drawer, *atomic.Int32) {
	t.Helper()
	s := inject.NewSensor("force-1")
	s.ReadingsFunc = func(context.Context, map[string]interface{}) (map[string]interface{}, error) {
		v, err := force()
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"Fz": v}, nil
	}
	stops := &atomic.Int32{}
	a := inject.NewArm("my-arm")
	a.StopFunc = func(context.Context, map[string]interface{}) error {
		stops.Add(1)
		return nil
	}
	return &drawer{
		logger:     logging.NewTestLogger(t),
		cfg:        &Config{},
		arm:        a,
		forceGuard: &forceGuard{sensor: s, reading: "Fz", max: 5, contact: 1},
	}, stops
}

func untilCanceled(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestWatchForce_normalForceLetsTheMoveFinish(t *testing.T) {
	d, stops := guardedDrawer(t, func() (interface{}, error) { return 3.0, nil })
	err := d.watchForce(context.Background(), &forceRun{}, false, func(context.Context) error { return nil })
	test.That(t, err, test.ShouldBeNil)
	test.That(t, stops.Load(), test.ShouldEqual, 0)
}

func TestWatchForce_excessForceStopsTheArm(t *testing.T) {
	d, stops := guardedDrawer(t, func() (interface{}, error) { return -7.5, nil })
	err := d.watchForce(context.Background(), &forceRun{}, false, untilCanceled)
	test.That(t, faultOf(err), test.ShouldEqual, faultExcessForce)
	test.That(t, err.Error(), test.ShouldContainSubstring, "cap")
	test.That(t, stops.Load(), test.ShouldEqual, 1)
}

func TestWatchForce_clearMoveUsesTheContactLimit(t *testing.T) {
	d, _ := guardedDrawer(t, func() (interface{}, error) { return 3.0, nil })
	err := d.watchForce(context.Background(), &forceRun{}, true, untilCanceled)
	test.That(t, faultOf(err), test.ShouldEqual, faultExcessForce)
	test.That(t, err.Error(), test.ShouldContainSubstring, "before the pen")
}

func TestWatchForce_measuresFromTheBaseline(t *testing.T) {
	// A wrist sensor carrying the tool reads far from zero with nothing touching.
	d, stops := guardedDrawer(t, func() (interface{}, error) { return -18.4, nil })
	err := d.watchForce(context.Background(), &forceRun{baseline: -18.0}, true, func(context.Context) error { return nil })
	test.That(t, err, test.ShouldBeNil)
	test.That(t, stops.Load(), test.ShouldEqual, 0)

	err = d.watchForce(context.Background(), &forceRun{baseline: -12.0}, false, untilCanceled)
	test.That(t, faultOf(err), test.ShouldEqual, faultExcessForce)
}

func TestForceRun_keepsLiftedAndPenDownApart(t *testing.T) {
	var reads atomic.Int32
	d, _ := guardedDrawer(t, func() (interface{}, error) {
		reads.Add(1)
		return -18.5, nil
	})
	run := &forceRun{baseline: -18.0}
	// A second read starting means the first sample is already recorded.
	untilSampled := func(context.Context) error {
		for reads.Load() < 2 {
			time.Sleep(time.Millisecond)
		}
		return nil
	}
	test.That(t, d.watchForce(context.Background(), run, true, untilSampled), test.ShouldBeNil)
	test.That(t, run.penDown.samples, test.ShouldEqual, 0)
	test.That(t, run.lifted.samples, test.ShouldBeGreaterThan, 0)
	test.That(t, run.lifted.mean(), test.ShouldAlmostEqual, 0.5, 1e-9)
	test.That(t, run.lifted.peak, test.ShouldAlmostEqual, 0.5, 1e-9)

	run.record(false, 2)
	run.record(false, 4)
	test.That(t, run.penDown.mean(), test.ShouldAlmostEqual, 3.0, 1e-9)
	test.That(t, run.penDown.peak, test.ShouldEqual, 4.0)
}

func TestStatus_reportsTheLastForceRun(t *testing.T) {
	d, _ := guardedDrawer(t, func() (interface{}, error) { return 0.0, nil })
	_, has := d.status()["last_force"]
	test.That(t, has, test.ShouldBeFalse)

	d.finishForceRun(&forceRun{})
	_, has = d.status()["last_force"]
	test.That(t, has, test.ShouldBeFalse)

	run := &forceRun{baseline: -18}
	run.record(false, 2)
	run.record(false, 4)
	d.finishForceRun(run)
	force, ok := d.status()["last_force"].(map[string]interface{})
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, force["baseline"], test.ShouldEqual, -18.0)
	penDown, ok := force["pen_down"].(map[string]interface{})
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, penDown["mean"], test.ShouldEqual, 3.0)
	test.That(t, penDown["peak"], test.ShouldEqual, 4.0)
	test.That(t, penDown["samples"], test.ShouldEqual, 2)
}

func TestMeasureBaseline_averagesTheSamples(t *testing.T) {
	var n atomic.Int32
	d, _ := guardedDrawer(t, func() (interface{}, error) {
		if n.Add(1)%2 == 0 {
			return -18.2, nil
		}
		return -17.8, nil
	})
	baseline, err := d.forceGuard.measureBaseline(context.Background())
	test.That(t, err, test.ShouldBeNil)
	test.That(t, baseline, test.ShouldAlmostEqual, -18.0, 1e-9)
	test.That(t, n.Load(), test.ShouldEqual, baselineSamples)
}

func TestMeasureBaseline_refusesAFrozenSensor(t *testing.T) {
	d, _ := guardedDrawer(t, func() (interface{}, error) { return 0.0, nil })
	_, err := d.forceGuard.measureBaseline(context.Background())
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "not streaming")
}

func TestMeasureBaseline_propagatesReadErrors(t *testing.T) {
	d, _ := guardedDrawer(t, func() (interface{}, error) { return nil, errors.New("unplugged") })
	_, err := d.forceGuard.measureBaseline(context.Background())
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "unplugged")
}

func TestWatchForce_unreadableSensorStopsTheMove(t *testing.T) {
	d, stops := guardedDrawer(t, func() (interface{}, error) { return nil, errors.New("unplugged") })
	err := d.watchForce(context.Background(), &forceRun{}, false, untilCanceled)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "unplugged")
	test.That(t, faultOf(err), test.ShouldEqual, "")
	test.That(t, stops.Load(), test.ShouldEqual, 1)
}

func TestWatchForce_nonNumericReadingStopsTheMove(t *testing.T) {
	d, _ := guardedDrawer(t, func() (interface{}, error) { return "high", nil })
	err := d.watchForce(context.Background(), &forceRun{}, false, untilCanceled)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "not a number")
}

func TestWatchForce_withoutAGuardJustMoves(t *testing.T) {
	d := &drawer{logger: logging.NewTestLogger(t), cfg: &Config{}}
	moved := false
	err := d.watchForce(context.Background(), &forceRun{}, true, func(context.Context) error {
		moved = true
		return nil
	})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, moved, test.ShouldBeTrue)
}

func TestStatus_excessForceIsAFaultUntilDrawingRestarts(t *testing.T) {
	d := &drawer{logger: logging.NewTestLogger(t), cfg: &Config{}}
	d.setFailed(&excessForceError{force: 7, limit: 5})
	s := d.status()
	test.That(t, s["state"], test.ShouldEqual, phaseFailed)
	test.That(t, s["fault"], test.ShouldEqual, faultExcessForce)
	d.startDrawing(10, 100)
	_, hasFault := d.status()["fault"]
	test.That(t, hasFault, test.ShouldBeFalse)
}

func TestWatchForce_aFrozenSensorStopsTheMove(t *testing.T) {
	// Plausible, in range, and exactly the same every time: a stream that stopped.
	d, stops := guardedDrawer(t, func() (interface{}, error) { return 0.3, nil })
	err := d.watchForce(context.Background(), &forceRun{}, false, untilCanceled)
	var frozen *frozenForceError
	test.That(t, errors.As(err, &frozen), test.ShouldBeTrue)
	test.That(t, err.Error(), test.ShouldContainSubstring, "stopped updating")
	test.That(t, stops.Load(), test.ShouldEqual, 1)
	test.That(t, guardStopped(err), test.ShouldBeTrue)
	test.That(t, faultOf(err), test.ShouldEqual, "")
}

func TestWatchForce_aLiveSensorIsNotFrozen(t *testing.T) {
	var n atomic.Int32
	d, _ := guardedDrawer(t, func() (interface{}, error) { return 0.3 + 0.001*float64(n.Add(1)%3), nil })
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(2*frozenPolls)*forcePollInterval)
	defer cancel()
	err := d.watchForce(ctx, &forceRun{}, false, untilCanceled)
	test.That(t, errors.Is(err, context.DeadlineExceeded), test.ShouldBeTrue)
}

func TestForceRun_countsRepeatsAcrossMoves(t *testing.T) {
	run := &forceRun{}
	for i := 0; i < frozenPolls-1; i++ {
		run.noteRaw(-12.5)
	}
	test.That(t, run.repeats, test.ShouldEqual, frozenPolls-1)
	run.noteRaw(-12.5)
	test.That(t, run.repeats, test.ShouldEqual, frozenPolls)
	run.noteRaw(-12.4)
	test.That(t, run.repeats, test.ShouldEqual, 1)
}

func TestConfigValidate_retreatPoseInDeps(t *testing.T) {
	guard := validGuard()
	guard.RetreatPose = "mat-view"
	deps, _, err := guardedConfig(guard).Validate("")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, deps, test.ShouldResemble, []string{"my-arm", "force-1", "mat-view"})
}

type fakePositioner struct{ positions []uint32 }

func (f *fakePositioner) SetPosition(_ context.Context, p uint32, _ map[string]interface{}) error {
	f.positions = append(f.positions, p)
	return nil
}

func TestPark_goesToTheRetreatPose(t *testing.T) {
	d, _ := guardedDrawer(t, func() (interface{}, error) { return 0.0, nil })
	pos := &fakePositioner{}
	d.forceGuard.retreat = pos
	d.cfg = &Config{ForceGuard: &ForceGuardConfig{RetreatPose: "mat-view"}}
	test.That(t, d.park(context.Background(), nil), test.ShouldBeNil)
	test.That(t, pos.positions, test.ShouldResemble, []uint32{retreatPosition})
}
