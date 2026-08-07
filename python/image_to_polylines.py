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

# 8-connected neighbour kernel, reused by spur pruning.
_NEIGHBOURS = np.array([[1, 1, 1], [1, 0, 1], [1, 1, 1]], np.float32)

# YuNet face model (optional); long-side cap used when no face is detected.
_YUNET_PATH = Path(__file__).resolve().parent.parent / "models" / \
    "face_detection_yunet_2023mar.onnx"
_MAX_WORK_PX = 1600
_face_detector = None


def region_edges(gray: np.ndarray, close_k: int) -> np.ndarray:
    """Edge map of the dark-vs-light tone regions (hair silhouette, hairline).

    Otsu thresholds skin vs hair, consolidates the dark mass into a solid blob
    (close), removes small specks (open), then traces the boundary — a clean
    continuous silhouette line that gradient edge detection would miss.
    """
    blurred = cv2.GaussianBlur(gray, (9, 9), 0)
    _, mask = cv2.threshold(blurred, 0, 255, cv2.THRESH_BINARY_INV + cv2.THRESH_OTSU)
    close_k = max(3, close_k | 1)
    mask = cv2.morphologyEx(
        mask, cv2.MORPH_CLOSE,
        cv2.getStructuringElement(cv2.MORPH_ELLIPSE, (close_k, close_k)),
    )
    open_k = max(3, (close_k // 3) | 1)
    mask = cv2.morphologyEx(
        mask, cv2.MORPH_OPEN,
        cv2.getStructuringElement(cv2.MORPH_ELLIPSE, (open_k, open_k)),
    )
    return cv2.Canny(mask, 50, 150)


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


def auto_canny_thresholds(filtered: np.ndarray, target_frac: float,
                          roi: np.ndarray = None,
                          ratio: float = 0.4) -> tuple[int, int]:
    """Pick Canny thresholds so ~`target_frac` of pixels become edges.

    Binary-searches the high threshold (low = ratio * high) so detail is
    consistent across bright/dark photos; `roi` restricts where density is
    measured.
    """
    lo_h, hi_h = 10.0, 300.0
    for _ in range(18):
        h = (lo_h + hi_h) / 2
        edges = cv2.Canny(filtered, int(ratio * h), int(h)) > 0
        frac = edges[roi].mean() if roi is not None else edges.mean()
        if frac > target_frac:
            lo_h = h
        else:
            hi_h = h
    h = (lo_h + hi_h) / 2
    return int(ratio * h), int(h)


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


def analyze_image(img: np.ndarray, isolate_subject: bool, face_detail: float,
                  face_size_px: int) -> tuple:
    """Grayscale the image; normalise resolution, optionally remove background.

    Scales so the detected face is ~`face_size_px` wide (detail params are in
    pixels, so this keeps detail consistent across resolutions). Returns
    (gray, roi, face): the subject mask and head-region mask, each a boolean
    array or None.
    """
    box = detect_face(img) if (face_detail > 0 or isolate_subject) else None

    if box is not None:
        scale = face_size_px / box[2]
    else:
        scale = min(1.0, _MAX_WORK_PX / max(img.shape[:2]))
    scale = float(np.clip(scale, 0.2, 3.0))
    if abs(scale - 1.0) > 0.05:
        interp = cv2.INTER_AREA if scale < 1 else cv2.INTER_CUBIC
        img = cv2.resize(img, None, fx=scale, fy=scale, interpolation=interp)
        if box is not None:
            box = tuple(v * scale for v in box)

    gray = cv2.cvtColor(img, cv2.COLOR_BGR2GRAY)
    roi = None
    if isolate_subject:
        mask = subject_mask(img, face=box)
        gray = gray.copy()
        gray[mask == 0] = 255
        roi = mask > 0
    face = head_ellipse(box, gray.shape) if (face_detail > 0 and box) else None
    if face is not None and roi is not None:
        face = face & roi
    return gray, roi, face


def extract_polylines(
    gray: np.ndarray,
    detail: float,
    region: int,
    low: int,
    high: int,
    merge: int,
    prune: int,
    min_len: int,
    smooth: float,
    min_dist: float,
    roi: np.ndarray = None,
    face: np.ndarray = None,
    face_detail: float = 0.0,
) -> list[np.ndarray]:
    """Extract clean single-stroke polylines from a grayscale image.

    Thresholds are auto-tuned to a `detail` density (0 uses fixed low/high).
    A `face` mask is traced at the higher `face_detail`, and the tone-region
    outline is dropped over the lower face (kept across the top for the
    hairline) so the features stay clean contour lines.
    """
    filtered = cv2.bilateralFilter(gray, d=9, sigmaColor=75, sigmaSpace=75)
    if detail > 0:
        low, high = auto_canny_thresholds(filtered, detail / 100.0, roi=roi)
    edges = cv2.Canny(filtered, low, high)

    if face is not None and face.any() and face_detail > 0:
        f_low, f_high = auto_canny_thresholds(filtered, face_detail / 100.0, roi=face)
        edges = np.where(face, cv2.Canny(filtered, f_low, f_high), edges).astype(np.uint8)

    if region > 0:
        reg = region_edges(gray, region)
        if face is not None and face.any() and face_detail > 0:
            rows = np.where(face.any(axis=1))[0]
            cutoff = rows.min() + int(0.4 * (rows.max() - rows.min()))
            lower_face = face.copy()
            lower_face[:cutoff, :] = False
            reg[lower_face] = 0
        edges = cv2.max(edges, reg)

    kernel = cv2.getStructuringElement(cv2.MORPH_ELLIPSE, (max(1, merge),) * 2)
    edges = cv2.morphologyEx(edges, cv2.MORPH_CLOSE, kernel)
    edges = cv2.ximgproc.thinning(edges, thinningType=cv2.ximgproc.THINNING_ZHANGSUEN)
    edges = prune_spurs(edges, prune)

    contours, _ = cv2.findContours(edges, cv2.RETR_LIST, cv2.CHAIN_APPROX_SIMPLE)

    polylines = []
    for cnt in contours:
        if cv2.arcLength(cnt, closed=False) < min_len:
            continue
        cnt = cv2.approxPolyDP(cnt, smooth, closed=False)
        cnt = decimate_vertices(cnt, min_dist)
        polylines.append(cnt)
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
    region: int,
    low: int,
    high: int,
    merge: int,
    prune: int,
    min_len: int,
    smooth: float,
    min_dist: float,
    detail: float = 1.0,
    face_detail: float = 6.0,
    isolate_subject: bool = True,
    face_size_px: int = 520,
    auto_rotate: bool = True,
) -> list[list[list[float]]]:
    """Decode image bytes and return paper-local mm polylines."""
    array = np.frombuffer(image_bytes, dtype=np.uint8)
    img = cv2.imdecode(array, cv2.IMREAD_COLOR)
    if img is None:
        raise ValueError("could not decode image bytes")
    gray, roi, face = analyze_image(img, isolate_subject, face_detail, face_size_px)
    if auto_rotate:
        img_h, img_w = gray.shape
        rotate = _rotation_for_paper(img_w, img_h, paper_width_mm, paper_height_mm)
    polylines = extract_polylines(
        gray, detail=detail, region=region, low=low, high=high, merge=merge,
        prune=prune, min_len=min_len, smooth=smooth, min_dist=min_dist,
        roi=roi, face=face, face_detail=face_detail,
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
    p.add_argument("--detail", type=float, default=1.0)
    p.add_argument("--face-detail", type=float, default=6.0)
    p.add_argument("--face-size-px", type=int, default=520)
    p.add_argument("--isolate-subject", action=argparse.BooleanOptionalAction, default=True)
    p.add_argument("--region", type=int, default=10)
    p.add_argument("--low", type=int, default=50)
    p.add_argument("--high", type=int, default=120)
    p.add_argument("--merge", type=int, default=3)
    p.add_argument("--prune", type=int, default=25)
    p.add_argument("--min-len", type=int, default=40)
    p.add_argument("--smooth", type=float, default=2.5)
    p.add_argument("--min-dist", type=float, default=5.0)
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
            region=args.region,
            low=args.low,
            high=args.high,
            merge=args.merge,
            prune=args.prune,
            min_len=args.min_len,
            smooth=args.smooth,
            min_dist=args.min_dist,
            detail=args.detail,
            face_detail=args.face_detail,
            isolate_subject=args.isolate_subject,
            face_size_px=args.face_size_px,
            auto_rotate=args.auto_rotate,
        )
    except Exception as e:
        print(f"error: {e}", file=sys.stderr)
        return 1

    json.dump({"polylines": polylines}, sys.stdout)
    return 0


if __name__ == "__main__":
    sys.exit(main())
