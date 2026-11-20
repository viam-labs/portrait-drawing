package drawer

import (
	"testing"

	"github.com/golang/geo/r3"
	"go.viam.com/test"
)

func stats(mean float64, n int) forceStats {
	return forceStats{samples: n, sum: mean * float64(n)}
}

func TestPenForce_lowersALightPenAndRaisesAHeavyOne(t *testing.T) {
	p := &penForce{cfg: (&PenForceConfig{Min: 1.2, Max: 3}).withDefaults()}
	test.That(t, p.judge(stats(0.7, 40)), test.ShouldEqual, penLight)
	test.That(t, p.offset, test.ShouldAlmostEqual, -0.1)
	test.That(t, p.judge(stats(2, 40)), test.ShouldEqual, penInRange)
	test.That(t, p.offset, test.ShouldAlmostEqual, -0.1)
	test.That(t, p.judge(stats(4, 40)), test.ShouldEqual, penHeavy)
	test.That(t, p.offset, test.ShouldAlmostEqual, 0.0)
}

func TestPenForce_poolsShortStrokes(t *testing.T) {
	p := &penForce{cfg: (&PenForceConfig{Min: 1.2, Max: 3}).withDefaults()}
	test.That(t, p.judge(stats(0.5, 5)), test.ShouldEqual, penUndecided)
	test.That(t, p.judge(stats(0.5, 5)), test.ShouldEqual, penUndecided)
	test.That(t, p.judge(stats(0.5, 5)), test.ShouldEqual, penLight)
	test.That(t, p.pending.samples, test.ShouldEqual, 0)
}

func TestPenForce_staysWithinMaxAdjust(t *testing.T) {
	p := &penForce{cfg: (&PenForceConfig{Min: 1.2, Max: 3, StepMM: 0.4, MaxAdjustMM: 1}).withDefaults()}
	for i := 0; i < 5; i++ {
		p.judge(stats(0, 40))
	}
	test.That(t, p.offset, test.ShouldAlmostEqual, -1.0)
}

func TestPenForce_validate(t *testing.T) {
	guard := &ForceGuardConfig{MaxForce: 7.5}
	test.That(t, (&PenForceConfig{Min: 1.2, Max: 3}).validate(guard), test.ShouldBeNil)
	test.That(t, (&PenForceConfig{Min: 1.2, Max: 3}).validate(nil), test.ShouldNotBeNil)
	test.That(t, (&PenForceConfig{Min: 3, Max: 1.2}).validate(guard), test.ShouldNotBeNil)
	test.That(t, (&PenForceConfig{Min: 1, Max: 8}).validate(guard), test.ShouldNotBeNil)
}

func TestStrokeMoves_marksOnlyStrokes(t *testing.T) {
	moves := strokeMoves(drawWaypoints([]Polyline{{{0, 0}, {10, 0}}}, 0, r3.Vector{Z: 50}, r3.Vector{X: 1}, 54, 50, 52))
	test.That(t, len(moves), test.ShouldEqual, 3)
	test.That(t, moves[0].stroke, test.ShouldBeFalse)
	test.That(t, moves[1].stroke, test.ShouldBeFalse)
	test.That(t, moves[2].stroke, test.ShouldBeTrue)
}
