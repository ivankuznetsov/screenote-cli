#!/usr/bin/env bash
set -euo pipefail

repo_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
test_root=$(mktemp -d "${TMPDIR:-/tmp}/screenote-aur-tests.XXXXXX")
trap 'rm -rf "$test_root"' EXIT

fake_bin="$test_root/bin"
output_dir="$test_root/aur"
mkdir -p "$fake_bin"

printf '%s\n' \
  '#!/usr/bin/env bash' \
  'set -euo pipefail' \
  'output=' \
  'while (($#)); do' \
  '  if [[ $1 == --output ]]; then' \
  '    output=$2' \
  '    shift 2' \
  '  else' \
  '    shift' \
  '  fi' \
  'done' \
  '[[ -n $output ]]' \
  "printf 'test archive\\n' >\"\$output\"" \
  >"$fake_bin/curl"
chmod +x "$fake_bin/curl"

PATH="$fake_bin:$PATH" \
  "$repo_dir/scripts/render-aur-package" v1.2.3 "$output_dir"

expected_checksum=$(printf 'test archive\n' | sha256sum | cut -d' ' -f1)
grep -Fq 'pkgver=1.2.3' "$output_dir/PKGBUILD"
grep -Fq "sha256sums=('$expected_checksum')" "$output_dir/PKGBUILD"
cmp "$repo_dir/packaging/aur/screenote-cli/.gitignore" "$output_dir/.gitignore"

mkdir -p "$output_dir/src/gopath" "$output_dir/pkg"
touch \
  "$output_dir/src/gopath/module.zip" \
  "$output_dir/pkg/screenote" \
  "$output_dir/screenote-cli-1.2.3-1-x86_64.pkg.tar.zst" \
  "$output_dir/screenote-cli-1.2.3.tar.gz" \
  "$output_dir/.SRCINFO"

git -C "$output_dir" init -q
for ignored_path in \
  src/gopath/module.zip \
  pkg/screenote \
  screenote-cli-1.2.3-1-x86_64.pkg.tar.zst \
  screenote-cli-1.2.3.tar.gz; do
  git -C "$output_dir" check-ignore -q "$ignored_path"
done

for tracked_path in PKGBUILD .SRCINFO; do
  ! git -C "$output_dir" check-ignore -q "$tracked_path"
done

printf 'AUR package rendering tests passed\n'
