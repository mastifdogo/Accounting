#!/bin/sh
# Downloads the official Node.js runtime for a release and verifies it
# against the published SHA256SUMS:
#   backend/fetch-node.sh 24.21.0 amd64 <output dir>
# Writes <output dir>/node and <output dir>/LICENSE.node. Downloads are
# cached in dist/cache.
set -eu
version=${1:?Node.js version, e.g. 24.21.0}
arch=${2:?amd64 or arm64}
out=${3:?output directory}

case "$arch" in
amd64) narch=x64 ;;
arm64) narch=arm64 ;;
*) echo "unsupported architecture: $arch" >&2; exit 1 ;;
esac

repo=$(cd "$(dirname "$0")/.." && pwd)
cache="$repo/dist/cache"
name="node-v$version-linux-$narch"
base="https://nodejs.org/dist/v$version"
mkdir -p "$cache" "$out"

if [ ! -f "$cache/$name.tar.xz" ]; then
	curl -fsSL --retry 3 -o "$cache/SHASUMS256-$version.txt" "$base/SHASUMS256.txt"
	curl -fsSL --retry 3 -o "$cache/$name.tar.xz.part" "$base/$name.tar.xz"
	want=$(awk -v f="$name.tar.xz" '$2 == f { print $1 }' "$cache/SHASUMS256-$version.txt")
	got=$(sha256sum "$cache/$name.tar.xz.part" | cut -d' ' -f1)
	if [ -z "$want" ] || [ "$want" != "$got" ]; then
		echo "checksum mismatch for $name.tar.xz (want '$want', got '$got')" >&2
		rm -f "$cache/$name.tar.xz.part"
		exit 1
	fi
	mv "$cache/$name.tar.xz.part" "$cache/$name.tar.xz"
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
tar -xJf "$cache/$name.tar.xz" -C "$tmp" "$name/bin/node" "$name/LICENSE"
install -m 0755 "$tmp/$name/bin/node" "$out/node"
install -m 0644 "$tmp/$name/LICENSE" "$out/LICENSE.node"
echo "Node.js $version ($narch) -> $out/node"
