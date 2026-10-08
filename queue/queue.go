// Package queue implements a Viam generic service that runs a reception desk's
// drawing jobs on one arm: name tags first, portraits when no tag is waiting.
package queue

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/generic"
	rutils "go.viam.com/rdk/utils"

	"github.com/viam-labs/portrait-drawing/internal/verb"
)

// Model is the reception-queue service.
var Model = resource.NewModel("viam", "portrait-drawing", "reception-queue")

func init() {
	resource.RegisterService(generic.API, Model,
		resource.Registration[resource.Resource, *Config]{
			Constructor: newQueue,
		},
	)
}

const (
	kindPortrait = "portrait"
	kindNameTag  = "name_tag"

	stateQueued   = "queued"
	stateActive   = "active"
	statePaused   = "paused"
	stateDone     = "done"
	stateFailed   = "failed"
	stateCanceled = "canceled"

	defaultPaperWidthMM  = 101.6
	defaultPaperHeightMM = 152.4
	defaultMarginMM      = 8.0

	// progressPoll is how often an active portrait's progress is saved, so a
	// restart resumes close to where the pen stopped.
	progressPoll = 2 * time.Second
	// keepFinished bounds how many finished jobs stay in the saved state.
	keepFinished = 50
)

// Config is the reception-queue service configuration.
type Config struct {
	// Drawer draws portraits; it must support draw with start_at, pause, and status.
	Drawer string `json:"drawer"`
	// StrokeGenerator turns a portrait photo into polylines when it is queued,
	// so the photo itself is never stored.
	StrokeGenerator string `json:"stroke_generator"`
	// Photo is a frame-buffer camera holding the visitor's photo. enqueue_portrait
	// without image_b64 reads it, then clears it.
	Photo string `json:"photo,omitempty"`
	// NameTagWriter writes name tags. Optional: without it, name tags are
	// refused at enqueue.
	NameTagWriter string `json:"name_tag_writer,omitempty"`
	// NameTagCommand is the DoCommand sent to NameTagWriter. Every string value
	// has "{name}" replaced with the visitor's name. It must block until the
	// tag is finished and the pen is lifted.
	NameTagCommand map[string]interface{} `json:"name_tag_command,omitempty"`
	// Paper geometry for portraits; it must match the drawer's.
	PaperWidthMM  float64 `json:"paper_width_mm,omitempty"`
	PaperHeightMM float64 `json:"paper_height_mm,omitempty"`
	MarginMM      float64 `json:"margin_mm,omitempty"`
	// StateFile is where jobs are saved. Defaults to a file named after the
	// service in $VIAM_MODULE_DATA.
	StateFile string `json:"state_file,omitempty"`
}

// Validate returns implicit dependencies and any config errors.
func (cfg *Config) Validate(path string) ([]string, []string, error) {
	if cfg.Drawer == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "drawer")
	}
	if cfg.StrokeGenerator == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "stroke_generator")
	}
	if cfg.NameTagCommand != nil && cfg.NameTagWriter == "" {
		return nil, nil, errors.New("name_tag_command is set but name_tag_writer is not")
	}
	for name, v := range map[string]float64{
		"paper_width_mm": cfg.PaperWidthMM, "paper_height_mm": cfg.PaperHeightMM, "margin_mm": cfg.MarginMM,
	} {
		if v < 0 {
			return nil, nil, fmt.Errorf("%s must be >= 0 (0 uses the default), got %g", name, v)
		}
	}
	deps := []string{cfg.Drawer, cfg.StrokeGenerator}
	if cfg.Photo != "" {
		deps = append(deps, cfg.Photo)
	}
	if cfg.NameTagWriter != "" {
		deps = append(deps, cfg.NameTagWriter)
	}
	return deps, nil, nil
}

type commander interface {
	DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error)
}

// Job is one name tag or portrait. Portraits carry their strokes, never the photo.
type Job struct {
	ID           string                 `json:"id"`
	Kind         string                 `json:"kind"`
	Name         string                 `json:"name,omitempty"`
	Visitor      map[string]interface{} `json:"visitor,omitempty"`
	State        string                 `json:"state"`
	Polylines    [][][2]float64         `json:"polylines,omitempty"`
	NextPolyline int                    `json:"next_polyline,omitempty"`
	Error        string                 `json:"error,omitempty"`
	CreatedAt    time.Time              `json:"created_at"`
	StartedAt    time.Time              `json:"started_at,omitempty"`
	FinishedAt   time.Time              `json:"finished_at,omitempty"`
}

type receptionQueue struct {
	resource.AlwaysRebuild

	name           resource.Name
	logger         logging.Logger
	drawer         commander
	generator      commander
	writer         commander
	photo          camera.Camera
	nameTagCommand map[string]interface{}
	paperW, paperH float64
	margin         float64
	stateFile      string

	mu   sync.Mutex
	jobs []*Job
	// wake nudges the worker when a job is queued; preempt asks it to pause the
	// portrait in progress because a name tag is waiting.
	wake    chan struct{}
	preempt chan struct{}

	cancel  context.CancelFunc
	workers sync.WaitGroup
}

func newQueue(ctx context.Context, deps resource.Dependencies, conf resource.Config, logger logging.Logger) (resource.Resource, error) {
	cfg, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return nil, err
	}
	drawer, err := generic.FromProvider(deps, cfg.Drawer)
	if err != nil {
		return nil, fmt.Errorf("reception-queue: get drawer %q: %w", cfg.Drawer, err)
	}
	gen, err := generic.FromProvider(deps, cfg.StrokeGenerator)
	if err != nil {
		return nil, fmt.Errorf("reception-queue: get stroke_generator %q: %w", cfg.StrokeGenerator, err)
	}
	var writer commander
	if cfg.NameTagWriter != "" {
		if writer, err = generic.FromProvider(deps, cfg.NameTagWriter); err != nil {
			return nil, fmt.Errorf("reception-queue: get name_tag_writer %q: %w", cfg.NameTagWriter, err)
		}
	}
	stateFile := cfg.StateFile
	if stateFile == "" {
		dir := os.Getenv("VIAM_MODULE_DATA")
		if dir == "" {
			dir = os.TempDir()
		}
		stateFile = filepath.Join(dir, "reception-queue-"+conf.ResourceName().Name+".json")
	}
	q := newWithClients(conf.ResourceName(), logger, drawer, gen, writer, cfg, stateFile)
	if cfg.Photo != "" {
		if q.photo, err = camera.FromProvider(deps, cfg.Photo); err != nil {
			return nil, fmt.Errorf("reception-queue: get photo %q: %w", cfg.Photo, err)
		}
	}
	if err := q.load(); err != nil {
		return nil, err
	}
	q.start()
	return q, nil
}

func newWithClients(name resource.Name, logger logging.Logger, drawer, gen, writer commander, cfg *Config, stateFile string) *receptionQueue {
	q := &receptionQueue{
		name: name, logger: logger, drawer: drawer, generator: gen, writer: writer,
		nameTagCommand: cfg.NameTagCommand,
		paperW:         orDefault(cfg.PaperWidthMM, defaultPaperWidthMM),
		paperH:         orDefault(cfg.PaperHeightMM, defaultPaperHeightMM),
		margin:         orDefault(cfg.MarginMM, defaultMarginMM),
		stateFile:      stateFile,
		wake:           make(chan struct{}, 1),
		preempt:        make(chan struct{}, 1),
	}
	if q.nameTagCommand == nil {
		q.nameTagCommand = map[string]interface{}{"write": map[string]interface{}{"text": "{name}"}}
	}
	return q
}

func orDefault(v, d float64) float64 {
	if v == 0 {
		return d
	}
	return v
}

func (q *receptionQueue) Name() resource.Name { return q.name }

// Status is the resource-level status the app shows; it is the same as the status verb.
func (q *receptionQueue) Status(context.Context) (map[string]interface{}, error) {
	return q.status(), nil
}

func (q *receptionQueue) start() {
	ctx, cancel := context.WithCancel(context.Background())
	q.cancel = cancel
	q.workers.Add(1)
	go func() {
		defer q.workers.Done()
		q.run(ctx)
	}()
	q.nudge(q.wake)
}

func (q *receptionQueue) Close(context.Context) error {
	if q.cancel != nil {
		q.cancel()
	}
	q.workers.Wait()
	return nil
}

func (q *receptionQueue) nudge(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (q *receptionQueue) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	v, err := verb.Single(cmd)
	if err != nil {
		return nil, err
	}
	args, _ := cmd[v].(map[string]interface{})
	switch v {
	case "enqueue_portrait":
		return q.enqueuePortrait(ctx, args)
	case "enqueue_name_tag":
		return q.enqueueNameTag(args)
	case "status":
		return q.status(), nil
	case "job":
		return q.jobSummary(str(args, "job_id"))
	case "cancel_job":
		return q.cancelJob(ctx, str(args, "job_id"))
	case "retry_job":
		return q.retryJob(str(args, "job_id"))
	default:
		return nil, fmt.Errorf("reception-queue: unknown verb %q; expected \"enqueue_portrait\", \"enqueue_name_tag\", "+
			"\"status\", \"job\", \"cancel_job\", or \"retry_job\"", v)
	}
}

func str(m map[string]interface{}, key string) string {
	s, _ := m[key].(string)
	return s
}

func visitorOf(args map[string]interface{}) map[string]interface{} {
	v, _ := args["visitor"].(map[string]interface{})
	return v
}

func (q *receptionQueue) enqueuePortrait(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
	image := str(args, "image_b64")
	if image == "" {
		if q.photo == nil {
			return nil, errors.New("reception-queue: enqueue_portrait needs image_b64, or a photo camera configured")
		}
		defer q.clearPhoto()
		var err error
		if image, err = q.readPhoto(ctx); err != nil {
			return nil, err
		}
	}
	resp, err := q.generator.DoCommand(ctx, map[string]interface{}{"generate": map[string]interface{}{
		"image_b64":       image,
		"paper_width_mm":  q.paperW,
		"paper_height_mm": q.paperH,
		"margin_mm":       q.margin,
	}})
	// The photo goes no further than the stroke generator: only strokes are kept.
	if err != nil {
		return nil, fmt.Errorf("reception-queue: generate strokes: %w", err)
	}
	polylines, err := parsePolylines(resp["polylines"])
	if err != nil {
		return nil, fmt.Errorf("reception-queue: stroke generator response: %w", err)
	}
	job := &Job{
		ID: newID(), Kind: kindPortrait, Name: str(args, "name"), Visitor: visitorOf(args),
		State: stateQueued, Polylines: polylines, CreatedAt: time.Now(),
	}
	return q.add(job)
}

func (q *receptionQueue) readPhoto(ctx context.Context) (string, error) {
	images, _, err := q.photo.Images(ctx, nil, nil)
	if err != nil {
		return "", fmt.Errorf("reception-queue: read photo: %w", err)
	}
	for i := range images {
		if images[i].MimeType() == rutils.MimeTypeRawDepth {
			continue
		}
		raw, err := images[i].Bytes(ctx)
		if err != nil {
			return "", fmt.Errorf("reception-queue: read photo bytes: %w", err)
		}
		return base64.StdEncoding.EncodeToString(raw), nil
	}
	return "", errors.New("reception-queue: the photo camera holds no picture; capture one first")
}

// clearPhoto runs even when stroke generation fails, so a rejected photo is not left behind either.
func (q *receptionQueue) clearPhoto() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := q.photo.DoCommand(ctx, map[string]interface{}{"clear": map[string]interface{}{}}); err != nil {
		q.logger.Errorf("reception-queue: clear photo; the visitor's photo is still on the camera: %v", err)
	}
}

func (q *receptionQueue) enqueueNameTag(args map[string]interface{}) (map[string]interface{}, error) {
	if q.writer == nil {
		return nil, errors.New("reception-queue: no name_tag_writer is configured")
	}
	name := strings.TrimSpace(str(args, "name"))
	if name == "" {
		return nil, errors.New("reception-queue: enqueue_name_tag needs name")
	}
	job := &Job{ID: newID(), Kind: kindNameTag, Name: name, Visitor: visitorOf(args), State: stateQueued, CreatedAt: time.Now()}
	out, err := q.add(job)
	if err == nil {
		q.nudge(q.preempt)
	}
	return out, err
}

func (q *receptionQueue) add(job *Job) (map[string]interface{}, error) {
	q.mu.Lock()
	q.jobs = append(q.jobs, job)
	err := q.saveLocked()
	pos := q.positionLocked(job.ID)
	q.mu.Unlock()
	if err != nil {
		return nil, err
	}
	q.nudge(q.wake)
	q.logger.Infof("reception-queue: queued %s %s (position %d)", job.Kind, job.ID, pos)
	return map[string]interface{}{"job_id": job.ID, "position": pos}, nil
}

func (q *receptionQueue) cancelJob(ctx context.Context, id string) (map[string]interface{}, error) {
	q.mu.Lock()
	job := q.findLocked(id)
	if job == nil {
		q.mu.Unlock()
		return nil, fmt.Errorf("reception-queue: no job %q", id)
	}
	wasActive := job.State == stateActive
	switch job.State {
	case stateDone, stateFailed, stateCanceled:
		q.mu.Unlock()
		return nil, fmt.Errorf("reception-queue: job %q is already %s", id, job.State)
	}
	job.State, job.FinishedAt = stateCanceled, time.Now()
	err := q.saveLocked()
	q.mu.Unlock()
	if wasActive && job.Kind == kindPortrait {
		if _, cerr := q.drawer.DoCommand(ctx, map[string]interface{}{"cancel": map[string]interface{}{}}); cerr != nil {
			q.logger.Warnf("reception-queue: cancel drawer: %v", cerr)
		}
	}
	if err != nil {
		return nil, err
	}
	return q.jobSummary(id)
}

func (q *receptionQueue) retryJob(id string) (map[string]interface{}, error) {
	q.mu.Lock()
	job := q.findLocked(id)
	if job == nil || job.State != stateFailed {
		q.mu.Unlock()
		return nil, fmt.Errorf("reception-queue: no failed job %q to retry", id)
	}
	job.State, job.Error = stateQueued, ""
	if job.NextPolyline > 0 {
		job.State = statePaused
	}
	err := q.saveLocked()
	q.mu.Unlock()
	if err != nil {
		return nil, err
	}
	q.nudge(q.wake)
	return q.jobSummary(id)
}

func summary(j *Job) map[string]interface{} {
	out := map[string]interface{}{"job_id": j.ID, "kind": j.Kind, "state": j.State, "created_at": j.CreatedAt.UTC().Format(time.RFC3339)}
	if j.Name != "" {
		out["name"] = j.Name
	}
	if j.Visitor != nil {
		out["visitor"] = j.Visitor
	}
	if j.Kind == kindPortrait {
		out["polylines_total"] = len(j.Polylines)
		out["polylines_done"] = j.NextPolyline
	}
	if j.Error != "" {
		out["error"] = j.Error
	}
	if !j.FinishedAt.IsZero() {
		out["finished_at"] = j.FinishedAt.UTC().Format(time.RFC3339)
	}
	return out
}

func (q *receptionQueue) jobSummary(id string) (map[string]interface{}, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	job := q.findLocked(id)
	if job == nil {
		return nil, fmt.Errorf("reception-queue: no job %q", id)
	}
	out := summary(job)
	if pos := q.positionLocked(id); pos > 0 {
		out["position"] = pos
	}
	return out, nil
}

func (q *receptionQueue) status() map[string]interface{} {
	q.mu.Lock()
	defer q.mu.Unlock()
	var active interface{}
	waiting := []interface{}{}
	finished := []interface{}{}
	for _, j := range q.orderedLocked() {
		waiting = append(waiting, summary(j))
	}
	for _, j := range q.jobs {
		switch j.State {
		case stateActive:
			active = summary(j)
		case stateDone, stateFailed, stateCanceled:
			finished = append(finished, summary(j))
		}
	}
	return map[string]interface{}{"active": active, "waiting": waiting, "finished": finished}
}

// orderedLocked is the waiting jobs in the order they will run: name tags
// first, then a paused portrait before any new one, each oldest first.
func (q *receptionQueue) orderedLocked() []*Job {
	var out []*Job
	for _, j := range q.jobs {
		if j.State == stateQueued || j.State == statePaused {
			out = append(out, j)
		}
	}
	rank := func(j *Job) int {
		switch {
		case j.Kind == kindNameTag:
			return 0
		case j.State == statePaused:
			return 1
		default:
			return 2
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return rank(out[a]) < rank(out[b]) })
	return out
}

func (q *receptionQueue) positionLocked(id string) int {
	for i, j := range q.orderedLocked() {
		if j.ID == id {
			return i + 1
		}
	}
	return 0
}

func (q *receptionQueue) findLocked(id string) *Job {
	for _, j := range q.jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

func (q *receptionQueue) nameTagWaitingLocked() bool {
	for _, j := range q.jobs {
		if j.Kind == kindNameTag && j.State == stateQueued {
			return true
		}
	}
	return false
}

func (q *receptionQueue) run(ctx context.Context) {
	for {
		q.mu.Lock()
		var next *Job
		if ordered := q.orderedLocked(); len(ordered) > 0 {
			next = ordered[0]
			next.State = stateActive
			if next.StartedAt.IsZero() {
				next.StartedAt = time.Now()
			}
			if err := q.saveLocked(); err != nil {
				q.logger.Warnf("reception-queue: %v", err)
			}
		}
		q.mu.Unlock()

		if next == nil {
			select {
			case <-ctx.Done():
				return
			case <-q.wake:
				continue
			}
		}
		if next.Kind == kindNameTag {
			q.runNameTag(ctx, next)
		} else {
			q.runPortrait(ctx, next)
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func (q *receptionQueue) finish(job *Job, state string, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if job.State == stateCanceled {
		return
	}
	job.State = state
	if err != nil {
		job.Error = err.Error()
	}
	if state == stateDone || state == stateFailed {
		job.FinishedAt = time.Now()
	}
	q.pruneLocked()
	if serr := q.saveLocked(); serr != nil {
		q.logger.Warnf("reception-queue: %v", serr)
	}
}

func (q *receptionQueue) runNameTag(ctx context.Context, job *Job) {
	q.logger.Infof("reception-queue: writing name tag %s", job.ID)
	_, err := q.writer.DoCommand(ctx, fillName(q.nameTagCommand, job.Name).(map[string]interface{}))
	switch {
	case ctx.Err() != nil:
		q.finish(job, stateQueued, nil)
	case err != nil:
		q.logger.Warnf("reception-queue: name tag %s failed: %v", job.ID, err)
		q.finish(job, stateFailed, err)
	default:
		q.finish(job, stateDone, nil)
	}
}

func (q *receptionQueue) runPortrait(ctx context.Context, job *Job) {
	q.mu.Lock()
	start, total := job.NextPolyline, len(job.Polylines)
	q.mu.Unlock()
	q.logger.Infof("reception-queue: drawing portrait %s from polyline %d of %d", job.ID, start, total)

	// The tag may have arrived before this portrait started; let the loop take it.
	select {
	case <-q.preempt:
	default:
	}

	type result struct {
		resp map[string]interface{}
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := q.drawer.DoCommand(ctx, map[string]interface{}{"draw": map[string]interface{}{
			"polylines": polylinesArg(job.Polylines), "start_at": start,
		}})
		done <- result{resp, err}
	}()

	ticker := time.NewTicker(progressPoll)
	defer ticker.Stop()
	pausing := false
	for {
		select {
		case r := <-done:
			q.portraitReturned(ctx, job, r.resp, r.err)
			return
		case <-q.preempt:
			if pausing {
				continue
			}
			q.mu.Lock()
			waiting := q.nameTagWaitingLocked()
			q.mu.Unlock()
			if !waiting {
				continue
			}
			pausing = true
			q.logger.Infof("reception-queue: pausing portrait %s for a name tag", job.ID)
			go func() {
				if _, err := q.drawer.DoCommand(ctx, map[string]interface{}{"pause": map[string]interface{}{}}); err != nil {
					q.logger.Warnf("reception-queue: pause drawer: %v", err)
				}
			}()
		case <-ticker.C:
			q.recordProgress(ctx, job)
		}
	}
}

func (q *receptionQueue) recordProgress(ctx context.Context, job *Job) {
	st, err := q.drawer.DoCommand(ctx, map[string]interface{}{"status": map[string]interface{}{}})
	if err != nil {
		return
	}
	done, ok := toInt(st["polylines_done"])
	if !ok {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if done > job.NextPolyline && done <= len(job.Polylines) {
		job.NextPolyline = done
		if serr := q.saveLocked(); serr != nil {
			q.logger.Warnf("reception-queue: %v", serr)
		}
	}
}

func (q *receptionQueue) portraitReturned(ctx context.Context, job *Job, resp map[string]interface{}, err error) {
	if next, ok := toInt(resp["next_polyline"]); ok {
		q.mu.Lock()
		job.NextPolyline = next
		q.mu.Unlock()
	}
	switch {
	case ctx.Err() != nil:
		// Shutting down: keep it resumable for the next start.
		q.recordProgress(context.Background(), job)
		q.finish(job, statePaused, nil)
	case err != nil:
		q.recordProgress(context.Background(), job)
		q.logger.Warnf("reception-queue: portrait %s failed: %v", job.ID, err)
		q.finish(job, stateFailed, err)
	case resp["paused"] == true:
		q.finish(job, statePaused, nil)
	default:
		q.mu.Lock()
		job.NextPolyline = len(job.Polylines)
		q.mu.Unlock()
		q.logger.Infof("reception-queue: portrait %s done", job.ID)
		q.finish(job, stateDone, nil)
	}
}

func (q *receptionQueue) load() error {
	raw, err := os.ReadFile(q.stateFile) //nolint:gosec // the path is the operator's own state_file config
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reception-queue: read %s: %w", q.stateFile, err)
	}
	var jobs []*Job
	if err := json.Unmarshal(raw, &jobs); err != nil {
		return fmt.Errorf("reception-queue: parse %s: %w", q.stateFile, err)
	}
	for _, j := range jobs {
		// Whatever was running when the module stopped resumes rather than restarts.
		if j.State == stateActive {
			if j.Kind == kindPortrait {
				j.State = statePaused
			} else {
				j.State = stateQueued
			}
		}
	}
	q.jobs = jobs
	if n := len(jobs); n > 0 {
		q.logger.Infof("reception-queue: loaded %d jobs from %s", n, q.stateFile)
	}
	return nil
}

func (q *receptionQueue) saveLocked() error {
	raw, err := json.Marshal(q.jobs)
	if err != nil {
		return fmt.Errorf("reception-queue: encode state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(q.stateFile), 0o750); err != nil { //nolint:gosec // operator-configured state_file
		return fmt.Errorf("reception-queue: create state dir: %w", err)
	}
	tmp := q.stateFile + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil { //nolint:gosec // operator-configured state_file
		return fmt.Errorf("reception-queue: write state: %w", err)
	}
	if err := os.Rename(tmp, q.stateFile); err != nil { //nolint:gosec // operator-configured state_file
		return fmt.Errorf("reception-queue: save state: %w", err)
	}
	return nil
}

func (q *receptionQueue) pruneLocked() {
	var finished []int
	for i, j := range q.jobs {
		if j.State == stateDone || j.State == stateFailed || j.State == stateCanceled {
			finished = append(finished, i)
		}
	}
	if extra := len(finished) - keepFinished; extra > 0 {
		drop := map[int]bool{}
		for _, i := range finished[:extra] {
			drop[i] = true
		}
		kept := q.jobs[:0]
		for i, j := range q.jobs {
			if !drop[i] {
				kept = append(kept, j)
			}
		}
		q.jobs = kept
	}
	// Strokes are only needed until a portrait is drawn.
	for _, j := range q.jobs {
		if j.State == stateDone || j.State == stateCanceled {
			j.Polylines = nil
		}
	}
}

func fillName(v interface{}, name string) interface{} {
	switch t := v.(type) {
	case string:
		return strings.ReplaceAll(t, "{name}", name)
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, val := range t {
			out[k] = fillName(val, name)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, val := range t {
			out[i] = fillName(val, name)
		}
		return out
	default:
		return v
	}
}

func parsePolylines(v interface{}) ([][][2]float64, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out [][][2]float64
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("polylines: %w", err)
	}
	if len(out) == 0 {
		return nil, errors.New("no polylines")
	}
	return out, nil
}

// polylinesArg converts strokes to the nested []interface{} that DoCommand's
// protobuf Struct encoding accepts; fixed-size arrays are rejected.
func polylinesArg(polylines [][][2]float64) []interface{} {
	out := make([]interface{}, len(polylines))
	for i, poly := range polylines {
		pts := make([]interface{}, len(poly))
		for j, p := range poly {
			pts[j] = []interface{}{p[0], p[1]}
		}
		out[i] = pts
	}
	return out
}

func toInt(v interface{}) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
