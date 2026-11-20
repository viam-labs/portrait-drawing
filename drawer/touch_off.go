package drawer

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/spatialmath"
)

const (
	defaultTouchForce        = 1.5
	defaultTouchStartAboveMM = 1.0
	defaultTouchStepMM       = 0.2
	defaultTouchFineStepMM   = 0.05
	// firstTouchForce is where stepping turns fine: past it, one coarse step
	// into a stiff surface, a sticker on its backing say, can jump past max_force.
	firstTouchForce        = 0.3
	defaultTouchMaxDepthMM = 1.5
	// defaultPressMM is how far below the touched plane the pen draws. A card is
	// not flat: a probe map on armfarm07 put it 0.13 mm off its best plane on
	// average and 0.2 mm at worst, and drawn on the plane itself, a rigid pen
	// floated wherever the card dipped.
	defaultPressMM = 0.2
	// touchSettle lets the arm stop ringing before a step's force is read: a
	// reading taken while it still moves is the arm, not the paper.
	touchSettle        = 100 * time.Millisecond
	touchReadings      = 3
	minTouchSpreadMM   = 20.0
	maxTouchTiltPer1mm = 0.01
)

// TouchOffConfig finds the paper before each drawing by touching the pen down
// on it at three points with the arm still, where the force sensor is precise,
// and draws on the plane through them. A rigid pen has a few tenths of a
// millimetre between not touching and pressing hard, less than a sheet's tilt
// across its width. It needs force_guard, whose sensor it reads.
type TouchOffConfig struct {
	// Force is what counts as touching, over a still reading 1 mm up. The plane
	// passes through where each touch reached it, so it is also roughly what
	// the pen draws with.
	Force float64 `json:"force,omitempty"`
	// StartAboveMM is how far above the paper's configured height each touch
	// starts stepping down.
	StartAboveMM float64 `json:"start_above_mm,omitempty"`
	// StepMM is the step down until the pen first feels the paper, FineStepMM
	// the step after.
	StepMM     float64 `json:"step_mm,omitempty"`
	FineStepMM float64 `json:"fine_step_mm,omitempty"`
	// MaxDepthMM is how far below the configured height a touch may go before
	// it gives up: no paper there.
	MaxDepthMM float64 `json:"max_depth_mm,omitempty"`
	// PressMM is how far below the touched plane the pen draws, so it stays on
	// the paper where the sheet dips. Each 0.1 mm presses about 1 N more with a
	// felt tip on card.
	PressMM *float64 `json:"press_mm,omitempty"`
}

func (c TouchOffConfig) pressMM() float64 {
	if c.PressMM == nil {
		return defaultPressMM
	}
	return *c.PressMM
}

func (c *TouchOffConfig) withDefaults() TouchOffConfig {
	out := *c
	def := func(v *float64, d float64) {
		if *v == 0 {
			*v = d
		}
	}
	def(&out.Force, defaultTouchForce)
	def(&out.StartAboveMM, defaultTouchStartAboveMM)
	def(&out.StepMM, defaultTouchStepMM)
	def(&out.FineStepMM, defaultTouchFineStepMM)
	def(&out.MaxDepthMM, defaultTouchMaxDepthMM)
	return out
}

func (c *TouchOffConfig) validate(guard *ForceGuardConfig) error {
	if guard == nil {
		return errors.New("touch_off needs force_guard, whose sensor it reads")
	}
	for name, v := range map[string]float64{
		"force": c.Force, "start_above_mm": c.StartAboveMM, "step_mm": c.StepMM, "fine_step_mm": c.FineStepMM,
		"max_depth_mm": c.MaxDepthMM, "press_mm": c.pressMM(),
	} {
		if v < 0 {
			return fmt.Errorf("touch_off.%s must be >= 0 (0 uses the default), got %g", name, v)
		}
	}
	if force := c.withDefaults().Force; force >= guard.MaxForce {
		return fmt.Errorf("touch_off.force (%g) must be below force_guard.max_force (%g)", force, guard.MaxForce)
	}
	return nil
}

// touchPlane is z = z0 + bx*(x-x0) + by*(y-y0) in world mm.
type touchPlane struct {
	x0, y0, z0, bx, by float64
	points             [3]r3.Vector
}

func (p *touchPlane) z(x, y float64) float64 {
	return p.z0 + p.bx*(x-p.x0) + p.by*(y-p.y0)
}

// tilt is the plane's steepest slope, in mm per mm.
func (p *touchPlane) tilt() float64 {
	return math.Hypot(p.bx, p.by)
}

func planeThrough(pts [3]r3.Vector) (*touchPlane, error) {
	u, v := pts[1].Sub(pts[0]), pts[2].Sub(pts[0])
	n := u.Cross(v)
	if math.Abs(n.Z) < 1e-6*u.Norm()*v.Norm() {
		return nil, errors.New("touch-off points lie in a line, so they fix no plane")
	}
	return &touchPlane{x0: pts[0].X, y0: pts[0].Y, z0: pts[0].Z, bx: -n.X / n.Z, by: -n.Y / n.Z, points: pts}, nil
}

type touchSample struct{ z, force float64 }

// contactZ is where the force first reached force, interpolated between the two
// steps either side of it: snapping to a step would put each touch up to a step
// off, by a different amount at each point, and the plane would read that as tilt.
func contactZ(samples []touchSample, force float64) (float64, bool) {
	for i := 1; i < len(samples); i++ {
		prev, cur := samples[i-1], samples[i]
		if cur.force < force {
			continue
		}
		if cur.force == prev.force {
			return cur.z, true
		}
		f := (force - prev.force) / (cur.force - prev.force)
		return prev.z + f*(cur.z-prev.z), true
	}
	return 0, false
}

// touchPointsFor is where to touch: two corners and the far edge's middle of
// the drawing's own extent, so the plane is measured where the pen will be.
func touchPointsFor(polylines []Polyline, first int) [3][2]float64 {
	minX, minY, maxX, maxY := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, poly := range polylines[first:] {
		for _, p := range poly {
			minX, maxX = math.Min(minX, p[0]), math.Max(maxX, p[0])
			minY, maxY = math.Min(minY, p[1]), math.Max(maxY, p[1])
		}
	}
	grow := func(lo, hi float64) (float64, float64) {
		if hi-lo >= minTouchSpreadMM {
			return lo, hi
		}
		mid := (lo + hi) / 2
		return mid - minTouchSpreadMM/2, mid + minTouchSpreadMM/2
	}
	minX, maxX = grow(minX, maxX)
	minY, maxY = grow(minY, maxY)
	return [3][2]float64{{minX, minY}, {maxX, minY}, {(minX + maxX) / 2, maxY}}
}

// touchOff touches the pen down at each point in turn and fits the paper's plane.
func (d *drawer) touchOff(
	ctx context.Context, fs *referenceframe.FrameSystem, run *forceRun, at [3]r3.Vector,
	orientation spatialmath.Orientation, zDown float64,
) (*touchPlane, error) {
	cfg := d.cfg.TouchOff.withDefaults()
	var found [3]r3.Vector
	for i, p := range at {
		z, err := d.touchOne(ctx, fs, run, p, orientation, zDown, cfg)
		if err != nil {
			return nil, fmt.Errorf("touch-off %d of 3 at (%.1f, %.1f): %w", i+1, p.X, p.Y, err)
		}
		found[i] = r3.Vector{X: p.X, Y: p.Y, Z: z}
	}
	plane, err := planeThrough(found)
	if err != nil {
		return nil, err
	}
	if plane.tilt() > maxTouchTiltPer1mm {
		return nil, fmt.Errorf("the touches fit a plane sloping %.2f mm per 100 mm, more than a sheet lying flat; "+
			"check the paper is flat and nothing was under the pen", 100*plane.tilt())
	}
	d.logger.Infof("drawer: touched off at z %.2f, %.2f, %.2f; paper slopes %.2f mm per 100 mm",
		found[0].Z, found[1].Z, found[2].Z, 100*plane.tilt())
	return plane, nil
}

// Every move down is guarded like a drawing's: a capped pen, being longer, meets
// the paper on the way down to the start of a touch, and trips the lower limit
// there. The lift is not: moving away can only ease the force, and a guard that
// stopped it would leave the pen pressing.
func (d *drawer) touchOne(
	ctx context.Context, fs *referenceframe.FrameSystem, run *forceRun, p r3.Vector, orientation spatialmath.Orientation,
	zDown float64, cfg TouchOffConfig,
) (float64, error) {
	move := func(z float64, linear, clear bool) error {
		target := spatialmath.NewPose(r3.Vector{X: p.X, Y: p.Y, Z: z}, orientation)
		return d.guardedMove(ctx, fs, []spatialmath.Pose{target}, run, linear, clear, nil)
	}
	// A free move that also descends can dip below its end on the way, so the
	// arm crosses between touches high and only comes down straight.
	travel := zDown + baselineHeightMM
	lift := func() {
		up := spatialmath.NewPose(r3.Vector{X: p.X, Y: p.Y, Z: travel}, orientation)
		if err := d.planAndExecute(ctx, fs, up, d.constraints(true)); err != nil {
			d.logger.Warnf("drawer: lift after touch-off: %v", err)
		}
	}
	if err := move(travel, false, true); err != nil {
		return 0, err
	}
	start := zDown + cfg.StartAboveMM
	if err := move(start, true, true); err != nil {
		return 0, err
	}
	baseline, err := d.forceGuard.measureBaseline(ctx)
	if err != nil {
		return 0, err
	}
	samples := []touchSample{{z: start}}
	step := cfg.StepMM
	for z := start - step; z >= zDown-cfg.MaxDepthMM-1e-9; z -= step {
		if err := move(z, true, false); err != nil {
			lift()
			return 0, err
		}
		force, err := d.settledForce(ctx, baseline)
		if err != nil {
			lift()
			return 0, err
		}
		samples = append(samples, touchSample{z: z, force: force})
		if force > d.forceGuard.max {
			lift()
			return 0, &excessForceError{force: force, limit: d.forceGuard.max}
		}
		if cz, ok := contactZ(samples, cfg.Force); ok {
			lift()
			d.logger.Infof("drawer: touch at (%.1f, %.1f): %.1f N at z %.2f, contact at %.2f",
				p.X, p.Y, force, z, cz)
			return cz, nil
		}
		if force >= firstTouchForce {
			step = cfg.FineStepMM
		}
	}
	lift()
	return 0, fmt.Errorf("felt nothing down to %.1f mm below the paper's configured height; is there paper there?", cfg.MaxDepthMM)
}

// settledForce waits for the arm to stop ringing, then averages a few readings
// and returns how far they are from baseline.
func (d *drawer) settledForce(ctx context.Context, baseline float64) (float64, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-time.After(touchSettle):
	}
	var sum float64
	for i := 0; i < touchReadings; i++ {
		v, err := d.forceGuard.read(ctx)
		if err != nil {
			return 0, fmt.Errorf("force_guard: read %q: %w", d.forceGuard.reading, err)
		}
		sum += v
	}
	return math.Abs(sum/touchReadings - baseline), nil
}

// onPlane moves every waypoint from the configured paper height onto the
// touched plane, keeping its height above the paper, and presses the pen-down
// ones press below it.
func onPlane(wps []waypoint, plane *touchPlane, zDown, press float64) {
	for i := range wps {
		wps[i].z += plane.z(wps[i].x, wps[i].y) - zDown
		if wps[i].contact {
			wps[i].z -= press
		}
	}
}

// touchedPaper is a sheet's plane and where the sheet was.
type touchedPaper struct {
	corner, across r3.Vector
	plane          *touchPlane
}

func (t *touchedPaper) same(paper surface) bool {
	return t != nil && t.corner.Distance(paper.corner.Point()) < 0.5 && t.across.Distance(paper.across) < 1e-3
}

// paperPlane touches the paper off, or for a drawing resuming on the sheet the
// last one measured, reuses that.
func (d *drawer) paperPlane(
	ctx context.Context, fs *referenceframe.FrameSystem, run *forceRun, polylines []Polyline, startAt int,
	paper surface, zDown float64,
) (*touchPlane, error) {
	if startAt > 0 && d.touched.same(paper) {
		return d.touched.plane, nil
	}
	corner := paper.corner.Point()
	var at [3]r3.Vector
	for i, p := range touchPointsFor(polylines, startAt) {
		x, y := paperToWorld(corner, paper.across, p)
		at[i] = r3.Vector{X: x, Y: y}
	}
	plane, err := d.touchOff(ctx, fs, run, at, paper.corner.Orientation(), zDown)
	if err != nil {
		return nil, err
	}
	d.touched = &touchedPaper{corner: corner, across: paper.across, plane: plane}
	return plane, nil
}
