#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"

ENGINE="${WHATSFEATHER_TEST_GCRYPTFS:-}"
if [[ -z "$ENGINE" ]]; then
  printf 'ERROR: opt-in encrypted-profile acceptance requires WHATSFEATHER_TEST_GCRYPTFS.\n' >&2
  exit 2
fi
if [[ "$ENGINE" != /* || ! -f "$ENGINE" || ! -x "$ENGINE" ]]; then
  printf 'ERROR: WHATSFEATHER_TEST_GCRYPTFS must name an absolute executable regular file.\n' >&2
  exit 2
fi
UNMOUNT="${WHATSFEATHER_TEST_FUSERMOUNT3:-/usr/sbin/fusermount3}"
if [[ "$UNMOUNT" != /* || ! -f "$UNMOUNT" || ! -x "$UNMOUNT" ]]; then
  printf 'ERROR: normal FUSE unmount helper is missing or invalid: %s\n' "$UNMOUNT" >&2
  exit 2
fi
if [[ ! -c /dev/fuse ]]; then
  printf 'ERROR: real-FUSE acceptance was requested but /dev/fuse is unavailable.\n' >&2
  exit 2
fi

cmake -S "$ROOT" -B "$ROOT/build" -DCMAKE_BUILD_TYPE=Release
cmake --build "$ROOT/build" --target feather-profile-storage-tests --parallel "${BUILD_JOBS:-2}"
NATIVE_TEST_BIN="$ROOT/build/bin/feather-profile-storage-tests"
if [[ ! -f "$NATIVE_TEST_BIN" || ! -x "$NATIVE_TEST_BIN" ]]; then
  printf 'ERROR: focused native storage test executable was not built: %s\n' "$NATIVE_TEST_BIN" >&2
  exit 2
fi

export WHATSFEATHER_RUN_NATIVE_ENCRYPTED_ACCEPTANCE=1
export WHATSFEATHER_NATIVE_PROFILE_TEST_BIN="$NATIVE_TEST_BIN"
export WHATSFEATHER_TEST_FUSERMOUNT3="$UNMOUNT"
export QT_QPA_PLATFORM=offscreen
export QT_FORCE_STDERR_LOGGING=1
printf 'Running real gocryptfs/FUSE + native Controller close/reopen acceptance.\n'
printf 'Engine: %s\nNative test: %s\n' "$ENGINE" "$NATIVE_TEST_BIN"
(cd "$ROOT/backend" && go test -race -count=1 -v ./cmd/whatsfeatherd -run '^(TestEncryptedDaemonRPCMediaSurvivesFUSECloseAndReopen|TestEncryptedDaemonRPCMigratedLegacyProfileSurvivesFUSECloseAndReopen)$')
printf 'PASS: real FUSE/native encrypted-profile acceptance completed without a skipped gate.\n'
