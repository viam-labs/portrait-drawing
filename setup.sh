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

# Node, for building the web application. Viam's Linux build runner ships none
# and its macOS runner ships v20.13, so check the version rather than merely
# whether node exists. The floor is what vite 6 requires; vite 7 and 8 raise it
# to 20.19, which the macOS runner does not meet.
NODE_MIN_MAJOR=20
NODE_MIN_MINOR=0

node_new_enough() {
    command -v node >/dev/null 2>&1 || return 1
    local major minor
    major="$(node -p 'process.versions.node.split(".")[0]' 2>/dev/null)" || return 1
    minor="$(node -p 'process.versions.node.split(".")[1]' 2>/dev/null)" || return 1
    if [ "$major" -gt "$NODE_MIN_MAJOR" ]; then
        return 0
    fi
    [ "$major" -eq "$NODE_MIN_MAJOR" ] && [ "$minor" -ge "$NODE_MIN_MINOR" ]
}

if ! node_new_enough; then
    echo "node $(command -v node >/dev/null 2>&1 && node --version || echo 'absent') is below \
v$NODE_MIN_MAJOR.$NODE_MIN_MINOR; installing" >&2
    if [[ "$OS" == "Linux" ]]; then
        sudo apt-get install -y curl ca-certificates
        curl -fsSL https://deb.nodesource.com/setup_22.x | sudo bash -
        sudo apt-get install -y nodejs
    elif [[ "$OS" == "Darwin" ]]; then
        brew install node || brew upgrade node
        # Homebrew's node must win over whatever the runner already had on PATH.
        export PATH="$(brew --prefix)/bin:$PATH"
        brew link --overwrite --force node || true
    fi
fi

node --version && npm --version
if ! node_new_enough; then
    echo "error: node is still below v$NODE_MIN_MAJOR.$NODE_MIN_MINOR; the web build will fail" >&2
    exit 1
fi
