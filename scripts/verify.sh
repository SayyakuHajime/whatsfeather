#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
# Every run owns its evidence; never overwrite an earlier QA profile/capture.
umask 077
export WHATSFEATHER_QA_ROOT
WHATSFEATHER_QA_ROOT="$(python3 -c 'import tempfile; print(tempfile.mkdtemp(prefix="wf-evidence-"))')"
printf 'Verification evidence: %s\n' "$WHATSFEATHER_QA_ROOT"
"$ROOT/scripts/build.sh"
(cd "$ROOT/backend" && go test -race ./... && go vet ./...)
ctest --test-dir "$ROOT/build" --output-on-failure
python3 "$ROOT/tests/test_theme.py"
python3 "$ROOT/tests/test_app_icon.py"
python3 "$ROOT/tests/test_asset_license_manifest.py"
python3 "$ROOT/tests/test_chat_wallpaper.py"
python3 "$ROOT/tests/integration/test_rpc.py"
python3 "$ROOT/tests/test_live_test_scope_cli.py"
python3 "$ROOT/tests/test_live_test_scope_profiles.py"
python3 "$ROOT/tests/integration/test_desktop.py"
python3 "$ROOT/tests/integration/test_settings_theme_connection.py"
python3 "$ROOT/tests/integration/test_community_folders.py"
python3 "$ROOT/tests/integration/test_media_download.py"
python3 "$ROOT/tests/integration/test_chat_creation_roster.py"
python3 "$ROOT/tests/integration/test_contact_cards.py"
python3 "$ROOT/tests/integration/test_location.py"
python3 "$ROOT/tests/integration/test_poll_card.py"
python3 "$ROOT/tests/integration/test_poll_daemon_ui.py"
python3 "$ROOT/tests/integration/test_list_message.py"
python3 "$ROOT/tests/integration/test_buttons_message.py"
python3 "$ROOT/tests/test_text_card.py"
python3 "$ROOT/tests/integration/test_text_cards.py"
python3 "$ROOT/tests/integration/test_install.py"
python3 "$ROOT/tests/test_verification_isolation.py"
python3 "$ROOT/tests/test_benchmark_backend.py"
python3 "$ROOT/tests/test_benchmark_desktop.py"
python3 "$ROOT/tests/test_release_checks.py"
python3 "$ROOT/tests/test_dependency_inventory.py"
python3 "$ROOT/scripts/check-release.py" --mode verify
python3 "$ROOT/scripts/dependency-inventory.py" --check --format text
mkdir -p "$WHATSFEATHER_QA_ROOT/config" "$WHATSFEATHER_QA_ROOT/data" "$WHATSFEATHER_QA_ROOT/cache"
# Unix socket paths are bounded: do not nest runtime under the evidence root.
QA_RUNTIME="$(python3 -c 'import tempfile; print(tempfile.mkdtemp(prefix="wf-qa-"))')"
trap 'rm -rf -- "$QA_RUNTIME"' EXIT
export XDG_RUNTIME_DIR="$QA_RUNTIME"
export XDG_CONFIG_HOME="$WHATSFEATHER_QA_ROOT/config"
export XDG_DATA_HOME="$WHATSFEATHER_QA_ROOT/data"
export XDG_CACHE_HOME="$WHATSFEATHER_QA_ROOT/cache"
export QT_QPA_PLATFORM=offscreen QT_QUICK_BACKEND=software QT_FORCE_STDERR_LOGGING=1
"$ROOT/build/bin/whatsfeather" --demo --ui-test --theme dark
"$ROOT/build/bin/whatsfeather" --demo --screenshot "$WHATSFEATHER_QA_ROOT/dark.png" --theme dark --quit-after 2600
"$ROOT/build/bin/whatsfeather" --demo --screenshot "$WHATSFEATHER_QA_ROOT/light.png" --theme light --quit-after 2600
printf 'PASS: build, race/vet, native core, private-RPC integration, native UI smoke, two theme captures.
'
