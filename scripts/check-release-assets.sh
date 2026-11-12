#!/usr/bin/env bash
# check-release-assets.sh: check a directory of kx release files against the
# release-artifact contract that the install scripts and the
# Kubernetix-AI/deploy Action rely on.
#
# Usage: scripts/check-release-assets.sh [--exact] [--version X.Y.Z] DIR
#
#   DIR          goreleaser's dist/ directory, or a folder of downloaded assets
#   --exact      DIR must hold the seven contract files and nothing else
#   --version V  also run this machine's kx from its archive and check that
#                `kx version` reports V (needs a linux or darwin, amd64 or
#                arm64 machine)
#
# Checks: all six archives and checksums.txt exist; checksums.txt lists exactly
# the six archives in sha256sum format and every checksum matches; each archive
# holds only kx (kx.exe for Windows) at its root, as a regular file.
set -euo pipefail

ARCHIVES=(
  kx_linux_amd64.tar.gz
  kx_linux_arm64.tar.gz
  kx_darwin_amd64.tar.gz
  kx_darwin_arm64.tar.gz
  kx_windows_amd64.zip
  kx_windows_arm64.zip
)

fail() {
  echo "check-release-assets: $*" >&2
  exit 1
}

usage() {
  sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//' >&2
  exit 2
}

exact=false
want_version=""
dir=""
while [ $# -gt 0 ]; do
  case "$1" in
    --exact) exact=true ;;
    --version)
      [ $# -ge 2 ] || usage
      want_version="$2"
      shift
      ;;
    -h | --help) usage ;;
    -*) usage ;;
    *)
      [ -z "$dir" ] || usage
      dir="$1"
      ;;
  esac
  shift
done
[ -n "$dir" ] || usage
[ -d "$dir" ] || fail "$dir is not a directory"

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# 1. Every contract file is present.
for f in "${ARCHIVES[@]}" checksums.txt; do
  [ -f "$dir/$f" ] || fail "missing $f"
  [ ! -L "$dir/$f" ] || fail "$f is a symlink"
done

if $exact; then
  extra=$(find "$dir" -mindepth 1 -maxdepth 1 \
    ! -name checksums.txt ! -name 'kx_*_*.tar.gz' ! -name 'kx_windows_*.zip' -print)
  [ -z "$extra" ] || fail "unexpected files in $dir: $extra"
  count=$(find "$dir" -mindepth 1 -maxdepth 1 | wc -l | tr -d ' ')
  [ "$count" -eq 7 ] || fail "expected 7 files in $dir, found $count"
fi

# 2. checksums.txt lists exactly the six archives, each once, in sha256sum
#    format, and every checksum matches.
lines=0
while IFS= read -r line || [ -n "$line" ]; do
  lines=$((lines + 1))
  if ! [[ "$line" =~ ^([0-9a-f]{64})\ \ ([A-Za-z0-9._-]+)$ ]]; then
    fail "checksums.txt line $lines is not '<sha256>  <file>'"
  fi
  sum="${BASH_REMATCH[1]}"
  name="${BASH_REMATCH[2]}"
  known=false
  for a in "${ARCHIVES[@]}"; do
    if [ "$a" = "$name" ]; then known=true; fi
  done
  $known || fail "checksums.txt lists $name, which is not a release archive"
  got=$(sha256_of "$dir/$name")
  [ "$got" = "$sum" ] || fail "checksum mismatch for $name"
done <"$dir/checksums.txt"
[ "$lines" -eq "${#ARCHIVES[@]}" ] || fail "checksums.txt has $lines lines, want ${#ARCHIVES[@]}"
for a in "${ARCHIVES[@]}"; do
  n=$(awk -v f="$a" '$2 == f' "$dir/checksums.txt" | wc -l | tr -d ' ')
  [ "$n" -eq 1 ] || fail "checksums.txt lists $a $n times"
done

# 3. Each archive holds only the binary, at its root, as a regular file.
for a in "${ARCHIVES[@]}"; do
  case "$a" in
    *.tar.gz)
      listing=$(tar -tzvf "$dir/$a")
      names=$(tar -tzf "$dir/$a")
      [ "$names" = "kx" ] || fail "$a must hold only kx at its root, holds: $names"
      case "$listing" in
        -*) ;;
        *) fail "kx in $a is not a regular file" ;;
      esac
      ;;
    *.zip)
      names=$(unzip -Z1 "$dir/$a")
      [ "$names" = "kx.exe" ] || fail "$a must hold only kx.exe at its root, holds: $names"
      entry=$(unzip -Z "$dir/$a" | awk '$NF == "kx.exe"')
      case "$entry" in
        -*) ;;
        *) fail "kx.exe in $a is not a regular file" ;;
      esac
      ;;
  esac
done

# 4. Optionally run this machine's binary.
if [ -n "$want_version" ]; then
  os=$(uname -s | tr '[:upper:]' '[:lower:]')
  case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) fail "--version needs an amd64 or arm64 machine" ;;
  esac
  case "$os" in
    linux | darwin) ;;
    *) fail "--version needs a linux or darwin machine" ;;
  esac
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  tar -xzf "$dir/kx_${os}_${arch}.tar.gz" -C "$tmp" kx
  out=$("$tmp/kx" version)
  case "$out" in
    "kx $want_version ("*", ${os}/${arch})") ;;
    *) fail "kx version printed '$out', want 'kx $want_version (<commit>, <date>, ${os}/${arch})'" ;;
  esac
  echo "$out"
fi

echo "check-release-assets: OK (${#ARCHIVES[@]} archives and checksums.txt in $dir)"
