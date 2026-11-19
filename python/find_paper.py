#!/usr/bin/env python3
"""Find the sheets on the drawing mat in a photo from the survey pose, and the
calibration that turns their pixels into arm coordinates.

    find_paper.py sheets  < photo.jpg      sheets on the mat, in pixels
    find_paper.py crosses < photo.jpg      the calibration crosses, in pixels
    find_paper.py fit     < pairs.json     match crosses to the arm points that
                                           drew them; image-to-arm homography

Everything here is in pixels; turning that into millimetres and arm poses is
the Go service's job, with the homography `fit` returns.

A sheet is anything clearly brighter than the mat around it. A portrait card
is white all over; a name tag is a coloured label inside a white border (its
peel-off backing), and the colour varies by event. Comparing a sheet's middle
with its edge tells the two apart whatever the label colour, and cancels the
camera's colour cast.
"""

import argparse
import itertools
import json
import math
import sys
from pathlib import Path

import cv2
import numpy as np

CARD_MM = (101.6, 152.4)
TAG_MM = (85.0, 57.0)
# Mat brightness is fitted as a smooth surface; a sheet must stand this far
# above it. The Cricut grid lines reach about 26, a dim tag border over 30.
BRIGHT_ABOVE_MAT = 29
# Smallest sheet, in pixels of area, so grid squares and scraps are ignored.
MIN_SHEET_PX = 8000
MIN_RECT_FILL = 0.9
# Middle-to-edge colour difference (LAB a/b) above which a sheet is a tag.
TAG_COLOUR_SHIFT = 12
# A card is "used" once it has this many ink strokes at least this long.
USED_CARD_STROKES = 2
CARD_STROKE_MM = 8.0
# A tag is "used" once it has handwriting: ink taller than its printed labels.
TAG_HANDWRITING_MM = 4.0
LOGO_MIN_SCORE = 0.45
DEFAULT_LOGO = Path(__file__).resolve().parent.parent / "assets" / "viam_logo.png"


def read_image(data):
    img = cv2.imdecode(np.frombuffer(data, np.uint8), cv2.IMREAD_COLOR)
    if img is None:
        raise SystemExit("could not decode the image on stdin")
    return img


def mat_region(lab):
    """The mat: the big region that isn't the reddish wooden table around it."""
    mat = (lab[..., 1] < 134).astype(np.uint8) * 255
    mat = cv2.morphologyEx(mat, cv2.MORPH_OPEN, np.ones((15, 15), np.uint8))
    contours, _ = cv2.findContours(mat, cv2.RETR_EXTERNAL, cv2.CHAIN_APPROX_SIMPLE)
    if not contours:
        return None, None
    hull = cv2.convexHull(max(contours, key=cv2.contourArea))
    inside = np.zeros(mat.shape, np.uint8)
    cv2.drawContours(inside, [hull], -1, 255, -1)
    return hull, cv2.erode(inside, np.ones((9, 9), np.uint8))


def mat_background(lightness, inside):
    """A quadratic fit of the mat's brightness, ignoring anything brighter.

    Lighting varies across the mat, so a fixed threshold either misses dim
    sheets or catches bright mat.
    """
    h, w = lightness.shape
    ys, xs = np.nonzero(inside)
    pick = np.random.default_rng(0).choice(len(xs), min(20000, len(xs)), replace=False)
    xs, ys = xs[pick], ys[pick]

    def basis(x, y):
        x, y = x / w, y / h
        return np.stack([np.ones_like(x), x, y, x * x, x * y, y * y], -1)

    b = basis(xs.astype(float), ys.astype(float))
    vals = lightness[ys, xs]
    keep = np.ones(len(xs), bool)
    for _ in range(5):
        coef, *_ = np.linalg.lstsq(b[keep], vals[keep], rcond=None)
        keep = (vals - (b * coef).sum(-1)) < 8
    gx, gy = np.meshgrid(np.arange(w, dtype=float), np.arange(h, dtype=float))
    return (basis(gx, gy) * coef).sum(-1)


def quad_corners(contour):
    """Four sub-pixel corners, clockwise in the image, from straight-line fits
    to each side. Perspective makes a sheet a general quadrilateral, so a
    rotated rectangle would misplace its corners."""
    peri = cv2.arcLength(contour, True)
    approx = cv2.approxPolyDP(contour, 0.02 * peri, True).reshape(-1, 2)
    if len(approx) != 4:
        return order_clockwise(cv2.boxPoints(cv2.minAreaRect(contour)))
    approx = order_clockwise(approx.astype(float))
    pts = contour.reshape(-1, 2).astype(float)
    lines = []
    for i in range(4):
        a, b = approx[i], approx[(i + 1) % 4]
        d = b - a
        length = np.linalg.norm(d)
        rel = pts - a
        t = (rel[:, 0] * d[0] + rel[:, 1] * d[1]) / (length * length)
        dist = np.abs(d[0] * rel[:, 1] - d[1] * rel[:, 0]) / length
        side = pts[(t > 0.15) & (t < 0.85) & (dist < 4)]
        if len(side) < 5:
            return approx
        vx, vy, x0, y0 = cv2.fitLine(side.astype(np.float32), cv2.DIST_HUBER, 0, 0.01, 0.01).ravel()
        lines.append((np.array([x0, y0]), np.array([vx, vy])))
    corners = []
    for i in range(4):
        (p1, d1), (p2, d2) = lines[i - 1], lines[i]
        m = np.array([d1, -d2]).T
        if abs(np.linalg.det(m)) < 1e-6:
            return approx
        s, _ = np.linalg.solve(m, p2 - p1)
        corners.append(p1 + s * d1)
    return np.array(corners)


def order_clockwise(pts):
    """Clockwise on screen (y runs down), starting top-left-most."""
    pts = np.asarray(pts, float)
    c = pts.mean(0)
    pts = pts[np.argsort(np.arctan2(pts[:, 1] - c[1], pts[:, 0] - c[0]))]
    start = np.argmin(pts.sum(1))
    return np.roll(pts, -start, axis=0)


def side_lengths(corners):
    return [float(np.linalg.norm(corners[(i + 1) % 4] - corners[i])) for i in range(4)]


def start_at(corners, first):
    return np.roll(corners, -first, axis=0)


def portrait_order(corners):
    """A card's corners from its top-left with a short edge across the top:
    portrait. Either way up is fine, so the top is the short edge nearer the
    top of the image."""
    sides = side_lengths(corners)
    short = [i for i in range(4) if sides[i] <= sides[(i + 1) % 4]]
    first = min(short, key=lambda i: corners[i][1] + corners[(i + 1) % 4][1])
    return start_at(corners, first)


def reading_order(corners, up):
    """Corners from the top-left as the label reads, given its "up" in the image."""
    right = np.array([-up[1], up[0]])
    c = corners.mean(0)
    rel = corners - c
    first = int(np.argmax(rel[:, 0] * (up[0] - right[0]) + rel[:, 1] * (up[1] - right[1])))
    return start_at(corners, first)


# The logo template turned by each angle (clockwise on screen) and where the
# label's "up" then points in the image.
ROTATIONS = {
    0: (None, (0.0, -1.0)),
    90: (cv2.ROTATE_90_CLOCKWISE, (1.0, 0.0)),
    180: (cv2.ROTATE_180, (0.0, 1.0)),
    270: (cv2.ROTATE_90_COUNTERCLOCKWISE, (-1.0, 0.0)),
}


def find_logo(gray, mask, template):
    """Best match of the logo inside mask over four turns and a range of sizes,
    since the logo's place and size vary between tag designs."""
    ys, xs = np.nonzero(mask)
    if len(xs) == 0:
        return None
    x0, x1, y0, y1 = xs.min(), xs.max() + 1, ys.min(), ys.max() + 1
    region = gray[y0:y1, x0:x1]
    best = None
    for deg, (code, up) in ROTATIONS.items():
        turned = template if code is None else cv2.rotate(template, code)
        for scale in np.linspace(0.6, 1.8, 13):
            t = cv2.resize(turned, None, fx=scale, fy=scale, interpolation=cv2.INTER_AREA)
            if t.shape[0] >= region.shape[0] or t.shape[1] >= region.shape[1] or min(t.shape) < 8:
                continue
            res = cv2.matchTemplate(region, t, cv2.TM_CCOEFF_NORMED)
            _, score, _, at = cv2.minMaxLoc(res)
            if best is None or score > best["score"]:
                best = {"rotation": deg, "score": float(score), "up": up,
                        "box": [int(x0 + at[0]), int(y0 + at[1]), int(t.shape[1]), int(t.shape[0])]}
    return best


def ink_strokes(lightness, mask, px_per_mm, exclude=None):
    """Dark marks inside mask: (long side, short side) in mm for each one."""
    if not mask.any():
        return []
    paper = np.median(lightness[mask > 0])
    dark = ((lightness < paper * 0.6) & (mask > 0)).astype(np.uint8) * 255
    if exclude is not None:
        x, y, w, h = exclude
        pad = int(2 * px_per_mm)
        dark[max(0, y - pad):y + h + pad, max(0, x - pad):x + w + pad] = 0
    dark = cv2.morphologyEx(dark, cv2.MORPH_CLOSE, np.ones((3, 3), np.uint8))
    n, labels, stats, _ = cv2.connectedComponentsWithStats(dark)
    out = []
    for i in range(1, n):
        if stats[i, cv2.CC_STAT_AREA] < 4:
            continue
        pts = np.column_stack(np.nonzero(labels == i)[::-1]).astype(np.float32)
        (_, _), (a, b), _ = cv2.minAreaRect(pts)
        out.append((max(a, b) / px_per_mm, min(a, b) / px_per_mm))
    return out


def find_sheets(img, template):
    lab = cv2.cvtColor(img, cv2.COLOR_BGR2LAB).astype(float)
    lightness = lab[..., 0]
    gray = cv2.cvtColor(img, cv2.COLOR_BGR2GRAY)
    hull, inside = mat_region(lab)
    if hull is None:
        return {"sheets": [], "mat_px": []}
    bg = mat_background(lightness, inside)
    bright = (((lightness - bg) > BRIGHT_ABOVE_MAT) & (inside > 0)).astype(np.uint8) * 255
    bright = cv2.morphologyEx(bright, cv2.MORPH_CLOSE, np.ones((3, 3), np.uint8))
    # Fill each outline (a tag's label is darker than its border), then open with
    # a big kernel: solid sheets survive, thin lines touching them don't.
    contours, _ = cv2.findContours(bright, cv2.RETR_EXTERNAL, cv2.CHAIN_APPROX_SIMPLE)
    solid = np.zeros_like(bright)
    cv2.drawContours(solid, contours, -1, 255, -1)
    solid = cv2.morphologyEx(solid, cv2.MORPH_OPEN, np.ones((21, 21), np.uint8))
    # Every boundary pixel is kept: the corner fit needs points along each side.
    contours, _ = cv2.findContours(solid, cv2.RETR_EXTERNAL, cv2.CHAIN_APPROX_NONE)

    sheets = []
    for c in contours:
        area = cv2.contourArea(c)
        if area < MIN_SHEET_PX:
            continue
        (_, _), (rw, rh), _ = cv2.minAreaRect(c)
        if area / (rw * rh) < MIN_RECT_FILL:
            continue
        filled = np.zeros(solid.shape, np.uint8)
        cv2.drawContours(filled, [c], -1, 255, -1)
        core = cv2.erode(filled, np.ones((31, 31), np.uint8))
        edge = filled & ~cv2.erode(filled, np.ones((7, 7), np.uint8))
        if not core.any() or not edge.any():
            continue
        ab = lab[..., 1:]
        shift = float(np.linalg.norm(np.median(ab[core > 0], 0) - np.median(ab[edge > 0], 0)))
        kind = "name_tag" if shift >= TAG_COLOUR_SHIFT else "card"
        corners = quad_corners(c)
        long_px, short_px = max(rw, rh), min(rw, rh)
        nominal = TAG_MM if kind == "name_tag" else CARD_MM
        px_per_mm = (long_px / max(nominal) + short_px / min(nominal)) / 2
        sheet = {"kind": kind, "colour_shift": round(shift, 1), "size_px": [round(short_px, 1), round(long_px, 1)]}
        # Keep ink checks off the sheet's edge, where the border meets the mat.
        interior = cv2.erode(filled, np.ones((int(3 * px_per_mm) | 1,) * 2, np.uint8))
        if kind == "name_tag":
            logo = find_logo(gray, interior, template)
            if logo and logo["score"] >= LOGO_MIN_SCORE:
                corners = reading_order(corners, np.array(logo["up"]))
                sheet["logo"] = {"rotation": logo["rotation"], "score": round(logo["score"], 2), "box_px": logo["box"]}
                exclude = logo["box"]
            else:
                # Without the logo, which way up is unknown; keep it landscape.
                corners = start_at(portrait_order(corners), 1)
                sheet["logo"] = None
                exclude = None
            marks = ink_strokes(lightness, interior, px_per_mm, exclude)
            tall = [m for m in marks if m[1] >= TAG_HANDWRITING_MM]
            sheet["handwriting"] = len(tall)
            sheet["used"] = len(tall) > 0
        else:
            corners = portrait_order(corners)
            marks = ink_strokes(lightness, interior, px_per_mm)
            strokes = [m for m in marks if m[0] >= CARD_STROKE_MM]
            sheet["strokes"] = len(strokes)
            sheet["used"] = len(strokes) >= USED_CARD_STROKES
        sheet["corners_px"] = [[round(float(x), 2), round(float(y), 2)] for x, y in corners]
        sheets.append(sheet)
    return {"sheets": sheets, "mat_px": hull.reshape(-1, 2).tolist()}


def find_crosses(img, min_px=8, max_px=80):
    """Centres of drawn calibration crosses: thin dark marks about as wide as
    they are tall that fill little of their bounding box."""
    gray = cv2.cvtColor(img, cv2.COLOR_BGR2GRAY)
    local = cv2.medianBlur(gray, 31).astype(float)
    dark = ((gray.astype(float) < local * 0.65) & (local > 120)).astype(np.uint8) * 255
    dark = cv2.morphologyEx(dark, cv2.MORPH_CLOSE, np.ones((3, 3), np.uint8))
    n, labels, stats, centroids = cv2.connectedComponentsWithStats(dark)
    out = []
    for i in range(1, n):
        x, y, w, h, area = stats[i]
        if not (min_px <= w <= max_px and min_px <= h <= max_px):
            continue
        if not 0.6 <= w / h <= 1.6 or area / (w * h) > 0.5:
            continue
        # A plus sign's arms run through its middle row and column.
        mid_row = labels[y + h // 2, x:x + w] == i
        mid_col = labels[y:y + h, x + w // 2] == i
        if mid_row.mean() < 0.5 or mid_col.mean() < 0.5:
            continue
        out.append([round(float(centroids[i][0]), 2), round(float(centroids[i][1]), 2)])
    return out


def fit_homography(image_points, arm_points, tol_px=6.0):
    """Match each arm point to the cross it drew and fit image -> arm.

    Which cross is which isn't known, and a symmetric pattern would match
    itself turned or mirrored, so every assignment of the four most spread arm
    points to four detected crosses is tried; the one that explains the most
    other crosses wins.
    """
    img = np.asarray(image_points, float)
    arm = np.asarray(arm_points, float)
    if len(arm) < 4 or len(img) < 4:
        raise SystemExit("need at least four crosses and four arm points")
    anchors = max(itertools.combinations(range(len(arm)), 4),
                  key=lambda q: cv2.contourArea(cv2.convexHull(arm[list(q)].astype(np.float32))))
    src = arm[list(anchors)].astype(np.float32)
    best = None
    for perm in itertools.permutations(range(len(img)), 4):
        dst = img[list(perm)].astype(np.float32)
        try:
            h = cv2.getPerspectiveTransform(src, dst)
        except cv2.error:
            continue
        projected = cv2.perspectiveTransform(arm.reshape(-1, 1, 2), h).reshape(-1, 2)
        if not np.all(np.isfinite(projected)):
            continue
        d = np.linalg.norm(projected[:, None, :] - img[None, :, :], axis=2)
        match = d.argmin(1)
        ok = d[np.arange(len(arm)), match] < tol_px
        # One cross per arm point.
        if len(set(match[ok])) != ok.sum():
            continue
        score = (ok.sum(), -d[np.arange(len(arm)), match][ok].sum())
        if best is None or score > best[0]:
            best = (score, match, ok)
    if best is None or best[0][0] < 4:
        raise SystemExit("could not match the crosses to the arm points")
    _, match, ok = best
    pairs_img = img[match[ok]]
    pairs_arm = arm[ok]
    h, _ = cv2.findHomography(pairs_img, pairs_arm, 0)
    mapped = cv2.perspectiveTransform(pairs_img.reshape(-1, 1, 2), h).reshape(-1, 2)
    residual = np.linalg.norm(mapped - pairs_arm, axis=1)
    return {
        "homography": h.tolist(),
        "matched": [{"arm": a.tolist(), "image": i.tolist(), "residual_mm": round(float(r), 3)}
                    for a, i, r in zip(pairs_arm, pairs_img, residual)],
        "unmatched_arm_points": arm[~ok].tolist(),
        "rms_mm": round(float(math.sqrt(np.mean(residual ** 2))), 3),
        "max_mm": round(float(residual.max()), 3),
    }


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = p.add_subparsers(dest="cmd", required=True)
    s = sub.add_parser("sheets")
    s.add_argument("--logo", default=str(DEFAULT_LOGO))
    c = sub.add_parser("crosses")
    c.add_argument("--min-px", type=int, default=8)
    c.add_argument("--max-px", type=int, default=80)
    sub.add_parser("fit")
    a = p.parse_args()

    data = sys.stdin.buffer.read()
    if a.cmd == "sheets":
        template = cv2.imread(a.logo, cv2.IMREAD_GRAYSCALE)
        if template is None:
            raise SystemExit(f"could not read the logo template {a.logo}")
        out = find_sheets(read_image(data), template)
    elif a.cmd == "crosses":
        out = {"crosses": find_crosses(read_image(data), a.min_px, a.max_px)}
    else:
        req = json.loads(data)
        out = fit_homography(req["image_points"], req["arm_points"])
    json.dump(out, sys.stdout)


if __name__ == "__main__":
    main()
