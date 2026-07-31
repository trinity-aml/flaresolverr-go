#!/usr/bin/env bash
set -euo pipefail

BINARY="flaresolverr"
CMD="./cmd/flaresolverr"
OUT="./Dist"

VERSION_PKG="github.com/trinity-aml/flaresolverr-go/internal/buildinfo"

# Stamp the version at link time so a release no longer needs a source edit.
# Falls back to the literal compiled into version.go outside a git checkout.
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || true)}"

LDFLAGS="-s -w"
if [[ -n "${VERSION}" ]]; then
  LDFLAGS="${LDFLAGS} -X ${VERSION_PKG}.Version=${VERSION}"
fi

export GOTOOLCHAIN="${GOTOOLCHAIN:-local}"
export GOCACHE="${GOCACHE:-$PWD/.gocache}"
export GOMODCACHE="${GOMODCACHE:-$PWD/.gomodcache}"

# Platforms: OS/ARCH
TARGETS=(
  linux/amd64
  linux/arm64
  linux/arm
  linux/386
  darwin/amd64
  darwin/arm64
  windows/amd64
  windows/arm64
  windows/386
  freebsd/amd64
  freebsd/arm64
)

mkdir -p "${OUT}"
rm -fr "${OUT:?}"/*
mkdir -p "${GOCACHE}" "${GOMODCACHE}"

# Per-run temp file: a fixed /tmp path collides between concurrent runs and,
# on a shared host, between users.
BUILD_ERR="$(mktemp)"
trap 'rm -f "${BUILD_ERR}"' EXIT

echo "version: ${VERSION:-<compiled-in default>}"
echo ""

OK=0
FAIL=0

for TARGET in "${TARGETS[@]}"; do
  GOOS="${TARGET%/*}"
  GOARCH="${TARGET#*/}"

  NAME="${BINARY}-${GOOS}-${GOARCH}"
  [[ "${GOOS}" == "windows" ]] && NAME="${NAME}.exe"

  OUTFILE="${OUT}/${NAME}"

  printf "  %-30s" "${TARGET}"

  if CGO_ENABLED=0 GOOS="${GOOS}" GOARCH="${GOARCH}" \
      go build -trimpath -ldflags "${LDFLAGS}" -o "${OUTFILE}" "${CMD}" 2>"${BUILD_ERR}"; then
    SIZE=$(du -sh "${OUTFILE}" 2>/dev/null | cut -f1)
    echo "OK  (${SIZE})"
    (( OK++ )) || true
  else
    echo "FAILED"
    sed 's/^/    /' "${BUILD_ERR}"
    (( FAIL++ )) || true
  fi
done

# Checksums for release verification.
if (( OK > 0 )); then
  ( cd "${OUT}" && sha256sum -- * > SHA256SUMS 2>/dev/null ) || true
fi

echo ""
echo "done: ${OK} ok, ${FAIL} failed  →  ${OUT}/"
ls -lh "${OUT}"

(( FAIL == 0 ))
