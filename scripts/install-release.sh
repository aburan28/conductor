#!/usr/bin/env bash
#
# Download a conductor release from GitHub and install its binaries.
#
#   install-release.sh <repo> <dir> [version]
#
# <repo>     owner/name of the GitHub repository that holds the releases
# <dir>      directory the binaries are installed into (created if missing)
# [version]  release tag to install (default: the repository's latest release)
#
# No Go toolchain is required. The expected assets, produced by
# .github/workflows/release.yml, are (<version> is the tag, with its leading v):
#
#   conductor_<version>_<os>_<arch>.tar.gz  containing conductor_<version>_<os>_<arch>/
#   SHA256SUMS                               checked against the downloaded archive
#
# Each archive also carries a GitHub build-provenance attestation; the script prints the
# command that verifies it. RELEASE_BASE_URL overrides https://github.com/<repo>/releases
# (a mirror, or a local copy when testing the release pipeline).

set -euo pipefail

repo="${1:?usage: install-release.sh <repo> <dir> [version]}"
dest="${2:?usage: install-release.sh <repo> <dir> [version]}"
version="${3:-}"

# Map uname to the GOOS/GOARCH the release workflow builds for.
goos="$(uname -s)"
case "$goos" in
  Darwin) goos=darwin ;;
  Linux)  goos=linux ;;
  *) echo "unsupported OS: $goos (need darwin or linux)" >&2; exit 1 ;;
esac
goarch="$(uname -m)"
case "$goarch" in
  arm64|aarch64) goarch=arm64 ;;
  x86_64|amd64)  goarch=amd64 ;;
  *) echo "unsupported architecture: $goarch (need arm64 or amd64)" >&2; exit 1 ;;
esac

base="${RELEASE_BASE_URL:-https://github.com/${repo}/releases}"
if [[ -z "$version" ]]; then
  # /releases/latest 302-redirects to /releases/tag/<tag>. Reading the
  # Location header needs no JSON parser and no authentication.
  version="$(curl -fsSI "$base/latest" \
    | awk 'tolower($1) == "location:" {n = split($2, p, "/"); print p[n]}' \
    | tr -d '\r\n ')"
fi
[[ -n "$version" ]] || { echo "could not resolve a release for ${repo}" >&2; exit 1; }

asset="conductor_${version}_${goos}_${goarch}.tar.gz"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "downloading ${asset} from ${repo} release ${version}"
curl -fSL --retry 3 -o "$tmp/$asset" "$base/download/${version}/${asset}"
curl -fsSL -o "$tmp/SHA256SUMS" "$base/download/${version}/SHA256SUMS"

# macOS ships shasum rather than sha256sum; both read "hash  file" checksum files.
if command -v sha256sum >/dev/null 2>&1; then
  sha_check() { (cd "$tmp" && sha256sum -c -); }
else
  sha_check() { (cd "$tmp" && shasum -a 256 -c -); }
fi
grep " ${asset}\$" "$tmp/SHA256SUMS" | sha_check || {
  echo "checksum mismatch for ${asset}; refusing to install" >&2
  exit 1
}

tar -xzf "$tmp/$asset" -C "$tmp"
mkdir -p "$dest"
for bin in conductord conductor conductor-mcp; do
  install -m 0755 "$tmp/conductor_${version}_${goos}_${goarch}/$bin" "$dest/$bin"
done
echo "installed conductord, conductor, conductor-mcp from ${version} into ${dest}"
echo "to verify where the archive was built: gh attestation verify ${asset} --repo ${repo}"
