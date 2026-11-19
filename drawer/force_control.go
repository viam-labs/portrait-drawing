package drawer

import (
	"fmt"
	"math"
)

const (
	defaultTargetForce   = 1.0
	defaultGainMMPerUnit = 0.04
	defaultMaxStepMM     = 0.05
	defaultSearchStepMM  = 0.1
	defaultMaxOffsetMM   = 1.0
	// touchFraction of the target is the least force that counts as touching.
	touchFraction = 0.3
	// onTargetFraction is how close to the target a point must be to teach the
	// surface model where the paper is.
	onTargetFraction = 0.5
	maxSurfacePoints = 400
	minFitPoints     = 8
	refitEvery       = 10
	// surfaceResetMM is how far the configured paper height may move before the
	// learned surface is taken to belong to some other setup.
	surfaceResetMM = 2.0
	// forceTailSamples is how many of a move's last readings stand for the force
	// it ended at: the mean over the whole move would include the time in the air.
	forceTailSamples = 3
)

// ForceControlConfig draws with the pen held at a target force instead of at a
// fixed height. Every pen-down point is set from a surface the drawer learns
// from where the force was on target, corrected by the force the move before it
// ended at, so it costs no extra moves. It needs force_guard, whose sensor and
// baseline it uses and whose limits still apply.
type ForceControlConfig struct {
	// TargetForce is the force to draw with, over the guard's baseline, in the
	// sensor's units.
	TargetForce float64 `json:"target_force,omitempty"`
	// GainMMPerUnit is how far one unit of force error moves the next point.
	// Below 1/stiffness it settles without overshooting; a felt tip on card is
	// about 10 N/mm.
	GainMMPerUnit float64 `json:"gain_mm_per_unit,omitempty"`
	// MaxStepMM caps the correction between two points while the pen touches.
	MaxStepMM float64 `json:"max_step_mm,omitempty"`
	// SearchStepMM is how far each point goes down while the pen touches nothing.
	SearchStepMM float64 `json:"search_step_mm,omitempty"`
	// MaxOffsetMM bounds how far from the configured paper height the pen may go.
	MaxOffsetMM float64 `json:"max_offset_mm,omitempty"`
}

func (c *ForceControlConfig) withDefaults() ForceControlConfig {
	out := *c
	def := func(v *float64, d float64) {
		if *v == 0 {
			*v = d
		}
	}
	def(&out.TargetForce, defaultTargetForce)
	def(&out.GainMMPerUnit, defaultGainMMPerUnit)
	def(&out.MaxStepMM, defaultMaxStepMM)
	def(&out.SearchStepMM, defaultSearchStepMM)
	def(&out.MaxOffsetMM, defaultMaxOffsetMM)
	return out
}

func (c *ForceControlConfig) validate(guard *ForceGuardConfig) error {
	if guard == nil {
		return fmt.Errorf("force_control needs force_guard, whose sensor and baseline it uses")
	}
	for name, v := range map[string]float64{
		"target_force": c.TargetForce, "gain_mm_per_unit": c.GainMMPerUnit, "max_step_mm": c.MaxStepMM,
		"search_step_mm": c.SearchStepMM, "max_offset_mm": c.MaxOffsetMM,
	} {
		if v < 0 {
			return fmt.Errorf("force_control.%s must be >= 0 (0 uses the default), got %g", name, v)
		}
	}
	if target := c.withDefaults().TargetForce; target >= guard.MaxForce {
		return fmt.Errorf("force_control.target_force (%g) must be below force_guard.max_force (%g)", target, guard.MaxForce)
	}
	return nil
}

// surfaceModel is a plane through the heights where the pen drew at its target
// force. It outlives a drawing, so the next one on the same mat starts close.
type surfaceModel struct {
	ref    float64
	pts    [][3]float64
	since  int
	fitted bool
	// z = a + b*(x-mx) + c*(y-my); centring keeps the fit well conditioned.
	a, b, c, mx, my float64
}

func newSurfaceModel(ref float64) *surfaceModel {
	return &surfaceModel{ref: ref}
}

func (m *surfaceModel) predict(x, y float64) float64 {
	if !m.fitted {
		return m.ref
	}
	return m.a + m.b*(x-m.mx) + m.c*(y-m.my)
}

func (m *surfaceModel) add(x, y, z float64) {
	m.pts = append(m.pts, [3]float64{x, y, z})
	if len(m.pts) > maxSurfacePoints {
		m.pts = m.pts[len(m.pts)-maxSurfacePoints:]
	}
	m.since++
	if len(m.pts) >= minFitPoints && (!m.fitted || m.since >= refitEvery) {
		m.fit()
		m.since = 0
	}
}

func (m *surfaceModel) fit() {
	n := float64(len(m.pts))
	var mx, my, mz float64
	for _, p := range m.pts {
		mx, my, mz = mx+p[0], my+p[1], mz+p[2]
	}
	mx, my, mz = mx/n, my/n, mz/n
	var sxx, sxy, syy, sxz, syz float64
	for _, p := range m.pts {
		dx, dy, dz := p[0]-mx, p[1]-my, p[2]-mz
		sxx, sxy, syy = sxx+dx*dx, sxy+dx*dy, syy+dy*dy
		sxz, syz = sxz+dx*dz, syz+dy*dz
	}
	m.a, m.mx, m.my, m.fitted = mz, mx, my, true
	// Points along one line fix no tilt across it; a level plane is the honest fit.
	det := sxx*syy - sxy*sxy
	if det < 1e-6*(sxx+syy)*(sxx+syy) || sxx+syy < 1 {
		m.b, m.c = 0, 0
		return
	}
	m.b = (sxz*syy - syz*sxy) / det
	m.c = (syz*sxx - sxz*sxy) / det
}

// forceController sets each pen-down point's height for one drawing.
type forceController struct {
	cfg   ForceControlConfig
	model *surfaceModel
	ref   float64
	// off is the local correction on top of the model, carried from point to
	// point and stroke to stroke.
	off float64
}

func newForceController(cfg ForceControlConfig, model *surfaceModel, ref float64) *forceController {
	return &forceController{cfg: cfg, model: model, ref: ref}
}

func (f *forceController) height(x, y float64) float64 {
	z := f.model.predict(x, y) + f.off
	return math.Max(f.ref-f.cfg.MaxOffsetMM, math.Min(f.ref+f.cfg.MaxOffsetMM, z))
}

// observe takes the force a pen-down move to (x, y) at height z ended at, and
// sets where the next point goes.
func (f *forceController) observe(x, y, z, force float64) {
	target := f.cfg.TargetForce
	next := z
	if force < touchFraction*target {
		next -= f.cfg.SearchStepMM
	} else {
		e := target - force
		next -= math.Max(-f.cfg.MaxStepMM, math.Min(f.cfg.MaxStepMM, f.cfg.GainMMPerUnit*e))
		if math.Abs(e) <= onTargetFraction*target {
			f.model.add(x, y, z)
		}
	}
	f.off = next - f.model.predict(x, y)
}

func (f *forceController) summary() map[string]interface{} {
	out := map[string]interface{}{"fitted": f.model.fitted, "points": len(f.model.pts), "offset_mm": f.off}
	if f.model.fitted {
		out["plane"] = map[string]interface{}{
			"z_at_centre": f.model.a, "centre": []float64{f.model.mx, f.model.my},
			"mm_per_100mm_x": 100 * f.model.b, "mm_per_100mm_y": 100 * f.model.c,
		}
	}
	return out
}
