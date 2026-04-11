#!/usr/bin/env bash
set -euo pipefail

BINARY="flaresolverr"
CMD="./cmd/flaresolverr"
OUT="./Dist"

LDFLAGS="-s -w"
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

rm -fr ${OUT}/*
mkdir -p "${OUT}"
mkdir -p "${GOCACHE}" "${GOMODCACHE}"

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
      go build -trimpath -ldflags "${LDFLAGS}" -o "${OUTFILE}" "${CMD}" 2>/tmp/build_err; then
    SIZE=$(du -sh "${OUTFILE}" 2>/dev/null | cut -f1)
    echo "OK  (${SIZE})"
    (( OK++ )) || true
  else
    echo "FAILED"
    cat /tmp/build_err | sed 's/^/    /'
    (( FAIL++ )) || true
  fi
done

echo ""
echo "done: ${OK} ok, ${FAIL} failed  →  ${OUT}/"
ls -lh "${OUT}"
