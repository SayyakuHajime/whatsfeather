#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ ! -x "$ROOT/build/bin/whatsfeather" || ! -x "$ROOT/build/bin/whatsfeatherd" ]]; then
  printf 'Build first: %s/scripts/build.sh
' "$ROOT" >&2
  exit 1
fi
exec "$ROOT/build/bin/whatsfeather" "$@"
