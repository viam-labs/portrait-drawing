// Package paperfinder implements a Viam generic service that finds portrait
// cards and name tags on the drawing mat with a camera, and hands the drawer
// each one's position as a paper surface.
package paperfinder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/components/camera"
	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/generic"
	"go.viam.com/rdk/spatialmath"
	rutils "go.viam.com/rdk/utils"

	"github.com/viam-labs/portrait-drawing/internal/verb"
	"github.com/viam-labs/portrait-drawing/pyrunner"
)

// Model is the paper-finder service.
var Model = resource.NewModel("viam", "portrait-drawing", "paper-finder")

func init() {
	resource.RegisterService(generic.API, Model,
		resource.Registration[resource.Resource, *Config]{
			Constructor: newPaperFinder,
		},
	)
}

const (
	kindCard    = "card"
	kindNameTag = "name_tag"

	defaultCardWidthMM    = 101.6
	defaultCardHeightMM   = 152.4
	defaultTagWidthMM     = 82.0
	defaultTagHeightMM    = 53.0
	defaultMaxSizeErrorMM = 10.0
	defaultCrossMM        = 10.0
	// surveyPosition is arm-position-saver's "go to" position.
	surveyPosition = 2
	// minCalibrationPoints is what a homography needs, plus spares so a bad
	// cross shows up in the residuals instead of bending the fit.
	minCalibrationPoints = 5
)

// Config is the paper-finder service configuration.
type Config struct {
	Camera string `json:"camera"`
	// SourceName picks the camera's colour image when it returns several, such
	// as a RealSense's "color" and "depth".
	SourceName string `json:"source_name,omitempty"`
	// SurveyPose is an arm-position-saver switch the arm goes to before every
	// photo, so the camera always sees the mat from where it was calibrated.
	SurveyPose string `json:"survey_pose,omitempty"`
	// ViewCamera is a viam:frame-buffer:camera each look and find shows its
	// photo on, with what it found drawn over it.
	ViewCamera string `json:"view_camera,omitempty"`
	// Drawer draws the calibration crosses.
	Drawer string `json:"drawer,omitempty"`
	// ToolOrientation is the pen-down orientation every paper is drawn with.
	ToolOrientation *spatialmath.OrientationConfig `json:"tool_orientation"`
	// PenDownZ is the arm's Z with the pen tip on the mat.
	PenDownZ *float64 `json:"pen_down_z"`
	// CalibrationPoints are where the drawer draws a cross for calibrate, in
	// arm X/Y. They must lie on paper and in the camera's view.
	CalibrationPoints []r3.Vector `json:"calibration_points,omitempty"`
	CrossMM           float64     `json:"cross_mm,omitempty"`
	// Near is what "nearest" sheet means; unset is the arm's base.
	Near *r3.Vector `json:"near,omitempty"`

	CardWidthMM  float64 `json:"card_width_mm,omitempty"`
	CardHeightMM float64 `json:"card_height_mm,omitempty"`
	// NameTagWidthMM and NameTagHeightMM are the label's writing area, inside
	// its white backing.
	NameTagWidthMM  float64 `json:"name_tag_width_mm,omitempty"`
	NameTagHeightMM float64 `json:"name_tag_height_mm,omitempty"`
	// MaxSizeErrorMM is how far a sheet's measured size may be from its kind's
	// before it is rejected as something else lying on the mat.
	MaxSizeErrorMM float64 `json:"max_size_error_mm,omitempty"`
}

// Validate returns implicit dependencies and any config errors.
func (cfg *Config) Validate(path string) ([]string, []string, error) {
	if cfg.Camera == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "camera")
	}
	if cfg.ToolOrientation == nil {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "tool_orientation")
	}
	if _, err := cfg.ToolOrientation.ParseConfig(); err != nil {
		return nil, nil, fmt.Errorf("tool_orientation: %w", err)
	}
	if cfg.PenDownZ == nil {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "pen_down_z")
	}
	for name, v := range map[string]float64{
		"cross_mm": cfg.CrossMM, "card_width_mm": cfg.CardWidthMM, "card_height_mm": cfg.CardHeightMM,
		"name_tag_width_mm": cfg.NameTagWidthMM, "name_tag_height_mm": cfg.NameTagHeightMM,
		"max_size_error_mm": cfg.MaxSizeErrorMM,
	} {
		if v < 0 {
			return nil, nil, fmt.Errorf("%s must be >= 0 (0 uses the default), got %g", name, v)
		}
	}
	if n := len(cfg.CalibrationPoints); n > 0 && n < minCalibrationPoints {
		return nil, nil, fmt.Errorf("calibration_points needs at least %d points, got %d", minCalibrationPoints, n)
	}
	if len(cfg.CalibrationPoints) > 0 && cfg.Drawer == "" {
		return nil, nil, errors.New("calibration_points is set but drawer is not; calibrate draws the crosses with it")
	}
	deps := []string{cfg.Camera}
	if cfg.SurveyPose != "" {
		deps = append(deps, cfg.SurveyPose)
	}
	if cfg.Drawer != "" {
		deps = append(deps, cfg.Drawer)
	}
	if cfg.ViewCamera != "" {
		deps = append(deps, cfg.ViewCamera)
	}
	return deps, nil, nil
}

func (cfg *Config) applyDefaults() {
	def := func(v *float64, d float64) {
		if *v == 0 {
			*v = d
		}
	}
	def(&cfg.CardWidthMM, defaultCardWidthMM)
	def(&cfg.CardHeightMM, defaultCardHeightMM)
	def(&cfg.NameTagWidthMM, defaultTagWidthMM)
	def(&cfg.NameTagHeightMM, defaultTagHeightMM)
	def(&cfg.MaxSizeErrorMM, defaultMaxSizeErrorMM)
	def(&cfg.CrossMM, defaultCrossMM)
}

type imager interface {
	Images(ctx context.Context, filterSourceNames []string, extra map[string]interface{}) (
		[]camera.NamedImage, resource.ResponseMetadata, error)
}

type mover interface {
	SetPosition(ctx context.Context, position uint32, extra map[string]interface{}) error
}

type commander interface {
	DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error)
}

// runner runs find_paper.py with stdin and the given subcommand and flags.
type runner func(ctx context.Context, stdin []byte, args ...string) ([]byte, error)

// calibration is the image-to-arm homography, kept on disk so a restart does
// not need the crosses drawn again.
type calibration struct {
	Homography [3][3]float64    `json:"homography"`
	RMSMM      float64          `json:"rms_mm"`
	MaxMM      float64          `json:"max_mm"`
	Matched    []map[string]any `json:"matched"`
	Unmatched  [][]float64      `json:"unmatched_arm_points"`
	FittedAt   time.Time        `json:"fitted_at"`
}

func (c *calibration) toArm(u, v float64) r3.Vector {
	h := c.Homography
	w := h[2][0]*u + h[2][1]*v + h[2][2]
	return r3.Vector{X: (h[0][0]*u + h[0][1]*v + h[0][2]) / w, Y: (h[1][0]*u + h[1][1]*v + h[1][2]) / w}
}

type paperFinder struct {
	resource.AlwaysRebuild
	resource.TriviallyCloseable

	name        resource.Name
	logger      logging.Logger
	cfg         *Config
	camera      imager
	survey      mover
	drawer      commander
	view        commander
	run         runner
	orientation map[string]interface{}
	calPath     string
	cal         *calibration
}

func newPaperFinder(
	_ context.Context,
	deps resource.Dependencies,
	conf resource.Config,
	logger logging.Logger,
) (resource.Resource, error) {
	cfg, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return nil, err
	}
	cam, err := camera.FromProvider(deps, cfg.Camera)
	if err != nil {
		return nil, fmt.Errorf("paper-finder: get camera %q: %w", cfg.Camera, err)
	}
	var survey mover
	if cfg.SurveyPose != "" {
		if survey, err = toggleswitch.FromProvider(deps, cfg.SurveyPose); err != nil {
			return nil, fmt.Errorf("paper-finder: get survey_pose %q: %w", cfg.SurveyPose, err)
		}
	}
	var drawer commander
	if cfg.Drawer != "" {
		if drawer, err = generic.FromProvider(deps, cfg.Drawer); err != nil {
			return nil, fmt.Errorf("paper-finder: get drawer %q: %w", cfg.Drawer, err)
		}
	}
	exePath, err := os.Executable()
	if err != nil {
		return nil, err
	}
	moduleRoot := filepath.Dir(filepath.Dir(exePath))
	pythonBin := filepath.Join(moduleRoot, ".venv", "bin", "python")
	scriptPath := filepath.Join(moduleRoot, "python", "find_paper.py")
	run := func(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
		return pyrunner.RunWithStdin(ctx, logger, bytes.NewReader(stdin), pythonBin, scriptPath, args...)
	}
	dataDir := os.Getenv("VIAM_MODULE_DATA")
	if dataDir == "" {
		dataDir = moduleRoot
	}
	f, err := newWithClients(conf.ResourceName(), logger, cfg, cam, survey, drawer, run, dataDir)
	if err != nil {
		return nil, err
	}
	if cfg.ViewCamera != "" {
		if f.view, err = camera.FromProvider(deps, cfg.ViewCamera); err != nil {
			return nil, fmt.Errorf("paper-finder: get view_camera %q: %w", cfg.ViewCamera, err)
		}
	}
	return f, nil
}

func newWithClients(
	name resource.Name, logger logging.Logger, cfg *Config,
	cam imager, survey mover, drawer commander, run runner, dataDir string,
) (*paperFinder, error) {
	cfg.applyDefaults()
	orientation, err := orientationPayload(cfg.ToolOrientation)
	if err != nil {
		return nil, fmt.Errorf("paper-finder: tool_orientation: %w", err)
	}
	f := &paperFinder{
		name: name, logger: logger, cfg: cfg, camera: cam, survey: survey, drawer: drawer, run: run,
		orientation: orientation,
		calPath:     filepath.Join(dataDir, name.ShortName()+".calibration.json"),
	}
	//nolint:gosec // calPath is the module's data directory plus this resource's name.
	if raw, err := os.ReadFile(f.calPath); err == nil {
		var cal calibration
		if err := json.Unmarshal(raw, &cal); err != nil {
			logger.Warnf("paper-finder: ignoring unreadable calibration %s: %v", f.calPath, err)
		} else {
			f.cal = &cal
		}
	}
	return f, nil
}

func orientationPayload(o *spatialmath.OrientationConfig) (map[string]interface{}, error) {
	raw, err := json.Marshal(o)
	if err != nil {
		return nil, err
	}
	var out map[string]interface{}
	return out, json.Unmarshal(raw, &out)
}

func (f *paperFinder) Name() resource.Name {
	return f.name
}

func (f *paperFinder) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	v, err := verb.Single(cmd)
	if err != nil {
		return nil, err
	}
	switch v {
	case "find":
		return f.find(ctx, cmd["find"])
	case "look":
		return f.look(ctx)
	case "calibrate":
		return f.calibrate(ctx, cmd["calibrate"])
	case "status":
		return f.Status(ctx)
	default:
		return nil, fmt.Errorf("paper-finder: unknown verb %q; expected \"find\", \"look\", \"calibrate\", or \"status\"", v)
	}
}

func decode(payload, into interface{}) error {
	if payload == nil {
		return nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("parse payload: %w", err)
	}
	return nil
}

// photo moves to the survey pose and returns the colour image's bytes.
func (f *paperFinder) photo(ctx context.Context) ([]byte, error) {
	if f.survey != nil {
		if err := f.survey.SetPosition(ctx, surveyPosition, nil); err != nil {
			return nil, fmt.Errorf("go to survey_pose %q: %w", f.cfg.SurveyPose, err)
		}
	}
	var filter []string
	if f.cfg.SourceName != "" {
		filter = []string{f.cfg.SourceName}
	}
	images, _, err := f.camera.Images(ctx, filter, nil)
	if err != nil {
		return nil, fmt.Errorf("read camera %q: %w", f.cfg.Camera, err)
	}
	for i := range images {
		if images[i].MimeType() == rutils.MimeTypeRawDepth {
			continue
		}
		if f.cfg.SourceName != "" && images[i].SourceName != f.cfg.SourceName {
			continue
		}
		return images[i].Bytes(ctx)
	}
	return nil, fmt.Errorf("camera %q returned no colour image", f.cfg.Camera)
}

type findArgs struct {
	// Kind is "card" or "name_tag"; empty returns both.
	Kind        string     `json:"kind,omitempty"`
	IncludeUsed bool       `json:"include_used,omitempty"`
	Near        *r3.Vector `json:"near,omitempty"`
}

// pySheet is one sheet as find_paper.py reports it, in pixels.
type pySheet struct {
	Kind        string       `json:"kind"`
	Used        bool         `json:"used"`
	CornersPx   [][2]float64 `json:"corners_px"`
	Strokes     *int         `json:"strokes,omitempty"`
	Handwriting *int         `json:"handwriting,omitempty"`
	Logo        any          `json:"logo,omitempty"`
}

func (f *paperFinder) find(ctx context.Context, payload interface{}) (map[string]interface{}, error) {
	var a findArgs
	if err := decode(payload, &a); err != nil {
		return nil, fmt.Errorf("paper-finder: %w", err)
	}
	if a.Kind != "" && a.Kind != kindCard && a.Kind != kindNameTag {
		return nil, fmt.Errorf("paper-finder: kind must be %q or %q, got %q", kindCard, kindNameTag, a.Kind)
	}
	if f.cal == nil {
		return nil, errors.New("paper-finder: not calibrated; run calibrate first")
	}
	stdout, err := f.detect(ctx)
	if err != nil {
		return nil, fmt.Errorf("paper-finder: %w", err)
	}
	var found struct {
		Sheets []pySheet `json:"sheets"`
	}
	if err := json.Unmarshal(stdout, &found); err != nil {
		return nil, fmt.Errorf("paper-finder: parse python output: %w", err)
	}
	near := r3.Vector{}
	if f.cfg.Near != nil {
		near = *f.cfg.Near
	}
	if a.Near != nil {
		near = *a.Near
	}

	sheets := []map[string]interface{}{}
	rejected := []map[string]interface{}{}
	for _, s := range found.Sheets {
		if a.Kind != "" && s.Kind != a.Kind {
			continue
		}
		out, reason := f.place(s, near)
		if reason != "" {
			rejected = append(rejected, map[string]interface{}{"kind": s.Kind, "reason": reason, "corners_px": pixelList(s.CornersPx)})
			continue
		}
		if s.Used && !a.IncludeUsed {
			continue
		}
		sheets = append(sheets, out)
	}
	sort.SliceStable(sheets, func(i, j int) bool {
		return sheets[i]["distance_mm"].(float64) < sheets[j]["distance_mm"].(float64)
	})
	return map[string]interface{}{"sheets": sheets, "rejected": rejected}, nil
}

// detect photographs the mat and finds the sheets on it, in pixels, showing
// the result on view_camera when there is one.
func (f *paperFinder) detect(ctx context.Context) ([]byte, error) {
	img, err := f.photo(ctx)
	if err != nil {
		return nil, err
	}
	if f.view == nil {
		return f.run(ctx, img, "sheets")
	}
	stdout, err := f.run(ctx, img, "sheets", "--annotate")
	if err != nil {
		return nil, err
	}
	var annotated struct {
		PNG string `json:"annotated_png_b64"`
	}
	if err := json.Unmarshal(stdout, &annotated); err == nil && annotated.PNG != "" {
		if _, err := f.view.DoCommand(ctx, map[string]interface{}{
			"set_image": map[string]interface{}{"image_b64": annotated.PNG, "source_name": "paper-view"},
		}); err != nil {
			// The view is for people watching; losing it is no reason to stop finding paper.
			f.logger.Warnf("paper-finder: could not show the view on %q: %v", f.cfg.ViewCamera, err)
		}
	}
	return stdout, nil
}

// look shows what the finder sees, in pixels, without needing a calibration.
func (f *paperFinder) look(ctx context.Context) (map[string]interface{}, error) {
	stdout, err := f.detect(ctx)
	if err != nil {
		return nil, fmt.Errorf("paper-finder: %w", err)
	}
	var found map[string]interface{}
	if err := json.Unmarshal(stdout, &found); err != nil {
		return nil, fmt.Errorf("paper-finder: parse python output: %w", err)
	}
	if f.view != nil {
		delete(found, "annotated_png_b64")
		found["view_camera"] = f.cfg.ViewCamera
	}
	delete(found, "mat_px")
	return found, nil
}

// pixelList is pts as a DoCommand response can carry them: structpb takes
// slices, not fixed-size arrays.
func pixelList(pts [][2]float64) []interface{} {
	out := make([]interface{}, len(pts))
	for i, p := range pts {
		out[i] = []interface{}{p[0], p[1]}
	}
	return out
}

// place turns a sheet's pixel corners into arm coordinates and a drawer paper
// payload, or says why it is not a sheet this finder should draw on.
func (f *paperFinder) place(s pySheet, near r3.Vector) (map[string]interface{}, string) {
	if len(s.CornersPx) != 4 {
		return nil, fmt.Sprintf("has %d corners, not 4", len(s.CornersPx))
	}
	var c [4]r3.Vector
	for i, p := range s.CornersPx {
		c[i] = f.cal.toArm(p[0], p[1])
	}
	across := c[1].Sub(c[0])
	down := c[3].Sub(c[0])
	// The drawer's page runs down clockwise from across, seen from above. A
	// calibration that mirrors the image would write names backwards.
	if across.X*down.Y-across.Y*down.X > 0 {
		return nil, "its corners map counter-clockwise; the calibration mirrors the image"
	}
	measuredW := (c[0].Distance(c[1]) + c[3].Distance(c[2])) / 2
	measuredH := (c[0].Distance(c[3]) + c[1].Distance(c[2])) / 2
	w, h, backing := f.cfg.CardWidthMM, f.cfg.CardHeightMM, 0.0
	if s.Kind == kindNameTag {
		w, h = f.cfg.NameTagWidthMM, f.cfg.NameTagHeightMM
		// What the camera outlines is the label's white backing; the label sits inside it.
		backing = 2 * defaultBackingMarginMM
	}
	if math.Abs(measuredW-w-backing) > f.cfg.MaxSizeErrorMM || math.Abs(measuredH-h-backing) > f.cfg.MaxSizeErrorMM {
		return nil, fmt.Sprintf("measures %.0f x %.0f mm, not a %s (%.0f x %.0f)", measuredW, measuredH, s.Kind, w, h)
	}
	centre := c[0].Add(c[1]).Add(c[2]).Add(c[3]).Mul(0.25)
	dir := across.Normalize()
	pageDown := r3.Vector{X: dir.Y, Y: -dir.X}
	z := *f.cfg.PenDownZ
	tl := centre.Sub(dir.Mul(w / 2)).Sub(pageDown.Mul(h / 2))
	tr := tl.Add(dir.Mul(w))
	tl.Z, tr.Z = z, z
	vec := func(v r3.Vector) map[string]interface{} { return map[string]interface{}{"x": v.X, "y": v.Y, "z": v.Z} }
	out := map[string]interface{}{
		"kind":        s.Kind,
		"used":        s.Used,
		"center":      vec(r3.Vector{X: centre.X, Y: centre.Y, Z: z}),
		"distance_mm": math.Hypot(centre.X-near.X, centre.Y-near.Y),
		"measured_mm": []float64{measuredW, measuredH},
		"corners_px":  pixelList(s.CornersPx),
		"paper": map[string]interface{}{
			"top_left_corner":  map[string]interface{}{"translation": vec(tl), "orientation": f.orientation},
			"top_right_corner": vec(tr),
			"width_mm":         w,
			"height_mm":        h,
		},
	}
	if s.Strokes != nil {
		out["strokes"] = *s.Strokes
	}
	if s.Handwriting != nil {
		out["handwriting"] = *s.Handwriting
	}
	if s.Kind == kindNameTag {
		out["logo"] = s.Logo
	}
	return out, ""
}

// defaultBackingMarginMM is the white border a name tag's backing shows round its label.
const defaultBackingMarginMM = 1.75

type calibrateArgs struct {
	// Redraw false fits from crosses already on the mat instead of drawing them again.
	Redraw *bool `json:"redraw,omitempty"`
}

// calibrate draws a cross at each calibration point, photographs them from the
// survey pose, and fits the homography that turns their pixels back into the
// arm positions that drew them.
func (f *paperFinder) calibrate(ctx context.Context, payload interface{}) (map[string]interface{}, error) {
	var a calibrateArgs
	if err := decode(payload, &a); err != nil {
		return nil, fmt.Errorf("paper-finder: %w", err)
	}
	if len(f.cfg.CalibrationPoints) == 0 {
		return nil, errors.New("paper-finder: calibration_points is not set")
	}
	if a.Redraw == nil || *a.Redraw {
		draws, err := f.crossDrawCommands()
		if err != nil {
			return nil, fmt.Errorf("paper-finder: %w", err)
		}
		for i, draw := range draws {
			f.logger.Infof("paper-finder: drawing calibration cross %d of %d", i+1, len(draws))
			resp, err := f.drawer.DoCommand(ctx, draw)
			if err != nil {
				return nil, fmt.Errorf("paper-finder: draw calibration cross %d: %w", i+1, err)
			}
			if paused, _ := resp["paused"].(bool); paused {
				return nil, errors.New("paper-finder: the drawer was paused while drawing the calibration crosses")
			}
		}
	}
	img, err := f.photo(ctx)
	if err != nil {
		return nil, fmt.Errorf("paper-finder: %w", err)
	}
	stdout, err := f.run(ctx, img, "crosses")
	if err != nil {
		return nil, fmt.Errorf("paper-finder: %w", err)
	}
	var crosses struct {
		Crosses [][2]float64 `json:"crosses"`
	}
	if err = json.Unmarshal(stdout, &crosses); err != nil {
		return nil, fmt.Errorf("paper-finder: parse crosses: %w", err)
	}
	arm := make([][2]float64, len(f.cfg.CalibrationPoints))
	for i, p := range f.cfg.CalibrationPoints {
		arm[i] = [2]float64{p.X, p.Y}
	}
	pairs, err := json.Marshal(map[string]interface{}{"image_points": crosses.Crosses, "arm_points": arm})
	if err != nil {
		return nil, err
	}
	stdout, err = f.run(ctx, pairs, "fit")
	if err != nil {
		return nil, fmt.Errorf("paper-finder: fit %d crosses to %d points: %w", len(crosses.Crosses), len(arm), err)
	}
	var cal calibration
	if err = json.Unmarshal(stdout, &cal); err != nil {
		return nil, fmt.Errorf("paper-finder: parse fit: %w", err)
	}
	if len(cal.Matched) < minCalibrationPoints {
		return nil, fmt.Errorf("paper-finder: only %d of %d crosses matched; need %d to trust the fit",
			len(cal.Matched), len(arm), minCalibrationPoints)
	}
	cal.FittedAt = time.Now().UTC()
	raw, err := json.MarshalIndent(cal, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(f.calPath, raw, 0o600); err != nil {
		return nil, fmt.Errorf("paper-finder: save calibration: %w", err)
	}
	f.cal = &cal
	f.logger.Infof("paper-finder: calibrated from %d crosses, rms %.2f mm, worst %.2f mm", len(cal.Matched), cal.RMSMM, cal.MaxMM)
	return map[string]interface{}{
		"crosses_found":    len(crosses.Crosses),
		"matched":          len(cal.Matched),
		"unmatched":        cal.Unmatched,
		"rms_mm":           cal.RMSMM,
		"max_mm":           cal.MaxMM,
		"calibration_file": f.calPath,
	}, nil
}

// crossDrawCommands is one draw per cross, each on its own small page around
// its point: the crosses go on separate sheets, which on a soft mat sit at
// heights a single touched plane through three of them does not fit.
func (f *paperFinder) crossDrawCommands() ([]map[string]interface{}, error) {
	if f.drawer == nil {
		return nil, errors.New("drawer is not set")
	}
	half := f.cfg.CrossMM / 2
	margin := half + 5
	out := make([]map[string]interface{}, 0, len(f.cfg.CalibrationPoints))
	for _, p := range f.cfg.CalibrationPoints {
		// The page's top-left is its smallest X and largest Y, running +X across and -Y down.
		out = append(out, map[string]interface{}{"draw": map[string]interface{}{
			"polylines": []interface{}{
				[]interface{}{[]float64{margin - half, margin}, []float64{margin + half, margin}},
				[]interface{}{[]float64{margin, margin - half}, []float64{margin, margin + half}},
			},
			"paper": map[string]interface{}{
				"top_left_corner": map[string]interface{}{
					"translation": map[string]interface{}{"x": p.X - margin, "y": p.Y + margin, "z": *f.cfg.PenDownZ},
					"orientation": f.orientation,
				},
				"width_mm":  2 * margin,
				"height_mm": 2 * margin,
			},
		}})
	}
	return out, nil
}

func (f *paperFinder) Status(_ context.Context) (map[string]interface{}, error) {
	out := map[string]interface{}{"calibrated": f.cal != nil}
	if f.cal != nil {
		out["rms_mm"] = f.cal.RMSMM
		out["max_mm"] = f.cal.MaxMM
		out["matched"] = len(f.cal.Matched)
		out["fitted_at"] = f.cal.FittedAt.Format(time.RFC3339)
	}
	return out, nil
}
