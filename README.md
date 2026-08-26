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
  "home_pose": {
    "translation": { "x": 254, "y": 5, "z": 551 },
    "orientation": { "type": "ov_degrees", "value": { "x": 0.127, "y": 0.011, "z": -0.992, "th": 5.19 } }
  },
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
| `home_pose` | pose | no | Where the arm parks after a drawing — and, for `capture_and_draw`, the pose it captures from. |
| `stroke_generator` | string | no | A `stroke-generator` service. Required for `draw_image` and `capture_and_draw`. |
| `photo` | string | no | A camera the drawer triggers and reads in `capture_and_draw` — a [frame-buffer](https://github.com/viam-labs/frame-buffer) camera pointed at your real camera. |
| `preview_camera` | string | no | A frame-buffer camera the drawer pushes rendered previews into, so previews are viewable in the Viam app instead of coming back as base64. |
| `input_range_override` | object | no | Per-joint limits tighter than the arm model declares, to keep planned motions inside a safe envelope. Limits can only be tightened, never loosened. |

### `capture_and_draw`

The whole loop in one command. The arm moves to `home_pose`, the `photo` camera
is triggered (it owns the countdown), and the resulting photo goes through the
stroke generator and onto the paper.

```json
{"capture_and_draw": {}}
```

All the stroke options below are accepted here too. Set `preview` first — the
arm still moves to the capture pose and takes the photo, but stops there:

```json
{"capture_and_draw": {"preview": true}}
```

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

`cancel` aborts whatever is drawing and stops the arm. `go_home` moves to
`home_pose`. Only one draw runs at a time — starting a second returns an error
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
