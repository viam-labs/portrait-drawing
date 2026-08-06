// Package strokegenerator implements a Viam generic service that turns an image into ordered 2D polylines in paper-local mm.
package strokegenerator

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/generic"
)

// Model is the stroke-generator service.
var Model = resource.NewModel("viam", "portrait-drawing", "stroke-generator")

func init() {
	resource.RegisterService(generic.API, Model,
		resource.Registration[resource.Resource, *Config]{
			Constructor: newStrokeGenerator,
		},
	)
}

// Config is the stroke-generator service configuration.
type Config struct{}

// Validate returns implicit dependencies and any config errors.
func (cfg *Config) Validate(_ string) ([]string, []string, error) {
	return nil, nil, nil
}

type strokeGenerator struct {
	resource.AlwaysRebuild
	resource.TriviallyCloseable

	name       resource.Name
	logger     logging.Logger
	pythonBin  string
	scriptPath string
}

func newStrokeGenerator(
	_ context.Context,
	_ resource.Dependencies,
	conf resource.Config,
	logger logging.Logger,
) (resource.Resource, error) {
	exePath, err := os.Executable()
	if err != nil {
		return nil, err
	}
	moduleRoot := filepath.Dir(filepath.Dir(exePath))
	return &strokeGenerator{
		name:       conf.ResourceName(),
		logger:     logger,
		pythonBin:  filepath.Join(moduleRoot, ".venv", "bin", "python"),
		scriptPath: filepath.Join(moduleRoot, "python", "image_to_polylines.py"),
	}, nil
}

func (s *strokeGenerator) Name() resource.Name {
	return s.name
}

func (s *strokeGenerator) DoCommand(_ context.Context, _ map[string]interface{}) (map[string]interface{}, error) {
	return nil, errors.New("stroke-generator: not implemented")
}

func (s *strokeGenerator) Status(_ context.Context) (map[string]interface{}, error) {
	return map[string]interface{}{"state": "ready"}, nil
}
