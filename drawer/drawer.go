// Package drawer implements a Viam generic service that draws polylines on paper with a robotic arm.
package drawer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/components/arm"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/motionplan"
	"go.viam.com/rdk/motionplan/armplanning"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/robot/framesystem"
	"go.viam.com/rdk/services/generic"
	"go.viam.com/rdk/spatialmath"

	"github.com/viam-labs/portrait-drawing/internal/verb"
)

// Model is the drawer service.
var Model = resource.NewModel("viam", "portrait-drawing", "drawer")

func init() {
	resource.RegisterService(generic.API, Model,
		resource.Registration[resource.Resource, *Config]{
			Constructor: newDrawer,
		},
	)
}

// Config is the drawer service configuration.
type Config struct {
	Arm string `json:"arm"`
	// PaperTopLeftCorner is the tool pose the arm holds when the pen tip
	// touches the paper's top-left corner. Orientation is reused for every
	// waypoint so the pen keeps the same attitude across the whole drawing.
	PaperTopLeftCorner *poseConfig `json:"paper_top_left_corner"`
	PaperWidthMM       float64     `json:"paper_width_mm"`
	PaperHeightMM      float64     `json:"paper_height_mm"`
	LiftOffZMM         float64     `json:"lift_off_z_mm,omitempty"`
	// HomePose, if set, is the tool pose the arm returns to after the last
	// polyline is drawn.
	HomePose           *poseConfig                                `json:"home_pose,omitempty"`
	InputRangeOverride map[string]map[string]referenceframe.Limit `json:"input_range_override,omitempty"`
}

type poseConfig struct {
	Translation r3.Vector                      `json:"translation"`
	Orientation *spatialmath.OrientationConfig `json:"orientation"`
}

const (
	defaultLiftOffZMM               = 5.0
	drawLineToleranceMM             = 0.5
	drawOrientationToleranceDegrees = 1.0
)

// Validate returns implicit dependencies and any config errors.
func (cfg *Config) Validate(path string) ([]string, []string, error) {
	if cfg.Arm == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "arm")
	}
	if cfg.PaperTopLeftCorner == nil {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "paper_top_left_corner")
	}
	if cfg.PaperTopLeftCorner.Orientation == nil {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "paper_top_left_corner.orientation")
	}
	if cfg.PaperWidthMM <= 0 {
		return nil, nil, fmt.Errorf("paper_width_mm must be > 0, got %g", cfg.PaperWidthMM)
	}
	if cfg.PaperHeightMM <= 0 {
		return nil, nil, fmt.Errorf("paper_height_mm must be > 0, got %g", cfg.PaperHeightMM)
	}
	if cfg.LiftOffZMM < 0 {
		return nil, nil, fmt.Errorf("lift_off_z_mm must be >= 0 (0 uses the default), got %g", cfg.LiftOffZMM)
	}
	if cfg.HomePose != nil && cfg.HomePose.Orientation == nil {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "home_pose.orientation")
	}
	return []string{cfg.Arm}, nil, nil
}

type drawer struct {
	resource.AlwaysRebuild
	resource.TriviallyCloseable

	name               resource.Name
	logger             logging.Logger
	cfg                *Config
	arm                arm.Arm
	fsService          framesystem.Service
	paperTopLeftCorner spatialmath.Pose
	homePose           spatialmath.Pose
}

func newDrawer(
	_ context.Context,
	deps resource.Dependencies,
	conf resource.Config,
	logger logging.Logger,
) (resource.Resource, error) {
	cfg, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return nil, err
	}
	if cfg.LiftOffZMM == 0 {
		cfg.LiftOffZMM = defaultLiftOffZMM
	}
	a, err := arm.FromProvider(deps, cfg.Arm)
	if err != nil {
		return nil, fmt.Errorf("drawer: get arm dep %q: %w", cfg.Arm, err)
	}
	fsService, err := framesystem.FromDependencies(deps)
	if err != nil {
		return nil, fmt.Errorf("drawer: get framesystem service: %w", err)
	}
	orientation, err := cfg.PaperTopLeftCorner.Orientation.ParseConfig()
	if err != nil {
		return nil, fmt.Errorf("drawer: parse paper_top_left_corner.orientation: %w", err)
	}
	var homePose spatialmath.Pose
	if cfg.HomePose != nil {
		homeOrientation, err := cfg.HomePose.Orientation.ParseConfig()
		if err != nil {
			return nil, fmt.Errorf("drawer: parse home_pose.orientation: %w", err)
		}
		homePose = spatialmath.NewPose(cfg.HomePose.Translation, homeOrientation)
	}
	return &drawer{
		name:               conf.ResourceName(),
		logger:             logger,
		cfg:                cfg,
		arm:                a,
		fsService:          fsService,
		paperTopLeftCorner: spatialmath.NewPose(cfg.PaperTopLeftCorner.Translation, orientation),
		homePose:           homePose,
	}, nil
}

func (d *drawer) Name() resource.Name {
	return d.name
}

// Polyline is an ordered sequence of 2D points drawn as one continuous stroke.
// Points are in mm, in paper-local coordinates where (0,0) is the top-left corner.
type Polyline [][2]float64

type drawArgs struct {
	Polylines [][][]float64 `json:"polylines"`
}

func parseDrawPayload(payload interface{}) ([]Polyline, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}
	var args drawArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("parse payload: %w", err)
	}
	if len(args.Polylines) == 0 {
		return nil, errors.New("polylines is required and must be non-empty")
	}
	out := make([]Polyline, len(args.Polylines))
	for i, poly := range args.Polylines {
		if len(poly) == 0 {
			return nil, fmt.Errorf("polyline %d is empty", i)
		}
		out[i] = make(Polyline, len(poly))
		for j, pt := range poly {
			if len(pt) != 2 {
				return nil, fmt.Errorf("polyline %d point %d must have exactly 2 elements, got %d", i, j, len(pt))
			}
			out[i][j] = [2]float64{pt[0], pt[1]}
		}
	}
	return out, nil
}

func (d *drawer) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	v, err := verb.Single(cmd)
	if err != nil {
		return nil, err
	}
	switch v {
	case "draw":
		return d.draw(ctx, cmd["draw"])
	default:
		return nil, fmt.Errorf("drawer: unknown verb %q; expected \"draw\"", v)
	}
}

func (d *drawer) draw(ctx context.Context, payload interface{}) (map[string]interface{}, error) {
	polylines, err := parseDrawPayload(payload)
	if err != nil {
		return nil, fmt.Errorf("drawer: %w", err)
	}

	fs, err := d.buildFrameSystem(ctx)
	if err != nil {
		return nil, err
	}

	corner := d.paperTopLeftCorner.Point()
	orientation := d.paperTopLeftCorner.Orientation()
	zDown := corner.Z
	zUp := corner.Z + d.cfg.LiftOffZMM

	// LinearConstraint enforces both a straight Cartesian path and orientation
	// lock along every planned move.
	constraints := motionplan.NewConstraints(
		[]motionplan.LinearConstraint{{
			LineToleranceMm:          drawLineToleranceMM,
			OrientationToleranceDegs: drawOrientationToleranceDegrees,
		}},
		nil, nil, nil,
	)

	moveTo := func(x, y, z float64) error {
		return d.planAndExecute(ctx, fs, spatialmath.NewPose(r3.Vector{X: x, Y: y, Z: z}, orientation), constraints)
	}

	total := 0
	for i, poly := range polylines {
		sx, sy := corner.X+poly[0][0], corner.Y+poly[0][1]
		if err := moveTo(sx, sy, zUp); err != nil {
			return nil, fmt.Errorf("drawer: polyline %d approach: %w", i, err)
		}
		if err := moveTo(sx, sy, zDown); err != nil {
			return nil, fmt.Errorf("drawer: polyline %d pen-down: %w", i, err)
		}
		for j := 1; j < len(poly); j++ {
			px, py := corner.X+poly[j][0], corner.Y+poly[j][1]
			if err := moveTo(px, py, zDown); err != nil {
				return nil, fmt.Errorf("drawer: polyline %d point %d: %w", i, j, err)
			}
		}
		lx, ly := corner.X+poly[len(poly)-1][0], corner.Y+poly[len(poly)-1][1]
		if err := moveTo(lx, ly, zUp); err != nil {
			return nil, fmt.Errorf("drawer: polyline %d pen-up: %w", i, err)
		}
		total += len(poly)
	}

	if d.homePose != nil {
		if err := d.planAndExecute(ctx, fs, d.homePose, nil); err != nil {
			return nil, fmt.Errorf("drawer: return to home: %w", err)
		}
	}

	return map[string]interface{}{
		"total_points": total,
	}, nil
}

func (d *drawer) buildFrameSystem(ctx context.Context) (*referenceframe.FrameSystem, error) {
	fs, err := framesystem.NewFromService(ctx, d.fsService, nil)
	if err != nil {
		return nil, fmt.Errorf("drawer: build frame system: %w", err)
	}
	if len(d.cfg.InputRangeOverride) > 0 {
		if err := applyJointLimits(d.logger, fs, d.cfg.InputRangeOverride); err != nil {
			return nil, fmt.Errorf("drawer: apply input_range_override: %w", err)
		}
	}
	return fs, nil
}

func (d *drawer) planAndExecute(ctx context.Context, fs *referenceframe.FrameSystem, target spatialmath.Pose, constraints *motionplan.Constraints) error {
	armName := d.arm.Name().Name
	currentInputs, err := d.arm.JointPositions(ctx, nil)
	if err != nil {
		return fmt.Errorf("get current joints: %w", err)
	}
	startState := armplanning.NewPlanState(nil, referenceframe.FrameSystemInputs{
		armName: currentInputs,
	})
	goalState := armplanning.NewPlanState(referenceframe.FrameSystemPoses{
		armName: referenceframe.NewPoseInFrame(referenceframe.World, target),
	}, nil)
	plan, _, err := armplanning.PlanMotion(ctx, d.logger, &armplanning.PlanRequest{
		FrameSystem: fs,
		StartState:  startState,
		Goals:       []*armplanning.PlanState{goalState},
		Constraints: constraints,
	})
	if err != nil {
		return fmt.Errorf("plan motion: %w", err)
	}
	trajectory := make([][]referenceframe.Input, 0, len(plan.Trajectory()))
	for _, fsInputs := range plan.Trajectory() {
		armInputs, err := fsInputs.GetFrameInputs(fs.Frame(armName))
		if err != nil {
			return fmt.Errorf("extract arm inputs from trajectory: %w", err)
		}
		trajectory = append(trajectory, armInputs)
	}
	if err := d.arm.MoveThroughJointPositions(ctx, trajectory, nil, nil); err != nil {
		return fmt.Errorf("execute trajectory: %w", err)
	}
	return nil
}

func applyJointLimits(logger logging.Logger, fs *referenceframe.FrameSystem, inputRangeOverride map[string]map[string]referenceframe.Limit) error {
	for fName, mods := range inputRangeOverride {
		f := fs.Frame(fName)
		if f == nil {
			return fmt.Errorf("frame (%s) in input_range_override doesn't exist", fName)
		}
		sm, ok := f.(*referenceframe.SimpleModel)
		if !ok {
			return fmt.Errorf("can only override joints for SimpleModel for now, not %T", f)
		}
		resolved := make(map[string]referenceframe.Limit, len(mods))
		moveableNames := sm.MoveableFrameNames()
		existingLimits := sm.DoF()
		for key, limit := range mods {
			matched := false
			for i, name := range moveableNames {
				if key == name || key == strconv.Itoa(i) {
					existing := existingLimits[i]
					tightened := referenceframe.Limit{
						Min: math.Max(limit.Min, existing.Min),
						Max: math.Min(limit.Max, existing.Max),
					}
					if tightened.Min != limit.Min || tightened.Max != limit.Max {
						logger.Warnf(
							"input_range_override for frame %q joint %q would loosen limits: requested [%.6f, %.6f], model declares [%.6f, %.6f]; tightening to [%.6f, %.6f]",
							fName, name,
							limit.Min, limit.Max,
							existing.Min, existing.Max,
							tightened.Min, tightened.Max,
						)
					}
					resolved[name] = tightened
					matched = true
					break
				}
			}
			if !matched {
				return fmt.Errorf("can't find mod (%s)", key)
			}
		}
		newModel, err := referenceframe.NewModelWithLimitOverrides(sm, resolved)
		if err != nil {
			return err
		}
		if err := fs.ReplaceFrame(newModel); err != nil {
			return err
		}
	}
	return nil
}

func (d *drawer) Status(_ context.Context) (map[string]interface{}, error) {
	return map[string]interface{}{"state": "ready"}, nil
}
