// Package drawer implements a Viam generic service that draws polylines on paper with a robotic arm.
package drawer

import (
	"context"
	"errors"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/generic"
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
type Config struct{}

// Validate returns implicit dependencies and any config errors.
func (cfg *Config) Validate(_ string) ([]string, []string, error) {
	return nil, nil, nil
}

type drawer struct {
	resource.AlwaysRebuild
	resource.TriviallyCloseable

	name   resource.Name
	logger logging.Logger
}

func newDrawer(
	_ context.Context,
	_ resource.Dependencies,
	conf resource.Config,
	logger logging.Logger,
) (resource.Resource, error) {
	return &drawer{
		name:   conf.ResourceName(),
		logger: logger,
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
