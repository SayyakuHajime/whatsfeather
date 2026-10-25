# Local pre-alpha snapshot only; this recipe is not approved for publication or installation.
# Release gates still open: FFmpeg advisory closure, complete dependency audit, signing,
# and clean-machine install/upgrade/rollback validation.
pkgname=whatsfeather-prealpha
pkgver=0.0.1.r12.ge86b53b
pkgrel=1
pkgdesc='Pre-alpha native WhatsApp client for Linux (not a production release)'
arch=('x86_64')
url='https://github.com/SayyakuHajime/whatsfeather'
license=('GPL-3.0-only')
provides=('whatsfeather')
conflicts=('whatsfeather')

# Qt 6.12 is the currently reviewed Arch baseline for the Qt runtime components.
# FFmpeg remains an unresolved security gate; keep this package local and unsigned.
depends=(
  'ffmpeg>=2:9.0.3'
  'qt6-base>=6.12.0'
  'qt6-declarative>=6.12.0'
  'qt6-multimedia>=6.12.0'
  'qt6-svg>=6.12.0'
)
makedepends=(
  'cmake>=3.22'
  'go>=2:1.27.3'
  'ninja'
  'python'
  'qt6-base>=6.12.0'
  'qt6-declarative>=6.12.0'
  'qt6-multimedia>=6.12.0'
)

_source_commit='e86b53bb8771901bfc7873a21ddb610a1f43168f'
source=(
  "$pkgname-$pkgver.tar.gz::https://github.com/SayyakuHajime/whatsfeather/archive/${_source_commit}.tar.gz"
)
sha256sums=('4e23b82721b857f3a712b7d0a06c38f7c7bcf339303754ee5078b5d31e027be9')

prepare() {
  local source_dir="$srcdir/whatsfeather-${_source_commit}"
  [[ -s "$source_dir/backend/vendor.tar.gz" ]] || { error 'Pinned Go dependency archive is missing'; return 1; }
  tar -xzf "$source_dir/backend/vendor.tar.gz" -C "$source_dir/backend"
}

build() {
  local jobs="${BUILD_JOBS:-2}"
  [[ "$jobs" =~ ^[1-9][0-9]*$ ]] || { error 'BUILD_JOBS must be a positive integer'; return 1; }

  local source_dir="$srcdir/whatsfeather-${_source_commit}"
  local build_dir="$srcdir/build"
  mkdir -p "$build_dir/bin"

  (
    cd "$source_dir/backend"
    GOTOOLCHAIN=local GOPROXY=off go build -mod=vendor -trimpath \
      -o "$build_dir/bin/whatsfeatherd" ./cmd/whatsfeatherd
  )

  cmake -S "$source_dir" -B "$build_dir" -G Ninja \
    -DCMAKE_BUILD_TYPE=Release \
    -DCMAKE_INSTALL_PREFIX=/usr
  cmake --build "$build_dir" --target whatsfeather --parallel "$jobs"
}

package() {
  local source_dir="$srcdir/whatsfeather-${_source_commit}"
  DESTDIR="$pkgdir" cmake --install "$srcdir/build" --prefix /usr || return 1

  local required
  for required in \
    usr/bin/whatsfeather \
    usr/bin/whatsfeatherd \
    usr/share/applications/whatsfeather.desktop \
    usr/share/licenses/WhatsFeather/LICENSE \
    usr/share/licenses/WhatsFeather/ASSET-LICENSES.md \
    usr/share/licenses/WhatsFeather/THIRD-PARTY-NOTICES.md; do
    [[ -s "$pkgdir/$required" ]] || { error "CMake install omitted $required"; return 1; }
  done

  local size
  for size in 16 24 32 48 64 128 256 512; do
    [[ -s "$pkgdir/usr/share/icons/hicolor/${size}x${size}/apps/whatsfeather.png" ]] || {
      error "CMake install omitted the ${size}x${size} application icon"
      return 1
    }
  done

  # CMake install(FILES ...) reads desktop assets and licenses from this tree.
  [[ -d "$source_dir" ]] || { error 'Pinned source directory is missing'; return 1; }
}
