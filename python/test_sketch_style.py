import cv2
import numpy as np
import pytest

import sketch_style
from image_to_polylines import image_bytes_to_polylines


def _png(img: np.ndarray) -> bytes:
    ok, encoded = cv2.imencode(".png", img)
    assert ok
    return encoded.tobytes()


def test_simplify_drops_redundant_points_and_tiny_strokes():
    straight = [[0.0, 0.0], [5.0, 0.05], [10.0, 0.0]]  # middle point is within 0.25 mm of the line
    tiny = [[50.0, 50.0], [50.3, 50.3]]  # under 1 mm long
    out = sketch_style.simplify_and_order([straight, tiny])
    assert out == [[[0.0, 0.0], [10.0, 0.0]]]


def test_order_starts_nearest_and_flips_strokes_to_shorten_travel():
    far = [[100.0, 0.0], [110.0, 0.0]]
    near_reversed = [[20.0, 0.0], [10.0, 0.0]]
    out = sketch_style.simplify_and_order([far, near_reversed])
    assert out[0] == [[10.0, 0.0], [20.0, 0.0]]  # nearest stroke first, drawn from its near end
    assert out[1] == [[100.0, 0.0], [110.0, 0.0]]


def test_line_map_is_same_size_dark_lines_on_white():
    img = np.full((300, 220, 3), 255, np.uint8)
    cv2.circle(img, (110, 150), 60, (0, 0, 0), 4)
    lines = sketch_style.line_map(img, res=256)
    assert lines.shape == (300, 220)
    assert lines.dtype == np.uint8
    assert lines[150, 110] > lines[150, 50]  # inside the ring stays light, the ring itself is dark


def test_anime_keeps_image_size():
    img = np.random.default_rng(0).integers(0, 255, (200, 160, 3), dtype=np.uint8)
    assert sketch_style.anime(img).shape == img.shape


def test_sketch_without_a_face_says_so():
    img = np.full((480, 640, 3), 200, np.uint8)
    with pytest.raises(ValueError, match="no face"):
        image_bytes_to_polylines(_png(img), 215.9, 279.4, 25, 0, False, 20, 36, 2.0, 3.0, style="sketch")


def test_unknown_style_is_rejected():
    img = np.full((100, 100, 3), 200, np.uint8)
    with pytest.raises(ValueError, match="style"):
        image_bytes_to_polylines(_png(img), 215.9, 279.4, 25, 0, False, 20, 36, 2.0, 3.0, style="cartoon")
