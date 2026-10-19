package textwriter

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/services/generic"
	"go.viam.com/rdk/spatialmath"
	"go.viam.com/test"

	"github.com/viam-labs/portrait-drawing/drawer"
)

func card() *drawer.PoseConfig {
	return &drawer.PoseConfig{
		Translation: r3.Vector{X: 100, Y: 50, Z: 20},
		Orientation: &spatialmath.OrientationConfig{Type: spatialmath.OrientationVectorDegreesType, Value: map[string]any{"x": 0.0, "y": 0.0, "z": -1.0, "th": 0.0}},
	}
}

func cardConfig() *Config {
	return &Config{Drawer: "d", PaperTopLeftCorner: card(), PaperWidthMM: 85.9, PaperHeightMM: 59.2}
}

type fakeDrawer struct {
	cmds []map[string]interface{}
	resp map[string]interface{}
	err  error
}

func (f *fakeDrawer) DoCommand(_ context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	f.cmds = append(f.cmds, cmd)
	return f.resp, f.err
}

const layoutJSON = `{"polylines": [[[1, 2], [3, 4]], [[5, 6], [7, 8]]], "font": "cursive (single-stroke)", "cap_mm": 7.2}`

func newTestWriter(t *testing.T, cfg *Config, drawer *fakeDrawer, out string, runErr error) (*textWriter, *[]string) {
	t.Helper()
	var got []string
	run := func(_ context.Context, args ...string) ([]byte, error) {
		got = args
		return []byte(out), runErr
	}
	return newWithClients(generic.Named("writer"), logging.NewTestLogger(t), cfg, drawer, run), &got
}

func TestConfigValidate(t *testing.T) {
	cfg := cardConfig()
	deps, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, deps, test.ShouldResemble, []string{"d"})

	noCorner := cardConfig()
	noCorner.PaperTopLeftCorner = nil
	noOrientation := cardConfig()
	noOrientation.PaperTopLeftCorner.Orientation = nil
	cornersTooClose := cardConfig()
	cornersTooClose.PaperTopRightCorner = &r3.Vector{X: 101, Y: 50}
	for name, bad := range map[string]*Config{
		"no drawer":      {PaperTopLeftCorner: card(), PaperWidthMM: 1, PaperHeightMM: 1},
		"no width":       {Drawer: "d", PaperTopLeftCorner: card(), PaperHeightMM: 1},
		"no corner":      noCorner,
		"no orientation": noOrientation,
		"corners close":  cornersTooClose,
		"fill > 1":       {Drawer: "d", PaperTopLeftCorner: card(), PaperWidthMM: 1, PaperHeightMM: 1, Fill: 1.5},
		"negative cap":   {Drawer: "d", PaperTopLeftCorner: card(), PaperWidthMM: 1, PaperHeightMM: 1, CapHeightMM: -1},
		"both fonts":     {Drawer: "d", PaperTopLeftCorner: card(), PaperWidthMM: 1, PaperHeightMM: 1, Outline: true, StrokeFont: "cursive"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := bad.Validate("")
			test.That(t, err, test.ShouldNotBeNil)
		})
	}
}

func TestParseWriteArgs(t *testing.T) {
	a, err := parseWriteArgs(map[string]interface{}{"text": "  Ada Lovelace ", "cap_height_mm": 12.0, "preview": true})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, a.Text, test.ShouldEqual, "Ada Lovelace")
	test.That(t, a.CapHeightMM, test.ShouldEqual, 12.0)
	test.That(t, a.Preview, test.ShouldBeTrue)

	_, err = parseWriteArgs(map[string]interface{}{"text": "   "})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "text is required")

	_, err = parseWriteArgs(map[string]interface{}{"text": "Ada", "fill": 2.0})
	test.That(t, err, test.ShouldNotBeNil)
}

func TestBuildCLIArgs(t *testing.T) {
	off := false
	cfg := cardConfig()
	cfg.StrokeFont, cfg.Join, cfg.SpacingMM = "futural", &off, 1.5
	w, _ := newTestWriter(t, cfg, &fakeDrawer{}, "", nil)

	args := strings.Join(w.buildCLIArgs(&writeArgs{Text: "Ada"}), " ")
	test.That(t, args, test.ShouldContainSubstring, "--text Ada")
	test.That(t, args, test.ShouldContainSubstring, "--width-mm 85.9 --height-mm 59.2")
	test.That(t, args, test.ShouldContainSubstring, "--fill 0.8")
	test.That(t, args, test.ShouldContainSubstring, "--stroke-font futural")
	test.That(t, args, test.ShouldContainSubstring, "--no-join")
	test.That(t, args, test.ShouldContainSubstring, "--spacing-mm 1.5")

	// A per-call cap height wins over the configured sizing.
	args = strings.Join(w.buildCLIArgs(&writeArgs{Text: "Ada", CapHeightMM: 12}), " ")
	test.That(t, args, test.ShouldContainSubstring, "--cap-mm 12")
	test.That(t, args, test.ShouldNotContainSubstring, "--fill")

	ocfg := cardConfig()
	ocfg.Outline, ocfg.Font, ocfg.CapHeightMM = true, "Helvetica", 10
	outline, _ := newTestWriter(t, ocfg, &fakeDrawer{}, "", nil)
	args = strings.Join(outline.buildCLIArgs(&writeArgs{Text: "Ada"}), " ")
	test.That(t, args, test.ShouldContainSubstring, "--outline --font Helvetica")
	test.That(t, args, test.ShouldContainSubstring, "--cap-mm 10")
	test.That(t, args, test.ShouldNotContainSubstring, "--stroke-font")
}

func TestWriteDrawsAndBlocksOnDrawer(t *testing.T) {
	cfg := cardConfig()
	cfg.PaperTopRightCorner = &r3.Vector{X: 100, Y: 135.9, Z: 20}
	fake := &fakeDrawer{resp: map[string]interface{}{"total_points": 4}}
	w, _ := newTestWriter(t, cfg, fake, layoutJSON, nil)

	resp, err := w.DoCommand(context.Background(), map[string]interface{}{"write": map[string]interface{}{"text": "Ada"}})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(fake.cmds), test.ShouldEqual, 1)
	draw := fake.cmds[0]["draw"].(map[string]interface{})
	test.That(t, len(draw["polylines"].([]interface{})), test.ShouldEqual, 2)

	// The card goes with every draw, as a plain map the drawer can parse back.
	paper := draw["paper"].(map[string]interface{})
	test.That(t, paper["width_mm"], test.ShouldEqual, 85.9)
	test.That(t, paper["top_right_corner"].(map[string]interface{})["y"], test.ShouldEqual, 135.9)
	corner := paper["top_left_corner"].(map[string]interface{})
	test.That(t, corner["translation"].(map[string]interface{})["x"], test.ShouldEqual, 100.0)
	test.That(t, corner["orientation"].(map[string]interface{})["type"], test.ShouldEqual, "ov_degrees")
	test.That(t, resp["polylines_total"], test.ShouldEqual, 2)
	test.That(t, resp["total_points"], test.ShouldEqual, 4)
	test.That(t, resp["font"], test.ShouldEqual, "cursive (single-stroke)")
	_, hasPolylines := resp["polylines"]
	test.That(t, hasPolylines, test.ShouldBeFalse)
}

func TestWritePreviewDoesNotDraw(t *testing.T) {
	fake := &fakeDrawer{}
	w, _ := newTestWriter(t, cardConfig(), fake, layoutJSON, nil)

	resp, err := w.DoCommand(context.Background(), map[string]interface{}{"write": map[string]interface{}{"text": "Ada", "preview": true}})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, fake.cmds, test.ShouldBeEmpty)
	test.That(t, len(resp["polylines"].([]interface{})), test.ShouldEqual, 2)
}

func TestWriteErrors(t *testing.T) {
	cfg := cardConfig()
	write := map[string]interface{}{"write": map[string]interface{}{"text": "Ada"}}

	w, _ := newTestWriter(t, cfg, &fakeDrawer{}, "", errors.New("card is 85.9x59.2mm"))
	_, err := w.DoCommand(context.Background(), write)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "card is")

	w, _ = newTestWriter(t, cfg, &fakeDrawer{err: errors.New("arm offline")}, layoutJSON, nil)
	_, err = w.DoCommand(context.Background(), write)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "arm offline")

	// A paused drawer returns with the pen up and the text unfinished; that is
	// not a written tag.
	w, _ = newTestWriter(t, cfg, &fakeDrawer{resp: map[string]interface{}{"paused": true}}, layoutJSON, nil)
	_, err = w.DoCommand(context.Background(), write)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "paused")

	_, err = w.DoCommand(context.Background(), map[string]interface{}{"nope": nil})
	test.That(t, err, test.ShouldNotBeNil)
}
