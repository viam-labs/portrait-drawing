"""Convert an image on stdin into ordered 2D polylines in paper-local mm.

Stub implementation: returns a hardcoded fixture so the Go module can be
wired end-to-end before the real OpenCV pipeline lands. Replace the body
of image_bytes_to_polylines with the real contour extraction.
"""

import argparse
import json
import sys


def image_bytes_to_polylines(
    image_bytes: bytes,
    epsilon: float,
    min_contour_length: float,
    target_width_mm: float,
    threshold: int,
    mirror: bool,
) -> list[list[list[float]]]:
    """Return polylines in paper-local mm. Stub returns a small square."""
    _ = image_bytes, epsilon, min_contour_length, target_width_mm, threshold, mirror
    return [[[10.0, 10.0], [30.0, 10.0], [30.0, 30.0], [10.0, 30.0], [10.0, 10.0]]]


def main() -> int:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--epsilon", type=float, default=1.5)
    p.add_argument("--min-length", dest="min_contour_length", type=float, default=10.0)
    p.add_argument("--target-width-mm", type=float, default=150.0)
    p.add_argument("--threshold", type=int, default=127)
    p.add_argument("--mirror", action="store_true")
    args = p.parse_args()

    image_bytes = sys.stdin.buffer.read()
    if not image_bytes:
        print("error: no image bytes on stdin", file=sys.stderr)
        return 1

    polylines = image_bytes_to_polylines(
        image_bytes,
        epsilon=args.epsilon,
        min_contour_length=args.min_contour_length,
        target_width_mm=args.target_width_mm,
        threshold=args.threshold,
        mirror=args.mirror,
    )
    json.dump({"polylines": polylines}, sys.stdout)
    return 0


if __name__ == "__main__":
    sys.exit(main())
