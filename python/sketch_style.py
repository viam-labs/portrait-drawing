"""Sketch style: a portrait drawn the way a person would sketch it, from a photo.

The photo is cropped to the face and turned into a cartoon (AnimeGAN face_paint), and
lines are extracted from the cartoon with the same informative-drawings model the default
style uses. Those lines carry the likeness (nose, cheekbones, the area around the mouth),
so they are kept, but cleaned: traced along their centre, joined where they continue,
and held to a minimum length that is stricter where short fragments read as clutter
(under the eyes, on clothing patterns). Eyes, lips, brows, glasses, a cap and its
lettering, and facial hair are drawn from MediaPipe face landmarks and segmentation,
because extracted lines get those wrong in ways people notice first.

Developed against real lobby photos and hand tracings for the Build on Viam reception
robot; see https://github.com/viam-labs/portrait-drawing for the history.
"""

import pathlib

import cv2
import mediapipe as mp
import numpy as np
import onnxruntime as ort
from mediapipe.tasks.python import BaseOptions, vision
from scipy.interpolate import splprep, splev
from scipy.spatial import cKDTree
from skimage.morphology import remove_small_holes, skeletonize

import image_to_polylines as itp

_MODELS = pathlib.Path(__file__).resolve().parent.parent / "models"
_SESSIONS = {}


def _session(filename):
    if filename not in _SESSIONS:
        path = _MODELS / filename
        if not path.exists():
            raise RuntimeError(f"sketch style: model missing at {path}; first_run.sh downloads it")
        _SESSIONS[filename] = ort.InferenceSession(str(path), providers=["CPUExecutionProvider"])
    return _SESSIONS[filename]


def anime(img):
    """Cartoonize: smooths skin texture and lighting so the line model draws shapes, not pores."""
    h, w = img.shape[:2]
    s = 512 / max(h, w)
    nw, nh = int(w * s) // 32 * 32, int(h * s) // 32 * 32
    x = cv2.cvtColor(cv2.resize(img, (nw, nh), interpolation=cv2.INTER_AREA), cv2.COLOR_BGR2RGB) / 127.5 - 1
    sess = _session("face_paint_512_v2.onnx")
    y = sess.run(None, {"input_image": x.transpose(2, 0, 1)[None].astype(np.float32)})[0][0]
    y = ((y.transpose(1, 2, 0) + 1) * 127.5).clip(0, 255).astype(np.uint8)
    return cv2.resize(cv2.cvtColor(y, cv2.COLOR_RGB2BGR), (w, h), interpolation=cv2.INTER_CUBIC)


def _resize_to_64(img, short_side):
    """The line model was trained on sides that are multiples of 64; match its preprocessing."""
    h, w = img.shape[:2]
    k = short_side / min(h, w)
    nh, nw = int(np.round(h * k / 64.0)) * 64, int(np.round(w * k / 64.0)) * 64
    return cv2.resize(img, (nw, nh), interpolation=cv2.INTER_LANCZOS4 if k > 1 else cv2.INTER_AREA)


def line_map(img_bgr, kind="realistic", res=768):
    """Dark lines on white, same size as the input. kind is kept for the call sites' benefit;
    only the informative-drawings line model is used."""
    h, w = img_bgr.shape[:2]
    sc = res / max(h, w)
    small = cv2.resize(img_bgr, (int(w * sc) // 8 * 8, int(h * sc) // 8 * 8), interpolation=cv2.INTER_AREA)
    rgb = _resize_to_64(cv2.cvtColor(small, cv2.COLOR_BGR2RGB), min(small.shape[:2]))
    sess = _session("lineart.onnx")
    x = (rgb.astype(np.float32) / 255.0).transpose(2, 0, 1)[None]
    line = sess.run(None, {sess.get_inputs()[0].name: x})[0][0][0]
    line = (line * 255.0).clip(0, 255).astype(np.uint8)
    return cv2.resize(line, (w, h), interpolation=cv2.INTER_AREA)


_LM = None


def landmarks(img):
    global _LM
    if _LM is None:
        _LM = vision.FaceLandmarker.create_from_options(vision.FaceLandmarkerOptions(
            base_options=BaseOptions(model_asset_path=str(_MODELS / "face_landmarker.task")), num_faces=1))
    r = _LM.detect(mp.Image(image_format=mp.ImageFormat.SRGB, data=cv2.cvtColor(img, cv2.COLOR_BGR2RGB)))
    if not r.face_landmarks:
        raise ValueError("sketch style: no face found in the photo")
    h, w = img.shape[:2]
    return np.array([[p.x * w, p.y * h] for p in r.face_landmarks[0]])


_SEG = None


def mp_seg(img):
    global _SEG
    if _SEG is None:
        _SEG = vision.ImageSegmenter.create_from_options(vision.ImageSegmenterOptions(
            base_options=BaseOptions(model_asset_path=str(_MODELS / "selfie_multiclass.tflite")),
            output_category_mask=True, output_confidence_masks=True))
    r = _SEG.segment(mp.Image(image_format=mp.ImageFormat.SRGB, data=cv2.cvtColor(img, cv2.COLOR_BGR2RGB)))
    h, w = img.shape[:2]
    cat = np.squeeze(r.category_mask.numpy_view()).astype(np.uint8)
    hair_conf = np.squeeze(r.confidence_masks[1].numpy_view()).astype(np.float32)
    cat = cv2.resize(cat, (w, h), interpolation=cv2.INTER_NEAREST)
    hair_conf = cv2.resize(hair_conf, (w, h), interpolation=cv2.INTER_LINEAR)
    return cat, hair_conf


def largest_attached(mask, anchor):
    n, lbl = cv2.connectedComponents(mask.astype(np.uint8))
    near = cv2.dilate(anchor.astype(np.uint8), np.ones((15, 15), np.uint8)) > 0
    keep = set(np.unique(lbl[near])) - {0}
    return np.isin(lbl, list(keep))


def segments(img):
    cat, _ = mp_seg(img)
    face = cat == 3
    hair = largest_attached(cat == 1, face)
    head = largest_attached(hair | face, face)
    return cat, face, hair, head


L_BROW_U, L_BROW_L = [70,63,105,66,107], [46,53,52,65,55]


R_BROW_U, R_BROW_L = [300,293,334,296,336], [276,283,282,295,285]


def headwear(cat, L, fw):
    acc = (cat == 5).astype(np.uint8)
    n, lbl, st, cen = cv2.connectedComponentsWithStats(acc)
    brow_y = min(L[L_BROW_U][:, 1].min(), L[R_BROW_U][:, 1].min())
    for i in range(1, n):
        if st[i, cv2.CC_STAT_AREA] > 0.25 * fw * fw and cen[i][1] < brow_y:
            return lbl == i
    return None


def poly_mask(shape, pts, grow):
    m = np.zeros(shape, np.uint8); cv2.fillPoly(m, [cv2.convexHull(pts.astype(np.int32))], 1)
    return cv2.dilate(m, np.ones((grow | 1, grow | 1), np.uint8)) > 0


def link_endpoints(lines, max_gap, max_angle_deg=30):
    """Join strokes whose ends meet and continue in the same direction (greedy, closest pairs first)."""
    lines = [l for l in lines if len(l) > 2]
    cos_t = np.cos(np.deg2rad(max_angle_deg))
    def ends(l):  # (point, outward tangent) for both ends
        a = l[0] - l[min(3, len(l) - 1)]; b = l[-1] - l[max(-4, -len(l))]
        return [(l[0], a / (np.linalg.norm(a) + 1e-9)), (l[-1], b / (np.linalg.norm(b) + 1e-9))]
    while True:
        E = [(i, e, *ends(l)[e]) for i, l in enumerate(lines) for e in (0, 1)]
        if len(E) < 4: break
        tree = cKDTree(np.array([x[2] for x in E]))
        pairs = sorted(tree.query_pairs(max_gap), key=lambda ij: np.linalg.norm(E[ij[0]][2] - E[ij[1]][2]))
        merged = False
        for a, b in pairs:
            ia, ea, pa, ta = E[a]; ib, eb, pb, tb = E[b]
            if ia == ib or ta @ tb > -cos_t: continue  # ends must face each other
            gap = pb - pa; d = np.linalg.norm(gap)
            if d > 1 and (gap / d) @ ta < cos_t: continue
            la = lines[ia] if ea == 1 else lines[ia][::-1]
            lb = lines[ib] if eb == 0 else lines[ib][::-1]
            lines[ia] = np.vstack([la, lb]); lines.pop(ib); merged = True
            break
        if not merged: break
    return lines


def biggest_contour(mask):
    cs, _ = cv2.findContours(mask.astype(np.uint8), cv2.RETR_EXTERNAL, cv2.CHAIN_APPROX_NONE)
    return max(cs, key=cv2.contourArea)[:, 0, :].astype(float) if cs else None


def gray(img): return cv2.cvtColor(img, cv2.COLOR_BGR2GRAY).astype(np.float32)


def curliness(img, hair, face_w):
    """0 = straight and sleek, 1 = very curly. Two cues: how ragged the hair outline is,
    and how consistently the strands point one way."""
    m = hair.astype(np.uint8)
    cs, _ = cv2.findContours(m, cv2.RETR_EXTERNAL, cv2.CHAIN_APPROX_NONE)
    if not cs: return 0.0, {}
    c = max(cs, key=cv2.contourArea)
    sm = (cv2.GaussianBlur(m.astype(np.float32), (0, 0), 0.08 * face_w) > 0.5).astype(np.uint8)
    cs2, _ = cv2.findContours(sm, cv2.RETR_EXTERNAL, cv2.CHAIN_APPROX_NONE)
    rough = cv2.arcLength(c, True) / max(1.0, cv2.arcLength(max(cs2, key=cv2.contourArea), True)) if cs2 else 1.0
    g = cv2.GaussianBlur(cv2.cvtColor(img, cv2.COLOR_BGR2GRAY).astype(np.float32), (0, 0), 1.5)
    gx, gy = cv2.Sobel(g, cv2.CV_32F, 1, 0), cv2.Sobel(g, cv2.CV_32F, 0, 1)
    k = max(3.0, 0.05 * face_w)
    jxx, jyy, jxy = [cv2.GaussianBlur(a, (0, 0), k) for a in (gx * gx, gy * gy, gx * gy)]
    coh = np.sqrt((jxx - jyy) ** 2 + 4 * jxy ** 2) / (jxx + jyy + 1e-6)
    inner = cv2.erode(m, np.ones((9, 9), np.uint8)) > 0
    coherence = float(np.median(coh[inner])) if inner.any() else 1.0
    score = np.clip(0.5 * (rough - 1.05) / 0.25 + 0.5 * (0.55 - coherence) / 0.3, 0, 1)
    return float(score), dict(roughness=round(rough, 3), coherence=round(coherence, 3))


def hair_type(img, hair, face, fw):
    area = hair.sum()
    if area < 0.15 * fw * fw: return "none", 0.0
    c = biggest_contour(hair)
    thickness = area / max(1.0, cv2.arcLength(c.astype(np.float32).reshape(-1, 1, 2), True) / 2)
    g = gray(img)
    contrast = float(np.clip(abs(1 - np.median(g[hair]) / (np.median(g[face]) + 1e-6)) * 1.6, 0, 1))
    if thickness < 0.17 * fw: return "short", contrast
    curly, _ = curliness(img, hair, fw)
    if curly >= 0.5: return "curly", contrast
    if curly >= 0.15: return "wavy", contrast
    return "straight", contrast


def full_beard(img, cat, L, fw):
    """A full beard: the lower face is clearly darker than the forehead on BOTH sides of the mouth.
    Returns the beard mask, or None. Stubble and side shadows stay below the bar on at least one side."""
    g = gray(img); face = cat == 3
    fh = np.zeros(g.shape, bool); x, y = L[151].astype(int); r = int(0.06 * fw); fh[max(0, y - r):y + r, max(0, x - r):x + r] = True
    ref = np.median(g[fh & face]) if (fh & face).any() else np.median(g[face])
    lips_m = np.zeros(g.shape, np.uint8)
    cv2.fillPoly(lips_m, [L[[61,185,40,39,37,0,267,269,270,409,291,375,321,405,314,17,84,181,91,146]].astype(np.int32)], 1)
    zone = face & ~(cv2.dilate(lips_m, np.ones((5, 5), np.uint8)) > 0); zone[:int(L[2, 1]), :] = False
    dark = (g < 0.6 * ref) & zone
    left = zone.copy(); left[:, int(L[61, 0]):] = False
    right = zone.copy(); right[:, :int(L[291, 0])] = False
    if not left.any() or not right.any() or min(dark[left].mean(), dark[right].mean()) < 0.25: return None
    m = cv2.morphologyEx(dark.astype(np.uint8), cv2.MORPH_CLOSE, np.ones((int(0.06 * fw) | 1,) * 2, np.uint8))
    m = cv2.morphologyEx(m, cv2.MORPH_OPEN, np.ones((5, 5), np.uint8))
    n, lbl, st, _ = cv2.connectedComponentsWithStats(m)
    if n <= 1: return None
    return lbl == 1 + np.argmax(st[1:, cv2.CC_STAT_AREA])


L_UP, L_LO = [33,246,161,160,159,158,157,173,133], [33,7,163,144,145,153,154,155,133]


R_UP, R_LO = [263,466,388,387,386,385,384,398,362], [263,249,390,373,374,380,381,382,362]


def smooth(pts, s=None, n=None, closed=False):
    pts = np.asarray(pts, float)
    keep = np.r_[True, np.linalg.norm(np.diff(pts, axis=0), axis=1) > 1e-6]
    pts = pts[keep]
    if len(pts) < 4: return pts
    k = 3
    tck, _ = splprep(pts.T, s=len(pts) * 2.0 if s is None else s, k=k, per=closed)
    n = n or max(20, len(pts) * 3)
    return np.array(splev(np.linspace(0, 1, n), tck)).T


def facial_hair(img, cat, L, fw):
    g = gray(img)
    face = cat == 3
    cheeks = np.zeros(g.shape, bool)
    for idx in (50, 280):  # cheek centres
        x, y = L[idx].astype(int); r = int(0.07 * fw)
        cheeks[max(0, y - r):y + r, max(0, x - r):x + r] = True
    ref = np.median(g[cheeks & face]) if (cheeks & face).any() else np.median(g[face])
    lips_m = np.zeros(g.shape, np.uint8)
    cv2.fillPoly(lips_m, [L[[61,185,40,39,37,0,267,269,270,409,291,375,321,405,314,17,84,181,91,146]].astype(np.int32)], 1)
    lips_m = cv2.dilate(lips_m, np.ones((5, 5), np.uint8)) > 0
    dark = (g < 0.6 * ref) & face & ~lips_m
    dark = cv2.morphologyEx(dark.astype(np.uint8), cv2.MORPH_OPEN, np.ones((3, 3), np.uint8))
    dark = cv2.morphologyEx(dark, cv2.MORPH_CLOSE, np.ones((7, 7), np.uint8)) > 0
    out, found = [], []
    nose_y, lip_top, lip_bot, chin_y = L[2, 1], L[0, 1], L[17, 1], L[152, 1]
    mouth_l, mouth_r = L[61, 0], L[291, 0]
    zones = {
        "moustache": (slice(int(nose_y), int(lip_top + 0.01 * fw)), slice(int(mouth_l - 0.05 * fw), int(mouth_r + 0.05 * fw))),
        "chin": (slice(int(lip_bot), int(chin_y + 0.02 * fw)), slice(int(mouth_l - 0.12 * fw), int(mouth_r + 0.12 * fw))),
    }
    for name, (ys, xs) in zones.items():
        z = np.zeros(g.shape, bool); z[ys, xs] = True
        m = dark & z
        zone_area = max(1, z[ys, xs].size)
        if m.sum() < 0.08 * zone_area: continue  # too little: leave it out (flattering)
        mid = 0.5 * (L[61, 0] + L[291, 0])
        yy, xx = np.nonzero(m)
        left, right = (xx < mid).sum(), (xx >= mid).sum()
        if min(left, right) < 0.45 * max(left, right): continue  # lopsided: stubble patches or a side-light shadow, not a moustache or goatee
        n, lbl, st, _ = cv2.connectedComponentsWithStats(m.astype(np.uint8))
        keep = np.zeros_like(m)
        for i in range(1, n):
            if st[i, cv2.CC_STAT_AREA] > 0.0015 * fw * fw: keep |= lbl == i
        if name == "moustache":  # a thin moustache breaks into pieces: join them into one shape
            keep = cv2.morphologyEx(keep.astype(np.uint8), cv2.MORPH_CLOSE, np.ones((3, int(0.06 * fw) | 1), np.uint8)) > 0
        cs, _ = cv2.findContours(keep.astype(np.uint8), cv2.RETR_EXTERNAL, cv2.CHAIN_APPROX_NONE)
        cs = [c[:, 0, :].astype(float) for c in cs if len(c) >= 12 and cv2.contourArea(c) > 0.0015 * fw * fw]
        if not cs: continue
        found.append(name)
        out += [smooth(np.vstack([c, c[:1]]), s=len(c) * 2, n=100) for c in cs]
        x0, y0, w, h = cv2.boundingRect(keep.astype(np.uint8))
        for f in np.linspace(0.25, 0.75, 3):  # a few hair strokes inside
            x = x0 + f * w
            col = np.nonzero(keep[:, int(x)])[0]
            if len(col) > 3:
                a, b = col.min() + 0.25 * (col.max() - col.min()), col.min() + 0.75 * (col.max() - col.min())
                out.append(np.array([[x, a], [x + (0.01 * fw if name == 'chin' else 0), b]]))
    return out, found


def wears_glasses(cat, L):
    acc = cat == 5
    ew = np.linalg.norm(L[33] - L[133])
    hits = []
    for o, i, brow_lo in ((33, 133, L_BROW_L), (263, 362, R_BROW_L)):
        c = 0.5 * (L[o] + L[i])
        y0 = int(L[brow_lo][:, 1].max() + 0.1 * ew); y1 = int(c[1] + 0.8 * ew)
        x0, x1 = int(c[0] - 1.1 * ew), int(c[0] + 1.1 * ew)
        hits.append(acc[max(0, y0):y1, max(0, x0):x1].sum())
    bx0, bx1 = int(min(L[133, 0], L[362, 0])), int(max(L[133, 0], L[362, 0]))
    by = int(0.5 * (L[133, 1] + L[362, 1]))
    bridge = acc[max(0, by - int(0.6 * ew)):by + int(0.3 * ew), bx0:bx1].sum()
    return min(hits) > 0.05 * ew * ew


LIPS = [61,185,40,39,37,0,267,269,270,409,291,375,321,405,314,17,84,181,91,146]


def trace_skeleton(sk, min_len):
    """Walk a 1-px skeleton into polylines (one pass per branch, not around both sides like findContours)."""
    sk = sk.copy().astype(bool)
    h, w = sk.shape
    nb = [(-1, -1), (-1, 0), (-1, 1), (0, -1), (0, 1), (1, -1), (1, 0), (1, 1)]
    def neighbours(y, x):
        return [(y + dy, x + dx) for dy, dx in nb if 0 <= y + dy < h and 0 <= x + dx < w and sk[y + dy, x + dx]]
    ys, xs = np.nonzero(sk)
    deg = {(y, x): len(neighbours(y, x)) for y, x in zip(ys, xs)}
    starts = [p for p, d in deg.items() if d == 1] + [p for p, d in deg.items() if d != 1]
    out = []
    for p in starts:
        if not sk[p]: continue
        line = [p]; sk[p] = False
        while True:
            n = neighbours(*line[-1])
            if not n: break
            nxt = n[0]; sk[nxt] = False; line.append(nxt)
        pts = np.array([[x, y] for y, x in line], float)
        if len(pts) > 1 and np.linalg.norm(np.diff(pts, axis=0), axis=1).sum() >= min_len:
            out.append(pts)
    return out


def lips(L):
    up = smooth(L[[61,185,40,39,37,0,267,269,270,409,291]], s=3, n=70)
    lo = smooth(L[[61,146,91,181,84,17,314,405,321,375,291]], s=4, n=70)
    inner_up = smooth(L[[78,191,80,81,82,13,312,311,310,415,308]], s=3, n=70)
    inner_lo = smooth(L[[78,95,88,178,87,14,317,402,318,324,308]], s=3, n=70)
    mouth_w = np.linalg.norm(L[61] - L[291])
    if np.linalg.norm(L[13] - L[14]) > 0.08 * mouth_w:  # open: a smile with teeth
        return [up, inner_up, np.vstack([inner_lo[::-1]]), lo[6:-6]]
    return [up, inner_up, lo[6:-6]]


def dot(c, r):
    t = np.linspace(0, 4 * np.pi, 18); rr = np.linspace(r, 0.3, 18)
    return c + np.c_[rr * np.cos(t), rr * np.sin(t)]


def arc_segments(pts, keep):
    segs, cur = [], []
    for p, ok in zip(pts, keep):
        if ok: cur.append(p)
        elif cur: segs.append(np.array(cur)); cur = []
    if cur: segs.append(np.array(cur))
    return segs


def eye_v7(L, up, lo, c, ring, a, b, up_mid, lo_mid, fw, iris="ring+pupil"):
    u, l = smooth(L[up], s=2, n=50), smooth(L[lo], s=2, n=50)
    openness = np.linalg.norm(L[up_mid] - L[lo_mid]) / (np.linalg.norm(L[a] - L[b]) + 1e-6)
    if openness < 0.2:  # smiling / closed: one happy arc
        t = np.linspace(0, 1, 50); sag = 0.18 * np.linalg.norm(u[0] - u[-1])
        return [(1 - t)[:, None] * u[0] + t[:, None] * u[-1] + np.c_[np.zeros(50), -sag * np.sin(np.pi * t)]]
    out = [np.vstack([u, l[::-1]])]  # open almond: upper and lower lid as one closed line
    cc = L[c]; r = 0.85 * np.mean(np.linalg.norm(L[ring] - L[c], axis=1))
    oU, oL = np.argsort(u[:, 0]), np.argsort(l[:, 0])
    t = np.linspace(0, 2 * np.pi, 60); circ = cc + r * np.c_[np.cos(t), np.sin(t)]
    keep = (circ[:, 1] > np.interp(circ[:, 0], u[oU, 0], u[oU, 1]) + 0.5) & (circ[:, 1] < np.interp(circ[:, 0], l[oL, 0], l[oL, 1]) - 0.5)
    if iris in ("ring", "ring+pupil"):
        out += [s for s in arc_segments(circ, keep) if len(s) > 3]  # iris ring, tucked between the lids
    if iris == "ring+pupil":
        out.append(dot(cc, max(1.0, 0.35 * r)))  # small pupil
    return out


def glasses_v7(cat, L, fw):
    """Lens shape fitted to the frames, drawn as a thick double rim."""
    acc = (cat == 5).astype(np.uint8)
    out, lenses = [], []
    for o, i in ((33, 133), (263, 362)):
        ew = np.linalg.norm(L[o] - L[i]); c = 0.5 * (L[o] + L[i])
        x0, x1 = int(c[0] - 1.3 * ew), int(c[0] + 1.3 * ew); y0, y1 = int(c[1] - 1.0 * ew), int(c[1] + 1.1 * ew)
        win = acc[max(0, y0):y1, max(0, x0):x1]
        ys, xs = np.nonzero(win)
        if len(xs) >= 30:
            pts = np.c_[xs + max(0, x0), ys + max(0, y0)].astype(np.float32)
            # keep the ring around this eye, not the arms of the frames
            d = np.linalg.norm(pts - c, axis=1); pts = pts[d < 1.25 * ew]
        if len(xs) < 30 or len(pts) < 20:
            (cx, cy), (a, b) = c, (1.0 * ew, 0.85 * ew)
        else:
            (cx, cy), (A, B), ang = cv2.fitEllipse(pts)
            a, b = np.clip(0.5 * max(A, B), 0.8 * ew, 1.25 * ew), np.clip(0.5 * min(A, B), 0.65 * ew, 1.1 * ew)
        lenses.append((np.array([cx, cy]), a, b))
        t = np.linspace(0, 2 * np.pi, 90)
        for k in (1.0, 0.88):  # double rim reads as a real frame
            out.append(np.array([cx, cy]) + np.c_[k * a * np.cos(t), k * b * np.sin(t)])
    (c1, a1, b1), (c2, a2, b2) = sorted(lenses, key=lambda t: t[0][0])
    p, q = c1 + [a1, -0.2 * b1], c2 + [-a2, -0.2 * b2]
    out.append(smooth(np.array([p, 0.5 * (p + q) + [0, -0.18 * b1], q]), s=0, n=20))
    for c, a, b, sg in ((c1, a1, b1, -1), (c2, a2, b2, 1)):
        s0 = c + [sg * a, -0.25 * b]; out.append(np.array([s0, s0 + [sg * 0.35 * a, -0.05 * b]]))
    return out


def brow_outline(L, up, lo):
    u, l = smooth(L[up], s=4, n=40), smooth(L[lo], s=4, n=40)
    return [np.vstack([u, l[::-1], u[:1]])]


def clip_out(strokes, mask, pad=3):
    """Drop the parts of strokes that fall under a mask (e.g. features hidden by a cap brim)."""
    m = cv2.dilate(mask.astype(np.uint8), np.ones((pad * 2 + 1, pad * 2 + 1), np.uint8)) > 0
    h, w = m.shape
    out = []
    for p in strokes:
        keep = np.array([not m[min(max(int(y), 0), h - 1), min(max(int(x), 0), w - 1)] for x, y in p])
        out += [q for q in arc_segments(p, keep) if len(q) > 1]
    return out


def style_hybrid(img, cart, kind="anime", source="cartoon", low=8, high=24, min_len_fw=0.07, brows="landmark"):
    L = landmarks(img)
    cat, face, hair, head = segments(img)
    fw = np.linalg.norm(L[234] - L[454])
    H, W = cat.shape
    hat = headwear(cat, L, fw)
    # where lines may appear: the person, cut off a little below the chin, and away from the frame edges
    person = (cat > 0)
    if hat is not None: person |= hat
    person[int(L[152, 1] + 0.75 * fw):, :] = False
    person = cv2.dilate(person.astype(np.uint8), np.ones((7, 7), np.uint8)) > 0
    m = max(3, int(0.04 * fw)); person[:, :m] = person[:, -m:] = person[:m, :] = False
    # features we draw ourselves get a clean hole in the extracted lines
    hole = poly_mask((H, W), L[L_UP + L_LO], int(0.06 * fw)) | poly_mask((H, W), L[R_UP + R_LO], int(0.06 * fw))
    hole |= poly_mask((H, W), L[[61,185,40,39,37,0,267,269,270,409,291,375,321,405,314,17,84,181,91,146]], int(0.03 * fw))
    if brows == "landmark":
        hole |= poly_mask((H, W), L[L_BROW_U + L_BROW_L], int(0.04 * fw)) | poly_mask((H, W), L[R_BROW_U + R_BROW_L], int(0.04 * fw))
    roi = person & ~hole
    resp = line_map(cart if source == "cartoon" else img, kind)
    ridge = itp.ridge_centerlines(resp, 1.4, low, high)
    ridge[~roi] = 0
    ridge = itp.prune_spurs(ridge, 8)
    cs, _ = cv2.findContours(ridge, cv2.RETR_LIST, cv2.CHAIN_APPROX_NONE)
    s = []
    for c in cs:
        if cv2.arcLength(c, False) < 2 * min_len_fw * fw: continue  # contours of 1-px lines run both ways
        c = cv2.approxPolyDP(c, 1.2, False)[:, 0, :].astype(float)
        s.append(c)
    # anchors drawn from landmarks
    for up, lo, ci, ring, a, b, um, lm in ((L_UP, L_LO, 468, [469, 470, 471, 472], 33, 133, 159, 145),
                                          (R_UP, R_LO, 473, [474, 475, 476, 477], 263, 362, 386, 374)):
        s += eye_v7(L, up, lo, ci, ring, a, b, um, lm, fw, "ring")
    s += lips(L)
    if brows == "landmark": s += brow_outline(L, L_BROW_U, L_BROW_L) + brow_outline(L, R_BROW_U, R_BROW_L)
    c2 = cat.copy()
    if hat is not None: c2[hat] = 0
    if wears_glasses(c2, L): s += glasses_v7(c2, L, fw)
    return [p for p in s if len(p) > 1]


def length(p): return float(np.linalg.norm(np.diff(p, axis=0), axis=1).sum())


def thin_parallel(lines, fw, spacing=0.03):
    """Keep the longest dash of each lock; drop shorter dashes running right beside a kept one."""
    from scipy.spatial import cKDTree
    out, tree = [], None
    for p in sorted(lines, key=lambda q: -length(q)):
        if tree is not None and np.mean(tree.query(p)[0] < spacing * fw) > 0.5: continue
        out.append(p); tree = cKDTree(np.vstack(out))
    return out


def moustache_dashes(L, fw):
    lip = smooth(L[[61,185,40,39,37,0,267,269,270,409,291]], s=3, n=60)
    gap = L[0, 1] - L[2, 1]
    row = lip[6:-6] + [0, -0.4 * gap]
    out = []
    for i in np.linspace(0, len(row) - 1, 9).astype(int):
        p = row[i]; out.append(np.array([p + [0, -0.12 * gap], p + [0.02 * fw, 0.12 * gap]]))
    return out


def goatee_shape(L, fw):
    """Short strokes fanned along the chin: reads as hair without a closed outline."""
    lip_c, chin = L[17], L[152]
    w = 0.32 * np.linalg.norm(L[61] - L[291])
    out = []
    for a in np.linspace(-1, 1, 7):
        top = lip_c + [a * w, 0.25 * (chin[1] - lip_c[1]) + 0.12 * abs(a) * (chin[1] - lip_c[1])]
        bot = top + [0.15 * a * w, 0.55 * (chin[1] - lip_c[1]) * (1 - 0.35 * abs(a))]
        out.append(np.array([top, bot]))
    return out


def cap_text(img, hat, fw):
    g = gray(img)
    inner = cv2.erode(hat.astype(np.uint8), np.ones((int(0.05 * fw) | 1,) * 2, np.uint8)) > 0
    if not inner.any(): return []
    bright = (g > max(150, np.percentile(g[inner], 95))) & inner
    bright = cv2.morphologyEx(bright.astype(np.uint8), cv2.MORPH_OPEN, np.ones((2, 2), np.uint8))
    n, lbl, st, cen = cv2.connectedComponentsWithStats(bright)
    out = []
    for i in range(1, n):
        if st[i, cv2.CC_STAT_AREA] < 12 or st[i, cv2.CC_STAT_AREA] > 0.05 * fw * fw: continue
        if st[i, cv2.CC_STAT_HEIGHT] > 0.25 * fw: continue  # a highlight streak, not a letter
        cs, _ = cv2.findContours((lbl == i).astype(np.uint8), cv2.RETR_EXTERNAL, cv2.CHAIN_APPROX_NONE)
        c = max(cs, key=cv2.contourArea)
        c = cv2.approxPolyDP(c, 1.0, True)[:, 0, :].astype(float)
        if len(c) >= 3: out.append(np.vstack([c, c[:1]]))
    ys = [o[:, 1].mean() for o in out]
    if len(out) >= 3:  # keep the row of letters: components roughly on one line
        med = np.median(ys); out = [o for o, y in zip(out, ys) if abs(y - med) < 0.06 * fw]
    return out if len(out) >= 2 else []


def draw_headwear(hat, fw):
    c = biggest_contour(cv2.morphologyEx(hat.astype(np.uint8), cv2.MORPH_CLOSE, np.ones((9, 9), np.uint8)))
    if c is None: return []
    out = [smooth(np.vstack([c, c[:1]]), s=len(c) * 4, n=200)]
    # seam: a soft curve from the crown down toward the brim, so it reads as a cap rather than a blob
    ys, xs = np.nonzero(hat)
    top = np.array([xs[ys.argmin()], ys.min()], float)
    bottom_y = np.percentile(ys, 70)
    out.append(smooth(np.array([top + [0, 0.05 * fw], [top[0] + 0.02 * fw, 0.5 * (top[1] + bottom_y)], [top[0], bottom_y]]), s=0, n=20))
    return out


def style_v13(img, cart, res=1024, low=8, high=24, min_face=0.06, min_undereye=0.25, min_clothes=0.4, min_hair=0.1, seed=5):
    L = landmarks(img)
    cat, face, hair, head = segments(img)
    fw = np.linalg.norm(L[234] - L[454])
    H, W = cat.shape
    hat = headwear(cat, L, fw)
    person = cat > 0
    if hat is not None: person |= hat
    person[int(L[152, 1] + 0.75 * fw):, :] = False
    person = cv2.dilate(person.astype(np.uint8), np.ones((7, 7), np.uint8)) > 0
    m = max(3, int(0.04 * fw)); person[:, :m] = person[:, -m:] = person[:m, :] = False
    hole = poly_mask((H, W), L[L_UP + L_LO], int(0.06 * fw)) | poly_mask((H, W), L[R_UP + R_LO], int(0.06 * fw))
    hole |= poly_mask((H, W), L[LIPS], int(0.03 * fw))
    hole |= poly_mask((H, W), L[L_BROW_U + L_BROW_L], int(0.04 * fw)) | poly_mask((H, W), L[R_BROW_U + R_BROW_L], int(0.04 * fw))
    if hat is not None: hole |= cv2.erode(hat.astype(np.uint8), np.ones((5, 5), np.uint8)) > 0  # the cap is drawn on its own
    # extraction at higher resolution: jobs are queued, so speed is not the constraint
    resp = line_map(cart, "realistic", res=res)
    ridge = itp.ridge_centerlines(resp, 1.4, low, high) > 0
    ridge &= person & ~hole
    ridge = remove_small_holes(ridge, area_threshold=int((0.03 * fw) ** 2))
    sk = itp.prune_spurs((skeletonize(ridge) * 255).astype(np.uint8), 6) > 0
    lines = link_endpoints(trace_skeleton(sk, 0.02 * fw), 0.03 * fw)
    # zones where short fragments read as clutter
    kind, _ = hair_type(img, hair, face, fw)
    beard = full_beard(img, cat, L, fw)
    beard_zone = cv2.dilate(beard.astype(np.uint8), np.ones((int(0.04 * fw) | 1,) * 2, np.uint8)) > 0 if beard is not None else np.zeros((H, W), bool)
    under_eye = np.zeros((H, W), bool)
    for lo in (L_LO, R_LO):
        x0, x1 = L[lo][:, 0].min(), L[lo][:, 0].max(); y0 = L[lo][:, 1].max()
        under_eye[int(y0):int(y0 + 0.13 * fw), int(x0 - 0.02 * fw):int(x1 + 0.02 * fw)] = True
    clothes = cat == 4
    def min_len(p):
        c = p.mean(0); x, y = min(max(int(c[0]), 0), W - 1), min(max(int(c[1]), 0), H - 1)
        if under_eye[y, x]: return min_undereye * fw
        if clothes[y, x]: return min_clothes * fw
        if beard_zone[y, x]: return 0.02 * fw  # beard texture is short strokes: that is the likeness
        if hair[y, x]: return (0.03 if kind == "curly" else min_hair) * fw
        return min_face * fw
    def in_hair(p):
        c = p.mean(0); return hair[min(max(int(c[1]), 0), H - 1), min(max(int(c[0]), 0), W - 1)]
    if kind in ("straight", "wavy", "short"):
        hair_lines = [p for p in lines if in_hair(p)]
        lines = [p for p in lines if not in_hair(p)] + thin_parallel(hair_lines, fw)
    kept = [smooth(p, s=len(p) * 0.8, n=max(6, len(p) // 2)) for p in lines if length(p) >= min_len(p)]
    if kind == "curly" or beard is not None:
        zone = np.zeros((H, W), bool)
        if kind == "curly": zone |= cv2.dilate(hair.astype(np.uint8), np.ones((5, 5), np.uint8)) > 0
        zone |= beard_zone
        def inside(p):
            c = p.mean(0); return zone[min(max(int(c[1]), 0), H - 1), min(max(int(c[0]), 0), W - 1)]
        v10 = style_hybrid(img, cart, "realistic", "cartoon")
        kept = [p for p in kept if not inside(p)] + [p for p in v10 if inside(p)]
    # anchors drawn from landmarks, as in v10
    s = kept
    for up, lo, ci, ring, a, b, um, lm in ((L_UP, L_LO, 468, [469, 470, 471, 472], 33, 133, 159, 145),
                                          (R_UP, R_LO, 473, [474, 475, 476, 477], 263, 362, 386, 374)):
        s += eye_v7(L, up, lo, ci, ring, a, b, um, lm, fw, "ring")
    s += lips(L) + brow_outline(L, L_BROW_U, L_BROW_L) + brow_outline(L, R_BROW_U, R_BROW_L)
    rng = np.random.default_rng(seed)
    _, found = facial_hair(img, cat, L, fw)
    if beard is None:  # a full beard is already drawn by the extracted strokes
        if 'moustache' in found: s += moustache_dashes(L, fw)
        if 'chin' in found: s += goatee_shape(L, fw)
    c2 = cat.copy()
    if hat is not None: c2[hat] = 0
    if wears_glasses(c2, L): s += glasses_v7(c2, L, fw)
    if hat is not None:
        s = clip_out(s, cv2.erode(hat.astype(np.uint8), np.ones((5, 5), np.uint8)) > 0) + draw_headwear(hat, fw)[:1] + cap_text(img, hat, fw)
    return [p for p in s if len(p) > 1]


def sketch_polylines(img):
    """Photo (BGR) in; pixel-space polylines and the size of the face crop they live in out."""
    box = itp.face_crop_box(img, 0.7, 1.3, 1.1)
    if box:
        img = img[box[1]:box[3], box[0]:box[2]]
    polylines = style_v13(img, anime(img))
    return polylines, img.shape[:2]


def simplify_and_order(polylines_mm, epsilon_mm=0.25, min_length_mm=1.0):
    """Drop points the pen cannot resolve, drop strokes too short to see, and order the rest
    nearest-first (flipping where that is shorter) so pen-up travel stays small."""
    strokes = []
    for p in polylines_mm:
        a = cv2.approxPolyDP(np.asarray(p, np.float32).reshape(-1, 1, 2), epsilon_mm, False)[:, 0, :].astype(float)
        if len(a) >= 2 and np.linalg.norm(np.diff(a, axis=0), axis=1).sum() >= min_length_mm:
            strokes.append(a)
    out, cur, left = [], np.zeros(2), list(range(len(strokes)))
    while left:
        best = min(((np.linalg.norm(strokes[i][0] - cur), i, False) for i in left),
                   key=lambda t: t[0])
        best_rev = min(((np.linalg.norm(strokes[i][-1] - cur), i, True) for i in left),
                       key=lambda t: t[0])
        _, i, flip = min(best, best_rev, key=lambda t: t[0])
        p = strokes[i][::-1] if flip else strokes[i]
        out.append([[round(float(x), 2), round(float(y), 2)] for x, y in p])
        left.remove(i)
        cur = p[-1]
    return out
