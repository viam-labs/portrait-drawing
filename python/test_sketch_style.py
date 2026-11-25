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


def test_simplify_caps_and_joins():
    touching = [[[0.0, 0.0], [10.0, 0.0]], [[10.2, 0.0], [20.0, 0.0]]]
    short = [[[50.0, 50.0], [52.0, 50.0]]]
    out = sketch_style.simplify_and_order(touching + short, max_strokes=1, join_mm=0.75)
    assert out == [[[0.0, 0.0], [10.0, 0.0], [20.0, 0.0]]]


def test_longest_keeps_the_longest_in_their_order():
    strokes = [np.array([[0, 0], [n, 0]], float) for n in (5, 1, 9, 3)]
    assert [p[1, 0] for p in sketch_style.longest(strokes, 2)] == [5, 9]


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


def test_face_fraction_uses_the_shorter_side():
    assert sketch_style.face_fraction(180, (720, 1280, 3)) == pytest.approx(0.25)


def test_a_face_too_far_away_is_rejected(monkeypatch):
    # Measured on armfarm7: a face on the far wall was 20 px wide in a 1280x720 frame.
    monkeypatch.setattr(sketch_style.itp, "detect_face", lambda img: (600.0, 300.0, 20.0, 24.0))
    img = np.full((720, 1280, 3), 200, np.uint8)
    with pytest.raises(ValueError, match="close enough"):
        sketch_style.sketch_polylines(img)


def test_the_threshold_is_configurable(monkeypatch):
    monkeypatch.setattr(sketch_style.itp, "detect_face", lambda img: (600.0, 300.0, 20.0, 24.0))
    img = np.full((720, 1280, 3), 200, np.uint8)
    # Allowed through the size check; it then fails later for want of real landmarks.
    with pytest.raises(ValueError, match="no face found"):
        sketch_style.sketch_polylines(img, min_face_fraction=0.01)


def test_features_are_drawn_last_and_counted_out():
    import numpy as np
    traced = [np.array([[0, 0], [30, 0]]), np.array([[0, 10], [30, 10]]), np.array([[0, 20], [2, 20]])]
    eye = np.array([[50 + 3 * np.cos(t), 50 + np.sin(t)] for t in np.linspace(0, 2 * np.pi, 40)])
    out, detail_from = sketch_style.simplify_and_order_with_detail(traced + [eye], n_detail=1, max_strokes=3)
    # The budget of 3 leaves 2 traced strokes once the eye is counted; the eye comes last.
    assert detail_from == 2
    assert len(out) == 3
    # Lighter simplification keeps a small eye round: far more points than its 0.25 mm cousin.
    assert len(out[-1]) > len(sketch_style.simplify_and_order([eye])[0])


def test_no_features_means_nothing_to_draw_slowly():
    import numpy as np
    out, detail_from = sketch_style.simplify_and_order_with_detail([np.array([[0, 0], [30, 0]])], n_detail=0)
    assert detail_from == len(out)
