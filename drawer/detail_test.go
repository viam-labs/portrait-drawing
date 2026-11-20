package drawer

import (
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/test"
)

func TestDetailStrokesAreSpacedFinerFromDetailFrom(t *testing.T) {
	polys := []Polyline{{{0, 0}, {10, 0}}, {{0, 20}, {10, 20}}}
	wps := drawWaypoints(polys, 0, r3.Vector{Z: 50}, r3.Vector{X: 1}, 54, 50, 52)
	markDetail(wps, surface{hasDetail: true, detailFrom: 1})
	moves := strokeMoves(wps)
	var strokes []drawMove
	for _, mv := range moves {
		if mv.stroke {
			strokes = append(strokes, mv)
		}
	}
	test.That(t, len(strokes), test.ShouldEqual, 2)
	test.That(t, strokes[0].detail, test.ShouldBeFalse)
	test.That(t, strokes[1].detail, test.ShouldBeTrue)
	// pen-down, the points between, the far end, pen-up: 1 mm apart, then 0.5.
	test.That(t, len(strokes[0].points), test.ShouldEqual, 12)
	test.That(t, len(strokes[1].points), test.ShouldEqual, 22)
}

func TestNoDetailFromMarksNothing(t *testing.T) {
	wps := drawWaypoints([]Polyline{{{0, 0}, {10, 0}}}, 0, r3.Vector{Z: 50}, r3.Vector{X: 1}, 54, 50, 52)
	markDetail(wps, surface{})
	for _, wp := range wps {
		test.That(t, wp.detail, test.ShouldBeFalse)
	}
}

func TestDetailLimitsDefaultToAThirdOfTheStrokes(t *testing.T) {
	d := &drawer{cfg: &Config{}}
	test.That(t, d.detailLimits(strokeLimits{speed: 30, acceleration: 150}), test.ShouldResemble, strokeLimits{speed: 10, acceleration: 50})
	test.That(t, d.detailLimits(strokeLimits{}), test.ShouldResemble,
		strokeLimits{speed: defaultDetailSpeedDegsPerSec, acceleration: defaultDetailAccelerationDegsPerSec})
	d.cfg.DetailStrokeSpeedDegsPerSec = 6
	test.That(t, d.detailLimits(strokeLimits{speed: 30, acceleration: 150}).speed, test.ShouldEqual, 6.0)
}

func TestDetailFromIsParsedAndChecked(t *testing.T) {
	from, err := parseDetailFrom(map[string]any{"polylines": []any{}, "detail_from": 7.0})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, *from, test.ShouldEqual, 7)
	from, err = parseDetailFrom(map[string]any{"polylines": []any{}})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, from, test.ShouldBeNil)
	_, err = parseDetailFrom(map[string]any{"detail_from": -1.0})
	test.That(t, err, test.ShouldNotBeNil)
}
