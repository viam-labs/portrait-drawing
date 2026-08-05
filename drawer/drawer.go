// Package drawer implements a Viam generic service that draws polylines on paper with a robotic arm.
package drawer

import (
	"context"
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

func (d *drawer) DoCommand(_ context.Context, _ map[string]interface{}) (map[string]interface{}, error) {
	return nil, errors.New("drawer: not implemented")
}

func (d *drawer) Status(_ context.Context) (map[string]interface{}, error) {
	return map[string]interface{}{"state": "ready"}, nil
}
