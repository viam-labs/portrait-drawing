#!/bin/bash
# Runs once when the module is installed on a target machine. Installs
# system deps required by armplanning.

set -euo pipefail

cd "$(dirname "$0")"

bash ./setup.sh
