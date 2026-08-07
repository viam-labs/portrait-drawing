#!/bin/bash
# Runs once when the module is installed on a target machine. Installs
# system deps (nlopt for armplanning), uv, creates the Python venv, and
# installs Python dependencies.

set -euo pipefail

cd "$(dirname "$0")"

bash ./setup.sh

if ! command -v uv >/dev/null && [ ! -x "$HOME/.local/bin/uv" ]; then
    curl -LsSf https://astral.sh/uv/install.sh | sh
fi

UV="$(command -v uv || echo "$HOME/.local/bin/uv")"

"$UV" venv --python=3.10 .venv --clear
"$UV" pip install --python .venv/bin/python -r requirements.txt

# YuNet face-detection model for the stroke-generator (fetched here rather
# than committed). The pipeline falls back to non-face-aware behaviour if
# this download fails.
YUNET="models/face_detection_yunet_2023mar.onnx"
if [ ! -f "$YUNET" ]; then
    mkdir -p models
    curl -fsSL \
        "https://github.com/opencv/opencv_zoo/raw/main/models/face_detection_yunet/face_detection_yunet_2023mar.onnx" \
        -o "$YUNET" || echo "warning: could not fetch $YUNET; face-aware detail disabled" >&2
fi
