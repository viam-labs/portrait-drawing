// Package drawer implements a Viam generic service that draws polylines on paper with a robotic arm.
package drawer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/components/arm"
	"go.viam.com/rdk/components/camera"
	toggleswitch "go.viam.com/rdk/components/switch"
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
	// HomePose, if set, is the tool pose the arm rests at between drawings.
	// CapturePose takes precedence over it when both are set.
	HomePose *poseConfig `json:"home_pose,omitempty"`
	// AllowedCollisions lists frame pairs the planner should not treat as a
	// collision. Anything bolted to the arm — a wrist camera, a pen holder —
	// overlaps the link it is mounted on in every configuration, which the
	// planner cannot distinguish from a real collision.
	AllowedCollisions []AllowedCollision `json:"allowed_collisions,omitempty"`
	// CapturePose, if set, names an arm-position-saver switch holding the
	// pose the arm rests at — the one its camera frames the subject from.
	// Preferred over HomePose: the pose is taught by jogging the arm rather
	// than by working out tool coordinates by hand.
	CapturePose string `json:"capture_pose,omitempty"`
	// StrokeGenerator, if set, is the name of a stroke-generator service
	// the drawer calls from the draw_image verb to turn a photo into
	// polylines in one DoCommand.
	StrokeGenerator string `json:"stroke_generator,omitempty"`
	// Photo, if set, is the name of a frame-buffer camera the drawer
	// triggers and then reads from in the capture_and_draw verb. The camera
	// is assumed to be on the arm, so HomePose doubles as the capture pose.
	Photo string `json:"photo,omitempty"`
	// PreviewCamera, if set, is the name of a frame-buffer camera the drawer
	// pushes rendered previews into, so they are viewable in the Viam app
	// instead of coming back as base64.
	PreviewCamera      string                                     `json:"preview_camera,omitempty"`
	InputRangeOverride map[string]map[string]referenceframe.Limit `json:"input_range_override,omitempty"`
}

// AllowedCollision is a pair of frame names permitted to overlap. Use the names
// exactly as the planner prints them, e.g. "arm-1:wrist_link" and "camera-1_origin".
type AllowedCollision struct {
	Frame1 string `json:"frame1"`
	Frame2 string `json:"frame2"`
}

type poseConfig struct {
	Translation r3.Vector                      `json:"translation"`
	Orientation *spatialmath.OrientationConfig `json:"orientation"`
}

const (
	defaultLiftOffZMM     = 5.0
	defaultPreviewPxPerMM = 2.0
	// capturePoseGoToLabel is the switch position an arm-position-saver
	// labels "go to". Matching the label rather than hardcoding an index
	// means a reordered or unrelated switch fails loudly instead of moving
	// the arm somewhere unintended.
	capturePoseGoToLabel            = "go to"
	drawLineToleranceMM             = 0.5
	drawOrientationToleranceDegrees = 1.0
	// progressStepPercent throttles progress logging by fraction of the
	// drawing done, so the line count does not scale with stroke count.
	progressStepPercent = 25
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
	if cfg.CapturePose != "" && cfg.HomePose != nil {
		return nil, nil, errors.New("capture_pose and home_pose are both set; capture_pose supersedes home_pose, so set only one")
	}
	for i, pair := range cfg.AllowedCollisions {
		if pair.Frame1 == "" || pair.Frame2 == "" {
			return nil, nil, fmt.Errorf("allowed_collisions[%d] needs both frame1 and frame2", i)
		}
	}
	if cfg.Photo != "" && cfg.StrokeGenerator == "" {
		return nil, nil, errors.New("photo is set but stroke_generator is not; capture_and_draw needs both")
	}
	deps := []string{cfg.Arm}
	if cfg.StrokeGenerator != "" {
		deps = append(deps, cfg.StrokeGenerator)
	}
	if cfg.Photo != "" {
		deps = append(deps, cfg.Photo)
	}
	if cfg.PreviewCamera != "" {
		deps = append(deps, cfg.PreviewCamera)
	}
	if cfg.CapturePose != "" {
		deps = append(deps, cfg.CapturePose)
	}
	return deps, nil, nil
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
	strokeGenerator    resource.Resource
	photo              camera.Camera
	previewCamera      camera.Camera
	capturePose        toggleswitch.Switch
	collisionSpecs     []motionplan.CollisionSpecification

	mu       sync.Mutex
	cancelFn context.CancelFunc
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
		homeOrientation, homeErr := cfg.HomePose.Orientation.ParseConfig()
		if homeErr != nil {
			return nil, fmt.Errorf("drawer: parse home_pose.orientation: %w", homeErr)
		}
		homePose = spatialmath.NewPose(cfg.HomePose.Translation, homeOrientation)
	}
	var gen resource.Resource
	if cfg.StrokeGenerator != "" {
		gen, err = generic.FromProvider(deps, cfg.StrokeGenerator)
		if err != nil {
			return nil, fmt.Errorf("drawer: get stroke_generator dep %q: %w", cfg.StrokeGenerator, err)
		}
	}
	var photo, previewCamera camera.Camera
	if cfg.Photo != "" {
		photo, err = camera.FromProvider(deps, cfg.Photo)
		if err != nil {
			return nil, fmt.Errorf("drawer: get photo dep %q: %w", cfg.Photo, err)
		}
	}
	if cfg.PreviewCamera != "" {
		previewCamera, err = camera.FromProvider(deps, cfg.PreviewCamera)
		if err != nil {
			return nil, fmt.Errorf("drawer: get preview_camera dep %q: %w", cfg.PreviewCamera, err)
		}
	}
	var capturePose toggleswitch.Switch
	if cfg.CapturePose != "" {
		capturePose, err = toggleswitch.FromProvider(deps, cfg.CapturePose)
		if err != nil {
			return nil, fmt.Errorf("drawer: get capture_pose dep %q: %w", cfg.CapturePose, err)
		}
	}
	return &drawer{
		name:               conf.ResourceName(),
		logger:             logger,
		cfg:                cfg,
		arm:                a,
		fsService:          fsService,
		paperTopLeftCorner: spatialmath.NewPose(cfg.PaperTopLeftCorner.Translation, orientation),
		homePose:           homePose,
		strokeGenerator:    gen,
		photo:              photo,
		previewCamera:      previewCamera,
		capturePose:        capturePose,
		collisionSpecs:     collisionSpecs(cfg.AllowedCollisions),
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
	case "draw_image":
		return d.drawImage(ctx, cmd["draw_image"])
	case "capture_and_draw":
		return d.captureAndDraw(ctx, cmd["capture_and_draw"])
	case "cancel":
		return d.cancel(ctx)
	case "go_home":
		return d.goHome(ctx)
	default:
		return nil, fmt.Errorf(
			"drawer: unknown verb %q; expected \"draw\", \"draw_image\", \"capture_and_draw\", \"cancel\", or \"go_home\"", v)
	}
}

// hasRestPose reports whether the arm has somewhere to sit between drawings.
func (d *drawer) hasRestPose() bool {
	return d.capturePose != nil || d.homePose != nil
}

// goToRestPose moves the arm to the pose it rests at between drawings — which
// is also the pose capture_and_draw shoots from. Pass fs to reuse an already
// built frame system; nil builds one.
func (d *drawer) goToRestPose(ctx context.Context, fs *referenceframe.FrameSystem) error {
	if d.capturePose != nil {
		position, err := d.capturePoseGoToPosition(ctx)
		if err != nil {
			return err
		}
		if err := d.capturePose.SetPosition(ctx, position, nil); err != nil {
			return fmt.Errorf("move capture_pose %q to position %d: %w", d.cfg.CapturePose, position, err)
		}
		return nil
	}
	if d.homePose == nil {
		return errors.New("neither capture_pose nor home_pose is configured")
	}
	if fs == nil {
		built, err := d.buildFrameSystem(ctx)
		if err != nil {
			return err
		}
		fs = built
	}
	return d.planAndExecute(ctx, fs, d.homePose, d.constraints(false))
}

func (d *drawer) capturePoseGoToPosition(ctx context.Context) (uint32, error) {
	count, labels, err := d.capturePose.GetNumberOfPositions(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("read positions of capture_pose %q: %w", d.cfg.CapturePose, err)
	}
	for i, label := range labels {
		if strings.EqualFold(strings.TrimSpace(label), capturePoseGoToLabel) {
			return uint32(i), nil
		}
	}
	return 0, fmt.Errorf(
		"capture_pose %q has no %q position (%d positions, labels %v); expected an arm-position-saver switch",
		d.cfg.CapturePose, capturePoseGoToLabel, count, labels)
}

func (d *drawer) goHome(parent context.Context) (map[string]interface{}, error) {
	if !d.hasRestPose() {
		return nil, errors.New("drawer: neither capture_pose nor home_pose is configured")
	}
	ctx, release, err := d.acquireDrawSlot(parent)
	if err != nil {
		return nil, err
	}
	defer release()

	if err := d.goToRestPose(ctx, nil); err != nil {
		return nil, fmt.Errorf("drawer: go_home: %w", err)
	}
	return map[string]interface{}{"status": "ok"}, nil
}

// Only one draw/draw_image in flight at a time. Cancel aborts it and Stops the arm.
func (d *drawer) acquireDrawSlot(parent context.Context) (context.Context, func(), error) {
	d.mu.Lock()
	if d.cancelFn != nil {
		d.mu.Unlock()
		return nil, nil, errors.New("drawer: another draw is already running; call cancel first")
	}
	ctx, cancel := context.WithCancel(parent)
	d.cancelFn = cancel
	d.mu.Unlock()
	return ctx, func() {
		d.mu.Lock()
		d.cancelFn = nil
		d.mu.Unlock()
		cancel()
	}, nil
}

func (d *drawer) cancel(ctx context.Context) (map[string]interface{}, error) {
	d.mu.Lock()
	cancelFn := d.cancelFn
	d.mu.Unlock()
	if cancelFn == nil {
		return map[string]interface{}{"canceled": false, "reason": "nothing running"}, nil
	}
	cancelFn()
	if err := d.arm.Stop(ctx, nil); err != nil {
		d.logger.Warnf("drawer: arm.Stop during cancel: %v", err)
	}
	return map[string]interface{}{"canceled": true}, nil
}

// strokeArgs are the stroke-generation options shared by the verbs that turn a
// photo into polylines. Paper geometry is not here — the drawer already knows
// paper_width_mm and paper_height_mm from its own config and forwards them to
// the stroke_generator.
type strokeArgs struct {
	MarginMM float64 `json:"margin_mm"`
	Rotate   int     `json:"rotate"`
	Mirror   bool    `json:"mirror"`
	// AutoRotate, when nil, defaults to true — the generator picks between
	// 0° and 90° to maximize paper coverage. Set to false to use Rotate.
	AutoRotate *bool `json:"auto_rotate,omitempty"`
	// Preview renders the strokes instead of drawing them, so a bad
	// extraction can be spotted before the arm commits to it.
	Preview        bool    `json:"preview,omitempty"`
	PreviewPxPerMM float64 `json:"preview_px_per_mm,omitempty"`
}

func (a *strokeArgs) validate() error {
	if a.MarginMM < 0 {
		return fmt.Errorf("margin_mm must be >= 0, got %g", a.MarginMM)
	}
	switch a.Rotate {
	case 0, 90, 180, 270:
	default:
		return fmt.Errorf("rotate must be 0, 90, 180, or 270, got %d", a.Rotate)
	}
	if a.PreviewPxPerMM < 0 {
		return fmt.Errorf("preview_px_per_mm must be >= 0 (0 uses the default), got %g", a.PreviewPxPerMM)
	}
	if a.PreviewPxPerMM == 0 {
		a.PreviewPxPerMM = defaultPreviewPxPerMM
	}
	return nil
}

// drawImageArgs is the DoCommand payload for the "draw_image" verb.
type drawImageArgs struct {
	ImageB64 string `json:"image_b64"`
	strokeArgs
}

func parseDrawImageArgs(payload interface{}) (*drawImageArgs, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}
	var a drawImageArgs
	if unmarshalErr := json.Unmarshal(raw, &a); unmarshalErr != nil {
		return nil, fmt.Errorf("parse payload: %w", unmarshalErr)
	}
	if a.ImageB64 == "" {
		return nil, errors.New("image_b64 is required")
	}
	if err := a.validate(); err != nil {
		return nil, err
	}
	return &a, nil
}

// captureAndDrawArgs is the DoCommand payload for the "capture_and_draw" verb.
type captureAndDrawArgs struct {
	// Recapture, when nil, defaults to true. Set it false to draw the frame
	// the photo camera already holds instead of taking a new one — which is
	// what makes tuning possible, since successive previews then compare
	// stroke settings against one fixed image rather than against a fresh
	// photo each time.
	Recapture *bool `json:"recapture,omitempty"`
	strokeArgs
}

func (a *captureAndDrawArgs) recapture() bool {
	return a.Recapture == nil || *a.Recapture
}

func parseCaptureAndDrawArgs(payload interface{}) (*captureAndDrawArgs, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}
	var a captureAndDrawArgs
	if unmarshalErr := json.Unmarshal(raw, &a); unmarshalErr != nil {
		return nil, fmt.Errorf("parse payload: %w", unmarshalErr)
	}
	if err := a.validate(); err != nil {
		return nil, err
	}
	return &a, nil
}

func (d *drawer) drawImage(parent context.Context, payload interface{}) (map[string]interface{}, error) {
	if d.strokeGenerator == nil {
		return nil, errors.New("drawer: draw_image requires stroke_generator to be configured")
	}
	a, err := parseDrawImageArgs(payload)
	if err != nil {
		return nil, fmt.Errorf("drawer: %w", err)
	}
	ctx, release, err := d.acquireDrawSlot(parent)
	if err != nil {
		return nil, err
	}
	defer release()

	polylines, err := d.generateStrokes(ctx, a.ImageB64, &a.strokeArgs)
	if err != nil {
		return nil, err
	}
	return d.drawOrPreview(ctx, polylines, &a.strokeArgs)
}

// captureAndDraw is the whole in-app loop: pose the arm so the camera frames
// the subject, trigger the shot, and draw what comes back.
func (d *drawer) captureAndDraw(parent context.Context, payload interface{}) (map[string]interface{}, error) {
	if d.photo == nil {
		return nil, errors.New("drawer: capture_and_draw requires photo to be configured")
	}
	if d.strokeGenerator == nil {
		return nil, errors.New("drawer: capture_and_draw requires stroke_generator to be configured")
	}
	a, err := parseCaptureAndDrawArgs(payload)
	if err != nil {
		return nil, fmt.Errorf("drawer: %w", err)
	}
	ctx, release, err := d.acquireDrawSlot(parent)
	if err != nil {
		return nil, err
	}
	defer release()

	if a.recapture() {
		if d.hasRestPose() {
			if moveErr := d.goToRestPose(ctx, nil); moveErr != nil {
				return nil, fmt.Errorf("drawer: move to capture pose: %w", moveErr)
			}
		} else {
			d.logger.Warn("drawer: capture_and_draw with no capture_pose or home_pose configured; capturing from the arm's current pose")
		}
		if photoErr := d.triggerPhoto(ctx); photoErr != nil {
			return nil, photoErr
		}
	} else {
		d.logger.Info("drawer: recapture is false; drawing the frame the photo camera already holds")
	}

	imageB64, err := d.readPhoto(ctx)
	if err != nil {
		return nil, err
	}
	polylines, err := d.generateStrokes(ctx, imageB64, &a.strokeArgs)
	if err != nil {
		return nil, err
	}
	return d.drawOrPreview(ctx, polylines, &a.strokeArgs)
}

// triggerPhoto asks the photo camera for a new frame. The countdown and image
// encoding are its business, not the drawer's.
func (d *drawer) triggerPhoto(ctx context.Context) error {
	if _, err := d.photo.DoCommand(ctx, map[string]interface{}{"capture": map[string]interface{}{}}); err != nil {
		return fmt.Errorf("drawer: trigger photo %q: %w", d.cfg.Photo, err)
	}
	return nil
}

// readPhoto returns whatever frame the photo camera currently holds.
func (d *drawer) readPhoto(ctx context.Context) (string, error) {
	images, _, err := d.photo.Images(ctx, nil, nil)
	if err != nil {
		return "", fmt.Errorf("drawer: read photo %q: %w", d.cfg.Photo, err)
	}
	if len(images) == 0 {
		return "", fmt.Errorf("drawer: photo %q returned no images", d.cfg.Photo)
	}
	raw, err := images[0].Bytes(ctx)
	if err != nil {
		return "", fmt.Errorf("drawer: read photo bytes: %w", err)
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

func (d *drawer) generateStrokes(ctx context.Context, imageB64 string, a *strokeArgs) ([]Polyline, error) {
	inner := map[string]interface{}{
		"image_b64":       imageB64,
		"paper_width_mm":  d.cfg.PaperWidthMM,
		"paper_height_mm": d.cfg.PaperHeightMM,
		"margin_mm":       a.MarginMM,
		"rotate":          a.Rotate,
		"mirror":          a.Mirror,
	}
	if a.AutoRotate != nil {
		inner["auto_rotate"] = *a.AutoRotate
	}
	resp, err := d.strokeGenerator.DoCommand(ctx, map[string]interface{}{"generate": inner})
	if err != nil {
		return nil, fmt.Errorf("drawer: stroke_generator call: %w", err)
	}
	polylines, err := parseDrawPayload(resp)
	if err != nil {
		return nil, fmt.Errorf("drawer: %w", err)
	}
	return polylines, nil
}

func (d *drawer) drawOrPreview(ctx context.Context, polylines []Polyline, a *strokeArgs) (map[string]interface{}, error) {
	if !a.Preview {
		return d.executeDraw(ctx, polylines)
	}
	rendered, err := renderPreviewPNG(polylines, d.cfg.PaperWidthMM, d.cfg.PaperHeightMM, a.PreviewPxPerMM)
	if err != nil {
		return nil, fmt.Errorf("drawer: %w", err)
	}
	total := 0
	for _, poly := range polylines {
		total += len(poly)
	}
	resp := map[string]interface{}{
		"preview":        true,
		"polyline_count": len(polylines),
		"total_points":   total,
	}
	encoded := base64.StdEncoding.EncodeToString(rendered)
	if d.previewCamera == nil {
		resp["preview_png_b64"] = encoded
		return resp, nil
	}
	if _, err := d.previewCamera.DoCommand(ctx, map[string]interface{}{
		"set_image": map[string]interface{}{"image_b64": encoded, "source_name": "line-preview"},
	}); err != nil {
		return nil, fmt.Errorf("drawer: push preview to %q: %w", d.cfg.PreviewCamera, err)
	}
	resp["preview_camera"] = d.cfg.PreviewCamera
	return resp, nil
}

func (d *drawer) draw(parent context.Context, payload interface{}) (map[string]interface{}, error) {
	polylines, err := parseDrawPayload(payload)
	if err != nil {
		return nil, fmt.Errorf("drawer: %w", err)
	}
	ctx, release, err := d.acquireDrawSlot(parent)
	if err != nil {
		return nil, err
	}
	defer release()
	return d.executeDraw(ctx, polylines)
}

// constraints builds the planner constraints for one move: the configured
// collision allowances always, and a straight-line requirement only when the pen
// is on the paper.
func (d *drawer) constraints(linear bool) *motionplan.Constraints {
	var linConstraints []motionplan.LinearConstraint
	if linear {
		linConstraints = []motionplan.LinearConstraint{{
			LineToleranceMm:          drawLineToleranceMM,
			OrientationToleranceDegs: drawOrientationToleranceDegrees,
		}}
	}
	if linConstraints == nil && d.collisionSpecs == nil {
		return nil
	}
	return motionplan.NewConstraints(linConstraints, nil, nil, d.collisionSpecs)
}

func collisionSpecs(pairs []AllowedCollision) []motionplan.CollisionSpecification {
	if len(pairs) == 0 {
		return nil
	}
	allows := make([]motionplan.CollisionSpecificationAllowedFrameCollisions, 0, len(pairs))
	for _, pair := range pairs {
		allows = append(allows, motionplan.CollisionSpecificationAllowedFrameCollisions{
			Frame1: pair.Frame1,
			Frame2: pair.Frame2,
		})
	}
	return []motionplan.CollisionSpecification{{Allows: allows}}
}

// waypoint is one planned move in a drawing. linear marks the moves where the
// pen is on the paper (or dropping onto it), which must follow a straight line;
// the rest are travel and are left to the planner.
type waypoint struct {
	x, y, z      float64
	linear       bool
	endsPolyline bool
	label        string
}

func drawWaypoints(polylines []Polyline, corner r3.Vector, zUp, zDown float64) []waypoint {
	var out []waypoint
	for i, poly := range polylines {
		sx, sy := corner.X+poly[0][0], corner.Y+poly[0][1]
		out = append(out,
			waypoint{x: sx, y: sy, z: zUp, label: fmt.Sprintf("polyline %d approach", i)},
			waypoint{x: sx, y: sy, z: zDown, linear: true, label: fmt.Sprintf("polyline %d pen-down", i)},
		)
		for j := 1; j < len(poly); j++ {
			px, py := corner.X+poly[j][0], corner.Y+poly[j][1]
			out = append(out, waypoint{x: px, y: py, z: zDown, linear: true, label: fmt.Sprintf("polyline %d point %d", i, j)})
		}
		lx, ly := corner.X+poly[len(poly)-1][0], corner.Y+poly[len(poly)-1][1]
		out = append(out, waypoint{
			x: lx, y: ly, z: zUp, linear: true, endsPolyline: true,
			label: fmt.Sprintf("polyline %d pen-up", i),
		})
	}
	return out
}

func (d *drawer) executeDraw(ctx context.Context, polylines []Polyline) (map[string]interface{}, error) {
	fs, err := d.buildFrameSystem(ctx)
	if err != nil {
		return nil, err
	}

	corner := d.paperTopLeftCorner.Point()
	orientation := d.paperTopLeftCorner.Orientation()
	zDown := corner.Z
	zUp := corner.Z + d.cfg.LiftOffZMM

	total := 0
	for _, poly := range polylines {
		total += len(poly)
	}
	d.logger.Infof("drawer: drawing %d polylines, %d points", len(polylines), total)
	startedAt := time.Now()
	drawn, nextProgress := 0, progressStepPercent

	for _, wp := range drawWaypoints(polylines, corner, zUp, zDown) {
		// Only pen-contact moves are held to a straight line. Travel moves span
		// the whole workspace, and a linear plan that far has no direct solution
		// — cbirrt, which would find one, is not allowed under a linear constraint.
		target := spatialmath.NewPose(r3.Vector{X: wp.x, Y: wp.y, Z: wp.z}, orientation)
		if err := d.planAndExecute(ctx, fs, target, d.constraints(wp.linear)); err != nil {
			return nil, fmt.Errorf("drawer: %s: %w", wp.label, err)
		}
		if !wp.endsPolyline {
			continue
		}
		drawn++
		// 100% is left to the "finished" line below.
		if percent := drawn * 100 / len(polylines); percent >= nextProgress && percent < 100 {
			d.logger.Infof("drawer: %d%% — %d/%d polylines, %s elapsed",
				percent, drawn, len(polylines), time.Since(startedAt).Round(time.Second))
			for nextProgress <= percent {
				nextProgress += progressStepPercent
			}
		}
	}
	d.logger.Infof("drawer: finished %d polylines in %s", len(polylines), time.Since(startedAt).Round(time.Second))

	if d.hasRestPose() {
		if err := d.goToRestPose(ctx, fs); err != nil {
			return nil, fmt.Errorf("drawer: return to rest pose: %w", err)
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
