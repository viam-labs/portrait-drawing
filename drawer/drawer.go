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
	"go.viam.com/rdk/components/sensor"
	toggleswitch "go.viam.com/rdk/components/switch"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/motionplan"
	"go.viam.com/rdk/motionplan/armplanning"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/robot/framesystem"
	"go.viam.com/rdk/services/generic"
	"go.viam.com/rdk/spatialmath"
	rutils "go.viam.com/rdk/utils"

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
	// touches the paper's top-left corner: smallest X, largest Y, since the
	// page runs +X across and -Y down. Orientation is reused for every
	// waypoint so the pen keeps the same attitude across the whole drawing.
	PaperTopLeftCorner *PoseConfig `json:"paper_top_left_corner"`
	// PaperTopRightCorner, when set, is where the pen touches the top-right
	// corner. The page then runs from top-left toward it, so the sheet can face
	// whichever way its reader stands. Unset keeps the page along +X.
	PaperTopRightCorner *r3.Vector `json:"paper_top_right_corner,omitempty"`
	PaperWidthMM        float64    `json:"paper_width_mm"`
	PaperHeightMM       float64    `json:"paper_height_mm"`
	LiftOffZMM          float64    `json:"lift_off_z_mm,omitempty"`
	// HomePose, if set, is the tool pose the arm rests at between drawings.
	HomePose *PoseConfig `json:"home_pose,omitempty"`
	// RemovedCapturePose is read only so Validate can reject it: an old config
	// that still sets capture_pose would otherwise lose its rest pose silently.
	RemovedCapturePose string `json:"capture_pose,omitempty"`
	// AllowedCollisions lists frame pairs the planner should not treat as a
	// collision. Anything bolted to the arm — a wrist camera, a pen holder —
	// overlaps the link it is mounted on in every configuration, which the
	// planner cannot distinguish from a real collision.
	AllowedCollisions []AllowedCollision `json:"allowed_collisions,omitempty"`
	// StrokeGenerator, if set, is the name of a stroke-generator service
	// the drawer calls from the draw_image verb to turn a photo into
	// polylines in one DoCommand.
	StrokeGenerator string `json:"stroke_generator,omitempty"`
	// Photo, if set, is the name of a frame-buffer camera the drawer
	// triggers and then reads from in the capture_and_draw verb. The arm
	// moves to HomePose first, out of the camera's view.
	Photo string `json:"photo,omitempty"`
	// PreviewCamera, if set, is the name of a frame-buffer camera the drawer
	// pushes rendered previews into, so they are viewable in the Viam app
	// instead of coming back as base64.
	PreviewCamera      string                                     `json:"preview_camera,omitempty"`
	InputRangeOverride map[string]map[string]referenceframe.Limit `json:"input_range_override,omitempty"`
	ForceGuard         *ForceGuardConfig                          `json:"force_guard,omitempty"`
	TouchOff           *TouchOffConfig                            `json:"touch_off,omitempty"`
	PenForce           *PenForceConfig                            `json:"pen_force,omitempty"`
	// StrokeSpeedDegsPerSec and StrokeAccelerationDegsPerSecPerSec cap the arm
	// while the pen is on the paper or going onto it; travel between strokes
	// runs at the arm's own. A draw may carry its own. 0 leaves the arm's.
	StrokeSpeedDegsPerSec              float64 `json:"stroke_speed_degs_per_sec,omitempty"`
	StrokeAccelerationDegsPerSecPerSec float64 `json:"stroke_acceleration_degs_per_sec_per_sec,omitempty"`
	// RemovedForceControl is read only so Validate can reject it: drawing at a
	// force by correcting between points could not see past the arm's own jolts.
	RemovedForceControl json.RawMessage `json:"force_control,omitempty"`
	// LineToleranceMM is how far a pen-on-paper move may stray from its straight
	// line, up and down included. A pen with no give needs it well under the
	// depth its force builds over, a few tenths of a millimetre.
	LineToleranceMM float64 `json:"line_tolerance_mm,omitempty"`
}

// AllowedCollision is a pair of frame names permitted to overlap. Use the names
// exactly as the planner prints them, e.g. "arm-1:wrist_link" and "camera-1_origin".
type AllowedCollision struct {
	Frame1 string `json:"frame1"`
	Frame2 string `json:"frame2"`
}

// PoseConfig is a tool pose as written in config.
type PoseConfig struct {
	Translation r3.Vector                      `json:"translation"`
	Orientation *spatialmath.OrientationConfig `json:"orientation"`
}

// Paper is a drawing surface: the pose at its top-left corner, optionally the
// position of its top-right corner, and its size. The drawer's config holds one;
// a draw call may carry another, so a service with its own surface — a card
// feeder beside the portrait paper — can draw with this drawer's arm.
type Paper struct {
	TopLeftCorner  *PoseConfig `json:"top_left_corner"`
	TopRightCorner *r3.Vector  `json:"top_right_corner,omitempty"`
	WidthMM        float64     `json:"width_mm"`
	HeightMM       float64     `json:"height_mm"`
}

// Validate checks the surface. Field names in errors carry `prefix`, so a
// config field reads as paper_top_left_corner and a payload one as paper.top_left_corner.
func (p *Paper) Validate(path, prefix string) error {
	if p.TopLeftCorner == nil {
		return resource.NewConfigValidationFieldRequiredError(path, prefix+"top_left_corner")
	}
	if p.TopLeftCorner.Orientation == nil {
		return resource.NewConfigValidationFieldRequiredError(path, prefix+"top_left_corner.orientation")
	}
	if p.WidthMM <= 0 {
		return fmt.Errorf("%swidth_mm must be > 0, got %g", prefix, p.WidthMM)
	}
	if p.TopRightCorner != nil {
		if d := topEdgeLength(p.TopLeftCorner.Translation, *p.TopRightCorner); d < minTopEdgeMM {
			return fmt.Errorf(
				"%stop_right_corner is %.1f mm from %stop_left_corner in X/Y; teach two distinct corners (at least %g mm apart)",
				prefix, d, prefix, float64(minTopEdgeMM))
		}
	}
	if p.HeightMM <= 0 {
		return fmt.Errorf("%sheight_mm must be > 0, got %g", prefix, p.HeightMM)
	}
	return nil
}

// surface is a Paper resolved for drawing: the corner as a pose and the unit
// world direction of the page's +x (left to right).
type surface struct {
	corner spatialmath.Pose
	across r3.Vector
	// stroke caps the arm on the paper, in degrees; held with the surface so a
	// drawing resumes at the speed it started at.
	stroke strokeLimits
}

type strokeLimits struct{ speed, acceleration float64 }

func (l strokeLimits) or(fallback strokeLimits) strokeLimits {
	if l.speed == 0 {
		l.speed = fallback.speed
	}
	if l.acceleration == 0 {
		l.acceleration = fallback.acceleration
	}
	return l
}

func (l strokeLimits) moveOptions() *arm.MoveOptions {
	if l.speed == 0 && l.acceleration == 0 {
		return nil
	}
	return &arm.MoveOptions{MaxVelRads: l.speed * math.Pi / 180, MaxAccRads: l.acceleration * math.Pi / 180}
}

// CheckTopEdge warns when the taught top edge disagrees with width_mm. Services
// call it once at startup; resolve runs on every draw and stays quiet.
func (p *Paper) CheckTopEdge(logger logging.Logger, prefix string) {
	if p.TopRightCorner == nil {
		return
	}
	if l := topEdgeLength(p.TopLeftCorner.Translation, *p.TopRightCorner); math.Abs(l-p.WidthMM) > topEdgeWarnMM {
		logger.Warnf("taught top edge is %.1f mm but %swidth_mm is %g; check both corners", l, prefix, p.WidthMM)
	}
}

func (p *Paper) resolve(prefix string) (surface, error) {
	orientation, err := p.TopLeftCorner.Orientation.ParseConfig()
	if err != nil {
		return surface{}, fmt.Errorf("parse %stop_left_corner.orientation: %w", prefix, err)
	}
	across := r3.Vector{X: 1}
	if p.TopRightCorner != nil {
		corner := p.TopLeftCorner.Translation
		across = r3.Vector{X: p.TopRightCorner.X - corner.X, Y: p.TopRightCorner.Y - corner.Y}.Normalize()
	}
	return surface{corner: spatialmath.NewPose(p.TopLeftCorner.Translation, orientation), across: across}, nil
}

func (cfg *Config) paper() *Paper {
	return &Paper{
		TopLeftCorner:  cfg.PaperTopLeftCorner,
		TopRightCorner: cfg.PaperTopRightCorner,
		WidthMM:        cfg.PaperWidthMM,
		HeightMM:       cfg.PaperHeightMM,
	}
}

const (
	defaultLiftOffZMM               = 5.0
	defaultPreviewPxPerMM           = 2.0
	drawLineToleranceMM             = 0.5
	drawOrientationToleranceDegrees = 1.0
	// progressStepPercent throttles progress logging by fraction of the
	// drawing done, so the line count does not scale with stroke count.
	progressStepPercent = 25
	// minTopEdgeMM rejects two taught corners so close that the page direction
	// between them is mostly measurement noise.
	minTopEdgeMM = 10
	// topEdgeWarnMM is how far the taught top edge may differ from
	// paper_width_mm before the drawer warns that a corner may be wrong.
	topEdgeWarnMM = 5
)

// Validate returns implicit dependencies and any config errors.
func (cfg *Config) Validate(path string) ([]string, []string, error) {
	if cfg.Arm == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "arm")
	}
	if err := cfg.paper().Validate(path, "paper_"); err != nil {
		return nil, nil, err
	}
	if cfg.LiftOffZMM < 0 {
		return nil, nil, fmt.Errorf("lift_off_z_mm must be >= 0 (0 uses the default), got %g", cfg.LiftOffZMM)
	}
	if cfg.RemovedCapturePose != "" {
		return nil, nil, errors.New("capture_pose was removed; set home_pose to the tool pose the arm rests at instead")
	}
	if cfg.HomePose != nil && cfg.HomePose.Orientation == nil {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "home_pose.orientation")
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
	if cfg.LineToleranceMM < 0 {
		return nil, nil, fmt.Errorf("line_tolerance_mm must be >= 0 (0 uses the default), got %g", cfg.LineToleranceMM)
	}
	if len(cfg.RemovedForceControl) > 0 {
		return nil, nil, errors.New("force_control was removed; remove it from the drawer's config, and see touch_off")
	}
	if cfg.StrokeSpeedDegsPerSec < 0 || cfg.StrokeAccelerationDegsPerSecPerSec < 0 {
		return nil, nil, errors.New("stroke_speed_degs_per_sec and stroke_acceleration_degs_per_sec_per_sec must be >= 0")
	}
	if cfg.TouchOff != nil {
		if err := cfg.TouchOff.validate(cfg.ForceGuard); err != nil {
			return nil, nil, err
		}
	}
	if cfg.PenForce != nil {
		if err := cfg.PenForce.validate(cfg.ForceGuard); err != nil {
			return nil, nil, err
		}
	}
	if cfg.ForceGuard != nil {
		liftOff := cfg.LiftOffZMM
		if liftOff == 0 {
			liftOff = defaultLiftOffZMM
		}
		if err := cfg.ForceGuard.validate(path, liftOff); err != nil {
			return nil, nil, err
		}
		deps = append(deps, cfg.ForceGuard.Sensor)
		if cfg.ForceGuard.RetreatPose != "" {
			deps = append(deps, cfg.ForceGuard.RetreatPose)
		}
	}
	if cfg.StrokeGenerator != "" {
		deps = append(deps, cfg.StrokeGenerator)
	}
	if cfg.Photo != "" {
		deps = append(deps, cfg.Photo)
	}
	if cfg.PreviewCamera != "" {
		deps = append(deps, cfg.PreviewCamera)
	}
	return deps, nil, nil
}

type drawer struct {
	resource.AlwaysRebuild
	resource.TriviallyCloseable

	name resource.Name
	// paper is the configured surface; a draw call may bring its own.
	paper           surface
	logger          logging.Logger
	cfg             *Config
	arm             arm.Arm
	fsService       framesystem.Service
	homePose        spatialmath.Pose
	strokeGenerator resource.Resource
	photo           camera.Camera
	previewCamera   camera.Camera
	collisionSpecs  []motionplan.CollisionSpecification
	forceGuard      *forceGuard

	// lastForce is what the force guard measured over the most recent draw.
	lastForce *forceRun
	// touched is the paper the last touch-off measured, kept so a drawing that
	// resumes on the same sheet does not touch it off again.
	touched *touchedPaper

	mu       sync.Mutex
	cancelFn context.CancelFunc
	progress progress
	// drawDone is closed when the running draw releases its slot, so pause can
	// wait for the arm to be free.
	drawDone chan struct{}
	// pauseRequested stops the running draw at the next polyline boundary.
	pauseRequested bool
	// held is the job a pause, cancel or failure interrupted, kept so resume can
	// carry on from nextPolyline. Cleared when a draw finishes.
	held         []Polyline
	heldPaper    surface
	nextPolyline int
}

// Phases a drawing passes through, reported by the status verb. A drawing runs
// for minutes with the DoCommand that started it still outstanding, so this is
// the only way a caller can tell what is happening — or that anything is.
const (
	phaseIdle       = "idle"
	phaseMoving     = "moving_to_home_pose"
	phaseCapturing  = "capturing"
	phaseGenerating = "generating_strokes"
	phaseDrawing    = "drawing"
	phaseDone       = "done"
	phaseCanceled   = "canceled"
	phaseFailed     = "failed"
	phasePaused     = "paused"
)

type progress struct {
	phase          string
	polylinesTotal int
	polylinesDone  int
	pointsTotal    int
	startedAt      time.Time
	endedAt        time.Time
	lastError      string
	// fault names a failure a person can clear, so a client can say what to do.
	fault string
}

func (d *drawer) setPhase(phase string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if phase == phaseMoving || (phase == phaseGenerating && d.progress.startedAt.IsZero()) {
		d.progress = progress{startedAt: time.Now()}
	}
	d.progress.phase = phase
	if phase == phaseDone || phase == phaseCanceled || phase == phaseFailed || phase == phasePaused {
		d.progress.endedAt = time.Now()
	}
}

func (d *drawer) setFailed(err error) {
	d.mu.Lock()
	d.progress.phase = phaseFailed
	d.progress.lastError = err.Error()
	d.progress.fault = faultOf(err)
	d.progress.endedAt = time.Now()
	d.mu.Unlock()
}

func (d *drawer) startDrawing(polylines, points int) {
	d.mu.Lock()
	d.progress.phase = phaseDrawing
	d.progress.fault = ""
	d.progress.polylinesTotal = polylines
	d.progress.pointsTotal = points
	d.progress.polylinesDone = 0
	if d.progress.startedAt.IsZero() {
		d.progress.startedAt = time.Now()
	}
	d.mu.Unlock()
}

func (d *drawer) advanceDrawing(done int) {
	d.mu.Lock()
	d.progress.polylinesDone = done
	d.mu.Unlock()
}

// status is readable while a drawing is in flight: executeDraw holds the draw
// slot but not the mutex, so this does not wait on it.
func (d *drawer) status() map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	p := d.progress
	phase := p.phase
	if phase == "" {
		phase = phaseIdle
	}
	out := map[string]any{
		"state":   phase,
		"drawing": d.cancelFn != nil,
	}
	if !p.startedAt.IsZero() {
		out["started_at"] = p.startedAt.UTC().Format(time.RFC3339)
		end := p.endedAt
		if end.IsZero() {
			end = time.Now()
		}
		out["elapsed_sec"] = end.Sub(p.startedAt).Round(time.Second).Seconds()
	}
	if p.polylinesTotal > 0 {
		out["polylines_total"] = p.polylinesTotal
		out["polylines_done"] = p.polylinesDone
		out["points_total"] = p.pointsTotal
		out["percent"] = p.polylinesDone * 100 / p.polylinesTotal
	}
	if p.lastError != "" {
		out["last_error"] = p.lastError
	}
	if p.fault != "" {
		out["fault"] = p.fault
	}
	if d.lastForce != nil {
		out["last_force"] = d.lastForce.summary()
	}
	if d.held != nil {
		out["resumable"] = true
		out["next_polyline"] = d.nextPolyline
	}
	return out
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
	cfg.paper().CheckTopEdge(logger, "paper_")
	paper, err := cfg.paper().resolve("paper_")
	if err != nil {
		return nil, fmt.Errorf("drawer: %w", err)
	}
	paper.stroke = strokeLimits{speed: cfg.StrokeSpeedDegsPerSec, acceleration: cfg.StrokeAccelerationDegsPerSecPerSec}
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
	var guard *forceGuard
	if fg := cfg.ForceGuard; fg != nil {
		s, sensorErr := sensor.FromProvider(deps, fg.Sensor)
		if sensorErr != nil {
			return nil, fmt.Errorf("drawer: get force_guard.sensor dep %q: %w", fg.Sensor, sensorErr)
		}
		guard = &forceGuard{sensor: s, reading: fg.Reading, max: fg.MaxForce, contact: fg.ContactForce}
		if fg.RetreatPose != "" {
			if guard.retreat, err = toggleswitch.FromProvider(deps, fg.RetreatPose); err != nil {
				return nil, fmt.Errorf("drawer: get force_guard.retreat_pose dep %q: %w", fg.RetreatPose, err)
			}
		}
	}
	return &drawer{
		forceGuard:      guard,
		paper:           paper,
		name:            conf.ResourceName(),
		logger:          logger,
		cfg:             cfg,
		arm:             a,
		fsService:       fsService,
		homePose:        homePose,
		strokeGenerator: gen,
		photo:           photo,
		previewCamera:   previewCamera,
		collisionSpecs:  collisionSpecs(cfg.AllowedCollisions),
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
	// StartAt skips the polylines before it, to carry on a job whose earlier
	// strokes are already on the paper.
	StartAt int `json:"start_at,omitempty"`
	// Paper, when set, is drawn on instead of the configured one.
	Paper *Paper `json:"paper,omitempty"`
	// StrokeSpeedDegsPerSec and StrokeAccelerationDegsPerSecPerSec override the
	// configured caps for this drawing.
	StrokeSpeedDegsPerSec              float64 `json:"stroke_speed_degs_per_sec,omitempty"`
	StrokeAccelerationDegsPerSecPerSec float64 `json:"stroke_acceleration_degs_per_sec_per_sec,omitempty"`
}

func (d *drawer) configuredStroke() strokeLimits {
	return strokeLimits{speed: d.cfg.StrokeSpeedDegsPerSec, acceleration: d.cfg.StrokeAccelerationDegsPerSecPerSec}
}

func parseStrokeLimits(payload any) (strokeLimits, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return strokeLimits{}, fmt.Errorf("marshal payload: %w", err)
	}
	var args drawArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return strokeLimits{}, fmt.Errorf("parse payload: %w", err)
	}
	if args.StrokeSpeedDegsPerSec < 0 || args.StrokeAccelerationDegsPerSecPerSec < 0 {
		return strokeLimits{}, errors.New("stroke_speed_degs_per_sec and stroke_acceleration_degs_per_sec_per_sec must be >= 0")
	}
	return strokeLimits{speed: args.StrokeSpeedDegsPerSec, acceleration: args.StrokeAccelerationDegsPerSecPerSec}, nil
}

func parseDrawPayload(payload any) ([]Polyline, int, *Paper, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("marshal payload: %w", err)
	}
	var args drawArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, 0, nil, fmt.Errorf("parse payload: %w", err)
	}
	if len(args.Polylines) == 0 {
		return nil, 0, nil, errors.New("polylines is required and must be non-empty")
	}
	if args.StartAt < 0 || args.StartAt >= len(args.Polylines) {
		return nil, 0, nil, fmt.Errorf("start_at must be in [0, %d), got %d", len(args.Polylines), args.StartAt)
	}
	if args.Paper != nil {
		if err := args.Paper.Validate("", "paper."); err != nil {
			return nil, 0, nil, err
		}
	}
	out := make([]Polyline, len(args.Polylines))
	for i, poly := range args.Polylines {
		if len(poly) == 0 {
			return nil, 0, nil, fmt.Errorf("polyline %d is empty", i)
		}
		out[i] = make(Polyline, len(poly))
		for j, pt := range poly {
			if len(pt) != 2 {
				return nil, 0, nil, fmt.Errorf("polyline %d point %d must have exactly 2 elements, got %d", i, j, len(pt))
			}
			out[i][j] = [2]float64{pt[0], pt[1]}
		}
	}
	return out, args.StartAt, args.Paper, nil
}

func (d *drawer) DoCommand(ctx context.Context, cmd map[string]any) (map[string]any, error) {
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
	case "pause":
		return d.pause(ctx)
	case "resume":
		return d.resume(ctx)
	case "status":
		return d.status(), nil
	case "go_home":
		return d.goHome(ctx)
	default:
		return nil, fmt.Errorf(
			"drawer: unknown verb %q; expected \"draw\", \"draw_image\", \"capture_and_draw\", \"status\", \"pause\", \"resume\", \"cancel\", or \"go_home\"", v)
	}
}

var errNoHomePose = errors.New("home_pose is not configured")

// hasRestPose reports whether the arm has somewhere to sit between drawings.
func (d *drawer) hasRestPose() bool {
	return d.homePose != nil
}

// goToRestPose moves the arm to home_pose. Pass fs to reuse an already built
// frame system; nil builds one.
func (d *drawer) goToRestPose(ctx context.Context, fs *referenceframe.FrameSystem) error {
	if d.homePose == nil {
		return errNoHomePose
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

func (d *drawer) goHome(parent context.Context) (map[string]any, error) {
	if !d.hasRestPose() {
		return nil, fmt.Errorf("drawer: %w", errNoHomePose)
	}
	ctx, release, err := d.acquireDrawSlot(parent)
	if err != nil {
		return nil, err
	}
	defer release()

	if err := d.goToRestPose(ctx, nil); err != nil {
		return nil, fmt.Errorf("drawer: go_home: %w", err)
	}
	return map[string]any{"status": "ok"}, nil
}

// Only one draw/draw_image in flight at a time. Cancel aborts it and Stops the arm.
func (d *drawer) acquireDrawSlot(parent context.Context) (context.Context, func(), error) {
	d.mu.Lock()
	if d.cancelFn != nil {
		d.mu.Unlock()
		return nil, nil, errors.New("drawer: another draw is already running; call pause or cancel first")
	}
	ctx, cancel := context.WithCancel(parent)
	d.cancelFn = cancel
	done := make(chan struct{})
	d.drawDone = done
	d.pauseRequested = false
	d.mu.Unlock()
	return ctx, func() {
		d.mu.Lock()
		d.cancelFn = nil
		d.drawDone = nil
		d.pauseRequested = false
		d.mu.Unlock()
		cancel()
		close(done)
	}, nil
}

// pause asks the running draw to stop after the polyline it is on, with the pen
// up and the arm back at its rest pose, and waits for that so the arm is free
// for another job when it returns.
func (d *drawer) pause(ctx context.Context) (map[string]any, error) {
	d.mu.Lock()
	done := d.drawDone
	if done == nil {
		d.mu.Unlock()
		return map[string]any{"paused": false, "reason": "nothing running"}, nil
	}
	d.pauseRequested = true
	d.mu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
		return nil, fmt.Errorf("drawer: waiting for the draw to pause: %w", ctx.Err())
	}
	out := d.status()
	out["paused"] = d.statusPhase() == phasePaused
	return out, nil
}

func (d *drawer) statusPhase() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.progress.phase
}

// resume carries on the job a pause, cancel or failure interrupted.
func (d *drawer) resume(parent context.Context) (map[string]any, error) {
	d.mu.Lock()
	held, next, paper := d.held, d.nextPolyline, d.heldPaper
	d.mu.Unlock()
	if held == nil {
		return nil, errors.New("drawer: nothing to resume")
	}
	ctx, release, err := d.acquireDrawSlot(parent)
	if err != nil {
		return nil, err
	}
	defer release()
	return d.executeDraw(ctx, held, next, paper)
}

func (d *drawer) pausePending() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pauseRequested
}

func (d *drawer) hold(polylines []Polyline, next int, paper surface) {
	d.mu.Lock()
	d.held, d.nextPolyline, d.heldPaper = polylines, next, paper
	d.mu.Unlock()
}

func (d *drawer) cancel(ctx context.Context) (map[string]any, error) {
	d.mu.Lock()
	cancelFn := d.cancelFn
	d.mu.Unlock()
	if cancelFn == nil {
		return map[string]any{"canceled": false, "reason": "nothing running"}, nil
	}
	cancelFn()
	d.setPhase(phaseCanceled)
	if err := d.arm.Stop(ctx, nil); err != nil {
		d.logger.Warnf("drawer: arm.Stop during cancel: %v", err)
	}
	return map[string]any{"canceled": true}, nil
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

func parseDrawImageArgs(payload any) (*drawImageArgs, error) {
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

func parseCaptureAndDrawArgs(payload any) (*captureAndDrawArgs, error) {
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

func (d *drawer) drawImage(parent context.Context, payload any) (map[string]any, error) {
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

	d.setPhase(phaseGenerating)
	polylines, err := d.generateStrokes(ctx, a.ImageB64, "", &a.strokeArgs)
	if err != nil {
		d.setFailed(err)
		return nil, err
	}
	return d.drawOrPreview(ctx, polylines, &a.strokeArgs)
}

// captureAndDraw is the whole in-app loop: pose the arm so the camera frames
// the subject, trigger the shot, and draw what comes back.
func (d *drawer) captureAndDraw(parent context.Context, payload any) (map[string]any, error) {
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
		d.setPhase(phaseMoving)
		if d.hasRestPose() {
			if moveErr := d.goToRestPose(ctx, nil); moveErr != nil {
				return nil, fmt.Errorf("drawer: move to home pose: %w", moveErr)
			}
		} else {
			d.logger.Warn("drawer: capture_and_draw with no home_pose configured; capturing with the arm wherever it is")
		}
		d.setPhase(phaseCapturing)
		if photoErr := d.triggerPhoto(ctx); photoErr != nil {
			d.setFailed(photoErr)
			return nil, photoErr
		}
	} else {
		d.logger.Info("drawer: recapture is false; drawing the frame the photo camera already holds")
	}

	imageB64, depthB64, err := d.readPhoto(ctx)
	if err != nil {
		d.setFailed(err)
		return nil, err
	}
	d.setPhase(phaseGenerating)
	polylines, err := d.generateStrokes(ctx, imageB64, depthB64, &a.strokeArgs)
	if err != nil {
		d.setFailed(err)
		return nil, err
	}
	return d.drawOrPreview(ctx, polylines, &a.strokeArgs)
}

// triggerPhoto asks the photo camera for a new frame. The countdown and image
// encoding are its business, not the drawer's.
func (d *drawer) triggerPhoto(ctx context.Context) error {
	if _, err := d.photo.DoCommand(ctx, map[string]any{"capture": map[string]any{}}); err != nil {
		return fmt.Errorf("drawer: trigger photo %q: %w", d.cfg.Photo, err)
	}
	return nil
}

// readPhoto returns whatever frames the photo camera currently holds: the
// picture, and a depth map beside it when the camera has one. Depth is optional
// — it improves background removal but nothing requires it.
func (d *drawer) readPhoto(ctx context.Context) (imageB64, depthB64 string, err error) {
	images, _, err := d.photo.Images(ctx, nil, nil)
	if err != nil {
		return "", "", fmt.Errorf("drawer: read photo %q: %w", d.cfg.Photo, err)
	}
	if len(images) == 0 {
		return "", "", fmt.Errorf("drawer: photo %q returned no images", d.cfg.Photo)
	}
	for i := range images {
		raw, bytesErr := images[i].Bytes(ctx)
		if bytesErr != nil {
			return "", "", fmt.Errorf("drawer: read photo bytes: %w", bytesErr)
		}
		encoded := base64.StdEncoding.EncodeToString(raw)
		if images[i].MimeType() == rutils.MimeTypeRawDepth {
			depthB64 = encoded
			continue
		}
		if imageB64 == "" {
			imageB64 = encoded
		}
	}
	if imageB64 == "" {
		return "", "", fmt.Errorf("drawer: photo %q returned only depth, no picture", d.cfg.Photo)
	}
	return imageB64, depthB64, nil
}

func (d *drawer) generateStrokes(
	ctx context.Context, imageB64, depthB64 string, a *strokeArgs,
) ([]Polyline, error) {
	inner := map[string]any{
		"image_b64":       imageB64,
		"paper_width_mm":  d.cfg.PaperWidthMM,
		"paper_height_mm": d.cfg.PaperHeightMM,
		"margin_mm":       a.MarginMM,
		"rotate":          a.Rotate,
		"mirror":          a.Mirror,
	}
	if depthB64 != "" {
		inner["depth_b64"] = depthB64
	}
	if a.AutoRotate != nil {
		inner["auto_rotate"] = *a.AutoRotate
	}
	resp, err := d.strokeGenerator.DoCommand(ctx, map[string]any{"generate": inner})
	if err != nil {
		return nil, fmt.Errorf("drawer: stroke_generator call: %w", err)
	}
	polylines, _, _, err := parseDrawPayload(resp)
	if err != nil {
		return nil, fmt.Errorf("drawer: %w", err)
	}
	return polylines, nil
}

func (d *drawer) drawOrPreview(ctx context.Context, polylines []Polyline, a *strokeArgs) (map[string]any, error) {
	total := 0
	for _, poly := range polylines {
		total += len(poly)
	}
	rendered, err := renderPreviewPNG(polylines, d.cfg.PaperWidthMM, d.cfg.PaperHeightMM, a.PreviewPxPerMM)
	if err != nil {
		return nil, fmt.Errorf("drawer: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(rendered)

	// Pushed before drawing as well as for a preview: a drawing takes minutes,
	// and during them the preview camera would otherwise still show whatever
	// was last previewed — stale, and indistinguishable from current.
	pushed := false
	if d.previewCamera != nil {
		_, pushErr := d.previewCamera.DoCommand(ctx, map[string]any{
			"set_image": map[string]any{"image_b64": encoded, "source_name": "line-preview"},
		})
		switch {
		case pushErr == nil:
			pushed = true
		case a.Preview:
			return nil, fmt.Errorf("drawer: push preview to %q: %w", d.cfg.PreviewCamera, pushErr)
		default:
			// Mid-draw the picture is a convenience, not the job. Losing it is
			// not a reason to refuse to draw.
			d.logger.Warnf("drawer: could not push preview to %q: %v", d.cfg.PreviewCamera, pushErr)
		}
	}

	if !a.Preview {
		return d.executeDraw(ctx, polylines, 0, d.paper)
	}
	resp := map[string]any{
		"preview":        true,
		"polyline_count": len(polylines),
		"total_points":   total,
	}
	if pushed {
		resp["preview_camera"] = d.cfg.PreviewCamera
	} else {
		resp["preview_png_b64"] = encoded
	}
	return resp, nil
}

func (d *drawer) draw(parent context.Context, payload any) (map[string]any, error) {
	polylines, startAt, override, err := parseDrawPayload(payload)
	if err != nil {
		return nil, fmt.Errorf("drawer: %w", err)
	}
	paper := d.paper
	if override != nil {
		if paper, err = override.resolve("paper."); err != nil {
			return nil, fmt.Errorf("drawer: %w", err)
		}
	}
	limits, err := parseStrokeLimits(payload)
	if err != nil {
		return nil, fmt.Errorf("drawer: %w", err)
	}
	paper.stroke = limits.or(d.configuredStroke())
	ctx, release, err := d.acquireDrawSlot(parent)
	if err != nil {
		return nil, err
	}
	defer release()
	return d.executeDraw(ctx, polylines, startAt, paper)
}

// constraints builds the planner constraints for one move: the configured
// collision allowances always, and a straight-line requirement only when the pen
// is on the paper.
func (d *drawer) constraints(linear bool) *motionplan.Constraints {
	var linConstraints []motionplan.LinearConstraint
	if linear {
		linConstraints = []motionplan.LinearConstraint{{
			LineToleranceMm:          d.lineToleranceMM(),
			OrientationToleranceDegs: drawOrientationToleranceDegrees,
		}}
	}
	if linConstraints == nil && d.collisionSpecs == nil {
		return nil
	}
	return motionplan.NewConstraints(linConstraints, nil, nil, d.collisionSpecs)
}

func (d *drawer) lineToleranceMM() float64 {
	if d.cfg.LineToleranceMM > 0 {
		return d.cfg.LineToleranceMM
	}
	return drawLineToleranceMM
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
	x, y, z float64
	linear  bool
	// clear marks a move that stays above zClear, where a bare tip touches nothing.
	clear bool
	// contact marks a move that ends with the pen on the paper.
	contact      bool
	endsPolyline bool
	label        string
}

// drawMove is one planned, uninterrupted motion through one or more points.
type drawMove struct {
	points []r3.Vector
	linear bool
	clear  bool
	// stroke is pen-down through pen-up, the moves pen_force judges.
	stroke bool
	// contactMM is how far a stroke runs with the pen on the paper.
	contactMM    float64
	endsPolyline bool
	label        string
}

// maxStrokeStepMM spaces the points a stroke is planned through. The arm moves
// between planned points by blending joint angles, which bows off a straight
// line by more the further apart they are, down into the paper included: on
// armfarm07, portrait strokes with points tens of millimetres apart pressed a
// rigid pen at 12 N where 2 mm apart lettering stayed near 7.
const maxStrokeStepMM = 1.0

// strokeMoves runs each stroke, from pen-down through its last point to
// pen-up, as one move: moving point to point, the arm stops and starts at every
// one, which is slow and jolts a wrist force sensor by more than a pen presses.
// Travel and the descent to the paper stay moves of their own, so the guard
// still checks the descent against its lower limit.
func strokeMoves(wps []waypoint) []drawMove {
	var out []drawMove
	for i := 0; i < len(wps); i++ {
		wp := wps[i]
		mv := drawMove{
			points: []r3.Vector{{X: wp.x, Y: wp.y, Z: wp.z}},
			linear: wp.linear, clear: wp.clear, endsPolyline: wp.endsPolyline, label: wp.label,
		}
		if wp.contact {
			mv.stroke = true
			mv.label = strings.TrimSuffix(wp.label, " pen-down") + " stroke"
			for i+1 < len(wps) && (wps[i+1].contact || wps[i+1].endsPolyline) {
				i++
				next := r3.Vector{X: wps[i].x, Y: wps[i].y, Z: wps[i].z}
				if wps[i].contact {
					mv.contactMM += mv.points[len(mv.points)-1].Distance(next)
					mv.points = append(mv.points, along(mv.points[len(mv.points)-1], next, maxStrokeStepMM)...)
				}
				mv.points = append(mv.points, next)
				if wps[i].endsPolyline {
					mv.endsPolyline = true
					break
				}
			}
		}
		out = append(out, mv)
	}
	return out
}

// along is the points strictly between from and to, no more than step apart.
func along(from, to r3.Vector, step float64) []r3.Vector {
	n := int(math.Ceil(from.Distance(to) / step))
	out := make([]r3.Vector, 0, max(n-1, 0))
	for k := 1; k < n; k++ {
		out = append(out, from.Add(to.Sub(from).Mul(float64(k)/float64(n))))
	}
	return out
}

func topEdgeLength(left, right r3.Vector) float64 {
	return math.Hypot(right.X-left.X, right.Y-left.Y)
}

// paperToWorld maps paper-local mm (x right, y down) onto the table: +x along
// across, +y along across turned 90° clockwise seen from above. With Z up that
// keeps the drawing's handedness, so it reads the right way round rather than
// mirrored.
func paperToWorld(corner, across r3.Vector, p [2]float64) (float64, float64) {
	down := r3.Vector{X: across.Y, Y: -across.X}
	return corner.X + p[0]*across.X + p[1]*down.X, corner.Y + p[0]*across.Y + p[1]*down.Y
}

// drawWaypoints splits each descent at zClear when it lies above zDown, so the
// part of it a bare tip makes without touching the paper is a move of its own.
func drawWaypoints(polylines []Polyline, first int, corner, across r3.Vector, zUp, zDown, zClear float64) []waypoint {
	checkContact := zClear > zDown
	var out []waypoint
	for k, poly := range polylines[first:] {
		i := first + k
		sx, sy := paperToWorld(corner, across, poly[0])
		out = append(out, waypoint{x: sx, y: sy, z: zUp, clear: checkContact, label: fmt.Sprintf("polyline %d approach", i)})
		if checkContact {
			out = append(out, waypoint{x: sx, y: sy, z: zClear, linear: true, clear: true, label: fmt.Sprintf("polyline %d descent", i)})
		}
		out = append(out, waypoint{x: sx, y: sy, z: zDown, linear: true, contact: true, label: fmt.Sprintf("polyline %d pen-down", i)})
		for j := 1; j < len(poly); j++ {
			px, py := paperToWorld(corner, across, poly[j])
			out = append(out, waypoint{
				x: px, y: py, z: zDown, linear: true, contact: true,
				label: fmt.Sprintf("polyline %d point %d", i, j),
			})
		}
		lx, ly := paperToWorld(corner, across, poly[len(poly)-1])
		out = append(out, waypoint{
			x: lx, y: ly, z: zUp, linear: true, endsPolyline: true,
			label: fmt.Sprintf("polyline %d pen-up", i),
		})
	}
	return out
}

func (d *drawer) executeDraw(ctx context.Context, polylines []Polyline, startAt int, paper surface) (map[string]any, error) {
	fs, err := d.buildFrameSystem(ctx)
	if err != nil {
		return nil, err
	}

	corner := paper.corner.Point()
	orientation := paper.corner.Orientation()
	zDown := corner.Z
	zUp := corner.Z + d.cfg.LiftOffZMM
	zClear := zDown
	if d.cfg.ForceGuard != nil {
		zClear = zDown + d.cfg.ForceGuard.contactMarginMM()
	}

	total := 0
	for _, poly := range polylines {
		total += len(poly)
	}
	if startAt > 0 {
		d.logger.Infof("drawer: resuming at polyline %d of %d", startAt, len(polylines))
	} else {
		d.logger.Infof("drawer: drawing %d polylines, %d points", len(polylines), total)
	}
	d.startDrawing(len(polylines), total)
	d.advanceDrawing(startAt)
	d.hold(polylines, startAt, paper)
	startedAt := time.Now()
	drawn, nextProgress := startAt, progressStepPercent

	waypoints := drawWaypoints(polylines, startAt, corner, paper.across, zUp, zDown, zClear)
	run := &forceRun{}
	defer d.finishForceRun(run)
	var above spatialmath.Pose
	if len(waypoints) > 0 {
		above = spatialmath.NewPose(r3.Vector{X: waypoints[0].x, Y: waypoints[0].y, Z: zDown + baselineHeightMM}, orientation)
		run.baseline, err = d.forceBaseline(ctx, fs, above)
		if err != nil {
			wrapped := fmt.Errorf("drawer: %w", err)
			if ctx.Err() != nil {
				d.setPhase(phaseCanceled)
			} else {
				d.setFailed(wrapped)
			}
			return nil, wrapped
		}
	}

	if d.cfg.TouchOff != nil && d.forceGuard != nil && len(waypoints) > 0 {
		plane, err := d.paperPlane(ctx, fs, run, polylines, startAt, paper, zDown)
		if err == nil {
			// Back over the first stroke, so the approach to it comes straight down.
			if err = d.guardedMove(ctx, fs, []spatialmath.Pose{above}, run, false, true, nil); err != nil {
				err = fmt.Errorf("return over the first stroke: %w", err)
			}
		}
		if err != nil {
			wrapped := fmt.Errorf("drawer: %w", err)
			if guardStopped(err) && ctx.Err() == nil {
				if retreatErr := d.retreat(ctx, fs, zUp, orientation); retreatErr != nil {
					d.logger.Warnf("drawer: retreat after the force guard stopped a touch-off: %v", retreatErr)
				}
			}
			if ctx.Err() != nil {
				d.setPhase(phaseCanceled)
			} else {
				d.setFailed(wrapped)
			}
			return nil, wrapped
		}
		onPlane(waypoints, plane, zDown, d.cfg.TouchOff.withDefaults().pressMM())
	}

	var pf *penForce
	if d.cfg.PenForce != nil && d.forceGuard != nil {
		pf = &penForce{cfg: d.cfg.PenForce.withDefaults()}
		defer d.finishPenForce(pf)
	}
	moves := strokeMoves(waypoints)
	polyStart, redrawn := -1, false
	for m := 0; m < len(moves); m++ {
		mv := moves[m]
		if (m == 0 || moves[m-1].endsPolyline) && m != polyStart {
			polyStart, redrawn = m, false
		}
		// Only pen-contact moves are held to a straight line. Travel moves span
		// the whole workspace, and a linear plan that far has no direct solution
		// — cbirrt, which would find one, is not allowed under a linear constraint.
		targets := make([]spatialmath.Pose, len(mv.points))
		for i, p := range mv.points {
			if pf != nil && mv.stroke && (!mv.endsPolyline || i != len(mv.points)-1) {
				p.Z += pf.offset
			}
			targets[i] = spatialmath.NewPose(p, orientation)
		}
		run.stroke = forceStats{}
		var options *arm.MoveOptions
		if mv.linear {
			options = paper.stroke.moveOptions()
		}
		if err := d.guardedMove(ctx, fs, targets, run, mv.linear, mv.clear, options); err != nil {
			wrapped := fmt.Errorf("drawer: %s: %w", mv.label, err)
			if ctx.Err() != nil {
				d.setPhase(phaseCanceled)
				return nil, wrapped
			}
			if guardStopped(err) {
				if retreatErr := d.retreat(ctx, fs, zUp, orientation); retreatErr != nil {
					d.logger.Warnf("drawer: retreat after the force guard stopped: %v", retreatErr)
				}
			}
			if ctx.Err() != nil {
				d.setPhase(phaseCanceled)
			} else {
				d.setFailed(wrapped)
			}
			return nil, wrapped
		}
		if pf != nil && mv.stroke && mv.contactMM >= minJudgedStrokeMM {
			before := pf.offset
			verdict := pf.judge(run.stroke)
			if verdict == penLight || verdict == penHeavy {
				d.logger.Infof("drawer: %s mean force %.2f N, outside %g–%g; pen now %+.2f mm",
					mv.label, run.stroke.mean(), pf.cfg.Min, pf.cfg.Max, pf.offset)
			}
			if verdict == penLight && !redrawn && pf.offset != before && run.stroke.samples >= minPenForceSamples {
				redrawn = true
				pf.redrawn++
				m = polyStart - 1
				continue
			}
		}
		if !mv.endsPolyline {
			continue
		}
		drawn++
		d.advanceDrawing(drawn)
		d.hold(polylines, drawn, paper)
		if drawn < len(polylines) && d.pausePending() {
			d.logger.Infof("drawer: paused after polyline %d of %d", drawn, len(polylines))
			if d.hasRestPose() {
				if err := d.goToRestPose(ctx, fs); err != nil {
					d.setFailed(err)
					return nil, fmt.Errorf("drawer: return to rest pose for pause: %w", err)
				}
			}
			d.setPhase(phasePaused)
			return map[string]any{
				"paused":          true,
				"next_polyline":   drawn,
				"polylines_total": len(polylines),
			}, nil
		}
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
	d.hold(nil, 0, surface{})

	// park: a finished sheet is taken away by hand, so the arm waits where a
	// person can reach it.
	if err := d.park(ctx, fs); err != nil {
		d.setFailed(err)
		return nil, fmt.Errorf("drawer: park: %w", err)
	}
	d.setPhase(phaseDone)

	return map[string]any{
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
	return d.planAndExecuteThrough(ctx, fs, []spatialmath.Pose{target}, constraints, nil)
}

// planAndExecuteThrough plans one motion through every target in turn and runs
// it as a single trajectory, so the arm keeps moving from one to the next
// instead of stopping at each.
func (d *drawer) planAndExecuteThrough(
	ctx context.Context, fs *referenceframe.FrameSystem, targets []spatialmath.Pose,
	constraints *motionplan.Constraints, options *arm.MoveOptions,
) error {
	armName := d.arm.Name().Name
	currentInputs, err := d.arm.JointPositions(ctx, nil)
	if err != nil {
		return fmt.Errorf("get current joints: %w", err)
	}
	startState := armplanning.NewPlanState(nil, referenceframe.FrameSystemInputs{
		armName: currentInputs,
	})
	goals := make([]*armplanning.PlanState, len(targets))
	for i, target := range targets {
		goals[i] = armplanning.NewPlanState(referenceframe.FrameSystemPoses{
			armName: referenceframe.NewPoseInFrame(referenceframe.World, target),
		}, nil)
	}
	plan, _, err := armplanning.PlanMotion(ctx, d.logger, &armplanning.PlanRequest{
		FrameSystem: fs,
		StartState:  startState,
		Goals:       goals,
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
	if err := d.arm.MoveThroughJointPositions(ctx, trajectory, options, nil); err != nil {
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

func (d *drawer) Status(_ context.Context) (map[string]any, error) {
	return d.status(), nil
}
