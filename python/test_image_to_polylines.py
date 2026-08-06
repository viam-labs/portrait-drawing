import cv2
import numpy as np

from image_to_polylines import (
    _rotate_ccw,
    _rotation_for_paper,
    image_bytes_to_polylines,
    polylines_to_mm,
)


def _synthetic_image_bytes() -> bytes:
    """400x400 white image with a black filled square — feeds a predictable contour."""
    img = np.full((400, 400), 255, dtype=np.uint8)
    cv2.rectangle(img, (100, 100), (300, 300), 0, -1)
    ok, encoded = cv2.imencode(".png", img)
    assert ok
    return encoded.tobytes()


def test_pipeline_returns_polylines_in_paper_mm():
    polylines = image_bytes_to_polylines(
        _synthetic_image_bytes(),
        paper_width_mm=215.9,
        paper_height_mm=279.4,
        margin_mm=40.0,
        rotate=0,
        mirror=False,
        region=0,
        low=60,
        high=160,
        merge=5,
        prune=25,
        min_len=30,
        smooth=2.5,
        min_dist=8,
    )
    assert isinstance(polylines, list)
    assert len(polylines) > 0
    for line in polylines:
        assert len(line) >= 2
        for pt in line:
            assert len(pt) == 2
            x, y = pt
            assert 0 <= x <= 216, f"x={x} out of paper bounds"
            assert 0 <= y <= 280, f"y={y} out of paper bounds"


def test_rejects_bad_bytes():
    try:
        image_bytes_to_polylines(
            b"not an image", paper_width_mm=100, paper_height_mm=100,
            margin_mm=10, rotate=0, mirror=False, region=0, low=60, high=160,
            merge=5, prune=25, min_len=30, smooth=2.5, min_dist=8,
        )
    except ValueError as e:
        assert "decode" in str(e)
    else:
        raise AssertionError("expected ValueError on bad image bytes")


def test_polylines_to_mm_empty():
    assert polylines_to_mm([], 100, 100, 10, 0, False) == []


def test_rotation_for_paper_matching_orientations():
    # Portrait image on portrait paper → no rotation
    assert _rotation_for_paper(300, 400, 215.9, 279.4) == 0
    # Landscape image on landscape paper → no rotation
    assert _rotation_for_paper(400, 300, 279.4, 215.9) == 0


def test_rotation_for_paper_mismatched_orientations():
    # Landscape image on portrait paper → rotate 90°
    assert _rotation_for_paper(400, 300, 215.9, 279.4) == 90
    # Portrait image on landscape paper → rotate 90°
    assert _rotation_for_paper(300, 400, 279.4, 215.9) == 90


def test_rotation_for_paper_square_image_no_rotate():
    # Square image (aspect == 1) is treated as portrait; stays 0° on portrait paper
    assert _rotation_for_paper(300, 300, 215.9, 279.4) == 0


def test_rotate_ccw_90():
    pts = np.array([[1, 0], [0, 1]])
    got = _rotate_ccw(pts, 90)
    assert got.tolist() == [[0, -1], [1, 0]]


def test_rotate_ccw_invalid():
    try:
        _rotate_ccw(np.array([[0, 0]]), 45)
    except ValueError:
        pass
    else:
        raise AssertionError("expected ValueError for non-90-multiple rotation")


def test_auto_rotate_default_true():
    """image_bytes_to_polylines defaults auto_rotate to True."""
    polylines = image_bytes_to_polylines(
        _synthetic_image_bytes(),
        paper_width_mm=215.9, paper_height_mm=279.4, margin_mm=40.0,
        rotate=0, mirror=False, region=0, low=60, high=160,
        merge=5, prune=25, min_len=30, smooth=2.5, min_dist=8,
    )
    # For a square input, auto-rotate picks 0° (no rotation); just check it ran.
    assert isinstance(polylines, list)
    assert len(polylines) > 0
