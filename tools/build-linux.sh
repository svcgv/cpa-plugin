#!/usr/bin/env bash
# Cross-compile the CLIProxyAPI plugins for Linux amd64 using the local zig
# toolchain (zig cc as the C compiler and linker, so no Linux C toolchain or
# osxcross install is needed on macOS).
#
# Usage: tools/build-linux.sh [--fetch]
#   --fetch   download the zig toolchain into tools/zig if it is missing.
#
# zig is a build tool only; tools/zig is git-ignored.
set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
ZIG_DIR="$REPO_ROOT/tools/zig"
ZIG_VERSION="0.14.1"
ZIG_ARCH="aarch64-macos"
ZIG_URL="https://ziglang.org/download/${ZIG_VERSION}/zig-${ZIG_ARCH}-${ZIG_VERSION}.tar.xz"

if [[ "${1:-}" == "--fetch" ]]; then
  if [[ ! -x "$ZIG_DIR/zig" ]]; then
    tmp="$(mktemp -d)"
    trap 'rm -rf "$tmp"' EXIT
    echo "fetching zig ${ZIG_VERSION} ..."
    curl -fL --proxy "${HTTPS_PROXY:-http://127.0.0.1:7890}" \
      -o "$tmp/zig.tar.xz" "$ZIG_URL"
    tar -xJf "$tmp/zig.tar.xz" -C "$tmp"
    mkdir -p "$REPO_ROOT/tools"
    rm -rf "$ZIG_DIR"
    mv "$tmp/zig-${ZIG_ARCH}-${ZIG_VERSION}" "$ZIG_DIR"
  fi
fi

if [[ ! -x "$ZIG_DIR/zig" ]]; then
  echo "zig not found at $ZIG_DIR; rerun with --fetch" >&2
  exit 1
fi

export ZIG="$ZIG_DIR/zig"
export CC="$REPO_ROOT/tools/zigcc"
cat > "$CC" <<'WRAPPER'
#!/usr/bin/env bash
exec "$ZIG" cc -target x86_64-linux-gnu "$@"
WRAPPER
chmod +x "$CC"

export CXX="$REPO_ROOT/tools/zigcxx"
cat > "$CXX" <<'WRAPPER'
#!/usr/bin/env bash
exec "$ZIG" c++ -target x86_64-linux-gnu "$@"
WRAPPER
chmod +x "$CXX"

export CGO_ENABLED=1
export GOOS=linux
export GOARCH=amd64
# Let Go supply its own cgo build flags; overriding CGO_CFLAGS drops the
# -D_GNU_SOURCE / -pthread defaults and breaks the runtime/cgo assembly.
# Only the compiler selection below is needed.

build_one() {
  local dir="$1" name="$2"
  echo "==> building $name (linux/amd64)"
  ( cd "$REPO_ROOT/$dir"
    go build -a -buildmode=c-shared \
      -ldflags "-s -w -X main.version=$(cat VERSION)" \
      -o "$REPO_ROOT/$name.so" . )
}

build_one workbuddy workbuddy
build_one qoderwork qoderwork

echo "==> done"
file "$REPO_ROOT/workbuddy.so" "$REPO_ROOT/qoderwork.so"
