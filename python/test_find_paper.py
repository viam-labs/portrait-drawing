import json
import subprocess
import sys
from pathlib import Path

import cv2
import numpy as np
import pytest

from find_paper import DEFAULT_LOGO, find_crosses, find_sheets, fit_homography

HERE = Path(__file__).resolve().parent
MAT = cv2.imread(str(HERE / "testdata" / "mat_view.jpg"))
LOGO = cv2.imread(str(DEFAULT_LOGO), cv2.IMREAD_GRAYSCALE)


def sheets_of(img):
    found = find_sheets(img, LOGO)["sheets"]
    return {s["kind"]: s for s in found}, found


def test_finds_the_card_and_the_tag_on_the_mat_only():
    by_kind, found = sheets_of(MAT)
    # The tag and the drawn portrait off the mat, bottom left, are not counted.
    assert sorted(s["kind"] for s in found) == ["card", "name_tag"]
    assert not by_kind["card"]["used"]
    assert not by_kind["name_tag"]["used"]


def test_a_single_dot_does_not_use_up_a_card():
    by_kind, _ = sheets_of(MAT)
    assert by_kind["card"]["strokes"] < 2


def test_card_corners_run_clockwise_from_a_short_top_edge():
    by_kind, _ = sheets_of(MAT)
    c = np.array(by_kind["card"]["corners_px"])
    top, right = np.linalg.norm(c[1] - c[0]), np.linalg.norm(c[2] - c[1])
    assert top < right
    assert top / right == pytest.approx(101.6 / 152.4, abs=0.05)
    # Clockwise on screen: the second corner is to the right of the first.
    assert c[1][0] > c[0][0]


def test_tag_reads_from_its_logo():
    by_kind, _ = sheets_of(MAT)
    tag = by_kind["name_tag"]
    assert abs(tag["logo"]["rotation"] - 180) <= 5
    # Upside down in the image, so its top-left is the image's bottom-right.
    tl = np.array(tag["corners_px"][0])
    centre = np.mean(tag["corners_px"], 0)
    assert tl[0] > centre[0] and tl[1] > centre[1]


def test_turning_the_photo_turns_the_tag_reading():
    turned = cv2.rotate(MAT, cv2.ROTATE_90_CLOCKWISE)
    by_kind, _ = sheets_of(turned)
    tag = by_kind["name_tag"]
    assert abs(tag["logo"]["rotation"] - 270) <= 5
    tl = np.array(tag["corners_px"][0])
    centre = np.mean(tag["corners_px"], 0)
    # The original top-left (image bottom-right) turns to the bottom-left.
    assert tl[0] < centre[0] and tl[1] > centre[1]


def test_lines_on_a_card_use_it_up():
    img = MAT.copy()
    for y in (300, 340, 380):
        cv2.line(img, (840, y), (970, y + 20), (30, 30, 30), 3)
    by_kind, _ = sheets_of(img)
    assert by_kind["card"]["strokes"] >= 3
    assert by_kind["card"]["used"]


def test_a_written_name_uses_up_a_tag_but_its_printed_labels_do_not():
    img = MAT.copy()
    # Upside down in the image, like the tag.
    patch = np.full((40, 130, 3), 255, np.uint8)
    cv2.putText(patch, "Ada", (5, 32), cv2.FONT_HERSHEY_SIMPLEX, 1.2, (0, 0, 0), 3)
    patch = cv2.rotate(patch, cv2.ROTATE_180)
    region = img[450:490, 490:620]
    img[450:490, 490:620] = np.where(patch < 128, patch, region)
    by_kind, _ = sheets_of(img)
    assert by_kind["name_tag"]["used"]


def synthetic_crosses(arm_points, h_arm_to_img, size=(720, 1280), arm_mm=10):
    img = np.full((*size, 3), 235, np.uint8)
    arms = np.asarray(arm_points, float)
    for x, y in arms:
        ends = np.array([[x - arm_mm / 2, y], [x + arm_mm / 2, y], [x, y - arm_mm / 2], [x, y + arm_mm / 2]])
        e = cv2.perspectiveTransform(ends.reshape(-1, 1, 2), h_arm_to_img).reshape(-1, 2)
        cv2.line(img, tuple(np.round(e[0]).astype(int)), tuple(np.round(e[1]).astype(int)), (20, 20, 20), 2)
        cv2.line(img, tuple(np.round(e[2]).astype(int)), tuple(np.round(e[3]).astype(int)), (20, 20, 20), 2)
    return img


ARM = [[150, -250], [150, 0], [150, 250], [300, -250], [300, 0], [300, 250], [230, -120]]
# Arm X runs up the image and arm Y to the left, with a little perspective.
H_ARM_TO_IMG = np.array([[0.0, -2.0, 640.0], [-2.0, 0.0, 950.0], [0.0, 0.0002, 1.0]])


def test_finds_drawn_crosses():
    img = synthetic_crosses(ARM, H_ARM_TO_IMG)
    found = np.array(find_crosses(img))
    want = cv2.perspectiveTransform(np.array(ARM, float).reshape(-1, 1, 2), H_ARM_TO_IMG).reshape(-1, 2)
    assert len(found) == len(ARM)
    for w in want:
        assert np.min(np.linalg.norm(found - w, axis=1)) < 1.0


def test_fit_recovers_arm_positions_whatever_order_the_crosses_come_in():
    img = synthetic_crosses(ARM, H_ARM_TO_IMG)
    found = find_crosses(img)[::-1]
    out = fit_homography(found, ARM)
    assert len(out["matched"]) == len(ARM)
    assert out["max_mm"] < 0.6
    h = np.array(out["homography"])
    probe = cv2.perspectiveTransform(np.array([[[640.0, 500.0]]]), h).ravel()
    want = cv2.perspectiveTransform(np.array([[[640.0, 500.0]]]), np.linalg.inv(H_ARM_TO_IMG)).ravel()
    assert np.linalg.norm(probe - want) < 0.6


def test_fit_ignores_a_stray_mark_and_reports_a_missing_cross():
    img = synthetic_crosses(ARM[:-1], H_ARM_TO_IMG)
    found = find_crosses(img) + [[100.0, 100.0]]
    out = fit_homography(found, ARM)
    assert len(out["matched"]) == len(ARM) - 1
    assert out["unmatched_arm_points"] == [ARM[-1]]


def test_cli_sheets_round_trip():
    raw = (HERE / "testdata" / "mat_view.jpg").read_bytes()
    out = subprocess.run([sys.executable, str(HERE / "find_paper.py"), "sheets"], input=raw,
                         capture_output=True, check=True)
    assert len(json.loads(out.stdout)["sheets"]) == 2



def test_a_black_mat_with_tilted_tags():
    found = find_sheets(cv2.imread(str(HERE / "testdata" / "black_mat.jpg")),
                        cv2.imread(str(DEFAULT_LOGO), cv2.IMREAD_GRAYSCALE))["sheets"]
    tags = [s for s in found if s["kind"] == "name_tag"]
    cards = [s for s in found if s["kind"] == "card"]
    assert len(tags) == 8
    assert all(t["logo"] and t["logo"]["score"] > 0.8 for t in tags)
    # The fifth card has a corner hidden under the arm's shadow.
    assert len(cards) == 4
    assert not any(s["used"] for s in found)


def test_crosses_on_cards_on_a_black_mat():
    found = find_crosses(cv2.imread(str(HERE / "testdata" / "crosses_black_mat.jpg")))
    # Eight drawn; one has a faint arm. The L and T shapes where cards meet are not crosses.
    assert len(found) == 7
    assert all(400 < x < 1100 or 300 < x < 400 for x, _ in found)


def test_fit_refuses_the_mirror_image_of_a_symmetric_layout():
    # A 3-2-3 layout with its far corner's cross missing fits its own mirror
    # image to a fraction of a millimetre; only the unmirrored match is right.
    image = [[1006.29, 214.02], [689.73, 224.38], [371.83, 228.53], [544.07, 328.39],
             [860.36, 331.97], [694.26, 444.96], [361.87, 458.24]]
    arm = [[387, 199], [388, 0], [392, -200], [322, 92], [319, -109], [241, 209], [246, -2], [242, -208]]
    out = fit_homography(image, arm)
    pairs = {tuple(m["arm"]): m["image"] for m in out["matched"]}
    assert pairs[(387.0, 199.0)] == [371.83, 228.53]
    assert out["unmatched_arm_points"] == [[242.0, -208.0]]
