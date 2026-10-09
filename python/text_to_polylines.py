#!/usr/bin/env python3
"""Lay a line of text out on a card, as polylines the drawer can trace.

Output is in card-local millimetres with (0, 0) at the top-left corner and y
running down the card, the same frame the drawer's `draw` verb takes.

Two kinds of font, and they draw differently. A TTF describes the *boundary*
of each letter, so tracing it draws hollow outlines. A Hershey stroke font is
the centreline, the path a pen would take, so it is the default here: one
pass per stroke, no retracing. The default face is the joined-up "cursive",
with the pen kept down between letters so a word is one stroke.

    python3 text_to_polylines.py --text "Ada Lovelace" --width-mm 85.9 --height-mm 59.2
"""

import argparse
import json
import math
import sys

# Helvetica is rarely installed on Linux; Nimbus Sans is URW's metric clone and
# the honest stand-in. DejaVu is last because its letterforms visibly differ.
FONT_CHAIN = ["Helvetica", "Nimbus Sans", "Liberation Sans", "DejaVu Sans"]

# "cursive" is a joined-up script face, the handwriting look a name tag wants.
# "futural" is Hershey Simplex, a plain sans; "rowmans"/"timesr" are seriffed.
DEFAULT_STROKE_FONT = "cursive"

# Join tolerance as a fraction of cap height: closes the gaps between letters
# in a script face while leaving word spaces (about a third of the cap) alone.
# Upright faces space their letters wider than this, so joining is a no-op there.
JOIN_TOL_RATIO = 0.22

# Nominal size glyph paths are built at; everything is scaled from here.
_EM_UNITS = 100.0


class TextError(RuntimeError):
    """The text could not be laid out."""


def resolve_font(family=None):
    """(FontProperties, name) for the first installed family in the chain.

    matplotlib substitutes silently when a family is missing, so this resolves
    explicitly and reports what was actually chosen.
    """
    import matplotlib
    matplotlib.use("Agg")
    from matplotlib.font_manager import FontProperties, findfont, get_font

    chain = [family] if family else FONT_CHAIN
    tried = []
    for want in chain:
        try:
            path = findfont(FontProperties(family=want), fallback_to_default=False)
        except ValueError:
            tried.append(want)
            continue
        return FontProperties(fname=path), get_font(path).family_name
    raise TextError(f"none of {chain} is installed (tried {tried})")


def _polygons(text, prop):
    from matplotlib.textpath import TextPath
    path = TextPath((0, 0), text, size=_EM_UNITS, prop=prop)
    return [p for p in path.to_polygons() if len(p) >= 3]


def _outline_paths(text, family):
    """Closed outlines at the nominal size, plus cap height and font name."""
    prop, name = resolve_font(family)
    cap = _polygons("H", prop)
    if not cap:
        raise TextError("could not measure cap height: 'H' produced no contours")
    ys = [pt[1] for p in cap for pt in p]
    polys = _polygons(text, prop)
    if not polys:
        raise TextError(f"{text!r} produced no contours in {name}")
    paths = []
    for p in polys:
        pts = [(float(x), float(y)) for x, y in p]
        if pts[0] != pts[-1]:
            pts.append(pts[0])
        paths.append(pts)
    return paths, max(ys) - min(ys), name


def _hershey(name):
    from HersheyFonts import HersheyFonts
    h = HersheyFonts()
    if name not in h.default_font_names:
        raise TextError(f"unknown stroke font {name!r}; available: "
                        f"{', '.join(h.default_font_names)}")
    h.load_default_font(name)
    h.normalize_rendering(_EM_UNITS)
    return h


def _stroke_paths(text, name):
    """Open centreline strokes at the nominal size, plus cap height and name."""
    h = _hershey(name)
    strokes = [[(float(x), float(y)) for x, y in seg] for seg in h.strokes_for_text(text)]
    strokes = [seg for seg in strokes if len(seg) >= 2]
    if not strokes:
        raise TextError(f"{text!r} produced no strokes in {name}")
    hs = [p for seg in _hershey(name).strokes_for_text("H") for p in seg]
    cap = max(p[1] for p in hs) - min(p[1] for p in hs)
    return strokes, cap, f"{name} (single-stroke)"


def _resample(points, spacing):
    """A point every `spacing` mm of arc length, keeping the original ends.

    The drawer plans each pen-down segment as a straight line, so this only
    matters for curves; it keeps the drawn curve close to the glyph.
    """
    out = [points[0]]
    carry = 0.0
    for (x0, y0), (x1, y1) in zip(points, points[1:]):
        seg = math.dist((x0, y0), (x1, y1))
        if seg == 0:
            continue
        t = spacing - carry
        while t <= seg:
            f = t / seg
            out.append((x0 + (x1 - x0) * f, y0 + (y1 - y0) * f))
            t += spacing
        carry = (carry + seg) % spacing
    if out[-1] != points[-1]:
        # A sliver shorter than half a step is a wasted waypoint, not a feature.
        if len(out) > 2 and math.dist(out[-1], points[-1]) < spacing * 0.5:
            out.pop()
        out.append(points[-1])
    return out


def join_strokes(strokes, tol_mm):
    """Merge consecutive strokes whose gap is at most `tol_mm`.

    A stroke font stores each glyph separately, so even a script face lifts
    the pen between letters. Closing small gaps makes the word one stroke,
    the way handwriting is; word spaces are wider than the tolerance and stay
    as lifts.
    """
    if tol_mm <= 0 or not strokes:
        return [list(s) for s in strokes]
    out = [list(strokes[0])]
    for stroke in strokes[1:]:
        prev = out[-1]
        gap = math.dist(prev[-1], stroke[0])
        if gap <= tol_mm:
            out[-1] = prev + (list(stroke[1:]) if gap < 1e-9 else list(stroke))
        else:
            out.append(list(stroke))
    return out


def _bbox(paths):
    xs = [p[0] for s in paths for p in s]
    ys = [p[1] for s in paths for p in s]
    return min(xs), max(xs), min(ys), max(ys)


def text_polylines(text, width_mm, height_mm, cap_mm=None, fill=0.8,
                   spacing_mm=2.0, font=None, stroke_font=DEFAULT_STROKE_FONT,
                   join=True):
    """Polylines for `text` centred on a width x height card. Returns (polylines, info).

    `cap_mm` fixes the capital height; with it unset the text is scaled so its
    inked box fills `fill` of the card on whichever axis binds first. An
    explicit cap height that overflows the card is an error rather than a
    drawing on the table.

    `stroke_font` names a Hershey face; None traces TTF outlines from `font`.
    """
    text = text.strip()
    if not text:
        raise TextError("text is empty")
    if width_mm <= 0 or height_mm <= 0:
        raise TextError(f"card must have positive size, got {width_mm}x{height_mm}")
    if cap_mm is not None and cap_mm <= 0:
        raise TextError(f"cap height must be positive, got {cap_mm}")
    if cap_mm is None and not 0 < fill <= 1:
        raise TextError(f"fill must be in (0, 1], got {fill}")
    if spacing_mm <= 0:
        raise TextError(f"spacing must be positive, got {spacing_mm}")

    if stroke_font:
        paths, cap_units, name = _stroke_paths(text, stroke_font)
    else:
        paths, cap_units, name = _outline_paths(text, font)
    x0, x1, y0, y1 = _bbox(paths)
    w, h = x1 - x0, y1 - y0
    if h <= 0 or w <= 0:
        raise TextError(f"{text!r} has no extent in {name}")

    if cap_mm is None:
        scale = min(fill * width_mm / w, fill * height_mm / h)
    else:
        scale = cap_mm / cap_units
    if w * scale > width_mm or h * scale > height_mm:
        raise TextError(
            f"{text!r} at cap {cap_units * scale:.1f}mm needs "
            f"{w * scale:.1f}x{h * scale:.1f}mm, card is {width_mm}x{height_mm}mm")

    # Glyph y runs up; card y runs down from the top-left corner. Centre the
    # inked box on the card and flip.
    ox = width_mm / 2 - (x0 + w / 2) * scale
    oy = height_mm / 2 + (y0 + h / 2) * scale
    out = []
    for seg in paths:
        pts = [(x * scale + ox, oy - y * scale) for x, y in seg]
        out.append(_resample(pts, spacing_mm) if len(pts) > 1 else pts)

    strokes_before_join = len(out)
    if join and stroke_font:
        out = join_strokes(out, cap_units * scale * JOIN_TOL_RATIO)

    info = {
        "font": name,
        "cap_mm": cap_units * scale,
        "bbox_w_mm": w * scale,
        "bbox_h_mm": h * scale,
        "strokes_before_join": strokes_before_join,
    }
    return [[[round(x, 3), round(y, 3)] for x, y in s] for s in out], info


def main() -> int:
    p = argparse.ArgumentParser(description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--text", required=True)
    p.add_argument("--width-mm", type=float, required=True)
    p.add_argument("--height-mm", type=float, required=True)
    p.add_argument("--cap-mm", type=float, default=None,
                   help="capital height; omit to size from --fill")
    p.add_argument("--fill", type=float, default=0.8,
                   help="fraction of the card the text fills when --cap-mm is "
                        "not given (default 0.8)")
    p.add_argument("--spacing-mm", type=float, default=2.0)
    p.add_argument("--font", default=None, help="TTF family for --outline")
    p.add_argument("--stroke-font", default=DEFAULT_STROKE_FONT,
                   help=f"Hershey face (default {DEFAULT_STROKE_FONT})")
    p.add_argument("--outline", action="store_true",
                   help="trace TTF outlines instead of a stroke font")
    p.add_argument("--join", action=argparse.BooleanOptionalAction, default=True,
                   help="keep the pen down between letters of a script face "
                        "(default on)")
    args = p.parse_args()

    try:
        polylines, info = text_polylines(
            args.text, args.width_mm, args.height_mm, cap_mm=args.cap_mm,
            fill=args.fill, spacing_mm=args.spacing_mm, font=args.font,
            stroke_font=None if args.outline else args.stroke_font,
            join=args.join)
    except TextError as e:
        print(f"error: {e}", file=sys.stderr)
        return 1
    except ImportError as e:
        print(f"error: missing dependency: {e}", file=sys.stderr)
        return 1
    print(json.dumps({"polylines": polylines, **info}))
    return 0


if __name__ == "__main__":
    sys.exit(main())
