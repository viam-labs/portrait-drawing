#!/bin/bash
set -euo pipefail

OS="$(uname -s)"
if [[ "$OS" == "Linux" ]]; then
    sudo apt-get update
    sudo apt-get install -y --no-install-recommends \
        ca-certificates \
        libnlopt-dev
elif [[ "$OS" == "Darwin" ]]; then
    brew tap viamrobotics/brews
    brew install nlopt-static
fi

# Node, for building the web application. Viam's cloud build runner does not
# ship it, and the module tarball has to contain web/dist.
if ! command -v npm >/dev/null 2>&1; then
    if [[ "$OS" == "Linux" ]]; then
        sudo apt-get install -y curl ca-certificates
        curl -fsSL https://deb.nodesource.com/setup_22.x | sudo bash -
        sudo apt-get install -y nodejs
    elif [[ "$OS" == "Darwin" ]]; then
        brew install node
    fi
fi
node --version && npm --version
