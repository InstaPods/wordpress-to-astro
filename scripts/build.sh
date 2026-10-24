#!/usr/bin/env bash
# Cross-compiles the CLI into dist/<os>-<arch>/ and packs release archives.
#   scripts/build.sh v0.1.0
# NO_ARCHIVE=1 skips the archives and checksums (CI smoke test).
set -euo pipefail
version="${1:?usage: scripts/build.sh vX.Y.Z}"
cd "$(dirname "$0")/.."
out=dist
mkdir -p "$out"
targets="darwin/arm64 darwin/amd64 linux/arm64 linux/amd64 windows/arm64 windows/amd64"
for t in $targets; do
  os="${t%/*}" arch="${t#*/}"
  dir="$out/$os-$arch"
  exe=wordpress-to-astro
  [ "$os" = windows ] && exe=wordpress-to-astro.exe
  mkdir -p "$dir"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
    -ldflags "-s -w -X main.version=$version" -o "$dir/$exe" .
  cp LICENSE README.md "$dir/"
  [ -n "${NO_ARCHIVE:-}" ] && continue
  name="wordpress-to-astro_${version#v}_${os}_${arch}"
  if [ "$os" = windows ]; then
    (cd "$dir" && zip -q -9 "../$name.zip" "$exe" LICENSE README.md)
  else
    tar -C "$dir" -czf "$out/$name.tar.gz" "$exe" LICENSE README.md
  fi
done
[ -z "${NO_ARCHIVE:-}" ] && (cd "$out" && shasum -a 256 *.tar.gz *.zip > checksums.txt)
echo "built $version into $out/"
