// Package textwriter implements a Viam generic service that writes a line of text on a card with the drawer.
package textwriter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/generic"

	"github.com/viam-labs/portrait-drawing/drawer"
	"github.com/viam-labs/portrait-drawing/internal/verb"
	"github.com/viam-labs/portrait-drawing/pyrunner"
)

// Model is the text-writer service.
var Model = resource.NewModel("viam", "portrait-drawing", "text-writer")

func init() {
	resource.RegisterService(generic.API, Model,
		resource.Registration[resource.Resource, *Config]{
			Constructor: newTextWriter,
		},
	)
}

// Config is the text-writer service configuration. The card is its own
// surface, taught the same way as the drawer's paper, and is sent along with
// every draw so the drawer's arm writes here while its own paper stays where
// portraits go.
type Config struct {
	Drawer string `json:"drawer"`
	// PaperTopLeftCorner is the tool pose with the pen tip on the card's
	// top-left corner, as its reader sees it. Its orientation is held for the
	// whole drawing.
	PaperTopLeftCorner *drawer.PoseConfig `json:"paper_top_left_corner"`
	// PaperTopRightCorner, when set, is where the pen touches the card's
	// top-right corner, which fixes which way the text runs. Unset keeps the
	// card along the arm's +X.
	PaperTopRightCorner *r3.Vector `json:"paper_top_right_corner,omitempty"`
	PaperWidthMM        float64    `json:"paper_width_mm"`
	PaperHeightMM       float64    `json:"paper_height_mm"`
	// Fill is the fraction of the card the text fills on whichever axis binds
	// first, when no cap height is given. Default 0.8.
	Fill float64 `json:"fill,omitempty"`
	// CapHeightMM fixes the capital height instead. Text that would overflow
	// the card at that height is refused rather than drawn off the edge.
	CapHeightMM float64 `json:"cap_height_mm,omitempty"`
	// StrokeFont is a Hershey single-stroke face. Default "cursive".
	StrokeFont string `json:"stroke_font,omitempty"`
	// Outline traces TTF outlines from Font instead of a stroke font, which
	// draws hollow letters.
	Outline bool   `json:"outline,omitempty"`
	Font    string `json:"font,omitempty"`
	// Join keeps the pen down between the letters of a script face. Nil means on.
	Join      *bool   `json:"join,omitempty"`
	SpacingMM float64 `json:"spacing_mm,omitempty"`
}

const defaultFill = 0.8

// Validate returns implicit dependencies and any config errors.
func (cfg *Config) Validate(path string) ([]string, []string, error) {
	if cfg.Drawer == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "drawer")
	}
	if err := cfg.paper().Validate(path, "paper_"); err != nil {
		return nil, nil, err
	}
	if cfg.Fill < 0 || cfg.Fill > 1 {
		return nil, nil, fmt.Errorf("fill must be in (0, 1], got %g", cfg.Fill)
	}
	if cfg.CapHeightMM < 0 {
		return nil, nil, fmt.Errorf("cap_height_mm must be >= 0, got %g", cfg.CapHeightMM)
	}
	if cfg.SpacingMM < 0 {
		return nil, nil, fmt.Errorf("spacing_mm must be >= 0, got %g", cfg.SpacingMM)
	}
	if cfg.Outline && cfg.StrokeFont != "" {
		return nil, nil, errors.New("outline and stroke_font are exclusive")
	}
	return []string{cfg.Drawer}, nil, nil
}

func (cfg *Config) paper() *drawer.Paper {
	return &drawer.Paper{
		TopLeftCorner:  cfg.PaperTopLeftCorner,
		TopRightCorner: cfg.PaperTopRightCorner,
		WidthMM:        cfg.PaperWidthMM,
		HeightMM:       cfg.PaperHeightMM,
	}
}

// paperPayload is the card as a plain map, which is what a DoCommand carries.
// Built by hand because r3.Vector has no JSON tags and would come out as X/Y/Z.
func (cfg *Config) paperPayload() (map[string]interface{}, error) {
	vec := func(v r3.Vector) map[string]interface{} {
		return map[string]interface{}{"x": v.X, "y": v.Y, "z": v.Z}
	}
	raw, err := json.Marshal(cfg.PaperTopLeftCorner.Orientation)
	if err != nil {
		return nil, err
	}
	var orientation map[string]interface{}
	if err := json.Unmarshal(raw, &orientation); err != nil {
		return nil, err
	}
	out := map[string]interface{}{
		"top_left_corner": map[string]interface{}{
			"translation": vec(cfg.PaperTopLeftCorner.Translation),
			"orientation": orientation,
		},
		"width_mm":  cfg.PaperWidthMM,
		"height_mm": cfg.PaperHeightMM,
	}
	if cfg.PaperTopRightCorner != nil {
		out["top_right_corner"] = vec(*cfg.PaperTopRightCorner)
	}
	return out, nil
}

type commander interface {
	DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error)
}

type runner func(ctx context.Context, args ...string) ([]byte, error)

type textWriter struct {
	resource.AlwaysRebuild
	resource.TriviallyCloseable

	name   resource.Name
	logger logging.Logger
	cfg    *Config
	drawer commander
	run    runner
}

func newTextWriter(
	_ context.Context,
	deps resource.Dependencies,
	conf resource.Config,
	logger logging.Logger,
) (resource.Resource, error) {
	cfg, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return nil, err
	}
	drawer, err := generic.FromProvider(deps, cfg.Drawer)
	if err != nil {
		return nil, fmt.Errorf("text-writer: get drawer %q: %w", cfg.Drawer, err)
	}
	exePath, err := os.Executable()
	if err != nil {
		return nil, err
	}
	moduleRoot := filepath.Dir(filepath.Dir(exePath))
	pythonBin := filepath.Join(moduleRoot, ".venv", "bin", "python")
	scriptPath := filepath.Join(moduleRoot, "python", "text_to_polylines.py")
	run := func(ctx context.Context, args ...string) ([]byte, error) {
		return pyrunner.Run(ctx, logger, pythonBin, scriptPath, args...)
	}
	return newWithClients(conf.ResourceName(), logger, cfg, drawer, run), nil
}

func newWithClients(name resource.Name, logger logging.Logger, cfg *Config, drawer commander, run runner) *textWriter {
	if cfg.Fill == 0 {
		cfg.Fill = defaultFill
	}
	return &textWriter{name: name, logger: logger, cfg: cfg, drawer: drawer, run: run}
}

func (w *textWriter) Name() resource.Name {
	return w.name
}

// writeArgs is the DoCommand payload for the "write" verb.
type writeArgs struct {
	Text string `json:"text"`
	// CapHeightMM and Fill override the configured sizing for this call.
	CapHeightMM float64 `json:"cap_height_mm,omitempty"`
	Fill        float64 `json:"fill,omitempty"`
	// Preview lays the text out and returns the polylines without drawing.
	Preview bool `json:"preview,omitempty"`
}

func parseWriteArgs(payload interface{}) (*writeArgs, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}
	var a writeArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("parse payload: %w", err)
	}
	a.Text = strings.TrimSpace(a.Text)
	if a.Text == "" {
		return nil, errors.New("text is required")
	}
	if a.CapHeightMM < 0 {
		return nil, fmt.Errorf("cap_height_mm must be >= 0, got %g", a.CapHeightMM)
	}
	if a.Fill < 0 || a.Fill > 1 {
		return nil, fmt.Errorf("fill must be in (0, 1], got %g", a.Fill)
	}
	return &a, nil
}

func (w *textWriter) buildCLIArgs(a *writeArgs) []string {
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	args := []string{
		"--text", a.Text,
		"--width-mm", f(w.cfg.PaperWidthMM),
		"--height-mm", f(w.cfg.PaperHeightMM),
	}
	switch {
	case a.CapHeightMM > 0:
		args = append(args, "--cap-mm", f(a.CapHeightMM))
	case a.Fill > 0:
		args = append(args, "--fill", f(a.Fill))
	case w.cfg.CapHeightMM > 0:
		args = append(args, "--cap-mm", f(w.cfg.CapHeightMM))
	default:
		args = append(args, "--fill", f(w.cfg.Fill))
	}
	if w.cfg.Outline {
		args = append(args, "--outline")
		if w.cfg.Font != "" {
			args = append(args, "--font", w.cfg.Font)
		}
	} else if w.cfg.StrokeFont != "" {
		args = append(args, "--stroke-font", w.cfg.StrokeFont)
	}
	if w.cfg.Join != nil && !*w.cfg.Join {
		args = append(args, "--no-join")
	}
	if w.cfg.SpacingMM > 0 {
		args = append(args, "--spacing-mm", f(w.cfg.SpacingMM))
	}
	return args
}

func (w *textWriter) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	v, err := verb.Single(cmd)
	if err != nil {
		return nil, err
	}
	switch v {
	case "write":
		return w.write(ctx, cmd["write"])
	case "status":
		return map[string]interface{}{"state": "ready"}, nil
	default:
		return nil, fmt.Errorf("text-writer: unknown verb %q; expected \"write\" or \"status\"", v)
	}
}

// write lays the text out and draws it, returning only once the drawer has
// finished and lifted the pen, which is what the reception queue relies on.
func (w *textWriter) write(ctx context.Context, payload interface{}) (map[string]interface{}, error) {
	a, err := parseWriteArgs(payload)
	if err != nil {
		return nil, fmt.Errorf("text-writer: %w", err)
	}
	stdout, err := w.run(ctx, w.buildCLIArgs(a)...)
	if err != nil {
		return nil, fmt.Errorf("text-writer: %w", err)
	}
	var layout map[string]interface{}
	if err = json.Unmarshal(stdout, &layout); err != nil {
		return nil, fmt.Errorf("text-writer: parse python output: %w", err)
	}
	polylines, ok := layout["polylines"].([]interface{})
	if !ok || len(polylines) == 0 {
		return nil, errors.New("text-writer: python output has no polylines")
	}
	if a.Preview {
		return layout, nil
	}
	delete(layout, "polylines")
	layout["polylines_total"] = len(polylines)

	paper, err := w.cfg.paperPayload()
	if err != nil {
		return nil, fmt.Errorf("text-writer: %w", err)
	}
	w.logger.Infof("text-writer: writing %q, %d strokes", a.Text, len(polylines))
	resp, err := w.drawer.DoCommand(ctx, map[string]interface{}{
		"draw": map[string]interface{}{"polylines": polylines, "paper": paper},
	})
	if err != nil {
		return nil, fmt.Errorf("text-writer: draw %q: %w", a.Text, err)
	}
	// The drawer returns early with the pen up if it was paused; the text is
	// then on the card only in part, and the caller must not treat it as done.
	if paused, _ := resp["paused"].(bool); paused {
		return nil, fmt.Errorf("text-writer: drawer was paused while writing %q", a.Text)
	}
	for k, v := range resp {
		layout[k] = v
	}
	return layout, nil
}

func (w *textWriter) Status(_ context.Context) (map[string]interface{}, error) {
	return map[string]interface{}{"state": "ready"}, nil
}
