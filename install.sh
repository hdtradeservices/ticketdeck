#!/usr/bin/env bash
# TicketDeck installer — installs the `ticketdeck` binary, the `deck` launcher,
# and its herdr dependency into your PATH.
#
# Run from a clone:      ./install.sh
# Or straight from web:  curl -fsSL https://raw.githubusercontent.com/hdtradeservices/ticketdeck/main/install.sh | bash
#
# Env:
#   BIN_DIR   where to install (default ~/.local/bin)
#   NO_HERDR  set to 1 to skip installing herdr
set -euo pipefail

REPO="hdtradeservices/ticketdeck"
HERDR_REPO="ogulcancelik/herdr"
BIN_DIR="${BIN_DIR:-$HOME/.local/bin}"
RAW="https://raw.githubusercontent.com/${REPO}/main"

say() { printf '\033[36m▸\033[0m %s\n' "$*"; }
die() { printf '\033[31m✗ %s\033[0m\n' "$*" >&2; exit 1; }

mkdir -p "$BIN_DIR"

# ── platform ────────────────────────────────────────────────────────────────
os=$(uname -s); arch=$(uname -m)
case "$os" in Linux) hos=linux ;; Darwin) hos=macos ;; *) die "unsupported OS: $os" ;; esac
case "$arch" in x86_64|amd64) harch=x86_64 ;; arm64|aarch64) harch=aarch64 ;; *) die "unsupported arch: $arch" ;; esac

# ── herdr ───────────────────────────────────────────────────────────────────
if [ "${NO_HERDR:-0}" != "1" ]; then
  if command -v herdr >/dev/null 2>&1; then
    say "herdr already installed ($(command -v herdr))"
  else
    url="https://github.com/${HERDR_REPO}/releases/latest/download/herdr-${hos}-${harch}"
    say "installing herdr from $url"
    curl -fsSL "$url" -o "$BIN_DIR/herdr" || die "failed to download herdr"
    chmod +x "$BIN_DIR/herdr"
  fi
  # Let herdr detect Claude working/blocked/idle (reversible, no-op outside herdr).
  "$BIN_DIR/herdr" integration install claude >/dev/null 2>&1 || true
fi

# ── ticketdeck ──────────────────────────────────────────────────────────────
command -v go >/dev/null 2>&1 || die "Go is required to build ticketdeck (https://go.dev/dl)"
if [ -f "./cmd/ticketdeck/main.go" ]; then
  say "building ticketdeck from source"
  go build -o "$BIN_DIR/ticketdeck" ./cmd/ticketdeck
else
  say "installing ticketdeck via 'go install'"
  GOBIN="$BIN_DIR" go install "github.com/${REPO}/cmd/ticketdeck@latest"
fi

# ── deck launcher ────────────────────────────────────────────────────────────
if [ -f "./scripts/deck" ]; then
  install -m 0755 ./scripts/deck "$BIN_DIR/deck"
else
  curl -fsSL "$RAW/scripts/deck" -o "$BIN_DIR/deck" && chmod +x "$BIN_DIR/deck"
fi

say "installed: ticketdeck, deck$([ "${NO_HERDR:-0}" != 1 ] && echo ", herdr") → $BIN_DIR"
case ":$PATH:" in
  *":$BIN_DIR:"*) : ;;
  *) printf '\033[33m! %s is not on your PATH — add:  export PATH="%s:$PATH"\033[0m\n' "$BIN_DIR" "$BIN_DIR" ;;
esac
cat <<'NEXT'

Next:
  1. export LINEAR_API_KEY=lin_api_...   # https://linear.app/settings/api
  2. deck                                # launch the workspace
NEXT
