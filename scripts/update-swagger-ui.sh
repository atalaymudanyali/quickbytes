#!/usr/bin/env bash
# Downloads one pinned Swagger UI release from npm, checks it against npm's
# published SHA-512 checksum, and copies the few files /docs needs into
# api/swaggerui/ (they're embedded in the binary from there).
#
# To upgrade: change VERSION, run `make swagger-ui`, try /docs, commit.
set -euo pipefail

VERSION="5.17.14"
FILES=(swagger-ui.css swagger-ui-bundle.js favicon-16x16.png favicon-32x32.png LICENSE NOTICE)

root="$(cd "$(dirname "$0")/.." && pwd)"
dest="$root/api/swaggerui"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

meta="https://registry.npmjs.org/swagger-ui-dist/$VERSION"
tarball="$(curl -fsS "$meta" | python3 -c 'import sys, json; print(json.load(sys.stdin)["dist"]["tarball"])')"
expected="$(curl -fsS "$meta" | python3 -c 'import sys, json; print(json.load(sys.stdin)["dist"]["integrity"])')"

curl -fsSL "$tarball" -o "$tmp/swagger-ui.tgz"

# npm's "integrity" is "sha512-" followed by the base64 SHA-512 of the tarball.
actual="sha512-$(openssl dgst -sha512 -binary "$tmp/swagger-ui.tgz" | openssl base64 -A)"
if [[ "$actual" != "$expected" ]]; then
  echo "checksum mismatch for swagger-ui-dist $VERSION: refusing to use it" >&2
  echo "  expected $expected" >&2
  echo "  got      $actual" >&2
  exit 1
fi

mkdir -p "$tmp/unpacked"
tar -xzf "$tmp/swagger-ui.tgz" -C "$tmp/unpacked"

mkdir -p "$dest"
for f in "${FILES[@]}"; do
  cp "$tmp/unpacked/package/$f" "$dest/$f"
done
echo "$VERSION" > "$dest/VERSION"

echo "swagger-ui-dist $VERSION verified and copied to api/swaggerui/"
