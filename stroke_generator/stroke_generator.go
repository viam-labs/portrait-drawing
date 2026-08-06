// Package strokegenerator implements a Viam generic service that turns an image into ordered 2D polylines in paper-local mm.
package strokegenerator

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/generic"

	"github.com/viam-labs/portrait-drawing/internal/verb"
	"github.com/viam-labs/portrait-drawing/pyrunner"
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

// Config is the stroke-generator service configuration. Fields tune the
// image-processing pipeline; per-call paper geometry lives in the
// generate DoCommand payload.
type Config struct {
	Region  int     `json:"region,omitempty"`
	Low     int     `json:"low,omitempty"`
	High    int     `json:"high,omitempty"`
	Merge   int     `json:"merge,omitempty"`
	Prune   int     `json:"prune,omitempty"`
	MinLen  int     `json:"min_len,omitempty"`
	Smooth  float64 `json:"smooth,omitempty"`
	MinDist float64 `json:"min_dist,omitempty"`
}

const (
	defaultRegion  = 25
	defaultLow     = 60
	defaultHigh    = 160
	defaultMerge   = 5
	defaultPrune   = 25
	defaultMinLen  = 90
	defaultSmooth  = 2.5
	defaultMinDist = 8.0
	defaultMargin  = 40.0
)

// Validate returns implicit dependencies and any config errors.
func (cfg *Config) Validate(_ string) ([]string, []string, error) {
	if cfg.Region < 0 {
		return nil, nil, fmt.Errorf("region must be >= 0, got %d", cfg.Region)
	}
	if cfg.Low < 0 || cfg.Low > 255 {
		return nil, nil, fmt.Errorf("low must be in [0, 255], got %d", cfg.Low)
	}
	if cfg.High < 0 || cfg.High > 255 {
		return nil, nil, fmt.Errorf("high must be in [0, 255], got %d", cfg.High)
	}
	if cfg.Merge < 0 {
		return nil, nil, fmt.Errorf("merge must be >= 0, got %d", cfg.Merge)
	}
	if cfg.Prune < 0 {
		return nil, nil, fmt.Errorf("prune must be >= 0, got %d", cfg.Prune)
	}
	if cfg.MinLen < 0 {
		return nil, nil, fmt.Errorf("min_len must be >= 0, got %d", cfg.MinLen)
	}
	if cfg.Smooth < 0 {
		return nil, nil, fmt.Errorf("smooth must be >= 0, got %g", cfg.Smooth)
	}
	if cfg.MinDist < 0 {
		return nil, nil, fmt.Errorf("min_dist must be >= 0, got %g", cfg.MinDist)
	}
	return nil, nil, nil
}

type strokeGenerator struct {
	resource.AlwaysRebuild
	resource.TriviallyCloseable

	name       resource.Name
	logger     logging.Logger
	cfg        *Config
	pythonBin  string
	scriptPath string
}

func newStrokeGenerator(
	_ context.Context,
	_ resource.Dependencies,
	conf resource.Config,
	logger logging.Logger,
) (resource.Resource, error) {
	cfg, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return nil, err
	}
	if cfg.Region == 0 {
		cfg.Region = defaultRegion
	}
	if cfg.Low == 0 {
		cfg.Low = defaultLow
	}
	if cfg.High == 0 {
		cfg.High = defaultHigh
	}
	if cfg.Merge == 0 {
		cfg.Merge = defaultMerge
	}
	if cfg.Prune == 0 {
		cfg.Prune = defaultPrune
	}
	if cfg.MinLen == 0 {
		cfg.MinLen = defaultMinLen
	}
	if cfg.Smooth == 0 {
		cfg.Smooth = defaultSmooth
	}
	if cfg.MinDist == 0 {
		cfg.MinDist = defaultMinDist
	}
	exePath, err := os.Executable()
	if err != nil {
		return nil, err
	}
	moduleRoot := filepath.Dir(filepath.Dir(exePath))
	return &strokeGenerator{
		name:       conf.ResourceName(),
		logger:     logger,
		cfg:        cfg,
		pythonBin:  filepath.Join(moduleRoot, ".venv", "bin", "python"),
		scriptPath: filepath.Join(moduleRoot, "python", "image_to_polylines.py"),
	}, nil
}

func (s *strokeGenerator) Name() resource.Name {
	return s.name
}

// generateArgs is the DoCommand payload for the "generate" verb.
// Paper geometry lives here (per-call) because it depends on which
// drawer/paper the polylines are ultimately drawn on.
type generateArgs struct {
	ImageB64      string  `json:"image_b64"`
	PaperWidthMM  float64 `json:"paper_width_mm"`
	PaperHeightMM float64 `json:"paper_height_mm"`
	MarginMM      float64 `json:"margin_mm"`
	Rotate        int     `json:"rotate"`
	Mirror        bool    `json:"mirror"`
	// AutoRotate, when nil, uses the Python default (true) that picks
	// between 0° and 90° to maximize paper coverage. Set to false to
	// use Rotate verbatim.
	AutoRotate *bool `json:"auto_rotate,omitempty"`
}

func parseGenerateArgs(payload interface{}) (*generateArgs, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}
	var a generateArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("parse payload: %w", err)
	}
	if a.ImageB64 == "" {
		return nil, errors.New("image_b64 is required")
	}
	if a.PaperWidthMM <= 0 {
		return nil, fmt.Errorf("paper_width_mm must be > 0, got %g", a.PaperWidthMM)
	}
	if a.PaperHeightMM <= 0 {
		return nil, fmt.Errorf("paper_height_mm must be > 0, got %g", a.PaperHeightMM)
	}
	if a.MarginMM < 0 {
		return nil, fmt.Errorf("margin_mm must be >= 0, got %g", a.MarginMM)
	}
	if a.MarginMM == 0 {
		a.MarginMM = defaultMargin
	}
	switch a.Rotate {
	case 0, 90, 180, 270:
	default:
		return nil, fmt.Errorf("rotate must be 0, 90, 180, or 270, got %d", a.Rotate)
	}
	return &a, nil
}

func (s *strokeGenerator) buildCLIArgs(a *generateArgs) []string {
	args := []string{
		"--paper-width-mm", strconv.FormatFloat(a.PaperWidthMM, 'f', -1, 64),
		"--paper-height-mm", strconv.FormatFloat(a.PaperHeightMM, 'f', -1, 64),
		"--margin-mm", strconv.FormatFloat(a.MarginMM, 'f', -1, 64),
		"--rotate", strconv.Itoa(a.Rotate),
		"--region", strconv.Itoa(s.cfg.Region),
		"--low", strconv.Itoa(s.cfg.Low),
		"--high", strconv.Itoa(s.cfg.High),
		"--merge", strconv.Itoa(s.cfg.Merge),
		"--prune", strconv.Itoa(s.cfg.Prune),
		"--min-len", strconv.Itoa(s.cfg.MinLen),
		"--smooth", strconv.FormatFloat(s.cfg.Smooth, 'f', -1, 64),
		"--min-dist", strconv.FormatFloat(s.cfg.MinDist, 'f', -1, 64),
	}
	if a.Mirror {
		args = append(args, "--mirror")
	}
	if a.AutoRotate != nil {
		if *a.AutoRotate {
			args = append(args, "--auto-rotate")
		} else {
			args = append(args, "--no-auto-rotate")
		}
	}
	return args
}

func (s *strokeGenerator) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	v, err := verb.Single(cmd)
	if err != nil {
		return nil, err
	}
	switch v {
	case "generate":
		return s.generate(ctx, cmd["generate"])
	default:
		return nil, fmt.Errorf("stroke-generator: unknown verb %q; expected \"generate\"", v)
	}
}

func (s *strokeGenerator) generate(ctx context.Context, payload interface{}) (map[string]interface{}, error) {
	a, err := parseGenerateArgs(payload)
	if err != nil {
		return nil, fmt.Errorf("stroke-generator: %w", err)
	}
	imageBytes, err := base64.StdEncoding.DecodeString(a.ImageB64)
	if err != nil {
		return nil, fmt.Errorf("stroke-generator: decode image_b64: %w", err)
	}
	stdout, err := pyrunner.RunWithStdin(
		ctx, s.logger, bytes.NewReader(imageBytes),
		s.pythonBin, s.scriptPath, s.buildCLIArgs(a)...,
	)
	if err != nil {
		return nil, fmt.Errorf("stroke-generator: %w", err)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(stdout, &resp); err != nil {
		return nil, fmt.Errorf("stroke-generator: parse python output: %w", err)
	}
	return resp, nil
}

func (s *strokeGenerator) Status(_ context.Context) (map[string]interface{}, error) {
	return map[string]interface{}{"state": "ready"}, nil
}
