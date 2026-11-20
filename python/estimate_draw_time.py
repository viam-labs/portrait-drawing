"""Estimate how long the arm takes to draw a polyline set, offline.

Fitted to xArm6 runs at stroke_speed_degs_per_sec 30: 90 polylines with strokes
averaging about 25 mm drew in 79 s, so each polyline costs about 0.88 s all in.
The pen-down speed is not measured separately; --pen-mm-per-sec only moves time
between the per-lift cost and the per-millimetre cost, so it matters for drawings
whose strokes are much longer or shorter than 25 mm on average.

Usage:
    python estimate_draw_time.py drawing.json [...] [--max-strokes 110 --join-mm 0.75]

Accepts {"polylines": ...} or a DoCommand payload wrapping it ({"draw": {...}}).
"""

import argparse
import json
import sys

import numpy as np

import image_to_polylines as itp

SECONDS_PER_POLYLINE = 0.88
REFERENCE_STROKE_MM = 25.0


def polylines_of(doc: dict) -> list:
    if "polylines" in doc:
        return doc["polylines"]
    for value in doc.values():
        if isinstance(value, dict) and "polylines" in value:
            return value["polylines"]
    raise ValueError("no polylines in document")


def estimate_seconds(polylines: list, pen_mm_per_sec: float) -> float:
    pen_down = sum(float(np.linalg.norm(np.diff(np.asarray(p, float), axis=0), axis=1).sum()) for p in polylines)
    per_lift = SECONDS_PER_POLYLINE - REFERENCE_STROKE_MM / pen_mm_per_sec
    return len(polylines) * per_lift + pen_down / pen_mm_per_sec


def main() -> int:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("files", nargs="+")
    p.add_argument("--pen-mm-per-sec", type=float, default=60.0)
    p.add_argument("--max-strokes", type=int, default=110)
    p.add_argument("--join-mm", type=float, default=0.75)
    args = p.parse_args()
    print(f"{'file':40s} {'before':>14s} {'after':>14s}")
    for path in args.files:
        with open(path) as f:
            before = polylines_of(json.load(f))
        after = itp.finish_strokes(before, args.max_strokes, args.join_mm)
        cells = [f"{len(x):4d} {estimate_seconds(x, args.pen_mm_per_sec):5.0f}s" for x in (before, after)]
        print(f"{path.split('/')[-1][:40]:40s} {cells[0]:>14s} {cells[1]:>14s}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
