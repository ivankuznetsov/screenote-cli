#!/usr/bin/env bash
set -euo pipefail

repo_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
test_root=$(mktemp -d "${TMPDIR:-/tmp}/screenote-installer-tests.XXXXXX")
trap 'rm -rf "$test_root"' EXIT

version=9.8.7
release_dir="$test_root/releases/v$version"
payload_dir="$test_root/payload"
install_dir="$test_root/bin"
mkdir -p "$release_dir" "$payload_dir" "$install_dir"

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) printf 'unsupported test OS\n' >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) printf 'unsupported test architecture\n' >&2; exit 1 ;;
esac

archive="screenote_${version}_${os}_${arch}.tar.gz"
printf '#!/bin/sh\nprintf "screenote %s\\n"\n' "$version" >"$payload_dir/screenote"
chmod 755 "$payload_dir/screenote"
tar -czf "$release_dir/$archive" -C "$payload_dir" screenote
(
  cd "$release_dir"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$archive" >checksums.txt
  else
    shasum -a 256 "$archive" >checksums.txt
  fi
)

output=$(SCREENOTE_VERSION="$version" \
  SCREENOTE_DOWNLOAD_BASE="file://$test_root/releases" \
  SCREENOTE_INSTALL_DIR="$install_dir" \
  PATH="$install_dir:$PATH" \
  sh "$repo_dir/scripts/install.sh")

[[ -x "$install_dir/screenote" ]]
[[ $("$install_dir/screenote") == "screenote $version" ]]
[[ $output == *"Next: screenote login"* ]]

printf '0%.0s' {1..64} >"$release_dir/checksums.txt"
printf '  %s\n' "$archive" >>"$release_dir/checksums.txt"
rm -f "$install_dir/screenote"

set +e
output=$(SCREENOTE_VERSION="$version" \
  SCREENOTE_DOWNLOAD_BASE="file://$test_root/releases" \
  SCREENOTE_INSTALL_DIR="$install_dir" \
  sh "$repo_dir/scripts/install.sh" 2>&1)
status=$?
set -e

[[ $status -ne 0 ]]
[[ ! -e "$install_dir/screenote" ]]
[[ $output == *"checksum verification failed"* ]]

set +e
output=$(SCREENOTE_VERSION="9.8" \
  SCREENOTE_DOWNLOAD_BASE="file://$test_root/releases" \
  SCREENOTE_INSTALL_DIR="$install_dir" \
  sh "$repo_dir/scripts/install.sh" 2>&1)
status=$?
set -e

[[ $status -ne 0 ]]
[[ $output == *"invalid release version: 9.8"* ]]

printf 'installer tests passed\n'
