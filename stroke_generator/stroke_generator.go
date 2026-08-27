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
	// Pointer knobs are nil-as-omitted so the Python pipeline's own default
	// applies; an explicit 0 is honoured.

	// Size is the long side, in pixels, the line model sees. Larger recovers
	// finer features — lashes, nostrils — at a roughly linear cost in strokes.
	Size *int `json:"size,omitempty"`
	// Clahe lifts local contrast before the model runs. A face lit from above,
	// or underexposed because the camera metered for a window, otherwise draws
	// faintly around the mouth and nose while hair comes out strong.
	Clahe *float64 `json:"clahe,omitempty"`
	// Sigma smooths the model response before ridges are traced. Too little and
	// non-maximum suppression spawns a spur at every ripple.
	Sigma *float64 `json:"sigma,omitempty"`
	// Low and High are the hysteresis bounds on ridge strength: a stroke above
	// Low is kept when it connects to one above High, so faint features survive
	// by attachment rather than by their own amplitude.
	Low  *float64 `json:"low,omitempty"`
	High *float64 `json:"high,omitempty"`

	IsolateSubject *bool `json:"isolate_subject,omitempty"`
	// MaxDepthMM and MinDepthMM bound depth-based segmentation, used only when
	// the caller supplies a depth frame. The near bound matters as much as the
	// far one on an arm-mounted camera: the pen and its holder sit a few hundred
	// millimetres from the lens and are drawn into every portrait otherwise.
	MaxDepthMM *float64 `json:"max_depth_mm,omitempty"`
	MinDepthMM *float64 `json:"min_depth_mm,omitempty"`

	// CropFace crops to the subject before tracing, measured in multiples of
	// the detected face box. A photo taken from across a room spends most of
	// the paper on torso and room, leaving the face too small for its features
	// to survive; cropping in face units keeps framing consistent however far
	// away the subject sits. Nil leaves cropping off.
	CropFace  *bool    `json:"crop_face,omitempty"`
	CropAbove *float64 `json:"crop_above,omitempty"`
	CropBelow *float64 `json:"crop_below,omitempty"`
	CropSides *float64 `json:"crop_sides,omitempty"`

	Prune   int     `json:"prune,omitempty"`
	MinLen  int     `json:"min_len,omitempty"`
	Smooth  float64 `json:"smooth,omitempty"`
	MinDist float64 `json:"min_dist,omitempty"`
}

const (
	defaultPrune   = 20
	defaultMinLen  = 36
	defaultSmooth  = 2.0
	defaultMinDist = 3.0
	defaultMargin  = 40.0
)

// Validate returns implicit dependencies and any config errors.
func (cfg *Config) Validate(_ string) ([]string, []string, error) {
	for name, v := range map[string]*float64{
		"clahe":        cfg.Clahe,
		"sigma":        cfg.Sigma,
		"low":          cfg.Low,
		"high":         cfg.High,
		"max_depth_mm": cfg.MaxDepthMM,
		"min_depth_mm": cfg.MinDepthMM,
		"crop_above":   cfg.CropAbove,
		"crop_below":   cfg.CropBelow,
		"crop_sides":   cfg.CropSides,
	} {
		if v != nil && *v < 0 {
			return nil, nil, fmt.Errorf("%s must be >= 0, got %g", name, *v)
		}
	}
	if cfg.Size != nil && *cfg.Size < 64 {
		return nil, nil, fmt.Errorf("size must be >= 64, got %d", *cfg.Size)
	}
	if cfg.Low != nil && cfg.High != nil && *cfg.Low > *cfg.High {
		return nil, nil, fmt.Errorf("low (%g) must not exceed high (%g)", *cfg.Low, *cfg.High)
	}
	if cfg.MinDepthMM != nil && cfg.MaxDepthMM != nil && *cfg.MinDepthMM >= *cfg.MaxDepthMM {
		return nil, nil, fmt.Errorf(
			"min_depth_mm (%g) must be less than max_depth_mm (%g)", *cfg.MinDepthMM, *cfg.MaxDepthMM)
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
	// Pointer knobs are left nil-as-is (omitted -> Python default).
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
	ImageB64 string `json:"image_b64"`
	// DepthB64 is an optional Viam DEPTHMAP frame aligned to the photo. When
	// present it replaces colour segmentation, which cannot separate dark hair
	// from dark clutter behind it.
	DepthB64      string  `json:"depth_b64,omitempty"`
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
		"--prune", strconv.Itoa(s.cfg.Prune),
		"--min-len", strconv.Itoa(s.cfg.MinLen),
		"--smooth", strconv.FormatFloat(s.cfg.Smooth, 'f', -1, 64),
		"--min-dist", strconv.FormatFloat(s.cfg.MinDist, 'f', -1, 64),
	}
	if s.cfg.Size != nil {
		args = append(args, "--size", strconv.Itoa(*s.cfg.Size))
	}
	for flag, v := range map[string]*float64{
		"--clahe":        s.cfg.Clahe,
		"--sigma":        s.cfg.Sigma,
		"--low":          s.cfg.Low,
		"--high":         s.cfg.High,
		"--max-depth-mm": s.cfg.MaxDepthMM,
		"--min-depth-mm": s.cfg.MinDepthMM,
		"--crop-above":   s.cfg.CropAbove,
		"--crop-below":   s.cfg.CropBelow,
		"--crop-sides":   s.cfg.CropSides,
	} {
		if v != nil {
			args = append(args, flag, strconv.FormatFloat(*v, 'f', -1, 64))
		}
	}
	for flag, v := range map[string]*bool{
		"isolate-subject": s.cfg.IsolateSubject,
		"crop-face":       s.cfg.CropFace,
	} {
		if v == nil {
			continue
		}
		if *v {
			args = append(args, "--"+flag)
		} else {
			args = append(args, "--no-"+flag)
		}
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
	args := s.buildCLIArgs(a)

	if a.DepthB64 != "" {
		// Passed by file rather than on stdin, which the photo already uses, or
		// argv, which a megabyte of depth would overflow.
		depthBytes, decodeErr := base64.StdEncoding.DecodeString(a.DepthB64)
		if decodeErr != nil {
			return nil, fmt.Errorf("stroke-generator: decode depth_b64: %w", decodeErr)
		}
		path, cleanup, tmpErr := writeTemp(depthBytes)
		if tmpErr != nil {
			return nil, fmt.Errorf("stroke-generator: %w", tmpErr)
		}
		defer cleanup()
		args = append(args, "--depth", path)
	}

	stdout, err := pyrunner.RunWithStdin(
		ctx, s.logger, bytes.NewReader(imageBytes),
		s.pythonBin, s.scriptPath, args...,
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

func writeTemp(data []byte) (string, func(), error) {
	f, err := os.CreateTemp("", "stroke-depth-*.bin")
	if err != nil {
		return "", nil, fmt.Errorf("create temp file: %w", err)
	}
	cleanup := func() { _ = os.Remove(f.Name()) }
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return "", nil, fmt.Errorf("write temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("close temp file: %w", err)
	}
	return f.Name(), cleanup, nil
}

func (s *strokeGenerator) Status(_ context.Context) (map[string]interface{}, error) {
	return map[string]interface{}{"state": "ready"}, nil
}
