"""Convert a photo on stdin into ordered 2D polylines in paper-local mm.

Pipeline: normalise resolution by face size -> optional GrabCut background
removal -> Canny edges (thresholds auto-tuned to a target detail level, the
face traced richer than the body) -> tone-region outline (hair silhouette +
hairline) -> morphological close + thinning (collapse doubled edges to single
1px centerlines) -> spur pruning -> findContours -> simplify + decimate.
Coordinates are scaled and centered to fit the requested paper size, with
(0,0) at the paper's top-left corner and Y pointing down.

Usage:
    cat photo.jpg | python image_to_polylines.py \\
        --paper-width-mm 215.9 --paper-height-mm 279.4 --margin-mm 40 \\
        [--auto-rotate | --no-auto-rotate] [--rotate 0|90|180|270] [--mirror] \\
        [--detail 1.0] [--face-detail 6.0] [--face-size-px 520] \\
        [--isolate-subject | --no-isolate-subject] \\
        [--region 10] [--low 50] [--high 120] [--merge 3] [--prune 25] \\
        [--min-len 40] [--smooth 2.5] [--min-dist 5]

With --auto-rotate (the default), rotation is chosen (0 or 90) so the
image's long side aligns with the paper's long side; --rotate is ignored.
Pass --no-auto-rotate to use --rotate verbatim.

Face detection and resolution normalisation use the YuNet model in models/
(models/face_detection_yunet_2023mar.onnx). If it is absent the pipeline
falls back to non-face-aware behaviour.

Prints {"polylines": [[[x, y], ...], ...]} to stdout.
"""

import argparse
import json
import sys
from pathlib import Path

import cv2
import numpy as np
import onnxruntime as ort

# 8-connected neighbour kernel, reused by spur pruning.
_NEIGHBOURS = np.array([[1, 1, 1], [1, 0, 1], [1, 1, 1]], np.float32)

# YuNet face model (optional); long-side cap used when no face is detected.
_YUNET_PATH = Path(__file__).resolve().parent.parent / "models" / \
    "face_detection_yunet_2023mar.onnx"
_MAX_WORK_PX = 1600
_face_detector = None

# Line-drawing model (informative-drawings, CVPR 2022). first_run.sh fetches it;
# unlike YuNet there is no fallback, since it is the extraction stage itself.
_MODEL_PATH = Path(__file__).resolve().parent.parent / "models" / "lineart.onnx"
_session = None


def prune_spurs(skel: np.ndarray, iters: int) -> np.ndarray:
    """Remove short skeleton spurs by eroding free line-ends `iters` times.

    Each pass deletes every pixel with exactly one 8-neighbour (a line tip),
    so a spur of length N vanishes after N passes while a through-line only
    loses `iters` pixels from each of its two ends.
    """
    if iters <= 0:
        return skel
    sk = (skel > 0).astype(np.uint8)
    for _ in range(iters):
        counts = cv2.filter2D(sk.astype(np.float32), -1, _NEIGHBOURS)
        tips = (np.rint(counts).astype(np.int32) == 1) & (sk > 0)
        if not tips.any():
            break
        sk[tips] = 0
    return (sk * 255).astype(np.uint8)


def decimate_vertices(cnt: np.ndarray, min_dist: float) -> np.ndarray:
    """Drop vertices that sit closer than `min_dist` px to the last kept one."""
    if min_dist <= 0:
        return cnt
    pts = cnt.reshape(-1, 2)
    if len(pts) <= 2:
        return cnt
    kept = [pts[0]]
    for p in pts[1:-1]:
        if np.hypot(*(p - kept[-1])) >= min_dist:
            kept.append(p)
    kept.append(pts[-1])
    return np.array(kept, dtype=np.int32).reshape(-1, 1, 2)


def detect_face(img: np.ndarray) -> tuple:
    """Return the largest face box (fx, fy, fw, fh) via YuNet, or None.

    None if the model is missing or no face is found — callers then fall back
    to non-face-aware behaviour.
    """
    global _face_detector
    if not _YUNET_PATH.exists():
        return None
    if _face_detector is None:
        _face_detector = cv2.FaceDetectorYN_create(
            str(_YUNET_PATH), "", (320, 320), score_threshold=0.6)
    h, w = img.shape[:2]
    _face_detector.setInputSize((w, h))
    _, faces = _face_detector.detect(img)
    if faces is None or len(faces) == 0:
        return None
    return tuple(float(v) for v in max(faces, key=lambda f: f[2] * f[3])[:4])


def head_ellipse(box: tuple, shape: tuple) -> np.ndarray:
    """Boolean head-region mask: a generous ellipse over the detected face.

    Widened and shifted up to cover forehead and hair, so the high-detail zone
    tracks the whole head rather than just the face box.
    """
    fx, fy, fw, fh = box
    cx, cy = fx + fw / 2, fy + fh / 2 - 0.12 * fh
    mask = np.zeros(shape, np.uint8)
    cv2.ellipse(mask, (int(cx), int(cy)), (int(0.8 * fw), int(0.95 * fh)),
                0, 0, 360, 255, -1)
    return mask > 0


def subject_mask(img: np.ndarray, face: tuple = None) -> np.ndarray:
    """Segment the person from the background with GrabCut (downscaled).

    A face box seeds it (face sure-FG, head+body probable-FG, border BG) so the
    head survives even when dark hair blends into a dark background, while
    GrabCut still removes clutter that differs in colour. Falls back to a
    central-rectangle seed with no face. Returns the largest blob as a 0/255 mask.
    """
    h, w = img.shape[:2]
    scale = 800 / max(h, w)
    small = cv2.resize(img, (round(w * scale), round(h * scale)))
    sh, sw = small.shape[:2]
    bgd, fgd = np.zeros((1, 65), np.float64), np.zeros((1, 65), np.float64)
    if face is not None:
        fx, fy, fw, fh = (v * scale for v in face)
        cx = fx + fw / 2
        mask = np.full((sh, sw), cv2.GC_PR_BGD, np.uint8)
        b = max(2, int(0.03 * max(sh, sw)))
        mask[:b, :] = mask[-b:, :] = mask[:, :b] = mask[:, -b:] = cv2.GC_BGD
        mask[max(0, int(fy - 1.4 * fh)):int(fy + fh),
             max(0, int(cx - 0.75 * fw)):min(sw, int(cx + 0.75 * fw))] = cv2.GC_PR_FGD
        mask[int(fy + fh):sh,
             max(0, int(cx - fw)):min(sw, int(cx + fw))] = cv2.GC_PR_FGD
        mask[int(fy + 0.2 * fh):int(fy + fh),
             int(fx + 0.2 * fw):int(fx + 0.8 * fw)] = cv2.GC_FGD
        cv2.grabCut(small, mask, None, bgd, fgd, 5, cv2.GC_INIT_WITH_MASK)
    else:
        rect = (int(0.10 * sw), int(0.06 * sh), int(0.80 * sw), int(0.94 * sh))
        mask = np.zeros((sh, sw), np.uint8)
        cv2.grabCut(small, mask, rect, bgd, fgd, 5, cv2.GC_INIT_WITH_RECT)
    fg = np.where((mask == cv2.GC_FGD) | (mask == cv2.GC_PR_FGD), 255, 0).astype(np.uint8)
    fg = cv2.morphologyEx(fg, cv2.MORPH_OPEN, np.ones((5, 5), np.uint8))
    fg = cv2.morphologyEx(fg, cv2.MORPH_CLOSE, np.ones((15, 15), np.uint8))
    n, labels, stats, _ = cv2.connectedComponentsWithStats(fg, 8)
    if n > 1:
        biggest = 1 + int(np.argmax(stats[1:, cv2.CC_STAT_AREA]))
        fg = np.where(labels == biggest, 255, 0).astype(np.uint8)
    return cv2.resize(fg, (w, h), interpolation=cv2.INTER_NEAREST)


def crop_to_face(
    img: np.ndarray, above: float, below: float, sides: float,
) -> np.ndarray:
    """Crop to the subject, measured in multiples of the detected face box.

    The pipeline normalises resolution by face size and detail by edge density,
    but not composition. A photo taken from across a room spends most of the
    paper on torso and room, leaving the face too small for its features to
    survive the tracing. Cropping in face units keeps the framing consistent
    however far away the subject sits.

    Returns the image unchanged when no face is found, so a missed detection
    costs framing rather than the whole drawing.
    """
    box = detect_face(img)
    if box is None:
        return img
    fx, fy, fw, fh = box
    cx = fx + fw / 2
    h, w = img.shape[:2]
    x0, x1 = max(0, int(cx - sides * fw)), min(w, int(cx + sides * fw))
    y0, y1 = max(0, int(fy - above * fh)), min(h, int(fy + fh + below * fh))
    if x1 - x0 < 2 or y1 - y0 < 2:
        return img
    return img[y0:y1, x0:x1]


def isolate(img: np.ndarray, isolate_subject: bool, head_span: float = 2.4) -> tuple:
    """Return (image, subject mask or None).

    The line model draws whatever is in frame, so without a mask the arm spends
    its minutes on the room. GrabCut alone is not enough for a portrait: dark
    hair against dark clutter merges into one foreground blob, and the largest
    connected component then includes a chair rack. Bounding it to an ellipse
    around the head cuts that off — safe here because the crop is a head-and-
    shoulders portrait, so nothing worth drawing lies outside it.
    """
    if not isolate_subject:
        return img, None
    box = detect_face(img)
    mask = subject_mask(img, face=box) > 0
    if box is not None:
        fx, fy, fw, fh = box
        bound = np.zeros(mask.shape, np.uint8)
        cv2.ellipse(bound,
                    (int(fx + fw / 2), int(fy + fh / 2 - 0.12 * fh)),
                    (int(head_span * fw), int(head_span * fh)),
                    0, 0, 360, 255, -1)
        mask &= bound > 0
    return img, mask


def enhance(img: np.ndarray, clahe_clip: float) -> np.ndarray:
    """Lift local contrast before the model sees it.

    The model draws what it can distinguish. A face lit from above, or simply
    underexposed because the camera metered for a bright window, produces a weak
    response around the mouth and nose while the hair and silhouette come out
    strong. CLAHE equalises that locally, so faint features are lifted relative
    to their own neighbourhood; a global gamma would lift the whole face and
    flatten the very gradients worth keeping.
    """
    if clahe_clip <= 0:
        return img
    lab = cv2.cvtColor(img, cv2.COLOR_BGR2LAB)
    channel_l, channel_a, channel_b = cv2.split(lab)
    channel_l = cv2.createCLAHE(clipLimit=clahe_clip, tileGridSize=(8, 8)).apply(channel_l)
    return cv2.cvtColor(cv2.merge([channel_l, channel_a, channel_b]), cv2.COLOR_LAB2BGR)


def line_response(img: np.ndarray, size: int) -> np.ndarray:
    """Run the line-drawing model and return its greyscale response.

    The model takes dynamic height and width, so the image goes in at its own
    aspect ratio. Squashing it to a square makes the model draw a stretched
    face; letterboxing spends resolution on padding, which shrinks the face and
    costs the finest features first.
    """
    if not _MODEL_PATH.exists():
        raise RuntimeError(
            f"line-art model missing at {_MODEL_PATH}; first_run.sh downloads it")
    global _session
    if _session is None:
        _session = ort.InferenceSession(str(_MODEL_PATH), providers=["CPUExecutionProvider"])
    height, width = img.shape[:2]
    scale = size / max(height, width)
    new_w = max(8, int(round(width * scale / 8)) * 8)
    new_h = max(8, int(round(height * scale / 8)) * 8)
    interp = cv2.INTER_AREA if scale < 1 else cv2.INTER_CUBIC
    resized = cv2.resize(img, (new_w, new_h), interpolation=interp)
    rgb = cv2.cvtColor(resized, cv2.COLOR_BGR2RGB).astype(np.float32) / 255.0
    out = _session.run(None, {_session.get_inputs()[0].name: rgb.transpose(2, 0, 1)[None]})[0]
    line = np.squeeze(out)
    if line.ndim == 3:
        line = line.mean(axis=0)
    return (np.clip(line, 0, 1) * 255).astype(np.uint8)


def ridge_centerlines(response: np.ndarray, sigma: float, low: float, high: float) -> np.ndarray:
    """Trace stroke centrelines out of the model's greyscale response.

    Binarising first is lossy twice over: it discards the intensity that says
    how confident the model was, and it decides per pixel, so a faint feature is
    dropped even when it plainly continues a stroke that is strong elsewhere.
    Raising the threshold to keep features then admits noise everywhere else.

    Instead: a stroke is a ridge, so the smaller Hessian eigenvalue is strongly
    negative across it. Suppressing non-maxima in that direction yields a
    one-pixel centreline without binarising and without thinning having to guess
    it back. Hysteresis then keeps every ridge pixel above `low` that connects
    to one above `high`, so faint features survive by attachment rather than by
    their own amplitude.
    """
    bright = (255 - response).astype(np.float32)
    blurred = cv2.GaussianBlur(bright, (0, 0), sigma)
    ixx = cv2.Sobel(blurred, cv2.CV_32F, 2, 0, ksize=3)
    iyy = cv2.Sobel(blurred, cv2.CV_32F, 0, 2, ksize=3)
    ixy = cv2.Sobel(blurred, cv2.CV_32F, 1, 1, ksize=3)
    spread = np.sqrt((ixx - iyy) ** 2 + 4.0 * ixy ** 2)
    smaller = 0.5 * (ixx + iyy - spread)
    strength = np.maximum(0.0, -smaller)

    vec_x, vec_y = ixy, smaller - ixx
    norm = np.hypot(vec_x, vec_y) + 1e-6
    angle = (np.degrees(np.arctan2(vec_y / norm, vec_x / norm)) + 180.0) % 180.0
    sector = np.zeros(angle.shape, np.int32)
    sector[(angle >= 22.5) & (angle < 67.5)] = 1
    sector[(angle >= 67.5) & (angle < 112.5)] = 2
    sector[(angle >= 112.5) & (angle < 157.5)] = 3

    keep = np.ones(strength.shape, bool)
    for index, (dx, dy) in {0: (1, 0), 1: (1, 1), 2: (0, 1), 3: (-1, 1)}.items():
        forward = np.roll(np.roll(strength, -dy, axis=0), -dx, axis=1)
        backward = np.roll(np.roll(strength, dy, axis=0), dx, axis=1)
        here = sector == index
        keep &= ~(here & ((strength < forward) | (strength < backward)))
    ridge = np.where(keep, strength, 0.0)
    ridge[0, :] = ridge[-1, :] = ridge[:, 0] = ridge[:, -1] = 0

    weak = (ridge >= low).astype(np.uint8)
    count, labels = cv2.connectedComponents(weak, connectivity=8)
    if count <= 1:
        return np.zeros_like(weak)
    connected = np.zeros(count, bool)
    connected[np.unique(labels[ridge >= high])] = True
    connected[0] = False
    return (connected[labels] * 255).astype(np.uint8)


def extract_polylines(
    img: np.ndarray,
    size: int,
    clahe_clip: float,
    sigma: float,
    low: float,
    high: float,
    prune: int,
    min_len: int,
    smooth: float,
    min_dist: float,
    roi: np.ndarray = None,
) -> list[np.ndarray]:
    """Turn a photo into clean single-stroke polylines."""
    response = line_response(enhance(img, clahe_clip), size)
    edges = ridge_centerlines(response, sigma, low, high)
    # The model paints a border along the image edge; it belongs to no one.
    edges[:3, :] = edges[-3:, :] = edges[:, :3] = edges[:, -3:] = 0
    if roi is not None:
        mask = cv2.resize(roi.astype(np.uint8), (edges.shape[1], edges.shape[0]),
                          interpolation=cv2.INTER_NEAREST)
        edges[cv2.dilate(mask, np.ones((9, 9), np.uint8)) == 0] = 0
    edges = prune_spurs(edges, prune)

    contours, _ = cv2.findContours(edges, cv2.RETR_LIST, cv2.CHAIN_APPROX_SIMPLE)
    polylines = []
    for cnt in contours:
        if cv2.arcLength(cnt, closed=False) < min_len:
            continue
        cnt = cv2.approxPolyDP(cnt, smooth, closed=False)
        polylines.append(decimate_vertices(cnt, min_dist))
    return polylines


def _rotate_ccw(pts: np.ndarray, degrees: int) -> np.ndarray:
    """Rotate (N,2) points anti-clockwise by 0/90/180/270° about the origin."""
    x, y = pts[:, 0], pts[:, 1]
    d = degrees % 360
    if d == 0:
        rx, ry = x, y
    elif d == 90:
        rx, ry = y, -x
    elif d == 180:
        rx, ry = -x, -y
    elif d == 270:
        rx, ry = -y, x
    else:
        raise ValueError(f"rotate must be 0, 90, 180, or 270 (got {degrees})")
    return np.stack([rx, ry], axis=1)


def _rotation_for_paper(img_w: int, img_h: int, paper_w: float, paper_h: float) -> int:
    """Return 0 or 90 so the image's long side aligns with the paper's long side."""
    img_landscape = img_w > img_h
    paper_landscape = paper_w > paper_h
    return 90 if img_landscape != paper_landscape else 0


def polylines_to_mm(
    polylines: list[np.ndarray],
    paper_w: float,
    paper_h: float,
    margin: float,
    rotate: int,
    mirror: bool,
) -> list[list[list[float]]]:
    """Convert pixel-space polylines to paper-local mm, aspect preserved, centered."""
    if not polylines:
        return []

    all_pts = _rotate_ccw(
        np.concatenate([p.reshape(-1, 2) for p in polylines]), rotate,
    )
    origin = all_pts.min(axis=0)
    draw_w, draw_h = all_pts.max(axis=0) - origin

    avail_w, avail_h = paper_w - 2 * margin, paper_h - 2 * margin
    scale = min(avail_w / max(draw_w, 1), avail_h / max(draw_h, 1))
    off_x = (paper_w - draw_w * scale) / 2
    off_y = (paper_h - draw_h * scale) / 2

    out = []
    for cnt in polylines:
        p = _rotate_ccw(cnt.reshape(-1, 2).astype(float), rotate) - origin
        if mirror:
            p[:, 0] = draw_w - p[:, 0]
        line = [
            [round(off_x + x * scale, 2), round(off_y + y * scale, 2)]
            for x, y in p
        ]
        out.append(line)
    return out


def image_bytes_to_polylines(
    image_bytes: bytes,
    paper_width_mm: float,
    paper_height_mm: float,
    margin_mm: float,
    rotate: int,
    mirror: bool,
    prune: int,
    min_len: int,
    smooth: float,
    min_dist: float,
    size: int = 768,
    clahe: float = 2.0,
    sigma: float = 2.2,
    low: float = 4.0,
    high: float = 12.0,
    isolate_subject: bool = True,
    head_span: float = 2.4,
    auto_rotate: bool = True,
    crop_face: bool = False,
    crop_above: float = 1.2,
    crop_below: float = 2.0,
    crop_sides: float = 1.5,
) -> list[list[list[float]]]:
    """Decode image bytes and return paper-local mm polylines."""
    array = np.frombuffer(image_bytes, dtype=np.uint8)
    img = cv2.imdecode(array, cv2.IMREAD_COLOR)
    if img is None:
        raise ValueError("could not decode image bytes")
    if crop_face:
        img = crop_to_face(img, crop_above, crop_below, crop_sides)
    img, roi = isolate(img, isolate_subject, head_span)
    if auto_rotate:
        img_h, img_w = img.shape[:2]
        rotate = _rotation_for_paper(img_w, img_h, paper_width_mm, paper_height_mm)
    polylines = extract_polylines(
        img, size=size, clahe_clip=clahe, sigma=sigma, low=low, high=high,
        prune=prune, min_len=min_len, smooth=smooth, min_dist=min_dist, roi=roi,
    )
    return polylines_to_mm(
        polylines, paper_width_mm, paper_height_mm, margin_mm, rotate, mirror,
    )


def main() -> int:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--paper-width-mm", type=float, default=215.9)
    p.add_argument("--paper-height-mm", type=float, default=279.4)
    p.add_argument("--margin-mm", type=float, default=40.0)
    p.add_argument("--rotate", type=int, default=0, choices=[0, 90, 180, 270])
    p.add_argument("--auto-rotate", action=argparse.BooleanOptionalAction, default=True)
    p.add_argument("--mirror", action="store_true")
    p.add_argument("--size", type=int, default=768)
    p.add_argument("--clahe", type=float, default=2.0)
    p.add_argument("--sigma", type=float, default=2.2)
    p.add_argument("--isolate-subject", action=argparse.BooleanOptionalAction, default=True)
    p.add_argument("--head-span", type=float, default=2.4)
    p.add_argument("--crop-face", action=argparse.BooleanOptionalAction, default=False)
    p.add_argument("--crop-above", type=float, default=1.2)
    p.add_argument("--crop-below", type=float, default=2.0)
    p.add_argument("--crop-sides", type=float, default=1.5)
    p.add_argument("--low", type=float, default=4.0)
    p.add_argument("--high", type=float, default=12.0)
    p.add_argument("--prune", type=int, default=20)
    p.add_argument("--min-len", type=int, default=36)
    p.add_argument("--smooth", type=float, default=2.0)
    p.add_argument("--min-dist", type=float, default=3.0)
    args = p.parse_args()

    image_bytes = sys.stdin.buffer.read()
    if not image_bytes:
        print("error: no image bytes on stdin", file=sys.stderr)
        return 1

    try:
        polylines = image_bytes_to_polylines(
            image_bytes,
            paper_width_mm=args.paper_width_mm,
            paper_height_mm=args.paper_height_mm,
            margin_mm=args.margin_mm,
            rotate=args.rotate,
            mirror=args.mirror,
            size=args.size,
            clahe=args.clahe,
            sigma=args.sigma,
            low=args.low,
            high=args.high,
            prune=args.prune,
            min_len=args.min_len,
            smooth=args.smooth,
            min_dist=args.min_dist,
            isolate_subject=args.isolate_subject,
            head_span=args.head_span,
            crop_face=args.crop_face,
            crop_above=args.crop_above,
            crop_below=args.crop_below,
            crop_sides=args.crop_sides,
            auto_rotate=args.auto_rotate,
        )
    except Exception as e:
        print(f"error: {e}", file=sys.stderr)
        return 1

    json.dump({"polylines": polylines}, sys.stdout)
    return 0


if __name__ == "__main__":
    sys.exit(main())
