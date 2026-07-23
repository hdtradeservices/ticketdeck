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

  # Add the "Ctrl+b i" popup that shows the current ticket's description from
  # inside its session (idempotent; skipped if you already bind prefix+i).
  cfg="${XDG_CONFIG_HOME:-$HOME/.config}/herdr/config.toml"
  if ! { [ -f "$cfg" ] && { grep -q 'ticketdeck: describe keybind' "$cfg" || grep -qE 'key *= *"prefix\+i"' "$cfg"; }; }; then
    mkdir -p "$(dirname "$cfg")"
    cat >> "$cfg" <<'TOML'

# >>> ticketdeck: describe keybind — view the current ticket's description in a
# popup (Ctrl+b then i). `ticketdeck describe` resolves the ticket from the pane
# you invoked it from (HERDR_ACTIVE_PANE_ID), so it works inside a ticket session.
[[keys.command]]
key = "prefix+i"
command = "bash -lc 'ticketdeck describe | less -R'"
type = "popup"
width = "80%"
height = "80%"
description = "TicketDeck: show this ticket's description"
# <<< ticketdeck
TOML
    say "added Ctrl+b i (ticketdeck describe) keybind to herdr config"
    "$BIN_DIR/herdr" server reload-config >/dev/null 2>&1 || true
  fi
fi

# ── ticketdeck ──────────────────────────────────────────────────────────────
# Prefer the prebuilt release binary (no Go needed); fall back to building from
# source. Set FROM_SOURCE=1 to force a source build.
tmp=$(mktemp)
tdurl="https://github.com/${REPO}/releases/latest/download/ticketdeck-${hos}-${harch}"
if [ "${FROM_SOURCE:-0}" != "1" ] && curl -fsSL "$tdurl" -o "$tmp" 2>/dev/null && [ -s "$tmp" ]; then
  install -m 0755 "$tmp" "$BIN_DIR/ticketdeck"; rm -f "$tmp"
  say "installed ticketdeck release binary ($hos-$harch)"
else
  rm -f "$tmp"
  command -v go >/dev/null 2>&1 || die "no release binary for ${hos}-${harch} yet and Go isn't installed (https://go.dev/dl)"
  ver=$(git describe --tags --always 2>/dev/null || echo dev)
  if [ -f "./cmd/ticketdeck/main.go" ]; then
    say "building ticketdeck from source ($ver)"
    go build -ldflags "-X main.version=${ver}" -o "$BIN_DIR/ticketdeck" ./cmd/ticketdeck
  else
    say "installing ticketdeck via 'go install'"
    GOBIN="$BIN_DIR" go install "github.com/${REPO}/cmd/ticketdeck@latest"
  fi
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
