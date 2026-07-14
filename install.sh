#!/bin/sh
# microfiche installer: downloads the right release binary and registers
# it with Claude Code, Codex, and Cursor — whichever are installed.
#
#   curl -fsSL https://raw.githubusercontent.com/luispmenezes/microfiche/main/install.sh | sh
#
# Options via env: MICROFICHE_PROFILE=opus  MICROFICHE_BIN_DIR=~/bin
set -eu

REPO="luispmenezes/microfiche"
PROFILE="${MICROFICHE_PROFILE:-fable}"
BIN_DIR="${MICROFICHE_BIN_DIR:-$HOME/.local/bin}"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  darwin|linux) ;;
  *) echo "unsupported OS: $os — grab the Windows binary from" \
       "https://github.com/$REPO/releases"; exit 1 ;;
esac
arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "unsupported arch: $arch"; exit 1 ;;
esac

BIN="$BIN_DIR/microfiche"
mkdir -p "$BIN_DIR"
url="https://github.com/$REPO/releases/latest/download/microfiche-$os-$arch"
echo "downloading $url"
curl -fsSL "$url" -o "$BIN"
chmod +x "$BIN"
echo "installed: $BIN"

registered=""

# --- Claude Code ------------------------------------------------------
if command -v claude >/dev/null 2>&1; then
  claude mcp remove --scope user microfiche >/dev/null 2>&1 || true
  if [ "$PROFILE" = "fable" ]; then
    claude mcp add --scope user microfiche -- "$BIN" >/dev/null
  else
    claude mcp add --scope user microfiche -- "$BIN" -profile "$PROFILE" \
      >/dev/null
  fi
  registered="$registered Claude-Code"
fi

# --- Codex ------------------------------------------------------------
if command -v codex >/dev/null 2>&1 || [ -d "$HOME/.codex" ]; then
  CFG="$HOME/.codex/config.toml"
  mkdir -p "$HOME/.codex"
  if grep -q '^\[mcp_servers\.microfiche\]' "$CFG" 2>/dev/null; then
    echo "Codex: already configured in $CFG (left untouched)"
  else
    printf '\n[mcp_servers.microfiche]\ncommand = "%s"\nargs = ["-profile", "%s"]\n' \
      "$BIN" "$PROFILE" >>"$CFG"
    registered="$registered Codex"
  fi
fi

# --- Cursor -----------------------------------------------------------
if [ -d "$HOME/.cursor" ] || [ -d "/Applications/Cursor.app" ]; then
  if command -v python3 >/dev/null 2>&1; then
    MF_BIN="$BIN" MF_PROFILE="$PROFILE" python3 - <<'PY'
import json, os
path = os.path.expanduser("~/.cursor/mcp.json")
cfg = {}
if os.path.exists(path):
    try:
        cfg = json.load(open(path))
    except ValueError:
        raise SystemExit(f"Cursor: {path} is not valid JSON; add microfiche manually")
cfg.setdefault("mcpServers", {})["microfiche"] = {
    "command": os.environ["MF_BIN"],
    "args": ["-profile", os.environ["MF_PROFILE"]],
}
os.makedirs(os.path.dirname(path), exist_ok=True)
json.dump(cfg, open(path, "w"), indent=2)
PY
    registered="$registered Cursor"
  else
    echo "Cursor detected but python3 not found — add $BIN to ~/.cursor/mcp.json manually"
  fi
fi

echo "profile: $PROFILE (rerun with MICROFICHE_PROFILE=opus for Opus models)"
if [ -n "$registered" ]; then
  echo "registered with:$registered"
else
  echo "no MCP clients detected — register manually (see README)"
fi
