package drawer

import (
	"errors"
	"fmt"
	"math"
)

const (
	defaultPenForceStepMM      = 0.1
	defaultPenForceMaxAdjustMM = 0.5
	// minPenForceSamples is how many readings a judgement rests on. Short
	// strokes pool until there are this many, since a few readings are mostly
	// the arm's own jolts.
	minPenForceSamples = 15
	// minJudgedStrokeMM leaves shorter strokes unjudged: much of their move is
	// the pen coming down and going up, which reads as a light stroke.
	minJudgedStrokeMM = 20.0
)

// PenForceConfig holds each stroke's mean force between min and max by moving
// the pen down or up for the strokes after it. A stroke that came out light is
// drawn again once at the new depth.
type PenForceConfig struct {
	// Min and Max bound a stroke's mean force over the draw's baseline, in the
	// sensor's units. A moving arm reads about 0.8 N with the pen in the air.
	Min float64 `json:"min"`
	Max float64 `json:"max"`
	// StepMM is how far the pen moves after a stroke out of range. Default 0.1.
	StepMM float64 `json:"step_mm,omitempty"`
	// MaxAdjustMM bounds the total adjustment either way. Default 0.5.
	MaxAdjustMM float64 `json:"max_adjust_mm,omitempty"`
}

func (c *PenForceConfig) withDefaults() PenForceConfig {
	out := *c
	if out.StepMM == 0 {
		out.StepMM = defaultPenForceStepMM
	}
	if out.MaxAdjustMM == 0 {
		out.MaxAdjustMM = defaultPenForceMaxAdjustMM
	}
	return out
}

func (c *PenForceConfig) validate(guard *ForceGuardConfig) error {
	if guard == nil {
		return errors.New("pen_force needs force_guard, whose sensor it reads")
	}
	if c.StepMM < 0 || c.MaxAdjustMM < 0 {
		return errors.New("pen_force.step_mm and pen_force.max_adjust_mm must be >= 0 (0 uses the default)")
	}
	if c.Min <= 0 || c.Max <= c.Min {
		return fmt.Errorf("pen_force needs 0 < min < max, got min %g, max %g", c.Min, c.Max)
	}
	if c.Max >= guard.MaxForce {
		return fmt.Errorf("pen_force.max (%g) must be below force_guard.max_force (%g)", c.Max, guard.MaxForce)
	}
	return nil
}

// penForce is one drawing's regulation: the depth adjustment so far, and the
// readings not yet judged.
type penForce struct {
	cfg     PenForceConfig
	offset  float64
	pending forceStats
	light   int
	heavy   int
	redrawn int
}

type penVerdict int

const (
	penUndecided penVerdict = iota
	penInRange
	penLight
	penHeavy
)

// judge adds a stroke's readings and, once there are enough, moves the pen.
func (p *penForce) judge(stroke forceStats) penVerdict {
	p.pending.samples += stroke.samples
	p.pending.sum += stroke.sum
	if p.pending.samples < minPenForceSamples {
		return penUndecided
	}
	mean := p.pending.mean()
	p.pending = forceStats{}
	switch {
	case mean < p.cfg.Min:
		p.light++
		p.offset = math.Max(p.offset-p.cfg.StepMM, -p.cfg.MaxAdjustMM)
		return penLight
	case mean > p.cfg.Max:
		p.heavy++
		p.offset = math.Min(p.offset+p.cfg.StepMM, p.cfg.MaxAdjustMM)
		return penHeavy
	}
	return penInRange
}

func (d *drawer) finishPenForce(p *penForce) {
	d.logger.Infof("drawer: pen_force: pen at %+.2f mm; %d light and %d heavy judgements, %d strokes redrawn",
		p.offset, p.light, p.heavy, p.redrawn)
}
