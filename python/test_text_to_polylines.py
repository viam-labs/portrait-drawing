import json
import subprocess
import sys
from pathlib import Path

import pytest

from text_to_polylines import TextError, join_strokes, text_polylines

W, H = 85.9, 59.2


def _bbox(polylines):
    xs = [p[0] for s in polylines for p in s]
    ys = [p[1] for s in polylines for p in s]
    return min(xs), max(xs), min(ys), max(ys)


def test_fill_keeps_text_inside_card_and_centred():
    polylines, info = text_polylines("Ada Lovelace", W, H)
    x0, x1, y0, y1 = _bbox(polylines)
    assert 0 <= x0 and x1 <= W and 0 <= y0 and y1 <= H
    assert (x0 + x1) / 2 == pytest.approx(W / 2, abs=0.01)
    assert (y0 + y1) / 2 == pytest.approx(H / 2, abs=0.01)
    assert info["bbox_w_mm"] == pytest.approx(0.8 * W, abs=0.01)
    assert info["font"].startswith("cursive")


def test_short_name_is_height_bound():
    _, info = text_polylines("Al", W, H)
    assert info["bbox_h_mm"] == pytest.approx(0.8 * H, abs=0.01)
    assert info["bbox_w_mm"] < W


def test_y_runs_down_the_card():
    # The dot of a lowercase i is above its stem, so it must have the smaller y.
    polylines, _ = text_polylines("i", W, H, stroke_font="futural", join=False)
    tops = sorted(min(p[1] for p in s) for s in polylines)
    assert len(polylines) == 2
    assert tops[0] < tops[1]


def test_explicit_cap_height():
    _, info = text_polylines("Ada", W, H, cap_mm=12.0)
    assert info["cap_mm"] == pytest.approx(12.0)


def test_cap_height_that_overflows_is_refused():
    with pytest.raises(TextError, match="card is"):
        text_polylines("Ada", W, H, cap_mm=80.0)


def test_join_makes_a_word_one_stroke():
    joined, info = text_polylines("Ada Lovelace", W, H, join=True)
    apart, _ = text_polylines("Ada Lovelace", W, H, join=False)
    assert len(joined) < len(apart)
    assert info["strokes_before_join"] == len(apart)


def test_join_leaves_wide_gaps_alone():
    out = join_strokes([[(0, 0), (1, 0)], [(1.1, 0), (2, 0)], [(10, 0), (11, 0)]], 0.5)
    assert out == [[(0, 0), (1, 0), (1.1, 0), (2, 0)], [(10, 0), (11, 0)]]


def test_outline_font_gives_closed_contours():
    polylines, info = text_polylines("A", W, H, stroke_font=None)
    assert "single-stroke" not in info["font"]
    for s in polylines:
        assert s[0] == s[-1]


@pytest.mark.parametrize("text", ["", "   "])
def test_empty_text_is_refused(text):
    with pytest.raises(TextError, match="empty"):
        text_polylines(text, W, H)


def test_unknown_stroke_font_is_refused():
    with pytest.raises(TextError, match="unknown stroke font"):
        text_polylines("Ada", W, H, stroke_font="nope")


def test_cli_prints_json():
    script = Path(__file__).with_name("text_to_polylines.py")
    out = subprocess.run(
        [sys.executable, str(script), "--text", "Ada", "--width-mm", str(W), "--height-mm", str(H)],
        check=True, capture_output=True, text=True).stdout
    d = json.loads(out)
    assert d["polylines"] and all(len(p) == 2 for s in d["polylines"] for p in s)
    assert d["cap_mm"] > 0


def test_cli_reports_errors_on_stderr():
    script = Path(__file__).with_name("text_to_polylines.py")
    r = subprocess.run(
        [sys.executable, str(script), "--text", "Ada", "--width-mm", str(W),
         "--height-mm", str(H), "--cap-mm", "80"],
        capture_output=True, text=True)
    assert r.returncode == 1
    assert "card is" in r.stderr
