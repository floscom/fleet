#!/bin/sh
# Install fleet from the GitHub releases (macOS and Linux, amd64 and arm64).
#
#   curl -fsSL https://raw.githubusercontent.com/floscom/fleet/main/install.sh | sh
#
# Environment:
#   FLEET_VERSION      release tag to install, e.g. v0.1.0 (default: latest)
#   FLEET_INSTALL_DIR  target directory (default: /usr/local/bin if writable,
#                      else ~/.local/bin)
set -eu

REPO="floscom/fleet"
VERSION="${FLEET_VERSION:-latest}"

say() { printf '%s\n' "$*"; }
die() { printf 'fleet install: %s\n' "$*" >&2; exit 1; }

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) die "unsupported OS $(uname -s) (only macOS and Linux)" ;;
esac

case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) die "unsupported CPU $(uname -m) (only amd64 and arm64)" ;;
esac

# Rosetta: a shell translated to x86_64 on Apple silicon should still get arm64.
if [ "$os" = darwin ] && [ "$arch" = amd64 ] && [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || echo 0)" = 1 ]; then
  arch=arm64
fi

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -qO "$2" "$1"; }
else
  die "needs curl or wget"
fi

if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  die "needs sha256sum or shasum"
fi

if [ "$VERSION" = latest ]; then
  base="https://github.com/$REPO/releases/latest/download"
else
  base="https://github.com/$REPO/releases/download/$VERSION"
fi

target="fleet-$os-$arch"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

say "Downloading $target ($VERSION)..."
fetch "$base/$target.tar.gz" "$tmp/$target.tar.gz" || die "download failed: $base/$target.tar.gz"
fetch "$base/checksums.txt" "$tmp/checksums.txt" || die "download failed: $base/checksums.txt"

want=$(awk -v f="$target.tar.gz" '$2 == f { print $1 }' "$tmp/checksums.txt")
[ -n "$want" ] || die "$target.tar.gz is not listed in checksums.txt"
[ "$(sha256 "$tmp/$target.tar.gz")" = "$want" ] || die "checksum mismatch for $target.tar.gz"

tar -xzf "$tmp/$target.tar.gz" -C "$tmp"

dir="${FLEET_INSTALL_DIR:-}"
if [ -z "$dir" ]; then
  if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
    dir=/usr/local/bin
  else
    dir="$HOME/.local/bin"
  fi
fi
mkdir -p "$dir"
install -m 755 "$tmp/$target/fleet" "$dir/fleet.tmp"
mv -f "$dir/fleet.tmp" "$dir/fleet"

say "Installed $("$dir/fleet" version) to $dir/fleet"

case ":$PATH:" in
  *":$dir:"*) ;;
  *) say "Note: $dir is not on your PATH. Add it, e.g.: export PATH=\"$dir:\$PATH\"" ;;
esac
command -v tmux >/dev/null 2>&1 || say "Note: fleet needs tmux to run agents; install it first (brew install tmux / apt install tmux)."
command -v git >/dev/null 2>&1 || say "Note: fleet uses git for worktrees; install git."
if pgrep -x fleet >/dev/null 2>&1; then
  say "A fleet daemon is running; restart it to use the new version: fleet stop && fleet start"
fi
