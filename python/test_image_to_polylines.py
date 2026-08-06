from image_to_polylines import image_bytes_to_polylines


def test_stub_returns_polylines():
    result = image_bytes_to_polylines(
        b"anything",
        epsilon=1.5,
        min_contour_length=10.0,
        target_width_mm=150.0,
        threshold=127,
        mirror=False,
    )
    assert isinstance(result, list)
    assert len(result) > 0
    for polyline in result:
        for point in polyline:
            assert len(point) == 2
            assert all(isinstance(c, (int, float)) for c in point)
