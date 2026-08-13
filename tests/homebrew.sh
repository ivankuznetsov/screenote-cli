#!/usr/bin/env bash
set -euo pipefail

repo_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
test_root=$(mktemp -d "${TMPDIR:-/tmp}/screenote-homebrew-tests.XXXXXX")
trap 'rm -rf "$test_root"' EXIT

checksums="$test_root/checksums.txt"
formula="$test_root/Formula/screenote.rb"
for platform in darwin_amd64 darwin_arm64 linux_amd64 linux_arm64; do
  printf '%064x  screenote_1.2.3_%s.tar.gz\n' "$(( ${#platform} + 1 ))" "$platform" >>"$checksums"
done

"$repo_dir/scripts/render-homebrew-formula" v1.2.3 "$checksums" "$formula"

grep -Fq 'version "1.2.3"' "$formula"
grep -Fq 'screenote_1.2.3_darwin_arm64.tar.gz' "$formula"
grep -Fq 'screenote_1.2.3_linux_amd64.tar.gz' "$formula"
grep -Fq 'bin.install "screenote"' "$formula"

set +e
"$repo_dir/scripts/render-homebrew-formula" latest "$checksums" "$formula" >/dev/null 2>&1
status=$?
set -e
[[ $status -eq 2 ]]

printf 'homebrew formula tests passed\n'
