// Package queue implements a Viam generic service that runs a reception desk's
// drawing jobs on one arm: name tags and text cards first, portraits when no
// card is waiting.
package queue

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
	kindText     = "text"

	stateQueued   = "queued"
	stateActive   = "active"
	statePaused   = "paused"
	stateDone     = "done"
	stateFailed   = "failed"
	stateCanceled = "canceled"

	defaultPaperWidthMM  = 101.6
	defaultPaperHeightMM = 152.4
	defaultMarginMM      = 8.0

	// interruptedCard is the error an interrupted card is failed with: text
	// cannot resume mid-letter, and writing it again would go over the partly
	// written card.
	interruptedCard = "interrupted while writing; the card may be partly written, so replace it before retry_job"

	// progressPoll is how often an active portrait's progress is saved, so a
	// restart resumes close to where the pen stopped.
	progressPoll = 2 * time.Second
	// keepFinished bounds how many finished jobs stay in the saved state.
	keepFinished = 50
	// paperSwapWait gives the operator time to swap in a fresh sheet before
	// another job is drawn on the same surface.
	paperSwapWait = 8 * time.Second
	// heldSheetMM is how close a found sheet's centre may be to one a paused or
	// failed portrait still owns before it counts as that same sheet.
	heldSheetMM = 40.0

	sheetCard    = "card"
	sheetNameTag = "name_tag"
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
	// NameTagWriter writes name tags and text jobs. Optional: without it, both
	// are refused at enqueue.
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
	// PaperFinder, when set, picks a blank sheet off the mat for each job and
	// sends it with the draw or write, instead of the drawer's and writer's own
	// fixed surfaces. Without a sheet the job waits for resume.
	PaperFinder string `json:"paper_finder,omitempty"`
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
	if cfg.PaperFinder != "" {
		deps = append(deps, cfg.PaperFinder)
	}
	return deps, nil, nil
}

type commander interface {
	DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error)
}

type paperWait struct {
	since time.Time
	err   string
}

// Job is one name tag, text line, or portrait. Portraits carry their strokes, never the photo.
type Job struct {
	ID      string                 `json:"id"`
	Kind    string                 `json:"kind"`
	Name    string                 `json:"name,omitempty"`
	Visitor map[string]interface{} `json:"visitor,omitempty"`
	// Write is a text job's payload for the writer's write verb.
	Write        map[string]interface{} `json:"write,omitempty"`
	State        string                 `json:"state"`
	Polylines    [][][2]float64         `json:"polylines,omitempty"`
	Total        int                    `json:"polylines_total,omitempty"`
	NextPolyline int                    `json:"next_polyline,omitempty"`
	Error        string                 `json:"error,omitempty"`
	// Paper is the sheet the paper finder gave this job, kept so a paused
	// portrait resumes on the same one. SheetAt is that sheet's centre.
	Paper      map[string]interface{} `json:"paper,omitempty"`
	SheetAt    *[2]float64            `json:"sheet_at,omitempty"`
	CreatedAt  time.Time              `json:"created_at"`
	StartedAt  time.Time              `json:"started_at,omitempty"`
	FinishedAt time.Time              `json:"finished_at,omitempty"`
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
	paperSwapWait  time.Duration
	finder         commander

	mu   sync.Mutex
	jobs []*Job
	// needsPaper is, per sheet kind, why the last job needing one found none.
	// Jobs needing that kind wait until resume clears it.
	needsPaper map[string]*paperWait
	// wake nudges the worker when a job is queued; preempt asks it to pause the
	// portrait in progress because a name tag is waiting.
	wake    chan struct{}
	preempt chan struct{}

	// cancelCard stops the card being written, so cancel_job can stop it.
	cancelCard context.CancelFunc

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
	if cfg.PaperFinder != "" {
		if q.finder, err = generic.FromProvider(deps, cfg.PaperFinder); err != nil {
			return nil, fmt.Errorf("reception-queue: get paper_finder %q: %w", cfg.PaperFinder, err)
		}
		// A used sheet is never picked again, so there is nothing to wait out.
		q.paperSwapWait = 0
	}
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
		paperSwapWait:  paperSwapWait,
		needsPaper:     map[string]*paperWait{},
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
	case "enqueue_text":
		return q.enqueueText(args)
	case "status":
		return q.status(), nil
	case "job":
		return q.jobSummary(str(args, "job_id"))
	case "cancel_job":
		return q.cancelJob(ctx, str(args, "job_id"))
	case "retry_job":
		return q.retryJob(str(args, "job_id"))
	case "resume":
		return q.resume(), nil
	default:
		return nil, fmt.Errorf("reception-queue: unknown verb %q; expected \"enqueue_portrait\", \"enqueue_name_tag\", "+
			"\"enqueue_text\", \"status\", \"job\", \"cancel_job\", \"retry_job\", or \"resume\"", v)
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
		State: stateQueued, Polylines: polylines, Total: len(polylines), CreatedAt: time.Now(),
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
	return q.addCard(job)
}

// enqueueText queues a line of text for the writer. Only the write verb's
// sizing options pass through, so a caller cannot turn the job into a preview.
func (q *receptionQueue) enqueueText(args map[string]interface{}) (map[string]interface{}, error) {
	if q.writer == nil {
		return nil, errors.New("reception-queue: no name_tag_writer is configured")
	}
	text := strings.TrimSpace(str(args, "text"))
	if text == "" {
		return nil, errors.New("reception-queue: enqueue_text needs text")
	}
	write := map[string]interface{}{"text": text}
	for _, k := range []string{"cap_height_mm", "fill"} {
		v, ok := args[k]
		if !ok {
			continue
		}
		if n, isNum := v.(float64); !isNum || n < 0 {
			return nil, fmt.Errorf("reception-queue: enqueue_text %s must be a number >= 0, got %v", k, v)
		}
		write[k] = v
	}
	job := &Job{
		ID: newID(), Kind: kindText, Name: str(args, "name"), Visitor: visitorOf(args), Write: write,
		State: stateQueued, CreatedAt: time.Now(),
	}
	return q.addCard(job)
}

// addCard queues a writer job and asks a drawing portrait to make way for it.
func (q *receptionQueue) addCard(job *Job) (map[string]interface{}, error) {
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
	cancelCard := q.cancelCard
	q.mu.Unlock()
	if wasActive && isCard(job) && cancelCard != nil {
		cancelCard()
	}
	if wasActive {
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
	} else {
		// Only a portrait with strokes on its sheet goes back to it; a card that
		// failed may be part-written, so it gets a fresh one.
		job.Paper, job.SheetAt = nil, nil
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
	if j.Kind == kindText {
		out["text"] = str(j.Write, "text")
	}
	if j.Kind == kindPortrait {
		out["polylines_total"] = j.Total
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
	out := map[string]interface{}{"active": active, "waiting": waiting, "finished": finished}
	if len(q.needsPaper) > 0 {
		needs := []interface{}{}
		for _, kind := range []string{sheetNameTag, sheetCard} {
			w := q.needsPaper[kind]
			if w == nil {
				continue
			}
			n := map[string]interface{}{"kind": kind, "since": w.since.UTC().Format(time.RFC3339)}
			if w.err != "" {
				n["error"] = w.err
			}
			needs = append(needs, n)
		}
		out["needs_paper"] = needs
	}
	return out
}

// resume is what a person sends after putting paper down: jobs waiting for a
// sheet look again.
func (q *receptionQueue) resume() map[string]interface{} {
	q.mu.Lock()
	q.needsPaper = map[string]*paperWait{}
	q.mu.Unlock()
	q.nudge(q.wake)
	// A tag that can now be written should not wait out a portrait.
	q.nudge(q.preempt)
	return q.status()
}

// sheetKind is what a job is drawn on: portraits on cards, the writer's jobs on name tags.
func sheetKind(j *Job) string {
	if isCard(j) {
		return sheetNameTag
	}
	return sheetCard
}

// heldSheetsLocked is where the sheets other jobs still own lie: an unfinished
// portrait's card may not yet look used, but it is not blank.
func (q *receptionQueue) heldSheetsLocked(except *Job) [][2]float64 {
	var out [][2]float64
	for _, j := range q.jobs {
		if j == except || j.SheetAt == nil {
			continue
		}
		if j.State == stateQueued || j.State == statePaused || j.State == stateActive ||
			(j.State == stateFailed && j.NextPolyline > 0) {
			out = append(out, *j.SheetAt)
		}
	}
	return out
}

// findSheet asks the paper finder for the nearest blank sheet of kind that no
// other job owns.
func (q *receptionQueue) findSheet(ctx context.Context, kind string, held [][2]float64) (map[string]interface{}, [2]float64, error) {
	resp, err := q.finder.DoCommand(ctx, map[string]interface{}{"find": map[string]interface{}{"kind": kind}})
	if err != nil {
		return nil, [2]float64{}, fmt.Errorf("find a %s: %w", kind, err)
	}
	sheets, _ := resp["sheets"].([]interface{})
	for _, raw := range sheets {
		sheet, _ := raw.(map[string]interface{})
		paper, _ := sheet["paper"].(map[string]interface{})
		center, _ := sheet["center"].(map[string]interface{})
		x, xok := center["x"].(float64)
		y, yok := center["y"].(float64)
		if paper == nil || !xok || !yok {
			continue
		}
		owned := false
		for _, h := range held {
			if math.Hypot(x-h[0], y-h[1]) < heldSheetMM {
				owned = true
				break
			}
		}
		if !owned {
			return paper, [2]float64{x, y}, nil
		}
	}
	return nil, [2]float64{}, nil
}

// withPaper adds the job's sheet to the writer command's payload.
func withPaper(cmd, paper map[string]interface{}) map[string]interface{} {
	if paper == nil {
		return cmd
	}
	out := make(map[string]interface{}, len(cmd))
	for v, payload := range cmd {
		args, ok := payload.(map[string]interface{})
		if !ok {
			out[v] = payload
			continue
		}
		withSheet := make(map[string]interface{}, len(args)+1)
		for k, val := range args {
			withSheet[k] = val
		}
		withSheet["paper"] = paper
		out[v] = withSheet
	}
	return out
}

// orderedLocked is the waiting jobs in the order they will run: card jobs
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
		case isCard(j):
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

// isCard reports whether the job goes to the writer rather than the drawer.
func isCard(j *Job) bool {
	return j.Kind == kindNameTag || j.Kind == kindText
}

// cardWaitingLocked reports whether a card job could run now. One waiting for
// a tag sheet cannot, so it must not make a portrait step aside.
func (q *receptionQueue) cardWaitingLocked() bool {
	for _, j := range q.jobs {
		if !isCard(j) || j.State != stateQueued {
			continue
		}
		if !q.needsSheetLocked(j) || q.needsPaper[sheetNameTag] == nil {
			return true
		}
	}
	return false
}

func (q *receptionQueue) run(ctx context.Context) {
	// spent is, per surface (card or portrait paper), the last job whose sheet
	// it left marked: done, failed, or canceled mid-draw. Only a paused portrait
	// keeps its sheet, and a retry of the spent job itself goes back onto it.
	type spentSheet struct {
		id string
		at time.Time
	}
	spent := map[bool]spentSheet{}
	for {
		q.mu.Lock()
		var next *Job
		var swapLeft time.Duration
		for _, j := range q.orderedLocked() {
			if q.needsSheetLocked(j) && q.needsPaper[sheetKind(j)] != nil {
				continue
			}
			next = j
			break
		}
		var held [][2]float64
		if next != nil {
			if last, ok := spent[isCard(next)]; ok && last.id != next.ID {
				swapLeft = time.Until(last.at.Add(q.paperSwapWait))
			}
			held = q.heldSheetsLocked(next)
		}
		needSheet := next != nil && swapLeft <= 0 && q.needsSheetLocked(next)
		q.mu.Unlock()

		if next == nil || swapLeft > 0 {
			var swapped <-chan time.Time
			if swapLeft > 0 {
				swapped = time.After(swapLeft)
			}
			select {
			case <-ctx.Done():
				return
			case <-q.wake:
			case <-swapped:
			}
			continue
		}

		var paper map[string]interface{}
		var at [2]float64
		var findErr error
		if needSheet {
			paper, at, findErr = q.findSheet(ctx, sheetKind(next), held)
			if ctx.Err() != nil {
				return
			}
		}
		q.mu.Lock()
		// It may have been canceled while the finder looked.
		if next.State != stateQueued && next.State != statePaused {
			q.mu.Unlock()
			continue
		}
		// A card that arrived while the finder looked goes first; the portrait
		// keeps the sheet it was given for when its turn comes.
		if !isCard(next) && q.cardWaitingLocked() {
			if paper != nil {
				next.Paper, next.SheetAt = paper, &at
			}
			q.mu.Unlock()
			continue
		}
		if needSheet {
			if paper == nil {
				w := &paperWait{since: time.Now()}
				if findErr != nil {
					w.err = findErr.Error()
				}
				q.needsPaper[sheetKind(next)] = w
				q.mu.Unlock()
				q.logger.Infof("reception-queue: no blank %s for %s %s; waiting for resume", sheetKind(next), next.Kind, next.ID)
				if findErr != nil {
					q.logger.Warnf("reception-queue: %v", findErr)
				}
				continue
			}
			next.Paper, next.SheetAt = paper, &at
		}
		next.State = stateActive
		if next.StartedAt.IsZero() {
			next.StartedAt = time.Now()
		}
		if err := q.saveLocked(); err != nil {
			q.logger.Warnf("reception-queue: %v", err)
		}
		q.mu.Unlock()

		if isCard(next) {
			q.runCard(ctx, next)
		} else {
			q.runPortrait(ctx, next)
		}
		if ctx.Err() != nil {
			return
		}
		q.mu.Lock()
		if next.State != statePaused {
			spent[isCard(next)] = spentSheet{id: next.ID, at: time.Now()}
		}
		q.mu.Unlock()
	}
}

// needsSheetLocked reports whether the job still has to be given a sheet.
func (q *receptionQueue) needsSheetLocked(j *Job) bool {
	return q.finder != nil && j.Paper == nil
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

func (q *receptionQueue) runCard(ctx context.Context, job *Job) {
	cmd := map[string]interface{}{"write": job.Write}
	if job.Kind == kindNameTag {
		cmd = fillName(q.nameTagCommand, job.Name).(map[string]interface{})
	}
	cmd = withPaper(cmd, job.Paper)
	q.logger.Infof("reception-queue: writing %s %s", job.Kind, job.ID)
	cardCtx, cancel := context.WithCancel(ctx)
	q.mu.Lock()
	q.cancelCard = cancel
	q.mu.Unlock()
	defer func() {
		q.mu.Lock()
		q.cancelCard = nil
		q.mu.Unlock()
		cancel()
	}()
	_, err := q.writer.DoCommand(cardCtx, cmd)
	switch {
	case cardCtx.Err() != nil:
		q.finish(job, stateFailed, errors.New(interruptedCard))
	case err != nil:
		q.logger.Warnf("reception-queue: %s %s failed: %v", job.Kind, job.ID, err)
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
	draw := map[string]interface{}{"polylines": polylinesArg(job.Polylines), "start_at": start}
	if job.Paper != nil {
		draw["paper"] = job.Paper
	}
	done := make(chan result, 1)
	go func() {
		resp, err := q.drawer.DoCommand(ctx, map[string]interface{}{"draw": draw})
		done <- result{resp, err}
	}()

	ticker := time.NewTicker(progressPoll)
	defer ticker.Stop()
	var paused chan struct{}
	for {
		select {
		case r := <-done:
			// A pause sent as the portrait finished on its own must land before
			// the next job starts, or it would stop that job instead.
			if paused != nil {
				<-paused
			}
			q.portraitReturned(ctx, job, r.resp, r.err)
			return
		case <-q.preempt:
			if paused != nil {
				continue
			}
			q.mu.Lock()
			waiting := q.cardWaitingLocked()
			q.mu.Unlock()
			if !waiting {
				continue
			}
			paused = make(chan struct{})
			q.logger.Infof("reception-queue: pausing portrait %s for a card", job.ID)
			go func() {
				defer close(paused)
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
		if j.Total == 0 {
			j.Total = len(j.Polylines)
		}
		// A portrait interrupted by a stop resumes; a card cannot, so it waits for a person.
		if j.State == stateActive {
			if j.Kind == kindPortrait {
				j.State = statePaused
			} else {
				j.State, j.Error, j.FinishedAt = stateFailed, interruptedCard, time.Now()
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
