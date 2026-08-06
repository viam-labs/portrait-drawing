"""Convert a photo on stdin into ordered 2D polylines in paper-local mm.

Pipeline: Canny edges + tone-region outline (hair silhouette + hairline) →
morphological close + thinning (collapse doubled edges to single 1px
centerlines) → spur pruning → findContours → simplify + decimate.
Coordinates are scaled and centered to fit the requested paper size,
with (0,0) at the paper's top-left corner and Y pointing down.

Usage:
    cat photo.jpg | python image_to_polylines.py \\
        --paper-width-mm 215.9 --paper-height-mm 279.4 --margin-mm 40 \\
        [--rotate 0|90|180|270] [--mirror] [--region 25] \\
        [--low 60] [--high 160] [--merge 5] [--prune 25] \\
        [--min-len 90] [--smooth 2.5] [--min-dist 8]

Prints {"polylines": [[[x, y], ...], ...]} to stdout.
"""

import argparse
import json
import sys

import cv2
import numpy as np

# 8-connected neighbour kernel, reused by spur pruning.
_NEIGHBOURS = np.array([[1, 1, 1], [1, 0, 1], [1, 1, 1]], np.float32)


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


def extract_polylines(
    gray: np.ndarray,
    region: int,
    low: int,
    high: int,
    merge: int,
    prune: int,
    min_len: int,
    smooth: float,
    min_dist: float,
) -> list[np.ndarray]:
    """Extract clean single-stroke polylines from a grayscale image."""
    filtered = cv2.bilateralFilter(gray, d=9, sigmaColor=75, sigmaSpace=75)
    edges = cv2.Canny(filtered, low, high)

    if region > 0:
        edges = cv2.max(edges, region_edges(gray, region))

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
) -> list[list[list[float]]]:
    """Decode image bytes and return paper-local mm polylines."""
    array = np.frombuffer(image_bytes, dtype=np.uint8)
    img = cv2.imdecode(array, cv2.IMREAD_COLOR)
    if img is None:
        raise ValueError("could not decode image bytes")
    gray = cv2.cvtColor(img, cv2.COLOR_BGR2GRAY)
    polylines = extract_polylines(
        gray, region=region, low=low, high=high, merge=merge,
        prune=prune, min_len=min_len, smooth=smooth, min_dist=min_dist,
    )
    return polylines_to_mm(polylines, paper_width_mm, paper_height_mm, margin_mm, rotate, mirror)


def main() -> int:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--paper-width-mm", type=float, default=215.9)
    p.add_argument("--paper-height-mm", type=float, default=279.4)
    p.add_argument("--margin-mm", type=float, default=40.0)
    p.add_argument("--rotate", type=int, default=0, choices=[0, 90, 180, 270])
    p.add_argument("--mirror", action="store_true")
    p.add_argument("--region", type=int, default=25)
    p.add_argument("--low", type=int, default=60)
    p.add_argument("--high", type=int, default=160)
    p.add_argument("--merge", type=int, default=5)
    p.add_argument("--prune", type=int, default=25)
    p.add_argument("--min-len", type=int, default=90)
    p.add_argument("--smooth", type=float, default=2.5)
    p.add_argument("--min-dist", type=float, default=8.0)
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
        )
    except Exception as e:
        print(f"error: {e}", file=sys.stderr)
        return 1

    json.dump({"polylines": polylines}, sys.stdout)
    return 0


if __name__ == "__main__":
    sys.exit(main())
