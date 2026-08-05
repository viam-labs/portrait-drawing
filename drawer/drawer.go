// Package drawer implements a Viam generic service that draws polylines on paper with a robotic arm.
package drawer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/components/arm"
	"go.viam.com/rdk/logging"
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
	Arm                string                                     `json:"arm"`
	PaperTopLeftCorner *r3.Vector                                 `json:"paper_top_left_corner"`
	PaperWidthMM       float64                                    `json:"paper_width_mm"`
	PaperHeightMM      float64                                    `json:"paper_height_mm"`
	LiftOffZMM         float64                                    `json:"lift_off_z_mm,omitempty"`
	InputRangeOverride map[string]map[string]referenceframe.Limit `json:"input_range_override,omitempty"`
}

const defaultLiftOffZMM = 5.0

// Validate returns implicit dependencies and any config errors.
func (cfg *Config) Validate(path string) ([]string, []string, error) {
	if cfg.Arm == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "arm")
	}
	if cfg.PaperTopLeftCorner == nil {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "paper_top_left_corner")
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
	return &drawer{
		name:               conf.ResourceName(),
		logger:             logger,
		cfg:                cfg,
		arm:                a,
		fsService:          fsService,
		paperTopLeftCorner: spatialmath.NewPoseFromPoint(*cfg.PaperTopLeftCorner),
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

func (d *drawer) DoCommand(_ context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	v, err := verb.Single(cmd)
	if err != nil {
		return nil, err
	}
	switch v {
	case "draw":
		return d.draw(cmd["draw"])
	default:
		return nil, fmt.Errorf("drawer: unknown verb %q; expected \"draw\"", v)
	}
}

func (d *drawer) draw(payload interface{}) (map[string]interface{}, error) {
	polylines, err := parseDrawPayload(payload)
	if err != nil {
		return nil, fmt.Errorf("drawer: %w", err)
	}
	total := 0
	for _, p := range polylines {
		total += len(p)
	}
	d.logger.Infof("drawer: parsed %d polylines with %d total points (motion not yet implemented)", len(polylines), total)
	return map[string]interface{}{
		"total_points": total,
	}, nil
}

func (d *drawer) Status(_ context.Context) (map[string]interface{}, error) {
	return map[string]interface{}{"state": "ready"}, nil
}
