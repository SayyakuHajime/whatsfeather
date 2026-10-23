#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
(cd "$ROOT/backend" && go build -trimpath -o "$ROOT/build/bin/whatsfeatherd" ./cmd/whatsfeatherd)
cmake -S "$ROOT" -B "$ROOT/build" -DCMAKE_BUILD_TYPE=Release -DCMAKE_EXPORT_COMPILE_COMMANDS=ON
cmake --build "$ROOT/build" --parallel "${BUILD_JOBS:-2}"
