package queue

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/generic"
	"go.viam.com/test"
)

// fakeDrawer draws one polyline per step: the test advances it with step(), and
// a pause request stops it at the next step, like the real drawer.
type fakeDrawer struct {
	mu         sync.Mutex
	starts     []int
	done       int
	total      int
	running    bool
	pauseReq   bool
	steps      chan struct{}
	stopped    chan struct{}
	calls      []string
	failOnDraw error
}

func newFakeDrawer() *fakeDrawer {
	return &fakeDrawer{steps: make(chan struct{}), stopped: make(chan struct{}, 10)}
}

func (f *fakeDrawer) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	for v, args := range cmd {
		f.mu.Lock()
		f.calls = append(f.calls, v)
		f.mu.Unlock()
		switch v {
		case "draw":
			a := args.(map[string]interface{})
			polylines := a["polylines"].([]interface{})
			start := a["start_at"].(int)
			f.mu.Lock()
			if f.failOnDraw != nil {
				f.mu.Unlock()
				return nil, f.failOnDraw
			}
			f.starts = append(f.starts, start)
			f.done, f.total, f.running, f.pauseReq = start, len(polylines), true, false
			f.mu.Unlock()
			defer func() { f.stopped <- struct{}{} }()
			for {
				f.mu.Lock()
				if f.done >= f.total {
					f.running = false
					f.mu.Unlock()
					return map[string]interface{}{"total_points": 1}, nil
				}
				if f.pauseReq {
					f.running = false
					next := f.done
					f.mu.Unlock()
					return map[string]interface{}{"paused": true, "next_polyline": next}, nil
				}
				f.mu.Unlock()
				select {
				case <-f.steps:
					f.mu.Lock()
					f.done++
					f.mu.Unlock()
				case <-ctx.Done():
					f.mu.Lock()
					f.running = false
					f.mu.Unlock()
					return nil, ctx.Err()
				}
			}
		case "pause":
			f.mu.Lock()
			if !f.running {
				f.mu.Unlock()
				return map[string]interface{}{"paused": false}, nil
			}
			f.pauseReq = true
			f.mu.Unlock()
			// Unblock the draw loop so it notices the request.
			select {
			case f.steps <- struct{}{}:
			case <-time.After(time.Second):
			}
			return map[string]interface{}{"paused": true}, nil
		case "status":
			f.mu.Lock()
			defer f.mu.Unlock()
			return map[string]interface{}{"polylines_done": float64(f.done)}, nil
		case "cancel":
			return map[string]interface{}{"canceled": true}, nil
		}
	}
	return nil, nil
}

func (f *fakeDrawer) step(t *testing.T) {
	t.Helper()
	select {
	case f.steps <- struct{}{}:
	case <-time.After(2 * time.Second):
		t.Fatal("drawer never asked for the next polyline")
	}
}

type fakeWriter struct {
	mu    sync.Mutex
	cmds  []map[string]interface{}
	wrote chan string
}

func (w *fakeWriter) DoCommand(_ context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	w.mu.Lock()
	w.cmds = append(w.cmds, cmd)
	w.mu.Unlock()
	text := cmd["write"].(map[string]interface{})["text"].(string)
	w.wrote <- text
	return map[string]interface{}{}, nil
}

type fakeGenerator struct{ lastImage string }

func (g *fakeGenerator) DoCommand(_ context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	g.lastImage = cmd["generate"].(map[string]interface{})["image_b64"].(string)
	polylines := []interface{}{}
	for i := 0; i < 6; i++ {
		polylines = append(polylines, []interface{}{[]interface{}{float64(i), 0.0}, []interface{}{float64(i), 5.0}})
	}
	return map[string]interface{}{"polylines": polylines}, nil
}

type harness struct {
	q      *receptionQueue
	drawer *fakeDrawer
	writer *fakeWriter
	gen    *fakeGenerator
	state  string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		drawer: newFakeDrawer(),
		writer: &fakeWriter{wrote: make(chan string, 10)},
		gen:    &fakeGenerator{},
		state:  filepath.Join(t.TempDir(), "queue.json"),
	}
	h.q = newWithClients(generic.Named("queue"), logging.NewTestLogger(t), h.drawer, h.gen, h.writer, &Config{}, h.state)
	t.Cleanup(func() { _ = h.q.Close(context.Background()) })
	return h
}

func (h *harness) enqueue(t *testing.T, verb string, args map[string]interface{}) string {
	t.Helper()
	resp, err := h.q.DoCommand(context.Background(), map[string]interface{}{verb: args})
	test.That(t, err, test.ShouldBeNil)
	return resp["job_id"].(string)
}

func (h *harness) waitState(t *testing.T, id, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s, err := h.q.jobSummary(id)
		test.That(t, err, test.ShouldBeNil)
		if s["state"] == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	s, _ := h.q.jobSummary(id)
	t.Fatalf("job %s never reached %q; last %v", id, want, s["state"])
}

// drawUntil feeds the fake drawer polylines until the job reaches want.
func (h *harness) drawUntil(t *testing.T, id, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s, err := h.q.jobSummary(id)
		test.That(t, err, test.ShouldBeNil)
		if s["state"] == want {
			return
		}
		select {
		case h.drawer.steps <- struct{}{}:
		case <-time.After(5 * time.Millisecond):
		}
	}
	s, _ := h.q.jobSummary(id)
	t.Fatalf("job %s never reached %q; last %v", id, want, s["state"])
}

func (h *harness) waitDrawing(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.drawer.mu.Lock()
		running := h.drawer.running
		h.drawer.mu.Unlock()
		if running {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("drawer never started")
}

func TestPortraitIsDrawnAndThePhotoIsNotKept(t *testing.T) {
	h := newHarness(t)
	h.q.start()
	id := h.enqueue(t, "enqueue_portrait", map[string]interface{}{"image_b64": "SECRET-PHOTO", "name": "Ada"})
	test.That(t, h.gen.lastImage, test.ShouldEqual, "SECRET-PHOTO")

	h.waitDrawing(t)
	raw, err := os.ReadFile(h.state)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, strings.Contains(string(raw), "SECRET-PHOTO"), test.ShouldBeFalse)

	h.drawUntil(t, id, stateDone)
	h.drawer.mu.Lock()
	defer h.drawer.mu.Unlock()
	test.That(t, h.drawer.starts, test.ShouldResemble, []int{0})
}

func TestNameTagsGoBeforeWaitingPortraits(t *testing.T) {
	h := newHarness(t)
	portrait := h.enqueue(t, "enqueue_portrait", map[string]interface{}{"image_b64": "x"})
	h.enqueue(t, "enqueue_name_tag", map[string]interface{}{"name": "Grace Hopper"})

	st := h.q.status()
	waiting := st["waiting"].([]interface{})
	test.That(t, waiting[0].(map[string]interface{})["kind"], test.ShouldEqual, kindNameTag)

	h.q.start()
	select {
	case name := <-h.writer.wrote:
		test.That(t, name, test.ShouldEqual, "Grace Hopper")
	case <-time.After(2 * time.Second):
		t.Fatal("name tag was not written first")
	}
	h.waitDrawing(t)
	s, _ := h.q.jobSummary(portrait)
	test.That(t, s["state"], test.ShouldEqual, stateActive)
}

func TestNameTagPausesThePortraitThenItResumesWhereItStopped(t *testing.T) {
	h := newHarness(t)
	h.q.start()
	portrait := h.enqueue(t, "enqueue_portrait", map[string]interface{}{"image_b64": "x"})
	h.waitDrawing(t)
	h.drawer.step(t)
	h.drawer.step(t)

	h.enqueue(t, "enqueue_name_tag", map[string]interface{}{"name": "Ada"})
	select {
	case name := <-h.writer.wrote:
		test.That(t, name, test.ShouldEqual, "Ada")
	case <-time.After(3 * time.Second):
		t.Fatal("name tag was not written while the portrait waited")
	}

	h.drawUntil(t, portrait, stateDone)

	h.drawer.mu.Lock()
	defer h.drawer.mu.Unlock()
	test.That(t, len(h.drawer.starts), test.ShouldEqual, 2)
	test.That(t, h.drawer.starts[0], test.ShouldEqual, 0)
	test.That(t, h.drawer.starts[1], test.ShouldBeGreaterThanOrEqualTo, 2)
	test.That(t, h.drawer.calls, test.ShouldContain, "pause")
}

func TestARestartResumesAnInterruptedPortrait(t *testing.T) {
	state := filepath.Join(t.TempDir(), "queue.json")
	job := &Job{ID: "abc", Kind: kindPortrait, State: stateActive, NextPolyline: 4, CreatedAt: time.Now()}
	for i := 0; i < 6; i++ {
		job.Polylines = append(job.Polylines, [][2]float64{{float64(i), 0}, {float64(i), 5}})
	}
	raw, _ := json.Marshal([]*Job{job})
	test.That(t, os.WriteFile(state, raw, 0o600), test.ShouldBeNil)

	drawer := newFakeDrawer()
	q := newWithClients(generic.Named("queue"), logging.NewTestLogger(t), drawer, &fakeGenerator{}, nil, &Config{}, state)
	test.That(t, q.load(), test.ShouldBeNil)
	s, _ := q.jobSummary("abc")
	test.That(t, s["state"], test.ShouldEqual, statePaused)

	q.start()
	defer func() { test.That(t, q.Close(context.Background()), test.ShouldBeNil) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		drawer.mu.Lock()
		n := len(drawer.starts)
		drawer.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	drawer.mu.Lock()
	defer drawer.mu.Unlock()
	test.That(t, drawer.starts, test.ShouldResemble, []int{4})
}

func TestFailedPortraitCanBeRetriedFromWhereItStopped(t *testing.T) {
	h := newHarness(t)
	h.drawer.failOnDraw = errPlan
	h.q.start()
	id := h.enqueue(t, "enqueue_portrait", map[string]interface{}{"image_b64": "x"})
	h.waitState(t, id, stateFailed)
	s, _ := h.q.jobSummary(id)
	test.That(t, s["error"], test.ShouldContainSubstring, "no plan")

	h.drawer.mu.Lock()
	h.drawer.failOnDraw = nil
	h.drawer.mu.Unlock()
	_, err := h.q.DoCommand(context.Background(), map[string]interface{}{"retry_job": map[string]interface{}{"job_id": id}})
	test.That(t, err, test.ShouldBeNil)
	h.waitDrawing(t)
}

var errPlan = &planError{}

type planError struct{}

func (*planError) Error() string { return "no plan" }

func TestCancelAQueuedJob(t *testing.T) {
	h := newHarness(t)
	id := h.enqueue(t, "enqueue_name_tag", map[string]interface{}{"name": "Ada"})
	resp, err := h.q.DoCommand(context.Background(), map[string]interface{}{"cancel_job": map[string]interface{}{"job_id": id}})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, resp["state"], test.ShouldEqual, stateCanceled)
	test.That(t, h.q.status()["waiting"], test.ShouldBeEmpty)
}

func TestEnqueueValidation(t *testing.T) {
	h := newHarness(t)
	_, err := h.q.DoCommand(context.Background(), map[string]interface{}{"enqueue_portrait": map[string]interface{}{}})
	test.That(t, err, test.ShouldNotBeNil)
	_, err = h.q.DoCommand(context.Background(), map[string]interface{}{"enqueue_name_tag": map[string]interface{}{"name": "  "}})
	test.That(t, err, test.ShouldNotBeNil)

	noWriter := newWithClients(generic.Named("q"), logging.NewTestLogger(t), newFakeDrawer(), &fakeGenerator{}, nil, &Config{},
		filepath.Join(t.TempDir(), "q.json"))
	_, err = noWriter.DoCommand(context.Background(), map[string]interface{}{"enqueue_name_tag": map[string]interface{}{"name": "Ada"}})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "name_tag_writer")
}

func TestNameTagCommandTemplate(t *testing.T) {
	cmd := map[string]interface{}{"write": map[string]interface{}{"text": "{name}", "lines": []interface{}{"Hi {name}", 3.0}}}
	got := fillName(cmd, "Ada").(map[string]interface{})
	write := got["write"].(map[string]interface{})
	test.That(t, write["text"], test.ShouldEqual, "Ada")
	test.That(t, write["lines"], test.ShouldResemble, []interface{}{"Hi Ada", 3.0})
	test.That(t, cmd["write"].(map[string]interface{})["text"], test.ShouldEqual, "{name}")
}

func TestConfigValidate(t *testing.T) {
	deps, _, err := (&Config{Drawer: "drawer", StrokeGenerator: "gen", NameTagWriter: "writer"}).Validate("")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, deps, test.ShouldResemble, []string{"drawer", "gen", "writer"})

	for _, bad := range []*Config{
		{StrokeGenerator: "gen"},
		{Drawer: "drawer"},
		{Drawer: "drawer", StrokeGenerator: "gen", NameTagCommand: map[string]interface{}{"write": nil}},
		{Drawer: "drawer", StrokeGenerator: "gen", MarginMM: -1},
	} {
		_, _, err := bad.Validate("")
		test.That(t, err, test.ShouldNotBeNil)
	}
}

var _ resource.Resource = (*receptionQueue)(nil)
