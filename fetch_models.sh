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
    local dest="$1" url="$2" required="$3"
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
    echo "warning: could not fetch $dest; face-aware framing disabled" >&2
}

# The line model is the extraction stage itself — without it there is nothing to
# draw, so a failure here is fatal rather than degraded output.
fetch "$LINEART" "$LINEART_URL" required
fetch "$YUNET" "$YUNET_URL" optional
