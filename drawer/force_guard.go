package drawer

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"go.viam.com/rdk/components/arm"
	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/spatialmath"
)

const (
	defaultContactMarginMM = 2.0
	forcePollInterval      = 20 * time.Millisecond
	baselineSamples        = 10
	// baselineHeightMM is above the paper by more than a marker cap adds, so a
	// capped pen is still in the air when the baseline is taken.
	baselineHeightMM = 40.0
	faultExcessForce = "excess_force"
	// frozenPolls is how many polls in a row may read exactly the same before the
	// sensor counts as frozen: about half a second, far longer than a live
	// sensor's noise ever holds still.
	frozenPolls = 25
	// retreatPosition is an arm-position-saver switch's "go to" position.
	retreatPosition = 2
)

// ForceGuardConfig stops a drawing when the pen meets more resistance than a
// bare marker tip should, which is what a cap left on looks like.
type ForceGuardConfig struct {
	Sensor string `json:"sensor"`
	// Reading is the key in the sensor's Readings that carries the force.
	Reading string `json:"reading"`
	// MaxForce is the most the sensor reads in a normal drawing with the pen down.
	MaxForce float64 `json:"max_force"`
	// ContactForce is the limit while the pen is still more than ContactMarginMM
	// above the paper, where a bare tip touches nothing.
	ContactForce    float64 `json:"contact_force"`
	ContactMarginMM float64 `json:"contact_margin_mm,omitempty"`
	// RetreatPose is an arm-position-saver switch the arm goes to after the guard
	// stops a drawing, somewhere a person can reach the pen. Unset uses home_pose.
	RetreatPose string `json:"retreat_pose,omitempty"`
}

func (c *ForceGuardConfig) contactMarginMM() float64 {
	if c.ContactMarginMM == 0 {
		return defaultContactMarginMM
	}
	return c.ContactMarginMM
}

func (c *ForceGuardConfig) validate(path string, liftOffZMM float64) error {
	if c.Sensor == "" {
		return resource.NewConfigValidationFieldRequiredError(path, "force_guard.sensor")
	}
	if c.Reading == "" {
		return resource.NewConfigValidationFieldRequiredError(path, "force_guard.reading")
	}
	if c.MaxForce <= 0 {
		return fmt.Errorf("force_guard.max_force must be > 0, got %g", c.MaxForce)
	}
	if c.ContactForce <= 0 || c.ContactForce > c.MaxForce {
		return fmt.Errorf("force_guard.contact_force must be > 0 and at most max_force (%g), got %g", c.MaxForce, c.ContactForce)
	}
	if c.ContactMarginMM < 0 {
		return fmt.Errorf("force_guard.contact_margin_mm must be >= 0 (0 uses the default), got %g", c.ContactMarginMM)
	}
	if margin := c.contactMarginMM(); margin >= liftOffZMM {
		return fmt.Errorf(
			"force_guard.contact_margin_mm (%g) must be less than lift_off_z_mm (%g), or the pen is never high enough to check for early contact",
			margin, liftOffZMM)
	}
	return nil
}

type positioner interface {
	SetPosition(ctx context.Context, position uint32, extra map[string]interface{}) error
}

type forceGuard struct {
	sensor  sensor.Sensor
	reading string
	max     float64
	contact float64
	retreat positioner
}

func (g *forceGuard) limit(clear bool) float64 {
	if clear {
		return g.contact
	}
	return g.max
}

// measureBaseline averages the reading with the pen held still in the air. A
// wrist sensor carries the tool's weight and its own bias, and the pen keeps one
// attitude for the whole drawing, so that offset is constant.
func (g *forceGuard) measureBaseline(ctx context.Context) (float64, error) {
	var sum float64
	lowest, highest := math.Inf(1), math.Inf(-1)
	for i := 0; i < baselineSamples; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(forcePollInterval):
			}
		}
		v, err := g.read(ctx)
		if err != nil {
			return 0, fmt.Errorf("force_guard: read %q: %w", g.reading, err)
		}
		sum += v
		lowest, highest = math.Min(lowest, v), math.Max(highest, v)
	}
	// A live sensor always has noise; identical samples mean nothing is being measured.
	if lowest == highest {
		return 0, fmt.Errorf(
			"force_guard: %q read exactly %g %d times in a row, so the sensor is not streaming; refusing to draw unguarded",
			g.reading, lowest, baselineSamples)
	}
	return sum / baselineSamples, nil
}

func (g *forceGuard) read(ctx context.Context) (float64, error) {
	readings, err := g.sensor.Readings(ctx, nil)
	if err != nil {
		return 0, err
	}
	raw, ok := readings[g.reading]
	if !ok {
		return 0, fmt.Errorf("sensor has no reading %q", g.reading)
	}
	switch v := raw.(type) {
	case float64:
		return v, nil
	case float32:
		return float64(v), nil
	case int:
		return float64(v), nil
	case int32:
		return float64(v), nil
	case int64:
		return float64(v), nil
	default:
		return 0, fmt.Errorf("reading %q is %T, not a number", g.reading, raw)
	}
}

type forceStats struct {
	samples   int
	sum, peak float64
}

func (s *forceStats) record(force float64) {
	s.samples++
	s.sum += force
	s.peak = math.Max(s.peak, force)
}

func (s *forceStats) mean() float64 {
	if s.samples == 0 {
		return 0
	}
	return s.sum / float64(s.samples)
}

func (s *forceStats) summary() map[string]interface{} {
	return map[string]interface{}{"mean": s.mean(), "peak": s.peak, "samples": s.samples}
}

// forceRun is one draw's baseline and what the force did over it, split the way
// the limits are: lifted is what contact_force is held to, penDown max_force.
type forceRun struct {
	baseline        float64
	lifted, penDown forceStats
	// stroke is the current pen-down move's share of penDown.
	stroke forceStats
	// last and repeats track identical raw readings across moves, since a
	// frozen sensor stays frozen from one move to the next.
	last    float64
	repeats int
}

func (r *forceRun) noteRaw(raw float64) {
	if r.repeats > 0 && raw == r.last {
		r.repeats++
	} else {
		r.repeats = 1
	}
	r.last = raw
}

func (r *forceRun) record(clear bool, force float64) {
	if clear {
		r.lifted.record(force)
		return
	}
	r.penDown.record(force)
	r.stroke.record(force)
}

func (r *forceRun) summary() map[string]interface{} {
	out := map[string]interface{}{
		"baseline": r.baseline,
		"pen_down": r.penDown.summary(),
		"lifted":   r.lifted.summary(),
	}
	return out
}

// finishForceRun keeps the run for status and logs it, so the limits can be set
// from what a real drawing measured.
func (d *drawer) finishForceRun(run *forceRun) {
	if run.lifted.samples+run.penDown.samples == 0 {
		return
	}
	d.mu.Lock()
	d.lastForce = run
	d.mu.Unlock()
	d.logger.Infof(
		"drawer: force %q over baseline %.3g: pen down mean %.3g peak %.3g (%d samples), lifted mean %.3g peak %.3g (%d samples)",
		d.forceGuard.reading, run.baseline,
		run.penDown.mean(), run.penDown.peak, run.penDown.samples,
		run.lifted.mean(), run.lifted.peak, run.lifted.samples)
}

type excessForceError struct {
	force, limit float64
	clear        bool
}

// frozenForceError is a sensor that has stopped updating mid-drawing: lost its
// stream, or latched an overload. It reads plausible numbers that mean nothing.
type frozenForceError struct {
	reading string
	value   float64
}

func (e *frozenForceError) Error() string {
	return fmt.Sprintf("force_guard: %q read exactly %g for %d polls in a row, so the sensor has stopped updating; "+
		"power-cycle the arm's controller if it latched an overload", e.reading, e.value, frozenPolls)
}

func (e *excessForceError) Error() string {
	if e.clear {
		return fmt.Sprintf("force %.3g over its baseline exceeded %.3g before the pen should have reached the paper; "+
			"take the cap off the marker, then resume", e.force, e.limit)
	}
	return fmt.Sprintf("force %.3g over its baseline exceeded %.3g with the pen on the paper; "+
		"the pen is pressing too hard: check the paper's height, and that the sheet is lying flat", e.force, e.limit)
}

// guardedMove is planAndExecuteThrough with the force watched for the whole
// move. clear marks a move that stays above the paper, held to the lower
// contact limit.
func (d *drawer) guardedMove(
	ctx context.Context, fs *referenceframe.FrameSystem, targets []spatialmath.Pose, run *forceRun, linear, clear bool,
	options *arm.MoveOptions,
) error {
	return d.watchForce(ctx, run, clear, func(moveCtx context.Context) error {
		return d.planAndExecuteThrough(moveCtx, fs, targets, d.constraints(linear), options)
	})
}

// watchForce runs move, canceling it and stopping the arm if the force strays
// from the run's baseline by more than the limit, either way, or cannot be read.
func (d *drawer) watchForce(ctx context.Context, run *forceRun, clear bool, move func(context.Context) error) error {
	if d.forceGuard == nil {
		return move(ctx)
	}
	limit := d.forceGuard.limit(clear)
	moveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	tripped := make(chan error, 1)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		ticker := time.NewTicker(forcePollInterval)
		defer ticker.Stop()
		for {
			raw, err := d.forceGuard.read(moveCtx)
			if moveCtx.Err() != nil {
				return
			}
			force := math.Abs(raw - run.baseline)
			if err == nil {
				run.record(clear, force)
				run.noteRaw(raw)
			}
			switch {
			case err != nil:
				// A guard that cannot read is no guard, so the move does not go on without it.
				tripped <- fmt.Errorf("force_guard: read %q: %w", d.forceGuard.reading, err)
			case run.repeats >= frozenPolls:
				tripped <- &frozenForceError{reading: d.forceGuard.reading, value: raw}
			case force > limit:
				tripped <- &excessForceError{force: force, limit: limit, clear: clear}
			default:
				select {
				case <-moveCtx.Done():
					return
				case <-ticker.C:
				}
				continue
			}
			cancel()
			if stopErr := d.arm.Stop(ctx, nil); stopErr != nil {
				d.logger.Warnf("drawer: arm.Stop after force guard tripped: %v", stopErr)
			}
			return
		}
	}()
	err := move(moveCtx)
	cancel()
	<-watchDone
	select {
	case tripErr := <-tripped:
		return tripErr
	default:
		return err
	}
}

// forceBaseline holds the pen well above where the drawing starts and measures
// the force there. Without a guard it does nothing.
func (d *drawer) forceBaseline(ctx context.Context, fs *referenceframe.FrameSystem, above spatialmath.Pose) (float64, error) {
	if d.forceGuard == nil {
		return 0, nil
	}
	if err := d.planAndExecute(ctx, fs, above, d.constraints(false)); err != nil {
		return 0, fmt.Errorf("move to force baseline height: %w", err)
	}
	return d.forceGuard.measureBaseline(ctx)
}

// retreat lifts the pen straight up from wherever the guard stopped it and
// parks the arm, so the marker is clear of the paper and within reach.
func (d *drawer) retreat(ctx context.Context, fs *referenceframe.FrameSystem, zUp float64, orientation spatialmath.Orientation) error {
	armName := d.arm.Name().Name
	joints, err := d.arm.JointPositions(ctx, nil)
	if err != nil {
		return fmt.Errorf("get current joints: %w", err)
	}
	inputs := referenceframe.NewLinearInputs()
	inputs.Put(armName, joints)
	tf, err := fs.Transform(inputs, referenceframe.NewZeroPoseInFrame(armName), referenceframe.World)
	if err != nil {
		return fmt.Errorf("locate the pen: %w", err)
	}
	pif, ok := tf.(*referenceframe.PoseInFrame)
	if !ok {
		return fmt.Errorf("locate the pen: unexpected transform result %T", tf)
	}
	at := pif.Pose().Point()
	if at.Z < zUp {
		at.Z = zUp
		if err := d.planAndExecute(ctx, fs, spatialmath.NewPose(at, orientation), d.constraints(true)); err != nil {
			return fmt.Errorf("lift the pen: %w", err)
		}
	}
	return d.park(ctx, fs)
}

// park takes the arm to retreat_pose, or else to the rest pose.
func (d *drawer) park(ctx context.Context, fs *referenceframe.FrameSystem) error {
	if d.forceGuard != nil && d.forceGuard.retreat != nil {
		if err := d.forceGuard.retreat.SetPosition(ctx, retreatPosition, nil); err != nil {
			return fmt.Errorf("go to retreat_pose %q: %w", d.cfg.ForceGuard.RetreatPose, err)
		}
		return nil
	}
	if !d.hasRestPose() {
		return nil
	}
	return d.goToRestPose(ctx, fs)
}

// guardStopped reports whether the guard, not the planner or the arm, ended the
// move: then the pen may be pressing on the paper and has to come off it.
func guardStopped(err error) bool {
	var excess *excessForceError
	var frozen *frozenForceError
	return errors.As(err, &excess) || errors.As(err, &frozen)
}

func faultOf(err error) string {
	var excess *excessForceError
	if errors.As(err, &excess) {
		return faultExcessForce
	}
	return ""
}
