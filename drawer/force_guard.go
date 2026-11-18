package drawer

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/spatialmath"
)

const (
	defaultContactMarginMM = 2.0
	forcePollInterval      = 20 * time.Millisecond
	faultExcessForce       = "excess_force"
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

type forceGuard struct {
	sensor  sensor.Sensor
	reading string
	max     float64
	contact float64
}

func (g *forceGuard) limit(clear bool) float64 {
	if clear {
		return g.contact
	}
	return g.max
}

// read returns the magnitude of the force: which sign means "pushing on the
// paper" depends on how the sensor is mounted.
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
		return math.Abs(v), nil
	case float32:
		return math.Abs(float64(v)), nil
	case int:
		return math.Abs(float64(v)), nil
	case int32:
		return math.Abs(float64(v)), nil
	case int64:
		return math.Abs(float64(v)), nil
	default:
		return 0, fmt.Errorf("reading %q is %T, not a number", g.reading, raw)
	}
}

type excessForceError struct {
	force, limit float64
	clear        bool
}

func (e *excessForceError) Error() string {
	where := "with the pen on the paper"
	if e.clear {
		where = "before the pen should have reached the paper"
	}
	return fmt.Sprintf("force %.3g exceeded %.3g %s; take the cap off the marker, then resume", e.force, e.limit, where)
}

// guardedMove is planAndExecute with the force watched for the whole move. clear
// marks a move that stays above the paper, held to the lower contact limit.
func (d *drawer) guardedMove(
	ctx context.Context, fs *referenceframe.FrameSystem, target spatialmath.Pose, linear, clear bool,
) error {
	return d.watchForce(ctx, clear, func(moveCtx context.Context) error {
		return d.planAndExecute(moveCtx, fs, target, d.constraints(linear))
	})
}

// watchForce runs move, canceling it and stopping the arm if the force passes
// the limit or cannot be read.
func (d *drawer) watchForce(ctx context.Context, clear bool, move func(context.Context) error) error {
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
			force, err := d.forceGuard.read(moveCtx)
			if moveCtx.Err() != nil {
				return
			}
			switch {
			case err != nil:
				// A guard that cannot read is no guard, so the move does not go on without it.
				tripped <- fmt.Errorf("force_guard: read %q: %w", d.forceGuard.reading, err)
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
	if !d.hasRestPose() {
		return nil
	}
	return d.goToRestPose(ctx, fs)
}

func faultOf(err error) string {
	var excess *excessForceError
	if errors.As(err, &excess) {
		return faultExcessForce
	}
	return ""
}
