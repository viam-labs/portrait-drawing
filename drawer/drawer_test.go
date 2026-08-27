package drawer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/referenceframe"
	"go.viam.com/rdk/robot/framesystem"
	"go.viam.com/rdk/spatialmath"
	"go.viam.com/rdk/testutils/inject"
	"go.viam.com/test"
)

func validCorner() *poseConfig {
	return &poseConfig{
		Translation: r3.Vector{X: 400, Y: 0, Z: 200},
		Orientation: &spatialmath.OrientationConfig{Type: spatialmath.OrientationVectorDegreesType, Value: map[string]any{"th": 180.0, "x": 0.0, "y": 0.0, "z": 1.0}},
	}
}

func TestConfigValidate_missingArm(t *testing.T) {
	cfg := &Config{PaperTopLeftCorner: validCorner(), PaperWidthMM: 100, PaperHeightMM: 60}
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "arm")
}

func TestConfigValidate_missingPaperCorner(t *testing.T) {
	cfg := &Config{Arm: "my-arm", PaperWidthMM: 100, PaperHeightMM: 60}
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "paper_top_left_corner")
}

func TestConfigValidate_missingCornerOrientation(t *testing.T) {
	cfg := &Config{
		Arm:                "my-arm",
		PaperTopLeftCorner: &poseConfig{Translation: r3.Vector{X: 1}},
		PaperWidthMM:       100,
		PaperHeightMM:      60,
	}
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "orientation")
}

func TestConfigValidate_missingPaperWidth(t *testing.T) {
	cfg := &Config{Arm: "my-arm", PaperTopLeftCorner: validCorner(), PaperHeightMM: 60}
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "paper_width_mm")
}

func TestConfigValidate_missingPaperHeight(t *testing.T) {
	cfg := &Config{Arm: "my-arm", PaperTopLeftCorner: validCorner(), PaperWidthMM: 100}
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "paper_height_mm")
}

func TestConfigValidate_negativeLiftOff(t *testing.T) {
	cfg := &Config{
		Arm:                "my-arm",
		PaperTopLeftCorner: validCorner(),
		PaperWidthMM:       100,
		PaperHeightMM:      60,
		LiftOffZMM:         -1,
	}
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "lift_off_z_mm")
}

func TestConfigValidate_homePoseMissingOrientation(t *testing.T) {
	cfg := &Config{
		Arm:                "my-arm",
		PaperTopLeftCorner: validCorner(),
		PaperWidthMM:       100,
		PaperHeightMM:      60,
		HomePose:           &poseConfig{Translation: r3.Vector{X: 1}},
	}
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "home_pose.orientation")
}

func TestConfigValidate_valid(t *testing.T) {
	cfg := &Config{
		Arm:                "my-arm",
		PaperTopLeftCorner: validCorner(),
		PaperWidthMM:       100,
		PaperHeightMM:      60,
		LiftOffZMM:         5.0,
		InputRangeOverride: map[string]map[string]referenceframe.Limit{
			"my-arm": {"joint_0": {Min: -1, Max: 1}},
		},
	}
	deps, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, deps, test.ShouldResemble, []string{"my-arm"})
}

func TestConfigValidate_strokeGeneratorInDeps(t *testing.T) {
	cfg := &Config{
		Arm:                "my-arm",
		PaperTopLeftCorner: validCorner(),
		PaperWidthMM:       100,
		PaperHeightMM:      60,
		StrokeGenerator:    "my-generator",
	}
	deps, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, deps, test.ShouldResemble, []string{"my-arm", "my-generator"})
}

func TestParseDrawPayload_valid(t *testing.T) {
	payload := map[string]interface{}{
		"polylines": []interface{}{
			[]interface{}{[]interface{}{0.0, 0.0}, []interface{}{10.0, 5.0}},
			[]interface{}{[]interface{}{20.0, 20.0}},
		},
	}
	got, err := parseDrawPayload(payload)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(got), test.ShouldEqual, 2)
	test.That(t, len(got[0]), test.ShouldEqual, 2)
	test.That(t, got[0][0], test.ShouldResemble, [2]float64{0, 0})
	test.That(t, got[0][1], test.ShouldResemble, [2]float64{10, 5})
	test.That(t, got[1][0], test.ShouldResemble, [2]float64{20, 20})
}

func TestParseDrawPayload_missingPolylines(t *testing.T) {
	_, err := parseDrawPayload(map[string]interface{}{})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "polylines")
}

func TestParseDrawPayload_emptyPolylines(t *testing.T) {
	payload := map[string]interface{}{"polylines": []interface{}{}}
	_, err := parseDrawPayload(payload)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "polylines")
}

func TestParseDrawPayload_emptyInnerPolyline(t *testing.T) {
	payload := map[string]interface{}{
		"polylines": []interface{}{
			[]interface{}{[]interface{}{0.0, 0.0}},
			[]interface{}{},
		},
	}
	_, err := parseDrawPayload(payload)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "empty")
}

func TestParseDrawPayload_wrongPointArity(t *testing.T) {
	payload := map[string]interface{}{
		"polylines": []interface{}{
			[]interface{}{[]interface{}{0.0, 0.0, 5.0}},
		},
	}
	_, err := parseDrawPayload(payload)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "2 elements")
}

func TestParseDrawPayload_nonNumeric(t *testing.T) {
	payload := map[string]interface{}{
		"polylines": []interface{}{
			[]interface{}{[]interface{}{"not a number", 0.0}},
		},
	}
	_, err := parseDrawPayload(payload)
	test.That(t, err, test.ShouldNotBeNil)
}

func TestParseDrawImageArgs_valid(t *testing.T) {
	payload := map[string]interface{}{
		"image_b64": "abc",
		"margin_mm": 20.0,
		"rotate":    90,
		"mirror":    true,
	}
	got, err := parseDrawImageArgs(payload)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, got.ImageB64, test.ShouldEqual, "abc")
	test.That(t, got.MarginMM, test.ShouldEqual, 20.0)
	test.That(t, got.Rotate, test.ShouldEqual, 90)
	test.That(t, got.Mirror, test.ShouldBeTrue)
}

func TestParseDrawImageArgs_missingImage(t *testing.T) {
	_, err := parseDrawImageArgs(map[string]interface{}{"margin_mm": 20.0})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "image_b64")
}

func TestParseDrawImageArgs_negativeMargin(t *testing.T) {
	_, err := parseDrawImageArgs(map[string]interface{}{
		"image_b64": "abc",
		"margin_mm": -1.0,
	})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "margin_mm")
}

func TestParseDrawImageArgs_badRotate(t *testing.T) {
	_, err := parseDrawImageArgs(map[string]interface{}{
		"image_b64": "abc",
		"rotate":    45,
	})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "rotate")
}

func TestDrawImage_missingGenerator(t *testing.T) {
	d := &drawer{
		logger: logging.NewTestLogger(t),
		cfg:    &Config{},
	}
	_, err := d.drawImage(context.Background(), map[string]interface{}{"image_b64": "abc"})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "stroke_generator")
}

func TestGoHome_missingHomePose(t *testing.T) {
	d := &drawer{logger: logging.NewTestLogger(t)}
	_, err := d.goHome(context.Background())
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "home_pose")
}

func TestCancel_nothingRunning(t *testing.T) {
	d := &drawer{logger: logging.NewTestLogger(t)}
	resp, err := d.cancel(context.Background())
	test.That(t, err, test.ShouldBeNil)
	test.That(t, resp["canceled"], test.ShouldEqual, false)
}

func TestAcquireDrawSlot_blocksSecondCaller(t *testing.T) {
	d := &drawer{logger: logging.NewTestLogger(t)}
	_, release, err := d.acquireDrawSlot(context.Background())
	test.That(t, err, test.ShouldBeNil)
	defer release()
	_, _, err = d.acquireDrawSlot(context.Background())
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "already running")
}

func TestAcquireDrawSlot_releaseAllowsReacquire(t *testing.T) {
	d := &drawer{logger: logging.NewTestLogger(t)}
	_, release, err := d.acquireDrawSlot(context.Background())
	test.That(t, err, test.ShouldBeNil)
	release()
	_, release2, err := d.acquireDrawSlot(context.Background())
	test.That(t, err, test.ShouldBeNil)
	release2()
}

func TestBuildFrameSystemUsesService(t *testing.T) {
	fsSvc := inject.NewFrameSystemService("test")
	fsSvc.FrameSystemConfigFunc = func(_ context.Context) (*framesystem.Config, error) {
		return &framesystem.Config{Parts: nil}, nil
	}
	d := &drawer{
		logger:    logging.NewTestLogger(t),
		cfg:       &Config{},
		fsService: fsSvc,
	}
	fs, err := d.buildFrameSystem(context.Background())
	test.That(t, err, test.ShouldBeNil)
	test.That(t, fs, test.ShouldNotBeNil)
}

func TestBuildFrameSystemPropagatesServiceError(t *testing.T) {
	fsSvc := inject.NewFrameSystemService("test")
	fsSvc.FrameSystemConfigFunc = func(_ context.Context) (*framesystem.Config, error) {
		return nil, errors.New("boom")
	}
	d := &drawer{
		logger:    logging.NewTestLogger(t),
		cfg:       &Config{},
		fsService: fsSvc,
	}
	_, err := d.buildFrameSystem(context.Background())
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "build frame system")
	test.That(t, err.Error(), test.ShouldContainSubstring, "boom")
}

func TestBuildFrameSystemAppliesInputRangeOverride(t *testing.T) {
	fsSvc := inject.NewFrameSystemService("test")
	fsSvc.FrameSystemConfigFunc = func(_ context.Context) (*framesystem.Config, error) {
		return &framesystem.Config{Parts: nil}, nil
	}
	d := &drawer{
		logger: logging.NewTestLogger(t),
		cfg: &Config{
			InputRangeOverride: map[string]map[string]referenceframe.Limit{
				"nonexistent-arm": {"0": {Min: -1, Max: 1}},
			},
		},
		fsService: fsSvc,
	}
	_, err := d.buildFrameSystem(context.Background())
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "input_range_override")
}

func TestConfigValidate_photoWithoutStrokeGenerator(t *testing.T) {
	cfg := &Config{
		Arm:                "my-arm",
		PaperTopLeftCorner: validCorner(),
		PaperWidthMM:       100,
		PaperHeightMM:      60,
		Photo:              "photo",
	}
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "stroke_generator")
}

func TestConfigValidate_photoAndPreviewCameraInDeps(t *testing.T) {
	cfg := &Config{
		Arm:                "my-arm",
		PaperTopLeftCorner: validCorner(),
		PaperWidthMM:       100,
		PaperHeightMM:      60,
		StrokeGenerator:    "my-generator",
		Photo:              "photo",
		PreviewCamera:      "line-preview",
	}
	deps, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, deps, test.ShouldResemble, []string{"my-arm", "my-generator", "photo", "line-preview"})
}

func TestParseCaptureAndDrawArgs_defaultsPreviewScale(t *testing.T) {
	got, err := parseCaptureAndDrawArgs(map[string]interface{}{"preview": true})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, got.Preview, test.ShouldBeTrue)
	test.That(t, got.PreviewPxPerMM, test.ShouldEqual, defaultPreviewPxPerMM)
}

func TestParseCaptureAndDrawArgs_emptyPayloadIsValid(t *testing.T) {
	got, err := parseCaptureAndDrawArgs(map[string]interface{}{})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, got.Preview, test.ShouldBeFalse)
	test.That(t, got.Rotate, test.ShouldEqual, 0)
}

func TestParseCaptureAndDrawArgs_badRotate(t *testing.T) {
	_, err := parseCaptureAndDrawArgs(map[string]interface{}{"rotate": 45})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "rotate")
}

func TestParseCaptureAndDrawArgs_negativePreviewScale(t *testing.T) {
	_, err := parseCaptureAndDrawArgs(map[string]interface{}{"preview_px_per_mm": -2.0})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "preview_px_per_mm")
}

func TestCaptureAndDraw_missingPhoto(t *testing.T) {
	d := &drawer{logger: logging.NewTestLogger(t), cfg: &Config{}}
	_, err := d.captureAndDraw(context.Background(), map[string]interface{}{})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "photo")
}

func TestDrawOrPreview_returnsBase64WhenNoPreviewCamera(t *testing.T) {
	d := &drawer{
		logger: logging.NewTestLogger(t),
		cfg:    &Config{PaperWidthMM: 100, PaperHeightMM: 60},
	}
	polylines := []Polyline{{{0, 0}, {10, 10}}, {{20, 20}, {30, 25}, {40, 30}}}
	resp, err := d.drawOrPreview(context.Background(), polylines, &strokeArgs{Preview: true, PreviewPxPerMM: 2})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, resp["preview"], test.ShouldEqual, true)
	test.That(t, resp["polyline_count"], test.ShouldEqual, 2)
	test.That(t, resp["total_points"], test.ShouldEqual, 5)
	test.That(t, resp["preview_png_b64"], test.ShouldNotBeNil)
	_, hasCamera := resp["preview_camera"]
	test.That(t, hasCamera, test.ShouldBeFalse)
}

func TestConfigValidate_capturePoseInDeps(t *testing.T) {
	cfg := &Config{
		Arm:                "my-arm",
		PaperTopLeftCorner: validCorner(),
		PaperWidthMM:       100,
		PaperHeightMM:      60,
		CapturePose:        "camera-framing",
	}
	deps, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, deps, test.ShouldResemble, []string{"my-arm", "camera-framing"})
}

func TestConfigValidate_capturePoseAndHomePoseConflict(t *testing.T) {
	cfg := &Config{
		Arm:                "my-arm",
		PaperTopLeftCorner: validCorner(),
		PaperWidthMM:       100,
		PaperHeightMM:      60,
		HomePose:           validCorner(),
		CapturePose:        "camera-framing",
	}
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "capture_pose")
	test.That(t, err.Error(), test.ShouldContainSubstring, "home_pose")
}

func switchWithLabels(name string, labels []string, setPosition *uint32) *inject.Switch {
	sw := inject.NewSwitch(name)
	sw.GetNumberOfPositionsFunc = func(_ context.Context, _ map[string]interface{}) (uint32, []string, error) {
		return uint32(len(labels)), labels, nil
	}
	sw.SetPositionFunc = func(_ context.Context, position uint32, _ map[string]interface{}) error {
		*setPosition = position
		return nil
	}
	return sw
}

func TestCapturePoseGoToPosition_findsLabel(t *testing.T) {
	var got uint32
	d := &drawer{
		logger:      logging.NewTestLogger(t),
		cfg:         &Config{CapturePose: "camera-framing"},
		capturePose: switchWithLabels("camera-framing", []string{"idle", "update config", "go to"}, &got),
	}
	position, err := d.capturePoseGoToPosition(context.Background())
	test.That(t, err, test.ShouldBeNil)
	test.That(t, position, test.ShouldEqual, uint32(2))
}

func TestCapturePoseGoToPosition_labelIndexNotHardcoded(t *testing.T) {
	var got uint32
	d := &drawer{
		logger:      logging.NewTestLogger(t),
		cfg:         &Config{CapturePose: "camera-framing"},
		capturePose: switchWithLabels("camera-framing", []string{"Go To", "idle"}, &got),
	}
	position, err := d.capturePoseGoToPosition(context.Background())
	test.That(t, err, test.ShouldBeNil)
	test.That(t, position, test.ShouldEqual, uint32(0))
}

func TestCapturePoseGoToPosition_missingLabel(t *testing.T) {
	var got uint32
	d := &drawer{
		logger:      logging.NewTestLogger(t),
		cfg:         &Config{CapturePose: "some-switch"},
		capturePose: switchWithLabels("some-switch", []string{"off", "on"}, &got),
	}
	_, err := d.capturePoseGoToPosition(context.Background())
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "go to")
	test.That(t, err.Error(), test.ShouldContainSubstring, "arm-position-saver")
}

func TestGoToRestPose_usesSwitch(t *testing.T) {
	var got uint32
	d := &drawer{
		logger:      logging.NewTestLogger(t),
		cfg:         &Config{CapturePose: "camera-framing"},
		capturePose: switchWithLabels("camera-framing", []string{"idle", "update config", "go to"}, &got),
	}
	test.That(t, d.hasRestPose(), test.ShouldBeTrue)
	test.That(t, d.goToRestPose(context.Background(), nil), test.ShouldBeNil)
	test.That(t, got, test.ShouldEqual, uint32(2))
}

func TestGoToRestPose_noneConfigured(t *testing.T) {
	d := &drawer{logger: logging.NewTestLogger(t), cfg: &Config{}}
	test.That(t, d.hasRestPose(), test.ShouldBeFalse)
	err := d.goToRestPose(context.Background(), nil)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "capture_pose")
}

func TestDrawWaypoints_approachIsNotLinear(t *testing.T) {
	wps := drawWaypoints([]Polyline{{{0, 0}, {10, 5}}}, r3.Vector{X: 100, Y: 200, Z: 50}, 55, 50)
	test.That(t, len(wps), test.ShouldEqual, 4)

	test.That(t, wps[0].label, test.ShouldEqual, "polyline 0 approach")
	test.That(t, wps[0].linear, test.ShouldBeFalse)
	test.That(t, wps[0].z, test.ShouldEqual, 55.0)

	for _, wp := range wps[1:] {
		test.That(t, wp.linear, test.ShouldBeTrue)
	}
}

func TestDrawWaypoints_everyPolylineTravelsFreely(t *testing.T) {
	wps := drawWaypoints([]Polyline{{{0, 0}}, {{5, 5}}, {{9, 9}}}, r3.Vector{}, 5, 0)
	var free int
	for _, wp := range wps {
		if !wp.linear {
			free++
			test.That(t, wp.label, test.ShouldContainSubstring, "approach")
		}
	}
	test.That(t, free, test.ShouldEqual, 3)
}

func TestDrawWaypoints_offsetsFromPaperCorner(t *testing.T) {
	corner := r3.Vector{X: 100, Y: 200, Z: 50}
	wps := drawWaypoints([]Polyline{{{3, 7}, {11, 13}}}, corner, 55, 50)

	test.That(t, wps[1].label, test.ShouldEqual, "polyline 0 pen-down")
	test.That(t, wps[1].x, test.ShouldEqual, 103.0)
	test.That(t, wps[1].y, test.ShouldEqual, 207.0)
	test.That(t, wps[1].z, test.ShouldEqual, 50.0)

	test.That(t, wps[2].label, test.ShouldEqual, "polyline 0 point 1")
	test.That(t, wps[2].x, test.ShouldEqual, 111.0)
	test.That(t, wps[2].y, test.ShouldEqual, 213.0)
}

func TestDrawWaypoints_penUpReturnsToLastPoint(t *testing.T) {
	wps := drawWaypoints([]Polyline{{{0, 0}, {10, 20}}}, r3.Vector{}, 5, 0)
	last := wps[len(wps)-1]
	test.That(t, last.label, test.ShouldEqual, "polyline 0 pen-up")
	test.That(t, last.x, test.ShouldEqual, 10.0)
	test.That(t, last.y, test.ShouldEqual, 20.0)
	test.That(t, last.z, test.ShouldEqual, 5.0)
}

func TestDrawWaypoints_onlyPenUpEndsAPolyline(t *testing.T) {
	wps := drawWaypoints([]Polyline{{{0, 0}, {1, 1}}, {{5, 5}}}, r3.Vector{}, 5, 0)
	var ends int
	for _, wp := range wps {
		if wp.endsPolyline {
			ends++
			test.That(t, wp.label, test.ShouldContainSubstring, "pen-up")
			test.That(t, wp.z, test.ShouldEqual, 5.0)
		}
	}
	test.That(t, ends, test.ShouldEqual, 2)
}

func TestConfigValidate_allowedCollisionNeedsBothFrames(t *testing.T) {
	cfg := &Config{
		Arm:                "my-arm",
		PaperTopLeftCorner: validCorner(),
		PaperWidthMM:       100,
		PaperHeightMM:      60,
		AllowedCollisions:  []AllowedCollision{{Frame1: "camera-1_origin"}},
	}
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "allowed_collisions[0]")
}

func TestCollisionSpecs_emptyIsNil(t *testing.T) {
	test.That(t, collisionSpecs(nil), test.ShouldBeNil)
	test.That(t, collisionSpecs([]AllowedCollision{}), test.ShouldBeNil)
}

func TestCollisionSpecs_mapsEveryPair(t *testing.T) {
	specs := collisionSpecs([]AllowedCollision{
		{Frame1: "arm-1:wrist_link", Frame2: "camera-1_origin"},
		{Frame1: "arm-1:forearm_link", Frame2: "camera-1_origin"},
	})
	test.That(t, len(specs), test.ShouldEqual, 1)
	test.That(t, len(specs[0].Allows), test.ShouldEqual, 2)
	test.That(t, specs[0].Allows[0].Frame1, test.ShouldEqual, "arm-1:wrist_link")
	test.That(t, specs[0].Allows[1].Frame2, test.ShouldEqual, "camera-1_origin")
}

func TestConstraints_travelCarriesAllowancesWithoutLinear(t *testing.T) {
	d := &drawer{
		logger:         logging.NewTestLogger(t),
		cfg:            &Config{},
		collisionSpecs: collisionSpecs([]AllowedCollision{{Frame1: "a", Frame2: "b"}}),
	}
	c := d.constraints(false)
	test.That(t, c, test.ShouldNotBeNil)
	test.That(t, len(c.LinearConstraint), test.ShouldEqual, 0)
	test.That(t, len(c.CollisionSpecification), test.ShouldEqual, 1)
}

func TestConstraints_penContactAddsLinear(t *testing.T) {
	d := &drawer{
		logger:         logging.NewTestLogger(t),
		cfg:            &Config{},
		collisionSpecs: collisionSpecs([]AllowedCollision{{Frame1: "a", Frame2: "b"}}),
	}
	c := d.constraints(true)
	test.That(t, len(c.LinearConstraint), test.ShouldEqual, 1)
	test.That(t, c.LinearConstraint[0].LineToleranceMm, test.ShouldEqual, drawLineToleranceMM)
	test.That(t, len(c.CollisionSpecification), test.ShouldEqual, 1)
}

func TestConstraints_nilWhenNothingApplies(t *testing.T) {
	d := &drawer{logger: logging.NewTestLogger(t), cfg: &Config{}}
	test.That(t, d.constraints(false), test.ShouldBeNil)
	test.That(t, d.constraints(true), test.ShouldNotBeNil)
}

func TestParseCaptureAndDrawArgs_recaptureDefaultsTrue(t *testing.T) {
	got, err := parseCaptureAndDrawArgs(map[string]interface{}{})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, got.recapture(), test.ShouldBeTrue)
}

func TestParseCaptureAndDrawArgs_recaptureFalse(t *testing.T) {
	got, err := parseCaptureAndDrawArgs(map[string]interface{}{"recapture": false})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, got.recapture(), test.ShouldBeFalse)
}

func TestParseCaptureAndDrawArgs_recaptureKeepsStrokeArgs(t *testing.T) {
	got, err := parseCaptureAndDrawArgs(map[string]interface{}{
		"recapture": false,
		"preview":   true,
		"margin_mm": 25.0,
	})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, got.recapture(), test.ShouldBeFalse)
	test.That(t, got.Preview, test.ShouldBeTrue)
	test.That(t, got.MarginMM, test.ShouldEqual, 25.0)
	test.That(t, got.PreviewPxPerMM, test.ShouldEqual, defaultPreviewPxPerMM)
}

func TestDrawOrPreview_pushesToPreviewCameraOnPreview(t *testing.T) {
	var got map[string]interface{}
	cam := inject.NewCamera("line-preview")
	cam.DoFunc = func(_ context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
		got = cmd
		return nil, nil
	}
	d := &drawer{
		logger:        logging.NewTestLogger(t),
		cfg:           &Config{PaperWidthMM: 100, PaperHeightMM: 60, PreviewCamera: "line-preview"},
		previewCamera: cam,
	}
	resp, err := d.drawOrPreview(context.Background(), []Polyline{{{0, 0}, {10, 10}}},
		&strokeArgs{Preview: true, PreviewPxPerMM: 2})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, got, test.ShouldNotBeNil)
	test.That(t, resp["preview_camera"], test.ShouldEqual, "line-preview")
	_, hasB64 := resp["preview_png_b64"]
	test.That(t, hasB64, test.ShouldBeFalse)
}

func TestDrawOrPreview_previewFailureIsFatalOnlyForPreview(t *testing.T) {
	// Mid-draw the picture is a convenience; refusing to draw over it would be
	// the wrong trade. Asking for a preview and getting none is a real failure.
	cam := inject.NewCamera("line-preview")
	cam.DoFunc = func(_ context.Context, _ map[string]interface{}) (map[string]interface{}, error) {
		return nil, errors.New("buffer unavailable")
	}
	d := &drawer{
		logger:        logging.NewTestLogger(t),
		cfg:           &Config{PaperWidthMM: 100, PaperHeightMM: 60, PreviewCamera: "line-preview"},
		previewCamera: cam,
	}
	_, err := d.drawOrPreview(context.Background(), []Polyline{{{0, 0}, {10, 10}}},
		&strokeArgs{Preview: true, PreviewPxPerMM: 2})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "push preview")
}

func TestDrawOrPreview_fallsBackToBase64WithoutACamera(t *testing.T) {
	d := &drawer{
		logger: logging.NewTestLogger(t),
		cfg:    &Config{PaperWidthMM: 100, PaperHeightMM: 60},
	}
	resp, err := d.drawOrPreview(context.Background(), []Polyline{{{0, 0}, {10, 10}}},
		&strokeArgs{Preview: true, PreviewPxPerMM: 2})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, resp["preview_png_b64"], test.ShouldNotBeNil)
}

func TestStatus_idleBeforeAnything(t *testing.T) {
	d := &drawer{logger: logging.NewTestLogger(t), cfg: &Config{}}
	s := d.status()
	test.That(t, s["state"], test.ShouldEqual, phaseIdle)
	test.That(t, s["drawing"], test.ShouldEqual, false)
	_, hasTotal := s["polylines_total"]
	test.That(t, hasTotal, test.ShouldBeFalse)
}

func TestStatus_reportsDrawingProgress(t *testing.T) {
	d := &drawer{logger: logging.NewTestLogger(t), cfg: &Config{}}
	d.setPhase(phaseMoving)
	d.startDrawing(138, 1590)
	d.advanceDrawing(35)
	s := d.status()
	test.That(t, s["state"], test.ShouldEqual, phaseDrawing)
	test.That(t, s["polylines_total"], test.ShouldEqual, 138)
	test.That(t, s["polylines_done"], test.ShouldEqual, 35)
	test.That(t, s["points_total"], test.ShouldEqual, 1590)
	test.That(t, s["percent"], test.ShouldEqual, 25)
	test.That(t, s["elapsed_sec"], test.ShouldNotBeNil)
}

func TestStatus_readableWhileTheSlotIsHeld(t *testing.T) {
	// The whole point: a drawing runs for minutes with its DoCommand still
	// outstanding, so status must not wait on the draw slot.
	d := &drawer{logger: logging.NewTestLogger(t), cfg: &Config{}}
	_, release, err := d.acquireDrawSlot(context.Background())
	test.That(t, err, test.ShouldBeNil)
	defer release()
	d.startDrawing(10, 100)
	d.advanceDrawing(3)
	s := d.status()
	test.That(t, s["drawing"], test.ShouldEqual, true)
	test.That(t, s["polylines_done"], test.ShouldEqual, 3)
}

func TestStatus_failureIsRecorded(t *testing.T) {
	d := &drawer{logger: logging.NewTestLogger(t), cfg: &Config{}}
	d.setPhase(phaseMoving)
	d.setFailed(errors.New("planner gave up"))
	s := d.status()
	test.That(t, s["state"], test.ShouldEqual, phaseFailed)
	test.That(t, s["last_error"], test.ShouldContainSubstring, "planner gave up")
}

func TestStatus_elapsedStopsAtTheEnd(t *testing.T) {
	d := &drawer{logger: logging.NewTestLogger(t), cfg: &Config{}}
	d.setPhase(phaseMoving)
	d.setPhase(phaseDone)
	first := d.status()["elapsed_sec"]
	time.Sleep(1100 * time.Millisecond)
	test.That(t, d.status()["elapsed_sec"], test.ShouldEqual, first)
}

func TestDoCommand_statusVerb(t *testing.T) {
	d := &drawer{logger: logging.NewTestLogger(t), cfg: &Config{}}
	resp, err := d.DoCommand(context.Background(), map[string]interface{}{"status": map[string]interface{}{}})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, resp["state"], test.ShouldEqual, phaseIdle)
}
