#!/bin/sh
# PairDesk installer: downloads the latest release for your CPU, verifies the
# checksum, and puts `pairdesk` in ~/.local/bin. Nothing runs as root.
set -eu
REPO="Kumarsujay03/pairdesk"
BIN_DIR="${PAIRDESK_BIN_DIR:-$HOME/.local/bin}"
case "$(uname -s)" in Linux) os=linux ;; *) echo "PairDesk runs on Linux only." >&2; exit 1 ;; esac
case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) echo "unsupported CPU: $(uname -m)" >&2; exit 1 ;; esac
need() { command -v "$1" >/dev/null 2>&1 || { echo "need $1" >&2; exit 1; }; }
need curl; need tar; need sha256sum
# github.com/.../releases/latest redirects to the tag page; unlike the API it
# is not rate limited per IP, so shared networks do not get 403s.
tag="$(curl -fsSI -o /dev/null -w '%{redirect_url}' "https://github.com/$REPO/releases/latest" | sed -n 's#.*/releases/tag/##p')"
[ -n "$tag" ] || tag="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)"
[ -n "$tag" ] || { echo "could not find the latest release (is github.com reachable?)" >&2; exit 1; }
asset="pairdesk_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$tag"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
echo "• downloading PairDesk $tag for $os/$arch"
curl -fsSL -o "$tmp/$asset" "$base/$asset"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"
( cd "$tmp" && grep " [*]*$asset\$" checksums.txt | sha256sum -c --quiet - ) || { echo "checksum mismatch, aborting" >&2; exit 1; }
tar -xzf "$tmp/$asset" -C "$tmp" pairdesk
mkdir -p "$BIN_DIR"
install -m 0755 "$tmp/pairdesk" "$BIN_DIR/pairdesk"
echo "• installed to $BIN_DIR/pairdesk"
case ":$PATH:" in *":$BIN_DIR:"*) ;; *) echo "• add to PATH:  export PATH=\"$BIN_DIR:\$PATH\"" ;; esac
command -v bwrap >/dev/null 2>&1 || echo "• recommended for hosting sessions: install bubblewrap (apt/dnf/pacman: bubblewrap); PairDesk falls back to unshare"
if [ ! -w /dev/uinput ] 2>/dev/null; then
  echo "• optional, for screen control sessions: sudo usermod -aG input \$USER  (then log out and in)"
fi
echo "• run: pairdesk"
