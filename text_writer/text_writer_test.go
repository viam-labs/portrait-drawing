package textwriter

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/services/generic"
	"go.viam.com/test"
)

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
	cfg := &Config{Drawer: "drawer", PaperWidthMM: 85.9, PaperHeightMM: 59.2}
	deps, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, deps, test.ShouldResemble, []string{"drawer"})

	for name, bad := range map[string]Config{
		"no drawer":    {PaperWidthMM: 1, PaperHeightMM: 1},
		"no width":     {Drawer: "d", PaperHeightMM: 1},
		"fill > 1":     {Drawer: "d", PaperWidthMM: 1, PaperHeightMM: 1, Fill: 1.5},
		"negative cap": {Drawer: "d", PaperWidthMM: 1, PaperHeightMM: 1, CapHeightMM: -1},
		"both fonts":   {Drawer: "d", PaperWidthMM: 1, PaperHeightMM: 1, Outline: true, StrokeFont: "cursive"},
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
	w, _ := newTestWriter(t, &Config{
		Drawer: "d", PaperWidthMM: 85.9, PaperHeightMM: 59.2,
		StrokeFont: "futural", Join: &off, SpacingMM: 1.5,
	}, &fakeDrawer{}, "", nil)

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

	outline, _ := newTestWriter(t, &Config{
		Drawer: "d", PaperWidthMM: 1, PaperHeightMM: 1, Outline: true, Font: "Helvetica", CapHeightMM: 10,
	}, &fakeDrawer{}, "", nil)
	args = strings.Join(outline.buildCLIArgs(&writeArgs{Text: "Ada"}), " ")
	test.That(t, args, test.ShouldContainSubstring, "--outline --font Helvetica")
	test.That(t, args, test.ShouldContainSubstring, "--cap-mm 10")
	test.That(t, args, test.ShouldNotContainSubstring, "--stroke-font")
}

func TestWriteDrawsAndBlocksOnDrawer(t *testing.T) {
	drawer := &fakeDrawer{resp: map[string]interface{}{"total_points": 4}}
	w, _ := newTestWriter(t, &Config{Drawer: "d", PaperWidthMM: 85.9, PaperHeightMM: 59.2}, drawer, layoutJSON, nil)

	resp, err := w.DoCommand(context.Background(), map[string]interface{}{"write": map[string]interface{}{"text": "Ada"}})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(drawer.cmds), test.ShouldEqual, 1)
	draw := drawer.cmds[0]["draw"].(map[string]interface{})
	test.That(t, len(draw["polylines"].([]interface{})), test.ShouldEqual, 2)
	test.That(t, resp["polylines_total"], test.ShouldEqual, 2)
	test.That(t, resp["total_points"], test.ShouldEqual, 4)
	test.That(t, resp["font"], test.ShouldEqual, "cursive (single-stroke)")
	_, hasPolylines := resp["polylines"]
	test.That(t, hasPolylines, test.ShouldBeFalse)
}

func TestWritePreviewDoesNotDraw(t *testing.T) {
	drawer := &fakeDrawer{}
	w, _ := newTestWriter(t, &Config{Drawer: "d", PaperWidthMM: 85.9, PaperHeightMM: 59.2}, drawer, layoutJSON, nil)

	resp, err := w.DoCommand(context.Background(), map[string]interface{}{"write": map[string]interface{}{"text": "Ada", "preview": true}})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, drawer.cmds, test.ShouldBeEmpty)
	test.That(t, len(resp["polylines"].([]interface{})), test.ShouldEqual, 2)
}

func TestWriteErrors(t *testing.T) {
	cfg := &Config{Drawer: "d", PaperWidthMM: 85.9, PaperHeightMM: 59.2}
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
