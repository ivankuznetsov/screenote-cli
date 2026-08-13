#!/bin/sh
set -eu

repository="ivankuznetsov/screenote-cli"
download_base="${SCREENOTE_DOWNLOAD_BASE:-https://github.com/$repository/releases/download}"
install_dir="${SCREENOTE_INSTALL_DIR:-}"
version="${SCREENOTE_VERSION:-}"

fail() {
  printf 'screenote installer: %s\n' "$*" >&2
  exit 1
}

command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v tar >/dev/null 2>&1 || fail "tar is required"
command -v install >/dev/null 2>&1 || fail "install is required"

case "$(uname -s)" in
  Darwin) os="darwin" ;;
  Linux) os="linux" ;;
  *) fail "unsupported operating system: $(uname -s)" ;;
esac

case "$(uname -m)" in
  x86_64 | amd64) arch="amd64" ;;
  arm64 | aarch64) arch="arm64" ;;
  *) fail "unsupported architecture: $(uname -m)" ;;
esac

if [ -z "$version" ]; then
  latest_url="$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/$repository/releases/latest")"
  version="${latest_url##*/}"
fi
version="${version#v}"
printf '%s\n' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' || fail "invalid release version: $version"

archive="screenote_${version}_${os}_${arch}.tar.gz"
release_url="$download_base/v$version"
tmp_root="${TMPDIR:-/tmp}"
tmp_dir="$(mktemp -d "$tmp_root/screenote-install.XXXXXX")"
trap 'rm -rf "$tmp_dir"' EXIT HUP INT TERM

curl -fsSL "$release_url/$archive" -o "$tmp_dir/$archive"
curl -fsSL "$release_url/checksums.txt" -o "$tmp_dir/checksums.txt"

expected="$(awk -v file="$archive" '$2 == file || $2 == "./" file { print $1; exit }' "$tmp_dir/checksums.txt")"
[ -n "$expected" ] || fail "checksums.txt does not contain $archive"

if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "$tmp_dir/$archive" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
  actual="$(shasum -a 256 "$tmp_dir/$archive" | awk '{print $1}')"
else
  fail "sha256sum or shasum is required"
fi
[ "$actual" = "$expected" ] || fail "checksum verification failed for $archive"

tar -xzf "$tmp_dir/$archive" -C "$tmp_dir" screenote
[ -f "$tmp_dir/screenote" ] || fail "release archive does not contain screenote"

path_contains() {
  case ":${PATH:-}:" in
    *":$1:"*) return 0 ;;
    *) return 1 ;;
  esac
}

if [ -z "$install_dir" ]; then
  if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
    install_dir="/usr/local/bin"
  elif path_contains "$HOME/.local/bin"; then
    install_dir="$HOME/.local/bin"
  elif path_contains "$HOME/bin"; then
    install_dir="$HOME/bin"
  elif command -v sudo >/dev/null 2>&1; then
    install_dir="/usr/local/bin"
  else
    install_dir="$HOME/.local/bin"
  fi
fi

if mkdir -p "$install_dir" 2>/dev/null && [ -w "$install_dir" ]; then
  install -m 0755 "$tmp_dir/screenote" "$install_dir/screenote"
elif command -v sudo >/dev/null 2>&1; then
  sudo install -d -m 0755 "$install_dir"
  sudo install -m 0755 "$tmp_dir/screenote" "$install_dir/screenote"
else
  fail "cannot write to $install_dir; set SCREENOTE_INSTALL_DIR to a writable directory on PATH"
fi

printf 'Installed Screenote %s to %s/screenote\n' "$version" "$install_dir"
if path_contains "$install_dir"; then
  printf 'Next: screenote login\n'
else
  printf 'Add %s to PATH, then run: screenote login\n' "$install_dir"
fi
