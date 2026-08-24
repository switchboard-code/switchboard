#!/bin/sh
# Switchboard installer. Usage:
#
#   curl -fsSL https://raw.githubusercontent.com/switchboard-code/switchboard/main/install.sh | bash
#
# Downloads the latest release (or SB_VERSION=vX.Y.Z for a specific one),
# verifies it against the release's checksums, and installs sb into
# SB_INSTALL_DIR or ~/.local/bin. No sudo: if you want /usr/local/bin, set
# SB_INSTALL_DIR and take responsibility for it.
set -eu
LC_ALL=C
export LC_ALL

REPO="switchboard-code/switchboard"
INSTALL_DIR="${SB_INSTALL_DIR:-$HOME/.local/bin}"
MAX_RELEASE_BYTES=1048576
MAX_CHECKSUM_BYTES=65536
MAX_ARCHIVE_BYTES=134217728
MAX_BINARY_BYTES=268435456

die() { echo "install: $*" >&2; exit 1; }

file_size() {
  size=$(wc -c < "$1" | tr -d '[:space:]')
  case "$size" in
    ''|*[!0-9]*) die "could not measure downloaded file" ;;
  esac
  printf '%s\n' "$size"
}

download_bounded() {
  url=$1
  output=$2
  limit=$3
  description=$4
  curl -fsSL --max-filesize "$limit" "$url" -o "$output" || die "could not download $description"
  size=$(file_size "$output")
  [ "$size" -le "$limit" ] || die "$description exceeds the $limit-byte limit"
}

valid_release_tag() {
  [ "${#1}" -le 128 ] || return 1
  printf '%s\n' "$1" | awk '
    $0 !~ /^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$/ { exit 1 }
    {
      value = substr($0, 2)
      plus = index(value, "+")
      if (plus > 0) value = substr(value, 1, plus - 1)
      dash = index(value, "-")
      prerelease = ""
      if (dash > 0) {
        prerelease = substr(value, dash + 1)
        value = substr(value, 1, dash - 1)
      }
      count = split(value, core, "[.]")
      if (count != 3) exit 1
      for (i = 1; i <= count; i++) {
        if (length(core[i]) > 1 && substr(core[i], 1, 1) == "0") exit 1
        if (length(core[i]) > 20 || (length(core[i]) == 20 && ("x" core[i]) > "x18446744073709551615")) exit 1
      }
      if (length(prerelease) > 0) {
        count = split(prerelease, identifiers, "[.]")
        for (i = 1; i <= count; i++) {
          if (identifiers[i] ~ /^[0-9]+$/ && length(identifiers[i]) > 1 && substr(identifiers[i], 1, 1) == "0") exit 1
        }
      }
    }
    END { if (NR != 1) exit 1 }
  '
}

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$OS" in
  darwin|linux) ;;
  *) die "unsupported OS $OS; on Windows, download from https://github.com/$REPO/releases" ;;
esac
case "$ARCH" in
  x86_64|amd64) ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) die "unsupported architecture $ARCH" ;;
esac

command -v curl >/dev/null || die "curl is required"
if command -v sha256sum >/dev/null; then
  sha256() { sha256sum "$@" | cut -d' ' -f1; }
elif command -v shasum >/dev/null; then
  sha256() { shasum -a 256 "$@" | cut -d' ' -f1; }
else
  die "sha256sum or shasum is required; the checksum is not optional"
fi

TMP=$(mktemp -d)
STAGED=""
cleanup() {
  status=$?
  set +e
  [ -z "$STAGED" ] || rm -f -- "$STAGED"
  [ -z "$TMP" ] || rm -rf -- "$TMP"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

TAG="${SB_VERSION:-}"
if [ -z "$TAG" ]; then
  download_bounded "https://api.github.com/repos/$REPO/releases/latest" "$TMP/release.json" "$MAX_RELEASE_BYTES" "release metadata"
  TAG=$(sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' "$TMP/release.json" | head -1)
  [ -n "$TAG" ] || die "could not determine the latest release"
fi
valid_release_tag "$TAG" || die "release tag is not a canonical semantic version"

VERSION="${TAG#v}"
ASSET="sb_${VERSION}_${OS}_${ARCH}.tar.gz"
BASE="https://github.com/$REPO/releases/download/$TAG"

echo "installing switchboard $TAG ($OS/$ARCH)"
download_bounded "$BASE/$ASSET" "$TMP/$ASSET" "$MAX_ARCHIVE_BYTES" "build for $OS/$ARCH at $TAG"
download_bounded "$BASE/checksums.txt" "$TMP/checksums.txt" "$MAX_CHECKSUM_BYTES" "checksums.txt"

WANT=$(awk -v asset="$ASSET" '
  {
    line = $0
    sub(/\r$/, "", line)
    if (line == "") next
    sum = substr(line, 1, 64)
    marker = substr(line, 65, 2)
    file = substr(line, 67)
    if (length(line) < 67 || length(sum) != 64 || sum ~ /[^0-9A-Fa-f]/ || (marker != "  " && marker != " *") || file == "") {
      invalid = 1
      next
    }
    if (file == asset) { count++; found = tolower(sum) }
  }
  END {
    if (invalid || count != 1) exit 1
    print found
  }
' "$TMP/checksums.txt") || die "checksums.txt is malformed or does not have exactly one entry for $ASSET"
GOT=$(sha256 "$TMP/$ASSET")
[ "$GOT" = "$WANT" ] || die "checksum mismatch; nothing was installed"

mkdir -p "$INSTALL_DIR"
[ ! -d "$INSTALL_DIR/sb" ] || die "$INSTALL_DIR/sb is a directory; refusing to replace it"
MEMBERS=$(tar -tzf "$TMP/$ASSET") || die "could not inspect $ASSET"
[ "$MEMBERS" = "sb" ] || die "$ASSET must contain exactly one member named sb"
MEMBER_TYPE=$(tar -tvzf "$TMP/$ASSET" | awk 'NR == 1 { type = substr($1, 1, 1) } END { if (NR != 1) exit 1; print type }') || die "could not inspect the member type in $ASSET"
[ "$MEMBER_TYPE" = "-" ] || die "$ASSET member sb must be a regular file"
STAGED=$(mktemp "$INSTALL_DIR/.sb-install.XXXXXX") || die "could not stage the new binary beside its destination"
# POSIX shells express `ulimit -f` in 512-byte blocks, while Bash uses
# 1024-byte blocks even in a script whose portable entry point is /bin/sh.
# The documented curl path runs Bash, so account for its actual unit instead
# of accidentally allowing twice the advertised extraction bound.
ULIMIT_FILE_BLOCK_BYTES=512
if [ -n "${BASH_VERSION:-}" ]; then
  ULIMIT_FILE_BLOCK_BYTES=1024
fi
MAX_BINARY_BLOCKS=$((MAX_BINARY_BYTES / ULIMIT_FILE_BLOCK_BYTES))
(ulimit -f "$MAX_BINARY_BLOCKS" && tar -xOzf "$TMP/$ASSET" sb) > "$STAGED" || die "could not extract a bounded sb from $ASSET"
BINARY_SIZE=$(file_size "$STAGED")
[ "$BINARY_SIZE" -gt 0 ] && [ "$BINARY_SIZE" -le "$MAX_BINARY_BYTES" ] || die "release binary is empty or exceeds the $MAX_BINARY_BYTES-byte limit"
chmod 755 "$STAGED"
mv -f "$STAGED" "$INSTALL_DIR/sb"
STAGED=""

echo "installed to $INSTALL_DIR/sb"
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) echo "note: $INSTALL_DIR is not on your PATH" ;;
esac
