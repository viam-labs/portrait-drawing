# portrait-drawing

A Viam module that turns a photo of a person into a line drawing and traces it
on paper with a robotic arm.

Two services work together:

- **`viam:portrait-drawing:stroke-generator`** — image in, ordered 2D polylines
  out, in paper-local millimetres.
- **`viam:portrait-drawing:drawer`** — takes polylines and draws them, lifting
  the pen between strokes.

The drawer can drive the whole loop itself: pose the arm so its camera frames
the subject, take the photo, generate the strokes, and draw the result — one
DoCommand, no image data passing through your hands.

## `viam:portrait-drawing:drawer`

### Configuration

```json
{
  "arm": "arm-1",
  "paper_top_left_corner": {
    "translation": { "x": 179, "y": -196, "z": 266 },
    "orientation": { "type": "ov_degrees", "value": { "x": -0.073, "y": 0.009, "z": -0.997, "th": 172.16 } }
  },
  "paper_width_mm": 215.9,
  "paper_height_mm": 279.4,
  "capture_pose": "camera-framing",
  "stroke_generator": "stroke-generator",
  "photo": "photo",
  "preview_camera": "line-preview"
}
```

| Attribute | Type | Required | Description |
|---|---|---|---|
| `arm` | string | **yes** | The arm that holds the pen. |
| `paper_top_left_corner` | pose | **yes** | The tool pose where the pen tip touches the paper's top-left corner. Its orientation is reused for every waypoint, so the pen keeps one attitude across the drawing. |
| `paper_width_mm` | number | **yes** | Paper width. |
| `paper_height_mm` | number | **yes** | Paper height. |
| `lift_off_z_mm` | number | no | How far the pen lifts between strokes. Defaults to 5. |
| `capture_pose` | string | no | An [arm-position-saver](https://github.com/erh/vmodutils) switch holding the pose the arm rests at — the same pose `capture_and_draw` shoots from. Preferred over `home_pose`. |
| `home_pose` | pose | no | The same idea as `capture_pose`, written as literal tool coordinates. Set one or the other, not both. |
| `stroke_generator` | string | no | A `stroke-generator` service. Required for `draw_image` and `capture_and_draw`. |
| `photo` | string | no | A camera the drawer triggers and reads in `capture_and_draw` — a [frame-buffer](https://github.com/viam-labs/frame-buffer) camera pointed at your real camera. |
| `preview_camera` | string | no | A frame-buffer camera the drawer pushes rendered previews into, so previews are viewable in the Viam app instead of coming back as base64. |
| `allowed_collisions` | array | no | Frame pairs the planner should not treat as a collision. See [Anything bolted to the arm](#anything-bolted-to-the-arm). |
| `input_range_override` | object | no | Per-joint limits tighter than the arm model declares, to keep planned motions inside a safe envelope. Limits can only be tightened, never loosened. |

### Anything bolted to the arm

A wrist-mounted camera, a pen holder, a bracket — anything rigidly attached to a
link overlaps that link's geometry in *every* configuration. The planner cannot
tell that apart from a real collision, so it rejects every IK solution:

```
all IK solutions failed constraints. Failures: { self-collision constraint:
violation between arm-1:wrist_link and camera-1_origin geometries: 100.00% }
```

The `100.00%` is the giveaway: a genuine collision is pose-dependent, so it fails
some fraction of solutions. Failing all of them means the two shapes always
overlap.

Declare the pair as allowed rather than deleting the attachment's geometry —
the geometry is what stops a wrist camera from being driven into the table:

```json
"allowed_collisions": [
  { "frame1": "arm-1:wrist_link", "frame2": "camera-1_origin" }
]
```

Use the frame names exactly as the planner prints them in that error. Note that a
component's geometry hangs off its `_origin` frame, not its bare name.

Allowances apply to every move the drawer plans, travel and drawing alike.

### The rest pose

The arm has one pose it lives at between drawings: it starts there, it shoots
from there, and it returns there when a drawing finishes. Framing a subject and
parking are the same job, so they are the same pose.

Set it with `capture_pose`, naming an
[arm-position-saver](https://github.com/erh/vmodutils) switch:

```json
{
  "name": "camera-framing",
  "api": "rdk:component:switch",
  "model": "erh:vmodutils:arm-position-saver",
  "attributes": { "arm": "arm-1" }
}
```

Teach it by jogging the arm until the camera frames where a subject will sit,
then setting the switch to **"update config"** — it writes the joint angles into
its own config. The drawer moves there by setting the switch to **"go to"**.

Leave the switch's `motion` attribute unset so it saves and replays joint
angles: joint-space is exactly repeatable, and there is nothing to plan around
on the way back to a pose the arm just came from.

`home_pose` does the same job with literal tool coordinates, and still works.
Setting both is a config error — pick the one you want to be the source of
truth.

With neither set, `capture_and_draw` shoots from wherever the arm happens to be
and logs a warning, and `go_home` returns an error.

### `capture_and_draw`

The whole loop in one command. The arm moves to the rest pose, the `photo`
camera is triggered (it owns the countdown), and the resulting photo goes
through the stroke generator and onto the paper.

```json
{"capture_and_draw": {}}
```

All the stroke options below are accepted here too. Set `preview` first — the
arm still moves to the capture pose and takes the photo, but stops there:

```json
{"capture_and_draw": {"preview": true}}
```

| Option | Type | Description |
|---|---|---|
| `recapture` | bool | Take a new photo. Defaults to true. Set false to use the frame the `photo` camera already holds. |

**`recapture: false` is what makes tuning practical.** A default
`capture_and_draw` takes a fresh photo every time, so two previews with
different stroke settings are comparing two different photographs. Shoot once,
then iterate against that one frame:

```json
{"capture": {}}
```
```json
{"capture_and_draw": {"recapture": false, "preview": true}}
```

It also fixes self-portraits from the app: triggering `capture_and_draw` by hand
puts you at the keyboard three seconds later, not in front of the camera. Take
the photo when you are in frame, then draw it. Triggering by gesture does not
have this problem — you are already in frame when it fires.

With `recapture: false` the arm does not move to the capture pose first, since
there is nothing to frame.

### `draw_image`

Same thing, but you supply the photo instead of the arm taking it.

```json
{"draw_image": {"image_b64": "/9j/4AAQ…"}}
```

### Stroke options

Accepted by both `capture_and_draw` and `draw_image`:

| Option | Type | Description |
|---|---|---|
| `margin_mm` | number | Blank border around the drawing. Defaults to 40. |
| `rotate` | 0/90/180/270 | Rotation applied to the image. Ignored unless `auto_rotate` is false. |
| `auto_rotate` | bool | Picks between 0° and 90° to fill the paper best. Defaults to true. |
| `mirror` | bool | Mirror horizontally — useful when the camera faces the subject. |
| `preview` | bool | Render the strokes instead of drawing them. The arm never touches the paper. |
| `preview_px_per_mm` | number | Preview resolution. Defaults to 2, so US Letter renders at 432×558. |

### Previewing before you commit

`preview: true` generates the strokes and renders them **in paper coordinates** —
what the pen will actually trace, not what the camera saw. A drawing can take
several minutes of arm motion, so it is worth a look first.

With `preview_camera` configured, the render is pushed there and you watch it in
the app's camera panel:

```json
{"preview": true, "polyline_count": 128, "total_points": 4310, "preview_camera": "line-preview"}
```

Without one, the PNG comes back as base64 in `preview_png_b64` instead.

If the result looks too sparse or too dense, tune the stroke-generator's
`detail` and `face_detail` and preview again. When it looks right, run the same
command without `preview`.

### `draw`

Draws polylines you supply directly, in paper-local mm with `(0, 0)` at the
top-left corner. This is what the other verbs call underneath.

```json
{"draw": {"polylines": [[[0, 0], [10, 5]], [[20, 20], [25, 30]]]}}
```

### `cancel` and `go_home`

`cancel` aborts whatever is drawing and stops the arm. `go_home` moves to the
rest pose. Only one draw runs at a time — starting a second returns an error
telling you to cancel the first.

```json
{"cancel": {}}
```

## `viam:portrait-drawing:stroke-generator`

Turns an image into polylines. It holds no state and has no dependencies, so it
is safe to call directly while tuning.

### Configuration

Every attribute is optional; the defaults are tuned for portraits.

| Attribute | Type | Description |
|---|---|---|
| `detail` | number | Overall line density. Higher means more lines. |
| `face_detail` | number | Extra density inside a detected face, so features survive when the background is simplified. |
| `face_size_px` | int | Size the detected face is normalised to before detail is applied. |
| `isolate_subject` | bool | Drop the background and keep the person. Defaults to true. |
| `region`, `low`, `high` | int | Adaptive-threshold region and Canny hysteresis bounds. |
| `merge`, `prune`, `min_len` | int | Contour merging distance, spur pruning length, and the shortest polyline kept. |
| `smooth`, `min_dist` | number | Curve smoothing and the minimum spacing between retained points. |

Face-aware detail needs the YuNet face model on the machine; without it the
pipeline still runs with uniform detail.

### `generate`

```json
{"generate": {
  "image_b64": "/9j/4AAQ…",
  "paper_width_mm": 215.9,
  "paper_height_mm": 279.4,
  "margin_mm": 40
}}
```

Returns `{"polylines": [[[x, y], …], …]}` in paper-local mm — exactly the shape
the drawer's `draw` verb accepts.

## Setting up the capture loop

`capture_and_draw` needs a camera that holds a still, rather than a live stream:
the drawer triggers a shot and then reads it back. Configure a
[frame-buffer](https://github.com/viam-labs/frame-buffer) camera pointed at your
real camera, and a second one to receive previews:

```json
{
  "name": "photo",
  "api": "rdk:component:camera",
  "model": "viam:frame-buffer:camera",
  "attributes": { "camera": "camera-1", "source_name": "color", "delay_sec": 3 }
},
{
  "name": "line-preview",
  "api": "rdk:component:camera",
  "model": "viam:frame-buffer:camera",
  "attributes": {}
}
```

Then point the drawer at them with `"photo": "photo"` and
`"preview_camera": "line-preview"`. Both appear as camera panels in the app —
one shows the photo that was taken, the other the line drawing it became.

The countdown lives on the `photo` camera's `delay_sec`, not on the drawer.
