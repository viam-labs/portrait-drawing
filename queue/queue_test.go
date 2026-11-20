package queue

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/data"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/generic"
	"go.viam.com/rdk/testutils/inject"
	rutils "go.viam.com/rdk/utils"
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
	draws      []map[string]any
	failOnDraw error
	// pauseGate, when set, holds a pause call back until it is closed, like a
	// pause still in flight.
	pauseGate chan struct{}
}

func newFakeDrawer() *fakeDrawer {
	return &fakeDrawer{steps: make(chan struct{}), stopped: make(chan struct{}, 10)}
}

func (f *fakeDrawer) DoCommand(ctx context.Context, cmd map[string]any) (map[string]any, error) {
	for v, args := range cmd {
		f.mu.Lock()
		f.calls = append(f.calls, v)
		f.mu.Unlock()
		switch v {
		case "draw":
			a := args.(map[string]any)
			polylines := a["polylines"].([]any)
			start := a["start_at"].(int)
			f.mu.Lock()
			if f.failOnDraw != nil {
				f.mu.Unlock()
				return nil, f.failOnDraw
			}
			f.starts = append(f.starts, start)
			f.draws = append(f.draws, a)
			f.done, f.total, f.running, f.pauseReq = start, len(polylines), true, false
			f.mu.Unlock()
			defer func() { f.stopped <- struct{}{} }()
			for {
				f.mu.Lock()
				if f.done >= f.total {
					f.running = false
					f.mu.Unlock()
					return map[string]any{"total_points": 1}, nil
				}
				if f.pauseReq {
					f.running = false
					next := f.done
					f.mu.Unlock()
					return map[string]any{"paused": true, "next_polyline": next}, nil
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
			gate := f.pauseGate
			f.mu.Unlock()
			if gate != nil {
				<-gate
			}
			f.mu.Lock()
			if !f.running {
				f.mu.Unlock()
				return map[string]any{"paused": false}, nil
			}
			f.pauseReq = true
			f.mu.Unlock()
			// Unblock the draw loop so it notices the request.
			select {
			case f.steps <- struct{}{}:
			case <-time.After(time.Second):
			}
			return map[string]any{"paused": true}, nil
		case "status":
			f.mu.Lock()
			defer f.mu.Unlock()
			return map[string]any{"polylines_done": float64(f.done)}, nil
		case "cancel":
			return map[string]any{"canceled": true}, nil
		case "go_home":
			return map[string]any{"status": "ok"}, nil
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
	cmds  []map[string]any
	wrote chan string
	// block keeps the writer writing until its context ends.
	block bool
}

func (w *fakeWriter) DoCommand(ctx context.Context, cmd map[string]any) (map[string]any, error) {
	w.mu.Lock()
	w.cmds = append(w.cmds, cmd)
	block := w.block
	w.mu.Unlock()
	text := cmd["write"].(map[string]any)["text"].(string)
	w.wrote <- text
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return map[string]any{}, nil
}

type fakeGenerator struct {
	lastImage string
	fail      error
}

func (g *fakeGenerator) DoCommand(_ context.Context, cmd map[string]any) (map[string]any, error) {
	g.lastImage = cmd["generate"].(map[string]any)["image_b64"].(string)
	if g.fail != nil {
		return nil, g.fail
	}
	polylines := []any{}
	for i := range 6 {
		polylines = append(polylines, []any{[]any{float64(i), 0.0}, []any{float64(i), 5.0}})
	}
	return map[string]any{"polylines": polylines}, nil
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
	h.q.paperSwapWait = 0
	t.Cleanup(func() { _ = h.q.Close(context.Background()) })
	return h
}

func (h *harness) enqueue(t *testing.T, verb string, args map[string]any) string {
	t.Helper()
	resp, err := h.q.DoCommand(context.Background(), map[string]any{verb: args})
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
	id := h.enqueue(t, "enqueue_portrait", map[string]any{"image_b64": "SECRET-PHOTO", "name": "Ada"})
	test.That(t, h.gen.lastImage, test.ShouldEqual, "SECRET-PHOTO")

	h.waitDrawing(t)
	raw, err := os.ReadFile(h.state)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, strings.Contains(string(raw), "SECRET-PHOTO"), test.ShouldBeFalse)

	h.drawUntil(t, id, stateDone)
	s, err := h.q.jobSummary(id)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, s["polylines_total"], test.ShouldEqual, 6)
	test.That(t, s["polylines_done"], test.ShouldEqual, 6)
	h.drawer.mu.Lock()
	defer h.drawer.mu.Unlock()
	test.That(t, h.drawer.starts, test.ShouldResemble, []int{0})
}

func TestNameTagsGoBeforeWaitingPortraits(t *testing.T) {
	h := newHarness(t)
	portrait := h.enqueue(t, "enqueue_portrait", map[string]any{"image_b64": "x"})
	h.enqueue(t, "enqueue_name_tag", map[string]any{"name": "Grace Hopper"})

	st := h.q.status()
	waiting := st["waiting"].([]any)
	test.That(t, waiting[0].(map[string]any)["kind"], test.ShouldEqual, kindNameTag)

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

func TestTextJobIsWrittenWithItsOwnSizingBeforePortraits(t *testing.T) {
	h := newHarness(t)
	h.enqueue(t, "enqueue_portrait", map[string]any{"image_b64": "x"})
	id := h.enqueue(t, "enqueue_text", map[string]any{
		"text": " Welcome ", "cap_height_mm": 12.0, "preview": true, "visitor": map[string]any{"id": "v1"},
	})

	s, err := h.q.jobSummary(id)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, s["kind"], test.ShouldEqual, kindText)
	test.That(t, s["text"], test.ShouldEqual, "Welcome")
	test.That(t, s["position"], test.ShouldEqual, 1)

	h.q.start()
	select {
	case text := <-h.writer.wrote:
		test.That(t, text, test.ShouldEqual, "Welcome")
	case <-time.After(2 * time.Second):
		t.Fatal("text was not written first")
	}
	h.waitState(t, id, stateDone)
	h.writer.mu.Lock()
	defer h.writer.mu.Unlock()
	test.That(t, h.writer.cmds[0], test.ShouldResemble, map[string]any{
		"write": map[string]any{"text": "Welcome", "cap_height_mm": 12.0},
	})
}

func TestNameTagPausesThePortraitThenItResumesWhereItStopped(t *testing.T) {
	h := newHarness(t)
	// A paused portrait keeps its sheet, so it resumes with no swap after the tag.
	h.q.paperSwapWait = time.Hour
	h.q.start()
	portrait := h.enqueue(t, "enqueue_portrait", map[string]any{"image_b64": "x"})
	h.waitDrawing(t)
	h.drawer.step(t)
	h.drawer.step(t)

	h.enqueue(t, "enqueue_name_tag", map[string]any{"name": "Ada"})
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
	for i := range 6 {
		job.Polylines = append(job.Polylines, [][2]float64{{float64(i), 0}, {float64(i), 5}})
	}
	raw, _ := json.Marshal([]*Job{job})
	test.That(t, os.WriteFile(state, raw, 0o600), test.ShouldBeNil)

	drawer := newFakeDrawer()
	q := newWithClients(generic.Named("queue"), logging.NewTestLogger(t), drawer, &fakeGenerator{}, nil, &Config{}, state)
	test.That(t, q.load(), test.ShouldBeNil)
	s, _ := q.jobSummary("abc")
	test.That(t, s["state"], test.ShouldEqual, statePaused)
	test.That(t, s["polylines_total"], test.ShouldEqual, 6)

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
	id := h.enqueue(t, "enqueue_portrait", map[string]any{"image_b64": "x"})
	h.waitState(t, id, stateFailed)
	s, _ := h.q.jobSummary(id)
	test.That(t, s["error"], test.ShouldContainSubstring, "no plan")

	h.drawer.mu.Lock()
	h.drawer.failOnDraw = nil
	h.drawer.mu.Unlock()
	_, err := h.q.DoCommand(context.Background(), map[string]any{"retry_job": map[string]any{"job_id": id}})
	test.That(t, err, test.ShouldBeNil)
	h.waitDrawing(t)
}

var errPlan = &planError{}

type planError struct{}

func (*planError) Error() string { return "no plan" }

func TestCancelAQueuedJob(t *testing.T) {
	h := newHarness(t)
	id := h.enqueue(t, "enqueue_name_tag", map[string]any{"name": "Ada"})
	resp, err := h.q.DoCommand(context.Background(), map[string]any{"cancel_job": map[string]any{"job_id": id}})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, resp["state"], test.ShouldEqual, stateCanceled)
	test.That(t, h.q.status()["waiting"], test.ShouldBeEmpty)
}

func TestEnqueueValidation(t *testing.T) {
	h := newHarness(t)
	_, err := h.q.DoCommand(context.Background(), map[string]any{"enqueue_portrait": map[string]any{}})
	test.That(t, err, test.ShouldNotBeNil)
	_, err = h.q.DoCommand(context.Background(), map[string]any{"enqueue_name_tag": map[string]any{"name": "  "}})
	test.That(t, err, test.ShouldNotBeNil)
	_, err = h.q.DoCommand(context.Background(), map[string]any{"enqueue_text": map[string]any{"text": "  "}})
	test.That(t, err, test.ShouldNotBeNil)
	_, err = h.q.DoCommand(context.Background(), map[string]any{"enqueue_text": map[string]any{"text": "Hi", "fill": "big"}})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "fill")

	noWriter := newWithClients(generic.Named("q"), logging.NewTestLogger(t), newFakeDrawer(), &fakeGenerator{}, nil, &Config{},
		filepath.Join(t.TempDir(), "q.json"))
	_, err = noWriter.DoCommand(context.Background(), map[string]any{"enqueue_name_tag": map[string]any{"name": "Ada"}})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "name_tag_writer")
	_, err = noWriter.DoCommand(context.Background(), map[string]any{"enqueue_text": map[string]any{"text": "Hi"}})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "name_tag_writer")
}

func TestNameTagCommandTemplate(t *testing.T) {
	cmd := map[string]any{"write": map[string]any{"text": "{name}", "lines": []any{"Hi {name}", 3.0}}}
	got := fillName(cmd, "Ada").(map[string]any)
	write := got["write"].(map[string]any)
	test.That(t, write["text"], test.ShouldEqual, "Ada")
	test.That(t, write["lines"], test.ShouldResemble, []any{"Hi Ada", 3.0})
	test.That(t, cmd["write"].(map[string]any)["text"], test.ShouldEqual, "{name}")
}

func TestConfigValidate(t *testing.T) {
	deps, _, err := (&Config{Drawer: "drawer", StrokeGenerator: "gen", NameTagWriter: "writer"}).Validate("")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, deps, test.ShouldResemble, []string{"drawer", "gen", "writer"})

	for _, bad := range []*Config{
		{StrokeGenerator: "gen"},
		{Drawer: "drawer"},
		{Drawer: "drawer", StrokeGenerator: "gen", NameTagCommand: map[string]any{"write": nil}},
		{Drawer: "drawer", StrokeGenerator: "gen", MarginMM: -1},
	} {
		_, _, err := bad.Validate("")
		test.That(t, err, test.ShouldNotBeNil)
	}
}

var _ resource.Resource = (*receptionQueue)(nil)

// fakePhoto is a frame-buffer camera: it holds a picture until cleared.
func fakePhoto(held []byte) (*inject.Camera, *[]byte) {
	cam := inject.NewCamera("photo")
	cam.ImagesFunc = func(context.Context, []string, map[string]any) ([]camera.NamedImage, resource.ResponseMetadata, error) {
		if held == nil {
			return nil, resource.ResponseMetadata{}, errors.New("frame-buffer: no image")
		}
		depth, err := camera.NamedImageFromBytes([]byte("DEPTH"), "depth", rutils.MimeTypeRawDepth, data.Annotations{})
		if err != nil {
			return nil, resource.ResponseMetadata{}, err
		}
		color, err := camera.NamedImageFromBytes(held, "color", rutils.MimeTypeJPEG, data.Annotations{})
		return []camera.NamedImage{depth, color}, resource.ResponseMetadata{}, err
	}
	cam.DoFunc = func(_ context.Context, cmd map[string]any) (map[string]any, error) {
		if _, ok := cmd["clear"]; ok {
			held = nil
		}
		return map[string]any{}, nil
	}
	return cam, &held
}

func TestPortraitFromThePhotoCameraClearsIt(t *testing.T) {
	h := newHarness(t)
	cam, held := fakePhoto([]byte("VISITOR"))
	h.q.photo = cam
	h.enqueue(t, "enqueue_portrait", map[string]any{"name": "Ada"})
	test.That(t, h.gen.lastImage, test.ShouldEqual, base64.StdEncoding.EncodeToString([]byte("VISITOR")))
	test.That(t, *held, test.ShouldBeNil)

	_, err := h.q.DoCommand(context.Background(), map[string]any{"enqueue_portrait": map[string]any{}})
	test.That(t, err, test.ShouldNotBeNil)
}

func TestARejectedPhotoIsClearedToo(t *testing.T) {
	h := newHarness(t)
	cam, held := fakePhoto([]byte("POSTER"))
	h.q.photo = cam
	h.gen.fail = errors.New("face is too small")
	_, err := h.q.DoCommand(context.Background(), map[string]any{"enqueue_portrait": map[string]any{}})
	test.That(t, err.Error(), test.ShouldContainSubstring, "face is too small")
	test.That(t, *held, test.ShouldBeNil)
	test.That(t, h.q.status()["waiting"], test.ShouldBeEmpty)
}

func TestALatePauseLandsBeforeTheNameTagStarts(t *testing.T) {
	h := newHarness(t)
	gate := make(chan struct{})
	h.drawer.pauseGate = gate
	h.q.start()
	portrait := h.enqueue(t, "enqueue_portrait", map[string]any{"image_b64": "x"})
	h.waitDrawing(t)
	h.enqueue(t, "enqueue_name_tag", map[string]any{"name": "Ada"})
	for range 6 {
		h.drawer.step(t)
	}
	select {
	case name := <-h.writer.wrote:
		t.Fatalf("wrote %q while the pause was still in flight", name)
	case <-time.After(100 * time.Millisecond):
	}
	close(gate)
	select {
	case <-h.writer.wrote:
	case <-time.After(2 * time.Second):
		t.Fatal("name tag never written")
	}
	h.waitState(t, portrait, stateDone)
}

func TestCancelStopsANameTagBeingWritten(t *testing.T) {
	h := newHarness(t)
	h.writer.block = true
	h.q.start()
	id := h.enqueue(t, "enqueue_name_tag", map[string]any{"name": "Ada"})
	<-h.writer.wrote
	resp, err := h.q.DoCommand(context.Background(), map[string]any{"cancel_job": map[string]any{"job_id": id}})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, resp["state"], test.ShouldEqual, stateCanceled)
	h.drawer.mu.Lock()
	test.That(t, h.drawer.calls, test.ShouldContain, "cancel")
	h.drawer.mu.Unlock()

	h.writer.mu.Lock()
	h.writer.block = false
	h.writer.mu.Unlock()
	h.enqueue(t, "enqueue_name_tag", map[string]any{"name": "Grace"})
	select {
	case name := <-h.writer.wrote:
		test.That(t, name, test.ShouldEqual, "Grace")
	case <-time.After(2 * time.Second):
		t.Fatal("the queue stayed stuck on the canceled tag")
	}
	s, err := h.q.jobSummary(id)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, s["state"], test.ShouldEqual, stateCanceled)
}

func TestARestartFailsAnInterruptedNameTagInsteadOfRewritingIt(t *testing.T) {
	restartFailsAnInterruptedCard(t, &Job{ID: "tag", Kind: kindNameTag, Name: "Ada", State: stateActive, CreatedAt: time.Now()})
}

func TestARestartFailsAnInterruptedTextCardInsteadOfRewritingIt(t *testing.T) {
	restartFailsAnInterruptedCard(t, &Job{
		ID: "tag", Kind: kindText, State: stateActive, CreatedAt: time.Now(),
		Write: map[string]any{"text": "Welcome"},
	})
}

func restartFailsAnInterruptedCard(t *testing.T, job *Job) {
	t.Helper()
	state := filepath.Join(t.TempDir(), "queue.json")
	raw, _ := json.Marshal([]*Job{job})
	test.That(t, os.WriteFile(state, raw, 0o600), test.ShouldBeNil)

	writer := &fakeWriter{wrote: make(chan string, 1)}
	q := newWithClients(generic.Named("queue"), logging.NewTestLogger(t), newFakeDrawer(), &fakeGenerator{}, writer, &Config{}, state)
	test.That(t, q.load(), test.ShouldBeNil)
	q.start()
	defer func() { test.That(t, q.Close(context.Background()), test.ShouldBeNil) }()

	s, err := q.jobSummary("tag")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, s["state"], test.ShouldEqual, stateFailed)
	test.That(t, s["error"], test.ShouldContainSubstring, "replace it")
	select {
	case name := <-writer.wrote:
		t.Fatalf("rewrote %q on the partly written card", name)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestCardsInARowWaitForTheCardSwap(t *testing.T) {
	h := newHarness(t)
	h.q.paperSwapWait = 300 * time.Millisecond
	h.enqueue(t, "enqueue_name_tag", map[string]any{"name": "Ada"})
	h.enqueue(t, "enqueue_text", map[string]any{"text": "Welcome"})
	h.q.start()

	test.That(t, <-h.writer.wrote, test.ShouldEqual, "Ada")
	start := time.Now()
	_, err := h.q.DoCommand(context.Background(), map[string]any{"status": map[string]any{}})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, time.Since(start), test.ShouldBeLessThan, 100*time.Millisecond)
	select {
	case text := <-h.writer.wrote:
		test.That(t, text, test.ShouldEqual, "Welcome")
		test.That(t, time.Since(start), test.ShouldBeGreaterThanOrEqualTo, 250*time.Millisecond)
	case <-time.After(2 * time.Second):
		t.Fatal("the second card was never written")
	}
}

func TestAPortraitAfterACardDoesNotWait(t *testing.T) {
	h := newHarness(t)
	h.q.paperSwapWait = time.Hour
	h.enqueue(t, "enqueue_name_tag", map[string]any{"name": "Ada"})
	h.enqueue(t, "enqueue_portrait", map[string]any{"image_b64": "x"})
	h.q.start()

	test.That(t, <-h.writer.wrote, test.ShouldEqual, "Ada")
	h.waitDrawing(t)
}

func TestPortraitPaperIsSwappedEvenWithACardInBetween(t *testing.T) {
	h := newHarness(t)
	h.q.paperSwapWait = time.Hour
	h.q.start()
	first := h.enqueue(t, "enqueue_portrait", map[string]any{"image_b64": "x"})
	h.drawUntil(t, first, stateDone)

	h.enqueue(t, "enqueue_name_tag", map[string]any{"name": "Ada"})
	second := h.enqueue(t, "enqueue_portrait", map[string]any{"image_b64": "y"})
	test.That(t, <-h.writer.wrote, test.ShouldEqual, "Ada")
	time.Sleep(200 * time.Millisecond)

	s, err := h.q.jobSummary(second)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, s["state"], test.ShouldEqual, stateQueued)
	h.drawer.mu.Lock()
	defer h.drawer.mu.Unlock()
	test.That(t, len(h.drawer.starts), test.ShouldEqual, 1)
}

func TestACanceledCardIsSwappedAndCloseSkipsTheWait(t *testing.T) {
	h := newHarness(t)
	h.q.paperSwapWait = time.Hour
	h.writer.block = true
	h.q.start()
	id := h.enqueue(t, "enqueue_name_tag", map[string]any{"name": "Ada"})
	<-h.writer.wrote
	_, err := h.q.DoCommand(context.Background(), map[string]any{"cancel_job": map[string]any{"job_id": id}})
	test.That(t, err, test.ShouldBeNil)

	h.writer.mu.Lock()
	h.writer.block = false
	h.writer.mu.Unlock()
	next := h.enqueue(t, "enqueue_name_tag", map[string]any{"name": "Grace"})
	select {
	case name := <-h.writer.wrote:
		t.Fatalf("%s was written on the half-written card", name)
	case <-time.After(200 * time.Millisecond):
	}

	closed := make(chan struct{})
	go func() {
		_ = h.q.Close(context.Background())
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked on the paper swap")
	}
	s, err := h.q.jobSummary(next)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, s["state"], test.ShouldEqual, stateQueued)
}

// fakeFinder hands out the sheets it is given, per kind.
type fakeFinder struct {
	mu     sync.Mutex
	sheets map[string][]any
	err    error
	calls  []string
	// looking, when set, is told each time a search starts, and the search then
	// waits for gate, like the arm still on its way to the survey pose.
	looking chan string
	gate    chan struct{}
}

func (f *fakeFinder) DoCommand(_ context.Context, cmd map[string]any) (map[string]any, error) {
	kind := cmd["find"].(map[string]any)["kind"].(string)
	if f.looking != nil {
		f.looking <- kind
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, kind)
	if f.err != nil {
		return nil, f.err
	}
	return map[string]any{"sheets": f.sheets[kind]}, nil
}

func (f *fakeFinder) set(kind string, sheets ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sheets[kind] = sheets
}

func (f *fakeFinder) callsFor(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == kind {
			n++
		}
	}
	return n
}

func sheetAt(x, y float64) map[string]any {
	return map[string]any{
		"center": map[string]any{"x": x, "y": y, "z": 280.0},
		"paper":  map[string]any{"top_left_corner": map[string]any{"x": x}, "width_mm": 101.6},
	}
}

func paperX(paper any) float64 {
	return paper.(map[string]any)["top_left_corner"].(map[string]any)["x"].(float64)
}

func withFinder(h *harness) *fakeFinder {
	f := &fakeFinder{sheets: map[string][]any{}}
	h.q.finder = f
	return f
}

func needsPaper(h *harness) []any {
	n, _ := h.q.status()["needs_paper"].([]any)
	return n
}

func (h *harness) waitNeedsPaper(t *testing.T, kind string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, n := range needsPaper(h) {
			if n.(map[string]any)["kind"] == kind {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the queue never asked for a %s", kind)
}

func TestAPortraitIsDrawnOnTheSheetTheFinderGives(t *testing.T) {
	h := newHarness(t)
	f := withFinder(h)
	f.set(sheetCard, sheetAt(120, 0), sheetAt(300, 0))
	h.q.start()
	id := h.enqueue(t, "enqueue_portrait", map[string]any{"image_b64": "x"})
	h.drawUntil(t, id, stateDone)

	h.drawer.mu.Lock()
	defer h.drawer.mu.Unlock()
	test.That(t, paperX(h.drawer.draws[0]["paper"]), test.ShouldEqual, 120.0)
}

func TestANameTagIsWrittenOnTheTagTheFinderGives(t *testing.T) {
	h := newHarness(t)
	f := withFinder(h)
	f.set(sheetNameTag, sheetAt(200, 50))
	h.q.start()
	id := h.enqueue(t, "enqueue_name_tag", map[string]any{"name": "Ada"})
	test.That(t, <-h.writer.wrote, test.ShouldEqual, "Ada")
	h.waitState(t, id, stateDone)

	h.writer.mu.Lock()
	defer h.writer.mu.Unlock()
	write := h.writer.cmds[0]["write"].(map[string]any)
	test.That(t, write["text"], test.ShouldEqual, "Ada")
	test.That(t, paperX(write["paper"]), test.ShouldEqual, 200.0)
	test.That(t, f.callsFor(sheetNameTag), test.ShouldEqual, 1)
}

func TestWithNoSheetTheJobWaitsForResume(t *testing.T) {
	h := newHarness(t)
	f := withFinder(h)
	h.q.start()
	id := h.enqueue(t, "enqueue_portrait", map[string]any{"image_b64": "x"})
	h.waitNeedsPaper(t, sheetCard)
	s, err := h.q.jobSummary(id)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, s["state"], test.ShouldEqual, stateQueued)

	// Being woken alone does not make it look again; only resume does.
	calls := f.callsFor(sheetCard)
	f.set(sheetCard, sheetAt(150, 0))
	h.q.nudge(h.q.wake)
	time.Sleep(50 * time.Millisecond)
	test.That(t, f.callsFor(sheetCard), test.ShouldEqual, calls)

	out, err := h.q.DoCommand(context.Background(), map[string]any{"resume": map[string]any{}})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, out["needs_paper"], test.ShouldBeNil)
	h.drawUntil(t, id, stateDone)
}

func TestAMissingTagDoesNotHoldUpAPortrait(t *testing.T) {
	h := newHarness(t)
	f := withFinder(h)
	f.set(sheetCard, sheetAt(150, 0))
	h.q.start()
	tag := h.enqueue(t, "enqueue_name_tag", map[string]any{"name": "Ada"})
	portrait := h.enqueue(t, "enqueue_portrait", map[string]any{"image_b64": "x"})
	h.drawUntil(t, portrait, stateDone)
	h.waitNeedsPaper(t, sheetNameTag)
	s, _ := h.q.jobSummary(tag)
	test.That(t, s["state"], test.ShouldEqual, stateQueued)
}

func TestAFinderErrorIsReportedAndWaits(t *testing.T) {
	h := newHarness(t)
	f := withFinder(h)
	f.err = errors.New("not calibrated; run calibrate first")
	h.q.start()
	h.enqueue(t, "enqueue_portrait", map[string]any{"image_b64": "x"})
	h.waitNeedsPaper(t, sheetCard)
	n := needsPaper(h)[0].(map[string]any)
	test.That(t, n["error"], test.ShouldContainSubstring, "not calibrated")
}

func TestAPausedPortraitKeepsItsSheetThroughANameTag(t *testing.T) {
	h := newHarness(t)
	f := withFinder(h)
	f.set(sheetCard, sheetAt(120, 0))
	f.set(sheetNameTag, sheetAt(250, 80))
	h.q.start()
	portrait := h.enqueue(t, "enqueue_portrait", map[string]any{"image_b64": "x"})
	h.waitDrawing(t)
	h.drawer.step(t)
	h.drawer.step(t)

	// The finder now sees a different card nearest; the portrait must not move to it.
	f.set(sheetCard, sheetAt(60, 0))
	h.enqueue(t, "enqueue_name_tag", map[string]any{"name": "Ada"})
	test.That(t, <-h.writer.wrote, test.ShouldEqual, "Ada")
	h.drawUntil(t, portrait, stateDone)

	h.drawer.mu.Lock()
	defer h.drawer.mu.Unlock()
	test.That(t, len(h.drawer.draws), test.ShouldEqual, 2)
	test.That(t, paperX(h.drawer.draws[1]["paper"]), test.ShouldEqual, 120.0)
	test.That(t, f.callsFor(sheetCard), test.ShouldEqual, 1)
}

func TestASheetAnUnfinishedPortraitOwnsIsNotHandedOut(t *testing.T) {
	h := newHarness(t)
	f := withFinder(h)
	// The failed portrait's card has too little on it to look used.
	h.q.jobs = []*Job{{
		ID: "old", Kind: kindPortrait, State: stateFailed, NextPolyline: 1, Polylines: [][][2]float64{{{0, 0}, {1, 1}}, {{2, 2}, {3, 3}}},
		Paper: sheetAt(120, 0)["paper"].(map[string]any), SheetAt: &[2]float64{120, 0}, CreatedAt: time.Now(),
	}}
	f.set(sheetCard, sheetAt(121, 2), sheetAt(300, 0))
	h.q.start()
	id := h.enqueue(t, "enqueue_portrait", map[string]any{"image_b64": "x"})
	h.drawUntil(t, id, stateDone)

	h.drawer.mu.Lock()
	defer h.drawer.mu.Unlock()
	test.That(t, paperX(h.drawer.draws[0]["paper"]), test.ShouldEqual, 300.0)
}

func TestARetriedCardGetsAFreshTag(t *testing.T) {
	h := newHarness(t)
	f := withFinder(h)
	f.set(sheetNameTag, sheetAt(200, 50))
	h.writer.block = true
	h.q.start()
	id := h.enqueue(t, "enqueue_name_tag", map[string]any{"name": "Ada"})
	<-h.writer.wrote
	_ = h.q.Close(context.Background())
	h.waitState(t, id, stateFailed)

	h.writer.mu.Lock()
	h.writer.block = false
	h.writer.mu.Unlock()
	f.set(sheetNameTag, sheetAt(260, 50))
	_, err := h.q.DoCommand(context.Background(), map[string]any{"retry_job": map[string]any{"job_id": id}})
	test.That(t, err, test.ShouldBeNil)
	h.q.start()
	test.That(t, <-h.writer.wrote, test.ShouldEqual, "Ada")
	h.waitState(t, id, stateDone)

	h.writer.mu.Lock()
	defer h.writer.mu.Unlock()
	last := h.writer.cmds[len(h.writer.cmds)-1]["write"].(map[string]any)
	test.That(t, paperX(last["paper"]), test.ShouldEqual, 260.0)
}

func TestConfigValidateAddsThePaperFinder(t *testing.T) {
	deps, _, err := (&Config{Drawer: "d", StrokeGenerator: "g", PaperFinder: "finder"}).Validate("")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, deps, test.ShouldContain, "finder")
}

func TestANameTagThatArrivesWhileThePortraitsCardIsFoundGoesFirst(t *testing.T) {
	h := newHarness(t)
	f := withFinder(h)
	f.set(sheetCard, sheetAt(120, 0))
	f.set(sheetNameTag, sheetAt(250, 80))
	f.looking, f.gate = make(chan string, 4), make(chan struct{})
	h.q.start()
	portrait := h.enqueue(t, "enqueue_portrait", map[string]any{"image_b64": "x"})
	test.That(t, <-f.looking, test.ShouldEqual, sheetCard)
	h.enqueue(t, "enqueue_name_tag", map[string]any{"name": "Ada"})
	close(f.gate)

	test.That(t, <-h.writer.wrote, test.ShouldEqual, "Ada")
	h.drawer.mu.Lock()
	started := len(h.drawer.draws)
	h.drawer.mu.Unlock()
	test.That(t, started, test.ShouldEqual, 0)
	h.drawUntil(t, portrait, stateDone)
	// The portrait kept the card found for it rather than looking again.
	test.That(t, f.callsFor(sheetCard), test.ShouldEqual, 1)
}

func TestOnceTheQueueKnowsThereAreNoTagsATagDoesNotPauseThePortrait(t *testing.T) {
	h := newHarness(t)
	f := withFinder(h)
	f.set(sheetCard, sheetAt(120, 0))
	h.q.start()
	h.enqueue(t, "enqueue_name_tag", map[string]any{"name": "Ada"})
	h.waitNeedsPaper(t, sheetNameTag)
	portrait := h.enqueue(t, "enqueue_portrait", map[string]any{"image_b64": "x"})
	h.waitDrawing(t)
	h.drawer.step(t)

	h.enqueue(t, "enqueue_name_tag", map[string]any{"name": "Grace"})
	h.drawUntil(t, portrait, stateDone)
	h.drawer.mu.Lock()
	defer h.drawer.mu.Unlock()
	test.That(t, len(h.drawer.draws), test.ShouldEqual, 1)
	test.That(t, h.drawer.calls, test.ShouldNotContain, "pause")
}

func (h *harness) do(t *testing.T, verb string) map[string]any {
	t.Helper()
	resp, err := h.q.DoCommand(context.Background(), map[string]any{verb: map[string]any{}})
	test.That(t, err, test.ShouldBeNil)
	return resp
}

func (h *harness) drawerCalled(verb string) bool {
	h.drawer.mu.Lock()
	defer h.drawer.mu.Unlock()
	return slices.Contains(h.drawer.calls, verb)
}

func expectNothingWritten(t *testing.T, w *fakeWriter) {
	t.Helper()
	select {
	case name := <-w.wrote:
		t.Fatalf("wrote %q while paused", name)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestPauseStopsAPortraitMidStrokeAndItResumesFromThatStroke(t *testing.T) {
	h := newHarness(t)
	h.q.start()
	id := h.enqueue(t, "enqueue_portrait", map[string]any{"image_b64": "x"})
	h.waitDrawing(t)
	h.drawer.step(t)
	h.drawer.step(t)

	resp := h.do(t, "pause")
	test.That(t, resp["paused"], test.ShouldNotBeNil)
	test.That(t, h.drawerCalled("cancel"), test.ShouldBeTrue)
	h.waitState(t, id, statePaused)

	h.do(t, "resume")
	h.drawUntil(t, id, stateDone)
	h.drawer.mu.Lock()
	defer h.drawer.mu.Unlock()
	test.That(t, h.drawer.starts, test.ShouldResemble, []int{0, 2})
}

func TestPauseStopsANameTagAndItGoesFirstOnResume(t *testing.T) {
	h := newHarness(t)
	h.writer.block = true
	h.q.start()
	ada := h.enqueue(t, "enqueue_name_tag", map[string]any{"name": "Ada"})
	test.That(t, <-h.writer.wrote, test.ShouldEqual, "Ada")
	h.enqueue(t, "enqueue_name_tag", map[string]any{"name": "Grace"})

	h.do(t, "pause")
	test.That(t, h.drawerCalled("cancel"), test.ShouldBeTrue)
	h.waitState(t, ada, stateQueued)
	s, err := h.q.jobSummary(ada)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, s["position"], test.ShouldEqual, 1)
	test.That(t, s["error"], test.ShouldBeNil)
	expectNothingWritten(t, h.writer)

	h.writer.mu.Lock()
	h.writer.block = false
	h.writer.mu.Unlock()
	h.do(t, "resume")
	test.That(t, <-h.writer.wrote, test.ShouldEqual, "Ada")
	test.That(t, <-h.writer.wrote, test.ShouldEqual, "Grace")
}

func TestPauseWhileIdleHoldsNewJobsUntilResume(t *testing.T) {
	h := newHarness(t)
	h.q.start()
	h.do(t, "pause")
	h.enqueue(t, "enqueue_name_tag", map[string]any{"name": "Ada"})
	expectNothingWritten(t, h.writer)

	resp := h.do(t, "resume")
	test.That(t, resp["paused"], test.ShouldBeNil)
	select {
	case name := <-h.writer.wrote:
		test.That(t, name, test.ShouldEqual, "Ada")
	case <-time.After(2 * time.Second):
		t.Fatal("resume did not start the waiting tag")
	}
}

func TestGoHomeOnlyWhilePaused(t *testing.T) {
	h := newHarness(t)
	h.q.start()
	_, err := h.q.DoCommand(context.Background(), map[string]any{"go_home": map[string]any{}})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "pause")
	test.That(t, h.drawerCalled("go_home"), test.ShouldBeFalse)

	h.do(t, "pause")
	h.do(t, "go_home")
	test.That(t, h.drawerCalled("go_home"), test.ShouldBeTrue)
}

func TestAPauseSurvivesARestart(t *testing.T) {
	h := newHarness(t)
	h.do(t, "pause")
	h.enqueue(t, "enqueue_name_tag", map[string]any{"name": "Ada"})

	writer := &fakeWriter{wrote: make(chan string, 1)}
	q := newWithClients(generic.Named("queue"), logging.NewTestLogger(t), newFakeDrawer(), &fakeGenerator{}, writer, &Config{}, h.state)
	test.That(t, q.load(), test.ShouldBeNil)
	q.start()
	defer func() { test.That(t, q.Close(context.Background()), test.ShouldBeNil) }()
	test.That(t, q.status()["paused"], test.ShouldNotBeNil)
	expectNothingWritten(t, writer)
}

type fakeFeeder struct {
	mu    sync.Mutex
	feeds [][2]float64
	err   error
}

func (f *fakeFeeder) GoFor(_ context.Context, rpm, revolutions float64, _ map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.feeds = append(f.feeds, [2]float64{rpm, revolutions})
	return f.err
}

func (f *fakeFeeder) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.feeds)
}

func withDispenser(h *harness) *fakeFeeder {
	f := &fakeFeeder{}
	h.q.dispenser = f
	h.q.dispenserCfg = CardDispenserConfig{Motor: "card-feeder:feed-motor", Revolutions: 2.5, RPM: 40}
	return f
}

func TestADispensedPortraitIsDrawnOnTheBoxThenFedOut(t *testing.T) {
	h := newHarness(t)
	finder := withFinder(h)
	feeder := withDispenser(h)
	h.q.start()
	id := h.enqueue(t, "enqueue_portrait", map[string]any{"image_b64": "x"})
	h.drawUntil(t, id, stateDone)

	deadline := time.Now().Add(2 * time.Second)
	for feeder.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	test.That(t, feeder.feeds, test.ShouldResemble, [][2]float64{{40, 2.5}})
	test.That(t, finder.callsFor(sheetCard), test.ShouldEqual, 0)
	h.drawer.mu.Lock()
	defer h.drawer.mu.Unlock()
	_, hasPaper := h.drawer.draws[0]["paper"]
	test.That(t, hasPaper, test.ShouldBeFalse)
}

func TestAFailedFeedHoldsPortraitsUntilResume(t *testing.T) {
	h := newHarness(t)
	feeder := withDispenser(h)
	feeder.err = errors.New("stalled")
	h.q.start()
	first := h.enqueue(t, "enqueue_portrait", map[string]any{"image_b64": "x"})
	h.drawUntil(t, first, stateDone)
	h.waitNeedsPaper(t, sheetCard)
	test.That(t, needsPaper(h)[0].(map[string]any)["error"], test.ShouldContainSubstring, "stalled")

	second := h.enqueue(t, "enqueue_portrait", map[string]any{"image_b64": "x"})
	time.Sleep(50 * time.Millisecond)
	s, err := h.q.jobSummary(second)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, s["state"], test.ShouldEqual, stateQueued)

	feeder.mu.Lock()
	feeder.err = nil
	feeder.mu.Unlock()
	_, err = h.q.DoCommand(context.Background(), map[string]any{"resume": map[string]any{}})
	test.That(t, err, test.ShouldBeNil)
	h.drawUntil(t, second, stateDone)
}

func TestCardDispenserConfigIsChecked(t *testing.T) {
	base := func() *Config { return &Config{Drawer: "d", StrokeGenerator: "g"} }
	cfg := base()
	cfg.CardDispenser = &CardDispenserConfig{Motor: "card-feeder:feed-motor", Revolutions: 2}
	deps, _, err := cfg.Validate("")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, deps, test.ShouldContain, "card-feeder:feed-motor")

	cfg.CardDispenser = &CardDispenserConfig{Motor: "m"}
	_, _, err = cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)
	cfg.CardDispenser = &CardDispenserConfig{Revolutions: 2}
	_, _, err = cfg.Validate("")
	test.That(t, err, test.ShouldNotBeNil)

	// Switched off, the block is kept but neither checked nor depended on.
	off := false
	cfg.CardDispenser = &CardDispenserConfig{Enabled: &off, Motor: "card-feeder:feed-motor", Revolutions: 2}
	deps, _, err = cfg.Validate("")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, deps, test.ShouldNotContain, "card-feeder:feed-motor")
}
