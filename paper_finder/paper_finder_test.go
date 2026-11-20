package paperfinder

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/data"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/generic"
	"go.viam.com/rdk/spatialmath"
	"go.viam.com/test"
	"go.viam.com/utils/protoutils"
)

func ptr[T any](v T) *T { return &v }

func validConfig() *Config {
	return &Config{
		Camera:          "cam",
		SourceName:      "color",
		SurveyPose:      "mat-view",
		ToolOrientation: &spatialmath.OrientationConfig{Type: spatialmath.OrientationVectorDegreesType, Value: map[string]any{"x": 0, "y": 0, "z": -1, "th": 0}},
		PenDownZ:        ptr(290.0),
	}
}

func TestConfigValidate_requiresCameraOrientationAndPenDownZ(t *testing.T) {
	for field, mutate := range map[string]func(*Config){
		"camera":           func(c *Config) { c.Camera = "" },
		"tool_orientation": func(c *Config) { c.ToolOrientation = nil },
		"pen_down_z":       func(c *Config) { c.PenDownZ = nil },
	} {
		cfg := validConfig()
		mutate(cfg)
		_, _, err := cfg.Validate("")
		test.That(t, err, test.ShouldNotBeNil)
		test.That(t, err.Error(), test.ShouldContainSubstring, field)
	}
}

func TestConfigValidate_depsIncludeSurveyPoseAndDrawer(t *testing.T) {
	cfg := validConfig()
	cfg.Drawer = "drawer"
	deps, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, deps, test.ShouldResemble, []string{"cam", "mat-view", "drawer"})
}

func TestConfigValidate_calibrationPoints(t *testing.T) {
	cfg := validConfig()
	cfg.Drawer = "drawer"
	cfg.CalibrationPoints = []r3.Vector{{X: 1}, {X: 2}, {X: 3}, {X: 4}}
	_, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "at least 5")

	cfg.CalibrationPoints = append(cfg.CalibrationPoints, r3.Vector{X: 5})
	cfg.Drawer = ""
	_, _, err = cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "drawer")
}

type fakeCamera struct{ filters [][]string }

func (c *fakeCamera) Images(_ context.Context, filter []string, _ map[string]interface{}) (
	[]camera.NamedImage, resource.ResponseMetadata, error,
) {
	c.filters = append(c.filters, filter)
	img, err := camera.NamedImageFromBytes([]byte("photo"), "color", "image/jpeg", data.Annotations{})
	return []camera.NamedImage{img}, resource.ResponseMetadata{}, err
}

type fakeSwitch struct{ positions []uint32 }

func (s *fakeSwitch) SetPosition(_ context.Context, p uint32, _ map[string]interface{}) error {
	s.positions = append(s.positions, p)
	return nil
}

type fakeDrawer struct{ cmds []map[string]interface{} }

func (d *fakeDrawer) DoCommand(_ context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	d.cmds = append(d.cmds, cmd)
	return map[string]interface{}{"total_points": 1}, nil
}

type fakePython struct {
	out   map[string]string
	stdin map[string][]byte
}

func (p *fakePython) run(_ context.Context, stdin []byte, args ...string) ([]byte, error) {
	p.stdin[args[0]] = stdin
	return []byte(p.out[args[0]]), nil
}

func newTestFinder(t *testing.T, cfg *Config, py map[string]string) (*paperFinder, *fakeCamera, *fakeSwitch, *fakeDrawer, *fakePython) {
	t.Helper()
	cam, sw, dr := &fakeCamera{}, &fakeSwitch{}, &fakeDrawer{}
	fp := &fakePython{out: py, stdin: map[string][]byte{}}
	f, err := newWithClients(generic.Named("finder"), logging.NewTestLogger(t), cfg, cam, sw, dr, fp.run, t.TempDir())
	test.That(t, err, test.ShouldBeNil)
	return f, cam, sw, dr, fp
}

// halfScale maps 2 px to 1 mm with image +y running arm -Y: a camera looking
// straight down, not mirrored.
var halfScale = calibration{Homography: [3][3]float64{{0.5, 0, 0}, {0, -0.5, 0}, {0, 0, 1}}}

func sheetJSON(t *testing.T, sheets ...map[string]interface{}) string {
	t.Helper()
	raw, err := json.Marshal(map[string]interface{}{"sheets": sheets})
	test.That(t, err, test.ShouldBeNil)
	return string(raw)
}

// cardAt is a card in pixels with its top-left at (u, v), at halfScale.
func cardAt(u, v float64, used bool) map[string]interface{} {
	w, h := 2*101.6, 2*152.4
	return map[string]interface{}{
		"kind": "card", "used": used, "strokes": 0,
		"corners_px": [][2]float64{{u, v}, {u + w, v}, {u + w, v + h}, {u, v + h}},
	}
}

func TestFind_needsCalibration(t *testing.T) {
	f, _, _, _, _ := newTestFinder(t, validConfig(), nil)
	_, err := f.DoCommand(context.Background(), map[string]interface{}{"find": map[string]interface{}{}})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "calibrate")
}

func TestFind_goesToSurveyPoseAndReadsColour(t *testing.T) {
	f, cam, sw, _, fp := newTestFinder(t, validConfig(), map[string]string{"sheets": `{"sheets":[]}`})
	f.cal = &halfScale
	_, err := f.DoCommand(context.Background(), map[string]interface{}{"find": nil})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, sw.positions, test.ShouldResemble, []uint32{surveyPosition})
	test.That(t, cam.filters, test.ShouldResemble, [][]string{{"color"}})
	test.That(t, string(fp.stdin["sheets"]), test.ShouldEqual, "photo")
}

func paperOf(t *testing.T, sheet interface{}) (tl, tr map[string]interface{}, w, h float64) {
	t.Helper()
	p := sheet.(map[string]interface{})["paper"].(map[string]interface{})
	tl = p["top_left_corner"].(map[string]interface{})["translation"].(map[string]interface{})
	return tl, p["top_right_corner"].(map[string]interface{}), p["width_mm"].(float64), p["height_mm"].(float64)
}

func TestFind_placesACardInArmCoordinates(t *testing.T) {
	f, _, _, _, _ := newTestFinder(t, validConfig(), map[string]string{"sheets": sheetJSON(t, cardAt(400, 200, false))})
	f.cal = &halfScale
	out, err := f.DoCommand(context.Background(), map[string]interface{}{"find": nil})
	test.That(t, err, test.ShouldBeNil)
	sheets := out["sheets"].([]map[string]interface{})
	test.That(t, len(sheets), test.ShouldEqual, 1)
	tl, tr, w, h := paperOf(t, sheets[0])
	test.That(t, tl["x"], test.ShouldAlmostEqual, 200.0)
	test.That(t, tl["y"], test.ShouldAlmostEqual, -100.0)
	test.That(t, tl["z"], test.ShouldEqual, 290.0)
	test.That(t, tr["x"], test.ShouldAlmostEqual, 301.6)
	test.That(t, tr["y"], test.ShouldAlmostEqual, -100.0)
	test.That(t, w, test.ShouldEqual, 101.6)
	test.That(t, h, test.ShouldEqual, 152.4)
}

func TestFind_nearestFirstAndUsedLeftOut(t *testing.T) {
	sheets := sheetJSON(t, cardAt(1000, 0, false), cardAt(0, 0, false), cardAt(500, 0, true))
	f, _, _, _, _ := newTestFinder(t, validConfig(), map[string]string{"sheets": sheets})
	f.cal = &halfScale
	out, err := f.DoCommand(context.Background(), map[string]interface{}{"find": map[string]interface{}{"kind": "card"}})
	test.That(t, err, test.ShouldBeNil)
	got := out["sheets"].([]map[string]interface{})
	test.That(t, len(got), test.ShouldEqual, 2)
	tl0, _, _, _ := paperOf(t, got[0])
	tl1, _, _, _ := paperOf(t, got[1])
	test.That(t, tl0["x"], test.ShouldAlmostEqual, 0.0)
	test.That(t, tl1["x"], test.ShouldAlmostEqual, 500.0)

	out, err = f.DoCommand(context.Background(), map[string]interface{}{"find": map[string]interface{}{"include_used": true}})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(out["sheets"].([]map[string]interface{})), test.ShouldEqual, 3)
}

func TestFind_kindFilter(t *testing.T) {
	f, _, _, _, _ := newTestFinder(t, validConfig(), map[string]string{"sheets": sheetJSON(t, cardAt(0, 0, false))})
	f.cal = &halfScale
	out, err := f.DoCommand(context.Background(), map[string]interface{}{"find": map[string]interface{}{"kind": "name_tag"}})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, out["sheets"], test.ShouldBeEmpty)

	_, err = f.DoCommand(context.Background(), map[string]interface{}{"find": map[string]interface{}{"kind": "portrait"}})
	test.That(t, err, test.ShouldNotBeNil)
}

func TestFind_nameTagPaperIsTheLabelInsideItsBacking(t *testing.T) {
	bw, bh := 2*(82+2*defaultBackingMarginMM), 2*(53+2*defaultBackingMarginMM)
	tag := map[string]interface{}{
		"kind": "name_tag", "used": false, "handwriting": 0, "logo": map[string]interface{}{"rotation": 0},
		"corners_px": [][2]float64{{0, 0}, {bw, 0}, {bw, bh}, {0, bh}},
	}
	f, _, _, _, _ := newTestFinder(t, validConfig(), map[string]string{"sheets": sheetJSON(t, tag)})
	f.cal = &halfScale
	out, err := f.DoCommand(context.Background(), map[string]interface{}{"find": nil})
	test.That(t, err, test.ShouldBeNil)
	tl, _, w, h := paperOf(t, out["sheets"].([]map[string]interface{})[0])
	test.That(t, tl["x"], test.ShouldAlmostEqual, defaultBackingMarginMM)
	test.That(t, tl["y"], test.ShouldAlmostEqual, -defaultBackingMarginMM)
	test.That(t, w, test.ShouldEqual, 82.0)
	test.That(t, h, test.ShouldEqual, 53.0)
}

func TestFind_rejectsWrongSizesAndMirroredCalibrations(t *testing.T) {
	big := cardAt(0, 0, false)
	big["corners_px"] = [][2]float64{{0, 0}, {400, 0}, {400, 600}, {0, 600}}
	f, _, _, _, _ := newTestFinder(t, validConfig(), map[string]string{"sheets": sheetJSON(t, big)})
	f.cal = &halfScale
	out, err := f.DoCommand(context.Background(), map[string]interface{}{"find": nil})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, out["sheets"], test.ShouldBeEmpty)
	rejected := out["rejected"].([]map[string]interface{})
	test.That(t, rejected[0]["reason"], test.ShouldContainSubstring, "measures 200 x 300")

	f, _, _, _, _ = newTestFinder(t, validConfig(), map[string]string{"sheets": sheetJSON(t, cardAt(0, 0, false))})
	f.cal = &calibration{Homography: [3][3]float64{{0.5, 0, 0}, {0, 0.5, 0}, {0, 0, 1}}}
	out, err = f.DoCommand(context.Background(), map[string]interface{}{"find": nil})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, out["rejected"].([]map[string]interface{})[0]["reason"], test.ShouldContainSubstring, "mirrors")
}

func calibrationConfig() *Config {
	cfg := validConfig()
	cfg.Drawer = "drawer"
	cfg.CalibrationPoints = []r3.Vector{{X: 150, Y: -100}, {X: 150, Y: 100}, {X: 300, Y: -100}, {X: 300, Y: 100}, {X: 220, Y: 30}}
	return cfg
}

const fitOK = `{"homography": [[0.5,0,0],[0,-0.5,0],[0,0,1]], "rms_mm": 0.3, "max_mm": 0.5,
  "matched": [{},{},{},{},{}], "unmatched_arm_points": []}`

func TestCalibrate_drawsACrossAtEachPoint(t *testing.T) {
	f, _, _, dr, _ := newTestFinder(t, calibrationConfig(), map[string]string{"crosses": `{"crosses":[]}`, "fit": fitOK})
	_, err := f.DoCommand(context.Background(), map[string]interface{}{"calibrate": nil})
	test.That(t, err, test.ShouldBeNil)
	// One draw per cross, so each touches off on its own sheet.
	test.That(t, len(dr.cmds), test.ShouldEqual, 5)
	draw := dr.cmds[0]["draw"].(map[string]interface{})
	polylines := draw["polylines"].([]interface{})
	test.That(t, len(polylines), test.ShouldEqual, 2)

	paper := draw["paper"].(map[string]interface{})
	tl := paper["top_left_corner"].(map[string]interface{})["translation"].(map[string]interface{})
	margin := defaultCrossMM/2 + 5
	// The first point is (150, -100), at the middle of its page.
	test.That(t, tl["x"], test.ShouldEqual, 150-margin)
	test.That(t, tl["y"], test.ShouldEqual, -100+margin)
	test.That(t, tl["z"], test.ShouldEqual, 290.0)
	test.That(t, paper["width_mm"], test.ShouldEqual, 2*margin)
	test.That(t, paper["height_mm"], test.ShouldEqual, 2*margin)
	first := polylines[0].([]interface{})
	test.That(t, first[0], test.ShouldResemble, []float64{margin - defaultCrossMM/2, margin})
	test.That(t, first[1], test.ShouldResemble, []float64{margin + defaultCrossMM/2, margin})
}

func TestCalibrate_fitsCrossesToTheArmPointsAndKeepsTheResult(t *testing.T) {
	cfg := calibrationConfig()
	f, _, _, _, fp := newTestFinder(t, cfg, map[string]string{"crosses": `{"crosses":[[1,2],[3,4]]}`, "fit": fitOK})
	out, err := f.DoCommand(context.Background(), map[string]interface{}{"calibrate": map[string]interface{}{}})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, out["rms_mm"], test.ShouldEqual, 0.3)

	var pairs struct {
		Image [][2]float64 `json:"image_points"`
		Arm   [][2]float64 `json:"arm_points"`
	}
	test.That(t, json.Unmarshal(fp.stdin["fit"], &pairs), test.ShouldBeNil)
	test.That(t, pairs.Image, test.ShouldResemble, [][2]float64{{1, 2}, {3, 4}})
	test.That(t, pairs.Arm[4], test.ShouldResemble, [2]float64{220, 30})

	status, err := f.Status(context.Background())
	test.That(t, err, test.ShouldBeNil)
	test.That(t, status["calibrated"], test.ShouldBeTrue)

	// A restart reads the saved calibration back.
	again, err := newWithClients(f.name, f.logger, cfg, &fakeCamera{}, nil, nil, nil, filepath.Dir(f.calPath))
	test.That(t, err, test.ShouldBeNil)
	test.That(t, again.cal, test.ShouldNotBeNil)
	test.That(t, again.cal.Homography[1][1], test.ShouldEqual, -0.5)
}

func TestCalibrate_withoutRedrawUsesTheCrossesAlreadyThere(t *testing.T) {
	f, _, _, dr, _ := newTestFinder(t, calibrationConfig(), map[string]string{"crosses": `{"crosses":[]}`, "fit": fitOK})
	_, err := f.DoCommand(context.Background(), map[string]interface{}{"calibrate": map[string]interface{}{"redraw": false}})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, dr.cmds, test.ShouldBeEmpty)
}

func TestCalibrate_refusesAFitFromTooFewCrosses(t *testing.T) {
	fit := `{"homography": [[1,0,0],[0,1,0],[0,0,1]], "matched": [{},{},{},{}], "unmatched_arm_points": [[220,30]]}`
	f, _, _, _, _ := newTestFinder(t, calibrationConfig(), map[string]string{"crosses": `{"crosses":[]}`, "fit": fit})
	_, err := f.DoCommand(context.Background(), map[string]interface{}{"calibrate": nil})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "only 4 of 5")
	_, statErr := os.Stat(f.calPath)
	test.That(t, os.IsNotExist(statErr), test.ShouldBeTrue)
}

func TestLook_worksUncalibratedAndShowsTheView(t *testing.T) {
	out := `{"sheets": [{"kind": "card", "used": false}], "rejected": [], "mat_px": [[0, 0]], "annotated_png_b64": "cG5n"}`
	f, _, sw, _, fp := newTestFinder(t, validConfig(), map[string]string{"sheets": out})
	view := &fakeDrawer{}
	f.view, f.cfg.ViewCamera = view, "paper-view"
	resp, err := f.DoCommand(context.Background(), map[string]interface{}{"look": map[string]interface{}{}})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(resp["sheets"].([]interface{})), test.ShouldEqual, 1)
	test.That(t, resp["view_camera"], test.ShouldEqual, "paper-view")
	_, hasPNG := resp["annotated_png_b64"]
	test.That(t, hasPNG, test.ShouldBeFalse)
	test.That(t, sw.positions, test.ShouldResemble, []uint32{surveyPosition})
	test.That(t, string(fp.stdin["sheets"]), test.ShouldEqual, "photo")
	test.That(t, len(view.cmds), test.ShouldEqual, 1)
	set := view.cmds[0]["set_image"].(map[string]interface{})
	test.That(t, set["image_b64"], test.ShouldEqual, "cG5n")
}

func TestFind_showsTheViewToo(t *testing.T) {
	f, _, _, _, _ := newTestFinder(t, validConfig(), map[string]string{"sheets": sheetJSON(t, cardAt(100, 100, false))})
	f.cal = &halfScale
	view := &fakeDrawer{}
	f.view = view
	_, err := f.DoCommand(context.Background(), map[string]interface{}{"find": map[string]interface{}{}})
	test.That(t, err, test.ShouldBeNil)
	// The fake output carries no image, so there is nothing to show.
	test.That(t, len(view.cmds), test.ShouldEqual, 0)
}

// Each response goes back over gRPC as a structpb.Struct, which refuses Go's
// fixed-size arrays: find reached the wire only to fail there.
func TestResponsesSurviveTheWire(t *testing.T) {
	sheets := sheetJSON(t, cardAt(400, 200, false), cardAt(0, 0, false))
	f, _, _, _, _ := newTestFinder(t, calibrationConfig(), map[string]string{
		"sheets": sheets, "crosses": `{"crosses":[[1,2],[3,4]]}`, "fit": fitOK,
	})
	f.cal = &halfScale
	for _, cmd := range []map[string]interface{}{
		{"find": map[string]interface{}{"include_used": true}},
		{"look": map[string]interface{}{}},
		{"calibrate": map[string]interface{}{"redraw": false}},
		{"status": map[string]interface{}{}},
	} {
		out, err := f.DoCommand(context.Background(), cmd)
		test.That(t, err, test.ShouldBeNil)
		_, err = protoutils.StructToStructPb(out)
		test.That(t, err, test.ShouldBeNil)
	}
}
