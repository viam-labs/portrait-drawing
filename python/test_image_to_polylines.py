import cv2
import numpy as np

from image_to_polylines import (
    _rotate_ccw,
    _rotation_for_paper,
    decode_depth,
    depth_mask,
    face_crop_box,
    isolate,
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
            margin_mm=10, rotate=0, mirror=False, prune=25, min_len=30, smooth=2.5, min_dist=8,
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
        rotate=0, mirror=False, prune=25, min_len=30, smooth=2.5, min_dist=8,
    )
    # For a square input, auto-rotate picks 0° (no rotation); just check it ran.
    assert isinstance(polylines, list)
    assert len(polylines) > 0


def test_pipeline_with_isolate_subject_off():
    """Background removal can be disabled; pipeline still returns polylines."""
    polylines = image_bytes_to_polylines(
        _synthetic_image_bytes(),
        paper_width_mm=215.9, paper_height_mm=279.4, margin_mm=40.0,
        rotate=0, mirror=False, prune=25, min_len=30, smooth=2.5, min_dist=8,
        isolate_subject=False,
    )
    assert isinstance(polylines, list)
    assert len(polylines) > 0


def test_pipeline_with_depth_masking():
    """A depth frame replaces colour segmentation; the pipeline still runs."""
    import struct

    size = 256
    depth = np.full((size, size), 3000, dtype=">u2")
    depth[40:216, 40:216] = 1000
    raw = b"DEPTHMAP" + struct.pack(">QQ", size, size) + depth.tobytes()
    polylines = image_bytes_to_polylines(
        _synthetic_image_bytes(),
        paper_width_mm=215.9, paper_height_mm=279.4, margin_mm=40.0,
        rotate=0, mirror=False, prune=25, min_len=30, smooth=2.5, min_dist=8,
        depth_bytes=raw, max_depth_mm=1500,
    )
    assert isinstance(polylines, list)


def test_isolate_off_returns_no_mask():
    img = cv2.imdecode(np.frombuffer(_synthetic_image_bytes(), np.uint8), cv2.IMREAD_COLOR)
    out, roi = isolate(img, isolate_subject=False)
    assert out.shape == img.shape
    assert roi is None


def _depth_payload(width, height, near_box=None, near_mm=1000, far_mm=3000):
    """A Viam DEPTHMAP payload: everything far except an optional near rectangle."""
    import struct

    depth = np.full((height, width), far_mm, dtype=">u2")
    if near_box:
        x0, y0, x1, y1 = near_box
        depth[y0:y1, x0:x1] = near_mm
    return b"DEPTHMAP" + struct.pack(">QQ", width, height) + depth.tobytes()


def test_decode_depth_roundtrip():
    depth = decode_depth(_depth_payload(8, 4, near_box=(1, 1, 3, 3), near_mm=900))
    assert depth.shape == (4, 8)
    assert depth[2, 2] == 900
    assert depth[0, 0] == 3000


def test_decode_depth_rejects_junk():
    for bad in (b"", b"NOTDEPTH" + b"\x00" * 16):
        try:
            decode_depth(bad)
        except ValueError:
            continue
        raise AssertionError("expected ValueError")


def test_depth_mask_keeps_the_near_band():
    depth = decode_depth(_depth_payload(60, 60, near_box=(10, 10, 50, 50), near_mm=1000))
    mask = depth_mask(depth, (60, 60), max_depth_mm=1500)
    assert mask[30, 30]
    assert not mask[2, 2]


def test_depth_mask_excludes_unmeasured_pixels():
    """A zero reading is unknown, not close: it must not count as foreground."""
    depth = decode_depth(_depth_payload(40, 40, near_box=(5, 5, 35, 35), near_mm=0))
    assert not depth_mask(depth, (40, 40), max_depth_mm=1500).any()


def test_depth_mask_near_bound_drops_the_foreground():
    """An arm-mounted pen sits nearer than any sitter and must be excluded."""
    depth = decode_depth(_depth_payload(60, 60, near_box=(10, 10, 50, 50), near_mm=250))
    assert depth_mask(depth, (60, 60), max_depth_mm=1500).any()
    assert not depth_mask(depth, (60, 60), max_depth_mm=1500, min_depth_mm=350).any()


def test_face_crop_box_none_without_a_face():
    assert face_crop_box(np.zeros((80, 80, 3), np.uint8), 1.0, 1.0, 1.0) is None
