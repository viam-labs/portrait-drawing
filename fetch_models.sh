#!/bin/bash
# Downloads the models the stroke-generator needs, into models/.
#
# Shared by first_run.sh (on the target machine) and CI (before tests) so there
# is one definition of where a model comes from. Both need them for the same
# reason: the pipeline cannot extract strokes without the line model.

set -euo pipefail

cd "$(dirname "$0")"
mkdir -p models

LINEART="models/lineart.onnx"
LINEART_URL="https://huggingface.co/rocca/informative-drawings-line-art-onnx/resolve/main/model.onnx"

YUNET="models/face_detection_yunet_2023mar.onnx"
YUNET_URL="https://github.com/opencv/opencv_zoo/raw/main/models/face_detection_yunet/face_detection_yunet_2023mar.onnx"

fetch() {
    local dest="$1" url="$2" required="$3" what="${4:-face-aware framing}"
    if [ -f "$dest" ]; then
        return 0
    fi
    for attempt in 1 2 3; do
        if curl -fsSL "$url" -o "$dest"; then
            return 0
        fi
        echo "fetch $dest failed (attempt $attempt)" >&2
        sleep 5
    done
    if [ "$required" = "required" ]; then
        echo "error: could not fetch $dest" >&2
        return 1
    fi
    echo "warning: could not fetch $dest; $what disabled" >&2
}

# The line model is the extraction stage itself — without it there is nothing to
# draw, so a failure here is fatal rather than degraded output.
fetch "$LINEART" "$LINEART_URL" required
fetch "$YUNET" "$YUNET_URL" optional

# The sketch style only. Optional so a failed download degrades that one style
# (which then reports the missing file) rather than breaking the default.
FACE_PAINT="models/face_paint_512_v2.onnx"
FACE_PAINT_URL="https://storage.googleapis.com/ailia-models/animeganv2/face_paint_512_v2.onnx"
LANDMARKER="models/face_landmarker.task"
LANDMARKER_URL="https://storage.googleapis.com/mediapipe-models/face_landmarker/face_landmarker/float16/1/face_landmarker.task"
SELFIE_SEG="models/selfie_multiclass.tflite"
SELFIE_SEG_URL="https://storage.googleapis.com/mediapipe-models/image_segmenter/selfie_multiclass_256x256/float32/latest/selfie_multiclass_256x256.tflite"
fetch "$FACE_PAINT" "$FACE_PAINT_URL" optional sketch
fetch "$LANDMARKER" "$LANDMARKER_URL" optional sketch
fetch "$SELFIE_SEG" "$SELFIE_SEG_URL" optional sketch
